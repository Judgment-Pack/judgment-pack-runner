package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
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

	pid := filepath.Join(t.TempDir(), "pid")
	r := journalRelease(t, w.s, fakeRuntime(t, w.cfg.Dir, "echo 'written while running' >&2\necho $$ > '"+pid+"'\nexec sleep 30"))
	j, e := w.s.createJob("Waits", r.ID)
	must(t, e)
	running, _, e := w.s.submit(j.ID, "running", journalInput())
	must(t, e)
	stop, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := w.s.dispatchNext(stop); done <- e }()
	waitForRunState(t, w.s, running.ID, "running")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, e := os.ReadFile(filepath.Join(w.cfg.Dir, "attempts", running.ID, "evaluation.stderr")); e == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the running evaluation wrote no stderr")
		}
	}
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
	if status, code, _, _ := diagnostics(w, expired.ID, ""); status != 404 || code != "no_diagnostics: No stderr is retained for this run, whose state is failed: it expired in the queue (queue-expired) and never started." {
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
	stdout := runOn(w, fakeRuntime(t, w.cfg.Dir, "head -c 65537 /dev/zero | tr '\\0' o\nexit 3"))
	if status, _, h, body = diagnostics(w, stdout.ID, "?stream=stdout"); status != 200 || len(body) != maxDiagnostics || h["X-Diagnostics-Bytes"] != "65537" || h["X-Diagnostics-Truncated"] != "true" || !bytes.Equal(body, bytes.Repeat([]byte("o"), maxDiagnostics)) {
		t.Fatalf("stdout: %d %v %d bytes", status, h, len(body))
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

// stored is a run whose stderr the Runtime wrote, and the path of its attempt
// directory.
func stored(w *coverWorld, stderr string) (Run, string) {
	w.t.Helper()
	run := runOn(w, talkingRuntime(w.t, w.cfg.Dir, stderr, "{}"))
	return run, filepath.Join(w.cfg.Dir, "attempts", run.ID)
}

const diagnosticsSecrets = "SECRET"

func refusedUnread(w *coverWorld, run string) {
	w.t.Helper()
	status, code, _, body := diagnostics(w, run, "")
	if status != 500 || !strings.HasPrefix(code, "diagnostics_refused") || bytes.Contains(body, []byte(diagnosticsSecrets)) {
		w.t.Fatalf("%d %s %q", status, code, body)
	}
}

// Diagnostics are read only from the run's own attempt directory and its own
// private files: a directory or a stream replaced by a link, to another
// run's, to a file beside it or to one outside the store, and a stream or a
// directory others may read, are refused, and nothing of them is served.
func TestDiagnosticsAreReadOnlyFromTheRunsOwnPrivateFiles(t *testing.T) {
	cases := map[string]func(w *coverWorld, dir, other string){
		"its directory, a link to another run's": func(w *coverWorld, dir, other string) {
			must(w.t, os.RemoveAll(dir))
			must(w.t, os.Symlink(other, dir))
		},
		"its directory, a link outside the store": func(w *coverWorld, dir, _ string) {
			outside := w.t.TempDir()
			must(w.t, os.WriteFile(filepath.Join(outside, "evaluation.stderr"), []byte("OUTSIDE-SECRET"), 0600))
			must(w.t, os.RemoveAll(dir))
			must(w.t, os.Symlink(outside, dir))
		},
		"its stream, a link to a file beside it": func(w *coverWorld, dir, _ string) {
			must(w.t, os.WriteFile(filepath.Join(dir, "beside"), []byte("BESIDE-SECRET"), 0600))
			must(w.t, os.Remove(filepath.Join(dir, "evaluation.stderr")))
			must(w.t, os.Symlink("beside", filepath.Join(dir, "evaluation.stderr")))
		},
		"its stream, a link to another run's": func(w *coverWorld, dir, other string) {
			must(w.t, os.Remove(filepath.Join(dir, "evaluation.stderr")))
			must(w.t, os.Symlink(filepath.Join(other, "evaluation.stderr"), filepath.Join(dir, "evaluation.stderr")))
		},
		"its stream, a link outside the store": func(w *coverWorld, dir, _ string) {
			outside := filepath.Join(w.t.TempDir(), "outside")
			must(w.t, os.WriteFile(outside, []byte("OUTSIDE-SECRET"), 0600))
			must(w.t, os.Remove(filepath.Join(dir, "evaluation.stderr")))
			must(w.t, os.Symlink(outside, filepath.Join(dir, "evaluation.stderr")))
		},
		"its stream, readable by others": func(w *coverWorld, dir, _ string) {
			must(w.t, os.Chmod(filepath.Join(dir, "evaluation.stderr"), 0644))
		},
		"its directory, readable by others": func(w *coverWorld, dir, _ string) {
			must(w.t, os.Chmod(dir, 0755))
		},
		"its stream, a directory": func(w *coverWorld, dir, _ string) {
			must(w.t, os.Remove(filepath.Join(dir, "evaluation.stderr")))
			must(w.t, os.Mkdir(filepath.Join(dir, "evaluation.stderr"), 0700))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			w := newCoverWorld(t)
			run, dir := stored(w, "the run-own stderr")
			_, other := stored(w, "OTHER-RUN-SECRET")
			if status, _, _, body := diagnostics(w, run.ID, ""); status != 200 || string(body) != "the run-own stderr" {
				t.Fatal(status, string(body))
			}
			change(w, dir, other)
			refusedUnread(w, run.ID)
		})
	}
}

// What a name holds when Runner looks at it is what Runner reads, or it reads
// nothing: a run's directory or stream swapped between the look and the open,
// for a link or for another file, is refused.
func TestASwapBetweenTheLookAndTheOpenIsRefused(t *testing.T) {
	t.Cleanup(func() { diagnosticsHook = nil })
	cases := map[string]struct {
		stage string
		swap  func(w *coverWorld, dir, other string)
	}{
		"the stream, for a link to a file beside it": {"stream", func(w *coverWorld, dir, _ string) {
			must(w.t, os.WriteFile(filepath.Join(dir, "beside"), []byte("BESIDE-SECRET"), 0600))
			must(w.t, os.Symlink("beside", filepath.Join(dir, "link")))
			must(w.t, os.Rename(filepath.Join(dir, "link"), filepath.Join(dir, "evaluation.stderr")))
		}},
		"the stream, for another file": {"stream", func(w *coverWorld, dir, _ string) {
			must(w.t, os.WriteFile(filepath.Join(dir, "replacement"), []byte("REPLACEMENT-SECRET"), 0600))
			must(w.t, os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "evaluation.stderr")))
		}},
		"the directory, for a link to another run's": {"attempt", func(w *coverWorld, dir, other string) {
			must(w.t, os.Symlink(other, dir+".link"))
			must(w.t, os.Rename(dir, dir+".gone"))
			must(w.t, os.Rename(dir+".link", dir))
		}},
		"the directory, for another run's": {"attempt", func(w *coverWorld, dir, other string) {
			must(w.t, os.Rename(dir, dir+".gone"))
			must(w.t, os.Rename(other, dir))
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newCoverWorld(t)
			run, dir := stored(w, "the run-own stderr")
			_, other := stored(w, "OTHER-RUN-SECRET")
			if status, _, _, body := diagnostics(w, run.ID, ""); status != 200 || string(body) != "the run-own stderr" {
				t.Fatal(status, string(body))
			}
			diagnosticsHook = func(stage string) {
				if stage == c.stage {
					diagnosticsHook = nil
					c.swap(w, dir, other)
				}
			}
			refusedUnread(w, run.ID)
		})
	}
}

