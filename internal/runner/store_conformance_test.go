package runner

// The store contract's conformance suite (docs/design/store-interface.md),
// run against every kind: the sqlite kind the Runner ships and the in-memory
// kind. A case uses only the contract and the Runner's own functions over it
// (changeQueuedRun, finishRunIn, appendRunEntry); what it needs that the
// contract does not cover, a job made outside it, the journal's entries, a
// commit made to fail and a record that does not read, it asks of the kind
// under test. The sqlite kind's own cases, a restart and the lock, follow.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// storeKindUnderTest is a kind and what the suite asks of it beside the
// contract.
type storeKindUnderTest struct {
	name string
	// open is a new, empty store of the kind.
	open func(t *testing.T) runStore
	// seedJob keeps a job and its first revision, without the record of
	// its creation: the world's job, made directly.
	seedJob func(t *testing.T, st runStore, j Job)
	// schema is the schema the store is at, as the sqlite kind names it.
	schema func(t *testing.T, st runStore) string
	// failCommits makes every later commit fail with err, or succeed again
	// for nil.
	failCommits func(st runStore, err error)
	// spoil makes a run's record not read as one.
	spoil func(t *testing.T, st runStore, run string)
	// own are the kind's own cases.
	own []storeCase
}

type storeCase struct {
	name string
	run  func(t *testing.T, k storeKindUnderTest)
}

// privateStateDir is a private directory, by its physical path, as a state
// directory must be.
func privateStateDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openSQLiteForTest(t *testing.T, dir string) *sqliteStore {
	t.Helper()
	k, err := openSQLiteStore(storeOpening{dir: dir, workspace: "conformance", owner: "owner", migrate: true})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func storeKindsUnderTest() []storeKindUnderTest {
	return []storeKindUnderTest{
		{
			name: "sqlite",
			open: func(t *testing.T) runStore {
				k := openSQLiteForTest(t, privateStateDir(t))
				t.Cleanup(func() { k.close() })
				return k
			},
			seedJob: func(t *testing.T, st runStore, j Job) {
				// Two of the statements createJob makes (store_sqlite_jobs.go).
				db := st.(*sqliteStore).db
				if _, err := db.Exec("INSERT INTO jobs(id,release_id,record)VALUES(?,?,?)", j.ID, j.ReleaseID, string(encode(j))); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("INSERT INTO job_revisions(job_id,revision,release_id,record) VALUES (?,?,?,?)", j.ID, j.Revision, j.ReleaseID, string(encode(j))); err != nil {
					t.Fatal(err)
				}
			},
			schema: func(t *testing.T, st runStore) string {
				var schema string
				if err := st.(*sqliteStore).db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema); err != nil {
					t.Fatal(err)
				}
				return schema
			},
			failCommits: func(st runStore, err error) {
				k := st.(*sqliteStore)
				if err == nil {
					k.commit = (*sql.Tx).Commit
					return
				}
				k.commit = func(*sql.Tx) error { return err }
			},
			spoil: func(t *testing.T, st runStore, run string) {
				if _, err := st.(*sqliteStore).db.Exec("UPDATE runs SET record='{' WHERE id=?", run); err != nil {
					t.Fatal(err)
				}
			},
			own: sqliteOwnCases,
		},
		{
			name:        "memory",
			open:        func(t *testing.T) runStore { return newMemoryStore() },
			seedJob:     func(t *testing.T, st runStore, j Job) { memoryOf(st).seedJob(j) },
			schema:      func(t *testing.T, st runStore) string { return memoryOf(st).heldSchema() },
			failCommits: func(st runStore, err error) { memoryOf(st).failCommits(err) },
			spoil:       func(t *testing.T, st runStore, run string) { memoryOf(st).spoilRun(run) },
		},
		// The in-memory kind under the twice mode (store_twice_test.go):
		// every transaction's fn run twice, the first run thrown away.
		{
			name:        "twice",
			open:        func(t *testing.T) runStore { return &twiceStore{runStore: newMemoryStore(), record: newTwiceRecord()} },
			seedJob:     func(t *testing.T, st runStore, j Job) { memoryOf(st).seedJob(j) },
			schema:      func(t *testing.T, st runStore) string { return memoryOf(st).heldSchema() },
			failCommits: func(st runStore, err error) { memoryOf(st).failCommits(err) },
			spoil:       func(t *testing.T, st runStore, run string) { memoryOf(st).spoilRun(run) },
		},
	}
}

