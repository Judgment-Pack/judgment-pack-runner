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
 CREATE TABLE IF NOT EXISTS occurrence_keys(occurrence_id TEXT PRIMARY KEY,key_hash TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS occurrences_state ON occurrences(state,seq);
 CREATE INDEX IF NOT EXISTS triggers_job ON triggers(job_id);
 CREATE INDEX IF NOT EXISTS occurrences_job ON occurrences(job_id,seq);
 CREATE UNIQUE INDEX IF NOT EXISTS triggers_cloud_binding ON triggers(json_extract(record,'$.config.cloud.connection'),json_extract(record,'$.config.cloud.job')) WHERE json_extract(record,'$.config.kind')='cloud';`

func (s *Service) trigger(id string) (Trigger, string, error) {
	var t Trigger
	var raw, hash, observed, pending, pendingAt string
	var keyRevision sql.NullInt64
	e := s.db.QueryRow("SELECT record,key_hash,observed,pending,pending_at,key_revision FROM triggers WHERE id=?", id).Scan(&raw, &hash, &observed, &pending, &pendingAt, &keyRevision)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &t)
		t.HasKey = hash != ""
		t.observed, t.pending, t.pendingAt = observed, pending, pendingAt
		if keyRevision.Valid {
			v := int(keyRevision.Int64)
			t.keyRevision = &v
		}
	}
	return t, hash, e
}
func saveTrigger(tx *sql.Tx, t Trigger, hash string, history bool) error {
	var keyRevision any
	if t.keyRevision != nil {
		keyRevision = *t.keyRevision
	}
	_, e := tx.Exec("UPDATE triggers SET record=?,key_hash=?,observed=?,pending=?,pending_at=?,key_revision=? WHERE id=?", string(encode(t)), hash, t.observed, t.pending, t.pendingAt, keyRevision, t.ID)
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
	from, previous := "", 0
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
		previous = t.Revision
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
	} else if from, e = triggerBefore(tx, t.ID, previous); e != nil {
		return t, e
	}
	if e = saveTrigger(tx, t, hash, true); e != nil {
		return t, e
	}
	if _, e = appendEvent(tx, change{kind: "trigger.configured", by: s.installation(), concerns: EventConcerns{Job: job.ID, Release: job.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: from, to: "paused", detail: EventDetail{PreviousRevision: revisionValue(previous), TriggerKind: c.Kind}}); e != nil {
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
	job, e := s.job(t.JobID)
	if e != nil {
		return t, "", e
	}
	from := triggerState(t.Paused)
	t.Paused = paused
	t.Revision++
	t.UpdatedAt = now()
	t.Problem = ""
	detail := EventDetail{PreviousRevision: revisionValue(t.Revision - 1), KeyIssued: secret != ""}
	if secret != "" {
		issued := t.Revision
		t.keyRevision = &issued
	}
	kind := "trigger.paused"
	if !paused {
		kind = "trigger.resumed"
		detail.NextAt = t.NextAt
	}
	e = s.writeTriggerChange(t, hash, change{kind: kind, by: s.installation(), concerns: EventConcerns{Job: job.ID, Release: job.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: from, to: triggerState(t.Paused), detail: detail})
	return t, secret, e
}

// writeTriggerChange writes a trigger's new revision, its snapshot and the
// change's entry in one transaction.
func (s *Service) writeTriggerChange(t Trigger, hash string, c change) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if c.from, e = triggerBefore(tx, t.ID, t.Revision-1); e != nil {
		return e
	}
	if e = saveTrigger(tx, t, hash, true); e != nil {
		return e
	}
	if _, e = appendEvent(tx, c); e != nil {
		return e
	}
	return tx.Commit()
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
	job, e := s.job(t.JobID)
	if e != nil {
		return t, "", e
	}
	secret := hex.EncodeToString(key[:])
	t.HasKey = true
	t.Revision++
	t.UpdatedAt = now()
	issued := t.Revision
	t.keyRevision = &issued
	state := triggerState(t.Paused)
	e = s.writeTriggerChange(t, digest([]byte(secret)), change{kind: "trigger.key-rotated", by: s.installation(), concerns: EventConcerns{Job: job.ID, Release: job.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: state, to: state, detail: EventDetail{PreviousRevision: revisionValue(t.Revision - 1)}})
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
	// The credential is authenticated: a refusal from here on is its own,
	// named by the revision its key was issued at, whatever happens to the
	// key before the refusal is recorded.
	o, replay, e := s.admitEvent(t, hash, event)
	if e != nil {
		e = &credentialRefusal{err: e, by: credentialActor(t.ID, t.keyRevision)}
	}
	return o, replay, e
}

// admitEvent admits an event delivery whose credential event authenticated.
func (s *Service) admitEvent(t Trigger, hash string, event EventDelivery) (Occurrence, bool, error) {
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
	// The occurrence is bound to the credential that created it, for that
	// credential's read of its result.
	if _, e = tx.Exec("INSERT INTO occurrence_keys(occurrence_id,key_hash) VALUES(?,?)", o.ID, hash); e != nil {
		return o, false, e
	}
	e = tx.Commit()
	o.Input = nil
	return o, false, e
}

// eventResult answers a trigger token about an occurrence that token created:
// its state and, once its run completed, the disposition and handoff target.
// Nothing else: no inputs, no identifiers of other records, no listing. The
// token must be its trigger's current credential and the one that created the
// occurrence; knowing an occurrence ID is not enough. Every other case gets one
// refusal, which does not say whether the occurrence exists.
func (s *Service) eventResult(triggerID, occurrenceID, token string) (map[string]any, error) {
	_, hash, e := s.trigger(triggerID)
	if e != nil || len(token) != 64 || hash == "" || subtle.ConstantTimeCompare([]byte(digest([]byte(token))), []byte(hash)) != 1 {
		return nil, &apiError{401, "invalid_trigger_token", "The event credential is invalid."}
	}
	// One statement, so a rotation after the check above still refuses.
	var raw string
	e = s.db.QueryRow("SELECT o.record FROM occurrences o JOIN occurrence_keys k ON k.occurrence_id=o.id JOIN triggers t ON t.id=o.trigger_id WHERE o.id=? AND o.trigger_id=? AND k.key_hash=? AND t.key_hash=k.key_hash", occurrenceID, triggerID, hash).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, &apiError{404, "occurrence_not_found", "No occurrence with this ID was created with this credential."}
	}
	var o Occurrence
	if e == nil {
		e = json.Unmarshal([]byte(raw), &o)
	}
	if e != nil {
		return nil, e
	}
	answer := map[string]any{"id": o.ID, "state": o.State}
	if o.RunID == "" {
		return answer, nil
	}
	run, e := s.run(o.RunID)
	if e != nil {
		return nil, e
	}
	summary := map[string]any{"state": run.State}
	if run.State == "completed" {
		var result map[string]json.RawMessage
		if e = json.Unmarshal(run.Result, &result); e != nil {
			return nil, e
		}
		summary["result"] = map[string]json.RawMessage{"disposition": result["disposition"], "handoffTarget": result["handoffTarget"]}
	}
	answer["run"] = summary
	return answer, nil
}
func newOccurrence(t Trigger, j Job, at time.Time) Occurrence {
	return Occurrence{PreparationSeconds: t.Config.PreparationSeconds, QueueSeconds: t.Config.QueueSeconds, ID: id("occ_"), JobID: j.ID, ReleaseID: j.ReleaseID, JobRevision: j.Revision, TriggerID: t.ID, TriggerRevision: t.Revision, Kind: t.Config.Kind, ReceivedAt: at.UTC().Format(time.RFC3339Nano), ExpiresAt: at.Add(time.Duration(t.Config.QueueSeconds) * time.Second).UTC().Format(time.RFC3339Nano), State: "accepted"}
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
		if e := tx.QueryRow("SELECT count(*) FROM occurrences WHERE state IN ('accepted','preparing','waiting')").Scan(&pending); e != nil {
			return e
		}
		if e := tx.QueryRow("SELECT (SELECT count(*) FROM runs WHERE job_id=? AND state IN ('queued','running'))+(SELECT count(*) FROM occurrences WHERE job_id=? AND state IN ('accepted','preparing','waiting','needs-attention'))", o.JobID, o.JobID).Scan(&active); e != nil {
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
	if _, e := tx.Exec("INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES(?,?,?,?,?,?,?)", o.ID, o.JobID, o.TriggerID, identity, payload, o.State, string(encode(o))); e != nil {
		return e
	}
	// An event delivery is the credential's that the delivery carried, the
	// trigger's current key; a schedule, a file change or a cloud signal is
	// the trigger's, at its revision.
	by := triggerActor(t.ID, t.Revision)
	if o.Kind == "event" {
		by = credentialActor(t.ID, t.keyRevision)
	}
	_, e := appendEvent(tx, occurrenceChange(*o, "", by))
	return e
}

// saveOccurrence records an occurrence, and the entry of its change of state,
// if any, in one transaction. Runner made the change.
func (s *Service) saveOccurrence(o Occurrence) error {
	return s.changeOccurrence(o, "", runnerActor)
}
