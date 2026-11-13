package runner

// The conformance cases of triggers and occurrences behind the store contract
// (issue #244, docs/design/store-interface.md, section 9). Every kind runs
// them, as it runs storeCases.

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// worldTrigger is a trigger of the world's job.
func (w storeWorld) worldTrigger(id string) Trigger {
	return Trigger{ID: id, JobID: w.job.ID, Revision: 1, Authority: "local", Config: TriggerConfig{Kind: "event", QueueSeconds: 60}, Paused: true, CreatedAt: "2026-11-01T10:00:00Z", UpdatedAt: "2026-11-01T10:00:00Z"}
}

// cloudTrigger is a cloud trigger of the world's job bound to job of
// connection.
func (w storeWorld) cloudTrigger(id, connection, job string) Trigger {
	t := w.worldTrigger(id)
	t.Authority = "google-cloud"
	t.Config.Kind = "cloud"
	t.Config.Cloud = &CloudBinding{Connection: connection, Subscription: "sub", Job: job}
	return t
}

// worldOccurrence is an occurrence of a trigger of the world's job.
func (w storeWorld) worldOccurrence(id, trigger, state string) Occurrence {
	return Occurrence{ID: id, JobID: w.job.ID, ReleaseID: w.release.ID, JobRevision: 1, TriggerID: trigger, TriggerRevision: 1, Kind: "event", ReceivedAt: "2026-11-01T10:00:00Z", ExpiresAt: "2026-11-01T10:01:00Z", State: state}
}

// keep makes each trigger and admits each occurrence, in order, in one
// transaction.
func (w storeWorld) keep(t *testing.T, triggers []Trigger, occurrences ...Occurrence) {
	t.Helper()
	must(t, w.st.update(func(tx runTx) error {
		for _, tr := range triggers {
			if err := tx.createTrigger(tr); err != nil {
				return err
			}
		}
		for _, o := range occurrences {
			if err := tx.createOccurrence(o, "identity:"+o.ID, "sha256:"+o.ID); err != nil {
				return err
			}
		}
		return nil
	}))
}

func occurrenceIDs(items []Occurrence) []string {
	ids := []string{}
	for _, o := range items {
		ids = append(ids, o.ID)
	}
	return ids
}

