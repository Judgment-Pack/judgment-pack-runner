package runner

// Shared ticks on a postgresql store with several Runners
// (JPACK_TEST_POSTGRESQL; docs/design/schedules.md, section 5, and
// docs/design/store-postgresql.md, D17 and D18).

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two Runners of one store called for the same shared tick at once, each
// having read the trigger at the same version: one admits the due time's
// occurrence, the other's write meets the version moved on and records
// nothing, and neither fails. A later call for the tick, on either Runner,
// records nothing; one naming a scope identifier is refused.
func TestPostgreSQLTwoRunnersCalledForOneSharedTickMakeOneOccurrence(t *testing.T) {
	p := pgRunners(t)
	runners := [2]*Service{p.open(), p.open()}
	for _, sv := range runners {
		sv.cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m"}}
	}
	got := serve(t, runners[0], "POST", "/v1/jobs/"+p.job.ID+"/triggers?scope=team-a", string(encode(map[string]any{"config": sharedSchedule("15m", "")})), nil)
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var created Trigger
	must(t, json.Unmarshal(got.Body.Bytes(), &created))
	if got = serve(t, runners[0], "POST", "/v1/triggers/"+created.ID+"/state?scope=team-a", string(encode(map[string]any{"revision": created.Revision, "paused": false, "reviewed": true})), nil); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	tick := time.Now().UTC().Truncate(15 * time.Minute)
	for _, sv := range runners {
		sv.clock = func() time.Time { return tick }
	}
	held, hash := storedTrigger(t, runners[0], created.ID)
	held.NextAt = tick.Format(time.RFC3339Nano)
	must(t, runners[0].writeTrigger(held, hash, false))

	stores := [2]runStore{runners[0].store, runners[1].store}
	var read sync.WaitGroup
	read.Add(len(runners))
	for _, sv := range runners {
		sv.store = &tickRace{runStore: sv.store, read: &read}
	}
	var results [2]TickResult
	var errs [2]error
	var wg sync.WaitGroup
	for i, sv := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = sv.sharedTick(TickRequest{Interval: "15m", At: ticked(tick)}, requester{})
		}()
	}
	wg.Wait()
	for i, sv := range runners {
		sv.store = stores[i]
		if errs[i] != nil {
			t.Fatalf("the tick on Runner %d failed: %v", i, errs[i])
		}
	}
	if n := results[0].Occurrences + results[1].Occurrences; n != 1 {
		t.Fatalf("one tick on two Runners recorded %d occurrences: %+v", n, results)
	}
	items := p.occurrences(runners[1])
	if len(items) != 1 || items[0].State != "accepted" || items[0].ScheduledAt != tick.Format(time.RFC3339Nano) {
		t.Fatalf("one tick made %+v", items)
	}
	if after, _ := storedTrigger(t, runners[1], created.ID); after.NextAt != tick.Add(15*time.Minute).Format(time.RFC3339Nano) {
		t.Fatal("the trigger is next due", after.NextAt)
	}
	for _, sv := range runners {
		got = serve(t, sv, "POST", "/v1/ticks", string(encode(map[string]string{"interval": "15m", "at": ticked(tick)})), nil)
		if got.Code != 200 || !strings.Contains(got.Body.String(), `"occurrences":0`) {
			t.Fatalf("the tick again: %d %s", got.Code, got.Body.String())
		}
	}
	if got = serve(t, runners[0], "POST", "/v1/ticks?scope=team-a", `{"interval":"15m"}`, nil); got.Code != 400 || !strings.Contains(got.Body.String(), "invalid_scope") {
		t.Fatalf("a tick naming a scope identifier: %d %s", got.Code, got.Body.String())
	}
	if items = p.occurrences(runners[0]); len(items) != 1 {
		t.Fatalf("the later calls made %+v", items)
	}
}

// Section 6 and D15: a store migrated to the file before
// 0005_schedules.sql is refused at the start until it is migrated, and the
// migration then applies that file alone.
func TestPostgreSQLRefusesAStoreWithoutTheSchedulesFile(t *testing.T) {
	pgRefusesAStoreWithout(t, "0005_")
}