// Under a stream swapped back and forth with a link while it is read, every
// answer is the run's own bytes or a refusal.
func TestAStreamSwappedWhileItIsReadIsServedWhollyOrNotAtAll(t *testing.T) {
	w := newCoverWorld(t)
	run, dir := stored(w, "the run-own stderr")
	stream := filepath.Join(dir, "evaluation.stderr")
	must(t, os.WriteFile(filepath.Join(dir, "beside"), []byte("BESIDE-SECRET"), 0600))
	must(t, os.Link(stream, filepath.Join(dir, "own")))
	stop := make(chan struct{})
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for i := 0; ; i++ {
			select {
			case <-stop:
				os.Remove(filepath.Join(dir, "next"))
				os.Link(filepath.Join(dir, "own"), filepath.Join(dir, "next"))
				os.Rename(filepath.Join(dir, "next"), stream)
				return
			default:
			}
			os.Remove(filepath.Join(dir, "next"))
			if i%2 == 0 {
				os.Symlink("beside", filepath.Join(dir, "next"))
			} else {
				os.Link(filepath.Join(dir, "own"), filepath.Join(dir, "next"))
			}
			os.Rename(filepath.Join(dir, "next"), stream)
		}
	}()
	served, refused := 0, 0
	for i := 0; i < 300; i++ {
		status, code, _, body := diagnostics(w, run.ID, "")
		switch {
		case status == 200 && string(body) == "the run-own stderr":
			served++
		case status == 500 && strings.HasPrefix(code, "diagnostics_refused"), status == 404:
			refused++
		default:
			close(stop)
			<-swapped
			t.Fatalf("answer %d: %d %s %q", i, status, code, body)
		}
	}
	close(stop)
	<-swapped
	t.Logf("%d served whole, %d refused", served, refused)
}

