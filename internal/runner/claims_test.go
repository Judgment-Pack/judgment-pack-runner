package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// leasingWrapper is a store that says it leases its claims, as the second
// kind does, over a sqlite store: it counts the renewals made through it,
// fails each while fail is set, and holds each from the holdFrom-th on
// until hold is closed.
type leasingWrapper struct {
	runStore
	leasing  bool
	renewals atomic.Int32
	fail     atomic.Bool
	holdFrom int32
	hold     chan struct{}
}

func (l *leasingWrapper) leasesClaims() bool { return l.leasing }

func (l *leasingWrapper) update(fn func(runTx) error) error {
	return l.runStore.update(func(tx runTx) error { return fn(leasingTx{tx, l}) })
}

type leasingTx struct {
	runTx
	l *leasingWrapper
}

func (t leasingTx) renewClaim(run string, c runClaim) error {
	n := t.l.renewals.Add(1)
	if t.l.holdFrom > 0 && n >= t.l.holdFrom {
		<-t.l.hold
	}
	if t.l.fail.Load() {
		return storeFailure(faultConflict, "run", errors.New("the claim's lease ran out"))
	}
	if c.token == "" {
		return errors.New("a renewal holds no claim")
	}
	return t.runTx.renewClaim(run, c)
}

// shortClaimTimes makes the renewals and the stop of claims.go short for
// one test.
func shortClaimTimes(t *testing.T, renewal, stop time.Duration) {
	was := claimTimes
	claimTimes.renewal, claimTimes.stop = renewal, stop
	t.Cleanup(func() { claimTimes = was })
}

// leasingRun queues a run of a Runtime that waits at gate, on w's store
// wrapped as l, and dispatches it in the background. The Runtime writes its
// process id to pid; it also ends by itself once the gate's directory is gone
// or after 30 seconds, so that no test, failed or killed, leaves it running.
// The test's cleanup ends what it started (endLeasingRun).
func leasingRun(t *testing.T, w *coverWorld, l *leasingWrapper, gate, pid string) (string, chan error) {
	t.Helper()
	runtime := fakeRuntime(t, w.cfg.Dir, "echo $$ > '"+pid+"'\nn=0\nwhile [ ! -e '"+gate+"' ] && [ -d '"+filepath.Dir(gate)+"' ] && [ $n -lt 600 ]; do sleep 0.05; n=$((n+1)); done\nexit 3")
	r := journalRelease(t, w.s, runtime)
	j, e := w.s.createJob("Leased", r.ID)
	must(t, e)
	run, _, e := w.s.submit(j.ID, "one", journalInput())
	must(t, e)
	underLeasing(w, l)
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		_, e := w.s.dispatchNext(context.Background())
		done <- e
	}()
	t.Cleanup(func() { endLeasingRun(t, gate, pid, finished) })
	return run.ID, done
}

// endLeasingRun ends what leasingRun started, whatever the test did: it opens
// the gate, and if the dispatcher has not returned within a second, kills the
// Runtime by its process id (the dispatcher has not reaped it, so the id is
// still its), then waits for the dispatcher and fails the test if the Runtime
// is still running.
func endLeasingRun(t *testing.T, gate, pid string, finished <-chan struct{}) {
	select {
	case <-finished:
		return
	default:
	}
	os.WriteFile(gate, nil, 0o600)
	select {
	case <-finished:
		return
	case <-time.After(time.Second):
	}
	if n, ok := pidIn(pid); ok {
		if p, err := os.FindProcess(n); err == nil {
			p.Kill()
			p.Release()
		}
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Error("the dispatcher did not return after its Runtime was ended")
	}
	if alive(t, pid) {
		t.Error("the test's Runtime is still running after its cleanup")
	}
}

// pidIn is the process id the file pid holds, if it holds one.
func pidIn(pid string) (int, bool) {
	b, err := os.ReadFile(pid)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return n, err == nil && n > 0
}

// waitFor waits up to ten seconds for ok.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
	}
}

// alive says whether the process named in the file pid still exists, not
// yet reaped included.
func alive(t *testing.T, pid string) bool {
	n, ok := pidIn(pid)
	if !ok {
		return false
	}
	p, err := os.FindProcess(n)
	if err != nil {
		return false
	}
	defer p.Release()
	return p.Signal(syscall.Signal(0)) == nil
}

// underLeasing puts l between w's Service and its store: inside the twice
// mode when the store is under it, so that the mode still names the
// function that opened each transaction.
func underLeasing(w *coverWorld, l *leasingWrapper) {
	if k, ok := w.s.store.(*twiceStore); ok {
		l.runStore = k.runStore
		w.s.store = &twiceStore{runStore: l, record: k.record}
		return
	}
	l.runStore = w.s.store
	w.s.store = l
}

// D3: on a kind that leases claims, the dispatcher renews its claim while the
// attempt goes on, holding the claim it made, and finishes the run under it.
func TestALeasingKindsClaimIsRenewedWhileItsAttemptGoesOn(t *testing.T) {
	shortClaimTimes(t, 40*time.Millisecond, 80*time.Millisecond)
	w := newCoverWorld(t)
	dir := t.TempDir()
	gate, pid := filepath.Join(dir, "gate"), filepath.Join(dir, "pid")
	l := &leasingWrapper{leasing: true}
	id, done := leasingRun(t, w, l, gate, pid)
	waitFor(t, "three renewals", func() bool { return l.renewals.Load() >= 3 })
	must(t, os.WriteFile(gate, nil, 0o600))
	if err := <-done; err != nil {
		t.Fatal("a renewed claim did not finish its run:", err)
	}
	got, err := w.s.run(id)
	must(t, err)
	if got.State != "failed" || got.FinishedAt == "" {
		t.Fatal("the run was not finished under its renewed claim:", got.State)
	}
	// No renewal is made after the attempt ends.
	after := l.renewals.Load()
	time.Sleep(120 * time.Millisecond)
	if l.renewals.Load() != after {
		t.Fatal("a renewal was made after the claim was released")
	}
}

