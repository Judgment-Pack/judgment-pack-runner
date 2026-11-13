package runner

// The twice mode of a store kind (docs/design/store-postgresql.md, D2): a
// kind that retries a transaction runs its fn again, so fn must do on its
// second run what it would have done had that run been its first. In this
// mode update runs fn, throws its writes and its answer away as a
// serialization failure would, commits a test's change between the runs if
// one is set, and runs fn again, keeping that run. A closure that carries its
// first run's decision into its second, or that reads anything but the store,
// its arguments and the clock, answers here otherwise than it would have.
//
// The conformance suite runs the in-memory kind under the mode (the "twice"
// kind); the Service's flows run the sqlite kind, the one a Service opens,
// under it, and TestEveryTransactionIsSafeToRunTwice holds every function
// that opens a transaction, found in the source, to have run there.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// errThrownAway is the first run's answer, which the mode throws away with
// the run's writes.
var errThrownAway = errors.New("the first of a transaction's two runs, thrown away")

// twiceRecord is what the mode saw, across every store opened under it: the
// functions that opened a transaction, and the changes set to be made
// between the two runs of a function's next transaction.
type twiceRecord struct {
	mu      sync.Mutex
	opened  map[string]int
	between map[string]func(runStore) error
}

func newTwiceRecord() *twiceRecord {
	return &twiceRecord{opened: map[string]int{}, between: map[string]func(runStore) error{}}
}

// betweenRuns makes change between the two runs of the next transaction
// that opener opens, once: outside any transaction, given the store the
// runs are made in, as another writer, or a file's writer, would make it.
// The test fails if no such transaction is opened.
func (r *twiceRecord) betweenRuns(t *testing.T, opener string, change func(runStore) error) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.between[opener] = change
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, left := r.between[opener]; left {
			delete(r.between, opener)
			t.Errorf("%s opened no transaction to make the change between its runs", opener)
		}
	})
}

// saw notes a transaction of opener's, and takes the change set for it.
func (r *twiceRecord) saw(opener string) func(runStore) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened[opener]++
	for set, change := range r.between {
		if sameOpener(opener, set) {
			delete(r.between, set)
			return change
		}
	}
	return nil
}

// sameOpener says whether the function runtime named seen is the one the
// source names name. A closure inlined into its caller is named after both,
// "(*Service).handler.(*Service).briefHandler", the function it was written
// in last.
func sameOpener(seen, name string) bool {
	return seen == name || strings.HasSuffix(seen, "."+name)
}

func (r *twiceRecord) openers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.opened))
}

func (r *twiceRecord) ran(opener string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for seen := range r.opened {
		if sameOpener(seen, opener) {
			return true
		}
	}
	return false
}

// twiceStore is a kind under the mode.
type twiceStore struct {
	runStore
	record *twiceRecord
}

func (k *twiceStore) update(fn func(runTx) error) error {
	between := k.record.saw(transactionOpener())
	if err := k.runStore.update(func(tx runTx) error {
		fn(tx)
		return errThrownAway
	}); !errors.Is(err, errThrownAway) {
		return err
	}
	if between != nil {
		if err := between(k.runStore); err != nil {
			return fmt.Errorf("the change between the runs: %w", err)
		}
	}
	return k.runStore.update(fn)
}

// transactionOpener is the function that called update, as runtime names
// it without its package: "(*Service).createJobWith", "finishRunIn". A
// closure is its enclosing function's.
func transactionOpener() string {
	pc := make([]uintptr, 1)
	// 0 is runtime.Callers, 1 transactionOpener, 2 update, 3 its caller.
	if runtime.Callers(3, pc) == 0 {
		return ""
	}
	frame, _ := runtime.CallersFrames(pc).Next()
	return sourceName(frame.Function)
}

// sourceName is a function's name as runtime reports it, without its
// package path or the suffixes of the closures inside it.
func sourceName(function string) string {
	name := function[strings.LastIndex(function, "/")+1:]
	name = name[strings.Index(name, ".")+1:]
	parts := strings.Split(name, ".")
	for len(parts) > 1 {
		last := parts[len(parts)-1]
		if !strings.HasPrefix(last, "func") && strings.Trim(last, "0123456789") != "" {
			break
		}
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, ".")
}

// twiceActive is the record of the mode in force, if any.
var twiceActive *twiceRecord

