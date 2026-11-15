package runner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The installation's limits as the operator configures them
// (docs/design/schedules.md, sections 8 and 9).

// limitsWorld is a tickWorld whose Runner is configured with limits.
func limitsWorld(t *testing.T, limits LimitSettings) *tickWorld {
	t.Helper()
	cfg := journalConfig(t)
	cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m"}}
	cfg.Limits = limits
	w := &tickWorld{t: t, cfg: cfg, s: openJournal(t, cfg)}
	t.Cleanup(func() { w.s.Close() })
	w.runtime = completingRuntime(t, cfg.Dir)
	return w
}

// refusedTriggerLimit says that e is the trigger limit's refusal, stating
// limit.
func refusedTriggerLimit(t *testing.T, e error, limit string) {
	t.Helper()
	var api *apiError
	if !errors.As(e, &api) || api.Status != 422 || api.Code != "trigger_limit" || api.Message != "This local runner supports up to "+limit+" triggers." {
		t.Fatalf("%v, want 422 trigger_limit stating %s", e, limit)
	}
}

// A Runner holds its configured limits, not the defaults, and states them:
// GET /v1/status names them; the trigger past the limit is refused,
// configured or made with its job, 422 trigger_limit stating the configured
// number; a tick that finds more due than the queue has room for records
// the rest skipped, reason queue-full; and an event delivery at the full
// queue is refused, 429 queue_full stating the configured number.
func TestAStoreAtItsConfiguredLimitsRefusesTheNextTriggerAndOccurrence(t *testing.T) {
	w := limitsWorld(t, LimitSettings{Triggers: 4, PendingOccurrences: 2})
	got := serve(t, w.s, "GET", "/v1/status", "", nil)
	var status struct {
		Limits map[string]int `json:"limits"`
	}
	must(t, json.Unmarshal(got.Body.Bytes(), &status))
	if got.Code != 200 || len(status.Limits) != 2 || status.Limits["triggers"] != 4 || status.Limits["pendingOccurrences"] != 2 {
		t.Fatalf("the status states the limits as %d %v", got.Code, status.Limits)
	}

	tick := w.at(time.Now()).UTC().Truncate(15 * time.Minute)
	var due []Trigger
	for range 3 {
		due = append(due, w.trigger("", sharedSchedule("15m", ""), true, tick))
	}
	ev, e := w.s.configureTrigger(w.job("").ID, "", 0, eventTriggerConfig())
	must(t, e)
	ev, key, e := w.s.setTriggerState(ev.ID, ev.Revision, false, true)
	must(t, e)
	_, e = w.s.configureTrigger(w.job("").ID, "", 0, eventTriggerConfig())
	refusedTriggerLimit(t, e, "4")
	c := eventTriggerConfig()
	_, e = w.s.createJobWith(requester{}, "Past the limit", journalRelease(t, w.s, w.runtime).ID, "", &c, nil)
	refusedTriggerLimit(t, e, "4")

	r := tickResult(t, readOpenAPI(t), w.tick("15m", ticked(tick)))
	if r.Occurrences != len(due) {
		t.Fatalf("the tick recorded %d occurrences of %d triggers due", r.Occurrences, len(due))
	}
	var accepted, full int
	must(t, sqliteDB(w.s).QueryRow("SELECT count(CASE WHEN json_extract(record,'$.state')='accepted' THEN 1 END), count(CASE WHEN json_extract(record,'$.reason')='queue-full' THEN 1 END) FROM occurrences").Scan(&accepted, &full))
	if accepted != 2 || full != 1 {
		t.Fatalf("%d accepted and %d skipped queue-full, at a queue of 2", accepted, full)
	}
	got = serve(t, w.s, "POST", "/v1/triggers/"+ev.ID+"/events", string(encode(delivery("past-the-queue"))), map[string]string{"X-Trigger-Token": key})
	if code, a := answerOf(t, got.Code, got.Body.Bytes()); code != 429 || a.Error.Code != "queue_full" || !strings.HasPrefix(a.Error.Message, "The local queue is full, at its limit of 2 pending occurrences. ") {
		t.Fatalf("an event at the full queue: %d %s", got.Code, got.Body.String())
	}
}

