package runner

// The installation's profile rows written by several Runners of one store
// (#282): a registration is applied to the row as its transaction reads it,
// not to the Runner's copy, so that a Runner started before another
// registered keeps the other's profiles in the store; and a Runner whose copy
// lacks a profile reads the rows again (refreshProfiles).

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// rowIDs are the ids of the profiles the store's row under key holds, in its
// order, as sv's store reads it.
func rowIDs(t *testing.T, sv *Service, key string) []string {
	t.Helper()
	raw, err := sv.store.profileRow(key)
	if absent(err) {
		return nil
	}
	must(t, err)
	var profiles []InputProfile
	must(t, json.Unmarshal([]byte(raw), &profiles))
	ids := []string{}
	for _, p := range profiles {
		ids = append(ids, p.ID)
	}
	return ids
}

func profileIDs(profiles []InputProfile) []string {
	ids := []string{}
	for _, p := range profiles {
		ids = append(ids, p.ID)
	}
	return ids
}

// A registration whose row another writer, as another Runner of the store
// would, wrote between the two runs of its transaction keeps that writer's
// profile, and this Runner holds both from then on; so does a selected
// profile's admission. The default kind runs under the twice mode, its one
// Runner standing for both.
func TestARegistrationKeepsAProfileWrittenSinceTheRunnerReadTheRow(t *testing.T) {
	record := twiceMode(t)
	t.Run("an ordinary profile", func(t *testing.T) {
		s := openJournal(t, journalConfig(t))
		defer s.Close()
		mine, _ := testProfile(t)
		theirs := mine
		theirs.ID = "registered-elsewhere"
		record.betweenRuns(t, "(*Service).registerInputProfile", func(st runStore) error {
			return st.update(func(tx runTx) error {
				return tx.putProfileRow(registeredProfilesKey, storedRow([]json.RawMessage{encode(theirs)}))
			})
		})
		if created, err := s.registerInputProfile(mine); err != nil || !created {
			t.Fatal("the registration:", created, err)
		}
		if got := rowIDs(t, s, registeredProfilesKey); !slices.Equal(got, []string{theirs.ID, mine.ID}) {
			t.Fatal("the row after the registration holds", got)
		}
		if got := profileIDs(s.inputProfiles()); !slices.Equal(got, []string{theirs.ID, mine.ID}) {
			t.Fatal("the Runner holds", got)
		}
	})
	t.Run("a selected profile", func(t *testing.T) {
		s := openJournal(t, journalConfig(t))
		defer s.Close()
		f := selectedProducer(t)
		mine, theirs := selectedNamed(f, "selected-here"), selectedNamed(f, "selected-elsewhere")
		record.betweenRuns(t, "(*Service).admitSelectedProfile", func(st runStore) error {
			return st.update(func(tx runTx) error {
				return tx.putProfileRow(selectedProfilesKey, storedRow([]json.RawMessage{encode(theirs)}))
			})
		})
		if created, err := s.admitSelectedProfile(mine); err != nil || !created {
			t.Fatal("the admission:", created, err)
		}
		if got := rowIDs(t, s, selectedProfilesKey); !slices.Equal(got, []string{theirs.ID, mine.ID}) {
			t.Fatal("the row after the admission holds", got)
		}
		if got := profileIDs(s.selectedInputProfiles()); !slices.Equal(got, []string{theirs.ID, mine.ID}) {
			t.Fatal("the Runner holds", got)
		}
	})
}

