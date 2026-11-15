package runner

// Several Runners on one postgresql store (JPACK_TEST_POSTGRESQL) whose
// automation meets journal_head's lock, held by another connection past the
// lock bound: the store is busy, not failing, and no Runner's automation
// stops on it (tickLost, occurrenceLost).

import (
	"strings"
	"testing"
	"time"
)

// Two Runners tick a schedule trigger at its due time while another session
// holds journal_head's lock past the lock bound: each tick makes nothing and
// returns no error, so that neither Runner's automation stops, and the
// trigger is left as it was, its due time still due. Once the lock is let
// go, the next ticks admit the due time's one occurrence, dispatched once,
// and both Runners report themselves healthy.
func TestPostgreSQLTwoRunnersTickingBehindAHeldLockGoOn(t *testing.T) {
	shortPGBounds(t, 300*time.Millisecond, 5*time.Second, 30*time.Second)
	s := pgServer(t)
	db := s.database(t)
	p := &pgStoreRunners{t: t, suite: s.pool(t, pgRoleSuite, db), conn: s.connections(db)}
	runners := [2]*Service{p.open(), p.open()}
	tr, due := p.schedule(runners[0])
	before, _ := storedTrigger(t, runners[0], tr.ID)

	end := pgHoldJournal(t, s, db)
	for i, sv := range runners {
		if err := sv.automationTick(due.Add(time.Second)); err != nil {
			t.Fatalf("the tick of Runner %d behind a held lock returned an error, which stops its automation: %v", i, err)
		}
	}
	if items := p.occurrences(runners[0]); len(items) != 0 {
		t.Fatalf("a tick behind a held lock made %+v", items)
	}
	if held, _ := storedTrigger(t, runners[1], tr.ID); held.version != before.version || held.NextAt != before.NextAt {
		t.Fatalf("a tick behind a held lock wrote the trigger: version %d, next %s", held.version, held.NextAt)
	}
	end()

	for i, sv := range runners {
		if err := sv.automationTick(due.Add(2 * time.Second)); err != nil || sv.unhealthy.Load() {
			t.Fatalf("the tick of Runner %d after the lock was let go failed: %v", i, err)
		}
	}
	items := p.occurrences(runners[1])
	if len(items) != 1 {
		t.Fatalf("one due time made %d occurrences: %+v", len(items), items)
	}
	if at, err := time.Parse(time.RFC3339Nano, items[0].ScheduledAt); err != nil || !at.Equal(due) || items[0].MissedFrom != "" {
		t.Fatalf("the occurrence is not the due time's, on time: %+v", items[0])
	}
	if items[0].State != "submitted" || items[0].RunID == "" {
		t.Fatalf("the occurrence was not dispatched: %+v", items[0])
	}
	if n := entriesOfKind(t, runners[0].store, "occurrence.submitted", items[0].ID); n != 1 {
		t.Fatal("the occurrence was submitted", n, "times")
	}
	for i, sv := range runners {
		if got := serve(t, sv, "GET", "/v1/status", "", nil); got.Code != 200 || !strings.Contains(got.Body.String(), `"healthy":true`) {
			t.Fatalf("Runner %d is not healthy: %d %s", i, got.Code, got.Body.String())
		}
	}
}