// memoryOf is the in-memory kind st is, or runs on under the twice mode.
func memoryOf(st runStore) *memoryStore { return underlying(st).(*memoryStore) }

// TestStoreKindsConform runs the conformance suite against every kind.
func TestStoreKindsConform(t *testing.T) {
	for _, k := range storeKindsUnderTest() {
		t.Run(k.name, func(t *testing.T) {
			for _, c := range slices.Concat(storeCases, widenedStoreCases, triggerStoreCases, jobStoreCases, listStoreCases, k.own) {
				t.Run(c.name, func(t *testing.T) { c.run(t, k) })
			}
		})
	}
}

// storeWorld is a store with a release and a job to queue runs for.
type storeWorld struct {
	st      runStore
	release Release
	job     Job
}

func newStoreWorld(t *testing.T, k storeKindUnderTest) storeWorld {
	t.Helper()
	w := storeWorld{st: k.open(t)}
	w.release = Release{SchemaVersion: "1", ID: "rel_conformance", PackID: "conformance", PackVersion: "1.0.0", Title: "Conformance", CreatedAt: now(), Tests: "passed"}
	must(t, w.st.update(func(tx runTx) error { return tx.createRelease(w.release) }))
	w.job = Job{SchemaVersion: "1", ID: "job_conformance", Name: "Conformance", ReleaseID: w.release.ID, Revision: 1, CreatedAt: now(), Workspace: "conformance", Owner: "owner"}
	k.seedJob(t, w.st, w.job)
	return w
}

// queuedRun is a run of the world's job, as a submission makes one.
func (w storeWorld) queuedRun(id string) Run {
	return Run{SchemaVersion: "1", ID: id, JobID: w.job.ID, ReleaseID: w.job.ReleaseID, Revision: w.job.Revision, State: "queued", Input: journalInput(), CreatedAt: now(), RequestedBy: "owner"}
}

// enqueue queues a run under key in one transaction, as a submission does:
// the run, then its entry.
func (w storeWorld) enqueue(id, key string) (Run, error) {
	r := w.queuedRun(id)
	return r, w.st.update(func(tx runTx) error {
		if err := tx.createRun(r, "owner", key, "sha256:"+key); err != nil {
			return err
		}
		_, err := tx.appendJournal(runChange(r, "", runnerActor, EventDetail{}))
		return err
	})
}

// claim claims the oldest queued run as the dispatcher does, and retries
// when another claimer took the run it read: it returns the run it claimed,
// or false when none is queued. A claim that conflicts without end is an
// error: each conflict is another claimer's success, and they are bounded.
func claim(st runStore) (Run, bool, error) {
	for range 10000 {
		r, err := st.oldestQueued()
		if absent(err) {
			return Run{}, false, nil
		}
		if err != nil {
			return Run{}, false, err
		}
		r.State, r.StartedAt, r.Attempt = "running", now(), 1
		switch err = changeQueuedRun(st, r); faultOf(err) {
		case "":
			return r, err == nil, err
		case faultConflict:
			continue
		default:
			return Run{}, false, err
		}
	}
	return Run{}, false, errors.New("a claim conflicted ten thousand times")
}

func wantFault(t *testing.T, err error, want storeFault, what string) {
	t.Helper()
	if got := faultOf(err); got != want {
		t.Fatalf("%s: fault %q (%v), want %q", what, got, err, want)
	}
}

