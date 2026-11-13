package runner

// The conformance cases of the scope a transaction, or a read outside one,
// is held to (docs/design/store-postgresql.md, sections 2 and 6). A kind that
// holds a row policy (holdsScopes: the in-memory kind, and the twice mode over
// it) is held to it here; the sqlite kind ignores the scope, its one process
// holding every scope, and its cases say so.

import (
	"slices"
	"testing"
)

// heldWorld is a store with a job, a queued run and a journal entry in each
// of the scopes "a" and "b" and the none scope, each job from a release of
// its own: a release makes one job (D9).
type heldWorld struct {
	st   runStore
	jobs map[string]Job
	runs map[string]Run
}

func newHeldWorld(t *testing.T, k storeKindUnderTest) heldWorld {
	t.Helper()
	w := heldWorld{st: k.open(t), jobs: map[string]Job{}, runs: map[string]Run{}}
	for _, scope := range []string{"a", "b", ""} {
		name := scope
		if name == "" {
			name = "none"
		}
		release := Release{SchemaVersion: "1", ID: "rel_" + name, PackID: "conformance", PackVersion: "1.0.0", Title: "Conformance", CreatedAt: now(), Tests: "passed"}
		must(t, w.st.update(func(tx runTx) error { return tx.createRelease(release) }))
		j := Job{SchemaVersion: "1", ID: "job_" + name, Name: "Conformance", ReleaseID: release.ID, Revision: 1, CreatedAt: now(), Workspace: "conformance", Owner: "owner", Scope: scope}
		k.seedJob(t, w.st, j)
		r := Run{SchemaVersion: "1", ID: "run_" + name, JobID: j.ID, ReleaseID: j.ReleaseID, Revision: 1, State: "queued", Input: journalInput(), CreatedAt: now(), RequestedBy: "owner", Scope: scope}
		must(t, w.st.update(func(tx runTx) error {
			if err := tx.createRun(r, "owner", "key-"+name, "sha256:"+name); err != nil {
				return err
			}
			_, err := tx.appendJournal(runChange(r, "", runnerActor, EventDetail{}))
			return err
		}))
		w.jobs[scope], w.runs[scope] = j, r
	}
	return w
}

// reached are the scopes, of "a", "b" and none, whose job, run and scope
// check st reaches, each read in a transaction and outside one.
func (w heldWorld) reached(t *testing.T, st runStore) []string {
	t.Helper()
	var scopes []string
	for _, scope := range []string{"a", "b", ""} {
		j, r := w.jobs[scope], w.runs[scope]
		var inTx []bool
		must(t, st.update(func(tx runTx) error {
			inTx = nil
			for _, err := range []error{readErr(tx.job(j.ID)), readErr(tx.run(r.ID))} {
				if err != nil && !absent(err) {
					return err
				}
				inTx = append(inTx, err == nil)
			}
			return nil
		}))
		outside := []bool{}
		for _, err := range []error{readErr(st.job(j.ID)), readErr(st.run(r.ID)), readErr(st.recordScope("jobs", j.ID)), readErr(st.recordScope("runs", r.ID))} {
			if err != nil && !absent(err) {
				t.Fatal(err)
			}
			outside = append(outside, err == nil)
		}
		all := slices.Concat(inTx, outside)
		switch {
		case !slices.Contains(all, false):
			scopes = append(scopes, scope)
		case slices.Contains(all, true):
			t.Fatalf("scope %q: some reads reach its records and some do not: %v", scope, all)
		}
	}
	return scopes
}

func readErr[T any](_ T, err error) error { return err }

// journalJobs are the jobs, "" for none, of the entries a page of st's
// journal holds.
func journalJobs(t *testing.T, st runStore) []string {
	t.Helper()
	items, _, err := st.journalPage(journalFilter{}, journalRange{limit: 100})
	must(t, err)
	var jobs []string
	for _, e := range items {
		jobs = append(jobs, e.Concerns.Job)
	}
	return jobs
}

