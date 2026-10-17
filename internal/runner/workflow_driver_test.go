package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func workflowDriverFixture(t *testing.T, sources int) (workflowInventory, Input, []InputProfile, time.Time) {
	t.Helper()
	f := newImportFixture(t)
	for len(f.direct.Sources) < sources {
		next := f.direct.Sources[0]
		next.Name = "second"
		f.direct.Sources = append(f.direct.Sources, next)
		// Distinct targets avoid overlap; the second request remains independent.
		next.Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/other", Source: "/description"}}, Evidence: []EvidenceMapping{}}}
		f.direct.Sources[len(f.direct.Sources)-1] = next
	}
	profiles := []InputProfile{f.profile}
	raw, in, runtime := workflowFreezeFixture(t, encode(f.direct), nil, profiles)
	doc := decoded(t, raw).(map[string]any)
	doc["limits"].(map[string]any)["maxAttempts"] = 3
	w, err := newWorkflowInventory(encode(doc), f.input.Source.Case, "driver", []workflowFreezeInput{in}, profiles, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return w, f.input, profiles, f.at
}
func workflowDriverStore(t *testing.T, w workflowInventory) *workflowStore {
	t.Helper()
	store, db := openWorkflowStoreTest(t, filepath.Join(t.TempDir(), "workflow.sqlite"))
	t.Cleanup(func() { db.Close() })
	if err := store.create(w); err != nil {
		t.Fatal(err)
	}
	return store
}
func retainedWorkflowRun(t *testing.T, w workflowInventory) Run {
	t.Helper()
	a := w.Execution.Instances[0].Attempts
	if len(a) == 0 || a[len(a)-1].RunDigest == nil {
		t.Fatal("missing retained attempt")
	}
	var bundle VerificationBundle
	if err := strictJSON(w.Artifacts["run/"+*a[len(a)-1].RunDigest], &bundle); err != nil {
		t.Fatal(err)
	}
	return bundle.Run
}

func TestWorkflowDriverCheckpointBeforeNextSourceAndUncertainRecovery(t *testing.T) {
	w, input, profiles, at := workflowDriverFixture(t, 2)
	store := workflowDriverStore(t, w)
	calls := 0
	driver := workflowDriver{Store: store, Profiles: profiles, Now: func() time.Time { return at }}
	driver.Acquire = func(ctx context.Context, request workflowAcquisition) (SourceValue, error) {
		calls++
		held, _, err := store.load(w.Execution.Execution)
		if err != nil || held.Pending["instance-1"] != 1 {
			t.Fatal("acquired before durable admission", err)
		}
		run := retainedWorkflowRun(t, held)
		if calls == 1 {
			if len(run.Input.Source.Sources) != 0 {
				t.Fatal("reused sample answer")
			}
			// Callback mutation must not alter driver-owned case/mapping.
			request.Input.Source.Case = []byte(`{"id":999}`)
			return input.Source.Sources["vendor"], nil
		}
		if _, ok := run.Input.Source.Sources["vendor"]; !ok {
			t.Fatal("second request before first receipt committed")
		}
		panic("simulated process interruption")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("missing interruption")
			}
		}()
		_, _ = driver.run(context.Background(), w.Execution.Execution, "instance-1", nil)
	}()
	held, revision, err := store.load(w.Execution.Execution)
	if err != nil || held.Pending["instance-1"] != 1 || calls != 2 {
		t.Fatal(err, calls)
	}
	recovered := recoverWorkflowAttempts(held)
	if err = store.save(recovered, revision); err != nil {
		t.Fatal(err)
	}
	if _, err = driver.run(context.Background(), w.Execution.Execution, "instance-1", nil); err == nil || calls != 2 {
		t.Fatal("silently retried uncertain acquisition", err)
	}
	if len(retainedWorkflowRun(t, recovered).Input.Source.Sources) != 1 {
		t.Fatal("lost received answer")
	}
	driver.Acquire = func(_ context.Context, request workflowAcquisition) (SourceValue, error) {
		calls++
		if request.Attempt != 2 || len(request.Input.Source.Sources) != 0 {
			t.Fatal("retry reused old answers")
		}
		return SourceValue{}, errWorkflowAcquisitionUnavailable
	}
	retried, err := driver.run(context.Background(), w.Execution.Execution, "instance-1", &workflowRetry{Actor: "operator", At: at.Format("2006-01-02T15:04:05Z")})
	if err != nil || calls != 3 || len(retried.Execution.Instances[0].Attempts) != 2 || retried.Execution.Instances[0].State != "failed" {
		t.Fatal(err, calls, retried.Execution.Instances)
	}
	if *retried.Execution.Instances[0].Attempts[0].RunDigest != *held.Execution.Instances[0].Attempts[0].RunDigest {
		t.Fatal("retry rewrote previous attempt")
	}
}

