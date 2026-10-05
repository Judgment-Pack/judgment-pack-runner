package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// talkingRuntime writes stderr and stdout, then refuses the evaluation.
func talkingRuntime(t *testing.T, dir, stderr, stdout string) string {
	return fakeRuntime(t, dir, "printf '%s' '"+stdout+"'\nprintf '%s' '"+stderr+"' >&2\nexit 3")
}

// runOn submits a run to a job of a release on runtime, and dispatches it.
func runOn(w *coverWorld, runtime string) Run {
	w.t.Helper()
	r := journalRelease(w.t, w.s, runtime)
	j, e := w.s.createJob("Diagnostics", r.ID)
	must(w.t, e)
	run, _, e := w.s.submit(j.ID, "one", journalInput())
	must(w.t, e)
	_, e = w.s.dispatchNext(context.Background())
	must(w.t, e)
	run, e = w.s.run(run.ID)
	must(w.t, e)
	return run
}

func diagnostics(w *coverWorld, run, query string) (int, string, map[string]string, []byte) {
	w.t.Helper()
	got := serve(w.t, w.s, "GET", "/v1/runs/"+run+"/diagnostics"+query, "", nil)
	h := map[string]string{}
	for _, k := range []string{"Content-Type", "Cache-Control", "X-Diagnostics-Bytes", "X-Diagnostics-Truncated"} {
		h[k] = got.Header().Get(k)
	}
	var refusal struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	code := ""
	if got.Code != 200 && json.Unmarshal(got.Body.Bytes(), &refusal) == nil {
		code = refusal.Error.Code + ": " + refusal.Error.Message
	}
	return got.Code, code, h, got.Body.Bytes()
}

// A finished run's diagnostics are what its Runtime wrote, as text: standard
// error by default, standard output when asked. A run still queued or running
// is refused; one that kept none answers 404 with the reason. Reading them
// writes no entry: a read is no change of state.
func TestARunsDiagnosticsAreServedOnceItHasFinished(t *testing.T) {
	w := newCoverWorld(t)
	failed := runOn(w, talkingRuntime(t, w.cfg.Dir, "the Runtime refused: marker-stderr", `{"status":"refused","marker":"stdout"}`))
	if failed.State != "failed" {
		t.Fatal(failed.State)
	}
	before := len(w.entries())
	status, _, h, body := diagnostics(w, failed.ID, "")
	if status != 200 || string(body) != "the Runtime refused: marker-stderr" || h["Content-Type"] != "text/plain; charset=utf-8" || h["Cache-Control"] != "no-store" || h["X-Diagnostics-Bytes"] != strconv.Itoa(len(body)) || h["X-Diagnostics-Truncated"] != "false" {
		t.Fatalf("stderr: %d %v %q", status, h, body)
	}
	if status, _, _, body = diagnostics(w, failed.ID, "?stream=stdout"); status != 200 || string(body) != `{"status":"refused","marker":"stdout"}` {
		t.Fatalf("stdout: %d %q", status, body)
	}
	if status, _, _, body = diagnostics(w, failed.ID, "?stream=stderr"); status != 200 || string(body) != "the Runtime refused: marker-stderr" {
		t.Fatalf("stderr asked for: %d %q", status, body)
	}
	if added := w.entries()[before:]; len(added) != 0 {
		t.Fatalf("a read wrote %v", kindsOf(added))
	}

	completed := runOn(w, completingRuntime(t, w.cfg.Dir))
	if status, _, h, body = diagnostics(w, completed.ID, ""); completed.State != "completed" || status != 200 || len(body) != 0 || h["X-Diagnostics-Bytes"] != "0" {
		t.Fatalf("a completed run's empty stderr: %s %d %v %q", completed.State, status, h, body)
	}

	queued, _, e := w.s.submit(w.job.ID, "queued", journalInput())
	must(t, e)
	if status, code, _, _ := diagnostics(w, queued.ID, ""); status != 409 || !strings.HasPrefix(code, "run_not_finished") {
		t.Fatalf("a queued run: %d %s", status, code)
	}
	_, e = w.s.dispatchNext(context.Background())
	must(t, e)

	r := journalRelease(t, w.s, waitingRuntime(t, w.cfg.Dir, filepath.Join(t.TempDir(), "pid")))
	j, e := w.s.createJob("Waits", r.ID)
	must(t, e)
	running, _, e := w.s.submit(j.ID, "running", journalInput())
	must(t, e)
	stop, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := w.s.dispatchNext(stop); done <- e }()
	waitForRunState(t, w.s, running.ID, "running")
	status, code, _, _ := diagnostics(w, running.ID, "")
	cancel()
	must(t, <-done)
	if status != 409 || !strings.HasPrefix(code, "run_not_finished") {
		t.Fatalf("a running run: %d %s", status, code)
	}

	tr := w.scheduleTrigger("")
	expired, _, e := w.s.submitInternal(w.job.ID, "late", journalInput(), &TriggerOrigin{OccurrenceID: "occ_late", TriggerID: tr.ID, TriggerRevision: tr.Revision, Kind: "schedule", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)})
	must(t, e)
	_, e = w.s.dispatchNext(context.Background())
	must(t, e)
	if status, code, _, _ := diagnostics(w, expired.ID, ""); status != 404 || code != "no_diagnostics: This run expired in the queue and was never evaluated, so it kept no diagnostics." {
		t.Fatalf("an expired run: %d %s", status, code)
	}
	for query, want := range map[string]int{"?stream=audit": 400, "?stream=stderr&stream=stdout": 400, "?stream=%zz": 400} {
		if status, code, _, _ := diagnostics(w, failed.ID, query); status != want || !strings.HasPrefix(code, "invalid_stream") {
			t.Errorf("%s: %d %s", query, status, code)
		}
	}
	if status, code, _, _ := diagnostics(w, "run_unknown", ""); status != 404 || !strings.HasPrefix(code, "not_found") {
		t.Fatalf("an unknown run: %d %s", status, code)
	}
}

