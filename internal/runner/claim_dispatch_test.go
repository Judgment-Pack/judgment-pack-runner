package runner

import (
	"context"
	"testing"
)

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
