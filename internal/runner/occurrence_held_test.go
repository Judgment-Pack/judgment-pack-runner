package runner

// An occurrence whose own record cannot be dispatched, under a Runner's
// claim (#282): it is failed once, journaled, and the dispatch returns no
// error, so that no Runner's automation stops on it, then or one lease
// later.

import (
	"testing"
	"time"
)

// noInputFailedOnce dispatches the accepted occurrence id, which holds no
// input, on first, and ticks second's automation after: the dispatch fails
// the occurrence once, with its reason journaled, and returns no error, and
// the tick finds nothing to dispatch and stays healthy.
func noInputFailedOnce(t *testing.T, first, second *Service, id string, at time.Time) {
	t.Helper()
	if err := first.dispatchOccurrence(storedOccurrence(t, first, id), at); err != nil {
		t.Fatal("the dispatch returned an error, which stops the Runner's automation:", err)
	}
	failed := storedOccurrence(t, second, id)
	if failed.State != "failed" || failed.Reason != "The accepted occurrence holds no input." || failed.ErrorClass != classInternal || failed.RunID != "" {
		t.Fatalf("the occurrence was not failed with its reason: %+v", failed)
	}
	if n := entriesOfKind(t, second.store, "occurrence.failed", id); n != 1 {
		t.Fatal("the failure was journaled", n, "times")
	}
	if err := second.automationTick(at); err != nil || second.unhealthy.Load() {
		t.Fatal("the next tick met the occurrence again:", err)
	}
	if again := storedOccurrence(t, second, id); string(encode(again)) != string(encode(failed)) {
		t.Fatalf("the next tick changed the failed occurrence: %+v", again)
	}
	if n := entriesOfKind(t, second.store, "occurrence.failed", id); n != 1 {
		t.Fatal("the failure was journaled", n, "times after the next tick")
	}
}

// One Runner, on the default kind: the dispatch fails the occurrence, and
// the same Runner's next tick goes on.
func TestAnAcceptedOccurrenceWithNoInputIsFailedAndTheAutomationGoesOn(t *testing.T) {
	s, _, j, _ := automaticFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	tr, err := s.configureTrigger(j.ID, "", 0, constantTrigger(at))
	must(t, err)
	_, _, err = s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(t, err)
	held, hash := storedTrigger(t, s, tr.ID)
	due, err := time.Parse(time.RFC3339Nano, held.NextAt)
	must(t, err)
	must(t, s.scheduleTick(held, hash, due))
	o := latestOccurrence(t, s)
	if o.State != "accepted" || o.Input == nil {
		t.Fatalf("the tick admitted %+v, not an accepted occurrence", o)
	}
	o.Input, o.InputDigest = nil, ""
	must(t, s.saveOccurrence(o))
	noInputFailedOnce(t, s, s, o.ID, due)
}

// Two Runners on the postgresql kind: the first fails the occurrence under
// its claim, which it does not keep, and the second's tick finds nothing to
// do and stays healthy.
func TestPostgreSQLAnOccurrenceWithNoInputIsFailedOnceAndTheOtherRunnerGoesOn(t *testing.T) {
	p := pgRunners(t)
	first, second := p.open(), p.open()
	o, due := p.admitted(first)
	o.Input, o.InputDigest = nil, ""
	must(t, first.saveOccurrence(o))
	noInputFailedOnce(t, first, second, o.ID, due.Add(time.Second))
	var claimed int
	must(t, p.suite.QueryRowContext(pgContext, "SELECT count(*) FROM occurrences WHERE id=$1 AND claim_id IS NOT NULL", o.ID).Scan(&claimed))
	if claimed != 0 {
		t.Fatal("the failed occurrence's claim was kept")
	}
}
