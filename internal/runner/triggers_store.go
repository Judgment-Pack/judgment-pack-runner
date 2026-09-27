package runner

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const triggerSchema = `
 CREATE TABLE IF NOT EXISTS triggers(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,record TEXT NOT NULL,key_hash TEXT NOT NULL DEFAULT '',observed TEXT NOT NULL DEFAULT '',pending TEXT NOT NULL DEFAULT '',pending_at TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS trigger_revisions(trigger_id TEXT NOT NULL,revision INTEGER NOT NULL,record TEXT NOT NULL,PRIMARY KEY(trigger_id,revision));
 CREATE TABLE IF NOT EXISTS occurrences(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,trigger_id TEXT NOT NULL,identity TEXT NOT NULL,payload_digest TEXT NOT NULL,state TEXT NOT NULL,record TEXT NOT NULL,UNIQUE(trigger_id,identity));
 CREATE INDEX IF NOT EXISTS occurrences_state ON occurrences(state,seq);
 CREATE INDEX IF NOT EXISTS triggers_job ON triggers(job_id);
 CREATE INDEX IF NOT EXISTS occurrences_job ON occurrences(job_id,seq);
 CREATE UNIQUE INDEX IF NOT EXISTS triggers_cloud_binding ON triggers(json_extract(record,'$.config.cloud.connection'),json_extract(record,'$.config.cloud.job')) WHERE json_extract(record,'$.config.kind')='cloud';`