var triggerStoreCases = []storeCase{
	{"a trigger is made once, written whole and read with its cursor and key", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		tr := w.worldTrigger("trg_1")
		w.keep(t, []Trigger{tr})
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createTrigger(tr) }), faultTaken, "a second trigger under one id")
		held, hash, err := w.st.trigger(tr.ID)
		must(t, err)
		if hash != "" || held.HasKey || held.observed != "" || held.keyRevision != nil || held.Config.Kind != "event" || !held.Paused {
			t.Fatalf("a new trigger reads %+v, key %q", held, hash)
		}
		issued := 2
		tr.Revision, tr.Paused, tr.keyRevision = 2, false, &issued
		tr.observed, tr.pending, tr.pendingAt = "sha256:seen", "sha256:next", "2026-11-01T10:05:00Z"
		must(t, w.st.update(func(tx runTx) error { return tx.putTrigger(tr, "sha256:key", true) }))
		held, hash, err = w.st.trigger(tr.ID)
		must(t, err)
		if hash != "sha256:key" || !held.HasKey || held.Paused || held.Revision != 2 || held.observed != "sha256:seen" || held.pending != "sha256:next" || held.pendingAt != "2026-11-01T10:05:00Z" || held.keyRevision == nil || *held.keyRevision != 2 {
			t.Fatalf("a written trigger reads %+v, key %q", held, hash)
		}
		tr.Problem = "again"
		wantFault(t, w.st.update(func(tx runTx) error { return tx.putTrigger(tr, "sha256:key", true) }), faultTaken, "a revision's snapshot kept twice")
		must(t, w.st.update(func(tx runTx) error { return tx.putTrigger(tr, "", false) }))
		if held, hash, err = w.st.trigger(tr.ID); err != nil || hash != "" || held.HasKey || held.Problem != "again" {
			t.Fatalf("a trigger written without its snapshot reads %+v, key %q, %v", held, hash, err)
		}
		wantFault(t, w.st.update(func(tx runTx) error { return tx.putTrigger(w.worldTrigger("trg_none"), "", false) }), faultAbsent, "a trigger written that is not held")
		_, _, err = w.st.trigger("trg_none")
		wantFault(t, err, faultAbsent, "a trigger not held")
		if n, err := w.st.triggerCount(); err != nil || n != 1 {
			t.Fatalf("%d triggers counted, %v", n, err)
		}
	}},
	{"a cloud binding is held by one cloud trigger", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		w.keep(t, []Trigger{w.cloudTrigger("trg_a", "conn", "nightly"), w.worldTrigger("trg_b")})
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createTrigger(w.cloudTrigger("trg_c", "conn", "nightly")) }), faultTaken, "a second trigger bound to one job")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.putTrigger(w.cloudTrigger("trg_b", "conn", "nightly"), "", false) }), faultTaken, "a trigger rebound to a held job")
		w.keep(t, []Trigger{w.cloudTrigger("trg_d", "conn", "weekly")})
		if id, err := w.st.cloudBoundTrigger("conn", "weekly"); err != nil || id != "trg_d" {
			t.Fatalf("the weekly binding is %q, %v", id, err)
		}
		_, err := w.st.cloudBoundTrigger("other", "nightly")
		wantFault(t, err, faultAbsent, "a binding no trigger holds")
		for _, c := range []struct {
			except, job string
			want        int
		}{{"", "nightly", 1}, {"trg_a", "nightly", 0}, {"trg_b", "nightly", 1}, {"trg_a", "monthly", 0}} {
			if n, err := w.st.cloudBindings(c.except, "conn", c.job); err != nil || n != c.want {
				t.Fatalf("bindings of %s except %q: %d, %v; want %d", c.job, c.except, n, err, c.want)
			}
		}
	}},
	{"enabled triggers and a job's triggers are read in the order they were made", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		third, first := w.worldTrigger("trg_3"), w.worldTrigger("trg_1")
		third.Paused, first.Paused = false, false
		other := w.worldTrigger("trg_2")
		other.JobID = "job_other"
		w.keep(t, []Trigger{third, w.worldTrigger("trg_0"), other, first})
		must(t, w.st.update(func(tx runTx) error { return tx.putTrigger(first, "sha256:key", false) }))
		if ids, err := w.st.enabledTriggers(); err != nil || !slices.Equal(ids, []string{"trg_3", "trg_1"}) {
			t.Fatalf("enabled triggers %v, %v", ids, err)
		}
		items, err := w.st.jobTriggers(w.job.ID)
		must(t, err)
		var ids []string
		for _, tr := range items {
			ids = append(ids, fmt.Sprintf("%s:%v", tr.ID, tr.HasKey))
		}
		if !slices.Equal(ids, []string{"trg_3:false", "trg_0:false", "trg_1:true"}) {
			t.Fatalf("the job's triggers %v", ids)
		}
		if items, err = w.st.jobTriggers("job_none"); err != nil || items == nil || len(items) != 0 {
			t.Fatalf("a job with no triggers has %v, %v", items, err)
		}
	}},
	{"an occurrence is admitted once under its id and once under its identity", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		o := w.worldOccurrence("occ_1", "trg_1", "accepted")
		w.keep(t, nil, o)
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createOccurrence(o, "identity:other", "") }), faultTaken, "a second occurrence under one id")
		again := w.worldOccurrence("occ_2", "trg_1", "accepted")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createOccurrence(again, "identity:occ_1", "") }), faultTaken, "a second occurrence under one identity")
		again.TriggerID = "trg_2"
		must(t, w.st.update(func(tx runTx) error { return tx.createOccurrence(again, "identity:occ_1", "sha256:two") }))
		held, payload, err := w.st.identifiedOccurrence("trg_1", "identity:occ_1")
		if err != nil || held.ID != "occ_1" || payload != "sha256:occ_1" {
			t.Fatalf("the identity reads %s, %q, %v", held.ID, payload, err)
		}
		if held, err = w.st.occurrence("occ_2"); err != nil || held.TriggerID != "trg_2" {
			t.Fatalf("occ_2 reads %+v, %v", held, err)
		}
		_, _, err = w.st.identifiedOccurrence("trg_3", "identity:occ_1")
		wantFault(t, err, faultAbsent, "an identity of another trigger")
		_, err = w.st.occurrence("occ_none")
		wantFault(t, err, faultAbsent, "an occurrence not held")
	}},
	{"an occurrence's state is changed only from the state its writer expects", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		o := w.worldOccurrence("occ_1", "trg_1", "preparing")
		w.keep(t, nil, o)
		o.State, o.Reason = "failed", "stopped"
		var from string
		err := w.st.update(func(tx runTx) (err error) { from, err = tx.putOccurrence(o, "accepted"); return err })
		wantFault(t, err, faultConflict, "a change from a state not held")
		if from != "preparing" {
			t.Fatalf("a refused change read %q", from)
		}
		if held, _ := w.st.occurrence(o.ID); held.State != "preparing" || held.Reason != "" {
			t.Fatalf("a refused change wrote %+v", held)
		}
		must(t, w.st.update(func(tx runTx) (err error) { from, err = tx.putOccurrence(o, "preparing"); return err }))
		if held, _ := w.st.occurrence(o.ID); from != "preparing" || held.State != "failed" || held.Reason != "stopped" {
			t.Fatalf("a change from preparing read %q and wrote %+v", from, held)
		}
		o.State = "skipped"
		must(t, w.st.update(func(tx runTx) (err error) { from, err = tx.putOccurrence(o, anyState); return err }))
		if held, _ := w.st.occurrence(o.ID); from != "failed" || held.State != "skipped" {
			t.Fatalf("a change from any state read %q and wrote %+v", from, held)
		}
		_, err = w.st.occurrence("occ_none")
		wantFault(t, err, faultAbsent, "an occurrence not held")
		wantFault(t, w.st.update(func(tx runTx) error {
			_, err := tx.putOccurrence(w.worldOccurrence("occ_none", "trg_1", "failed"), anyState)
			return err
		}), faultAbsent, "a change of an occurrence not held")
	}},
	{"a preparation still in flight does not overwrite a final state", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		o := w.worldOccurrence("occ_1", "trg_1", "failed")
		w.keep(t, nil, o)
		before := journalKindsOf(t, w.st)
		o.State = "waiting"
		if err := w.st.update(func(tx runTx) error { return recordOccurrence(tx, o, "preparing", runnerActor, nil) }); !errors.Is(err, errPreparationStopped) {
			t.Fatalf("a preparation's write over a final state: %v", err)
		}
		if held, _ := w.st.occurrence(o.ID); held.State != "failed" || len(journalKindsOf(t, w.st)) != len(before) {
			t.Fatalf("a stopped preparation wrote %+v", held)
		}
		o.State = "preparing"
		must(t, w.st.update(func(tx runTx) error { return recordOccurrence(tx, o, "", runnerActor, nil) }))
		if kinds := journalKindsOf(t, w.st); len(kinds) != len(before)+1 || kinds[len(kinds)-1] != "occurrence.preparing" {
			t.Fatalf("a change of state journals %v", kinds)
		}
	}},
	{"the automatic queue is counted and served in the order occurrences were admitted", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		input := &AutomaticInput{Kind: "mapped-sources"}
		var held []Occurrence
		for i, state := range []string{"submitted", "accepted", "waiting", "accepted", "preparing", "needs-attention", "accepted", "failed"} {
			o := w.worldOccurrence(fmt.Sprintf("occ_%d", i), "trg_1", state)
			if i == 1 || i == 6 {
				o.PendingInput = input
			}
			if i == 6 {
				o.JobID = "job_other"
			}
			held = append(held, o)
		}
		w.keep(t, nil, held...)
		if _, err := w.enqueue("run_1", "key_1"); err != nil {
			t.Fatal(err)
		}
		must(t, w.st.update(func(tx runTx) error {
			pending, err := tx.pendingOccurrences()
			if err != nil || pending != 5 {
				return fmt.Errorf("%d pending, %v", pending, err)
			}
			active, err := tx.jobActivity(w.job.ID)
			if err != nil || active != 6 {
				return fmt.Errorf("%d active, %v", active, err)
			}
			if active, err = tx.jobActivity("job_other"); err != nil || active != 1 {
				return fmt.Errorf("%d active for the other job, %v", active, err)
			}
			return nil
		}))
		if o, err := w.st.readyOccurrence(); err != nil || o.ID != "occ_3" {
			t.Fatalf("the ready occurrence is %s, %v", o.ID, err)
		}
		var given []string
		must(t, w.st.preparableOccurrences(func(o Occurrence) (bool, error) { given = append(given, o.ID); return false, nil }))
		if !slices.Equal(given, []string{"occ_1", "occ_2", "occ_6"}) {
			t.Fatalf("the preparable occurrences %v", given)
		}
		given = nil
		must(t, w.st.preparableOccurrences(func(o Occurrence) (bool, error) { given = append(given, o.ID); return o.ID == "occ_2", nil }))
		if !slices.Equal(given, []string{"occ_1", "occ_2"}) {
			t.Fatalf("the preparable occurrences up to the one chosen %v", given)
		}
		stop := errors.New("stop")
		if err := w.st.preparableOccurrences(func(Occurrence) (bool, error) { return false, stop }); !errors.Is(err, stop) {
			t.Fatalf("each's error came back as %v", err)
		}
		must(t, w.st.update(func(tx runTx) error {
			o := held[3]
			o.State = "submitted"
			_, err := tx.putOccurrence(o, "accepted")
			return err
		}))
		_, err := w.st.readyOccurrence()
		wantFault(t, err, faultAbsent, "no occurrence ready")
	}},
	{"a job's occurrences are read newest first, in pages, and by their preparations", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		var held []Occurrence
		for i := range 7 {
			o := w.worldOccurrence(fmt.Sprintf("occ_%d", i), "trg_1", "accepted")
			switch i {
			case 1, 4, 5:
				o.Preparation = &SourcePreparation{}
			case 3:
				o.JobID = "job_other"
			}
			if i == 5 {
				o.State = "submitted"
			}
			held = append(held, o)
		}
		w.keep(t, nil, held...)
		page := func(before int64, preparations bool, limit int) ([]string, int64) {
			t.Helper()
			items, next, err := w.st.occurrencePage(w.job.ID, before, preparations, limit)
			must(t, err)
			return occurrenceIDs(items), next
		}
		const all = int64(1) << 62
		ids, next := page(all, false, 4)
		if !slices.Equal(ids, []string{"occ_6", "occ_5", "occ_4", "occ_2"}) || next == 0 {
			t.Fatalf("the first page %v, next %d", ids, next)
		}
		if ids, after := page(next, false, 4); !slices.Equal(ids, []string{"occ_1", "occ_0"}) || after != 0 {
			t.Fatalf("the second page %v, next %d", ids, after)
		}
		if ids, after := page(all, true, 4); !slices.Equal(ids, []string{"occ_4", "occ_1"}) || after != 0 {
			t.Fatalf("the preparations %v, next %d", ids, after)
		}
		if ids, after := page(all, false, 6); len(ids) != 6 || after != 0 {
			t.Fatalf("a page holding every occurrence %v, next %d", ids, after)
		}
		if items, after, err := w.st.occurrencePage("job_none", all, false, 4); err != nil || items == nil || len(items) != 0 || after != 0 {
			t.Fatalf("a job with none %v, %d, %v", items, after, err)
		}
	}},
	{"an occurrence is read under its delivery key only while the key is its trigger's", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		tr := w.worldTrigger("trg_1")
		w.keep(t, []Trigger{tr}, w.worldOccurrence("occ_1", tr.ID, "accepted"), w.worldOccurrence("occ_2", tr.ID, "accepted"))
		must(t, w.st.update(func(tx runTx) error {
			if err := tx.putTrigger(tr, "sha256:key", false); err != nil {
				return err
			}
			return tx.createOccurrenceKey("occ_1", "sha256:key")
		}))
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createOccurrenceKey("occ_1", "sha256:key") }), faultTaken, "an occurrence bound twice")
		if o, err := w.st.keyedOccurrence("occ_1", tr.ID, "sha256:key"); err != nil || o.ID != "occ_1" {
			t.Fatalf("the bound occurrence reads %s, %v", o.ID, err)
		}
		for _, c := range []struct{ what, id, trigger, hash string }{
			{"another key", "occ_1", tr.ID, "sha256:other"},
			{"another trigger", "occ_1", "trg_2", "sha256:key"},
			{"an occurrence no key made", "occ_2", tr.ID, "sha256:key"},
		} {
			_, err := w.st.keyedOccurrence(c.id, c.trigger, c.hash)
			wantFault(t, err, faultAbsent, c.what)
		}
		must(t, w.st.update(func(tx runTx) error { return tx.putTrigger(tr, "sha256:rotated", false) }))
		_, err := w.st.keyedOccurrence("occ_1", tr.ID, "sha256:key")
		wantFault(t, err, faultAbsent, "a key rotated away")
	}},
	{"a restart fails what was left preparing, from when its preparation began", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		left, waiting, earlier := w.worldOccurrence("occ_1", "trg_1", "preparing"), w.worldOccurrence("occ_2", "trg_1", "waiting"), w.worldOccurrence("occ_3", "trg_1", "preparing")
		left.Attempts = []Attempt{{Number: 1}}
		w.keep(t, nil, left, waiting, earlier)
		must(t, w.st.update(func(tx runTx) error {
			for _, c := range []change{
				{kind: "occurrence.preparing", at: "2026-11-01T10:00:00Z", by: runnerActor, concerns: EventConcerns{Occurrence: "occ_1"}},
				{kind: "occurrence.preparing", at: "2026-11-01T10:01:00Z", by: runnerActor, concerns: EventConcerns{Occurrence: "occ_2"}},
				{kind: "occurrence.preparing", at: "2026-11-01T10:02:00Z", by: runnerActor, concerns: EventConcerns{Occurrence: "occ_1"}},
				{kind: "occurrence.failed", at: "2026-11-01T10:03:00Z", by: runnerActor, concerns: EventConcerns{Occurrence: "occ_1"}},
			} {
				if _, err := tx.appendJournal(c); err != nil {
					return err
				}
			}
			ids, err := tx.preparingOccurrences()
			if err != nil || !slices.Equal(occurrenceIDs(ids), []string{"occ_1", "occ_3"}) {
				return fmt.Errorf("left preparing %v, %v", occurrenceIDs(ids), err)
			}
			if at, err := tx.lastPreparing("occ_1"); err != nil || at != "2026-11-01T10:02:00Z" {
				return fmt.Errorf("occ_1 began preparing at %q, %v", at, err)
			}
			_, err = tx.lastPreparing("occ_3")
			wantFault(t, err, faultAbsent, "an occurrence with no preparing entry")
			return restartOccurrences(tx, 9)
		}))
		for id, since := range map[string]string{"occ_1": "2026-11-01T10:02:00Z", "occ_3": ""} {
			o, err := w.st.occurrence(id)
			must(t, err)
			if o.State != "failed" || o.ErrorClass != classInterrupted {
				t.Fatalf("%s after the restart: %+v", id, o)
			}
			items, _, err := w.st.journalPage(journalFilter{}, journalRange{backward: true, tail: true, limit: 2})
			must(t, err)
			found := false
			for _, e := range items {
				if e.Concerns.Occurrence == id && e.Interruption != nil && e.Interruption.LastKnownRunning == since && e.Interruption.Restart == 9 {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s's interruption is not journaled from %q", id, since)
			}
		}
		if o, _ := w.st.occurrence("occ_1"); o.Attempts[0].Outcome != "interrupted" {
			t.Fatalf("the open attempt was left %+v", o.Attempts)
		}
		if o, _ := w.st.occurrence("occ_2"); o.State != "waiting" {
			t.Fatalf("a waiting occurrence was changed: %+v", o)
		}
	}},
}