var scopeStoreCases = []storeCase{
	{"a store opened reaches every scope's records", func(t *testing.T, k storeKindUnderTest) {
		// Work for every scope (section 6): the dispatcher, the ticks, the
		// restart and a listing that names no scope read every scope's
		// records, as the store opened does.
		w := newHeldWorld(t, k)
		if got := w.reached(t, w.st); !slices.Equal(got, []string{"a", "b", ""}) {
			t.Fatal("the store opened reaches", got)
		}
		if got := w.reached(t, w.st.heldTo(everyScope)); !slices.Equal(got, []string{"a", "b", ""}) {
			t.Fatal("the store held to every scope reaches", got)
		}
		if got := journalJobs(t, w.st); !slices.Equal(got, []string{"", "job_a", "job_b", "job_none"}) {
			t.Fatal("the journal read with no scope holds the entries of", got)
		}
	}},
	{"a transaction held to one scope reads that scope's records alone", func(t *testing.T, k storeKindUnderTest) {
		w := newHeldWorld(t, k)
		held := w.st.heldTo(scopeHeld("a"))
		if !k.holdsScopes {
			// The sqlite kind's answer: it holds no policy, and is the
			// store it was.
			if held != w.st {
				t.Fatal("a kind that holds no policy returned another store")
			}
			return
		}
		if got := w.reached(t, held); !slices.Equal(got, []string{"a"}) {
			t.Fatal("a store held to scope a reaches", got)
		}
		// The journal's read policy: the scope's entries, and the none
		// scope's that name no job (journal.began).
		if got := journalJobs(t, held); !slices.Equal(got, []string{"", "job_a"}) {
			t.Fatal("a journal read held to scope a holds the entries of", got)
		}
		// The store it was held from still reaches every scope.
		if got := w.reached(t, w.st); !slices.Equal(got, []string{"a", "b", ""}) {
			t.Fatal("holding a view to a scope narrowed the store:", got)
		}
	}},
	{"a read held to the none scope reaches the records that carry none", func(t *testing.T, k storeKindUnderTest) {
		w := newHeldWorld(t, k)
		held := w.st.heldTo(storeScope{})
		if !k.holdsScopes {
			if held != w.st {
				t.Fatal("a kind that holds no policy returned another store")
			}
			return
		}
		// The zero scope is the none scope (s:), never every scope: a
		// request naming no scope reaches only what carries none.
		if got := w.reached(t, held); !slices.Equal(got, []string{""}) {
			t.Fatal("a store held to the none scope reaches", got)
		}
		if got := journalJobs(t, held); !slices.Equal(got, []string{"", "job_none"}) {
			t.Fatal("a journal read held to the none scope holds the entries of", got)
		}
	}},
	{"a transaction held to one scope writes no other scope's records", func(t *testing.T, k storeKindUnderTest) {
		if !k.holdsScopes {
			t.Skip("the kind holds no row policy: its process holds every scope")
		}
		w := newHeldWorld(t, k)
		held := w.st.heldTo(scopeHeld("a"))
		other := w.runs["b"]
		// A new record carrying another scope is refused, as a row
		// policy's check refuses it.
		fresh := other
		fresh.ID = "run_b2"
		wantFault(t, held.update(func(tx runTx) error { return tx.createRun(fresh, "owner", "key-b2", "sha256:b2") }), faultRefused, "a run of scope b created held to scope a")
		// A record of another scope is not there to change.
		moved := other
		moved.State, moved.StartedAt, moved.Attempt = "running", now(), 1
		wantFault(t, held.update(func(tx runTx) error {
			_, err := tx.putRun(moved, "queued")
			return err
		}), faultAbsent, "a run of scope b changed held to scope a")
		// A record of the scope may not be written into another.
		mine := w.runs["a"]
		mine.Scope = "b"
		wantFault(t, held.update(func(tx runTx) error {
			_, err := tx.putRun(mine, "queued")
			return err
		}), faultRefused, "a run of scope a moved to scope b")
		// An entry about another scope's job is refused.
		wantFault(t, held.update(func(tx runTx) error {
			_, err := tx.appendJournal(runChange(other, "queued", runnerActor, EventDetail{}))
			return err
		}), faultRefused, "an entry of scope b appended held to scope a")
		jobB := w.jobs["b"]
		jobB.Name, jobB.Revision = "Renamed", 2
		wantFault(t, held.update(func(tx runTx) error { return tx.reviseJob(jobB, "sha256:revise") }), faultAbsent, "a job of scope b revised held to scope a")
		// Nothing was written.
		r, err := w.st.run(other.ID)
		must(t, err)
		if r.State != "queued" {
			t.Fatal("the run of scope b was changed:", r.State)
		}
		if _, err = w.st.run(fresh.ID); !absent(err) {
			t.Fatal("the refused run was kept:", err)
		}
		if j, err := w.st.job(jobB.ID); err != nil || j.Name != "Conformance" {
			t.Fatal("the job of scope b was changed:", j.Name, err)
		}
		if got := journalJobs(t, w.st); !slices.Equal(got, []string{"", "job_a", "job_b", "job_none"}) {
			t.Fatal("a refused entry was kept:", got)
		}
		// The scope's own record is written.
		mine = w.runs["a"]
		mine.State, mine.StartedAt, mine.Attempt = "running", now(), 1
		must(t, held.update(func(tx runTx) error {
			_, err := tx.putRun(mine, "queued")
			return err
		}))
	}},
	{"a transaction held to one scope reaches that scope's triggers and occurrences alone", func(t *testing.T, k storeKindUnderTest) {
		w := newHeldWorld(t, k)
		for _, scope := range []string{"a", "b"} {
			j := w.jobs[scope]
			tr := Trigger{ID: "trg_" + scope, JobID: j.ID, Revision: 1, Authority: "local", Config: TriggerConfig{Kind: "event", QueueSeconds: 60}, Paused: true, CreatedAt: "2026-11-01T10:00:00Z", UpdatedAt: "2026-11-01T10:00:00Z", Scope: scope}
			o := Occurrence{ID: "occ_" + scope, JobID: j.ID, ReleaseID: j.ReleaseID, JobRevision: 1, TriggerID: tr.ID, TriggerRevision: 1, Kind: "event", ReceivedAt: "2026-11-01T10:00:00Z", ExpiresAt: "2026-11-01T10:01:00Z", State: "accepted", Scope: scope}
			must(t, w.st.update(func(tx runTx) error {
				if err := tx.createTrigger(tr); err != nil {
					return err
				}
				return tx.createOccurrence(o, "identity:"+o.ID, "sha256:"+o.ID)
			}))
		}
		held := w.st.heldTo(scopeHeld("a"))
		want := map[string]bool{"a": true, "b": !k.holdsScopes}
		for scope, reach := range want {
			must(t, held.update(func(tx runTx) error {
				_, _, err := tx.trigger("trg_" + scope)
				if reach != (err == nil) || err != nil && !absent(err) {
					t.Fatalf("held to scope a, the trigger of scope %s read %v", scope, err)
				}
				_, err = tx.occurrence("occ_" + scope)
				if reach != (err == nil) || err != nil && !absent(err) {
					t.Fatalf("held to scope a, the occurrence of scope %s read %v", scope, err)
				}
				records, err := tx.triggerRecords(w.jobs[scope].ID)
				if err != nil || reach != (len(records) == 1) {
					t.Fatalf("held to scope a, the triggers of scope %s's job read %d (%v)", scope, len(records), err)
				}
				return nil
			}))
			listed, err := held.listedTriggers([]string{w.jobs[scope].ID})
			if err != nil || reach != (len(listed) == 1) {
				t.Fatalf("held to scope a, the listing of scope %s's triggers held %d (%v)", scope, len(listed), err)
			}
		}
		if !k.holdsScopes {
			return
		}
		// The installation's rules read every scope's: 128 triggers, and the
		// automatic queue's limit (D9).
		must(t, held.update(func(tx runTx) error {
			n, err := tx.triggerCount()
			if err != nil || n != 2 {
				t.Fatalf("held to scope a, the triggers counted %d (%v), want 2", n, err)
			}
			if n, err = tx.pendingOccurrences(); err != nil || n != 2 {
				t.Fatalf("held to scope a, the pending occurrences counted %d (%v), want 2", n, err)
			}
			return nil
		}))
		// A trigger of another scope is not there to change, and one may not
		// be made for another scope.
		other, _, err := w.st.trigger("trg_b")
		must(t, err)
		other.Paused = false
		wantFault(t, held.update(func(tx runTx) error { return tx.putTrigger(other, "", false) }), faultAbsent, "a trigger of scope b changed held to scope a")
		fresh := other
		fresh.ID = "trg_b2"
		wantFault(t, held.update(func(tx runTx) error { return tx.createTrigger(fresh) }), faultRefused, "a trigger of scope b made held to scope a")
		wantFault(t, held.update(func(tx runTx) error { return tx.createOccurrenceKey("occ_b", "sha256:key") }), faultRefused, "a key bound to scope b's occurrence held to scope a")
	}},
	{"the rules across scopes count every scope's records", func(t *testing.T, k storeKindUnderTest) {
		// D9: the queue's limit is the installation's, whatever scope a
		// transaction is held to.
		w := newHeldWorld(t, k)
		for _, sc := range []storeScope{everyScope, scopeHeld("a"), {}} {
			must(t, w.st.heldTo(sc).update(func(tx runTx) error {
				n, err := tx.activeRuns()
				if err != nil {
					return err
				}
				if n != 3 {
					t.Fatalf("held to %+v, the active runs counted %d, want 3", sc, n)
				}
				return nil
			}))
		}
	}},
}
