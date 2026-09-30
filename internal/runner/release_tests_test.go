package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// An installation can refuse jobs from releases whose tests never ran. Off by
// default; on, it refuses the creation of a job and nothing else.
func TestInstallationCanRefuseUntestedReleases(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	untested := testRelease(t, s)
	earlier, err := s.createJob("Untested", untested.ID)
	if err != nil || untested.Tests != "not-run" {
		t.Fatal("default refused an untested release", err, untested.Tests)
	}
	s.Close()
	cfg.RequireTestedReleases = true
	if s, err = Open(cfg); err != nil {
		t.Fatal(err)
	}
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	check := func(matrix string) Release {
		r, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: sample(), Matrix: matrix})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	fresh := check("")
	var refusal *apiError
	if _, err = s.createJob("Untested", fresh.ID); !errors.As(err, &refusal) || refusal.Status != 409 || refusal.Code != "release_untested" {
		t.Fatal("untested release became a job", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(encode(map[string]any{"name": "Untested", "releaseId": fresh.ID, "reviewed": true})))
	req.Header.Set("Authorization", "Bearer test-token")
	res := httptest.NewRecorder()
	s.Handler("test-token").ServeHTTP(res, req)
	var body struct {
		Error struct{ Code, Message string }
	}
	if json.Unmarshal(res.Body.Bytes(), &body); res.Code != 409 || body.Error.Code != "release_untested" || body.Error.Message != refusal.Message {
		t.Fatal(res.Code, res.Body.String())
	}
	// A job created with its first trigger is refused the same, and leaves no trigger.
	trigger := TriggerConfig{Name: "Intake event", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	if _, err = s.createJobConfigured("Untested", fresh.ID, &trigger); !errors.As(err, &refusal) || refusal.Code != "release_untested" {
		t.Fatal("untested release became a job with a trigger", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(encode(map[string]any{"name": "Untested", "releaseId": fresh.ID, "reviewed": true, "trigger": trigger})))
	req.Header.Set("Authorization", "Bearer test-token")
	res = httptest.NewRecorder()
	s.Handler("test-token").ServeHTTP(res, req)
	if json.Unmarshal(res.Body.Bytes(), &body); res.Code != 409 || body.Error.Code != "release_untested" {
		t.Fatal(res.Code, res.Body.String())
	}
	for table, want := range map[string]int{"jobs": 1, "triggers": 0, "trigger_revisions": 0} {
		if n := countRows(t, s, table); n != want {
			t.Fatal("refused job left rows in", table, n)
		}
	}
	// A job made before the setting was on is returned as before, and runs.
	if again, e := s.createJob("Untested", untested.ID); e != nil || again.ID != earlier.ID {
		t.Fatal("earlier job refused", e)
	}
	run, _, err := s.submit(earlier.ID, "after-setting", sample())
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRun(t, s, run.ID); done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	// Tests that ran keep their gate: failed has its own refusal, passed is a job.
	if _, err = s.createJob("Failed", check(releaseMatrix("decline-redirect")).ID); !errors.As(err, &refusal) || refusal.Code != "release_not_ready" {
		t.Fatal("failed release", err)
	}
	if _, err = s.createJob("Passed", check(releaseMatrix("proceed")).ID); err != nil {
		t.Fatal("passed release refused", err)
	}
}
