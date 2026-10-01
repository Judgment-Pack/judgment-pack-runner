package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// exportedRun completes one mapping v2 run with the real Runtime, through the
// runner's HTTP API, and returns its verification export.
func exportedRun(t *testing.T) (bundle map[string]json.RawMessage, digest string) {
	t.Helper()
	bin := os.Getenv("JPACK_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_TEST_BIN to exercise the real Runtime contract")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := runner.Open(runner.Config{Dir: dir, Runtime: bin, Workspace: "test-workspace", Owner: "local-owner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h := s.Handler("test")
	call := func(method, path string, body any, key string, want int) json.RawMessage {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatal(method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	pack, err := os.ReadFile("../../internal/runner/testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	input := runner.Input{Source: &runner.SourceInput{
		Mapping: runner.InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &runner.CaseMapping{
			Facts:    []runner.FactMapping{{Target: "/request", Source: "/facts/request"}},
			Evidence: []runner.EvidenceMapping{{Requirement: "intake-form", Source: "/evidence/intake-form"}, {Requirement: "sponsor-endorsement", Source: "/evidence/sponsor-endorsement"}},
		}},
		Case: json.RawMessage(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`),
	}}
	var id struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	json.Unmarshal(call("POST", "/v1/previews", map[string]any{"pack": string(pack), "input": input}, "", 201), &id)
	json.Unmarshal(call("POST", "/v1/jobs", map[string]any{"name": "Verified", "releaseId": id.ID, "reviewed": true}, "", 201), &id)
	json.Unmarshal(call("POST", "/v1/jobs/"+id.ID+"/runs", input, "one", 202), &id)
	for deadline := time.Now().Add(10 * time.Second); id.State != "completed"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) || id.State == "failed" || id.State == "interrupted" {
			t.Fatal("run did not complete:", id.State)
		}
		json.Unmarshal(call("GET", "/v1/runs/"+id.ID, nil, "", 200), &id)
	}
	if err = json.Unmarshal(call("GET", "/v1/runs/"+id.ID+"/verification", nil, "", 200), &bundle); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(bundle["releaseDigest"], &digest); err != nil {
		t.Fatal(err)
	}
	return bundle, digest
}

// verify runs the command on an export, as an operator would, with no trusted
// profiles: the fixture's mapping reads its case only.
func verify(t *testing.T, bundle map[string]json.RawMessage, digest string, extra ...string) (map[string]string, string, error) {
	t.Helper()
	dir := t.TempDir()
	file, profiles := filepath.Join(dir, "run.json"), filepath.Join(dir, "profiles.json")
	raw, _ := json.Marshal(bundle)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profiles, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := verifyRun(append([]string{"--file", file, "--profiles", profiles, "--release-digest", digest}, extra...), &stdout, &stderr)
	var report map[string]string
	if stdout.Len() > 0 {
		d := json.NewDecoder(&stdout)
		d.DisallowUnknownFields()
		if e := d.Decode(&report); e != nil {
			t.Fatal("report is not one JSON object:", e)
		}
	}
	return report, stderr.String(), err
}

// verified-inputs must not be readable as covering the result. A retained
// disposition changed after the run is not detected, and the report says it was
// not checked, in the members a program reads and in the sentence a person reads.
func TestVerifyRunSaysTheDispositionWasNotChecked(t *testing.T) {
	bundle, digest := exportedRun(t)
	var run map[string]json.RawMessage
	if err := json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(run["result"], []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`), 1)
	if bytes.Equal(changed, run["result"]) {
		t.Fatal("fixture has no disposition to change")
	}
	for name, result := range map[string]json.RawMessage{"retained": run["result"], "changed after the run": changed} {
		t.Run(name, func(t *testing.T) {
			run["result"] = result
			bundle["run"], _ = json.Marshal(run)
			report, human, err := verify(t, bundle, digest)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"status": "verified-inputs", "retainedDisposition": "not-checked", "scope": "retained input derivation and audit binding; not sealed-session completeness or policy truth"}
			if len(report) != len(want) {
				t.Fatal(report)
			}
			for k, v := range want {
				if report[k] != v {
					t.Fatal(k, report)
				}
			}
			if !strings.Contains(human, "disposition was not checked") || !strings.Contains(human, "does not say the run decided what its record says") {
				t.Fatal("human output:", human)
			}
		})
	}
}
