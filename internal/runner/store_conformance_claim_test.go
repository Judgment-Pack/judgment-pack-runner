package runner

// The conformance cases of a claim's fence (docs/design/store-postgresql.md,
// D3): every write a claimant makes is fenced by its claim's token, and by the
// claim's lease, which the store's clock holds. A kind that fences claims
// (fencesClaims: the in-memory kind, and the twice mode over it) is held to
// it here. The sqlite kind takes a claim and ignores it: its lock keeps every
// other dispatcher out (the sqlite kind's own case "a second opening of one
// state directory is refused"), and its cases say so.

import (
	"testing"
	"time"
)

// claimWorld is a store with one run claimed, as the dispatcher claims it.
func claimWorld(t *testing.T, k storeKindUnderTest) (storeWorld, Run) {
	t.Helper()
	w := newStoreWorld(t, k)
	_, err := w.enqueue("run_claimed", "key-claimed")
	must(t, err)
	r, ok, err := claim(w.st)
	if err != nil || !ok {
		t.Fatal("no run claimed:", ok, err)
	}
	if r.claim.token == "" || len(r.claim.token) != 32 || r.claim.attempt != r.Attempt {
		t.Fatalf("the claim is not a new token of 16 bytes for the attempt: %+v", r.claim)
	}
	return w, r
}

// heldAs fails the test unless the store holds the run in state at attempt,
// and its journal holds entries entries.
func heldAs(t *testing.T, w storeWorld, id, state string, attempt, entries int) {
	t.Helper()
	r, err := w.st.run(id)
	must(t, err)
	if r.State != state || r.Attempt != attempt {
		t.Fatalf("the run is held %s at attempt %d, want %s at %d", r.State, r.Attempt, state, attempt)
	}
	if got := storeJournalLength(t, w.st); got != entries {
		t.Fatalf("the journal holds %d entries, want %d", got, entries)
	}
}

func storeJournalLength(t *testing.T, st runStore) int {
	t.Helper()
	items, _, err := st.journalPage(journalFilter{}, journalRange{limit: 100})
	must(t, err)
	return len(items)
}

// completed is r completed, with its record's bytes: a finish that appends
// the chain's entry.
func completed(r Run) Run {
	r.State, r.FinishedAt, r.AuditBytes = "completed", now(), []byte(`{"recordVersion":"1"}`)
	return r
}