// #282 on the postgresql kind. Runner a opens the store, then Runner b
// registers; a's registration after b's keeps b's profile in the store, and
// a holds both from then on, while b, whose copy lacks a's, holds it once it
// reads the rows again. So for the selected namespace; an id one namespace
// took through b is refused to the other through a; a definition b
// registered is immutable through a. Registrations through both at once all
// land.
func TestPostgreSQLARunnerStartedEarlierKeepsAnotherRunnersProfiles(t *testing.T) {
	p := pgRunners(t)
	a, b := p.open(), p.open()
	ordinary, _ := testProfile(t)
	named := func(id string) InputProfile {
		q := ordinary
		q.ID = id
		return q
	}
	register := func(sv *Service, q InputProfile) {
		t.Helper()
		if created, err := sv.registerInputProfile(q); err != nil || !created {
			t.Fatal("the registration of", q.ID, created, err)
		}
	}
	register(b, named("on-b"))
	register(a, named("on-a"))
	if got := rowIDs(t, a, registeredProfilesKey); !slices.Equal(got, []string{"on-b", "on-a"}) {
		t.Fatal("the row after both registrations holds", got)
	}
	if got := profileIDs(a.inputProfiles()); !slices.Equal(got, []string{"on-b", "on-a"}) {
		t.Fatal("the Runner that registered last holds", got)
	}
	if got := profileIDs(b.inputProfiles()); !slices.Equal(got, []string{"on-b"}) {
		t.Fatal("the other Runner's copy, before it reads the rows again, holds", got)
	}
	if !b.refreshProfiles() || b.refreshProfiles() {
		t.Fatal("a refresh did not report the profile gained once")
	}
	if got := profileIDs(b.inputProfiles()); !slices.Equal(got, []string{"on-b", "on-a"}) {
		t.Fatal("the other Runner, having read the rows again, holds", got)
	}

	f := selectedProducer(t)
	for _, admit := range []struct {
		sv *Service
		id string
	}{{b, "selected-on-b"}, {a, "selected-on-a"}} {
		if created, err := admit.sv.admitSelectedProfile(selectedNamed(f, admit.id)); err != nil || !created {
			t.Fatal("the admission of", admit.id, created, err)
		}
	}
	if got := rowIDs(t, a, selectedProfilesKey); !slices.Equal(got, []string{"selected-on-b", "selected-on-a"}) {
		t.Fatal("the selected row after both admissions holds", got)
	}

	// Each namespace's id, taken through b, is refused to the other through a,
	// whose copy has not been refreshed since.
	if _, err := b.admitSelectedProfile(selectedNamed(f, "taken-on-b")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.registerInputProfile(named("taken-on-b")); refusedCode(err) != codeInputProfileSelected {
		t.Fatal("an ordinary id a selected profile took through another Runner:", err)
	}
	register(b, named("ordinary-on-b"))
	if _, err := a.admitSelectedProfile(selectedNamed(f, "ordinary-on-b")); refusedCode(err) != codeSelectedOrdinary {
		t.Fatal("a selected id an ordinary profile took through another Runner:", err)
	}
	register(b, named("defined-on-b"))
	changed := named("defined-on-b")
	changed.Tools = []string{"lookup"}
	if _, err := a.registerInputProfile(changed); refusedCode(err) != "input_profile_conflict" {
		t.Fatal("another definition under an id registered through another Runner:", err)
	}
	if created, err := a.registerInputProfile(named("defined-on-b")); err != nil || created {
		t.Fatal("the same definition through the other Runner:", created, err)
	}

	// Registrations through both Runners at once.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		sv := a
		if i%2 == 1 {
			sv = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sv.registerInputProfile(named(fmt.Sprintf("together%d", i))); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal("a registration made at once with another:", err)
	}
	held := rowIDs(t, a, registeredProfilesKey)
	for i := range 8 {
		if !slices.Contains(held, fmt.Sprintf("together%d", i)) {
			t.Fatalf("together%d is not in the row: %v", i, held)
		}
	}
	if len(held) != 12 {
		t.Fatal("the row holds", held)
	}
	// A Runner started now reads every profile either registered.
	c := p.open()
	if got := len(c.inputProfiles()); got != 12 {
		t.Fatal("a Runner started after the registrations holds", got)
	}
	if got := profileIDs(c.selectedInputProfiles()); !slices.Equal(got, []string{"selected-on-b", "selected-on-a", "taken-on-b"}) {
		t.Fatal("a Runner started after the admissions holds", got)
	}
}