func TestWorkflowDriverWriteFailureStopsBeforeNextAcquisition(t *testing.T) {
	w, input, profiles, at := workflowDriverFixture(t, 2)
	store := workflowDriverStore(t, w)
	calls := 0
	driver := workflowDriver{Store: store, Profiles: profiles, Now: func() time.Time { return at }}
	driver.Acquire = func(context.Context, workflowAcquisition) (SourceValue, error) {
		calls++
		if _, err := store.db.Exec(`CREATE TRIGGER refuse_update BEFORE UPDATE ON workflow_executions BEGIN SELECT RAISE(ABORT,'test storage failure'); END`); err != nil {
			t.Fatal(err)
		}
		return input.Source.Sources["vendor"], nil
	}
	if _, err := driver.run(context.Background(), w.Execution.Execution, "instance-1", nil); err == nil || calls != 1 {
		t.Fatal("continued after failed checkpoint", err, calls)
	}
	held, _, err := store.load(w.Execution.Execution)
	if err != nil || held.Pending["instance-1"] != 1 || len(retainedWorkflowRun(t, held).Input.Source.Sources) != 0 {
		t.Fatal("failed write fabricated retained answer", err)
	}
}

func TestWorkflowDriverClassifiesUnavailableUncertainCancelledAndPreparationFailures(t *testing.T) {
	for _, kind := range []string{"unavailable", "uncertain", "cancelled", "invalid-response"} {
		t.Run(kind, func(t *testing.T) {
			w, input, profiles, at := workflowDriverFixture(t, 1)
			store := workflowDriverStore(t, w)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := 0
			driver := workflowDriver{Store: store, Profiles: profiles, Now: func() time.Time { return at }, Evaluate: func(context.Context, Release, Input, string) (workflowEvaluation, error) {
				t.Fatal("evaluated failed acquisition")
				return workflowEvaluation{}, nil
			}}
			driver.Acquire = func(context.Context, workflowAcquisition) (SourceValue, error) {
				called++
				switch kind {
				case "unavailable":
					return SourceValue{}, errWorkflowAcquisitionUnavailable
				case "uncertain":
					return input.Source.Sources["vendor"], errors.New("connection lost after response")
				case "cancelled":
					cancel()
					return input.Source.Sources["vendor"], context.Canceled
				default:
					return SourceValue{Response: []byte(`{"invalid":true}`)}, nil
				}
			}
			done, err := driver.run(ctx, w.Execution.Execution, "instance-1", nil)
			if err != nil || called != 1 || len(done.Pending) != 0 {
				t.Fatal(err, called)
			}
			state := done.Execution.Instances[0].State
			expected := map[string]string{"unavailable": "failed", "uncertain": "interrupted", "cancelled": "cancelled", "invalid-response": "failed"}[kind]
			if state != expected || len(done.Execution.OutputSets[0].Records) != 0 {
				t.Fatal(done.Execution)
			}
			if kind == "invalid-response" && *done.Execution.Instances[0].Failure != "preparation-failed" {
				t.Fatal(done.Execution)
			}
			if kind != "unavailable" && len(retainedWorkflowRun(t, done).Input.Source.Sources) != 1 {
				t.Fatal("discarded available response")
			}
		})
	}
}