// Section 9 and D9: a Runner on the kind holds its configured limits, not
// the defaults. counts.triggers, the installation's count, is raised past
// 128 up to the configured limit and no further: the Runner's own read
// refuses the trigger past it, 422 trigger_limit stating the configured
// number, and the row itself refuses a trigger written past it. A tick that
// finds more triggers due than the configured queue has room for records
// the rest skipped, reason queue-full.
func TestPostgreSQLHoldsTheConfiguredLimits(t *testing.T) {
	p := pgRunners(t)
	p.limits = LimitSettings{Triggers: 130, PendingOccurrences: 2}
	sv := p.open()
	sv.cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m"}}
	held := func() int {
		t.Helper()
		var n int
		must(t, p.suite.QueryRowContext(pgContext, "SELECT value FROM counts WHERE name='triggers'").Scan(&n))
		return n
	}
	configure := func(want int) []byte {
		t.Helper()
		got := serve(t, sv, "POST", "/v1/jobs/"+p.job.ID+"/triggers?scope=team-a", string(encode(map[string]any{"config": sharedSchedule("15m", "")})), nil)
		if got.Code != want {
			t.Fatalf("a trigger at the installation's count %d: %d %s, want %d", held(), got.Code, got.Body.String(), want)
		}
		return got.Body.Bytes()
	}
	// The count stands as if 126 triggers were held: the third configured
	// from here is the installation's 129th, past the default.
	_, err := p.suite.ExecContext(pgContext, "UPDATE counts SET value=$1 WHERE name='triggers'", defaultTriggerLimit-2)
	must(t, err)
	var made []Trigger
	for range 4 {
		var tr Trigger
		must(t, json.Unmarshal(configure(200), &tr))
		made = append(made, tr)
	}
	if n := held(); n != 130 {
		t.Fatal("the installation's trigger count is", n, "want 130")
	}
	if got := string(configure(422)); !strings.Contains(got, `"code":"trigger_limit"`) || !strings.Contains(got, "This local runner supports up to 130 triggers.") {
		t.Fatal("the trigger past the configured limit:", got)
	}
	// The row itself, past the Runner's own read, written as a request in
	// the job's scope writes it.
	err = sv.store.heldTo(scopeHeld(p.job.Scope)).update(func(tx runTx) error {
		return tx.createTrigger(Trigger{ID: id("trg_"), JobID: p.job.ID, Revision: 1, Authority: "local", Config: sharedSchedule("15m", ""), Paused: true, CreatedAt: now(), UpdatedAt: now(), Scope: p.job.Scope})
	})
	if faultOf(err) != faultRefused || !strings.Contains(err.Error(), "the installation holds its limit of triggers") || held() != 130 {
		t.Fatalf("a trigger written past the configured limit: %v, the count %d", err, held())
	}

	// The queue: three triggers due at one tick, and room for two.
	tick := time.Now().UTC().Truncate(15 * time.Minute)
	sv.clock = func() time.Time { return tick }
	for _, tr := range made[:3] {
		if got := serve(t, sv, "POST", "/v1/triggers/"+tr.ID+"/state?scope=team-a", string(encode(map[string]any{"revision": tr.Revision, "paused": false, "reviewed": true})), nil); got.Code != 200 {
			t.Fatal(got.Code, got.Body.String())
		}
		enabled, hash := storedTrigger(t, sv, tr.ID)
		enabled.NextAt = tick.Format(time.RFC3339Nano)
		must(t, sv.writeTrigger(enabled, hash, false))
	}
	r, err := sv.sharedTick(TickRequest{Interval: "15m", At: ticked(tick)}, requester{})
	must(t, err)
	var accepted, full int
	must(t, p.suite.QueryRowContext(pgContext, "SELECT count(*) FILTER (WHERE state='accepted'), count(*) FILTER (WHERE record::json->>'reason'='queue-full') FROM occurrences").Scan(&accepted, &full))
	if r.Occurrences != 3 || accepted != 2 || full != 1 {
		t.Fatalf("the tick recorded %d: %d accepted and %d skipped queue-full, at a queue of 2", r.Occurrences, accepted, full)
	}
}

// countRaced is a store whose reads of the trigger count, its own, its
// views' and its transactions', are answered through after: a test moves
// the installation's count, or answers it as it stood, between the
// Runner's read and its write.
type countRaced struct {
	runStore
	after func(int) int
}

func (c *countRaced) leasesClaims() bool { return leases(c.runStore) }

func (c *countRaced) holdsScopes() bool { return holdsScopes(c.runStore) }

func (c *countRaced) updateWithin(d time.Duration, fn func(runTx) error) error {
	return updateWithin(c.runStore, d, fn)
}

func (c *countRaced) heldTo(sc storeScope) runStore {
	return &countRaced{runStore: c.runStore.heldTo(sc), after: c.after}
}

func (c *countRaced) triggerCount() (int, error) {
	n, err := c.runStore.triggerCount()
	return c.after(n), err
}

