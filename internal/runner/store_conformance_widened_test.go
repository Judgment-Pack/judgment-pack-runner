package runner

// The conformance cases of the state the contract widened to hold (issue
// #244, docs/design/store-interface.md, section 9): the journal's reads and
// refusals, profile rows, briefs, a record's scope and the schemas raised
// through the contract. Every kind runs them, as it runs storeCases.

import (
	"errors"
	"slices"
	"testing"
)

// journalKindsOf is the kinds of the journal's entries, in order, read
// through the contract, the entry that began the journal left out.
func journalKindsOf(t *testing.T, st runStore) []string {
	t.Helper()
	var kinds []string
	for after := int64(0); ; {
		items, more, err := st.journalPage(journalFilter{}, journalRange{after: after, limit: 7})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range items {
			if e.Sequence <= after {
				t.Fatalf("entry %d read after %d", e.Sequence, after)
			}
			after = e.Sequence
			if e.Kind != "journal.began" {
				kinds = append(kinds, e.Kind)
			}
		}
		if !more {
			return kinds
		}
	}
}

// entriesWorld is a world whose journal holds, after the entry that began it
// (1): the job's creation under scope alpha (2), a preview of the job's
// release (3) and of another release (4), the job's run completed with an
// outcome under alpha (5) and failed with an error class under beta (6), the
// Runner's start (7) and another job's run queued (8).
func entriesWorld(t *testing.T, k storeKindUnderTest) storeWorld {
	t.Helper()
	w := newStoreWorld(t, k)
	mine := EventConcerns{Job: w.job.ID, Release: w.release.ID}
	other := EventConcerns{Job: "job_other", Release: "rel_other", Run: "run_other"}
	entries := []change{
		{kind: "job.created", at: "2026-11-01T10:00:00Z", by: runnerActor, concerns: mine, detail: EventDetail{Scope: "alpha"}},
		{kind: "release.previewed", at: "2026-11-01T10:01:00Z", by: runnerActor, concerns: EventConcerns{Release: w.release.ID}},
		{kind: "release.previewed", at: "2026-11-01T10:02:00Z", by: runnerActor, concerns: EventConcerns{Release: "rel_other"}},
		{kind: "run.completed", at: "2026-11-01T10:03:00Z", by: runnerActor, concerns: mine, revision: 1, states: true, from: "running", to: "completed", detail: EventDetail{Scope: "alpha", Disposition: &EventDisposition{Kind: "outcome", OutcomeID: "approve"}}},
		{kind: "run.failed", at: "2026-11-01T10:04:00Z", by: runnerActor, concerns: mine, states: true, from: "running", to: "failed", reason: "it failed", detail: EventDetail{Scope: "beta", ErrorClass: "timeout"}},
		{kind: "runner.started", at: "2026-11-01T10:05:00Z", by: runnerActor},
		{kind: "run.queued", at: "2026-11-01T10:06:00Z", by: runnerActor, concerns: other, states: true, to: "queued"},
	}
	must(t, w.st.update(func(tx runTx) error {
		for i, c := range entries {
			sequence, err := tx.appendJournal(c)
			if err != nil {
				return err
			}
			if sequence != int64(i+2) {
				t.Errorf("entry %d appended at %d", i+2, sequence)
			}
		}
		return nil
	}))
	return w
}

