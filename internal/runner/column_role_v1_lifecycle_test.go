package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Gateway below is a signed producer fixture, not a live Desk producer.
// Runtime execution, SQLite custody, source-worker transport and exports are real.
func TestColumnRoleRealRuntimeFirstSaveSignedExports(t *testing.T) {
	cfg := testConfig(t)
	cfg.SigningKey = signingKey(t)
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	if !runtimeTakesBinding(t, cfg.Runtime) {
		t.Fatal("selected Runtime test binary must expose an explicit version supporting preparation bindings")
	}
	producer := selectedProducer(t)
	producer.profile.ConversionContracts[0].Limits["responseBytes"] = 2621440
	producer.profile.ConversionContracts[0].Limits["carrierBytes"] = 1572864
	producer.profile.ConversionContracts[0].Limits["originalSnapshotBytes"] = 262144
	producer.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(producer.profile)
	producer.input.Source.Mapping.Case.Facts = []FactMapping{{Target: "/caseNumber", Source: "/retained", Conversion: json.RawMessage(`{"contractVersion":"1","column":"X","kind":"number"}`)}}
	producer.input.Source.Mapping.Sources[0].Read.Copy.Evidence[0].Requirement = "intake-form"
	producer.input.Source.Mapping.Admits.Facts["/caseNumber"] = []string{"asserted"}
	producer.input.Source.Mapping.Admits.Evidence = map[string][]string{"intake-form": {"generated"}}
	producer.input.Source.Mapping.Unmapped = []string{"/request/appropriateness", "/request/completeness", "/request/embargoedInformationToUnauthorizedRecipients", "/request/type"}
	producer.input.Source.Mapping.UnmappedEvidence = []string{"sponsor-endorsement", "sensitive-data-approvals"}
	rawCase := []byte("{\n \"retained\":{\"contractVersion\":\"1\",\"origin\":\"asserted\",\"values\":{\"X\":\"-0\"},\"research\":null},\"policy\":{\"kind\":\"number\"},\"provenance\":{\"originalValues\":{\"X\":\"-0\"}}\n}")
	producer.input.Source.Case = rawCase
	cfg.InputProfiles = []InputProfile{producer.profile}
	var mu sync.Mutex
	var received []byte
	calls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seal" {
			write(w, 200, map[string]any{"sealed": true})
			return
		}
		var request struct {
			Session   string          `json:"session"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if r.URL.Path != "/acquire" || json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		signed := signResponse(t, producer.profile, producer.key, request.Arguments, producer.carrier(), time.Now(), 0)
		var parts map[string]json.RawMessage
		json.Unmarshal(signed, &parts)
		var receipt map[string]any
		json.Unmarshal(parts["receipt"], &receipt)
		receipt["sessionId"] = request.Session
		signReceipt(t, receipt, producer.key)
		raw := []byte("{\n \"result\":" + string(producer.carrier()) + ", \"receipt\":" + string(encode(receipt)) + ", \"salts\":" + string(parts["salts"]) + " }\n")
		mu.Lock()
		received = append([]byte(nil), raw...)
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer gateway.Close()
	worker, e := OpenSourceWorker(SourceWorkerConfig{Dir: filepath.Join(sourceWorkerTempDir(t), "caller"), Gateway: gateway.URL})
	if e != nil {
		t.Fatal(e)
	}
	defer worker.Close()
	operations := httptest.NewServer(worker.Handler())
	defer operations.Close()
	cfg.GatewayConnections = []GatewayConnection{{Profile: producer.profile.ID, URL: gateway.URL, Durable: true, OperationsURL: operations.URL, OperationsTokenFile: worker.TokenFile()}}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	pack, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	body := encode(PreviewRequest{Pack: string(pack), Input: producer.input})
	body = bytes.Replace(body, encode(rawCase), rawCase, 1)
	sampleResponse := producer.input.Source.Sources["desk"].Response
	body = bytes.Replace(body, encode(sampleResponse), sampleResponse, 1)
	req := httptest.NewRequest("POST", "/v1/retained-column/1/previews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer lifecycle")
	req.Header.Set("JPack-Conversion-Contract", selectedHeader(selectedContract()))
	w := httptest.NewRecorder()
	s.Handler("lifecycle").ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("actual selected release preview: %d %.1000s", w.Code, w.Body.String())
	}
	var release Release
	if json.Unmarshal(w.Body.Bytes(), &release) != nil {
		t.Fatal("release response")
	}
	job, e := s.createJob("Selected retained", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	triggerConfig := constantTrigger(time.Now())
	triggerConfig.Input = &AutomaticInput{Kind: "mapped-sources", Case: rawCase}
	triggerConfig.PreparationSeconds = 3600
	trigger, e := s.configureTrigger(job.ID, "", 0, triggerConfig)
	if e != nil {
		t.Fatal(e)
	}
	occurrence := newOccurrence(trigger, job, time.Now())
	occurrence.PendingInput = triggerConfig.Input
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if e = s.admitOccurrence(tx, trigger, &occurrence, "fixture", ""); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	// Actual seed and first durable checkpoint, before advance can mask a loss.
	seed, e := s.automaticInputSeed(occurrence.PendingInput, release, nil)
	if e != nil || !sameConversion(inputConversion(seed), selectedContract()) {
		t.Fatal("automatic seed lost selected release identity", e)
	}
	occurrence.State = "waiting"
	occurrence.Preparation = &SourcePreparation{StartedAt: now(), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Tasks: []SourceTask{}, Input: &seed}
	if e = s.saveOccurrence(occurrence); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	occurrence, e = s.occurrence(occurrence.ID)
	if e != nil {
		t.Fatal(e)
	}
	if occurrence.Preparation == nil || occurrence.Preparation.Input == nil || !sameConversion(inputConversion(*occurrence.Preparation.Input), selectedContract()) || !bytes.Equal(mustColumnCaseBytes(occurrence.Preparation.Input.Source), rawCase) {
		t.Fatal("actual first-save restart lost selected identity or exact case")
	}
	for step := 0; step < 30 && occurrence.State == "waiting"; step++ {
		occurrence = stepPreparation(t, s, occurrence.ID)
		if occurrence.State == "waiting" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if occurrence.State != "accepted" || occurrence.Input == nil || len(occurrence.Preparation.Tasks) != 1 {
		t.Fatalf("actual continued source preparation: state=%s reason=%s", occurrence.State, occurrence.Reason)
	}
	task := occurrence.Preparation.Tasks[0]
	if !taskConversionMatches(task.Request, selectedContract()) {
		t.Fatal("durable source request lost selected identity")
	}
	changed := selectedContract()
	changed.Roles = append(append([]string(nil), changed.Roles[:3]...), changed.Roles[4])
	if taskConversionMatches(task.Request, changed) {
		t.Fatal("resumed operation allowed conversion rebind")
	}
	var heldRequest sourceRequest
	if strictJSON(task.Request, &heldRequest) != nil || !heldRequest.RetainedColumn || heldRequest.ConversionLimits["responseBytes"] != 2621440 || heldRequest.ConversionLimits["carrierBytes"] != 1572864 || heldRequest.ConversionLimits["originalSnapshotBytes"] != 262144 {
		t.Fatal("durable operation request")
	}
	heldRequest.ConversionContract = changed
	if _, code, e := worker.submit(heldRequest); code != 409 || e == nil {
		t.Fatal("source operation ID accepted a changed selected identity", code, e)
	}
	mu.Lock()
	exact := append([]byte(nil), received...)
	count := calls
	mu.Unlock()
	if count != 1 || occurrence.Input.Source.Sources["desk"].RetainedResponseJSON != string(exact) {
		t.Fatal("source worker first durable response save changed exact custody or repeated acquisition")
	}
	worker.mu.Lock()
	status, e := worker.status(task.ID)
	worker.mu.Unlock()
	if e != nil || status.RetainedResponseJSON != string(exact) || len(status.Response) != 0 {
		t.Fatal("selected source status custody changed", e)
	}
	// Admit the exact recovered occurrence input through the public run adapter.
	req = httptest.NewRequest("POST", "/v1/retained-column/1/jobs/"+job.ID+"/runs", bytes.NewReader(encode(*occurrence.Input)))
	req.Header.Set("Authorization", "Bearer lifecycle")
	req.Header.Set("JPack-Conversion-Contract", selectedHeader(selectedContract()))
	req.Header.Set("Idempotency-Key", "selected-first-save")
	w = httptest.NewRecorder()
	s.Handler("lifecycle").ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("actual selected run admission: %d %.1000s", w.Code, w.Body.String())
	}
	var queued Run
	if json.Unmarshal(w.Body.Bytes(), &queued) != nil {
		t.Fatal("run response")
	}
	if worked, e := s.dispatchNext(context.Background()); e != nil || !worked {
		t.Fatal("actual Runtime dispatch", worked, e)
	}
	run := waitRun(t, s, queued.ID)
	if run.State != "completed" || run.Binding == nil || run.Binding.Status != RunBound || run.Signing == nil || run.Signing.Status != SigningSigned || len(run.AuditSignatures) == 0 {
		t.Fatalf("actual Runtime did not complete bound signed record: state=%s binding=%+v signing=%+v problem=%s", run.State, run.Binding, run.Signing, run.Problem)
	}
	facts := member(run.Audit, "inputs", "facts")
	if !bytes.Contains(facts, []byte(`"caseNumber":-0`)) || !bytes.Contains(facts, []byte(`"min":-0`)) {
		t.Fatalf("actual Runtime audit changed asserted/range negative zero: %s", facts)
	}
	evidence := os.Getenv("JPACK_ROLE_EVIDENCE_DIR")
	for _, version := range []int{2, 3, 4, 5} {
		req = httptest.NewRequest("GET", fmt.Sprintf("/v1/retained-column/1/runs/%s/verification?version=%d", run.ID, version), nil)
		req.Header.Set("Authorization", "Bearer lifecycle")
		req.Header.Set("JPack-Conversion-Contract", selectedHeader(selectedContract()))
		w = httptest.NewRecorder()
		s.Handler("lifecycle").ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("actual export v%d: %d %.1000s", version, w.Code, w.Body.String())
		}
		v, e := VerifyInputs(w.Body.Bytes(), cfg.InputProfiles, releaseDigest(release), selectedContract())
		if e != nil || !v.Checked() || v.ExportVersion() != version {
			t.Fatalf("actual signed Runtime export v%d offline: %v", version, e)
		}
		if version == 5 {
			report := v.CheckSignature(s.expectedKey)
			if report.Status != SignatureSigned {
				t.Fatalf("real Runtime signature not verified: %+v", report)
			}
		}
		if evidence != "" {
			if e = os.WriteFile(filepath.Join(evidence, fmt.Sprintf("real-export-v%d.json", version)), w.Body.Bytes(), 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	if evidence != "" {
		for name, data := range map[string][]byte{"real-release.json": encode(release), "real-run.json": encode(served(run)), "real-selector.txt": []byte(selectedHeader(selectedContract())), "real-profiles.json": encode(cfg.InputProfiles), "real-release-digest.txt": []byte(releaseDigest(release)), "real-record.json": run.AuditBytes, "real-operation-request.json": task.Request, "real-acquired-response.json": exact, "real-dialect.json": encode(run.Input.Source.Mapping), "real-case.json": mustColumnCaseBytes(run.Input.Source), "real-sources.json": encode(run.Input.Source.Sources), "real-preparation.json": encode(run.Input.Preparation), "real-preparation-digest.txt": []byte(digest(encode(run.Input.Preparation)))} {
			if e = os.WriteFile(filepath.Join(evidence, name), data, 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	if !strings.Contains(string(exact), "\n") {
		t.Fatal("exact custody fixture lost its intentional whitespace")
	}
	t.Logf("actual pinned Runtime %s; record=%s bytes=%d; selected SQLite first-save restart, real SourceWorker with signed producer fixture, binding, signature and exports2/3/4/5 passed", release.RuntimeDigest, digest(run.AuditBytes), len(run.AuditBytes))
}
