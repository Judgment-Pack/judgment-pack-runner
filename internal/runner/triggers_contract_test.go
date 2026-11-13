package runner

// The checks the Runner makes on triggers and occurrences it reads through
// the store contract (issue #244): each refuses, or passes by, what it did
// before the contract stood in front of the store.

import (
	"context"
	"testing"
	"time"
)

func TestATriggerIsConfiguredOnlyThroughItsOwnJob(t *testing.T) {
	s, _, j, _ := automaticFixture(t)
	other, e := s.createJob("Other", testRelease(t, s).ID)
	if e != nil {
		t.Fatal(e)
	}
	if other.ID == j.ID {
		t.Fatal("the fixture made one job")
	}
	c := constantTrigger(time.Now())
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	if e != nil {
		t.Fatal(e)
	}
	c.Name = "Through another job"
	if _, e = s.configureTrigger(other.ID, tr.ID, tr.Revision, c); !absent(e) {
		t.Fatalf("a trigger configured through another job: %v", e)
	}
	if held, _, e := s.trigger(tr.ID); e != nil || held.Revision != tr.Revision || held.Config.Name != "Every minute" || held.JobID != j.ID {
		t.Fatalf("the trigger after the refusal: %+v, %v", held, e)
	}
}

func TestALegacyPreparationPassesByAnOccurrenceNoLongerAccepted(t *testing.T) {
	s, _, j, _ := automaticFixture(t)
	tr, e := s.configureTrigger(j.ID, "", 0, constantTrigger(time.Now()))
	if e != nil {
		t.Fatal(e)
	}
	// Its queue time has passed: read as accepted, it would be expired.
	o := newOccurrence(tr, j, time.Now().Add(-2*time.Hour))
	o.State = "submitted"
	must(t, s.store.update(func(tx runTx) error { return s.admitOccurrence(tx, tr, &o, "submitted-first", "") }))
	if e = s.prepareLegacyOccurrence(context.Background(), o.ID); e != nil {
		t.Fatal(e)
	}
	if held, e := s.occurrence(o.ID); e != nil || held.State != "submitted" || held.Reason != "" {
		t.Fatalf("a submitted occurrence after a legacy preparation: %+v, %v", held, e)
	}
	if e = s.prepareLegacyOccurrence(context.Background(), "occ_none"); e != nil {
		t.Fatalf("a legacy preparation of an occurrence not held: %v", e)
	}
}
