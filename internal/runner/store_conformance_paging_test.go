package runner

import (
	"fmt"
	"testing"
)

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
		// A window the bound left unfinished keeps its value and names its
		// end and position; a run that continued an older position of the
		// same window, whose value is the same, completes late and moves
		// nothing; the run that reads the window's last page finishes it.
		windowed := func(id string, continued, unfinished *UnfinishedWindow, to string) Run {
			r := claimed(id, "b2", to)
			r.Trigger.Paging[0].Cursor.Continued, r.Trigger.Paging[0].Cursor.Unfinished = continued, unfinished
			return r
		}
		at := func(position string) *UnfinishedWindow { return &UnfinishedWindow{End: "e1", Position: position} }
		opened := windowed("run_opened", nil, at("p1"), "")
		must(t, finishRunIn(w.st, completed(opened)))
		held, err := w.st.pagingCursors(w.job.ID)
		must(t, err)
		if row := held["feed"]; row.Value != "b2" || row.RunID != opened.ID || row.Unfinished == nil || *row.Unfinished != *at("p1") {
			t.Fatalf("the unfinished window's row: %+v", row)
		}
		continued := windowed("run_continued", at("p1"), at("p2"), "")
		must(t, finishRunIn(w.st, completed(continued)))
		for _, late := range []Run{windowed("run_window_late", at("p1"), at("p9"), ""), windowed("run_window_none", nil, nil, "e9")} {
			must(t, finishRunIn(w.st, completed(late)))
			if held, _ := w.st.pagingCursors(w.job.ID); held["feed"].RunID != continued.ID {
				t.Fatalf("%s moved the unfinished window: %+v", late.ID, held["feed"])
			}
		}
		finished := windowed("run_finished", at("p2"), nil, "e1")
		must(t, finishRunIn(w.st, completed(finished)))
		if held, _ := w.st.pagingCursors(w.job.ID); held["feed"].Value != "e1" || held["feed"].Unfinished != nil || held["feed"].RunID != finished.ID {
			t.Fatalf("the finished window's row: %+v", held["feed"])
		}
	}},
	{"an occurrence's job activity before it counts the job's open runs and the open occurrences admitted before it", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		other := w.worldOccurrence("occ_other", "trg_1", "accepted")
		other.JobID = "job_other"
		held := []Occurrence{other}
		for i, state := range []string{"submitted", "accepted", "failed", "preparing", "waiting", "needs-attention", "accepted", "accepted"} {
			held = append(held, w.worldOccurrence(fmt.Sprintf("occ_%d", i), "trg_1", state))
		}
		w.keep(t, nil, held...)
		// Before each occurrence: the open ones admitted before it, of its
		// own job; an id no occurrence holds counts every open one.
		want := map[string]int{"occ_0": 0, "occ_1": 0, "occ_3": 1, "occ_6": 4, "occ_7": 5, "occ_none": 6}
		check := func(runs int) {
			t.Helper()
			for id, n := range want {
				got, err := w.st.jobActivityBefore(w.job.ID, id)
				must(t, err)
				must(t, w.st.update(func(tx runTx) error {
					in, err := tx.jobActivityBefore(w.job.ID, id)
					if err == nil && in != got {
						err = fmt.Errorf("the transaction counts %d, the store %d", in, got)
					}
					return err
				}))
				if got != n+runs {
					t.Fatalf("before %s: %d, want %d", id, got, n+runs)
				}
			}
		}
		check(0)
		// A queued run and a running one are activity before every
		// occurrence of their job; one that ended is not.
		_, err := w.enqueue("run_queued", "key_queued")
		must(t, err)
		check(1)
		_, err = w.enqueue("run_claimed", "key_claimed")
		must(t, err)
		r, ok, err := claim(w.st)
		if err != nil || !ok {
			t.Fatal("no run was claimed:", ok, err)
		}
		check(2)
		r.State, r.FinishedAt, r.Problem, r.ErrorClass = "failed", now(), "Failed.", classEvaluation
		must(t, finishRunIn(w.st, r))
		check(1)
		if n, err := w.st.jobActivityBefore("job_other", "occ_other"); err != nil || n != 0 {
			t.Fatal("another job's activity:", n, err)
		}
	}},
}
