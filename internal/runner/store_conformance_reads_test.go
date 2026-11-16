package runner

// The conformance case of every read whose rows lie in a scope
// (docs/design/store-postgresql.md, section 6): held to one scope, each reads
// that scope's rows alone. One world holds a record of each kind in the
// scopes "a" and "b" and the none scope, and one table names each read and
// what it reaches; a kind that holds a row policy (holdsScopes) reaches scope
// a's alone, and the sqlite kind, which holds none, every scope's.

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// readsWorld is a heldWorld with, in each scope, a trigger holding a
// delivery key, an occurrence admitted under an identity and bound to that
// key, a brief of the job, a journaled refusal and an occurrence's
// preparation entry.
type readsWorld struct{ heldWorld }

// scopeName is the name a scope's records are made under: "none" for none.
func scopeName(scope string) string {
	if scope == "" {
		return "none"
	}
	return scope
}

func newReadsWorld(t *testing.T, k storeKindUnderTest) readsWorld {
	t.Helper()
	w := readsWorld{newHeldWorld(t, k)}
	for _, scope := range []string{"a", "b", ""} {
		n, j := scopeName(scope), w.jobs[scope]
		tr := Trigger{ID: "trg_" + n, JobID: j.ID, Revision: 1, Authority: "local", Config: TriggerConfig{Kind: "event", QueueSeconds: 60}, Paused: true, CreatedAt: "2026-11-01T10:00:00Z", UpdatedAt: "2026-11-01T10:00:00Z", Scope: scope}
		o := Occurrence{ID: "occ_" + n, JobID: j.ID, ReleaseID: j.ReleaseID, JobRevision: 1, TriggerID: tr.ID, TriggerRevision: 1, Kind: "event", ReceivedAt: "2026-11-01T10:00:00Z", ExpiresAt: "2026-11-01T10:01:00Z", State: "accepted", Scope: scope}
		must(t, w.st.update(func(tx runTx) error {
			if err := tx.createTrigger(tr); err != nil {
				return err
			}
			if err := tx.putTrigger(tr, "sha256:key-"+n, false); err != nil {
				return err
			}
			if err := tx.createOccurrence(o, "identity:"+n, "sha256:payload-"+n); err != nil {
				return err
			}
			if err := tx.createOccurrenceKey(o.ID, "sha256:key-"+n); err != nil {
				return err
			}
			if err := tx.putBrief("job:"+j.ID, []byte(`{"history":[]}`)); err != nil {
				return err
			}
			concerns := EventConcerns{Job: j.ID, Release: j.ReleaseID}
			if _, err := tx.appendJournal(change{kind: "admission.refused", by: runnerActor, concerns: concerns, detail: EventDetail{Scope: scope}, refusal: "refusal-" + n}); err != nil {
				return err
			}
			concerns.Trigger, concerns.Occurrence = tr.ID, o.ID
			_, err := tx.appendJournal(change{kind: "occurrence.preparing", by: runnerActor, concerns: concerns, detail: EventDetail{Scope: scope}})
			return err
		}))
	}
	return w
}

// scopedRead is one read of a scope's rows: reach says whether st reaches the
// row the world holds in scope, failing the test on an error that is not
// "nothing held".
type scopedRead struct {
	name  string
	reach func(t *testing.T, w readsWorld, st runStore, scope string) bool
}

// heldRead is a read's answer as a reach: a row read, none held, or a failure.
func heldRead(t *testing.T, what string, err error) bool {
	t.Helper()
	if err != nil && !absent(err) {
		t.Fatalf("%s: %v", what, err)
	}
	return err == nil
}

// inTx is a reach read inside a transaction of st's.
func inTx(t *testing.T, st runStore, read func(tx runTx) bool) bool {
	t.Helper()
	var reached bool
	if err := st.update(func(tx runTx) error {
		reached = read(tx)
		return errKeepNothing
	}); !errors.Is(err, errKeepNothing) {
		t.Fatal(err)
	}
	return reached
}

// listedRecordIDs are the ids of a listing's records.
func listedRecordIDs(t *testing.T, items []listedRecord, err error) []string {
	t.Helper()
	must(t, err)
	var ids []string
	for _, item := range items {
		var record struct {
			ID string `json:"id"`
		}
		must(t, json.Unmarshal(item.record, &record))
		ids = append(ids, record.ID)
	}
	return ids
}

const readsBefore = int64(1) << 62