func kindCount(kinds []string, kind string) int {
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

var storeCases = []storeCase{
	{"a release is created once", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		second := w.release
		second.Title = "Another"
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createRelease(second) }), faultTaken, "a second release under one id")
		held, err := w.st.release(w.release.ID)
		if err != nil || held.Title != w.release.Title || held.PackID != w.release.PackID {
			t.Fatal("the release held is not the first:", held.Title, err)
		}
		_, err = w.st.release("rel_none")
		wantFault(t, err, faultAbsent, "a release never made")
		if !absent(err) {
			t.Fatal("absent does not say a release never made is absent")
		}
	}},
	{"a run is queued once under its id and once under its request key", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		first, err := w.enqueue("run_a", "key-a")
		must(t, err)
		_, err = w.enqueue("run_a", "key-b")
		wantFault(t, err, faultTaken, "a second run under one id")
		_, err = w.enqueue("run_b", "key-a")
		wantFault(t, err, faultTaken, "a second run under one request key")
		held, err := w.st.requestedRun(w.job.ID, "owner", "key-a")
		must(t, err)
		if held.digest != "sha256:key-a" || held.id != first.ID {
			t.Fatal("the request's digest or run is not the first's:", held.digest, held.id)
		}
		if r, err := held.run(); err != nil || r.ID != first.ID {
			t.Fatal("the run held under the key is not the first:", r.ID, err)
		}
		for _, other := range [][3]string{{w.job.ID, "owner", "key-b"}, {w.job.ID, "trigger:t", "key-a"}, {"job_other", "owner", "key-a"}} {
			_, err = w.st.requestedRun(other[0], other[1], other[2])
			wantFault(t, err, faultAbsent, fmt.Sprint("another request key ", other))
		}
		if got := kindCount(journalKindsOf(t, w.st), "run.queued"); got != 1 {
			t.Fatal("run.queued entries:", got)
		}
	}},
	{"the queue is served in the order runs were queued", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		// Ids that sort against the order of queueing: the order is the
		// queue's, never the ids'.
		var order []string
		for i := range 5 {
			id := fmt.Sprintf("run_%d", 9-i)
			_, err := w.enqueue(id, id)
			must(t, err)
			order = append(order, id)
		}
		must(t, w.st.update(func(tx runTx) error {
			active, err := tx.activeRuns()
			if err != nil || active != 5 {
				t.Fatal("active runs:", active, err)
			}
			return nil
		}))
		var claimed []string
		for range order {
			r, ok, err := claim(w.st)
			if err != nil || !ok {
				t.Fatal("no run claimed:", ok, err)
			}
			claimed = append(claimed, r.ID)
		}
		if !slices.Equal(claimed, order) {
			t.Fatal("claimed in the order", claimed, "not", order)
		}
		_, err := w.st.oldestQueued()
		wantFault(t, err, faultAbsent, "an empty queue")
		// The runs left running are read in the order they were queued, as a
		// restart reads them, after one ended.
		done := w.queuedRun(order[2])
		done.State = "failed"
		must(t, finishRunIn(w.st, done))
		must(t, w.st.update(func(tx runTx) error {
			running, err := tx.runningRuns()
			if err != nil {
				return err
			}
			var ids []string
			for _, r := range running {
				ids = append(ids, r.ID)
			}
			if want := slices.Delete(slices.Clone(order), 2, 3); !slices.Equal(ids, want) {
				t.Fatal("running runs read in the order", ids, "not", want)
			}
			if active, err := tx.activeRuns(); err != nil || active != 4 {
				t.Fatal("active runs:", active, err)
			}
			return nil
		}))
	}},
	{"a run read queued is claimed once", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		_, err := w.enqueue("run_a", "key-a")
		must(t, err)
		first, err := w.st.oldestQueued()
		must(t, err)
		second, err := w.st.oldestQueued()
		must(t, err)
		first.State, second.State = "running", "running"
		must(t, changeQueuedRun(w.st, first))
		second.Problem = "a second claim"
		wantFault(t, changeQueuedRun(w.st, second), faultConflict, "a second claim of a run read queued")
		held, err := w.st.run("run_a")
		if err != nil || held.State != "running" || held.Problem != "" {
			t.Fatal("the second claim wrote:", held.State, held.Problem, err)
		}
		// A versioned update at a state the run is not in writes nothing.
		ended := held
		ended.State, ended.Problem = "failed", "ended in the queue"
		wantFault(t, changeQueuedRun(w.st, ended), faultConflict, "a running run ended as queued")
		if held, err = w.st.run("run_a"); err != nil || held.State != "running" || held.Problem != "" {
			t.Fatal("a conflicting update wrote:", held.State, held.Problem, err)
		}
		kinds := journalKindsOf(t, w.st)
		if kindCount(kinds, "run.started") != 1 || kindCount(kinds, "run.expired") != 0 {
			t.Fatal("entries for one claim:", kinds)
		}
	}},
	{"concurrent claimers grant every run once and lose none", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		const producers, perProducer, claimers = 4, 10, 6
		var produced atomic.Bool
		var wg sync.WaitGroup
		for p := range producers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perProducer {
					id := fmt.Sprintf("run_%d_%d", p, i)
					if _, err := w.enqueue(id, id); err != nil {
						t.Error(err)
					}
				}
			}()
		}
		// The claimers share nothing but the store: each keeps what it
		// claimed, and they are compared after.
		grants := make([][]string, claimers)
		var cw sync.WaitGroup
		for c := range claimers {
			cw.Add(1)
			go func() {
				defer cw.Done()
				for {
					r, ok, err := claim(w.st)
					if err != nil {
						t.Error(err)
						return
					}
					if ok {
						grants[c] = append(grants[c], r.ID)
						continue
					}
					if produced.Load() {
						// Produced and nothing queued: confirm once more.
						if _, err = w.st.oldestQueued(); absent(err) {
							return
						}
						continue
					}
					time.Sleep(time.Millisecond)
				}
			}()
		}
		wg.Wait()
		produced.Store(true)
		cw.Wait()
		seen := map[string]int{}
		for _, g := range grants {
			for _, id := range g {
				seen[id]++
			}
		}
		if len(seen) != producers*perProducer {
			t.Fatal("runs claimed:", len(seen), "of", producers*perProducer)
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatal(id, "was granted", n, "times")
			}
			if held, err := w.st.run(id); err != nil || held.State != "running" {
				t.Fatal(id, held.State, err)
			}
		}
		kinds := journalKindsOf(t, w.st)
		if kindCount(kinds, "run.queued") != producers*perProducer || kindCount(kinds, "run.started") != producers*perProducer {
			t.Fatal("entries:", kindCount(kinds, "run.queued"), kindCount(kinds, "run.started"))
		}
	}},
	{"an interrupted run is queued again once, keeping its attempts", func(t *testing.T, k storeKindUnderTest) {
		// A retry (retries.go) is the one writer that moves a run back to
		// queued: from interrupted, under the same fence as the claim.
		w := newStoreWorld(t, k)
		_, err := w.enqueue("run_a", "key-a")
		must(t, err)
		r, ok, err := claim(w.st)
		if err != nil || !ok {
			t.Fatal("no run claimed:", ok, err)
		}
		r.State, r.FinishedAt = "interrupted", now()
		must(t, finishRunIn(w.st, r))
		next := r
		next.State, next.StartedAt, next.FinishedAt = "queued", "", ""
		next.Attempts = []Attempt{{Number: 1, StartedAt: r.StartedAt, Outcome: "interrupted", Reason: runAttemptRestarted}}
		retry := func() error {
			return w.st.update(func(tx runTx) error {
				if err := tx.raiseSchema(retryStoreSchema); err != nil {
					return err
				}
				_, err := tx.putRun(next, "interrupted")
				return err
			})
		}
		must(t, retry())
		wantFault(t, retry(), faultConflict, "a second retry of a run queued again")
		again, ok, err := claim(w.st)
		if err != nil || !ok || again.ID != r.ID {
			t.Fatal("the run queued again is not claimed:", again.ID, ok, err)
		}
		if len(again.Attempts) != 1 || again.Attempts[0] != next.Attempts[0] {
			t.Fatalf("the kept attempt is not held: %+v", again.Attempts)
		}
		wantFault(t, w.st.update(func(tx runTx) error { return tx.raiseSchema("7") }), faultRefused, "a schema the kind does not know")
	}},
	{"a finished run keeps its counts and its error class", func(t *testing.T, k storeKindUnderTest) {
		// The counts and the class are members of the run's record
		// (run_results.go): a kind keeps them as it keeps the record, and a
		// run not finished holds neither.
		w := newStoreWorld(t, k)
		for _, id := range []string{"run_a", "run_b", "run_c"} {
			_, err := w.enqueue(id, id)
			must(t, err)
		}
		done, _, err := claim(w.st)
		must(t, err)
		done.State, done.FinishedAt, done.AuditBytes = "completed", now(), []byte(`{"recordVersion":"1"}`)
		done.Result = json.RawMessage(`{"outputVersion":"2","disposition":{"kind":"outcome","outcomeId":"proceed"}}`)
		must(t, finishRunIn(w.st, done))
		failed, _, err := claim(w.st)
		must(t, err)
		failed.State, failed.FinishedAt, failed.Problem, failed.ErrorClass = "failed", now(), "Failed.", classEvaluation
		must(t, finishRunIn(w.st, failed))
		for id, want := range map[string]string{
			"run_a": `"" {"decisions":1,"byKind":{"outcome":1},"byOutcome":{"proceed":1}}`,
			"run_b": `"evaluation" {"decisions":0,"byKind":{},"byOutcome":{}}`,
			"run_c": `"" null`,
		} {
			held, err := w.st.run(id)
			if got := fmt.Sprintf("%q %s", held.ErrorClass, encode(held.Counts)); err != nil || got != want {
				t.Errorf("%s holds %s (%v), want %s", id, got, err, want)
			}
			if err := w.st.update(func(tx runTx) error {
				if in, err := tx.run(id); err != nil || in.ErrorClass != held.ErrorClass || string(encode(in.Counts)) != string(encode(held.Counts)) {
					t.Errorf("%s in a transaction holds %q %s (%v)", id, in.ErrorClass, encode(in.Counts), err)
				}
				return errKeepNothing
			}); !errors.Is(err, errKeepNothing) {
				t.Fatal(err)
			}
		}
	}},
	{"a transaction that fails keeps nothing", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		before := journalKindsOf(t, w.st)
		abandoned := errors.New("the transaction is abandoned")
		for _, end := range []error{abandoned, errKeepNothing} {
			err := w.st.update(func(tx runTx) error {
				r := w.queuedRun("run_kept")
				if err := tx.createRun(r, "owner", "key", "sha256:key"); err != nil {
					return err
				}
				if _, err := tx.appendJournal(runChange(r, "", runnerActor, EventDetail{})); err != nil {
					return err
				}
				// The transaction sees its own writes.
				if _, err := tx.requestedRun(w.job.ID, "owner", "key"); err != nil {
					return err
				}
				release := w.release
				release.ID = "rel_kept"
				if err := tx.createRelease(release); err != nil {
					return err
				}
				if err := tx.createEntry(1, r.ID, []byte("{}")); err != nil {
					return err
				}
				return end
			})
			if err != end {
				t.Fatal("update did not return its function's error as it was:", err)
			}
			if _, err = w.st.run("run_kept"); !absent(err) {
				t.Fatal("a run was kept:", err)
			}
			if _, err = w.st.release("rel_kept"); !absent(err) {
				t.Fatal("a release was kept:", err)
			}
			if _, err = w.st.entrySequence("run_kept"); !absent(err) {
				t.Fatal("an entry was kept:", err)
			}
			if after := journalKindsOf(t, w.st); !slices.Equal(after, before) {
				t.Fatal("journal entries were kept:", after)
			}
		}
		// What one failed transaction gave back is given again.
		_, err := w.enqueue("run_kept", "key")
		must(t, err)
	}},
	{"a commit that fails is an error, never a success", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		_, err := w.enqueue("run_a", "key-a")
		must(t, err)
		before := journalKindsOf(t, w.st)
		failed := errors.New("the disk refused the commit")
		k.failCommits(w.st, failed)
		_, err = w.enqueue("run_b", "key-b")
		wantFault(t, err, faultDurability, "a queued run whose commit failed")
		if !errors.Is(err, failed) || err.Error() != failed.Error() {
			t.Fatal("the failure is not its cause's:", err)
		}
		r, err := w.st.oldestQueued()
		must(t, err)
		r.State = "running"
		wantFault(t, changeQueuedRun(w.st, r), faultDurability, "a claim whose commit failed")
		release := w.release
		release.ID = "rel_b"
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createRelease(release) }), faultDurability, "a release whose commit failed")
		if _, err = w.st.run("run_b"); !absent(err) {
			t.Fatal("a run whose commit failed was kept:", err)
		}
		if held, err := w.st.run("run_a"); err != nil || held.State != "queued" {
			t.Fatal("a claim whose commit failed was kept:", held.State, err)
		}
		if _, err = w.st.release("rel_b"); !absent(err) {
			t.Fatal("a release whose commit failed was kept:", err)
		}
		if after := journalKindsOf(t, w.st); !slices.Equal(after, before) {
			t.Fatal("journal entries whose commit failed were kept:", after)
		}
		k.failCommits(w.st, nil)
		_, err = w.enqueue("run_b", "key-b")
		must(t, err)
		must(t, changeQueuedRun(w.st, r))
	}},
	{"the chain gives a run one entry and is read in order", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		var runs []string
		records := map[string][]byte{}
		for i := range 5 {
			run, record := syntheticRun(i)
			runs, records[run] = append(runs, run), record
			must(t, w.st.update(func(tx runTx) error { return appendRunEntry(tx, run, record) }))
		}
		wantFault(t, w.st.update(func(tx runTx) error { return appendRunEntry(tx, runs[0], records[runs[0]]) }), faultTaken, "a second entry for a run")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createEntry(3, "run_other", []byte("{}")) }), faultTaken, "a second entry at a sequence")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createEntry(0, "run_other", []byte("{}")) }), faultRefused, "an entry before the first")
		var lines [][]byte
		for after := int64(0); ; {
			page, last, err := w.st.chainPage(after, 2)
			must(t, err)
			if len(page) == 0 {
				if last != after {
					t.Fatal("an empty page moved the cursor:", last)
				}
				break
			}
			if len(page) > 2 || last != after+int64(len(page)) {
				t.Fatal("a page of", len(page), "ending at", last, "after", after)
			}
			lines, after = append(lines, page...), last
		}
		if len(lines) != len(runs) {
			t.Fatal("lines:", len(lines))
		}
		for i, run := range runs {
			e, err := ParseChainEntry(lines[i])
			if err != nil || e.Run != run || e.Sequence != int64(i+1) || e.AuditDigest != digest(records[run]) {
				t.Fatal("entry", i+1, "is", e.Run, e.Sequence, err)
			}
			if i > 0 && e.Previous != digest(lines[i-1]) {
				t.Fatal("entry", i+1, "does not link to the one before")
			}
			line, err := w.st.entryLine(run)
			if err != nil || string(line) != string(lines[i]) {
				t.Fatal("the line of", run, "is not the one read in order:", err)
			}
			if sequence, err := w.st.entrySequence(run); err != nil || sequence != int64(i+1) {
				t.Fatal("the sequence of", run, "is", sequence, err)
			}
		}
		// A completed run is recorded with its entry, or not at all.
		_, err := w.enqueue("run_done", "key-done")
		must(t, err)
		r, ok, err := claim(w.st)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		r.State = "completed"
		if err = finishRunIn(w.st, r); err != errNoAuditBytes {
			t.Fatal("a completion without its record's bytes:", err)
		}
		if held, err := w.st.run(r.ID); err != nil || held.State != "running" {
			t.Fatal("a completion without its entry was recorded:", held.State, err)
		}
		r.AuditBytes = []byte(`{"recordVersion":"1"}`)
		must(t, finishRunIn(w.st, r))
		if sequence, err := w.st.entrySequence(r.ID); err != nil || sequence != int64(len(runs)+1) {
			t.Fatal("the completed run's entry is at", sequence, err)
		}
		if held, err := w.st.run(r.ID); err != nil || held.State != "completed" {
			t.Fatal(held.State, err)
		}
		if got := kindCount(journalKindsOf(t, w.st), "run.completed"); got != 1 {
			t.Fatal("run.completed entries:", got)
		}
	}},
	{"chain entries are read in order of sequence, whatever order they were made in", func(t *testing.T, k storeKindUnderTest) {
		st := k.open(t)
		// Made last-first, under runs whose names sort against the order.
		for _, e := range []struct {
			sequence int64
			run      string
		}{{9, "run_a"}, {2, "run_c"}, {4, "run_b"}} {
			must(t, st.update(func(tx runTx) error {
				return tx.createEntry(e.sequence, e.run, fmt.Appendf(nil, "%d", e.sequence))
			}))
		}
		must(t, st.update(func(tx runTx) error {
			last, line, err := tx.lastEntry()
			if err != nil || last != 9 || string(line) != "9" {
				t.Fatal("the last entry is", last, string(line), err)
			}
			return nil
		}))
		page, last, err := st.chainPage(2, -1)
		if err != nil || last != 9 || len(page) != 2 || string(page[0]) != "4" || string(page[1]) != "9" {
			t.Fatalf("the entries after 2 read %q to %d: %v", page, last, err)
		}
		if page, last, err = st.chainPage(0, 0); err != nil || len(page) != 0 || last != 0 {
			t.Fatalf("a page of none read %q to %d: %v", page, last, err)
		}
	}},
	{"nothing held reads absent", func(t *testing.T, k storeKindUnderTest) {
		st := k.open(t)
		_, err := st.run("run_none")
		wantFault(t, err, faultAbsent, "run")
		_, err = st.job("job_none")
		wantFault(t, err, faultAbsent, "job")
		_, err = st.release("rel_none")
		wantFault(t, err, faultAbsent, "release")
		_, err = st.requestedRun("job_none", "owner", "key")
		wantFault(t, err, faultAbsent, "requested run")
		_, err = st.oldestQueued()
		wantFault(t, err, faultAbsent, "oldest queued")
		_, err = st.entrySequence("run_none")
		wantFault(t, err, faultAbsent, "entry sequence")
		_, err = st.entryLine("run_none")
		wantFault(t, err, faultAbsent, "entry line")
		if page, last, err := st.chainPage(0, 10); err != nil || len(page) != 0 || last != 0 {
			t.Fatal("an empty chain's page:", len(page), last, err)
		}
		wantFault(t, st.update(func(tx runTx) error {
			_, err := tx.putRun(Run{ID: "run_none", State: "running"}, anyState)
			return err
		}), faultAbsent, "a run never queued, changed")
		wantFault(t, st.update(func(tx runTx) error {
			_, _, err := tx.lastEntry()
			return err
		}), faultAbsent, "an empty chain's last entry")
	}},
	{"a record that does not read is unreadable", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		_, err := w.enqueue("run_a", "key-a")
		must(t, err)
		k.spoil(t, w.st, "run_a")
		_, err = w.st.run("run_a")
		wantFault(t, err, faultUnreadable, "a spoiled run")
		_, err = w.st.oldestQueued()
		wantFault(t, err, faultUnreadable, "a spoiled run at the head of the queue")
		held, err := w.st.requestedRun(w.job.ID, "owner", "key-a")
		must(t, err)
		if _, err = held.run(); err == nil {
			t.Fatal("a spoiled run read under its request key")
		}
	}},
}

