package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegisteredProfilesPersistWithoutRestartingOrMutatingPins(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	p, _ := testProfile(t)
	raw := string(encode(p))
	response := serve(t, s, "POST", "/v1/input-profiles", raw, nil)
	if response.Code != http.StatusCreated {
		t.Fatal(response.Code, response.Body.String())
	}
	var registered any
	if err := json.Unmarshal(response.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	doc := readOpenAPI(t)
	if problems := doc.check("registration", doc.schema("InstalledInputProfile"), registered); len(problems) != 0 {
		t.Fatal(problems)
	}
	if response = serve(t, s, "POST", "/v1/input-profiles", raw, nil); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	// Both the caller's profile and a snapshot can be edited without altering the
	// installed endpoint, tool allowlist, or digest.
	original := profileHash(p)
	p.Tools[0] = "write"
	*p.Endpoint = "changed"
	snapshot := s.inputProfiles()
	snapshot[0].Tools[0] = "delete"
	*snapshot[0].Endpoint = "another"
	if got := s.inputProfiles(); len(got) != 1 || profileHash(got[0]) != original {
		t.Fatal("profile aliases caller state", got)
	}
	if response = serve(t, s, "POST", "/v1/input-profiles", string(encode(p)), nil); response.Code != http.StatusConflict {
		t.Fatal(response.Code, response.Body.String())
	}
	s.Close()
	s = openJournal(t, cfg)
	if got := s.inputProfiles(); len(got) != 1 || profileHash(got[0]) != original {
		t.Fatal("registration lost across restart", got)
	}
	// Explicit bootstrap settings cannot silently override registered authority.
	s.Close()
	cfg.InputProfiles = []InputProfile{p}
	if other, err := Open(cfg); err == nil {
		other.Close()
		t.Fatal("bootstrap replaced retained profile")
	}
}

func TestRegisteredProfilesHTTPRefusalsKeepAuthorityUnchanged(t *testing.T) {
	s := openJournal(t, journalConfig(t))
	defer s.Close()
	p, _ := testProfile(t)
	raw := string(encode(p))
	cases := []struct {
		name, body string
		headers    map[string]string
		status     int
	}{
		{"no authentication", raw, map[string]string{"Authorization": ""}, 401},
		{"browser origin", raw, map[string]string{"Origin": "http://localhost"}, 403},
		{"case alias", strings.Replace(raw, `"class"`, `"Class"`, 1), nil, 400},
		{"duplicate", strings.Replace(raw, `"id":`, `"id":"other","id":`, 1), nil, 400},
		{"null tools", strings.Replace(raw, `["execute_sql","lookup"]`, `null`, 1), nil, 400},
		{"missing endpoint", strings.Replace(raw, `"endpoint":"`+*p.Endpoint+`",`, "", 1), nil, 422},
		{"null root", "null", nil, 400},
		{"size", strings.Repeat(" ", 32<<10) + raw, nil, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := serve(t, s, "POST", "/v1/input-profiles", tc.body, tc.headers)
			if r.Code != tc.status {
				t.Fatal(r.Code, r.Body.String())
			}
			if len(s.inputProfiles()) != 0 {
				t.Fatal("refusal installed authority")
			}
		})
	}
}

func TestRegisteredProfilesConcurrentLimitAndIdempotency(t *testing.T) {
	s := openJournal(t, journalConfig(t))
	defer s.Close()
	p, _ := testProfile(t)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.registerInputProfile(p)
			if err != nil {
				errs <- err
			}
			_ = s.inputProfiles()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(s.inputProfiles()) != 1 {
		t.Fatal("duplicate registration")
	}
	for i := 1; i < 16; i++ {
		next := p
		next.ID = fmt.Sprintf("profile%d", i)
		if _, err := s.registerInputProfile(next); err != nil {
			t.Fatal(err)
		}
	}
	p.ID = "overflow"
	if _, err := s.registerInputProfile(p); err == nil {
		t.Fatal("profile bound not enforced")
	}
	if len(s.inputProfiles()) != 16 {
		t.Fatal("existing profiles were evicted")
	}
}

