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

func releaseMatrix(outcome string) string {
	return string(encode(map[string]any{"matrixVersion": "3", "cases": []any{map[string]any{
		"id": "clean", "facts": sample().Facts, "evidenceAvailability": sample().Evidence,
		"expectedDisposition": map[string]any{"kind": "outcome", "outcomeId": outcome, "reasons": []string{}, "handoff": map[string]string{"state": "none"}},
	}}}))
}
func TestReleaseReadinessRealRuntime(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	matrix := releaseMatrix("proceed")
	source := &TestSource{PackKey: "intake", SuiteRevision: 3, CaseNames: map[string]string{"clean": "Clean intake"}}
	pass, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: sample(), Matrix: matrix, TestSource: source})
	if err != nil {
		t.Fatal(err)
	}
	if pass.Tests != "passed" || !releaseReady(pass) {
		t.Fatalf("check: %+v", pass.TestEvidence)
	}
	if pass.TestEvidence.Matrix != matrix || pass.TestEvidence.MatrixDigest != digest([]byte(matrix)) || pass.TestEvidence.Source.SuiteRevision != 3 {
		t.Fatal("lost frozen test inputs")
	}
	var report struct {
		Packs []struct{ Coverage []struct{ Status string } }
	}
	json.Unmarshal(pass.TestEvidence.Report, &report)
	gaps := 0
	for _, probe := range report.Packs[0].Coverage {
		if probe.Status == "missing" {
			gaps++
		}
	}
	if gaps == 0 {
		t.Fatal("fixture must demonstrate advisory coverage gaps")
	}
	job, err := s.createJob("Checked intake", pass.ID)
	if err != nil {
		t.Fatal("coverage gated passing release", err)
	}
	for _, tc := range []struct{ name, matrix, status string }{
		{"mismatch", releaseMatrix("decline-redirect"), "failed"},
		{"empty", `{"matrixVersion":"3","cases":[]}`, "error"},
		{"invalid", `{"matrixVersion":"3","cases":[{"id":"bad","facts":{}}]}`, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: sample(), Matrix: tc.matrix})
			if e != nil {
				t.Fatal(e)
			}
			if r.Tests != tc.status {
				t.Fatalf("got %s: %+v", r.Tests, r.TestEvidence)
			}
			saved, e := s.release(r.ID)
			if e != nil || !bytes.Equal(encode(r), encode(saved)) {
				t.Fatal("failed check was not retained", e)
			}
			if _, e = s.createJob("Blocked", r.ID); e == nil {
				t.Fatal("direct caller bypassed gate")
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(encode(map[string]any{"name": "Blocked", "releaseId": r.ID, "reviewed": true})))
			req.Header.Set("Authorization", "Bearer test-token")
			res := httptest.NewRecorder()
			s.Handler("test-token").ServeHTTP(res, req)
			if res.Code != 409 {
				t.Fatalf("HTTP gate %d %s", res.Code, res.Body.String())
			}
		})
	}
	// A later test failure or source edit does not rewrite the admitted release.
	held, err := s.release(pass.ID)
	if err != nil || !bytes.Equal(encode(pass), encode(held)) {
		t.Fatal("release changed", err)
	}
	snap, err := s.briefSnapshot("job", job.ID)
	if err != nil || !bytes.Contains(snap, []byte(`"testEvidence"`)) {
		t.Fatal("brief lost readiness", err)
	}
	s.Close()
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	held, err = s.release(pass.ID)
	if err != nil || !bytes.Equal(encode(pass), encode(held)) {
		t.Fatal("restart lost evidence", err)
	}
	run, _, err := s.submit(job.ID, "after-restart", sample())
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRun(t, s, run.ID); done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	// A passing badge alone is never admission evidence.
	for _, mutate := range []func(*Release){
		func(r *Release) { r.TestEvidence = nil },
		func(r *Release) { r.TestEvidence.Matrix = releaseMatrix("decline-redirect") },
		func(r *Release) { r.TestEvidence.PackDigest = "wrong" },
		func(r *Release) { r.TestEvidence.RuntimeDigest = "wrong" },
		func(r *Release) { r.TestEvidence.Report = json.RawMessage(`{"status":"passed"}`) },
	} {
		var changed Release
		json.Unmarshal(encode(pass), &changed)
		mutate(&changed)
		if releaseReady(changed) {
			t.Fatal("accepted incomplete/mismatched evidence")
		}
	}
}

func TestReleaseCheckRejectsIncompleteProcess(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pack, _ := os.ReadFile("testdata/triage.pack.json")
	r, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: sample(), Matrix: releaseMatrix("proceed")})
	if err != nil || r.Tests != "passed" {
		t.Fatal(err, r.Tests)
	}
	// A process emitting a valid report then exiting abnormally must not pass.
	fake := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\ncat <<'REPORT'\n" + string(r.TestEvidence.Report) + "\nREPORT\nexit 2\n"
	if err = os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	bin, hash, err := s.pinRuntime(fake)
	if err != nil {
		t.Fatal(err)
	}
	s.runtime = bin
	r.RuntimeDigest = hash
	e := s.checkReleaseTests(context.Background(), r, releaseMatrix("proceed"), nil, filepath.Join(cfg.Dir, "abnormal"))
	if e.Status != "error" || e.Problem == "" {
		t.Fatal("abnormal process became pass", e)
	}
}
