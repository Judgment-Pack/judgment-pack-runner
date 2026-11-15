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