// The sqlite kind's own cases: what it keeps across a restart, and the lock
// that holds the store for one process.
var sqliteOwnCases = []storeCase{
	{"a reopened store holds what it held", func(t *testing.T, k storeKindUnderTest) {
		dir := privateStateDir(t)
		st := openSQLiteForTest(t, dir)
		w := storeWorld{st: st}
		w.release = Release{SchemaVersion: "1", ID: "rel_restart", PackID: "restart", Title: "Restart", Tests: "passed"}
		must(t, st.update(func(tx runTx) error { return tx.createRelease(w.release) }))
		w.job = Job{SchemaVersion: "1", ID: "job_restart", ReleaseID: w.release.ID, Revision: 1}
		k.seedJob(t, st, w.job)
		for _, id := range []string{"run_c", "run_b", "run_a"} {
			_, err := w.enqueue(id, id)
			must(t, err)
		}
		claimed, ok, err := claim(st)
		if err != nil || !ok || claimed.ID != "run_c" {
			t.Fatal(claimed.ID, ok, err)
		}
		done, ok, err := claim(st)
		if err != nil || !ok || done.ID != "run_b" {
			t.Fatal(done.ID, ok, err)
		}
		done.State, done.AuditBytes = "completed", []byte(`{"recordVersion":"1"}`)
		must(t, finishRunIn(st, done))
		line, err := st.entryLine(done.ID)
		must(t, err)
		journal := journalKindsOf(t, st)
		must(t, st.close())

		st = openSQLiteForTest(t, dir)
		defer st.close()
		if held, err := st.run(claimed.ID); err != nil || held.State != "running" || held.StartedAt != claimed.StartedAt {
			t.Fatal("the claimed run is not where it was:", held.State, err)
		}
		if held, err := st.run(done.ID); err != nil || held.State != "completed" {
			t.Fatal("the completed run is not where it was:", held.State, err)
		}
		if next, err := st.oldestQueued(); err != nil || next.ID != "run_a" {
			t.Fatal("the queue is not where it was:", next.ID, err)
		}
		if again, err := st.entryLine(done.ID); err != nil || string(again) != string(line) {
			t.Fatal("the chain is not where it was:", err)
		}
		if held, err := st.release(w.release.ID); err != nil || held.Title != w.release.Title {
			t.Fatal("the release is not where it was:", err)
		}
		if held, err := st.requestedRun(w.job.ID, "owner", "run_a"); err != nil || held.digest != "sha256:run_a" {
			t.Fatal("the request is not where it was:", err)
		}
		if after := journalKindsOf(t, st); !slices.Equal(after, journal) {
			t.Fatal("the journal is not where it was:", after, journal)
		}
		must(t, st.update(func(tx runTx) error {
			running, err := tx.runningRuns()
			if err != nil || len(running) != 1 || running[0].ID != claimed.ID {
				t.Fatal("runs left running:", len(running), err)
			}
			return nil
		}))
	}},
	{"one process holds the store", func(t *testing.T, k storeKindUnderTest) {
		dir := privateStateDir(t)
		first := openSQLiteForTest(t, dir)
		if second, err := openSQLiteStore(storeOpening{dir: dir, workspace: "conformance", owner: "owner", migrate: true}); err == nil {
			second.close()
			first.close()
			t.Fatal("a second opening took a store another holds")
		} else if err.Error() != "another runner owns this workspace" {
			t.Fatal(err)
		}
		must(t, first.close())
		again := openSQLiteForTest(t, dir)
		must(t, again.close())
	}},
	{"a transaction that fails or whose commit fails keeps no journal entry in the database", func(t *testing.T, k storeKindUnderTest) {
		// Read from the events table itself, not through journalPage.
		w := newStoreWorld(t, k)
		db := w.st.(*sqliteStore).db
		rows := func() (count int, last int64) {
			t.Helper()
			must(t, db.QueryRow("SELECT count(*),coalesce(max(sequence),0) FROM events").Scan(&count, &last))
			return count, last
		}
		count, last := rows()
		err := w.st.update(func(tx runTx) error {
			if _, err := tx.appendJournal(change{kind: "runner.started", by: runnerActor}); err != nil {
				return err
			}
			return errors.New("the transaction is abandoned")
		})
		if err == nil {
			t.Fatal("an abandoned transaction succeeded")
		}
		k.failCommits(w.st, errors.New("the disk refused the commit"))
		wantFault(t, w.st.update(func(tx runTx) error {
			_, err := tx.appendJournal(change{kind: "runner.started", by: runnerActor})
			return err
		}), faultDurability, "an entry whose commit failed")
		k.failCommits(w.st, nil)
		if c, l := rows(); c != count || l != last {
			t.Fatalf("the events table holds %d entries to %d, where it held %d to %d", c, l, count, last)
		}
	}},
	{"a read that fails is unavailable, and a record that does not decode unreadable", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		st := w.st.(*sqliteStore)
		held := st.sqliteReads
		st.sqliteReads = sqliteReads{failingReads{held.q, ""}}
		reads := map[string]func() error{
			"release":          func() error { _, err := st.release(w.release.ID); return err },
			"job":              func() error { _, err := st.job(w.job.ID); return err },
			"jobCreation":      func() error { _, err := st.jobCreation(w.release.ID); return err },
			"jobRevision":      func() error { _, _, err := st.jobRevision(w.job.ID, 1); return err },
			"releaseUses":      func() error { _, err := st.releaseUses(w.release.ID); return err },
			"packConflicts":    func() error { _, err := st.packConflicts("p", "1", "d"); return err },
			"dialectConflicts": func() error { _, err := st.dialectConflicts("d", "1", "d"); return err },
			"trigger":          func() error { _, _, err := st.trigger("trg_1"); return err },
			"occurrence":       func() error { _, err := st.occurrence("occ_1"); return err },
			"brief":            func() error { _, err := st.brief("job:" + w.job.ID); return err },
			"profileRow":       func() error { _, err := st.profileRow(registeredProfilesKey); return err },
			"recordScope":      func() error { _, err := st.recordScope("jobs", w.job.ID); return err },
			"journalPage":      func() error { _, _, err := st.journalPage(journalFilter{}, journalRange{limit: 1}); return err },
		}
		for name, read := range reads {
			wantFault(t, read(), faultUnavailable, "a failed read of "+name)
		}
		st.sqliteReads = held
		// Within a transaction, the rows of a query that fails, and a record
		// that does not decode.
		tx, err := st.db.Begin()
		must(t, err)
		defer tx.Rollback()
		wantFault(t, eachRecord(tx, "queue", "SELECT record FROM a_table_never_made", func([]byte) error { return nil }), faultUnavailable, "a failed query of records")
		wantFault(t, eachRecord(tx, "queue", "SELECT '{'", func(b []byte) error { _, err := decodeRun(b); return err }), faultUnreadable, "a record that does not decode")
	}},
}

// failingReads answers each read whose statement names table, or every read
// for "", with a statement that fails, as a store that is busy, damaged or
// closed answers it.
type failingReads struct {
	sqliteQuerier
	table string
}

func (f failingReads) fails(query string) bool {
	return f.table == "" || strings.Contains(query, " "+f.table+" ") || strings.HasSuffix(query, " "+f.table)
}

func (f failingReads) QueryRow(query string, args ...any) *sql.Row {
	if f.fails(query) {
		return f.sqliteQuerier.QueryRow("SELECT record FROM a_table_never_made")
	}
	return f.sqliteQuerier.QueryRow(query, args...)
}

func (f failingReads) Query(query string, args ...any) (*sql.Rows, error) {
	if f.fails(query) {
		return f.sqliteQuerier.Query("SELECT record FROM a_table_never_made")
	}
	return f.sqliteQuerier.Query(query, args...)
}
