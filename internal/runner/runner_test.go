package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	bin := os.Getenv("JPACK_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_TEST_BIN to exercise the real Runtime contract")
	}
	// macOS commonly exposes temporary directories through /var -> /private/var.
	// Give the fixture its physical path without relaxing Runner's state guard.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return Config{Dir: dir, Runtime: bin, Workspace: "test-workspace", Owner: "local-owner"}
}
func sample() Input {
	return Input{Facts: json.RawMessage(`{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}}`), Evidence: json.RawMessage(`{"intake-form":"present","sponsor-endorsement":"present"}`)}
}
func testRelease(t *testing.T, s *Service) Release {
	t.Helper()
	p, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.preview(context.Background(), PreviewRequest{Pack: string(p), Input: sample()})
	if e != nil {
		files, _ := filepath.Glob(filepath.Join(s.cfg.Dir, "releases", "*", "preview", "evaluation.stdout"))
		for _, p := range files {
			b, _ := os.ReadFile(p)
			t.Log(string(b))
		}
		t.Fatal(e)
	}
	return r
}
func waitRun(t *testing.T, s *Service, id string) Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, e := s.run(id)
		if e != nil {
			t.Fatal(e)
		}
		if r.State != "queued" && r.State != "running" {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run timed out")
	return Run{}
}
func TestRealRuntimeDurabilityAndIdempotency(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	release := testRelease(t, s)
	job, e := s.createJob("Intake", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	run, replay, e := s.submit(job.ID, "one", sample())
	if e != nil || replay {
		t.Fatal(e, replay)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	if len(done.Audit) == 0 {
		t.Fatal("missing audit")
	}
	again, replay, e := s.submit(job.ID, "one", sample())
	if e != nil || !replay || again.ID != run.ID {
		t.Fatal(e, replay, again.ID)
	}
	changed := sample()
	changed.Evidence = nil
	if _, _, e = s.submit(job.ID, "one", changed); e == nil {
		t.Fatal("key reused for different input")
	}
	missing, _, e := s.submit(job.ID, "missing", changed)
	if e != nil {
		t.Fatal(e)
	}
	done = waitRun(t, s, missing.ID)
	if done.State != "completed" || !bytes.Contains(done.Result, []byte(`"unresolved"`)) {
		t.Fatal(done.State, done.Problem, string(done.Result))
	}
	// A browser/client is not the dispatcher; acceptance completes before work.
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	again, replay, e = s.submit(job.ID, "one", sample())
	if e != nil || !replay || again.ID != run.ID {
		t.Fatal("idempotency did not survive restart", e)
	}
	got, e := s.release(release.ID)
	if e != nil || got.PackDigest != release.PackDigest {
		t.Fatal("release changed", e)
	}
}
func TestRecoveryAndExclusiveOwner(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := Open(cfg); e == nil {
		other.Close()
		t.Fatal("concurrent dispatcher allowed")
	}
	release := testRelease(t, s)
	j, e := s.createJob("Recovery", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	s.cancel()
	<-s.done
	// Inject the durable states on both sides of the claim boundary.
	for _, state := range []string{"queued", "running"} {
		r := Run{SchemaVersion: "1", ID: "run_" + state, JobID: j.ID, ReleaseID: release.ID, Revision: 1, State: state, Input: sample(), CreatedAt: now(), RequestedBy: cfg.Owner}
		_, e = s.db.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", r.ID, j.ID, cfg.Owner, state, "fixture", state, string(encode(r)))
		if e != nil {
			t.Fatal(e)
		}
	}
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	interrupted, e := s.run("run_running")
	if e != nil || interrupted.State != "interrupted" {
		t.Fatal("ambiguous invocation replayed", e)
	}
	if r := waitRun(t, s, "run_queued"); r.State != "completed" {
		t.Fatal(r.Problem)
	}
	if _, e = os.Stat(filepath.Join(cfg.Dir, "attempts", "run_running")); !os.IsNotExist(e) {
		t.Fatal("interrupted invocation reran")
	}
}
func TestHTTPAuthorityAndValidation(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	handler := s.Handler("private-test-bearer")
	for _, tc := range []struct {
		method, path, body, token, origin string
		status                            int
	}{
		{"GET", "/v1/jobs", "", "", "", 401}, {"GET", "/v1/jobs", "", "private-test-bearer", "https://evil.example", 403},
		{"POST", "/v1/jobs", "{\"name\":\"x\",\"owner\":\"other\"}", "private-test-bearer", "", 400},
		{"POST", "/v1/jobs/nope/runs", "{\"facts\":{}}", "private-test-bearer", "", 400},
		{"POST", "/v1/jobs", "{\"name\":\"x\",\"releaseId\":\"x\",\"reviewed\":false}", "private-test-bearer", "", 422},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	_ = http.MethodGet
}

// Child exits without Close after the real evaluator has appended its audit,
// while the durable logical run is still running. Recovery must not replay it.
func TestAuditCrashWindow(t *testing.T) {
	if os.Getenv("JOBS_CRASH_CHILD") == "1" {
		cfg := Config{Dir: os.Getenv("JOBS_CRASH_DIR"), Runtime: os.Getenv("JPACK_TEST_BIN"), Workspace: "crash", Owner: "owner"}
		s, e := Open(cfg)
		if e != nil {
			t.Fatal(e)
		}
		release := testRelease(t, s)
		j, e := s.createJob("Crash", release.ID)
		if e != nil {
			t.Fatal(e)
		}
		s.cancel()
		<-s.done
		r := Run{SchemaVersion: "1", ID: "run_crash", JobID: j.ID, ReleaseID: release.ID, Revision: 1, State: "running", Input: sample(), CreatedAt: now(), RequestedBy: cfg.Owner, Attempt: 1}
		_, e = s.db.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", r.ID, j.ID, cfg.Owner, "crash", "fixture", r.State, string(encode(r)))
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = s.evaluate(context.Background(), release, r.Input, filepath.Join(cfg.Dir, "attempts", r.ID), false); e != nil {
			t.Fatal(e)
		}
		os.Exit(57)
	}
	cfg := testConfig(t)
	cfg.Workspace = "crash"
	cfg.Owner = "owner"
	child := exec.Command(os.Args[0], "-test.run=^TestAuditCrashWindow$")
	child.Env = append(os.Environ(), "JOBS_CRASH_CHILD=1", "JOBS_CRASH_DIR="+cfg.Dir)
	out, e := child.CombinedOutput()
	if code, ok := e.(*exec.ExitError); !ok || code.ExitCode() != 57 {
		t.Fatalf("fault injection failed: %v %s", e, out)
	}
	path := filepath.Join(cfg.Dir, "attempts", "run_crash", "audit", "evaluations.jsonl")
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := s.run("run_crash")
	if e != nil || r.State != "interrupted" || len(r.Result) > 0 {
		t.Fatal("ambiguous result was published or retried", r.State, e)
	}
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(before, after) || bytes.Count(after, []byte("\n")) != 1 {
		t.Fatal("audit changed during recovery", e)
	}
}
func TestPinnedReleaseAndRuntimeDrift(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	release := testRelease(t, s)
	j, e := s.createJob("Pinned", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	j2, e := s.createJob("Pinned", release.ID)
	if e != nil || j.ID != j2.ID {
		t.Fatal("job creation retry duplicated the job", e)
	}
	// Editing the author's or preview's filesystem pack cannot change the sealed
	// database bytes used for an operational invocation.
	os.WriteFile(filepath.Join(cfg.Dir, "releases", release.ID, "pack.json"), []byte(`{}`), 0600)
	r, _, e := s.submit(j.ID, "sealed", sample())
	if e != nil {
		t.Fatal(e)
	}
	if got := waitRun(t, s, r.ID); got.State != "completed" {
		t.Fatal(got.Problem)
	}
	if e = os.WriteFile(s.runtime+".changed", []byte("changed executable"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(s.runtime+".changed", s.runtime); e != nil {
		t.Fatal(e)
	}
	r, _, e = s.submit(j.ID, "drift", sample())
	if e != nil {
		t.Fatal(e)
	}
	if got := waitRun(t, s, r.ID); got.State != "failed" || got.Result != nil {
		t.Fatal("runtime drift was evaluated", got.State, got.Problem)
	}
}