// twiceMode opens every store of the default kind under the mode, for the
// rest of the test, and returns what it sees; a test inside another under
// the mode shares its record.
func twiceMode(t *testing.T) *twiceRecord {
	t.Helper()
	if twiceActive != nil {
		return twiceActive
	}
	open := storeKinds[defaultStoreKind]
	record := newTwiceRecord()
	storeKinds[defaultStoreKind] = func(o storeOpening) (runStore, error) {
		st, err := open(o)
		if err != nil {
			return nil, err
		}
		return &twiceStore{runStore: st, record: record}, nil
	}
	twiceActive = record
	t.Cleanup(func() {
		storeKinds[defaultStoreKind] = open
		twiceActive = nil
	})
	return record
}

// underlying is the kind a store under the mode runs on, or the store.
func underlying(st runStore) runStore {
	if k, ok := st.(*twiceStore); ok {
		return k.runStore
	}
	return st
}

// transactionOpeners are the functions of the package's source, outside its
// tests and the kinds' own files, that call a store's update: each opens a
// transaction whose fn a kind may run again. They are found by the call, not
// listed, so that a new one is held to the gate as it is written.
func transactionOpeners(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var openers []string
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "store_sqlite") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			opens := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "update" && len(call.Args) == 1 {
						opens = true
					}
				}
				return true
			})
			if opens {
				openers = append(openers, declName(fn))
			}
		}
	}
	slices.Sort(openers)
	return openers
}

// declName is a function's name as sourceName gives it.
func declName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	switch r := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		return "(*" + r.X.(*ast.Ident).Name + ")." + fn.Name.Name
	case *ast.Ident:
		return r.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// The Service's flows, run with every transaction run twice: each still
// writes what it writes, and answers what it answers, when its fn runs a
// second time. Every function that opens a transaction runs here.
func TestEveryTransactionIsSafeToRunTwice(t *testing.T) {
	record := twiceMode(t)
	for _, flow := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"every change of state", TestEveryTransitionWritesItsEntry},
		{"an upgrade", TestJobUpgradePinsAndReceipts},
		{"a retry", TestAnInterruptedRunIsRetriedKeepingItsFirstAttempt},
		{"a brief", TestOperationalBriefPersistsWithFrozenRun},
		{"registered profiles", TestRegisteredProfilesPersistWithoutRestartingOrMutatingPins},
		{"runs finished at once", TestConcurrentRunsAreEachChainedOnce},
		{"a change between the runs", TestAClosureRunAgainDecidesFromTheStore},
	} {
		t.Run(flow.name, flow.run)
	}
	openers := transactionOpeners(t)
	if len(openers) == 0 {
		t.Fatal("no function in the source opens a transaction")
	}
	for _, opener := range openers {
		if !record.ran(opener) {
			t.Errorf("%s opens a transaction that no flow here ran twice; the flows ran %v", opener, record.openers())
		}
	}
}

// upgradeNamingALaterProfile is a job whose paused trigger acquires through
// a Gateway mapping, and an upgrade to a release whose mapping names a
// profile, "later", the installation has not registered: the upgrade, which
// leaves the job unchanged when it is refused, and the profile's
// registration.
func upgradeNamingALaterProfile(t *testing.T) (func() (Job, error), func(runStore) error) {
	t.Helper()
	s, _, o, _ := preparationFixture(t)
	o.State, o.Reason, o.PendingInput = "expired", "queue-expired", nil
	must(t, s.saveOccurrence(o))
	j, e := s.job(o.JobID)
	must(t, e)
	first, e := s.release(j.ReleaseID)
	must(t, e)
	later := s.cfg.InputProfiles[0]
	later.ID = "later"
	connection := s.cfg.GatewayConnections[0]
	connection.Profile = later.ID
	s.cfg.GatewayConnections = append(slices.Clone(s.cfg.GatewayConnections), connection)
	var mapping InputMapping
	must(t, json.Unmarshal(encode(*first.InputMapping), &mapping))
	mapping.Sources[0].Profile, mapping.Sources[0].ProfileDigest = later.ID, profileHash(later)
	next := first
	next.ID, next.CreatedAt, next.InputMapping = id("release_"), now(), &mapping
	must(t, s.saveRelease(next))
	upgrade := func() (Job, error) {
		upgraded, e := s.upgradeJob(context.Background(), j.ID, UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: j.Revision, Reviewed: true})
		if e != nil {
			if held, err := s.job(j.ID); err != nil || held != j {
				t.Error("the refused upgrade changed the job", held, err)
			}
		}
		return upgraded, e
	}
	registered := func(runStore) error {
		_, err := s.registerInputProfile(later)
		return err
	}
	return upgrade, registered
}