var scopedReads = []scopedRead{
	{"job", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return heldRead(t, "job", readErr(st.job(w.jobs[scope].ID)))
	}},
	{"run", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return heldRead(t, "run", readErr(st.run(w.runs[scope].ID)))
	}},
	{"recordScope", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return heldRead(t, "recordScope", readErr(st.recordScope("runs", w.runs[scope].ID)))
	}},
	{"listedJobs", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, err := st.listedJobs(readsBefore, recordFilter{}, 100)
		return slices.Contains(listedRecordIDs(t, items, err), w.jobs[scope].ID)
	}},
	{"listedRuns", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, err := st.listedRuns("", readsBefore, recordFilter{}, 100)
		return slices.Contains(listedRecordIDs(t, items, err), w.runs[scope].ID)
	}},
	{"listedRuns of a job", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, err := st.listedRuns(w.jobs[scope].ID, readsBefore, recordFilter{}, 100)
		return slices.Contains(listedRecordIDs(t, items, err), w.runs[scope].ID)
	}},
	{"recentRuns", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, err := st.recentRuns([]string{w.jobs["a"].ID, w.jobs["b"].ID, w.jobs[""].ID}, 5)
		must(t, err)
		return slices.ContainsFunc(items, func(r recentRun) bool { return r.id == w.runs[scope].ID })
	}},
	{"listedTriggers", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, err := st.listedTriggers([]string{w.jobs[scope].ID})
		must(t, err)
		return len(items) == 1
	}},
	{"requestedRun", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return heldRead(t, "requestedRun", readErr(st.requestedRun(w.jobs[scope].ID, "owner", "key-"+scopeName(scope))))
	}},
	{"jobRevision", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		_, _, err := st.jobRevision(w.jobs[scope].ID, 1)
		return heldRead(t, "jobRevision", err)
	}},
	{"brief", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return heldRead(t, "brief", readErr(st.brief("job:"+w.jobs[scope].ID)))
	}},
	{"putBrief", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		err := st.update(func(tx runTx) error { return tx.putBrief("job:"+w.jobs[scope].ID, []byte(`{"history":[1]}`)) })
		if err != nil && faultOf(err) != faultRefused {
			t.Fatal("putBrief:", err)
		}
		return err == nil
	}},
	{"lastRefusal", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return inTx(t, st, func(tx runTx) bool {
			return heldRead(t, "lastRefusal", readErr(tx.lastRefusal("refusal-"+scopeName(scope))))
		})
	}},
	{"openOccurrences", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return inTx(t, st, func(tx runTx) bool {
			n, err := tx.openOccurrences(w.jobs[scope].ID)
			must(t, err)
			return n == 1
		})
	}},
	{"jobActivity", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return inTx(t, st, func(tx runTx) bool {
			n, err := tx.jobActivity(w.jobs[scope].ID)
			must(t, err)
			return n == 2
		})
	}},
	{"jobActivityBefore", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		n, err := st.jobActivityBefore(w.jobs[scope].ID, "occ_none_held")
		must(t, err)
		return n == 2
	}},
	{"trigger", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		_, _, err := st.trigger("trg_" + scopeName(scope))
		return heldRead(t, "trigger", err)
	}},
	{"triggerRecords", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return inTx(t, st, func(tx runTx) bool {
			records, err := tx.triggerRecords(w.jobs[scope].ID)
			must(t, err)
			return len(records) == 1
		})
	}},
	{"jobTriggers", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, err := st.jobTriggers(w.jobs[scope].ID)
		must(t, err)
		return len(items) == 1
	}},
	{"occurrence", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return heldRead(t, "occurrence", readErr(st.occurrence("occ_"+scopeName(scope))))
	}},
	{"identifiedOccurrence", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		_, _, err := st.identifiedOccurrence("trg_"+scopeName(scope), "identity:"+scopeName(scope))
		return heldRead(t, "identifiedOccurrence", err)
	}},
	{"keyedOccurrence", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		n := scopeName(scope)
		return heldRead(t, "keyedOccurrence", readErr(st.keyedOccurrence("occ_"+n, "trg_"+n, "sha256:key-"+n)))
	}},
	{"occurrencePage", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, _, err := st.occurrencePage(w.jobs[scope].ID, readsBefore, false, 10)
		must(t, err)
		return len(items) == 1
	}},
	{"lastPreparing", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		return inTx(t, st, func(tx runTx) bool {
			return heldRead(t, "lastPreparing", readErr(tx.lastPreparing("occ_"+scopeName(scope))))
		})
	}},
	{"journalPage of a job", func(t *testing.T, w readsWorld, st runStore, scope string) bool {
		items, _, err := st.journalPage(journalFilter{job: w.jobs[scope].ID}, journalRange{limit: 100})
		must(t, err)
		return len(items) > 0
	}},
}

var readsStoreCases = []storeCase{
	{"every read of a scope's rows, held to one scope, reads that scope's alone", func(t *testing.T, k storeKindUnderTest) {
		w := newReadsWorld(t, k)
		want := []string{"a"}
		if !k.holdsScopes {
			// The sqlite kind holds no row policy: its process holds every
			// scope.
			want = []string{"a", "b", ""}
		}
		held := w.st.heldTo(scopeHeld("a"))
		for _, read := range scopedReads {
			t.Run(read.name, func(t *testing.T) {
				// The store opened reaches every scope's row.
				for _, scope := range []string{"a", "b", ""} {
					if !read.reach(t, w, w.st, scope) {
						t.Fatalf("the store opened does not reach scope %q's row", scope)
					}
				}
				var got []string
				for _, scope := range []string{"a", "b", ""} {
					if read.reach(t, w, held, scope) {
						got = append(got, scope)
					}
				}
				if !slices.Equal(got, want) {
					t.Fatalf("held to scope a, %s reaches the rows of %q, want %q", read.name, got, want)
				}
			})
		}
	}},
}
