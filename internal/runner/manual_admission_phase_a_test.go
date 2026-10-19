package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func manualServiceFixture(t *testing.T) *Service {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Dir: dir, Runtime: os.Args[0], Workspace: "manual-test", Owner: "local-owner", disableAutomation: true, disableDispatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func manualSavedJob(t *testing.T, s *Service) Job {
	t.Helper()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ ID, Version, Title string }
	_ = json.Unmarshal(pack, &doc)
	release := Release{SchemaVersion: "1", ID: "release_manual", Pack: string(pack), PackDigest: digest(pack), PackID: doc.ID, PackVersion: doc.Version, Title: doc.Title, RuntimeDigest: s.runtimeDigest, CreatedAt: "2026-10-19T12:00:00Z", Config: `{"configVersion":"3","packs":{"target":{"path":"pack.json"}}}`, Lock: `{}`, Validation: json.RawMessage(`{}`), Sample: sample(), Preview: json.RawMessage(`{}`), Tests: "not-run"}
	if err = s.saveRelease(release); err != nil {
		t.Fatal(err)
	}
	job, err := s.createJob("Manual binding", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func TestManualExecutionBindingOwnerExactExport(t *testing.T) {
	s := manualServiceFixture(t)
	job := manualSavedJob(t, s)
	request := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+job.ID+"/execution-binding", nil)
	denied := httptest.NewRecorder()
	s.Handler("owner-secret").ServeHTTP(denied, request)
	if denied.Code != 401 {
		t.Fatalf("unauthorized %d", denied.Code)
	}
	request.Header.Set("Authorization", "Bearer owner-secret")
	response := httptest.NewRecorder()
	s.Handler("owner-secret").ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("binding %d %s", response.Code, response.Body.String())
	}
	var binding ManualExecutionBinding
	if json.Unmarshal(response.Body.Bytes(), &binding) != nil {
		t.Fatal("binding JSON")
	}
	release, err := s.release(job.ReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := executionPayloadCanonical(release, maxExecutionRelease)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Version != 1 || binding.Workspace != s.cfg.Workspace || binding.JobID != job.ID || binding.JobRevision != "1" || binding.ReleaseProfile != runnerCanonicalProfile || !bytes.Equal(binding.ReleaseBytes, expected) || binding.ReleaseDigest != digest(expected) {
		t.Fatal("canonical binding changed")
	}
	binding.ReleaseBytes[0] = 'x'
	again, err := s.manualExecutionBinding(job.ID)
	if err != nil || !bytes.Equal(again.ReleaseBytes, expected) {
		t.Fatal("export mutability", err)
	}
	if _, err = s.db.Exec("INSERT INTO triggers(id,job_id,record)VALUES(?,?,?)", "trg_manual", job.ID, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.manualExecutionBinding(job.ID); err == nil {
		t.Fatal("paused/unsupported trigger exported")
	}
}
func TestManualIntentDurableExactPreimages(t *testing.T) {
	for _, facts := range []string{`false`, `0`, `null`, `{ "n":9007199254740993.00, "other":1e+2,"flag":false,"null":null,"unicode":"\u003c" }`} {
		t.Run(facts, func(t *testing.T) {
			raw := []byte("{\n \"facts\" : " + facts + "\n}")
			intent, err := captureManualIntent("intent_test", "original-key", raw)
			if err != nil {
				t.Fatal(err)
			}
			raw[0] = 'x'
			run := Run{SchemaVersion: "1", ID: "run_manual", Input: intent.input(), manual: intent}
			encoded := runText(run)
			restored, err := decodeRun([]byte(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(restored.Input.Facts, []byte(facts)) || restored.manual.key != "original-key" || restored.manual.intentID != "intent_test" || restored.manual.supplied || !bytes.Equal(restored.manual.stored().Request, []byte(intent.request)) {
				t.Fatal("exact preimages changed")
			}
			if bytes.Contains(encode(served(restored)), []byte("manualAdmission")) || bytes.Contains(encode(verificationExport(Release{}, restored, 2)), []byte("requestBytes")) {
				t.Fatal("private ingress metadata leaked into public export")
			}
			var wire map[string]json.RawMessage
			_ = json.Unmarshal([]byte(encoded), &wire)
			var stored manualIntentStored
			_ = json.Unmarshal(wire["manualAdmission"], &stored)
			stored.Facts = []byte(`true`)
			wire["manualAdmission"] = encode(stored)
			if _, err := decodeRun(encode(wire)); err == nil {
				t.Fatal("changed preimage accepted")
			}
		})
	}
	raw := []byte(`{ "facts": { "n": 1.00 }, "evidence": {"x":"unknown"} }`)
	intent, err := captureManualIntent("intent_test", "original-key", raw)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decodeRun([]byte(runText(Run{Input: intent.input(), manual: intent})))
	if err != nil || !bytes.Equal(restored.Input.Evidence, []byte(`{"x":"unknown"}`)) || !restored.manual.supplied {
		t.Fatal("evidence presence/preimage", err)
	}
	for _, raw := range []string{`{"facts":{},"source":null}`, `{"facts":{},"preparation":{}}`, `{"Facts":{}}`, `{"facts":{},"facts":{}}`, `{"facts":{},"evidence":null}`, `{"facts":{},"caller":"admin"}`} {
		if _, err := captureManualIntent("intent_test", "key", []byte(raw)); err == nil {
			t.Fatalf("unsupported input accepted %s", raw)
		}
	}
}
func TestManualAdmissionHeaderSelectorAndDormantPaths(t *testing.T) {
	s := manualServiceFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"facts":{}}`))
	request.Header.Set(ManualIntentHeader, "intent_forged")
	if _, _, err := s.manualOwnerInput(httptest.NewRecorder(), request, "key"); err == nil {
		t.Fatal("unconfigured private header accepted")
	}
	binding := manualFixture(t).Synthetic.Boot
	s.cfg.ExecutorAdmission = &binding
	request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{ "facts": { "n":9007199254740993.00 } }`))
	request.Header.Set(ManualIntentHeader, "intent_host_selector")
	request.Header.Set("Idempotency-Key", "original-key")
	input, intent, err := s.manualOwnerInput(httptest.NewRecorder(), request, "original-key")
	if err != nil || intent == nil || !bytes.Equal(input.Facts, []byte(`{ "n":9007199254740993.00 }`)) {
		t.Fatal("ingress lost original bytes", err)
	}
	if _, _, err = s.submit("missing-job", "key", Input{Facts: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("old queue ingress bypass")
	}
	if _, err = s.workflowDriver(nil).Acquire(context.Background(), workflowAcquisition{}); err == nil {
		t.Fatal("workflow acquired")
	}
	if _, err = s.workflowDriver(nil).Evaluate(context.Background(), Release{}, Input{}, "should-not-create"); err == nil {
		t.Fatal("workflow evaluated")
	}
	if _, err = s.upgradeJob(context.Background(), "missing-job", UpgradeJobRequest{}); err == nil {
		t.Fatal("upgrade bypass")
	}
	if _, _, _, _, err = s.evaluate(context.Background(), Release{}, Input{}, "should-not-create", false); err == nil {
		t.Fatal("alternate operational evaluator bypass")
	}
}
func TestManualAdmissionOpenRefusesUnsafeOwnershipBeforeSideEffects(t *testing.T) {
	f := manualFixture(t)
	o := &ExecutorOwnership{Root: "/never-opened", MachineWorkspace: f.Synthetic.Boot.MachineWorkspace, RunnerWorkspace: f.Synthetic.Boot.RunnerWorkspace, RootInode: 1, FileInode: 1}
	state := filepath.Join(t.TempDir(), "not-created")
	cfg := Config{Dir: state, Runtime: "/missing-runtime", Workspace: f.Synthetic.Boot.RunnerWorkspace, Owner: "owner", ExecutorOwnership: o, ExecutorAdmission: &f.Synthetic.Boot}
	if _, err := Open(cfg); err == nil || ManualAdmissionAvailable() && err == errManualInactive || !ManualAdmissionAvailable() && err != errManualInactive {
		t.Fatalf("unsafe ownership or unsupported profile accepted: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("unsafe ownership touched state", err)
	}
}

func TestManualIntentReservationBindsOriginalBodyAndPayload(t *testing.T) {
	f := manualFixture(t)
	request := manualVectorBody[ManualReserveRequest](t, f.Vectors[2].Request)
	rawFile, err := os.ReadFile("../../docs/design/manual-admission-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var rawFixture struct {
		Synthetic struct {
			Request []byte `json:"manualRequestBytes"`
		} `json:"synthetic"`
	}
	if json.Unmarshal(rawFile, &rawFixture) != nil {
		t.Fatal("fixture")
	}
	intent, err := captureManualIntent(request.IntentID, "original-key", rawFixture.Synthetic.Request)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := intent.reservation(request.Resource, request.Registration, request.Payload)
	if err != nil || !bytes.Equal(encode(actual), encode(request)) {
		t.Fatal("trusted body binding", err)
	}
	changed, err := captureManualIntent(request.IntentID, "original-key", append([]byte(" "), rawFixture.Synthetic.Request...))
	if err != nil {
		t.Fatal(err)
	}
	other, err := changed.reservation(request.Resource, request.Registration, request.Payload)
	if err != nil || other.RawRequestDigest == request.RawRequestDigest {
		t.Fatal("outer body bytes did not bind", err)
	}
	intent.facts = "{}"
	if _, err = intent.reservation(request.Resource, request.Registration, request.Payload); err == nil {
		t.Fatal("changed facts bound to original payload")
	}
}

func TestManualIntentSQLiteRestoreAndNoDowngrade(t *testing.T) {
	service := manualServiceFixture(t)
	job := manualSavedJob(t, service)
	binding := manualFixture(t).Synthetic.Boot
	service.cfg.ExecutorAdmission = &binding
	manualGateAuthority(t, service, nil)
	raw := []byte(`{ "facts": { "number":9007199254740993.00,"flag":false }, "evidence": { "x":"unknown" } }`)
	intent, err := captureManualIntent("intent_sqlite", "original-key", raw)
	if err != nil {
		t.Fatal(err)
	}
	accepted, replayed, err := service.submitInternalIntent(job.ID, "original-key", intent.input(), nil, intent)
	if err != nil || replayed {
		t.Fatal("queue", err)
	}
	stored, err := service.run(accepted.ID)
	if err != nil || !bytes.Equal(stored.Input.Facts, []byte(intent.facts)) || !bytes.Equal(stored.Input.Evidence, []byte(intent.evidence)) {
		t.Fatal("SQLite changed preimages", err)
	}
	changed, err := captureManualIntent("intent_sqlite", "original-key", append([]byte(" "), raw...))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.submitInternalIntent(job.ID, "original-key", changed.input(), nil, changed); err == nil {
		t.Fatal("same original key accepted changed outer body")
	}
	cfg := service.cfg
	cfg.ExecutorAdmission = nil
	service.Close()
	restored, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	held, err := restored.run(accepted.ID)
	if err != nil || !bytes.Equal(held.Input.Facts, []byte(intent.facts)) || !bytes.Equal(held.Input.Evidence, []byte(intent.evidence)) || held.manual == nil || held.manual.request != intent.request {
		t.Fatal("restore changed exact ingress", err)
	}
	if worked, err := restored.dispatchNext(context.Background()); err == nil || worked {
		t.Fatal("restored manual entry escaped through unconfigured dispatch")
	}
}