// Each closure that broke the rule for fn, run twice with the change that
// shows it made between its runs; and the chain's append, which keeps it.
func TestAClosureRunAgainDecidesFromTheStore(t *testing.T) {
	record := twiceMode(t)
	// ended ends an occurrence, as its expiry does, in a transaction of its
	// own.
	ended := func(o Occurrence) func(runStore) error {
		return func(st runStore) error {
			return st.update(func(tx runTx) error {
				o.State, o.Reason, o.Input = "expired", "queue-expired", nil
				return recordOccurrence(tx, o, "", runnerActor, nil)
			})
		}
	}
	// An admission whose first run met the job's overlap, which ended before
	// its second, admits the occurrence: it does not carry the first run's
	// skip, nor drop the occurrence's input.
	admittedAccepted := func(w *coverWorld, before int) {
		w.t.Helper()
		var admitted []Occurrence
		for _, e := range w.entries()[before:] {
			if e.Kind == "occurrence.received" || e.Kind == "occurrence.skipped" {
				admitted = append(admitted, storedOccurrence(w.t, w.s, e.Concerns.Occurrence))
			}
		}
		if len(admitted) != 1 || admitted[0].State != "accepted" || admitted[0].Input == nil && admitted[0].PendingInput == nil {
			w.t.Fatalf("admitted %+v", admitted)
		}
	}
	t.Run("a schedule", func(t *testing.T) {
		w := newCoverWorld(t)
		tr := w.scheduleTrigger("")
		tr.Config.Overlap = "skip"
		at := time.Now().UTC().Truncate(time.Second)
		tr.NextAt = at.Add(-time.Second).Format(time.RFC3339)
		must(t, w.s.writeTrigger(tr, "", false))
		blocking := w.admitted(tr, nil)
		before := len(w.entries())
		record.betweenRuns(t, "(*Service).scheduleTick", ended(blocking))
		must(t, w.s.scheduleTick(tr, "", at))
		admittedAccepted(w, before)
	})
	t.Run("a file change", func(t *testing.T) {
		w := newCoverWorld(t)
		tr := w.fileTrigger(`{"facts":{"version":1}}`)
		tr.Config.Overlap = "skip"
		must(t, w.s.writeTrigger(tr, "", false))
		blocking := w.admitted(tr, nil)
		before := len(w.entries())
		record.betweenRuns(t, "(*Service).fileTick", ended(blocking))
		must(t, w.fileChanged(tr, `{"facts":{"version":2}}`))
		admittedAccepted(w, before)
	})
	t.Run("a cloud signal", func(t *testing.T) {
		w := newCoverWorld(t)
		tr, conn := w.cloudTrigger("skip")
		tr.Config.Overlap = "skip"
		must(t, w.s.writeTrigger(tr, "", false))
		blocking := w.admitted(tr, nil)
		before := len(w.entries())
		record.betweenRuns(t, "(*Service).receiveCloud", ended(blocking))
		if _, e := w.s.receiveCloud(conn, cloudSignal(tr, time.Now())); e != nil {
			t.Fatal(e)
		}
		admittedAccepted(w, before)
	})
	t.Run("an event", func(t *testing.T) {
		w := newCoverWorld(t)
		tr, key := w.eventTrigger("skip")
		blocking := w.admitted(tr, nil)
		before := len(w.entries())
		record.betweenRuns(t, "(*Service).admitBoundEvent", ended(blocking))
		if _, _, e := w.s.event(tr.ID, key, delivery("after-overlap")); e != nil {
			t.Fatal(e)
		}
		admittedAccepted(w, before)
	})
	// An event whose first run met a full queue, which had room by its
	// second, is admitted and answered admitted: the first run's refusal is
	// not carried into the answer of a transaction that committed.
	t.Run("an event at a full queue that drains", func(t *testing.T) {
		s, queued := fullQueue(t, 0)
		before := len(s.w.entries())
		record.betweenRuns(t, "(*Service).admitBoundEvent", ended(queued[0]))
		got := s.post(string(encode(delivery("after-drain"))))
		if code, a := answerOf(t, got.Code, got.Body.Bytes()); code != 202 || a.Occurrence.State != "accepted" {
			t.Fatalf("answered %d %s", code, got.Body.String())
		}
		if code := s.held("after-drain"); code != 200 {
			t.Fatal("the admitted event is not held:", code)
		}
		admittedAccepted(s.w, before)
	})
	// A retry checks the attempt directories once, before its transaction:
	// a trail that is gone by the second run still refuses the retry, and a
	// run that moved to its next attempt between the runs is refused as
	// changed, its new attempt's directory not having been checked.
	t.Run("a retry whose trail goes between the runs", func(t *testing.T) {
		s, cfg, r := interruptedRun(t, "", false)
		audit := filepath.Join(cfg.Dir, "attempts", r.ID, "audit")
		must(t, os.MkdirAll(audit, 0700))
		trail := filepath.Join(audit, "evaluations.jsonl")
		must(t, os.WriteFile(trail, []byte(`{"decision":"x"}`+"\n"), 0600))
		record.betweenRuns(t, "(*Service).retryRunBy", func(runStore) error { return os.WriteFile(trail, nil, 0600) })
		refusedAsAudited(t, s, r, "1")
	})
	t.Run("a retry whose run moves to its next attempt between the runs", func(t *testing.T) {
		s, cfg, r := interruptedRun(t, "", false)
		later := r
		later.Attempt, later.Attempts = 2, []Attempt{{Number: 1, Outcome: "interrupted", Reason: runAttemptRestarted}}
		record.betweenRuns(t, "(*Service).retryRunBy", func(st runStore) error {
			audit := filepath.Join(cfg.Dir, "attempts", r.ID+".2", "audit")
			if err := os.MkdirAll(audit, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(audit, "evaluations.jsonl"), []byte(`{"decision":"x"}`+"\n"), 0600); err != nil {
				return err
			}
			return st.update(func(tx runTx) error { _, err := tx.putRun(later, anyState); return err })
		})
		got := serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry", `{"attempt":2}`, nil)
		if got.Code != 409 || !strings.Contains(got.Body.String(), "attempt_mismatch") {
			t.Fatalf("%d %s", got.Code, got.Body)
		}
		if held, e := s.run(r.ID); e != nil || runText(held) != runText(later) {
			t.Fatal("a refused retry changed the run", e)
		}
	})
	// A retry whose directories were read while its run still ran is
	// refused as a retry of a running run is, though the run is interrupted
	// by the second run with a trail its Runtime wrote: the directories were
	// read before the trail was.
	t.Run("a retry checked while its run ran, interrupted with a trail between the runs", func(t *testing.T) {
		s, cfg, r := interruptedRun(t, "", false)
		running := r
		running.State, running.FinishedAt, running.InterruptedAt, running.Problem = "running", "", "", ""
		must(t, underlying(s.store).update(func(tx runTx) error { _, err := tx.putRun(running, anyState); return err }))
		record.betweenRuns(t, "(*Service).retryRunBy", func(st runStore) error {
			audit := filepath.Join(cfg.Dir, "attempts", r.ID, "audit")
			if err := os.MkdirAll(audit, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(audit, "evaluations.jsonl"), []byte(`{"decision":"x"}`+"\n"), 0600); err != nil {
				return err
			}
			return st.update(func(tx runTx) error { _, err := tx.putRun(r, anyState); return err })
		})
		got := serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry", `{"attempt":1}`, nil)
		if got.Code != 409 || !strings.Contains(got.Body.String(), "run_not_retryable") {
			t.Fatalf("%d %s", got.Code, got.Body)
		}
		if held, e := s.run(r.ID); e != nil || runText(held) != runText(r) {
			t.Fatal("a refused retry changed the run", e)
		}
	})
	// A job's creation answers the profiles as they were when it was asked:
	// a profile selected between its runs does not refuse the job its first
	// run made, and refuses the next.
	t.Run("a job created while a profile it names is selected", func(t *testing.T) {
		w := newCoverWorld(t)
		f := selectedProducer(t)
		mapping := f.input.Source.Mapping
		release := func() Release {
			r := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), PackID: "journal", PackVersion: "1.0.0", Title: "Journal", RuntimeDigest: w.release.RuntimeDigest, CreatedAt: now(), Sample: journalInput(), Tests: "not-run", InputMapping: &mapping}
			must(t, w.s.saveRelease(r))
			return r
		}
		first, next := release(), release()
		record.betweenRuns(t, "(*Service).createJobWith", func(runStore) error {
			_, err := w.s.admitSelectedProfile(f.profile)
			return err
		})
		if _, e := w.s.createJob("Before", first.ID); e != nil {
			t.Fatal("the job its first run made:", e)
		}
		if _, e := w.s.createJob("After", next.ID); refusedCode(e) != codeInputProfileSelected {
			t.Fatal("a job naming a selected profile:", e)
		}
	})
	// An upgrade checks its triggers against the profiles as they were when
	// it was asked: a profile its release names, registered between its
	// runs, does not make a trigger its first run found incompatible
	// compatible. Registered before the upgrade, the same profile makes the
	// same upgrade pass (the control).
	t.Run("an upgrade while a profile its release names is registered", func(t *testing.T) {
		upgrade, registered := upgradeNamingALaterProfile(t)
		record.betweenRuns(t, "(*Service).upgradeJob", registered)
		if _, e := upgrade(); refusedCode(e) != "trigger_upgrade_incompatible" {
			t.Fatal("an upgrade whose trigger was found incompatible:", e)
		}
	})
	t.Run("an upgrade after a profile its release names is registered", func(t *testing.T) {
		upgrade, registered := upgradeNamingALaterProfile(t)
		must(t, registered(nil))
		if _, e := upgrade(); e != nil {
			t.Fatal("the control's upgrade:", e)
		}
	})
	// A finish whose first run met one chain head, and whose second meets
	// another finisher's entry, continues the chain from that entry.
	t.Run("a finish after another finisher's entry", func(t *testing.T) {
		w := newCoverWorld(t)
		submitRunning := func(key string) Run {
			r, _, e := w.s.submit(w.job.ID, key, journalInput())
			must(t, e)
			r.State, r.StartedAt, r.Attempt = "running", now(), 1
			must(t, w.s.changeRun(r))
			r.State, r.FinishedAt, r.AuditBytes = "completed", now(), []byte(`{"recordVersion":"1","run":"`+key+`"}`)
			return r
		}
		mine, other := submitRunning("mine"), submitRunning("other")
		record.betweenRuns(t, "finishRunIn", func(st runStore) error { return finishRunIn(st, other) })
		must(t, w.s.finishRun(mine))
		first, _, e := w.s.runEntrySequence(other.ID)
		must(t, e)
		second, _, e := w.s.runEntrySequence(mine.ID)
		must(t, e)
		if first != 1 || second != 2 {
			t.Fatalf("the chain holds the other finisher's entry at %d and this one's at %d", first, second)
		}
	})
}