// A kind that leases nothing (the sqlite kind, its lock the fence) renews
// nothing, however long the attempt.
func TestAKindThatLeasesNothingRenewsNothing(t *testing.T) {
	shortClaimTimes(t, 20*time.Millisecond, 40*time.Millisecond)
	w := newCoverWorld(t)
	dir := t.TempDir()
	gate, pid := filepath.Join(dir, "gate"), filepath.Join(dir, "pid")
	l := &leasingWrapper{leasing: false}
	_, done := leasingRun(t, w, l, gate, pid)
	waitFor(t, "the Runtime's start", func() bool { return alive(t, pid) })
	time.Sleep(150 * time.Millisecond)
	must(t, os.WriteFile(gate, nil, 0o600))
	must(t, <-done)
	if n := l.renewals.Load(); n != 0 {
		t.Fatal("a kind that leases nothing renewed its claim", n, "times")
	}
}

// A claimant whose renewal fails stops the attempt's Runtime, writes nothing
// more of the run (no finish, no entry), and stops its dispatcher with
// errClaimLost: the run is left running for the interruption of its lapsed
// lease.
func TestAClaimantWhoseRenewalFailsStopsItsRuntimeAndWritesNothingMore(t *testing.T) {
	shortClaimTimes(t, 40*time.Millisecond, 10*time.Second)
	w := newCoverWorld(t)
	dir := t.TempDir()
	gate, pid := filepath.Join(dir, "gate"), filepath.Join(dir, "pid")
	l := &leasingWrapper{leasing: true}
	id, done := leasingRun(t, w, l, gate, pid)
	waitFor(t, "the Runtime's start", func() bool { return alive(t, pid) })
	entries := len(journalEntries(t, w.cfg))
	l.fail.Store(true)
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a failed renewal did not stop the attempt")
	}
	if !errors.Is(err, errClaimLost) {
		t.Fatal("a failed renewal is not a lost claim:", err)
	}
	// The claimant reports its lost claim only once its Runtime is gone:
	// ended and reaped, not only signalled. A process not yet reaped still
	// answers alive, so this is checked at once, with no wait.
	if alive(t, pid) {
		t.Fatal("the Runtime of a lost claim is still running when its claimant reports the loss")
	}
	got, e := w.s.run(id)
	must(t, e)
	if got.State != "running" || got.FinishedAt != "" {
		t.Fatal("the claimant of a lost claim wrote its run:", got.State)
	}
	if n := len(journalEntries(t, w.cfg)); n != entries {
		t.Fatal("the claimant of a lost claim journaled", n-entries, "entries")
	}
}

// The margin: when no renewal succeeds before claimTimes.stop after the last
// that did, the Runtime is stopped while the renewal is still in flight; the
// release waits for it, and the claimant writes nothing more of the run.
func TestAClaimantStopsItsRuntimeWhenNoRenewalSucceedsInTime(t *testing.T) {
	shortClaimTimes(t, 40*time.Millisecond, 200*time.Millisecond)
	w := newCoverWorld(t)
	dir := t.TempDir()
	gate, pid := filepath.Join(dir, "gate"), filepath.Join(dir, "pid")
	l := &leasingWrapper{leasing: true, holdFrom: 2, hold: make(chan struct{})}
	id, done := leasingRun(t, w, l, gate, pid)
	waitFor(t, "the Runtime's start", func() bool { return alive(t, pid) })
	waitFor(t, "the Runtime's stop while the renewal hangs", func() bool { return !alive(t, pid) })
	select {
	case err := <-done:
		t.Fatal("the release did not wait for the renewal in flight:", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(l.hold)
	if err := <-done; !errors.Is(err, errClaimLost) {
		t.Fatal("a renewal that never came in time is not a lost claim:", err)
	}
	got, e := w.s.run(id)
	must(t, e)
	if got.State != "running" {
		t.Fatal("the claimant of a lost claim wrote its run:", got.State)
	}
}

// The dispatcher's tick, on a kind that leases claims, interrupts a run held
// running whose lease ran out (on the sqlite kind, under the wrapper, every
// run held running): the run reads interrupted, and its entry names when it
// was last known running and names no restart; only a restart's entry names
// one.
func TestTheTickInterruptsALapsedRunAndNamesNoRestart(t *testing.T) {
	w := newCoverWorld(t)
	queued, _, err := w.s.submit(w.job.ID, "lapsed", journalInput())
	must(t, err)
	running := queued
	running.State, running.StartedAt = "running", now()
	must(t, w.s.store.update(func(tx runTx) error {
		_, err := tx.putRun(running, runExpected{state: "queued"})
		return err
	}))
	underLeasing(w, &leasingWrapper{leasing: true})
	before := len(journalEntries(t, w.cfg))
	must(t, w.s.interruptLapsed())
	got, err := w.s.run(queued.ID)
	must(t, err)
	if got.State != "interrupted" {
		t.Fatal("the tick left a lapsed run:", got.State)
	}
	entries := journalEntries(t, w.cfg)
	if len(entries) != before+1 {
		t.Fatal("the tick wrote", len(entries)-before, "entries")
	}
	if i := entries[len(entries)-1].Interruption; entries[len(entries)-1].Kind != "run.interrupted" || i == nil || i.LastKnownRunning != running.StartedAt || i.Restart != 0 {
		t.Fatalf("the tick's interruption entry: %+v", entries[len(entries)-1])
	}
}
