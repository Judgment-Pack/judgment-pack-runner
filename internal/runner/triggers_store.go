package runner

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

func (s *Service) trigger(id string) (Trigger, string, error) { return s.store.trigger(id) }
func (s *Service) writeTrigger(t Trigger, hash string, history bool) error {
	return s.store.update(func(tx runTx) error { return tx.putTrigger(t, hash, history) })
}
func (s *Service) configureTrigger(jobID, triggerID string, revision int, c TriggerConfig) (Trigger, error) {
	return s.configureTriggerBy(s.store, requester{}, jobID, triggerID, revision, c)
}

// errCloudBindingUsed is the refusal of a cloud binding another trigger
// holds.
func errCloudBindingUsed() error {
	return bad("cloud_binding_used", "This scheduler job is already bound to a trigger.")
}

// configureTriggerBy is configureTrigger, requested by by, its records read
// and written in st, the store the request is held to (storeFor). The
// trigger limit and the cloud binding are rules of the installation (D9):
// on a kind that holds scopes, st's reads count the scope's triggers alone,
// and the limit's row and the binding's index, which no policy narrows,
// refuse the write, answered as the reads before it are.
func (s *Service) configureTriggerBy(st runStore, by requester, jobID, triggerID string, revision int, c TriggerConfig) (Trigger, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Trigger{}, manualUnsupported()
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	job, e := st.job(jobID)
	if e != nil {
		return Trigger{}, e
	}
	release, e := st.release(job.ReleaseID)
	if e != nil {
		return Trigger{}, e
	}
	if e = s.validateTrigger(&c, release, time.Now()); e != nil {
		return Trigger{}, e
	}
	var t Trigger
	hash := ""
	previous := 0
	if triggerID != "" {
		t, hash, e = st.trigger(triggerID)
		if e != nil {
			return t, e
		}
		if t.JobID != jobID {
			return t, storeFailure(faultAbsent, "trigger", errNoTrigger)
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
		count, e := st.triggerCount()
		if e != nil {
			return t, e
		}
		if count >= 128 {
			return t, bad("trigger_limit", "This local runner supports up to 128 triggers.")
		}
		t = Trigger{ID: id("trg_"), JobID: jobID, Revision: 1, Authority: "local", CreatedAt: now(), Scope: job.Scope}
	}
	if c.Cloud != nil {
		other, e := st.cloudBindings(t.ID, c.Cloud.Connection, c.Cloud.Job)
		if e != nil {
			return t, e
		}
		if other > 0 {
			return t, errCloudBindingUsed()
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
	e = st.update(func(tx runTx) (err error) {
		from := ""
		if triggerID == "" {
			if err = tx.createTrigger(t); err != nil {
				return err
			}
		} else if from, err = triggerBefore(tx, t.ID, previous); err != nil {
			return err
		}
		if err = tx.putTrigger(t, hash, true); err != nil {
			return err
		}
		_, err = tx.appendJournal(change{kind: "trigger.configured", by: s.actor(by), concerns: EventConcerns{Job: job.ID, Release: job.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: from, to: "paused", detail: EventDetail{Scope: job.Scope, PreviousRevision: revisionValue(previous), TriggerKind: c.Kind}})
		return err
	})
	if c.Cloud != nil && takenKey(e) == keyCloudBinding {
		// Another scope's trigger holds the binding (D9).
		return t, errCloudBindingUsed()
	}
	if faultOf(e) == faultConflict {
		// Written since it was read, by a tick or by a request on another
		// Runner of the same store (D17).
		return t, &apiError{409, "trigger_changed", "The trigger changed. Reload before saving."}
	}
	return t, e
}
func (s *Service) setTriggerState(triggerID string, revision int, paused, reviewed bool) (Trigger, string, error) {
	return s.setTriggerStateBy(s.store, requester{}, triggerID, revision, paused, reviewed)
}

// setTriggerStateBy is setTriggerState, requested by by, its records read
// and written in st, the store the request is held to (storeFor).
func (s *Service) setTriggerStateBy(st runStore, by requester, triggerID string, revision int, paused, reviewed bool) (Trigger, string, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Trigger{}, "", manualUnsupported()
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	t, hash, e := st.trigger(triggerID)
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
		job, e := st.job(t.JobID)
		if e != nil {
			return t, "", e
		}
		release, e := st.release(job.ReleaseID)
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
	job, e := st.job(t.JobID)
	if e != nil {
		return t, "", e
	}
	from := triggerState(t.Paused)
	t.Paused = paused
	t.Revision++
	t.UpdatedAt = now()
	t.Problem = ""
	detail := EventDetail{Scope: job.Scope, PreviousRevision: revisionValue(t.Revision - 1), KeyIssued: secret != ""}
	if secret != "" {
		issued := t.Revision
		t.keyRevision = &issued
	}
	kind := "trigger.paused"
	if !paused {
		kind = "trigger.resumed"
		detail.NextAt = t.NextAt
	}
	e = writeTriggerChangeIn(st, t, hash, change{kind: kind, by: s.actor(by), concerns: EventConcerns{Job: job.ID, Release: job.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: from, to: triggerState(t.Paused), detail: detail})
	return t, secret, e
}

// writeTriggerChange writes a trigger's new revision, its snapshot and the
// change's entry in one transaction.
func (s *Service) writeTriggerChange(t Trigger, hash string, c change) error {
	return writeTriggerChangeIn(s.store, t, hash, c)
}

// writeTriggerChangeIn is writeTriggerChange, in st. A trigger written since
// its writer read it, by a tick or by a request on another Runner of the
// same store, is not written over (D17): the request is answered as for
// another revision.
func writeTriggerChangeIn(st runStore, t Trigger, hash string, c change) error {
	err := st.update(func(tx runTx) (err error) {
		entry := c
		if entry.from, err = triggerBefore(tx, t.ID, t.Revision-1); err != nil {
			return err
		}
		if err = tx.putTrigger(t, hash, true); err != nil {
			return err
		}
		_, err = tx.appendJournal(entry)
		return err
	})
	if faultOf(err) == faultConflict {
		return &apiError{409, "trigger_changed", "The trigger changed. Reload before updating it."}
	}
	return err
}
func (s *Service) rotateTriggerKey(triggerID string, revision int) (Trigger, string, error) {
	return s.rotateTriggerKeyBy(s.store, requester{}, triggerID, revision)
}

// rotateTriggerKeyBy is rotateTriggerKey, requested by by, its records read
// and written in st, the store the request is held to (storeFor).
func (s *Service) rotateTriggerKeyBy(st runStore, by requester, triggerID string, revision int) (Trigger, string, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Trigger{}, "", manualUnsupported()
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	t, _, e := st.trigger(triggerID)
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
	job, e := st.job(t.JobID)
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
	e = writeTriggerChangeIn(st, t, digest([]byte(secret)), change{kind: "trigger.key-rotated", by: s.actor(by), concerns: EventConcerns{Job: job.ID, Release: job.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: state, to: state, detail: EventDetail{Scope: job.Scope, PreviousRevision: revisionValue(t.Revision - 1)}})
	return t, secret, e
}
func (s *Service) event(triggerID, token string, event EventDelivery) (Occurrence, bool, error) {
	return s.eventIn(s.store, triggerID, token, event)
}

// eventIn is event, its records read and written in st, the store the
// request is held to (storeFor).
func (s *Service) eventIn(st runStore, triggerID, token string, event EventDelivery) (Occurrence, bool, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Occurrence{}, false, manualUnsupported()
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	t, hash, e := st.trigger(triggerID)
	if e != nil {
		return Occurrence{}, false, &apiError{401, "invalid_trigger_token", "The event credential is invalid."}
	}
	if len(token) != 64 || hash == "" || subtle.ConstantTimeCompare([]byte(digest([]byte(token))), []byte(hash)) != 1 {
		return Occurrence{}, false, &apiError{401, "invalid_trigger_token", "The event credential is invalid."}
	}
	// The credential is authenticated: a refusal from here on is its own,
	// named by the revision its key was issued at, whatever happens to the
	// key before the refusal is recorded.
	o, replay, e := s.admitBoundEvent(st, t, hash, event, "", requester{})
	if e != nil {
		e = &credentialRefusal{err: e, by: credentialActor(t.ID, t.keyRevision)}
	}
	return o, replay, e
}

// deliverRecord is called only by the authenticated local control API. The caller
// persists the complete envelope before delivery; old timestamps are allowed, but
// source freshness and queue expiry still apply at admission/execution.
func (s *Service) deliverRecord(triggerID, releaseID string, event EventDelivery) (Occurrence, bool, error) {
	return s.deliverRecordBy(s.store, requester{}, triggerID, releaseID, event)
}

// deliverRecordBy is deliverRecord, for a delivery by: the occurrence it
// admits is published by by. Its records are read and written in st, the
// store the request is held to (storeFor).
func (s *Service) deliverRecordBy(st runStore, by requester, triggerID, releaseID string, event EventDelivery) (Occurrence, bool, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Occurrence{}, false, manualUnsupported()
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	t, _, e := st.trigger(triggerID)
	if e != nil {
		return Occurrence{}, false, e
	}
	if releaseID == "" {
		return Occurrence{}, false, bad("release_required", "Bind this delivery to its reviewed release.")
	}
	return s.admitBoundEvent(st, t, "", event, releaseID, by)
}

// admitBoundEvent admits an event delivery in st: one bound to
// expectedRelease, a record delivery, is published by by. The queue's limit
// is the installation's (D9): the pending occurrences are counted for every
// scope, in any view.
func (s *Service) admitBoundEvent(st runStore, t Trigger, hash string, event EventDelivery, expectedRelease string, by requester) (Occurrence, bool, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Occurrence{}, false, manualUnsupported()
	}
	if t.Config.Kind != "event" {
		return Occurrence{}, false, bad("not_event_trigger", "This trigger does not accept event delivery.")
	}
	if event.ID == "" || len(event.ID) > 128 {
		return Occurrence{}, false, bad("invalid_event", "Supply a stable event ID up to 128 bytes.")
	}
	// Delivery identity is stable across trigger configuration revisions.
	var envelope any = event
	if expectedRelease != "" {
		envelope = map[string]any{"event": event, "releaseId": expectedRelease}
	}
	raw, e := canonical(encode(envelope))
	if e != nil {
		return Occurrence{}, false, e
	}
	payload := digest(raw)
	old, held, e := st.identifiedOccurrence(t.ID, "event:"+event.ID)
	if e == nil {
		if held != payload {
			return old, false, &apiError{409, "event_conflict", "This event ID was already received with a different payload."}
		}
		old.Input = nil
		return old, true, nil
	}
	if !absent(e) {
		return old, false, e
	}
	if s.unhealthy.Load() {
		return old, false, &apiError{503, "runner_unavailable", "The runner requires a restart before accepting new events."}
	}
	if t.Paused {
		return old, false, &apiError{409, "trigger_paused", "This trigger is paused; the event was not accepted."}
	}
	at, e := time.Parse(time.RFC3339, event.OccurredAt)
	if e != nil || time.Until(at) > 30*time.Second || expectedRelease == "" && time.Since(at) > 5*time.Minute {
		return old, false, bad("event_expired", "Supply an event timestamp from the last five minutes.")
	}
	if event.Input.Preparation != nil {
		return old, false, bad("invalid_event", "Supply original inputs, not caller-computed preparation.")
	}
	job, e := st.job(t.JobID)
	if e != nil {
		return old, false, e
	}
	if expectedRelease != "" && job.ReleaseID != expectedRelease {
		return old, false, &apiError{409, "release_changed", "The job no longer uses the reviewed release."}
	}
	release, e := st.release(job.ReleaseID)
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
	var publisher *EventActor
	if expectedRelease != "" {
		actor := s.actor(by)
		publisher = &actor
	}
	var admitted Occurrence
	e = st.update(func(tx runTx) error {
		a, err := s.admitOccurrenceAs(tx, t, o, "event:"+event.ID, payload, publisher)
		if err != nil {
			return err
		}
		// An event delivery is refused while the queue is full, not recorded as
		// skipped: a skipped occurrence would answer its event ID for good, and
		// the event, a record revision or a sender's, could never be delivered.
		// The transaction is not committed, so nothing is admitted and the ID
		// stays unused; the caller journals the refusal. The same bytes delivered
		// again are admitted once the queue has room, a credential's while its
		// occurredAt is in the window above. A schedule, a file change or a cloud
		// signal has no sender to refuse, and is still recorded skipped.
		if a.State == "skipped" && a.Reason == "queue-full" {
			if expectedRelease != "" {
				return &apiError{429, "queue_full", "The local queue is full. Deliver the same record again after queued occurrences are submitted."}
			}
			return &apiError{429, "queue_full", "The local queue is full. Deliver the same event again after queued occurrences are submitted, while its occurredAt is within the last five minutes."}
		}
		// The occurrence is bound to the credential that created it, for that
		// credential's read of its result.
		if hash != "" {
			if err = tx.createOccurrenceKey(a.ID, hash); err != nil {
				return err
			}
		}
		admitted = a
		return nil
	})
	if e != nil {
		return Occurrence{}, false, e
	}
	admitted.Input = nil
	return admitted, false, nil
}

// eventResult answers a trigger token about an occurrence that token created:
// its state and, once its run completed, the disposition and handoff target.
// Nothing else: no inputs, no identifiers of other records, no listing. The
// token must be its trigger's current credential and the one that created the
// occurrence; knowing an occurrence ID is not enough. Every other case gets one
// refusal, which does not say whether the occurrence exists.
//
// Its records are read in st, the store the request is held to (storeFor).
func (s *Service) eventResult(st runStore, triggerID, occurrenceID, token string) (map[string]any, error) {
	_, hash, e := st.trigger(triggerID)
	if e != nil || len(token) != 64 || hash == "" || subtle.ConstantTimeCompare([]byte(digest([]byte(token))), []byte(hash)) != 1 {
		return nil, &apiError{401, "invalid_trigger_token", "The event credential is invalid."}
	}
	// One statement, so a rotation after the check above still refuses.
	o, e := st.keyedOccurrence(occurrenceID, triggerID, hash)
	if absent(e) {
		return nil, &apiError{404, "occurrence_not_found", "No occurrence with this ID was created with this credential."}
	}
	if e != nil {
		return nil, e
	}
	answer := map[string]any{"id": o.ID, "state": o.State}
	if o.RunID == "" {
		return answer, nil
	}
	run, e := st.run(o.RunID)
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
	return Occurrence{PreparationSeconds: t.Config.PreparationSeconds, QueueSeconds: t.Config.QueueSeconds, ID: id("occ_"), JobID: j.ID, ReleaseID: j.ReleaseID, JobRevision: j.Revision, TriggerID: t.ID, TriggerRevision: t.Revision, Kind: t.Config.Kind, ReceivedAt: at.UTC().Format(time.RFC3339Nano), ExpiresAt: at.Add(time.Duration(t.Config.QueueSeconds) * time.Second).UTC().Format(time.RFC3339Nano), State: "accepted", Scope: j.Scope}
}
func (s *Service) findOccurrence(trigger, identity string) (Occurrence, string, error) {
	return s.store.identifiedOccurrence(trigger, identity)
}

// admitOccurrence admits o, as a schedule, a file change or a cloud signal
// does, and answers the occurrence it recorded.
func (s *Service) admitOccurrence(tx runTx, t Trigger, o Occurrence, identity, payload string) (Occurrence, error) {
	return s.admitOccurrenceAs(tx, t, o, identity, payload, nil)
}

// admitOccurrenceAs records o, skipped when the queue is full or the job's
// overlap rule holds it, with its entry, and answers what it recorded. It runs
// inside a transaction the store may run again, so it works on its own copy
// of o and changes nothing of its caller's: every run decides the queue and
// the overlap from what it reads, as the first did.
func (s *Service) admitOccurrenceAs(tx runTx, t Trigger, o Occurrence, identity, payload string, publisher *EventActor) (Occurrence, error) {
	if o.State == "accepted" {
		pending, e := tx.pendingOccurrences()
		if e != nil {
			return Occurrence{}, e
		}
		active, e := tx.jobActivity(o.JobID)
		if e != nil {
			return Occurrence{}, e
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
	o = withOccurrenceClass(o)
	if e := tx.createOccurrence(o, identity, payload); e != nil {
		return Occurrence{}, e
	}
	// An event delivery is the credential's that the delivery carried, the
	// trigger's current key; a schedule, a file change or a cloud signal is
	// the trigger's, at its revision.
	by := triggerActor(t.ID, t.Revision)
	if o.Kind == "event" {
		by = credentialActor(t.ID, t.keyRevision)
	}
	if publisher != nil {
		by = *publisher
	}
	if _, e := tx.appendJournal(occurrenceChange(o, "", by)); e != nil {
		return Occurrence{}, e
	}
	return o, nil
}

// saveOccurrence records an occurrence, and the entry of its change of state,
// if any, in one transaction. Runner made the change. The write is fenced by
// the claim o is held under, and releases it (D18).
func (s *Service) saveOccurrence(o Occurrence) error {
	return s.changeOccurrence(o, "", runnerActor)
}

// keepOccurrence is saveOccurrence for a holder that goes on with the
// occurrence: the claim is held after the write, its lease kept (D18).
func (s *Service) keepOccurrence(o Occurrence, guard string) error {
	return changeOccurrenceAs(s.store, o, occurrenceExpected{state: guard, keep: true}, runnerActor)
}

// settleOccurrence is saveOccurrence for the holder of o's claim: a write
// refused, the claim having been lost to another Runner, wrote nothing, and is
// no error: the occurrence is that Runner's (D18).
func (s *Service) settleOccurrence(o Occurrence) error {
	if e := s.saveOccurrence(o); !errors.Is(e, errPreparationStopped) {
		return e
	}
	return nil
}

// claimOccurrence claims the occurrence held under id in state, for this
// Runner to prepare or dispatch it, and returns it as the store holds it,
// under the claim (D18). faultConflict when another Runner holds it, or it
// moved on from state; occurrenceLost says which errors those are.
func (s *Service) claimOccurrence(id, state string) (Occurrence, error) {
	claim, err := newOccurrenceClaim()
	if err != nil {
		return Occurrence{}, err
	}
	var o Occurrence
	err = s.store.update(func(tx runTx) error {
		var e error
		o, e = tx.claimOccurrence(id, state, claim)
		return e
	})
	return o, err
}

// occurrenceLost is nil for a claim's error that says the occurrence is not
// this Runner's to work: another Runner holds it, or it moved on or is gone
// since it was read. The Runner wrote nothing of it, and goes on to the next
// (#245). Any other error is err.
func occurrenceLost(err error) error {
	if faultOf(err) == faultConflict || absent(err) {
		return nil
	}
	return err
}