// A limit out of its bounds is refused at the start, before anything is
// opened.
func TestALimitOutOfItsBoundsIsRefusedAtTheStart(t *testing.T) {
	for _, limits := range []LimitSettings{{Triggers: maxTriggerLimit + 1}, {PendingOccurrences: maxPendingOccurrences + 1}, {Triggers: -1}} {
		cfg := journalConfig(t)
		cfg.Limits = limits
		if s, e := Open(cfg); e == nil || !strings.HasPrefix(e.Error(), "limits.") {
			if s != nil {
				s.Close()
			}
			t.Errorf("%+v: %v, want the limit's refusal", limits, e)
		}
	}
}

// tickAtTheLimits gives each of n jobs, made by makeJob, one enabled
// shared-interval trigger due at one tick, written as the store writes a
// trigger (a kind that holds the trigger count in a row holds it to the
// Runner's configured limit); calls the tick for it once, then again; and
// holds that the first call accepted every trigger's occurrence, none
// skipped queue-full (states counts them), and the second recorded none. It
// answers the time of each call.
func tickAtTheLimits(t *testing.T, s *Service, n int, makeJob func(int) Job, states func() (accepted, full int)) (first, repeated time.Duration) {
	t.Helper()
	at := time.Now()
	s.clock = func() time.Time { return at }
	tick := at.UTC().Truncate(15 * time.Minute)
	c := sharedSchedule("15m", "")
	for i := range n {
		j := makeJob(i)
		tr := Trigger{ID: id("trg_"), JobID: j.ID, Revision: 1, Authority: "local", Config: c, CreatedAt: now(), UpdatedAt: now(), Scope: j.Scope, NextAt: tick.Format(time.RFC3339Nano)}
		// Held to the job's scope, as a request's write is: on a kind that
		// holds scopes the worker login makes no trigger.
		must(t, s.store.heldTo(scopeHeld(j.Scope)).update(func(tx runTx) error {
			if err := tx.createTrigger(tr); err != nil {
				return err
			}
			if err := tx.putTrigger(tr, "", true); err != nil {
				return err
			}
			return tx.raiseSchema(scheduleStoreSchema)
		}))
	}
	doc := readOpenAPI(t)
	call := func() (TickResult, time.Duration) {
		started := time.Now()
		got := serve(t, s, "POST", "/v1/ticks", string(encode(map[string]string{"interval": "15m", "at": ticked(tick)})), nil)
		took := time.Since(started)
		return tickResult(t, doc, got), took
	}
	r, first := call()
	again, repeated := call()
	if r.Occurrences != n || again.Occurrences != 0 {
		t.Fatalf("the tick of %d triggers due recorded %d, and again %d", n, r.Occurrences, again.Occurrences)
	}
	if accepted, full := states(); accepted != n || full != 0 {
		t.Fatalf("%d accepted and %d skipped queue-full of %d triggers due, at a queue of %d", accepted, full, n, s.cfg.Limits.pendingOccurrences())
	}
	return first, repeated
}

// Section 8: one tick at several hundred jobs, each with one
// shared-interval trigger due, on a Runner whose limits hold them all: every
// trigger's occurrence is accepted, none skipped queue-full. The test
// reports the cost, and holds what the tick did, never its time. It runs at
// 400, not at the maximums, whose cost section 8 states.
func TestASharedTickAtTheConfiguredLimitsEnqueuesEveryDueTrigger(t *testing.T) {
	const jobs = 400
	w := limitsWorld(t, LimitSettings{Triggers: jobs, PendingOccurrences: jobs})
	first, repeated := tickAtTheLimits(t, w.s, jobs, func(i int) Job {
		return w.job([]string{"", "scope-a", "scope-b", "scope-c"}[i%4])
	}, func() (accepted, full int) {
		must(t, sqliteDB(w.s).QueryRow("SELECT count(CASE WHEN json_extract(record,'$.state')='accepted' THEN 1 END), count(CASE WHEN json_extract(record,'$.reason')='queue-full' THEN 1 END) FROM occurrences").Scan(&accepted, &full))
		return accepted, full
	})
	t.Logf("one tick of %d due triggers on the sqlite kind: %s (%s each); the same tick again: %s (this machine's figure, not a bound)", jobs, first.Round(time.Millisecond), (first / jobs).Round(time.Microsecond), repeated.Round(time.Millisecond))
}
