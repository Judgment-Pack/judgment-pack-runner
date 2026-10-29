//go:build linux || darwin || freebsd

package runner

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// The selected-profile namespace is served by the Service that holds the
// executor lease: admitting and preparing under it neither releases nor
// takes a lease, and a restart takes the one lease again and finds the
// namespace and the full ordinary registry as they were.
func TestSelectedProfilesExecutorLeaseContinuity(t *testing.T) {
	f := executorFixture(t)
	cfg := executorOwnedConfig(f, os.Args[0])
	s, err := Open(cfg)
	executorMust(t, err)
	defer func() { s.Close() }()
	lease := s.executor
	if lease == nil {
		t.Fatal("the owned service holds no lease")
	}
	_, ordinary, _, _ := v2Fixture(t)
	fillOrdinary(t, s, ordinary)
	p := selectedProducer(t)
	if w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(p.profile))); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	plan := func(when string) {
		t.Helper()
		w := serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(p.input))
		var planned InputPlan
		_ = json.Unmarshal(w.Body.Bytes(), &planned)
		if w.Code != 200 || planned.Input == nil {
			t.Fatal(when, w.Code, w.Body.String())
		}
	}
	plan("before restart")
	if s.executor != lease {
		t.Fatal("the route replaced the service's lease")
	}
	if held, err := f.acquire(); err == nil {
		held.Close()
		t.Fatal("the lease was released while the route served")
	}
	s.Close()
	// Closed, the service released its one lease: there was no other owner.
	held, err := f.acquire()
	executorMust(t, err)
	held.Close()
	s, err = Open(cfg)
	executorMust(t, err)
	if held, err := f.acquire(); err == nil {
		held.Close()
		t.Fatal("the restarted service holds no lease")
	}
	if len(s.inputProfiles()) != 16 || len(s.selectedInputProfiles()) != 1 || profileHash(s.selectedInputProfiles()[0]) != profileHash(p.profile) {
		t.Fatal("restart lost a profile:", len(s.inputProfiles()), len(s.selectedInputProfiles()))
	}
	plan("after restart")
	if held, err := f.acquire(); err == nil {
		held.Close()
		t.Fatal("the lease was released while the route served after restart")
	}
}
