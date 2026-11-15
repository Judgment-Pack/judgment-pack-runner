package runner

import (
	"context"
	"testing"
)

// takenFirst is a store in which another dispatcher claims the queued run
// each dispatch reads, between the read and the claim, as a second Runner on
// the same store does.
type takenFirst struct{ runStore }

func (q takenFirst) oldestQueued() (Run, error) {
	r, err := q.runStore.oldestQueued()
	if err != nil {
		return r, err
	}
	other := r
	other.State, other.StartedAt, other.Attempt = "running", now(), r.Attempt+1
	if err = changeQueuedRun(q.runStore, other); err != nil {
		return r, err
	}
	return r, nil
}

// A dispatcher that loses the run it read to another dispatcher says so, so
// that its worker reads the queue again at once rather than waiting up to a
// tick for its next read (#245); an empty queue still waits.
func TestADispatcherThatLosesARunReadsTheQueueAgainAtOnce(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	j, err := s.createJobBy(requester{}, "Raced", journalRelease(t, s, completingRuntime(t, cfg.Dir)).ID, "", nil)
	must(t, err)
	if _, _, err = s.submit(j.ID, "one", journalInput()); err != nil {
		t.Fatal(err)
	}
	inner := s.store
	s.store = takenFirst{inner}
	if d, err := s.dispatch(context.Background()); d != dispatchedLost || err != nil {
		t.Fatalf("a dispatcher that lost its run made %d, %v; want it lost, to read the queue again at once", d, err)
	}
	s.store = inner
	if d, err := s.dispatch(context.Background()); d != dispatchedNone || err != nil {
		t.Fatalf("an empty queue made %d, %v; want none, to wait", d, err)
	}
}

// The dispatcher claims each run under a claim of its own, for the attempt it
// makes, and finishes it under that claim (D3): on a kind that fences claims,
// the run is held under the claim the dispatcher made, a new one for each
// claim.
func TestTheDispatcherClaimsEachRunUnderItsOwnClaim(t *testing.T) {
	onTheMemoryKind(t)
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	runtime := completingRuntime(t, cfg.Dir)
	j, err := s.createJobBy(requester{}, "Claimed", journalRelease(t, s, runtime).ID, "", nil)
	must(t, err)
	claims := map[string]bool{}
	for _, key := range []string{"first", "second"} {
		r, _, err := s.submit(j.ID, key, journalInput())
		must(t, err)
		if more, err := s.dispatchNext(context.Background()); !more || err != nil {
			t.Fatal("nothing dispatched:", more, err)
		}
		done, err := s.run(r.ID)
		must(t, err)
		if done.State != "completed" {
			t.Fatalf("the run ended %s: %s", done.State, done.Problem)
		}
		m := memoryOf(s.store)
		m.mu.RLock()
		var held runClaim
		for _, row := range m.state.runs {
			if row.id == r.ID {
				held = row.claim
			}
		}
		m.mu.RUnlock()
		if len(held.token) != 32 || held.attempt != done.Attempt || claims[held.token] {
			t.Fatalf("the run is held under %+v, not a new claim for attempt %d", held, done.Attempt)
		}
		claims[held.token] = true
	}
}
