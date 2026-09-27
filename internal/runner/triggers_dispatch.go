package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *Service) automation(ctx context.Context) {
	defer close(s.automationDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if e := s.automationTick(time.Now()); e != nil {
			s.unhealthy.Store(true)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) automationTick(at time.Time) error {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	if s.unhealthy.Load() {
		return nil
	}
	rows, e := s.db.Query("SELECT id FROM triggers WHERE json_extract(record,'$.paused')=0 ORDER BY seq")
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		t, hash, e := s.trigger(id)
		if e != nil {
			return e
		}
		switch t.Config.Kind {
		case "schedule":
			if e = s.scheduleTick(t, hash, at); e != nil {
				return e
			}
		case "file":
			if e = s.fileTick(t, hash, at); e != nil {
				return e
			}
		}
	}
	// Each iteration handles a bounded number of accepted occurrences; the
	// evaluator has its own durable queue and cannot block the scheduler clock.
	for range 10 {
		var raw string
		e := s.db.QueryRow("SELECT record FROM occurrences WHERE state='accepted' AND json_extract(record,'$.pendingInput') IS NULL ORDER BY seq LIMIT 1").Scan(&raw)
		if errors.Is(e, sql.ErrNoRows) {
			break
		}
		if e != nil {
			return e
		}
		var o Occurrence
		if e = json.Unmarshal([]byte(raw), &o); e != nil {
			return e
		}
		if e = s.dispatchOccurrence(o, at); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) scheduleTick(t Trigger, hash string, at time.Time) error {
	if t.NextAt == "" {
		return nil
	}
	due, e := time.Parse(time.RFC3339, t.NextAt)
	if e != nil {
		return e
	}
	if due.After(at) {
		return nil
	}
	job, e := s.job(t.JobID)
	if e != nil {
		return e
	}
	release, e := s.release(job.ReleaseID)
	if e != nil {
		return e
	}
	latest := t.Config.Schedule.latest(at)
	if latest.Before(due) {
		latest = due
	}
	o := newOccurrence(t, job, at)
	o.ScheduledAt = due.Format(time.RFC3339Nano)
	identity := fmt.Sprintf("schedule:%d:%s", t.Revision, o.ScheduledAt)
	missed := at.Sub(due) > 30*time.Second
	if missed {
		o.MissedFrom = due.Format(time.RFC3339Nano)
	}
	if missed && t.Config.Missed == "skip" {
		o.State = "skipped"
		o.Reason = "missed"
		o.MissedThrough = latest.Format(time.RFC3339Nano)
	} else {
		if missed {
			o.ScheduledAt = latest.Format(time.RFC3339Nano)
			o.MissedThrough = latest.Format(time.RFC3339Nano)
			o.Reason = "latest-after-missed"
		}
		if t.Config.Input.Kind == "mapped-sources" {
			o.PendingInput = t.Config.Input
		} else {
			input, e := s.automaticInput(t.Config.Input, release)
			if e != nil {
				o.State = "failed"
				o.Reason = e.Error()
			} else {
				o.Input = &input
				o.InputDigest = digest(encode(input))
			}
		}
	}
	next := t.Config.Schedule.next(at)
	t.NextAt = ""
	if !next.IsZero() {
		t.NextAt = next.Format(time.RFC3339Nano)
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.admitOccurrence(tx, t, &o, identity, digest(encode(t.Config))); e != nil {
		return e
	}
	if e = saveTrigger(tx, t, hash, false); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) fileTick(t Trigger, hash string, at time.Time) error {
	raw, e := s.readAutomaticFile(t.Config.WatchPath)
	if e != nil {
		if t.Problem != e.Error() {
			t.Problem = e.Error()
			return s.writeTrigger(t, hash, false)
		}
		return nil
	}
	changed := digest(raw)
	if changed == t.observed {
		if t.pending != "" || t.Problem != "" {
			t.pending = ""
			t.pendingAt = ""
			t.Problem = ""
			return s.writeTrigger(t, hash, false)
		}
		return nil
	}
	if changed != t.pending {
		t.pending = changed
		t.pendingAt = at.UTC().Format(time.RFC3339Nano)
		t.Problem = ""
		return s.writeTrigger(t, hash, false)
	}
	since, e := time.Parse(time.RFC3339Nano, t.pendingAt)
	if e != nil {
		return e
	}
	if at.Sub(since) < time.Duration(t.Config.StableSeconds)*time.Second {
		return nil
	}
	job, e := s.job(t.JobID)
	if e != nil {
		return e
	}
	release, e := s.release(job.ReleaseID)
	if e != nil {
		return e
	}
	o := newOccurrence(t, job, at)
	o.EventID = changed
	input, prepError := s.automaticInputSnapshot(t.Config.Input, release, map[string][]byte{t.Config.WatchPath: raw})
	// Never attach newer bytes to an older file-change identity.
	check, e := s.readAutomaticFile(t.Config.WatchPath)
	if e != nil || digest(check) != changed {
		t.pending = ""
		t.pendingAt = ""
		return s.writeTrigger(t, hash, false)
	}
	if prepError != nil {
		o.State = "failed"
		o.Reason = prepError.Error()
	} else {
		o.Input = &input
		o.InputDigest = digest(encode(input))
	}
	identity := fmt.Sprintf("file:%d:%s:%s", t.Revision, t.pendingAt, changed)
	t.observed = changed
	t.pending = ""
	t.pendingAt = ""
	t.Problem = ""
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.admitOccurrence(tx, t, &o, identity, changed); e != nil {
		return e
	}
	if e = saveTrigger(tx, t, hash, false); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) dispatchOccurrence(o Occurrence, at time.Time) error {
	// Reconcile admission first: a crash after run creation must not become a
	// second evaluation or an expiry, even if source receipts have since aged.
	var runID string
	e := s.db.QueryRow("SELECT id FROM runs WHERE caller=? AND idem=? AND job_id=?", "trigger:"+o.TriggerID, o.ID, o.JobID).Scan(&runID)
	if e == nil {
		o.RunID = runID
		o.State = "submitted"
		o.Input = nil
		return s.saveOccurrence(o)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	expires := o.ExpiresAt
	if o.ReadyUntil != "" {
		expires = o.ReadyUntil
	}
	expiry, e := time.Parse(time.RFC3339Nano, expires)
	if e != nil {
		return e
	}
	if !at.Before(expiry) {
		o.State = "expired"
		o.Reason = "queue-expired"
		o.Input = nil
		return s.saveOccurrence(o)
	}
	if o.Input == nil {
		return errors.New("accepted occurrence has no input")
	}
	job, e := s.job(o.JobID)
	if e != nil {
		return e
	}
	if job.ReleaseID != o.ReleaseID || job.Revision != o.JobRevision {
		o.State = "failed"
		o.Reason = "The job release changed before admission."
		o.Input = nil
		return s.saveOccurrence(o)
	}
	origin := &TriggerOrigin{OccurrenceID: o.ID, TriggerID: o.TriggerID, TriggerRevision: o.TriggerRevision, Kind: o.Kind, ScheduledAt: o.ScheduledAt, EventID: o.EventID, InputDigest: o.InputDigest, ExpiresAt: expires}
	run, _, e := s.submitInternal(o.JobID, o.ID, *o.Input, origin)
	if e != nil {
		var api *apiError
		if errors.As(e, &api) {
			if api.Status == 429 {
				return nil
			}
			o.State = "failed"
			o.Reason = api.Message
			o.Input = nil
			return s.saveOccurrence(o)
		}
		return e
	}
	o.RunID = run.ID
	o.State = "submitted"
	o.Input = nil
	return s.saveOccurrence(o)
}
