package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	if got.Code != 200 || len(status.Limits) != 3 || status.Limits["triggers"] != 4 || status.Limits["pendingOccurrences"] != 2 || status.Limits["runQueue"] != defaultRunQueue {
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
	for _, limits := range []LimitSettings{{Triggers: maxTriggerLimit + 1}, {PendingOccurrences: maxPendingOccurrences + 1}, {Triggers: -1}, {RunQueue: maxRunQueue + 1}, {RunQueue: -1}} {
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

// A limits member with several limits refused answers the refusal of the
// first checked, triggers, then pendingOccurrences, then runQueue, every
// time, whatever order the members are read in.
func TestALimitsMemberWithSeveralLimitsRefusedAnswersTheFirstChecked(t *testing.T) {
	for raw, want := range map[string]string{
		`{"pendingOccurrences":0,"triggers":0}`:                       "limits.triggers is from 1 to 1000",
		`{"triggers":0,"pendingOccurrences":0}`:                       "limits.triggers is from 1 to 1000",
		`{"pendingOccurrences":0,"triggers":1001}`:                    "limits.triggers is from 1 to 1000",
		`{"triggers":1001,"pendingOccurrences":2001}`:                 "limits.triggers is from 1 to 1000",
		`{"runQueue":0,"pendingOccurrences":0,"triggers":1001}`:       "limits.triggers is from 1 to 1000",
		`{"runQueue":2001,"triggers":2,"pendingOccurrences":0}`:       "limits.pendingOccurrences is from 1 to 2000",
		`{"runQueue":-1,"pendingOccurrences":2001}`:                   "limits.pendingOccurrences is from 1 to 2000",
		`{"runQueue":2001,"triggers":1000,"pendingOccurrences":2000}`: "limits.runQueue is from 1 to 2000",
		`{"runQueue":0}`: "limits.runQueue is from 1 to 2000",
	} {
		for range 32 {
			if _, e := ParseLimitSettings(json.RawMessage(raw)); e == nil || e.Error() != want {
				t.Fatalf("%s: %v, want %q", raw, e, want)
			}
		}
	}
	for raw, want := range map[string]LimitSettings{
		`{"runQueue":1}`:    {RunQueue: 1},
		`{"runQueue":2000}`: {RunQueue: maxRunQueue},
		`{"triggers":1000,"pendingOccurrences":2000,"runQueue":500}`: {Triggers: maxTriggerLimit, PendingOccurrences: maxPendingOccurrences, RunQueue: 500},
	} {
		if got, e := ParseLimitSettings(json.RawMessage(raw)); e != nil || got != want {
			t.Fatalf("%s: %+v, %v", raw, got, e)
		}
	}
	for _, raw := range []string{`{"runQueue":null}`, `{"runQueue":1.5}`, `{"RunQueue":5}`, `{"runQueue":5,"runQueue":6}`, `{"runQueue":"5"}`} {
		if _, e := ParseLimitSettings(json.RawMessage(raw)); e == nil || !strings.HasPrefix(e.Error(), "limits, when present, is an object of triggers, pendingOccurrences and runQueue") {
			t.Fatalf("%s: %v, want the member's refusal", raw, e)
		}
	}
}

// Section 9: the run queue holds the configured number of runs queued or
// running, not the default. A submission past it is refused, 429
// queue_full, and writes nothing; an accepted occurrence the clock reaches at
// the full queue waits as it was, its run not made, and is dispatched once
// the queue has room. GET /v1/status states the number.
func TestTheRunQueueHoldsItsConfiguredLimit(t *testing.T) {
	cfg := journalConfig(t)
	cfg.Limits = LimitSettings{RunQueue: 3}
	cfg.disableDispatcher = true
	cfg.disableAutomation = true
	s := openJournal(t, cfg)
	t.Cleanup(s.Close)
	j, e := s.createJob("Queued", journalRelease(t, s, completingRuntime(t, cfg.Dir)).ID)
	must(t, e)
	var status struct {
		Limits map[string]int `json:"limits"`
	}
	got := serve(t, s, "GET", "/v1/status", "", nil)
	must(t, json.Unmarshal(got.Body.Bytes(), &status))
	if status.Limits["runQueue"] != 3 {
		t.Fatalf("the status states the run queue as %v", status.Limits)
	}
	for i := range 3 {
		if got = serve(t, s, "POST", "/v1/jobs/"+j.ID+"/runs", string(encode(journalInput())), map[string]string{"Idempotency-Key": "fill-" + strconv.Itoa(i)}); got.Code != 202 {
			t.Fatalf("run %d of a queue of 3: %d %s", i+1, got.Code, got.Body.String())
		}
	}
	got = serve(t, s, "POST", "/v1/jobs/"+j.ID+"/runs", string(encode(journalInput())), map[string]string{"Idempotency-Key": "past"})
	if code, a := answerOf(t, got.Code, got.Body.Bytes()); code != 429 || a.Error.Code != "queue_full" {
		t.Fatalf("a run past a queue of 3: %d %s", got.Code, got.Body.String())
	}
	var held int
	must(t, sqliteDB(s).QueryRow("SELECT count(*) FROM runs").Scan(&held))
	if held != 3 {
		t.Fatalf("the store holds %d runs, want 3", held)
	}

	tr, e := s.configureTrigger(j.ID, "", 0, constantTrigger(time.Now()))
	must(t, e)
	o := newOccurrence(tr, j, time.Now())
	input := journalInput()
	o.Input, o.InputDigest = &input, digest(encode(input))
	admit(t, s, tr, &o, "full")
	must(t, s.automationTick(time.Now()))
	if waits := storedOccurrence(t, s, o.ID); waits.State != "accepted" || waits.RunID != "" {
		t.Fatalf("the occurrence did not wait at the full queue: %+v", waits)
	}
	// One run ends: the queue has room for the occurrence's.
	_, e = s.dispatchNext(context.Background())
	must(t, e)
	must(t, s.automationTick(time.Now()))
	if moved := storedOccurrence(t, s, o.ID); moved.State != "submitted" || moved.RunID == "" {
		t.Fatalf("the occurrence was not dispatched once the queue had room: %+v", moved)
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

// Section 8: a tick's fan-out holds the automation lock for one trigger at a
// time. A request made after the fan-out's first trigger, here the pause of
// its last, is served before the fan-out reaches that trigger, which it
// reads again and passes by; with the lock held for the whole fan-out the
// request would wait for its end, and the paused trigger would have been
// ticked. The test waits on the request's answer, not on a time.
func TestASharedTickLetsARequestInBetweenItsTriggers(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
	tick := w.at(time.Now()).UTC().Truncate(15 * time.Minute)
	const n = 8
	var due []Trigger
	for range n {
		due = append(due, w.trigger("", sharedSchedule("15m", ""), true, tick))
	}
	last, _ := storedTrigger(t, w.s, due[n-1].ID)
	paused := make(chan error, 1)
	var once sync.Once
	served := false
	w.s.betweenSharedTicks = func() {
		once.Do(func() {
			go func() {
				_, _, e := w.s.setTriggerStateBy(w.s.store, requester{}, last.ID, last.Revision, true, false)
				paused <- e
			}()
			select {
			case e := <-paused:
				must(t, e)
				served = true
			case <-time.After(10 * time.Second):
			}
		})
	}
	r, e := w.s.sharedTick(TickRequest{Interval: "15m", At: ticked(tick)}, requester{})
	w.s.betweenSharedTicks = nil
	must(t, e)
	if !served {
		<-paused
		t.Fatal("the pause waited for the fan-out's end")
	}
	if r.Occurrences != n-1 {
		t.Fatalf("the tick recorded %d occurrences, want %d: the paused trigger ticked", r.Occurrences, n-1)
	}
	for i, tr := range due {
		if got := len(w.occurrencesOf(tr.ID)); got != map[bool]int{true: 0, false: 1}[i == n-1] {
			t.Fatalf("trigger %d admitted %d occurrences", i, got)
		}
	}
}

// Section 5: two calls for one tick at the same time on one Runner share
// the triggers due: each trigger admits one occurrence, and the two answers'
// counts add up to the triggers due.
func TestTwoCallsForOneTickAtOnceAdmitEachTriggerOnce(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
	tick := w.at(time.Now()).UTC().Truncate(15 * time.Minute)
	const n = 16
	var due []Trigger
	for range n {
		due = append(due, w.trigger("", sharedSchedule("15m", ""), true, tick))
	}
	start := make(chan struct{})
	answers := make([]*httptest.ResponseRecorder, 2)
	var calls sync.WaitGroup
	for i := range answers {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-start
			answers[i] = w.tick("15m", ticked(tick))
		}()
	}
	close(start)
	calls.Wait()
	doc := readOpenAPI(t)
	total := 0
	for _, got := range answers {
		total += tickResult(t, doc, got).Occurrences
	}
	if total != n {
		t.Fatalf("the two calls answered %d occurrences in all, want %d", total, n)
	}
	for i, tr := range due {
		if got := len(w.occurrencesOf(tr.ID)); got != 1 {
			t.Fatalf("trigger %d admitted %d occurrences", i, got)
		}
	}
}
