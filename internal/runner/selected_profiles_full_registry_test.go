//go:build linux || darwin || freebsd

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// gatedRuntime is completingRuntime held until the file gate exists: a run
// it evaluates stays running in the executor until the test opens the gate.
func gatedRuntime(t *testing.T, dir, gate string) string {
	return fakeRuntime(t, dir, `while [ ! -e '`+gate+`' ]; do sleep 0.1; done
mkdir -p audit
printf '{"recordVersion":"1","kind":"evaluation","run":"fake","reviewed":true,"pack":{"digest":"%s"},"inputs":{"facts":%s,"evidenceSupplied":false},"disposition":`+journalDisposition+`}\n' '`+digest([]byte(journalPack))+`' "$(cat facts.json)" > audit/evaluations.jsonl
printf '%s\n' '{"outputVersion":"2","status":"evaluated","packId":"journal","packVersion":"1.0.0","evaluatorSpecVersion":"0.2.0-draft","rehearsal":false,"disposition":`+journalDisposition+`}'`)
}

// Runner #190, the host Judgment-Pack/judgment-pack-desk#599 describes. Its
// ordinary registry is full: 15 configured profiles and its installed
// ordinary generated-input profile. It still prepares inputs for an
// explicitly selected retained generated-input profile, in the selected
// namespace (#191), through the one Service that holds the executor lease.
// Nothing ordinary is evicted, paused or raised: ordinary planning, the
// installed profile's retained preparation, an ordinary run already in the
// executor and one queued behind it go on as they were. Each boundary refuses
// in its exact bytes, and a restart finds every profile and run as it was.
func TestSelectedProfilesBesideFifteenConfiguredAndTheInstalledProfile(t *testing.T) {
	f := executorFixture(t)
	cfg := executorOwnedConfig(f, os.Args[0])
	ordinaryInput, vendors, _, _ := v2Fixture(t)
	cfg.InputProfiles = fifteenConfigured(vendors, nil)
	s, err := Open(cfg)
	executorMust(t, err)
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	lease := s.executor
	held := func(when string) {
		t.Helper()
		if other, err := f.acquire(); err == nil {
			other.Close()
			t.Fatal(when + ": the executor lease is not held")
		}
	}

	installed := installedProducer(t)
	if w := serve(t, s, "POST", "/v1/input-profiles", string(encode(installed.profile)), nil); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	full := func(when string) {
		t.Helper()
		got := s.inputProfiles()
		if len(got) != 16 || got[0].ID != vendors.ID || got[15].ID != installed.profile.ID || len(s.cfg.InputProfiles) != 15 {
			t.Fatal(when+": the ordinary registry is not the 15 configured and the installed profile:", len(got))
		}
	}
	full("at the start")
	wantBytes(t, "a 17th ordinary profile", serve(t, s, "POST", "/v1/input-profiles", string(encode(selectedNamed(selectedProducer(t), "seventeenth"))), nil), 422, refusedOrdinaryLimit)

	// Ordinary work in progress: a run of an ordinary job is in the executor,
	// its Runtime held at a gate, and another run is queued behind it.
	gate := filepath.Join(t.TempDir(), "gate")
	release := journalRelease(t, s, gatedRuntime(t, cfg.Dir, gate))
	job, err := s.createJob("Ordinary intake", release.ID)
	executorMust(t, err)
	inFlight, _, err := s.submit(job.ID, "in-flight", journalInput())
	executorMust(t, err)
	queued, _, err := s.submit(job.ID, "queued", journalInput())
	executorMust(t, err)
	dispatched := make(chan error, 1)
	go func() { _, e := s.dispatchNext(context.Background()); dispatched <- e }()
	var finished error
	waited := false
	finish := func() error {
		if !waited {
			waited = true
			if e := os.WriteFile(gate, nil, 0600); e != nil {
				return e
			}
			finished = <-dispatched
		}
		return finished
	}
	defer finish()
	waitForRunState(t, s, inFlight.ID, "running")

	// The selected profile is admitted beside the full registry and prepares:
	// the next source to acquire, then the complete preparation.
	selected := selectedProducer(t)
	if w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selected.profile))); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	prepareSelected := func(when string) {
		t.Helper()
		partial := cloneInput(selected.input)
		partial.Source.Sources = nil
		var plan InputPlan
		w := serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(partial))
		_ = json.Unmarshal(w.Body.Bytes(), &plan)
		if w.Code != 200 || plan.Next == nil || !plan.Next.RetainedColumn || plan.Next.Profile != selected.profile.ID {
			t.Fatal(when+": selected planning:", w.Code, w.Body.String())
		}
		plan = InputPlan{}
		w = serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(selected.input))
		_ = json.Unmarshal(w.Body.Bytes(), &plan)
		if w.Code != 200 || plan.Input == nil || plan.Input.Preparation == nil {
			t.Fatal(when+": selected preparation:", w.Code, w.Body.String())
		}
	}
	// Ordinary preparations work as before: a configured profile's planning
	// and the installed profile's retained preparation, under the ordinary
	// profiles.
	prepareOrdinary := func(when string) {
		t.Helper()
		var plan InputPlan
		w := serve(t, s, "POST", "/v1/inputs/next", string(encode(ordinaryInput)), nil)
		_ = json.Unmarshal(w.Body.Bytes(), &plan)
		if w.Code != 200 || plan.Input == nil {
			t.Fatal(when+": ordinary planning:", w.Code, w.Body.String())
		}
		plan = InputPlan{}
		w = serveSelected(t, s, "POST", "/v1/inputs/next", selectedBody(installed.input))
		_ = json.Unmarshal(w.Body.Bytes(), &plan)
		if w.Code != 200 || plan.Input == nil || plan.Input.Preparation == nil {
			t.Fatal(when+": the installed profile's retained preparation:", w.Code, w.Body.String())
		}
	}
	prepareSelected("beside the full registry")
	prepareOrdinary("beside the selected profile")
	full("beside the selected profile")
	if r, e := s.run(inFlight.ID); e != nil || r.State != "running" {
		t.Fatal("the run in the executor did not run on:", r.State, e)
	}
	if s.executor != lease {
		t.Fatal("the namespace replaced the service's lease")
	}
	held("while the namespace served")

	// Each boundary refuses in its exact bytes. The installed profile's id
	// and a configured profile's are ordinary, and are never selected.
	wantBytes(t, "selecting the installed profile's id", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(selected, installed.profile.ID)))), 409, refusedOrdinaryID)
	wantBytes(t, "selecting a configured profile's id", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(selected, "configured7")))), 409, refusedOrdinaryID)
	wantBytes(t, "selected planning of the installed profile", serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(installed.input)), 422, refusedOrdinaryPlanning(installed.profile.ID))
	wantBytes(t, "ordinary retained planning of the selected profile", serveSelected(t, s, "POST", "/v1/inputs/next", selectedBody(selected.input)), 422, refusedSelectedBinding(selected.profile.ID))
	wantBytes(t, "an ordinary registration of the selected id", serve(t, s, "POST", "/v1/input-profiles", string(encode(selected.profile)), nil), 409, refusedSelectedID)
	plain := cloneInput(selected.input)
	plain.Source.ConversionContract = nil
	wantBytes(t, "an ordinary run of the selected profile", serve(t, s, "POST", "/v1/jobs/"+job.ID+"/runs", selectedBody(plain), map[string]string{"Idempotency-Key": "selected"}), 422, refusedSelectedBinding(selected.profile.ID))
	var listing []struct {
		Profile InputProfile `json:"profile"`
	}
	_ = json.Unmarshal(serve(t, s, "GET", "/v1/input-profiles", "", nil).Body.Bytes(), &listing)
	if len(listing) != 16 {
		t.Fatal("the ordinary listing changed:", len(listing))
	}
	for _, p := range listing {
		if p.Profile.ID == selected.profile.ID {
			t.Fatal("the ordinary listing holds the selected profile")
		}
	}

	// The run in the executor completes, and the run queued behind it runs
	// after it: no ordinary work was paused, dropped or added.
	executorMust(t, finish())
	if r := waitForRunState(t, s, inFlight.ID, "completed"); len(r.Audit) == 0 {
		t.Fatal("the run in the executor completed without its audit")
	}
	if worked, e := s.dispatchNext(context.Background()); e != nil || !worked {
		t.Fatal("the queued run was not dispatched:", worked, e)
	}
	waitForRunState(t, s, queued.ID, "completed")
	if n := countRows(t, s, "runs"); n != 2 {
		t.Fatal("ordinary runs were added or dropped:", n)
	}

	// The namespace's own ceiling: 15 more fill it to 16, and one past it is
	// refused, evicting nothing on either side.
	for i := 1; i < selectedProfileBound; i++ {
		if w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(selected, fmt.Sprintf("selected%d", i))))); w.Code != http.StatusCreated {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	wantBytes(t, "one past the selected bound", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(selected, "overflow")))), 422, refusedSelectedLimit)
	if len(s.selectedInputProfiles()) != selectedProfileBound {
		t.Fatal("selected profiles were evicted or added:", len(s.selectedInputProfiles()))
	}
	full("beside a full namespace")

	// A restart: Close releases the one lease, there being no other owner,
	// and Open takes it again and finds every profile and run as it was.
	s.Close()
	other, err := f.acquire()
	executorMust(t, err)
	other.Close()
	s, err = Open(cfg)
	executorMust(t, err)
	held("after the restart")
	full("after the restart")
	if len(s.selectedInputProfiles()) != selectedProfileBound || profileHash(s.selectedInputProfiles()[0]) != profileHash(selected.profile) {
		t.Fatal("the restart changed the namespace:", len(s.selectedInputProfiles()))
	}
	for _, id := range []string{inFlight.ID, queued.ID} {
		if r, e := s.run(id); e != nil || r.State != "completed" {
			t.Fatal("the restart changed an ordinary run:", id, r.State, e)
		}
	}
	prepareSelected("after the restart")
	prepareOrdinary("after the restart")
}
