package runner

import "testing"

// pagingStoreCases hold each kind to the jobs' cursors (paging_loop.go,
// store_paging.go; docs/design/paging.md, section 4.2): a cursor is written
// by the transaction that records its run completed, under the run's claim,
// and only while the row holds what the run's occurrence read. A run that
// failed or was interrupted writes none, a finish that is refused writes
// none, and a run that completes late, from an older cursor, never moves it
// back.
var pagingStoreCases = []storeCase{
	{"a paging cursor moves only with a completed run, from what it read", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		must(t, w.st.update(func(tx runTx) error { return tx.raiseSchema(pagingStoreSchema) }))
		cursor := func() (string, string) {
			t.Helper()
			held, err := w.st.pagingCursors(w.job.ID)
			must(t, err)
			if len(held) > 1 {
				t.Fatal("the job holds cursors of more sources than it read:", held)
			}
			return held["feed"].Value, held["feed"].RunID
		}
		// claimed is a run of the world's job, claimed, whose occurrence
		// read read from the feed's cursor and whose pages name to next.
		claimed := func(id, read, to string) Run {
			t.Helper()
			_, err := w.enqueue(id, id)
			must(t, err)
			r, ok, err := claim(w.st)
			if err != nil || !ok || r.ID != id {
				t.Fatal("the run was not claimed:", r.ID, ok, err)
			}
			from := read
			if from == "" {
				from = "start"
			}
			r.Trigger = &TriggerOrigin{OccurrenceID: "occ_" + id, TriggerID: "trg_paging", TriggerRevision: 1, Kind: "schedule", Paging: []PagedRead{{Source: "feed", Pages: 2, End: "no-next", Cursor: &CursorAdvance{Read: read, From: from, To: to}}}}
			return r
		}
		failed := claimed("run_failed", "", "b1")
		failed.State, failed.FinishedAt, failed.Problem, failed.ErrorClass = "failed", now(), "Failed.", classEvaluation
		must(t, finishRunIn(w.st, failed))
		interrupted := claimed("run_interrupted", "", "b1")
		interrupted.State, interrupted.FinishedAt, interrupted.InterruptedAt = "interrupted", now(), now()
		must(t, finishRunIn(w.st, interrupted))
		if value, _ := cursor(); value != "" {
			t.Fatal("a run that did not complete moved the cursor to", value)
		}
		// A finish refused because the run is no longer running under its
		// claim writes no cursor either.
		lost := claimed("run_lost", "", "b1")
		ended := lost
		ended.State, ended.FinishedAt, ended.InterruptedAt = "interrupted", now(), now()
		must(t, finishRunIn(w.st, ended))
		wantFault(t, finishRunIn(w.st, completed(lost)), faultConflict, "a completion after the run ended")
		if value, _ := cursor(); value != "" {
			t.Fatal("a refused completion moved the cursor to", value)
		}
		first := claimed("run_first", "", "b1")
		must(t, finishRunIn(w.st, completed(first)))
		if value, by := cursor(); value != "b1" || by != first.ID {
			t.Fatalf("the cursor is %q by %s, want b1 by %s", value, by, first.ID)
		}
		// Runs whose occurrences read an older cursor, or none, complete
		// late: the cursor stays.
		for _, late := range []Run{claimed("run_late", "", "b0"), claimed("run_stale", "a0", "b2")} {
			must(t, finishRunIn(w.st, completed(late)))
			if value, by := cursor(); value != "b1" || by != first.ID {
				t.Fatalf("%s moved the cursor to %q", late.ID, value)
			}
		}
		// A run that read a pages' end with no next value moves nothing.
		none := claimed("run_none", "b1", "")
		must(t, finishRunIn(w.st, completed(none)))
		next := claimed("run_next", "b1", "b2")
		must(t, finishRunIn(w.st, completed(next)))
		if value, by := cursor(); value != "b2" || by != next.ID {
			t.Fatalf("the cursor is %q by %s, want b2 by %s", value, by, next.ID)
		}
		other, err := w.st.pagingCursors("job_other")
		if err != nil || len(other) != 0 {
			t.Fatal("another job holds the cursor:", other, err)
		}
	}},
}
