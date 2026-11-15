package runner

// The conformance cases of an occurrence's claim (docs/design/store-postgresql.md,
// D18), in the shape of a run's (D3): a Runner claims an occurrence before it
// prepares or dispatches it, every write of the occurrence is fenced by the
// claim and its lease, which the store's clock holds, and the readers that
// hand a Runner work pass by an occurrence another Runner holds. A kind that
// fences claims (fencesClaims) is held to it here; the sqlite kind checks the
// state a claim expects and holds no claim, its lock being the fence.

import (
	"slices"
	"testing"
)

// claimedOccurrence admits an occurrence in state and claims it, as a Runner
// does before it works it, and returns it under the claim.
func claimedOccurrence(t *testing.T, w storeWorld, id, state string) Occurrence {
	t.Helper()
	w.keep(t, nil, w.worldOccurrence(id, "trg_1", state))
	claim, err := newOccurrenceClaim()
	must(t, err)
	var o Occurrence
	must(t, w.st.update(func(tx runTx) (err error) {
		o, err = tx.claimOccurrence(id, state, claim)
		return err
	}))
	if o.ID != id || o.State != state || o.claim != claim || len(claim.token) != 32 {
		t.Fatalf("the claim returned %s %s under %+v, not the occurrence as held under a new claim", o.ID, o.State, o.claim)
	}
	return o
}

// putOccurrenceIn writes o in one transaction, expecting expected.
func putOccurrenceIn(st runStore, o Occurrence, expected occurrenceExpected) error {
	return st.update(func(tx runTx) error {
		_, err := tx.putOccurrence(o, expected)
		return err
	})
}

// readyIDs are the ids the readers that hand a Runner work give: the ready
// occurrence first, then the preparable ones.
func readyIDs(t *testing.T, st runStore) []string {
	t.Helper()
	var ids []string
	if o, err := st.readyOccurrence(); err == nil {
		ids = append(ids, o.ID)
	} else if !absent(err) {
		t.Fatal(err)
	}
	must(t, st.preparableOccurrences(func(o Occurrence) (bool, error) {
		ids = append(ids, o.ID)
		return false, nil
	}))
	return ids
}