func (s *Service) trigger(id string) (Trigger, string, error) {
	var t Trigger
	var raw, hash, observed, pending, pendingAt string
	e := s.db.QueryRow("SELECT record,key_hash,observed,pending,pending_at FROM triggers WHERE id=?", id).Scan(&raw, &hash, &observed, &pending, &pendingAt)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &t)
		t.HasKey = hash != ""
		t.observed, t.pending, t.pendingAt = observed, pending, pendingAt
	}
	return t, hash, e
}
func saveTrigger(tx *sql.Tx, t Trigger, hash string, history bool) error {
	_, e := tx.Exec("UPDATE triggers SET record=?,key_hash=?,observed=?,pending=?,pending_at=? WHERE id=?", string(encode(t)), hash, t.observed, t.pending, t.pendingAt, t.ID)
	if e != nil {
		return e
	}
	if history {
		_, e = tx.Exec("INSERT INTO trigger_revisions VALUES(?,?,?)", t.ID, t.Revision, string(encode(t)))
	}
	return e
}
func (s *Service) writeTrigger(t Trigger, hash string, history bool) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = saveTrigger(tx, t, hash, history); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) configureTrigger(jobID, triggerID string, revision int, c TriggerConfig) (Trigger, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	job, e := s.job(jobID)
	if e != nil {
		return Trigger{}, e
	}
	release, e := s.release(job.ReleaseID)
	if e != nil {
		return Trigger{}, e
	}
	if e = s.validateTrigger(&c, release, time.Now()); e != nil {
		return Trigger{}, e
	}
	var t Trigger
	hash := ""
	if triggerID != "" {
		t, hash, e = s.trigger(triggerID)
		if e != nil {
			return t, e
		}
		if t.JobID != jobID {
			return t, sql.ErrNoRows
		}
		if t.Revision != revision {
			return t, &apiError{409, "trigger_changed", "The trigger changed. Reload before saving."}
		}
		if t.Config.Kind != c.Kind {
			return t, bad("trigger_kind_fixed", "Create another trigger to change its type.")
		}
		t.Revision++
	} else {
		var count int
		if e = s.db.QueryRow("SELECT count(*) FROM triggers").Scan(&count); e != nil {
			return t, e
		}
		if count >= 128 {
			return t, bad("trigger_limit", "This local runner supports up to 128 triggers.")
		}
		t = Trigger{ID: id("trg_"), JobID: jobID, Revision: 1, Authority: "local", CreatedAt: now()}
	}
	if c.Cloud != nil {
		var other int
		if e = s.db.QueryRow(`SELECT count(*) FROM triggers WHERE id<>? AND json_extract(record,'$.config.cloud.connection')=? AND json_extract(record,'$.config.cloud.job')=?`, t.ID, c.Cloud.Connection, c.Cloud.Job).Scan(&other); e != nil {
			return t, e
		}
		if other > 0 {
			return t, bad("cloud_binding_used", "This scheduler job is already bound to a trigger.")
		}
		t.Authority = "google-cloud"
	}
	t.Config = c
	t.Paused = true
	t.UpdatedAt = now()
	t.NextAt = ""
	t.Problem = ""
	t.pending = ""
	t.pendingAt = ""
	tx, e := s.db.Begin()
	if e != nil {
		return t, e
	}
	defer tx.Rollback()
	if triggerID == "" {
		if _, e = tx.Exec("INSERT INTO triggers(id,job_id,record)VALUES(?,?,?)", t.ID, t.JobID, string(encode(t))); e != nil {
			return t, e
		}
	}
	if e = saveTrigger(tx, t, hash, true); e != nil {
		return t, e
	}
	return t, tx.Commit()
}
func (s *Service) setTriggerState(triggerID string, revision int, paused, reviewed bool) (Trigger, string, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	t, hash, e := s.trigger(triggerID)
	if e != nil {
		return t, "", e
	}
	if revision != t.Revision {
		return t, "", &apiError{409, "trigger_changed", "The trigger changed. Reload before updating it."}
	}
	if !paused && !reviewed {
		return t, "", bad("trigger_review_required", "Review the automatic inputs, timing and execution policy before enabling this trigger.")
	}
	if t.Paused == paused {
		return t, "", nil
	}
	secret := ""
	if !paused {
		job, e := s.job(t.JobID)
		if e != nil {
			return t, "", e
		}
		release, e := s.release(job.ReleaseID)
		if e != nil {
			return t, "", e
		}
		if e = s.validateTrigger(&t.Config, release, time.Now()); e != nil {
			return t, "", e
		}
		switch t.Config.Kind {
		case "schedule":
			if t.Config.Input.Kind != "mapped-sources" {
				if _, e = s.automaticInput(t.Config.Input, release); e != nil {
					return t, "", e
				}
			}
			next := t.Config.Schedule.next(time.Now())
			if next.IsZero() {
				return t, "", bad("schedule_finished", "This schedule has no future occurrence.")
			}
			t.NextAt = next.Format(time.RFC3339Nano)
		case "cloud":
			if t.Config.Input.Kind != "mapped-sources" {
				if _, e = s.automaticInput(t.Config.Input, release); e != nil {
					return t, "", e
				}
			}
		case "file":
			raw, e := s.readAutomaticFile(t.Config.WatchPath)
			if e == nil {
				t.observed = digest(raw)
			} else {
				t.observed = ""
			}
			t.pending = ""
			t.pendingAt = ""
		case "event":
			if hash == "" {
				var key [32]byte
				if _, e = rand.Read(key[:]); e != nil {
					return t, "", e
				}
				secret = hex.EncodeToString(key[:])
				hash = digest([]byte(secret))
				t.HasKey = true
			}
		}
	} else {
		t.NextAt = ""
	}
	t.Paused = paused
	t.Revision++
	t.UpdatedAt = now()
	t.Problem = ""
	e = s.writeTrigger(t, hash, true)
	return t, secret, e
}
func (s *Service) rotateTriggerKey(triggerID string, revision int) (Trigger, string, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	t, _, e := s.trigger(triggerID)
	if e != nil {
		return t, "", e
	}
	if t.Config.Kind != "event" {
		return t, "", bad("not_event_trigger", "Only event triggers have delivery credentials.")
	}
	if t.Revision != revision {
		return t, "", &apiError{409, "trigger_changed", "The trigger changed. Reload before updating it."}
	}
	var key [32]byte
	if _, e = rand.Read(key[:]); e != nil {
		return t, "", e
	}
	secret := hex.EncodeToString(key[:])
	t.HasKey = true
	t.Revision++
	t.UpdatedAt = now()
	e = s.writeTrigger(t, digest([]byte(secret)), true)
	return t, secret, e
}
func (s *Service) event(triggerID, token string, event EventDelivery) (Occurrence, bool, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	t, hash, e := s.trigger(triggerID)
	if e != nil {
		return Occurrence{}, false, &apiError{401, "invalid_trigger_token", "The event credential is invalid."}
	}
	if len(token) != 64 || hash == "" || subtle.ConstantTimeCompare([]byte(digest([]byte(token))), []byte(hash)) != 1 {
		return Occurrence{}, false, &apiError{401, "invalid_trigger_token", "The event credential is invalid."}
	}
	if t.Config.Kind != "event" {
		return Occurrence{}, false, bad("not_event_trigger", "This trigger does not accept event delivery.")
	}
	if event.ID == "" || len(event.ID) > 128 {
		return Occurrence{}, false, bad("invalid_event", "Supply a stable event ID up to 128 bytes.")
	}
	// Delivery identity is stable across trigger configuration revisions.
	raw, e := canonical(encode(event))
	if e != nil {
		return Occurrence{}, false, e
	}
	payload := digest(raw)
	old, held, e := s.findOccurrence(t.ID, "event:"+event.ID)
	if e == nil {
		if held != payload {
			return old, false, &apiError{409, "event_conflict", "This event ID was already received with a different payload."}
		}
		old.Input = nil
		return old, true, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return old, false, e
	}
	if s.unhealthy.Load() {
		return old, false, &apiError{503, "runner_unavailable", "The runner requires a restart before accepting new events."}
	}
	if t.Paused {
		return old, false, &apiError{409, "trigger_paused", "This trigger is paused; the event was not accepted."}
	}
	at, e := time.Parse(time.RFC3339, event.OccurredAt)
	if e != nil || time.Since(at) > 5*time.Minute || time.Until(at) > 30*time.Second {
		return old, false, bad("event_expired", "Supply an event timestamp from the last five minutes.")
	}
	if event.Input.Preparation != nil {
		return old, false, bad("invalid_event", "Supply original inputs, not caller-computed preparation.")
	}
	job, e := s.job(t.JobID)
	if e != nil {
		return old, false, e
	}
	release, e := s.release(job.ReleaseID)
	if e != nil {
		return old, false, e
	}
	if !mappingMatchesRelease(release, event.Input) {
		return old, false, bad("mapping_mismatch", "Use the job's reviewed input mapping.")
	}
	if _, e = s.normalizeInput(event.Input); e != nil {
		return old, false, e
	}
	o := newOccurrence(t, job, time.Now())
	o.EventID = event.ID
	o.Input = &event.Input
	o.InputDigest = digest(encode(event.Input))
	tx, e := s.db.Begin()
	if e != nil {
		return o, false, e
	}
	defer tx.Rollback()
	if e = s.admitOccurrence(tx, t, &o, "event:"+event.ID, payload); e != nil {
		return o, false, e
	}
	e = tx.Commit()
	o.Input = nil
	return o, false, e
}
func newOccurrence(t Trigger, j Job, at time.Time) Occurrence {
	return Occurrence{ID: id("occ_"), JobID: j.ID, ReleaseID: j.ReleaseID, JobRevision: j.Revision, TriggerID: t.ID, TriggerRevision: t.Revision, Kind: t.Config.Kind, ReceivedAt: at.UTC().Format(time.RFC3339Nano), ExpiresAt: at.Add(time.Duration(t.Config.QueueSeconds) * time.Second).UTC().Format(time.RFC3339Nano), State: "accepted"}
}
func (s *Service) findOccurrence(trigger, identity string) (Occurrence, string, error) {
	var o Occurrence
	var raw, hash string
	e := s.db.QueryRow("SELECT record,payload_digest FROM occurrences WHERE trigger_id=? AND identity=?", trigger, identity).Scan(&raw, &hash)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &o)
	}
	return o, hash, e
}
func (s *Service) admitOccurrence(tx *sql.Tx, t Trigger, o *Occurrence, identity, payload string) error {
	if o.State == "accepted" {
		var pending, active int
		if e := tx.QueryRow("SELECT count(*) FROM occurrences WHERE state IN ('accepted','preparing')").Scan(&pending); e != nil {
			return e
		}
		if e := tx.QueryRow("SELECT (SELECT count(*) FROM runs WHERE job_id=? AND state IN ('queued','running'))+(SELECT count(*) FROM occurrences WHERE job_id=? AND state IN ('accepted','preparing'))", o.JobID, o.JobID).Scan(&active); e != nil {
			return e
		}
		if pending >= queueLimit {
			o.State = "skipped"
			o.Reason = "queue-full"
		} else if active > 0 && t.Config.Overlap == "skip" {
			o.State = "skipped"
			o.Reason = "overlap"
		}
	}
	if o.State != "accepted" {
		o.Input = nil
		o.PendingInput = nil
	}
	_, e := tx.Exec("INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES(?,?,?,?,?,?,?)", o.ID, o.JobID, o.TriggerID, identity, payload, o.State, string(encode(o)))
	return e
}
func (s *Service) saveOccurrence(o Occurrence) error {
	_, e := s.db.Exec("UPDATE occurrences SET state=?,record=? WHERE id=?", o.State, string(encode(o)), o.ID)
	return e
}