// The route serves a stream's first 64 KiB, and says how large the stream
// is and that it was cut; a cut never ends inside a character.
func TestARunsDiagnosticsAreServedUpToTheirBound(t *testing.T) {
	w := newCoverWorld(t)
	long := runOn(w, fakeRuntime(t, w.cfg.Dir, "head -c 102400 /dev/zero | tr '\\0' x >&2\nexit 3"))
	status, _, h, body := diagnostics(w, long.ID, "")
	if status != 200 || len(body) != maxDiagnostics || maxDiagnostics != 65536 || h["X-Diagnostics-Bytes"] != "102400" || h["X-Diagnostics-Truncated"] != "true" || !bytes.Equal(body, bytes.Repeat([]byte("x"), maxDiagnostics)) {
		t.Fatalf("%d %v %d bytes", status, h, len(body))
	}
	// A two-byte character across the bound is left out whole.
	straddling := runOn(w, fakeRuntime(t, w.cfg.Dir, "{ head -c 65535 /dev/zero | tr '\\0' x; printf '\\303\\251'; head -c 100 /dev/zero | tr '\\0' y; } >&2\nexit 3"))
	status, _, h, body = diagnostics(w, straddling.ID, "")
	if status != 200 || len(body) != maxDiagnostics-1 || h["X-Diagnostics-Bytes"] != "65637" || h["X-Diagnostics-Truncated"] != "true" || body[len(body)-1] != 'x' {
		t.Fatalf("%d %v %d bytes, ending %q", status, h, len(body), body[len(body)-3:])
	}
	exact := runOn(w, fakeRuntime(t, w.cfg.Dir, "head -c 65536 /dev/zero | tr '\\0' x >&2\nexit 3"))
	if status, _, h, body = diagnostics(w, exact.ID, ""); status != 200 || len(body) != maxDiagnostics || h["X-Diagnostics-Truncated"] != "false" {
		t.Fatalf("a stream of exactly the bound: %d %v %d", status, h, len(body))
	}
}

// An automatic run whose queue time ran out keeps the code queue-expired as
// its reason, beside its state, failed, and its problem text: served with the
// run and in run lists, and not in a verification export. A run that failed
// in evaluation has no reason.
func TestAnExpiredQueueEntryHasItsOwnReason(t *testing.T) {
	w := newCoverWorld(t)
	tr := w.scheduleTrigger("")
	expired, _, e := w.s.submitInternal(w.job.ID, "late", journalInput(), &TriggerOrigin{OccurrenceID: "occ_late", TriggerID: tr.ID, TriggerRevision: tr.Revision, Kind: "schedule", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)})
	must(t, e)
	_, e = w.s.dispatchNext(context.Background())
	must(t, e)
	expired, e = w.s.run(expired.ID)
	must(t, e)
	if expired.State != "failed" || expired.Reason != "queue-expired" || expired.Problem == "" || expired.StartedAt != "" {
		t.Fatalf("%+v", expired)
	}
	got := serve(t, w.s, "GET", "/v1/runs/"+expired.ID, "", nil)
	var served map[string]json.RawMessage
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &served) != nil || string(served["reason"]) != `"queue-expired"` || string(served["state"]) != `"failed"` {
		t.Fatalf("the run is served %s", got.Body)
	}
	if list := serve(t, w.s, "GET", "/v1/jobs/"+w.job.ID+"/runs", "", nil); list.Code != 200 || !bytes.Contains(list.Body.Bytes(), []byte(`"reason":"queue-expired"`)) {
		t.Fatalf("the run list: %s", list.Body)
	}
	if exported := encode(verificationExport(w.release, expired, 2)); bytes.Contains(exported, []byte(`"reason"`)) {
		t.Fatal("a version-2 export carries the reason")
	}
	failed := runOn(w, failingRuntime(t, w.cfg.Dir))
	if got := serve(t, w.s, "GET", "/v1/runs/"+failed.ID, "", nil); failed.State != "failed" || failed.Reason != "" || bytes.Contains(got.Body.Bytes(), []byte(`"reason"`)) {
		t.Fatalf("a run that failed in evaluation: %+v %s", failed, got.Body)
	}
}
