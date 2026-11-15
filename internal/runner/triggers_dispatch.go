package runner

import (
	"context"
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
	ids, e := s.store.enabledTriggers()
	if e != nil {
		return e
	}
	for _, id := range ids {
		t, hash, e := s.trigger(id)
		if e != nil {
			return e
		}
		// One paused since the list was read, by a request on this Runner
		// or another of the store, makes nothing.
		if t.Paused {
			continue
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
	// One another Runner of the store holds is passed by (D18). A queue with
	// no room ends the iteration: the first ready occurrence, the one the next
	// read would hand back, waits for the next tick.
	for range 10 {
		o, e := s.store.readyOccurrence()
		if absent(e) {
			break
		}
		if e != nil {
			return e
		}
		e = s.dispatchReady(o, at)
		if errors.Is(e, errQueueHasNoRoom) {
			break
		}
		if e != nil {
			return e
		}
	}
	return nil
}

// errQueueHasNoRoom is the answer of a dispatch whose run the queue had no
// room for: the occurrence waits as it was read, its claim released. It is
// no error, and stops the tick's dispatching.
var errQueueHasNoRoom = errors.New("the queue has no room for the occurrence's run")

// queueWaits is a dispatch's error, nil for one the queue had no room for.
func queueWaits(err error) error {
	if errors.Is(err, errQueueHasNoRoom) {
		return nil
	}
	return err
}

// scheduleTick is the Runner's own clock's tick of a schedule trigger at at.
// A shared-interval schedule is not its: only its interval's calls tick it
// (sharedTick).
func (s *Service) scheduleTick(t Trigger, hash string, at time.Time) error {
	if s.cfg.ExecutorAdmission != nil {
		return manualUnsupported()
	}
	if sharedTicked(t, "") {
		return nil
	}
	_, e := s.scheduleDue(t, hash, at, at, ownClockGrace, nil)
	return e
}

// ownClockGrace is how late the Runner's own clock may tick a due time
// before the time counts as missed. A shared tick has none: the tick a call
// names is the clock, and a due time before it is one whose tick no call
// named.
const ownClockGrace = 30 * time.Second

// scheduleDue ticks the schedule trigger t, read at row version hash, at the
// time tick, the Runner's clock being now: a due time at or before the tick
// admits its occurrence, by the trigger's missed rule when the tick is more
// than grace after it, and moves the trigger's next time on past the tick,
// in one transaction that expects the version read (D17). made says
// whether this call recorded an occurrence; a due time after the tick, or
// a trigger written since it was read (by a request, a repeated tick, or
// another Runner's tick of the same due time), makes none. The occurrence's
// entry names caller, a shared tick's requester, or with none the trigger
// at its revision, as the Runner's own clock's tick does.
func (s *Service) scheduleDue(t Trigger, hash string, tick, now time.Time, grace time.Duration, caller *EventActor) (made bool, err error) {
	if s.cfg.ExecutorAdmission != nil {
		return false, manualUnsupported()
	}
	if t.NextAt == "" {
		return false, nil
	}
	due, e := time.Parse(time.RFC3339, t.NextAt)
	if e != nil {
		return false, e
	}
	if due.After(tick) {
		return false, nil
	}
	job, e := s.job(t.JobID)
	if e != nil {
		return false, e
	}
	release, e := s.release(job.ReleaseID)
	if e != nil {
		return false, e
	}
	latest := t.Config.Schedule.latest(tick)
	if latest.Before(due) {
		latest = due
	}
	o := newOccurrence(t, job, now)
	o.ScheduledAt = due.Format(time.RFC3339Nano)
	identity := fmt.Sprintf("schedule:%d:%s", t.Revision, o.ScheduledAt)
	missed := tick.Sub(due) > grace
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
				o.ErrorClass = acquisitionClass(e)
			} else {
				o.Input = &input
				o.InputDigest = digest(encode(input))
			}
		}
	}
	next := t.Config.Schedule.next(tick)
	t.NextAt = ""
	if !next.IsZero() {
		t.NextAt = next.Format(time.RFC3339Nano)
	}
	err = s.store.update(func(tx runTx) error {
		// The trigger first, at the version this tick read it (D17): a
		// write of it since, another Runner's tick of the same due time
		// among them, admits nothing here. An identity held while the
		// trigger is as read is no race, and fails as before.
		if err := tx.putTrigger(t, hash, false); err != nil {
			return err
		}
		_, err := s.admitOccurrenceAs(tx, t, o, identity, digest(encode(t.Config)), caller)
		return err
	})
	return err == nil, tickLost(err)
}