// TestTheTwiceModeRunsAFunctionTwice: a transaction under the mode runs its
// function twice, the change set between the runs made between them, and
// keeps only what the second run writes.
func TestTheTwiceModeRunsAFunctionTwice(t *testing.T) {
	record := newTwiceRecord()
	st := &twiceStore{runStore: newMemoryStore(), record: record}
	var seen []string
	record.betweenRuns(t, "TestTheTwiceModeRunsAFunctionTwice", func(runStore) error {
		seen = append(seen, "between")
		return nil
	})
	runs := 0
	err := st.update(func(tx runTx) error {
		runs++
		seen = append(seen, "run")
		// The first run writes one row, the second another.
		key := map[int]string{1: selectedProfilesKey, 2: registeredProfilesKey}[runs]
		return tx.putProfileRow(key, `{"run":`+strconv.Itoa(runs)+`}`)
	})
	if err != nil || runs != 2 || strings.Join(seen, ",") != "run,between,run" {
		t.Fatalf("the function ran %d times (%s): %v", runs, strings.Join(seen, ","), err)
	}
	if row, err := st.profileRow(selectedProfilesKey); !absent(err) {
		t.Fatalf("the first run's write is kept: %q %v", row, err)
	}
	if row, err := st.profileRow(registeredProfilesKey); err != nil || row != `{"run":2}` {
		t.Fatalf("the second run's write is not kept: %q %v", row, err)
	}
}