func TestRegisteredProfileAvailableToPreparationAndRetainedRelease(t *testing.T) {
	cfg := testConfig(t)
	input, p, key, at := v2Fixture(t)
	input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
	input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}}}}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), encode(sample()), at, 0)}
	s := openJournal(t, cfg)
	defer s.Close()
	if _, err := s.normalizeInput(input); err == nil {
		t.Fatal("uninstalled profile accepted")
	}
	if _, err := s.registerInputProfile(p); err != nil {
		t.Fatal(err)
	}
	r := serve(t, s, "POST", "/v1/inputs/next", string(encode(input)), nil)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	packBytes, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	pack := string(packBytes)
	release, err := s.preview(context.Background(), PreviewRequest{Pack: pack, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if len(release.InputProfiles) != 1 || profileHash(release.InputProfiles[0]) != profileHash(p) {
		t.Fatal("release did not retain registered profile")
	}
	another := p
	another.ID = "new-version"
	if _, err = s.registerInputProfile(another); err != nil {
		t.Fatal(err)
	}
	held, err := s.release(release.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(encode(held.InputProfiles)) != string(encode(release.InputProfiles)) {
		t.Fatal("registration changed existing release")
	}
	prepared, err := s.normalizeInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = normalizeV2(prepared, held.InputProfiles, time.Now().UTC().Truncate(time.Second)); err != nil {
		t.Fatal("retained profile no longer verifies", err)
	}
	var listed []struct {
		Profile InputProfile `json:"profile"`
	}
	r = serve(t, s, "GET", "/v1/input-profiles", "", nil)
	if json.Unmarshal(r.Body.Bytes(), &listed) != nil || len(listed) != 2 {
		t.Fatal("profile listing stale", r.Body.String())
	}
}

func TestRegisteredProfilesCannotBlockChecksInTransaction(t *testing.T) {
	s := openJournal(t, journalConfig(t))
	defer s.Close()
	p, _ := testProfile(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waitingBefore := s.db.Stats().WaitCount
	done := make(chan error, 1)
	go func() { _, err := s.registerInputProfile(p); done <- err }()
	// The registration is waiting for the sole connection held by the upgrade.
	deadline := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == waitingBefore && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.db.Stats().WaitCount == waitingBefore {
		t.Fatal("registration did not reach the blocked write")
	}
	checked := make(chan []InputProfile, 1)
	go func() { checked <- s.inputProfiles() }()
	select {
	case got := <-checked:
		if len(got) != 0 {
			t.Fatal("uncommitted profile exposed")
		}
	case <-time.After(time.Second):
		tx.Rollback()
		<-done
		t.Fatal("profile reads deadlock an in-progress job transaction")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if len(s.inputProfiles()) != 1 {
		t.Fatal("committed registration not exposed")
	}
}

func TestRegisteredProfilesLargestAcceptedSetReopens(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	p, _ := testProfile(t)
	for i := 0; i < 16; i++ {
		p.ID = fmt.Sprintf("large%d", i)
		p.Adapter.Name = "a"
		p.Adapter.Name = strings.Repeat("a", 1+(32<<10)-len(encode(p)))
		if len(encode(p)) != 32<<10 {
			t.Fatal("fixture is not at the profile bound")
		}
		if _, err := s.registerInputProfile(p); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s = openJournal(t, cfg)
	if len(s.inputProfiles()) != 16 {
		t.Fatal("accepted profiles could not be reloaded")
	}
}

func TestRegisteredProfilesCanBackConfiguredGatewayOnRestart(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	p, _ := testProfile(t)
	if _, err := s.registerInputProfile(p); err != nil {
		t.Fatal(err)
	}
	s.Close()
	cfg.GatewayConnections = []GatewayConnection{{Profile: p.ID, URL: "http://127.0.0.1:65530"}}
	s = openJournal(t, cfg)
	if len(s.gatewayProfiles()) != 1 || s.gatewayProfiles()[0] != p.ID {
		t.Fatal("persisted profile is not available to the configured connection")
	}
}

// A store holding a registered profile that an earlier Runner admitted and
// this one refuses is not opened, and the refusal names that profile and
// why: here a scoped profile whose entry has an escaped slash, which RFC 0018
// round 16 refuses (runner #152), registered beside an exact profile, in the
// one row the store writes. The exact profile is held with it (recovery:
// runner #154). A configured profile so refused is named alike.
func TestAStoreHoldingAProfileThisRunnerRefusesNamesIt(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	exact, _ := testProfile(t)
	if _, err := s.registerInputProfile(exact); err != nil {
		t.Fatal(err)
	}
	var scoped InputProfile
	if err := strictJSON(encode(scopedPageProfile(func(p map[string]any) {
		p["endpointScope"] = []any{"https://example.com/docs/%2Fx"}
	})), &scoped); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO metadata(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", registeredProfilesKey, string(encode([]InputProfile{exact, scoped}))); err != nil {
		t.Fatal(err)
	}
	s.Close()
	const reason = "an endpointScope entry is not an endpoint prefix in the constrained page form, nor the wildcard https://* (input-malformed)"
	for whose, cfg := range map[string]Config{"registered": cfg, "configured": journalConfig(t)} {
		if whose == "configured" {
			cfg.InputProfiles = []InputProfile{exact, scoped}
		}
		other, err := Open(cfg)
		if err == nil {
			other.Close()
			t.Fatalf("a store holding a %s profile this Runner refuses was opened", whose)
		}
		t.Logf("%s: %v", whose, err)
		if !strings.Contains(err.Error(), whose+` input profile "scoped" is refused`) || !strings.HasSuffix(err.Error(), reason) {
			t.Fatalf("the refusal names neither the %s profile nor why: %v", whose, err)
		}
	}
}