func TestWorkflowCheckpointCannotReplaceAnswers(t *testing.T) {
	w, input, profiles, _ := workflowDriverFixture(t, 1)
	active, err := beginWorkflowAttempt(w, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	var release Release
	_ = strictJSON(w.Artifacts["release/"+w.Execution.Frozen[0].ReleaseDigest], &release)
	source, err := workflowAttemptInput(active, "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	source.Source.Sources = input.Source.Sources
	run := Run{SchemaVersion: "1", ID: "one", ReleaseID: release.ID, State: "interrupted", Input: source, Attempt: 1}
	held, err := checkpointWorkflowAttempt(active, "instance-1", encode(verificationExport(release, run, 3)), profiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Run){
		func(r *Run) { delete(r.Input.Source.Sources, "vendor") },
		func(r *Run) { r.Input.Source.Sources["vendor"] = SourceValue{Response: []byte(`{"replaced":true}`)} },
		func(r *Run) { r.Input.Source.Sources["unknown"] = SourceValue{Response: []byte(`{}`)} },
		func(r *Run) { r.ID = "other" },
	} {
		current := run
		current.Input = workflowOwnedInput(run.Input)
		change(&current)
		if _, err := checkpointWorkflowAttempt(held, "instance-1", encode(verificationExport(release, current, 3)), profiles); err == nil {
			t.Fatal("replaced checkpoint")
		}
	}
}

func TestWorkflowDriverRealRuntimeAndPreparedCheckpoint(t *testing.T) {
	bundle, profiles, _ := completedExport(t, "asserted")
	raw, in, _ := workflowFreezeFixture(t, encode(*bundle.Release.InputMapping), nil, profiles)
	in.Release = bundle.Release
	doc := decoded(t, raw).(map[string]any)
	stage := doc["stages"].([]any)[0].(map[string]any)
	stage["pack"] = map[string]any{"id": bundle.Release.PackID, "digest": bundle.Release.PackDigest}
	stage["outputs"] = map[string]any{"pointer": "/request", "cardinality": "one"}
	w, err := newWorkflowInventory(encode(doc), bundle.Run.Input.Source.Case, "real-driver", []workflowFreezeInput{in}, profiles, bundle.Release.RuntimeDigest)
	if err != nil {
		t.Fatal(err)
	}
	store := workflowDriverStore(t, w)
	driver := workflowDriver{Store: store, Profiles: profiles}
	directory := t.TempDir()
	driver.Evaluate = func(ctx context.Context, release Release, input Input, key string) (workflowEvaluation, error) {
		held, _, err := store.load(w.Execution.Execution)
		if err != nil || held.Execution.Instances[0].Attempts[0].PreparationDigest == nil {
			t.Fatal("evaluation before prepared checkpoint", err)
		}
		result, audit, signatures, _, err := evaluateWith(ctx, os.Getenv("JPACK_TEST_BIN"), release, input, filepath.Join(directory, key), false, signingInputs{})
		if err != nil {
			return workflowEvaluation{}, err
		}
		return workflowEvaluation{Result: result, Audit: audit, Signatures: signatures}, errSidecarUnread
	}
	done, err := driver.run(context.Background(), w.Execution.Execution, "instance-1", nil)
	if err != nil || done.Execution.Instances[0].State != "completed" || len(done.Execution.OutputSets[0].Records) != 1 {
		t.Fatal(err, done.Execution)
	}
	held, _, err := store.load(w.Execution.Execution)
	if err != nil || !bytes.Equal(encode(held.Execution), encode(done.Execution)) {
		t.Fatal("returned stale pre-commit state", err)
	}
}

func TestWorkflowDriverCopiesReusedSourceBuffer(t *testing.T) {
	w, input, profiles, at := workflowDriverFixture(t, 2)
	store := workflowDriverStore(t, w)
	buffer := bytes.Clone(input.Source.Sources["vendor"].Response)
	calls := 0
	driver := workflowDriver{Store: store, Profiles: profiles, Now: func() time.Time { return at }, Acquire: func(context.Context, workflowAcquisition) (SourceValue, error) {
		calls++
		if calls == 1 {
			return SourceValue{Response: buffer}, nil
		}
		for i := range buffer {
			buffer[i] = 'x'
		}
		return SourceValue{}, errors.New("uncertain second call")
	}}
	done, err := driver.run(context.Background(), w.Execution.Execution, "instance-1", nil)
	if err != nil || calls != 2 || done.Execution.Instances[0].State != "interrupted" {
		t.Fatal(err, calls)
	}
	run := retainedWorkflowRun(t, done)
	if !sameJSON(run.Input.Source.Sources["vendor"].Response, input.Source.Sources["vendor"].Response) {
		t.Fatal("callback rewrote earlier answer")
	}
}

func TestWorkflowSignatureCustodyIsPerAttempt(t *testing.T) {
	bundle, profiles, _ := completedExport(t, "asserted")
	raw, in, _ := workflowFreezeFixture(t, encode(*bundle.Release.InputMapping), nil, profiles)
	in.Release = bundle.Release
	doc := decoded(t, raw).(map[string]any)
	doc["limits"].(map[string]any)["maxAttempts"] = 2
	stage := doc["stages"].([]any)[0].(map[string]any)
	stage["pack"] = map[string]any{"id": bundle.Release.PackID, "digest": bundle.Release.PackDigest}
	stage["outputs"] = map[string]any{"pointer": "/missing", "cardinality": "many"}
	w, err := newWorkflowInventory(encode(doc), bundle.Run.Input.Source.Case, "sidecar-attempts", []workflowFreezeInput{in}, profiles, bundle.Release.RuntimeDigest)
	if err != nil {
		t.Fatal(err)
	}
	first, err := beginWorkflowAttempt(w, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	done, err := finishWorkflowAttempt(first, "instance-1", workflowAttemptResult{Run: encode(bundle), State: "completed"}, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if done.Execution.Instances[0].State != "failed" || *done.Execution.Instances[0].Failure != "output-invalid" {
		t.Fatal(done.Execution)
	}
	second, err := beginWorkflowAttempt(done, "instance-1", &workflowRetry{Actor: "reviewer", At: "2026-10-17T01:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	// The same audit bytes are deliberately supplied to both failed attempts.
	// This tests custody only; signature verification is a separate reader.
	sidecar := []byte("{\"fixture\":\"signature custody only\"}\n")
	held, err := finishWorkflowAttempt(second, "instance-1", workflowAttemptResult{Run: encode(bundle), State: "completed", Signatures: sidecar}, profiles)
	if err != nil {
		t.Fatal(err)
	}
	files, err := held.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["instances/instance-1/attempts/1/audit-signatures.json"]; ok {
		t.Fatal("later signature retroactively assigned to earlier attempt")
	}
	if !bytes.Equal(files["instances/instance-1/attempts/2/audit-signatures.json"], sidecar) {
		t.Fatal("lost exact sidecar")
	}
}
