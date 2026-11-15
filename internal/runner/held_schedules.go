package runner

import "errors"

// The triggers a start finds outside the operator's schedule settings
// (docs/design/schedules.md, section 9).
//
// The shared intervals and the bounds are the configuration's, not the
// store's: a trigger enabled under one configuration stays enabled under
// the next, and on several Runners of one store, under each Runner's. A
// trigger bound to a shared interval the configuration does not name is
// never ticked by this Runner (sharedTick refuses the interval), and one
// whose cron expression the bounds in force refuse goes on being ticked on
// its schedule until it is changed or enabled again, which the settings in
// force hold. Neither is changed, paused or refused at its tick: the start
// reports each, in the journal and in GET /v1/status.

// heldSchedules is what the start found, in the order the triggers were
// made: the enabled triggers bound to a shared interval this Runner's
// configuration does not name, and those whose cron expression is outside
// its bounds.
type heldSchedules struct {
	at                    string
	unknownSharedInterval []string
	outsideBounds         []string
}

// status is the finding as GET /v1/status states it: when the start
// checked, and each list's count and trigger ids.
func (h *heldSchedules) status() map[string]any {
	list := func(ids []string) map[string]any {
		return map[string]any{"count": len(ids), "triggers": append([]string{}, ids...)}
	}
	return map[string]any{"checkedAt": h.at, "unknownSharedInterval": list(h.unknownSharedInterval), "outsideBounds": list(h.outsideBounds)}
}

// outsideSettingsKind is the kind of the journal entry of a trigger the
// start found outside the settings.
const outsideSettingsKind = "trigger.outside-settings"

// checkHeldSchedules reads every enabled trigger of every scope, as a shared
// tick reads them, and holds each cron or shared-interval schedule to the
// settings in force, as configuring or enabling it does (checkSchedule).
// Each one refused is listed for GET /v1/status and journaled once, an
// entry by the Runner that names the trigger at its revision, its job, the
// refusal's code and what it means for the trigger; the scope identifier is
// the entry's own member, as on every entry about a job created under one,
// and not in its reason. The entries are one transaction, and nothing about
// the trigger is written.
func (s *Service) checkHeldSchedules() (*heldSchedules, error) {
	found := &heldSchedules{at: now()}
	ids, err := s.store.enabledTriggers(false)
	if err != nil {
		return nil, err
	}
	// Many triggers share one schedule: each is held to the settings once.
	refusals := map[[3]string]error{}
	var entries []change
	for _, id := range ids {
		t, _, err := s.trigger(id)
		if absent(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := t.Config.Schedule
		if t.Paused || t.Config.Kind != "schedule" || sc == nil || sc.Kind != "cron" && sc.Kind != "tick" {
			continue
		}
		key := [3]string{sc.Kind, sc.Tick, sc.Cron}
		refusal, held := refusals[key]
		if !held {
			refusal = s.cfg.Schedules.checkSchedule(sc)
			refusals[key] = refusal
		}
		if refusal == nil {
			continue
		}
		var api *apiError
		if !errors.As(refusal, &api) {
			return nil, refusal
		}
		job, err := s.job(t.JobID)
		if err != nil {
			return nil, err
		}
		consequence := " This Runner goes on ticking the trigger on its schedule; changing it, or enabling it again, is held to the settings in force."
		if api.Code == "unknown_shared_interval" {
			found.unknownSharedInterval = append(found.unknownSharedInterval, t.ID)
			consequence = " This Runner does not tick the trigger."
		} else {
			found.outsideBounds = append(found.outsideBounds, t.ID)
		}
		entries = append(entries, change{kind: outsideSettingsKind, by: runnerActor, concerns: EventConcerns{Job: job.ID, Release: job.ReleaseID, Trigger: t.ID}, revision: t.Revision, reason: api.Message + consequence, detail: EventDetail{Scope: job.Scope, Code: api.Code}})
	}
	if len(entries) == 0 {
		return found, nil
	}
	// The entries are written together, after the start's own.
	if err = s.store.update(func(tx runTx) error {
		for _, entry := range entries {
			if _, err := tx.appendJournal(entry); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}