var widenedStoreCases = []storeCase{
	{"the journal is read forward and backward, in order, through its filters", func(t *testing.T, k storeKindUnderTest) {
		w := entriesWorld(t, k)
		read := func(f journalFilter, r journalRange) ([]int64, bool) {
			t.Helper()
			items, more, err := w.st.journalPage(f, r)
			if err != nil {
				t.Fatal(err)
			}
			return sequencesOf(items), more
		}
		for _, c := range []struct {
			what string
			f    journalFilter
			r    journalRange
			want []int64
			more bool
		}{
			{"every entry", journalFilter{}, journalRange{limit: 50}, []int64{1, 2, 3, 4, 5, 6, 7, 8}, false},
			{"after a sequence, to a limit", journalFilter{}, journalRange{after: 2, limit: 3}, []int64{3, 4, 5}, true},
			{"to a limit the entries fill exactly", journalFilter{}, journalRange{after: 5, limit: 3}, []int64{6, 7, 8}, false},
			{"a job's, with its release's", journalFilter{job: w.job.ID}, journalRange{limit: 50}, []int64{2, 3, 5, 6}, false},
			{"a job's and the Runner's own", journalFilter{job: w.job.ID, runnerOwn: true}, journalRange{limit: 50}, []int64{1, 2, 3, 5, 6, 7}, false},
			{"another job's", journalFilter{job: "job_other"}, journalRange{limit: 50}, []int64{8}, false},
			{"a job no entry names", journalFilter{job: "job_none"}, journalRange{limit: 50}, nil, false},
			{"under a scope", journalFilter{scope: "alpha"}, journalRange{limit: 50}, []int64{2, 5}, false},
			{"a job's and the Runner's own under a scope", journalFilter{job: w.job.ID, runnerOwn: true, scope: "beta"}, journalRange{limit: 50}, []int64{6}, false},
			{"of an outcome", journalFilter{results: resultFilter{Outcomes: []string{"approve"}}}, journalRange{limit: 50}, []int64{5}, false},
			{"of another outcome", journalFilter{results: resultFilter{Outcomes: []string{"decline"}}}, journalRange{limit: 50}, nil, false},
			{"of an error class", journalFilter{results: resultFilter{Classes: []string{"timeout"}}}, journalRange{limit: 50}, []int64{6}, false},
			{"of kinds", journalFilter{results: resultFilter{Kinds: []string{"run.queued", "runner.started"}}}, journalRange{limit: 50}, []int64{7, 8}, false},
			{"a job's of a kind under a scope", journalFilter{job: w.job.ID, scope: "alpha", results: resultFilter{Kinds: []string{"run.completed"}}}, journalRange{limit: 50}, []int64{5}, false},
			{"the newest", journalFilter{}, journalRange{backward: true, tail: true, limit: 2}, []int64{8, 7}, true},
			{"before a sequence", journalFilter{}, journalRange{backward: true, before: 5, limit: 50}, []int64{4, 3, 2, 1}, false},
			{"before a sequence, to a limit", journalFilter{}, journalRange{backward: true, before: 5, limit: 2}, []int64{4, 3}, true},
			{"before the first", journalFilter{}, journalRange{backward: true, before: 1, limit: 50}, nil, false},
			{"a job's, the newest", journalFilter{job: w.job.ID}, journalRange{backward: true, tail: true, limit: 3}, []int64{6, 5, 3}, true},
		} {
			got, more := read(c.f, c.r)
			if !slices.Equal(got, c.want) || more != c.more {
				t.Errorf("%s: %v, more %v; want %v, more %v", c.what, got, more, c.want, c.more)
			}
		}
		items, _, err := w.st.journalPage(journalFilter{}, journalRange{after: 4, limit: 2})
		must(t, err)
		got := string(encode(items))
		want := `[{"sequence":5,"entryVersion":"` + resultEntryVersion + `","kind":"run.completed","at":"2026-11-01T10:03:00Z","by":{"kind":"runner"},"concerns":{"job":"job_conformance","release":"rel_conformance"},"revision":1,"from":"running","to":"completed","scope":"alpha","disposition":{"kind":"outcome","outcomeId":"approve"}},` +
			`{"sequence":6,"entryVersion":"` + resultEntryVersion + `","kind":"run.failed","at":"2026-11-01T10:04:00Z","by":{"kind":"runner"},"concerns":{"job":"job_conformance","release":"rel_conformance"},"from":"running","to":"failed","reason":"it failed","scope":"beta","errorClass":"timeout"}]`
		if got != want {
			t.Errorf("the entries read\n%s\nwant\n%s", got, want)
		}
		items, _, err = w.st.journalPage(journalFilter{}, journalRange{after: 7, limit: 1})
		must(t, err)
		if got := string(encode(items)); got != `[{"sequence":8,"entryVersion":"1","kind":"run.queued","at":"2026-11-01T10:06:00Z","by":{"kind":"runner"},"concerns":{"job":"job_other","release":"rel_other","run":"run_other"},"from":null,"to":"queued"}]` {
			t.Errorf("a created record's entry reads %s", got)
		}
		began, err := w.st.journalBegan()
		must(t, err)
		first, _, err := w.st.journalPage(journalFilter{}, journalRange{limit: 1})
		must(t, err)
		if began == "" || len(first) != 1 || first[0].Kind != "journal.began" || began != first[0].At {
			t.Errorf("the journal began %q; its first entry is %v", began, first)
		}
	}},
	{"a refusal's newest entry is read under its key", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		refusal := func(at, key string) change {
			return change{kind: "admission.refused", at: at, by: runnerActor, concerns: EventConcerns{Job: w.job.ID}, detail: EventDetail{Request: "submit-run"}, refusal: key}
		}
		must(t, w.st.update(func(tx runTx) error {
			for _, c := range []change{refusal("2026-11-01T10:00:00Z", "submit\x00a"), refusal("2026-11-01T10:01:00Z", "submit\x00a"), refusal("2026-11-01T10:02:00Z", "submit\x00b"), {kind: "run.queued", at: "2026-11-01T10:03:00Z", by: runnerActor}} {
				if _, err := tx.appendJournal(c); err != nil {
					return err
				}
			}
			return nil
		}))
		err := w.st.update(func(tx runTx) error {
			for key, want := range map[string]string{"submit\x00a": "2026-11-01T10:01:00Z", "submit\x00b": "2026-11-01T10:02:00Z"} {
				if at, err := tx.lastRefusal(key); err != nil || at != want {
					t.Errorf("the refusal %q was last journaled at %q (%v), want %q", key, at, err, want)
				}
			}
			for _, key := range []string{"submit\x00c", "submit"} {
				_, err := tx.lastRefusal(key)
				wantFault(t, err, faultAbsent, "a key never journaled: "+key)
			}
			return errKeepNothing
		})
		if !errors.Is(err, errKeepNothing) {
			t.Fatal(err)
		}
	}},
	{"a profile row is written whole and read as written, under its key only", func(t *testing.T, k storeKindUnderTest) {
		st := k.open(t)
		for key := range profileRowKeys {
			_, err := st.profileRow(key)
			wantFault(t, err, faultAbsent, "a row never written")
		}
		rows := map[string]string{registeredProfilesKey: `[{"id":"one"}]`, selectedProfilesKey: `[{"id":"two"},{"id":"three"}]`}
		for key, row := range rows {
			must(t, st.update(func(tx runTx) error { return tx.putProfileRow(key, row) }))
		}
		rows[registeredProfilesKey] = `[{"id":"one"},{"id":"four"}]`
		must(t, st.update(func(tx runTx) error { return tx.putProfileRow(registeredProfilesKey, rows[registeredProfilesKey]) }))
		failed := errors.New("the transaction fails")
		if err := st.update(func(tx runTx) error {
			must(t, tx.putProfileRow(selectedProfilesKey, `[]`))
			return failed
		}); !errors.Is(err, failed) {
			t.Fatal(err)
		}
		for key, want := range rows {
			if got, err := st.profileRow(key); err != nil || got != want {
				t.Errorf("the row %s reads %q (%v), want %q", key, got, err, want)
			}
		}
		for _, key := range []string{"schema", "workspace", "manualExecutionDemand", ""} {
			_, err := st.profileRow(key)
			wantFault(t, err, faultRefused, "a read under "+key)
			err = st.update(func(tx runTx) error { return tx.putProfileRow(key, `[]`) })
			wantFault(t, err, faultRefused, "a write under "+key)
		}
	}},
	{"a brief is written whole and read as written", func(t *testing.T, k storeKindUnderTest) {
		st := k.open(t)
		_, err := st.brief("job:job_one")
		wantFault(t, err, faultAbsent, "a brief never written")
		first, second := []byte(`{"version":1,"subjects":{}}`), []byte(`{"version":1,"subjects":{"job:job_one":{}}}`)
		must(t, st.update(func(tx runTx) error { return tx.putBrief("job:job_one", first) }))
		must(t, st.update(func(tx runTx) error {
			held, err := tx.brief("job:job_one")
			if err != nil || string(held) != string(first) {
				t.Errorf("the transaction reads %q (%v)", held, err)
			}
			return tx.putBrief("job:job_one", second)
		}))
		if got, err := st.brief("job:job_one"); err != nil || string(got) != string(second) {
			t.Errorf("the brief reads %q (%v)", got, err)
		}
		_, err = st.brief("run:job_one")
		wantFault(t, err, faultAbsent, "another subject")
	}},
	{"a record's scope is read by its kind of record", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		scoped := w.job
		scoped.ID, scoped.ReleaseID, scoped.Scope = "job_scoped", "rel_scoped", "alpha"
		k.seedJob(t, w.st, scoped)
		run := w.queuedRun("run_scoped")
		run.JobID, run.Scope = scoped.ID, "alpha"
		must(t, w.st.update(func(tx runTx) error { return tx.createRun(run, "owner", "scoped", "sha256:scoped") }))
		must(t, w.st.update(func(tx runTx) error { return tx.createRun(w.queuedRun("run_none"), "owner", "none", "sha256:none") }))
		for _, c := range []struct{ records, id, want string }{{"jobs", "job_scoped", "alpha"}, {"jobs", w.job.ID, ""}, {"runs", "run_scoped", "alpha"}, {"runs", "run_none", ""}} {
			if got, err := w.st.recordScope(c.records, c.id); err != nil || got != c.want {
				t.Errorf("%s %s carries %q (%v), want %q", c.records, c.id, got, err, c.want)
			}
		}
		for _, c := range []struct{ records, id string }{{"jobs", "job_none"}, {"runs", "run_other"}, {"triggers", "trg_none"}, {"occurrences", "occ_none"}} {
			_, err := w.st.recordScope(c.records, c.id)
			wantFault(t, err, faultAbsent, c.records+" "+c.id)
		}
		for _, records := range []string{"releases", "metadata", "jobs WHERE 1=1 OR id", ""} {
			_, err := w.st.recordScope(records, w.job.ID)
			wantFault(t, err, faultRefused, "a scope read from "+records)
		}
	}},
	{"a schema is raised in order and never lowered", func(t *testing.T, k storeKindUnderTest) {
		st := k.open(t)
		raise := func(schema, want string) {
			t.Helper()
			must(t, st.update(func(tx runTx) error { return tx.raiseSchema(schema) }))
			if got := k.schema(t, st); got != want {
				t.Fatalf("raising %s leaves the store at %s, want %s", schema, got, want)
			}
		}
		if got := k.schema(t, st); got != storeSchema {
			t.Fatalf("a new store is at %s", got)
		}
		raise(scopedStoreSchema, scopedStoreSchema)
		raise(scopedStoreSchema, scopedStoreSchema)
		raise(retryStoreSchema, retryStoreSchema)
		raise(scopedStoreSchema, retryStoreSchema)
		for _, schema := range []string{storeSchema, manualStoreSchema, "7", ""} {
			err := st.update(func(tx runTx) error { return tx.raiseSchema(schema) })
			wantFault(t, err, faultRefused, "raising "+schema)
		}
		if got := k.schema(t, st); got != retryStoreSchema {
			t.Fatalf("a refused raise left the store at %s", got)
		}
		other := k.open(t)
		must(t, other.update(func(tx runTx) error { return tx.raiseSchema(retryStoreSchema) }))
		if got := k.schema(t, other); got != retryStoreSchema {
			t.Fatalf("the retry schema raised from a new store leaves it at %s", got)
		}
	}},
}