// tickLost is a tick's write that met a write of its trigger made since the
// tick read it, by a request or by another Runner's tick on the same store
// (D17), taken for no failure: the tick made nothing, and the next reads the
// trigger again. Any other error, another conflict of the tick's transaction
// among them, is the tick's.
func tickLost(err error) error {
	if faultOf(err) == faultConflict && errors.Is(err, errTriggerWritten) {
		return nil
	}
	return err
}
func (s *Service) fileTick(t Trigger, hash string, at time.Time) error {
	if s.cfg.ExecutorAdmission != nil {
		return manualUnsupported()
	}
	raw, e := s.readAutomaticFile(t.Config.WatchPath)
	if e != nil {
		if t.Problem != e.Error() {
			t.Problem = e.Error()
			return tickLost(s.writeTrigger(t, hash, false))
		}
		return nil
	}
	changed := digest(raw)
	if changed == t.observed {
		if t.pending != "" || t.Problem != "" {
			t.pending = ""
			t.pendingAt = ""
			t.Problem = ""
			return tickLost(s.writeTrigger(t, hash, false))
		}
		return nil
	}
	if changed != t.pending {
		t.pending = changed
		t.pendingAt = at.UTC().Format(time.RFC3339Nano)
		t.Problem = ""
		return tickLost(s.writeTrigger(t, hash, false))
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
		return tickLost(s.writeTrigger(t, hash, false))
	}
	if prepError != nil {
		o.State = "failed"
		o.Reason = prepError.Error()
		o.ErrorClass = acquisitionClass(prepError)
	} else {
		o.Input = &input
		o.InputDigest = digest(encode(input))
	}
	identity := fmt.Sprintf("file:%d:%s:%s", t.Revision, t.pendingAt, changed)
	t.observed = changed
	t.pending = ""
	t.pendingAt = ""
	t.Problem = ""
	return tickLost(s.store.update(func(tx runTx) error {
		// The trigger first, as in scheduleTick (D17).
		if err := tx.putTrigger(t, hash, false); err != nil {
			return err
		}
		_, err := s.admitOccurrence(tx, t, o, identity, changed)
		return err
	}))
}
func (s *Service) dispatchOccurrence(o Occurrence, at time.Time) error {
	return queueWaits(s.dispatchReady(o, at))
}

// dispatchReady is dispatchOccurrence, answering errQueueHasNoRoom for an
// occurrence whose run the queue had no room for.
func (s *Service) dispatchReady(o Occurrence, at time.Time) error {
	if s.cfg.ExecutorAdmission != nil {
		return manualUnsupported()
	}
	// The occurrence is claimed first (D18): one another Runner of the store
	// holds, or that is no longer accepted, is passed by. It is dispatched as
	// it was read, as before: a ready occurrence is not written in place, its
	// state unchanged, but by a holder that releases it as it read it. Each
	// write below is fenced by the claim and releases it, and one refused,
	// the claim having been lost, writes nothing: the Runner goes on
	// (settleOccurrence).
	held, e := s.claimOccurrence(o.ID, "accepted")
	if e != nil {
		return occurrenceLost(e)
	}
	o.claim = held.claim
	return s.dispatchClaimed(o, at)
}

// dispatchClaimed is dispatchReady once the occurrence is claimed. A second
// Runner that claims it after this one's lease ran out finds the run this
// one made by its request key, and submits no other. No error is returned
// with the claim held (#282): an occurrence whose own record cannot be
// dispatched is failed (failHeld), its job or release record unreadable
// included, and an error of the store's or of this Runner's releases the
// claim first (releasedWith).
func (s *Service) dispatchClaimed(o Occurrence, at time.Time) error {
	// Reconcile admission first: a crash after run creation must not become a
	// second evaluation or an expiry, even if source receipts have since aged.
	// The run's id alone is read, as before the contract: a run whose record
	// does not decode is still the occurrence's run, and holds no occurrence
	// behind it on every tick.
	held, e := s.store.requestedRun(o.JobID, "trigger:"+o.TriggerID, o.ID)
	if e == nil {
		o.RunID = held.id
		o.State = "submitted"
		o.Input = nil
		return s.settleOccurrence(o)
	}
	if !absent(e) {
		return s.releasedWith(o, e)
	}
	expires := o.ExpiresAt
	if o.ReadyUntil != "" {
		expires = o.ReadyUntil
	}
	expiry, e := time.Parse(time.RFC3339Nano, expires)
	if e != nil {
		return s.failHeld(o, "The accepted occurrence holds no readable expiry time.")
	}
	if !at.Before(expiry) {
		o.State = "expired"
		o.Reason = "queue-expired"
		o.Input = nil
		return s.settleOccurrence(o)
	}
	if o.Input == nil {
		return s.failHeld(o, "The accepted occurrence holds no input.")
	}
	job, e := s.job(o.JobID)
	if e != nil {
		return s.heldRecordOrReleased(o, e)
	}
	if job.ReleaseID != o.ReleaseID || job.Revision != o.JobRevision {
		o.State = "failed"
		o.Reason = "The job release changed before admission."
		o.ErrorClass = classConfiguration
		o.Input = nil
		return s.settleOccurrence(o)
	}
	origin := &TriggerOrigin{OccurrenceID: o.ID, TriggerID: o.TriggerID, TriggerRevision: o.TriggerRevision, Kind: o.Kind, ScheduledAt: o.ScheduledAt, EventID: o.EventID, InputDigest: o.InputDigest, ExpiresAt: expires}
	run, _, e := s.submitInternal(o.JobID, o.ID, *o.Input, origin)
	if e != nil {
		var api *apiError
		if errors.As(e, &api) {
			if api.Status == 429 {
				// The occurrence waits for room in the queue as it was
				// read, its claim released on a kind that leases one.
				if leases(s.store) {
					if e = s.settleOccurrence(o); e != nil {
						return e
					}
				}
				return errQueueHasNoRoom
			}
			o.State = "failed"
			o.Reason = api.Message
			o.ErrorClass = submissionClass(api)
			o.Input = nil
			return s.settleOccurrence(o)
		}
		return s.heldRecordOrReleased(o, e)
	}
	o.RunID = run.ID
	o.State = "submitted"
	o.Input = nil
	return s.settleOccurrence(o)
}