var occurrenceClaimStoreCases = []storeCase{
	{"an occurrence is claimed in the state its claimant read, by one claimant at a time", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		ready := claimedOccurrence(t, w, "occ_1", "accepted")
		waiting := claimedOccurrence(t, w, "occ_2", "waiting")
		other, err := newOccurrenceClaim()
		must(t, err)
		claimIn := func(id, state string) error {
			return w.st.update(func(tx runTx) error {
				_, err := tx.claimOccurrence(id, state, other)
				return err
			})
		}
		wantFault(t, claimIn("occ_1", "waiting"), faultConflict, "a claim of an occurrence that moved on from the state read")
		wantFault(t, claimIn("occ_none", "accepted"), faultAbsent, "a claim of an occurrence not held")
		if !k.fencesClaims {
			must(t, claimIn("occ_1", "accepted"))
			return
		}
		// Another Runner neither claims an occurrence held under a live
		// claim, nor is handed it.
		wantFault(t, claimIn("occ_1", "accepted"), faultConflict, "a second claim of a claimed occurrence")
		wantFault(t, claimIn("occ_2", "waiting"), faultConflict, "a second claim of a claimed preparation")
		if ids := readyIDs(t, w.st); len(ids) != 0 {
			t.Fatal("the readers handed out occurrences another Runner holds:", ids)
		}
		// Once the leases ran out, by the store's clock, both are handed out
		// and claimed again; the first claims are no longer held.
		k.passTime(w.st, claimLease)
		if ids := readyIDs(t, w.st); !slices.Equal(ids, []string{"occ_1", "occ_2"}) {
			t.Fatal("the readers did not hand out the occurrences whose lease ran out:", ids)
		}
		must(t, claimIn("occ_1", "accepted"))
		must(t, claimIn("occ_2", "waiting"))
		ready.Reason, waiting.Reason = "late", "late"
		wantFault(t, putOccurrenceIn(w.st, ready, occurrenceExpected{}), faultConflict, "the first claimant's write after its claim was taken")
		wantFault(t, putOccurrenceIn(w.st, waiting, occurrenceExpected{state: "waiting"}), faultConflict, "the first claimant's write of its preparation after its claim was taken")
		for _, id := range []string{"occ_1", "occ_2"} {
			if held, err := w.st.occurrence(id); err != nil || held.Reason != "" {
				t.Fatalf("%s reads %q after the first claimant's late write, %v", id, held.Reason, err)
			}
		}
	}},
	{"a holder's write is fenced by its claim and lease, and keeps the claim only when asked", func(t *testing.T, k storeKindUnderTest) {
		if !k.fencesClaims {
			t.Skip("the kind's lock is the fence")
		}
		w := newStoreWorld(t, k)
		o := claimedOccurrence(t, w, "occ_1", "accepted")
		read, err := w.st.occurrence(o.ID)
		must(t, err)
		if read.claim != (occurrenceClaim{}) {
			t.Fatal("an occurrence read back carries a claim:", read.claim)
		}
		forged := o
		forged.claim = occurrenceClaim{token: "forged"}
		forged.State = "failed"
		wantFault(t, putOccurrenceIn(w.st, forged, occurrenceExpected{}), faultConflict, "a write under another claim")
		read.State = "failed"
		wantFault(t, putOccurrenceIn(w.st, read, occurrenceExpected{}), faultConflict, "a write under no claim of a claimed occurrence")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewOccurrenceClaim(o.ID, forged.claim) }), faultConflict, "a renewal under another claim")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewOccurrenceClaim(o.ID, occurrenceClaim{}) }), faultConflict, "a renewal under no claim")
		// The holder's write that keeps the claim: the occurrence is still
		// its, renewed, until the lease the last renewal set runs out.
		o.State = "preparing"
		must(t, putOccurrenceIn(w.st, o, occurrenceExpected{state: "accepted", keep: true}))
		k.passTime(w.st, claimLease/2)
		must(t, w.st.update(func(tx runTx) error { return tx.renewOccurrenceClaim(o.ID, o.claim) }))
		k.passTime(w.st, claimLease/2)
		wantFault(t, putOccurrenceIn(w.st, read, occurrenceExpected{}), faultConflict, "a write under no claim within the renewed lease")
		o.Reason = "kept"
		must(t, putOccurrenceIn(w.st, o, occurrenceExpected{state: "preparing", keep: true}))
		k.passTime(w.st, claimLease)
		wantFault(t, w.st.update(func(tx runTx) error { return tx.renewOccurrenceClaim(o.ID, o.claim) }), faultConflict, "a renewal after the lease ran out")
		late := o
		late.State, late.Reason = "accepted", "late"
		wantFault(t, putOccurrenceIn(w.st, late, occurrenceExpected{}), faultConflict, "a holder's write after its lease ran out")
		if held, err := w.st.occurrence(o.ID); err != nil || held.State != "preparing" || held.Reason != "kept" {
			t.Fatalf("the late write wrote %s %q, %v", held.State, held.Reason, err)
		}
		// A write that does not keep the claim releases it: the occurrence
		// is handed out and written under none again.
		o2 := claimedOccurrence(t, w, "occ_2", "accepted")
		o2.Reason = "released"
		must(t, putOccurrenceIn(w.st, o2, occurrenceExpected{state: "accepted"}))
		if o, err := w.st.readyOccurrence(); err != nil || o.ID != "occ_2" {
			t.Fatal("a released occurrence is not handed out:", o.ID, err)
		}
		read2, err := w.st.occurrence("occ_2")
		must(t, err)
		read2.Reason = "unclaimed"
		must(t, putOccurrenceIn(w.st, read2, occurrenceExpected{}))
	}},
	{"a request's write passes the claim, and the holder's next write is refused", func(t *testing.T, k storeKindUnderTest) {
		if !k.fencesClaims {
			t.Skip("the kind's lock is the fence")
		}
		w := newStoreWorld(t, k)
		o := claimedOccurrence(t, w, "occ_1", "waiting")
		o.Reason = "intent"
		must(t, putOccurrenceIn(w.st, o, occurrenceExpected{state: "waiting", keep: true}))
		cancelled, err := w.st.occurrence(o.ID)
		must(t, err)
		cancelled.State = "cancelled"
		wantFault(t, putOccurrenceIn(w.st, cancelled, occurrenceExpected{state: "needs-attention", overrule: true}), faultConflict, "a request's write over a state it did not read")
		must(t, putOccurrenceIn(w.st, cancelled, occurrenceExpected{state: "waiting", overrule: true}))
		// The holder's write is refused by its claim, which the request
		// cleared, whatever state it expects.
		o.Reason = "late"
		wantFault(t, putOccurrenceIn(w.st, o, occurrenceExpected{}), faultConflict, "the holder's write after a cancellation")
		if held, err := w.st.occurrence(o.ID); err != nil || held.State != "cancelled" {
			t.Fatal("the cancellation did not win:", held.State, err)
		}
	}},
	{"a restart and the tick fail only the preparations no live claim holds", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		held := claimedOccurrence(t, w, "occ_1", "accepted")
		held.State = "preparing"
		must(t, putOccurrenceIn(w.st, held, occurrenceExpected{state: "accepted", keep: true}))
		w.keep(t, nil, w.worldOccurrence("occ_2", "trg_1", "preparing"))
		failed := func() []string {
			var ids []string
			for _, id := range []string{"occ_1", "occ_2"} {
				o, err := w.st.occurrence(id)
				must(t, err)
				if o.State == "failed" {
					ids = append(ids, id)
				}
			}
			return ids
		}
		must(t, w.st.update(func(tx runTx) error { return restartOccurrences(tx, 0) }))
		if !k.fencesClaims {
			if got := failed(); !slices.Equal(got, []string{"occ_1", "occ_2"}) {
				t.Fatal("the sqlite kind's restart did not fail every preparation:", got)
			}
			return
		}
		if got := failed(); !slices.Equal(got, []string{"occ_2"}) {
			t.Fatal("the restart failed other than the preparation no claim holds:", got)
		}
		k.passTime(w.st, claimLease)
		must(t, (&Service{store: w.st}).interruptLapsed())
		if got := failed(); !slices.Equal(got, []string{"occ_1", "occ_2"}) {
			t.Fatal("the tick left a preparation whose lease ran out:", got)
		}
		held.State, held.Reason = "accepted", "late"
		wantFault(t, putOccurrenceIn(w.st, held, occurrenceExpected{}), faultConflict, "the holder's write after the tick failed its preparation")
	}},
}