var claimStoreCases = []storeCase{
	{"a write under another claim's token writes nothing", func(t *testing.T, k storeKindUnderTest) {
		if !k.fencesClaims {
			t.Skip("the kind takes a claim and ignores it: its lock is the fence")
		}
		w, r := claimWorld(t, k)
		entries := storeJournalLength(t, w.st)
		forged := r
		var err error
		if forged.claim, err = newRunClaim(r.Attempt); err != nil {
			t.Fatal(err)
		}
		wantFault(t, finishRunIn(w.st, completed(forged)), faultConflict, "a finish under another token")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewClaim(r.ID, forged.claim) }), faultConflict, "a renewal under another token")
		// The token and the attempt are the claim: the claim's token for
		// another attempt is another claim.
		other := r
		other.claim.attempt++
		wantFault(t, finishRunIn(w.st, completed(other)), faultConflict, "a finish under the token for another attempt")
		// No claim at all is not the claim either.
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewClaim(r.ID, runClaim{}) }), faultConflict, "a renewal under no claim")
		heldAs(t, w, r.ID, "running", r.Attempt, entries)
		if _, err := w.st.entryLine(r.ID); !absent(err) {
			t.Fatal("a refused finish appended the chain's entry:", err)
		}
		// The claimant's own token writes.
		must(t, w.st.update(func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }))
		must(t, finishRunIn(w.st, completed(r)))
		heldAs(t, w, r.ID, "completed", r.Attempt, entries+1)
	}},
	{"a finish or a renewal after the lease ran out writes nothing", func(t *testing.T, k storeKindUnderTest) {
		if !k.fencesClaims {
			t.Skip("the kind holds no lease: its lock is the fence")
		}
		w, r := claimWorld(t, k)
		entries := storeJournalLength(t, w.st)
		// A renewal holds the claim for a lease from the renewal, by the
		// store's clock: 45 seconds after the claim, then 45 after the
		// renewal, is within it.
		k.passTime(w.st, 45*time.Second)
		must(t, w.st.update(func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }))
		k.passTime(w.st, 45*time.Second)
		must(t, w.st.update(func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }))
		// The store's clock, not the process's, runs the lease out.
		k.passTime(w.st, claimLease)
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }), faultConflict, "a renewal after the lease ran out")
		wantFault(t, finishRunIn(w.st, completed(r)), faultConflict, "a finish after the lease ran out")
		failed := r
		failed.State, failed.FinishedAt, failed.Problem = "failed", now(), "Failed."
		wantFault(t, finishRunIn(w.st, failed), faultConflict, "a failure recorded after the lease ran out")
		heldAs(t, w, r.ID, "running", r.Attempt, entries)
		if _, err := w.st.entryLine(r.ID); !absent(err) {
			t.Fatal("a finish after the lease ran out appended the chain's entry:", err)
		}
	}},
	{"a run interrupted and retried is not overwritten by its first claimant", func(t *testing.T, k storeKindUnderTest) {
		w, first := claimWorld(t, k)
		// The run is interrupted, as a restart interrupts it: whatever claim
		// it was held under, which the interruption clears.
		interrupted := first
		interrupted.claim = runClaim{}
		interrupted.State, interrupted.FinishedAt, interrupted.InterruptedAt = "interrupted", now(), now()
		must(t, w.st.update(func(tx runTx) error {
			return recordRun(tx, interrupted, runExpected{state: anyState}, runnerActor, EventDetail{})
		}))
		entries := storeJournalLength(t, w.st)
		// Every kind: the first claimant's finish meets a run not running.
		wantFault(t, finishRunIn(w.st, completed(first)), faultConflict, "the first claimant's finish of an interrupted run")
		heldAs(t, w, first.ID, "interrupted", first.Attempt, entries)
		if !k.fencesClaims {
			// The rest needs a first claimant still running beside a second:
			// the sqlite kind's lock keeps the second out.
			return
		}
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewClaim(first.ID, first.claim) }), faultConflict, "the first claimant's renewal of an interrupted run")
		// A caller retries it, and a dispatcher claims attempt 2.
		next := interrupted
		next.State, next.StartedAt, next.FinishedAt, next.InterruptedAt = "queued", "", "", ""
		next.Attempts = []Attempt{{Number: first.Attempt, StartedAt: first.StartedAt, Outcome: "interrupted", Reason: runAttemptRestarted}}
		must(t, w.st.update(func(tx runTx) error {
			if err := tx.raiseSchema(retryStoreSchema); err != nil {
				return err
			}
			return recordRun(tx, next, runExpected{state: "interrupted"}, runnerActor, EventDetail{})
		}))
		second, ok, err := claim(w.st)
		if err != nil || !ok || second.ID != first.ID || second.Attempt != first.Attempt+1 {
			t.Fatal("the retried run is not claimed for its next attempt:", second.ID, second.Attempt, ok, err)
		}
		entries = storeJournalLength(t, w.st)
		// The first claimant, its lease still live by its own reckoning,
		// writes nothing over attempt 2.
		wantFault(t, finishRunIn(w.st, completed(first)), faultConflict, "the first claimant's finish of the retried run")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewClaim(first.ID, first.claim) }), faultConflict, "the first claimant's renewal of the retried run")
		heldAs(t, w, first.ID, "running", second.Attempt, entries)
		must(t, finishRunIn(w.st, completed(second)))
		heldAs(t, w, first.ID, "completed", second.Attempt, entries+1)
	}},
	{"a claim the kind ignores still meets the state's fence", func(t *testing.T, k storeKindUnderTest) {
		// Every kind: a claim is made once, from queued, and a finish is of
		// a run held running.
		w, r := claimWorld(t, k)
		again := r
		var err error
		if again.claim, err = newRunClaim(r.Attempt); err != nil {
			t.Fatal(err)
		}
		wantFault(t, changeQueuedRun(w.st, again), faultConflict, "a second claim of a run held running")
		must(t, finishRunIn(w.st, completed(r)))
		// A second completion meets the chain's entry first (taken); a
		// second failure meets the state.
		failed := r
		failed.State, failed.FinishedAt, failed.Problem = "failed", now(), "Failed."
		wantFault(t, finishRunIn(w.st, failed), faultConflict, "a second finish")
		heldAs(t, w, r.ID, "completed", r.Attempt, storeJournalLength(t, w.st))
	}},
}