func (c *countRaced) update(fn func(runTx) error) error {
	return c.runStore.update(func(tx runTx) error { return fn(&countRacedTx{runTx: tx, after: c.after}) })
}

type countRacedTx struct {
	runTx
	after func(int) int
}

func (c *countRacedTx) triggerCount() (int, error) {
	n, err := c.runTx.triggerCount()
	return c.after(n), err
}

// Section 9 and D9: a trigger creation that read the installation's count
// below the limit, before another Runner's creation at the limit committed,
// meets the count's row, and is refused as its read would have refused it,
// 422 trigger_limit stating the configured number, configured or made with
// its job; nothing is written, and the count stays at the limit.
func TestPostgreSQLATriggerCreationRacedToTheLimitIsRefusedAsTheLimit(t *testing.T) {
	p := pgRunners(t)
	p.limits = LimitSettings{Triggers: 130}
	sv := p.open()
	setCount := func(n int) {
		t.Helper()
		_, err := p.suite.ExecContext(pgContext, "UPDATE counts SET value=$1 WHERE name='triggers'", n)
		must(t, err)
	}
	held := func(query string) int {
		t.Helper()
		var n int
		must(t, p.suite.QueryRowContext(pgContext, query).Scan(&n))
		return n
	}
	inner := sv.store
	t.Cleanup(func() { sv.store = inner })

	// Configured: the Runner reads 129, and another Runner's creation
	// raises the count to 130 before this one's transaction.
	setCount(129)
	sv.store = &countRaced{runStore: inner, after: func(n int) int {
		setCount(130)
		return n
	}}
	got := serve(t, sv, "POST", "/v1/jobs/"+p.job.ID+"/triggers?scope=team-a", string(encode(map[string]any{"config": eventTriggerConfig()})), nil)
	if code, a := answerOf(t, got.Code, got.Body.Bytes()); code != 422 || a.Error.Code != "trigger_limit" || a.Error.Message != "This local runner supports up to 130 triggers." {
		t.Fatalf("a trigger configured past the limit another Runner reached: %d %s", got.Code, got.Body.String())
	}

	// Made with its job: the Runner's read in the transaction answers the
	// count as it stood before the other Runner's creation committed.
	sv.store = inner
	release := journalRelease(t, sv, p.runtime)
	sv.store = &countRaced{runStore: inner, after: func(n int) int { return n - 1 }}
	c := eventTriggerConfig()
	_, e := sv.createJobWith(requester{}, "Raced to the limit", release.ID, "team-b", &c, nil)
	refusedTriggerLimit(t, e, "130")
	sv.store = inner

	if n := held("SELECT value FROM counts WHERE name='triggers'"); n != 130 {
		t.Fatal("the installation's trigger count is", n, "want 130")
	}
	if n := held("SELECT count(*) FROM triggers"); n != 0 {
		t.Fatal("the refused creations wrote", n, "triggers")
	}
	if n := held("SELECT count(*) FROM jobs"); n != 1 {
		t.Fatal("the refused job creation wrote a job:", n, "jobs held")
	}
}

// Section 8: one tick at the configured limits on the kind, every trigger's
// occurrence accepted, none skipped queue-full, counts.triggers raised past
// the default as each trigger is written. It runs at 200, not at the
// maximums, whose cost section 8 states.
func TestPostgreSQLASharedTickAtTheConfiguredLimitsEnqueuesEveryDueTrigger(t *testing.T) {
	const jobs = 200
	p := pgRunners(t)
	p.limits = LimitSettings{Triggers: jobs, PendingOccurrences: jobs}
	sv := p.open()
	sv.cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m"}}
	first, repeated := tickAtTheLimits(t, sv, jobs, func(i int) Job {
		j, e := sv.createJobWith(requester{}, "Ticked", journalRelease(t, sv, p.runtime).ID, []string{"team-a", "team-b"}[i%2], nil, nil)
		must(t, e)
		return j
	}, func() (accepted, full int) {
		must(t, p.suite.QueryRowContext(pgContext, "SELECT count(*) FILTER (WHERE state='accepted'), count(*) FILTER (WHERE record::json->>'reason'='queue-full') FROM occurrences").Scan(&accepted, &full))
		return accepted, full
	})
	t.Logf("one tick of %d due triggers on the postgresql kind: %s (%s each); the same tick again: %s (this machine's figure, not a bound)", jobs, first.Round(time.Millisecond), (first / jobs).Round(time.Microsecond), repeated.Round(time.Millisecond))
}