// A run whose diagnostics are gone is said to have none retained, with its
// state and what the store records of it: never that it was not evaluated.
func TestMissingDiagnosticsAreNotTakenForARunNeverEvaluated(t *testing.T) {
	w := newCoverWorld(t)
	run, dir := stored(w, "the run-own stderr")
	must(t, os.Remove(filepath.Join(dir, "evaluation.stderr")))
	want := "no_diagnostics: No stderr is retained for this run, whose state is failed."
	if status, code, _, _ := diagnostics(w, run.ID, ""); status != 404 || code != want {
		t.Fatalf("a stream gone: %d %s", status, code)
	}
	must(t, os.RemoveAll(dir))
	if status, code, _, _ := diagnostics(w, run.ID, ""); status != 404 || code != want {
		t.Fatalf("a directory gone: %d %s", status, code)
	}
	completed := runOn(w, completingRuntime(t, w.cfg.Dir))
	must(t, os.RemoveAll(filepath.Join(w.cfg.Dir, "attempts", completed.ID)))
	if status, code, _, _ := diagnostics(w, completed.ID, "?stream=stdout"); status != 404 || code != "no_diagnostics: No stdout is retained for this run, whose state is completed." {
		t.Fatalf("a completed run's: %d %s", status, code)
	}
}

// Runner serves the file it checked, from the descriptor it checked: a stream
// replaced at its name after the check and before the read is not what is
// served, and a name swapped for a link back to the same file is that file.
func TestTheCheckedDescriptorIsWhatIsRead(t *testing.T) {
	t.Cleanup(func() { diagnosticsHook = nil })
	w := newCoverWorld(t)
	run, dir := stored(w, "the run-own stderr")
	stream := filepath.Join(dir, "evaluation.stderr")
	diagnosticsHook = func(stage string) {
		if stage == "read" {
			diagnosticsHook = nil
			must(t, os.WriteFile(filepath.Join(dir, "replacement"), []byte("AFTER-CHECK-SECRET"), 0600))
			must(t, os.Rename(filepath.Join(dir, "replacement"), stream))
		}
	}
	if status, code, _, body := diagnostics(w, run.ID, ""); status != 200 || string(body) != "the run-own stderr" {
		t.Fatalf("a stream replaced after the check: %d %s %q", status, code, body)
	}

	w = newCoverWorld(t)
	run, dir = stored(w, "the run-own stderr")
	stream = filepath.Join(dir, "evaluation.stderr")
	diagnosticsHook = func(stage string) {
		if stage == "stream" {
			diagnosticsHook = nil
			must(t, os.Rename(stream, stream+".own"))
			must(t, os.Symlink("evaluation.stderr.own", stream))
		}
	}
	if status, code, _, body := diagnostics(w, run.ID, ""); status != 200 || string(body) != "the run-own stderr" {
		t.Fatalf("a link back to the same file: %d %s %q", status, code, body)
	}

	w = newCoverWorld(t)
	run, dir = stored(w, "the run-own stderr")
	diagnosticsHook = func(stage string) {
		if stage == "stream" {
			diagnosticsHook = nil
			must(t, os.Remove(filepath.Join(dir, "evaluation.stderr")))
		}
	}
	if status, code, _, _ := diagnostics(w, run.ID, ""); status != 404 || !strings.HasPrefix(code, "no_diagnostics") {
		t.Fatalf("a stream gone in between: %d %s", status, code)
	}
}
