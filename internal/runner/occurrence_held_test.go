package runner

// An occurrence whose own record cannot be dispatched, under a Runner's
// claim (#282): it is failed once, journaled, and the dispatch returns no
// error, so that no Runner's automation stops on it, then or one lease
// later. An error of the store's under the claim is returned with the claim
// released, the occurrence left for the next claimant.

import (
	"errors"
	"testing"
	"time"
)

// noInputFailedOnce dispatches the accepted occurrence id, which holds no
// input, on first, and ticks second's automation after: the dispatch fails
// the occurrence once, with its reason journaled, and returns no error, and
// the tick finds nothing to dispatch and stays healthy.
func noInputFailedOnce(t *testing.T, first, second *Service, id string, at time.Time) {
	t.Helper()
	failedOnce(t, first, second, id, at, "The accepted occurrence holds no input.")
}

// failedOnce is noInputFailedOnce for an occurrence failed with reason.
func failedOnce(t *testing.T, first, second *Service, id string, at time.Time, reason string) {
	t.Helper()
	if err := first.dispatchOccurrence(storedOccurrence(t, first, id), at); err != nil {
		t.Fatal("the dispatch returned an error, which stops the Runner's automation:", err)
	}
	failed := storedOccurrence(t, second, id)
	if failed.State != "failed" || failed.Reason != reason || failed.ErrorClass != classInternal || failed.RunID != "" {
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

// acceptedOnDefaultKind is the accepted occurrence a schedule trigger of a
// job of one Runner on the default kind admits, and its due time.
func acceptedOnDefaultKind(t *testing.T) (*Service, Occurrence, time.Time) {
	t.Helper()
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
	return s, o, due
}

// One Runner, on the default kind: the dispatch fails the occurrence, and
// the same Runner's next tick goes on.
func TestAnAcceptedOccurrenceWithNoInputIsFailedAndTheAutomationGoesOn(t *testing.T) {
	s, o, due := acceptedOnDefaultKind(t)
	o.Input, o.InputDigest = nil, ""
	must(t, s.saveOccurrence(o))
	noInputFailedOnce(t, s, s, o.ID, due)
}

// So for an accepted occurrence whose expiry does not read as a time.
func TestAnAcceptedOccurrenceWithNoReadableExpiryIsFailedAndTheAutomationGoesOn(t *testing.T) {
	s, o, due := acceptedOnDefaultKind(t)
	o.ExpiresAt = "not a time"
	must(t, s.saveOccurrence(o))
	failedOnce(t, s, s, o.ID, due, "The accepted occurrence holds no readable expiry time.")
}

// So for an occurrence whose job's record, or its release's, does not read
// as one: every Runner's read of it meets the same error, so it is failed
// naming the record, rather than released to be claimed again.
func TestAnOccurrenceWhoseJobOrReleaseIsUnreadableIsFailedAndTheAutomationGoesOn(t *testing.T) {
	for _, table := range []string{"jobs", "releases"} {
		t.Run(table, func(t *testing.T) {
			s, o, due := acceptedOnDefaultKind(t)
			id := o.JobID
			if table == "releases" {
				id = o.ReleaseID
			}
			if _, err := sqliteDB(s).Exec("UPDATE "+table+" SET record='[]' WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
			record := map[string]string{"jobs": "job", "releases": "release"}[table]
			failedOnce(t, s, s, o.ID, due, "The occurrence's "+record+" record cannot be read.")
		})
	}
}

// unansweredJob is a store whose read of a job is not answered, as a store
// that does not answer: the error is the store's, not the occurrence's.
type unansweredJob struct{ runStore }

func (u unansweredJob) leasesClaims() bool { return leases(u.runStore) }

func (u unansweredJob) updateWithin(d time.Duration, fn func(runTx) error) error {
	return updateWithin(u.runStore, d, fn)
}

func (u unansweredJob) job(string) (Job, error) {
	return Job{}, storeFailure(faultUnavailable, "job", errors.New("the store did not answer"))
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

// Two Runners on the postgresql kind: the first's read of the occurrence's
// job is not answered, an error of the store's. Its dispatch returns the
// error with the claim released and the occurrence as it was read, accepted
// and claimable; the second claims and dispatches it into a run.
func TestPostgreSQLAStoreErrorUnderAClaimLeavesTheOccurrenceToTheNextClaimant(t *testing.T) {
	p := pgRunners(t)
	first, second := p.open(), p.open()
	o, due := p.admitted(first)
	held := first.store
	first.store = unansweredJob{held}
	err := first.dispatchOccurrence(o, due.Add(time.Second))
	first.store = held
	if faultOf(err) != faultUnavailable {
		t.Fatal("the dispatch did not return the store's error:", err)
	}
	var claimed int
	must(t, p.suite.QueryRowContext(pgContext, "SELECT count(*) FROM occurrences WHERE id=$1 AND claim_id IS NOT NULL", o.ID).Scan(&claimed))
	if claimed != 0 {
		t.Fatal("the occurrence's claim was kept")
	}
	if left := storedOccurrence(t, second, o.ID); left.State != "accepted" || left.Input == nil {
		t.Fatalf("the occurrence was not left as it was read: %+v", left)
	}
	if err := second.dispatchOccurrence(storedOccurrence(t, second, o.ID), due.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if done := storedOccurrence(t, second, o.ID); done.State != "submitted" || done.RunID == "" {
		t.Fatalf("the next claimant did not submit the occurrence: %+v", done)
	}
}

// Two Runners on the postgresql kind, the run queue full (#282, item 3):
// the first's dispatch finds no room, and releases the claim with the
// occurrence accepted as it was read, returning no error; once there is
// room, the second claims it and submits its run.
func TestPostgreSQLAFullQueueReleasesTheOccurrencesClaim(t *testing.T) {
	p := pgRunners(t)
	first, second := p.open(), p.open()
	o, due := p.admitted(first)
	var active int
	must(t, p.suite.QueryRowContext(pgContext, "SELECT value FROM counts WHERE name='active-runs'").Scan(&active))
	_, err := p.suite.ExecContext(pgContext, "UPDATE counts SET value=$1 WHERE name='active-runs'", queueLimit)
	must(t, err)
	if err := first.dispatchOccurrence(o, due.Add(time.Second)); err != nil {
		t.Fatal("the dispatch with the queue full returned an error:", err)
	}
	var claimed int
	must(t, p.suite.QueryRowContext(pgContext, "SELECT count(*) FROM occurrences WHERE id=$1 AND claim_id IS NOT NULL", o.ID).Scan(&claimed))
	if claimed != 0 {
		t.Fatal("the occurrence's claim was kept with the queue full")
	}
	if left := storedOccurrence(t, second, o.ID); left.State != "accepted" || left.Input == nil || left.RunID != "" {
		t.Fatalf("the occurrence was not left as it was read: %+v", left)
	}
	_, err = p.suite.ExecContext(pgContext, "UPDATE counts SET value=$1 WHERE name='active-runs'", active)
	must(t, err)
	if err := second.dispatchOccurrence(storedOccurrence(t, second, o.ID), due.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if done := storedOccurrence(t, second, o.ID); done.State != "submitted" || done.RunID == "" {
		t.Fatalf("the next claimant did not submit the occurrence: %+v", done)
	}
}
