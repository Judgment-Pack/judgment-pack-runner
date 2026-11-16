package runner

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// admit admits an occurrence of the world's trigger under key, as a
// schedule does, and prepares nothing.
func (w *pagedWorld) admit(t *testing.T, tr Trigger, key string) Occurrence {
	t.Helper()
	o := newOccurrence(tr, w.job, time.Now())
	o.PendingInput = w.input
	var admitted Occurrence
	must(t, w.s.store.update(func(tx runTx) error {
		var err error
		admitted, err = w.s.admitOccurrence(tx, tr, o, key, "")
		return err
	}))
	return admitted
}

// unprepared fails the test unless the occurrence held under id is accepted
// with its input still to prepare and no paged read.
func (w *pagedWorld) unprepared(t *testing.T, id, why string) {
	t.Helper()
	if o := storedOccurrence(t, w.s, id); o.State != "accepted" || o.Input != nil || o.PendingInput == nil || len(o.Paging) != 0 || o.HeldPages != nil {
		t.Fatalf("%s: %s %s %+v", why, o.State, o.Reason, o.Paging)
	}
}

// entryKinds are the kinds of the entries of the occurrence held under id.
func entryKinds(t *testing.T, s *Service, id string) []string {
	t.Helper()
	var kinds []string
	for _, e := range occurrenceEntries(t, s, id) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// docs/design/paging.md, section 7, "Overlap" (#307): an occurrence of a job
// that keeps cursors, under the overlap rule "queue", is not prepared, and
// reads no cursor, while its job has a run or an earlier occurrence that has
// not settled; it stays accepted, the occurrences of other jobs are taken
// meanwhile, and it is taken once the job is idle, reading the cursor the
// run before it moved. One whose queue time runs out while it waits expires.
// The rule "skip" is unchanged.
func TestAPagedOccurrenceUnderQueueWaitsForItsJobsEarlierWork(t *testing.T) {
	w := newPagedWorld(t, nil, changeCursor, newPagedFeed(t, 3, nil))
	first := w.prepare(t, "first")
	onePagedRead(t, first, 3, "no-next")
	must(t, w.s.dispatchOccurrence(first, time.Now()))
	w.feed.taken()
	second := w.admit(t, w.tr, "second")
	third := w.admit(t, w.tr, "third")
	// An occurrence of a job that keeps no cursor, admitted after both.
	other := &pagedWorld{s: w.s, feed: w.feed, clock: w.clock}
	other.release(t, pagedFeedDialect(w.feed.p), "Unpaged", "", nil, nil)
	c := constantTrigger(time.Now())
	c.Input = w.input
	otherTrigger, e := w.s.configureTrigger(other.job.ID, "", 0, c)
	must(t, e)
	other.input = w.input
	unpaged := other.admit(t, otherTrigger, "unpaged")
	received := entryKinds(t, w.s, second.ID)

	// While the first run is queued, the preparation takes the other job's
	// occurrence, and neither of the paged job's.
	if o := w.step(t, unpaged.ID); o.State != "accepted" || o.Input == nil {
		t.Fatalf("the other job's occurrence was not prepared: %s %s", o.State, o.Reason)
	}
	for _, c := range w.feed.taken() {
		if c.since != "seed" {
			t.Fatalf("a call read from a cursor while the job's run is queued: %+v", c)
		}
	}
	w.unprepared(t, second.ID, "the second occurrence was prepared while the first run is queued")
	w.unprepared(t, third.ID, "the third occurrence was prepared while the first run is queued")
	// Its claim checks again: asked to prepare it, the Runner leaves it
	// accepted, and writes nothing of it.
	must(t, w.s.prepareLegacyOccurrence(t.Context(), second.ID))
	w.unprepared(t, second.ID, "the second occurrence was claimed while the first run is queued")
	if got := entryKinds(t, w.s, second.ID); !slices.Equal(got, received) {
		t.Fatalf("the held occurrence's entries: %v, want %v", got, received)
	}
	if calls := w.feed.taken(); len(calls) != 0 {
		t.Fatalf("a held occurrence asked the feed: %+v", calls)
	}

	// The first run completes; the second is prepared from the cursor it
	// moved, the third held behind it.
	if worked, e := w.s.dispatchNext(t.Context()); e != nil || !worked {
		t.Fatal("the first run was not worked:", worked, e)
	}
	if value, _ := w.cursor(t); value != "after-start" {
		t.Fatal("the first run did not move the cursor:", value)
	}
	o := w.step(t, second.ID)
	if read := onePagedRead(t, o, 3, "no-next"); *read.Cursor != (CursorAdvance{Read: "after-start", From: "after-start", To: "after-after-start"}) {
		t.Fatalf("the second read's cursor: %+v", read.Cursor)
	}
	if calls := w.feed.taken(); len(calls) != 3 || calls[0].since != "after-start" {
		t.Fatalf("the second read was not asked from the moved cursor: %+v", calls)
	}
	w.unprepared(t, third.ID, "the third occurrence was prepared beside the second")

	// Held past its queue time, the third expires and asks nothing.
	must(t, w.s.dispatchOccurrence(o, time.Now()))
	expires, e := time.Parse(time.RFC3339Nano, third.ExpiresAt)
	must(t, e)
	started := *w.clock
	*w.clock = expires.Add(time.Second)
	if o := w.step(t, third.ID); o.State != "expired" || o.Reason != "queue-expired" {
		t.Fatalf("the held occurrence past its queue time: %s %s", o.State, o.Reason)
	}
	if calls := w.feed.taken(); len(calls) != 0 {
		t.Fatalf("an expired occurrence asked the feed: %+v", calls)
	}
	*w.clock = started
	if worked, e := w.s.dispatchNext(t.Context()); e != nil || !worked {
		t.Fatal("the second run was not worked:", worked, e)
	}
	if value, by := w.cursor(t); value != "after-after-start" || by != storedOccurrence(t, w.s, second.ID).RunID {
		t.Fatalf("the second run did not move the cursor: %q by %s", value, by)
	}

	// Under "skip", an idle job's occurrence is admitted and prepared, and
	// one while its run is queued is skipped, as before.
	skip := constantTrigger(time.Now())
	skip.Overlap, skip.Input = "skip", w.input
	skipping, e := w.s.configureTrigger(w.job.ID, "", 0, skip)
	must(t, e)
	idle := w.admit(t, skipping, "idle")
	o = w.step(t, idle.ID)
	onePagedRead(t, o, 3, "no-next")
	must(t, w.s.dispatchOccurrence(o, time.Now()))
	if busy := w.admit(t, skipping, "busy"); busy.State != "skipped" || busy.Reason != "overlap" {
		t.Fatalf("an occurrence under skip while the job's run is queued: %s %s", busy.State, busy.Reason)
	}
	if worked, e := w.s.dispatchNext(t.Context()); e != nil || !worked {
		t.Fatal("the run under skip was not worked:", worked, e)
	}
}

// A completed run whose advance the job's cursor no longer admits, the
// cursor having moved on since its occurrence read it, says so: its record's
// read names no advance and is held "moved", and its run.completed entry
// names the source in cursorHeld, at entry version 6, each as openapi.json
// says.
func TestARunWhoseCursorMovedOnSaysSo(t *testing.T) {
	w := newPagedWorld(t, nil, changeCursor, newPagedFeed(t, 3, nil))
	o := w.prepare(t, "first")
	onePagedRead(t, o, 3, "no-next")
	must(t, w.s.store.update(func(tx runTx) error {
		_, err := tx.advancePagingCursor(PagingCursor{JobID: w.job.ID, Source: "changes", Value: "elsewhere", RunID: "run_elsewhere", UpdatedAt: now(), Scope: w.job.Scope}, "", nil)
		return err
	}))
	r := w.run(t, o)
	if r.State != "completed" || r.Trigger == nil || len(r.Trigger.Paging) != 1 || r.Trigger.Paging[0].Cursor == nil || *r.Trigger.Paging[0].Cursor != (CursorAdvance{From: "start", Held: cursorMoved}) {
		t.Fatalf("the run does not say its cursor moved on: %s %+v", r.State, r.Trigger)
	}
	if value, by := w.cursor(t); value != "elsewhere" || by != "run_elsewhere" {
		t.Fatalf("the cursor is %q by %s, want elsewhere by run_elsewhere", value, by)
	}
	e := completedEntry(t, w.s.store, r.ID)
	if e.EntryVersion != pagingEntryVersion || !slices.Equal(e.CursorHeld, []string{"changes"}) {
		t.Fatalf("the run's entry does not name its cursor held: %+v", e)
	}
	doc := readOpenAPI(t)
	if problems := doc.check("trigger", doc.schema("TriggerOrigin"), decodeNumbers(t, encode(r.Trigger))); len(problems) > 0 {
		t.Fatal("the run's origin is not as openapi.json says:", problems)
	}
	if problems := doc.check("entry", doc.schema("JournalEntry"), decodeNumbers(t, encode(e))); len(problems) > 0 {
		t.Fatal("the run's entry is not as openapi.json says:", problems)
	}
}

// Section 7, "Overlap", across Runners of one store of the second kind: an
// occurrence one Runner is asked to prepare while another Runner's run of
// its job is queued is neither prepared nor claimed, and is prepared from the
// cursor that run moved once it has completed.
func TestPostgreSQLAPagedOccurrenceWaitsForAnotherRunnersRun(t *testing.T) {
	p := pgRunners(t)
	feed := newPagedFeed(t, 3, nil)
	root := t.TempDir()
	opened := func() *pagedWorld {
		cfg := journalConfig(t)
		cfg.PostgreSQL, cfg.InputRoot = &p.conn, root
		return pagedRunner(t, cfg, feed)
	}
	a := opened()
	a.release(t, pagedFeedDialect(feed.p), "Paging", "", nil, changeCursor)
	c := constantTrigger(time.Now())
	c.Input = &AutomaticInput{Kind: "mapped-sources", Case: json.RawMessage(`{"since":"seed","from":"2026-01-01T00:00:00Z","until":"2026-01-01T00:00:00Z","label":"feed"}`)}
	// A trigger is a request's to write, held to its job's scope.
	tr, e := a.s.configureTriggerBy(a.s.store.heldTo(scopeHeld(a.job.Scope)), requester{}, a.job.ID, "", 0, c)
	must(t, e)
	a.tr, a.input = tr, c.Input
	b := opened()
	b.job, b.tr, b.input = a.job, a.tr, a.input

	first := a.prepare(t, "first")
	onePagedRead(t, first, 3, "no-next")
	must(t, a.s.dispatchOccurrence(first, time.Now()))
	feed.taken()
	second := b.admit(t, b.tr, "second")
	b.step(t, second.ID)
	must(t, b.s.prepareLegacyOccurrence(t.Context(), second.ID))
	b.unprepared(t, second.ID, "another Runner prepared the occurrence while the first run is queued")
	if calls := feed.taken(); len(calls) != 0 {
		t.Fatalf("a held occurrence asked the feed: %+v", calls)
	}
	if worked, e := a.s.dispatchNext(t.Context()); e != nil || !worked {
		t.Fatal("the first run was not worked:", worked, e)
	}
	o := b.step(t, second.ID)
	if read := onePagedRead(t, o, 3, "no-next"); *read.Cursor != (CursorAdvance{Read: "after-start", From: "after-start", To: "after-after-start"}) {
		t.Fatalf("the second read's cursor: %+v", read.Cursor)
	}
}
