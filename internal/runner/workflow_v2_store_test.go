package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func workflowV2Children(t *testing.T, f workflowV2RuntimeTest, w workflowV2Inventory) workflowV2Inventory {
	t.Helper()
	next, err := selectWorkflowV2(w, workflowV2Selection(t, w, 0, w.Core.Execution.OutputSets[0].Records, 1), []string{"person-a", "person-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	next, err = beginWorkflowV2Group(next, next.Reservations["person-a"], []workflowV2Begin{{"person-a", 1, nil}, {"person-b", 1, nil}}, []string{}, []workflowV2Cancel{})
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func workflowV2Results(t *testing.T, f workflowV2RuntimeTest, branch string) map[string]workflowV2Result {
	t.Helper()
	rows, err := f.store.custody("v2-execution", branch)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]workflowV2Result{}
	for _, c := range rows {
		if c.Kind == "result" {
			var r workflowV2Result
			if json.Unmarshal(c.Body, &r) != nil {
				t.Fatal("result")
			}
			out[r.Instance] = r
		}
	}
	return out
}
func TestWorkflowV2OutOfOrderCustodyAdmitsCanonicalActualResults(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	root, _ := workflowV2RootResult(t, f)
	begun := workflowV2Children(t, f, root)
	if err := f.store.create("canonical", begun); err != nil {
		t.Fatal(err)
	}
	canonical, err := f.driver.run(context.Background(), "v2-execution", "canonical")
	if err != nil {
		t.Fatal(err)
	}
	results := workflowV2Results(t, f, "canonical")
	if len(results) != 2 {
		t.Fatal("actual results")
	}
	if err = f.store.create("out-of-order", begun); err != nil {
		t.Fatal(err)
	}
	f.driver.Trace = workflowV2JourneyTrace(t, "out-of-order")
	before, _ := begun.recordingFiles()
	deferred, err := f.driver.receive("v2-execution", "out-of-order", results["person-b"])
	if err != nil {
		t.Fatal(err)
	}
	prefix, _ := deferred.recordingFiles()
	if !reflect.DeepEqual(before, prefix) {
		t.Fatal("future material in earlier accounting prefix")
	}
	workflowV2ExportJourney(t, "out-of-order/later-held", deferred, nil)
	if _, present := prefix["instances/person-b/attempts/1/run.json"]; present {
		t.Fatal("charged later run before its turn")
	}
	done, err := f.driver.receive("v2-execution", "out-of-order", results["person-a"])
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := canonical.recordingFiles()
	actual, _ := done.recordingFiles()
	if !reflect.DeepEqual(expected, actual) {
		t.Fatal("arrival order changed recording")
	}
	workflowV2ExportJourney(t, "out-of-order/complete", done, nil)
	e, _ := done.Proof.last()
	if e.Results[0].Instance != "person-a" || e.Results[1].Instance != "person-b" || len(done.Outputs) != 4 {
		t.Fatal("canonical membership/counts")
	}
}
func TestWorkflowV2LaterUnrecordableResultPreservesCustodyAndCommittedPrefix(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	root, rootResult := workflowV2RootResult(t, f)
	begun := workflowV2Children(t, f, root)
	if err := f.store.create("material", begun); err != nil {
		t.Fatal(err)
	}
	if _, err := f.driver.run(context.Background(), "v2-execution", "material"); err != nil {
		t.Fatal(err)
	}
	results := workflowV2Results(t, f, "material")
	build := func(limit int) workflowV2Inventory {
		w := workflowV2StartRoot(t, f.inventory(t, limit))
		var err error
		w, err = observeWorkflowV2(w, rootResult, nil)
		if err != nil {
			t.Fatal(err)
		}
		return workflowV2Children(t, f, w)
	}
	first, err := observeWorkflowV2(begun, results["person-a"], nil)
	if err != nil {
		t.Fatal(err)
	}
	limit := workflowV2Size(t, first) + 100
	limited := build(limit)
	f.driver.Trace = workflowV2JourneyTrace(t, "recording-unavailable")
	if err = f.store.create("bounded", limited); err != nil {
		t.Fatal(err)
	}
	oversized := results["person-b"]
	oversized.Run = append(bytes.Clone(oversized.Run), bytes.Repeat([]byte(" "), 100_000)...)
	if _, err = f.driver.receive("v2-execution", "bounded", oversized); err != nil {
		t.Fatal(err)
	}
	prefix, err := f.driver.receive("v2-execution", "bounded", results["person-a"])
	if !errors.Is(err, errWorkflowV2Recording) {
		t.Fatal("overflow manufactured refusal", err)
	}
	e, _ := prefix.Proof.last()
	if len(e.Results) != 1 || e.Results[0].Instance != "person-a" || prefix.History["person-b"].State != "interrupted" || prefix.Core.Pending["person-b"] != 1 {
		t.Fatal("invalid committed prefix")
	}
	before, _ := prefix.recordingFiles()
	workflowV2ExportJourney(t, "recording-unavailable/last-valid-prefix", prefix, nil)
	if before["instances/person-b/attempts/1/run.json"] != nil {
		t.Fatal("omitted bytes claimed in export")
	}
	held, err := f.store.custody("v2-execution", "bounded")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range held {
		if c.Kind == "result" && c.Instance == "person-b" {
			var r workflowV2Result
			_ = json.Unmarshal(c.Body, &r)
			found = bytes.Equal(r.Run, oversized.Run)
		}
	}
	if !found {
		t.Fatal("lost unrecordable actual material")
	}
	calls := 0
	f.driver.Acquire = func(context.Context, workflowAcquisition) (SourceValue, error) { calls++; return SourceValue{}, nil }
	f.driver.Evaluate = func(context.Context, Release, Input, string) (workflowEvaluation, error) {
		calls++
		return workflowEvaluation{}, nil
	}
	if _, err = f.driver.run(context.Background(), "v2-execution", "bounded"); !errors.Is(err, errWorkflowV2Recording) {
		t.Fatal(err)
	}
	if _, err = f.driver.recover("v2-execution", "bounded"); !errors.Is(err, errWorkflowV2Recording) {
		t.Fatal(err)
	}
	after, _, err := f.store.load("v2-execution", "bounded")
	if err != nil {
		t.Fatal(err)
	}
	files, _ := after.recordingFiles()
	if calls != 0 || !reflect.DeepEqual(before, files) {
		t.Fatal("overflow rewrote/reacquired")
	}
}
func TestWorkflowV2DeferredStartAndEarlierRetryKeepCanonicalSetsAndImmutableHistory(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "deferred", true: "retry"}[retry], func(t *testing.T) {
			f := newWorkflowV2RuntimeTest(t)
			trace := workflowV2JourneyTrace(t, "set-order/"+map[bool]string{false: "deferred", true: "retry"}[retry])
			f.driver.Trace = trace
			f.document.Stages[1].Selection = &workflowSelectionPolicy{Mode: "reviewer"}
			leaf := f.document.Stages[1]
			leaf.ID = "leaf"
			leaf.Input.FromStage = f.document.Stages[1].ID
			leaf.Selection = nil
			f.document.Stages = append(f.document.Stages, leaf)
			f.document.Result = "leaf"
			f.document.Limits.MaxStages = 3
			f.inputs = append(f.inputs, f.inputs[1])
			w, _ := workflowV2RootResult(t, f)
			var err error
			w, err = selectWorkflowV2(w, workflowV2Selection(t, w, 0, w.Core.Execution.OutputSets[0].Records, 1), []string{"person-a", "person-b"}, false)
			if err != nil {
				t.Fatal(err)
			}
			starts := []workflowV2Begin{{"person-b", 1, nil}}
			deferred := []string{"person-a"}
			if retry {
				starts = []workflowV2Begin{{"person-a", 1, nil}, {"person-b", 1, nil}}
				deferred = []string{}
			}
			w, err = beginWorkflowV2Group(w, w.Reservations["person-a"], starts, deferred, []workflowV2Cancel{})
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.create("order", w); err != nil {
				t.Fatal(err)
			}
			realEvaluate := f.driver.Evaluate
			if retry {
				f.driver.Evaluate = func(ctx context.Context, r Release, in Input, key string) (workflowEvaluation, error) {
					if bytes.Contains(in.Source.Case, []byte(`"name":"a"`)) {
						return workflowEvaluation{}, errors.New("uncertain evaluator")
					}
					return realEvaluate(ctx, r, in, key)
				}
			}
			before, err := f.driver.run(context.Background(), "v2-execution", "order")
			if err != nil {
				t.Fatal(err)
			}
			oldFiles, _ := before.recordingFiles()
			oldProof, _ := before.Proof.bytes()
			workflowV2ExportJourney(t, "set-order/"+map[bool]string{false: "deferred", true: "retry"}[retry]+"/before", before, nil)
			_, revision, err := f.store.load("v2-execution", "order")
			if err != nil {
				t.Fatal(err)
			}
			var resumed workflowV2Inventory
			if retry {
				resumed, err = beginWorkflowV2Retry(before, workflowV2Begin{"person-a", 2, &workflowRetry{Actor: "operator", At: "2026-10-17T00:00:00Z"}})
			} else {
				resumed, err = beginWorkflowV2Group(before, before.Reservations["person-a"], []workflowV2Begin{{"person-a", 1, nil}}, []string{}, []workflowV2Cancel{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.save("order", resumed, revision); err != nil {
				t.Fatal(err)
			}
			f.driver.Evaluate = realEvaluate
			after, err := f.driver.run(context.Background(), "v2-execution", "order")
			if err != nil {
				t.Fatal(err)
			}
			records := after.Core.Execution.OutputSets[1].Records
			if len(records) != 2 {
				t.Fatal("outputs")
			}
			for n, id := range []string{"person-a", "person-b"} {
				record, _, err := after.Core.record(records[n])
				if err != nil || record.Lineage.Instance != id || record.Lineage.Index != 0 {
					t.Fatal("completion order leaked into set", err)
				}
			}
			files, _ := after.recordingFiles()
			workflowV2ExportJourney(t, "set-order/"+map[bool]string{false: "deferred", true: "retry"}[retry]+"/after", after, nil)
			setBytes, _ := workflowCanonical(after.Core.outputSet(1), 8<<20)
			if !bytes.Equal(setBytes, files["sets/people.json"]) {
				t.Fatal("set serialization")
			}
			e, _ := after.Proof.last()
			if e.PreviousProofDigest != digest(oldProof) || len(after.History) != 3 || len(after.Outputs) != 4 {
				t.Fatal("history/counts")
			}
			for path, raw := range oldFiles {
				if bytes.HasPrefix([]byte(path), []byte("instances/person-b/")) || bytes.HasPrefix([]byte(path), []byte("outputs/people/person-b/")) {
					if !bytes.Equal(raw, files[path]) {
						t.Fatal("changed earlier immutable bytes")
					}
				}
			}
			selected, err := selectWorkflowV2(after, workflowV2Selection(t, after, 1, records, 1), []string{"leaf-a", "leaf-b"}, false)
			if err != nil {
				t.Fatal(err)
			}
			var selection workflowSelection
			_ = strictJSON(selected.Core.Artifacts["selection/"+*selected.Core.Execution.Selections[1]], &selection)
			if selection.OutputSetDigest != digest(setBytes) || len(selected.History) != 5 || len(selected.Outputs) != 4 {
				t.Fatal("selection/counts")
			}
			workflowV2ExportJourney(t, "set-order/"+map[bool]string{false: "deferred", true: "retry"}[retry]+"/selected", selected, nil)
		})
	}
}
func TestWorkflowV2RecoveryNeverAcquiresAndLateResultsCannotRewriteSealedEpoch(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	_, actual := workflowV2RootResult(t, f)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	f.driver.Trace = workflowV2JourneyTrace(t, "interrupted-recovery")
	if err := f.store.create("crash", w); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.driver.Evaluate = func(context.Context, Release, Input, string) (workflowEvaluation, error) {
		calls++
		panic("crash before evaluation reply")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no interruption")
			}
		}()
		_, _ = f.driver.run(context.Background(), "v2-execution", "crash")
	}()
	f.driver.Evaluate = func(context.Context, Release, Input, string) (workflowEvaluation, error) {
		t.Fatal("recovered by evaluation")
		return workflowEvaluation{}, nil
	}
	recovered, err := f.driver.recover("v2-execution", "crash")
	if err != nil || !recovered.Proof.closed() || recovered.History["root-a"].State != "interrupted" || calls != 1 {
		t.Fatal(err)
	}
	attempt := recovered.History["root-a"].Attempts[0]
	if attempt.PreparationDigest == nil || attempt.AuditDigest != nil {
		t.Fatal("lost prepared checkpoint")
	}
	before, _ := recovered.recordingFiles()
	workflowV2ExportJourney(t, "interrupted-recovery/closed", recovered, nil)
	late, err := f.driver.receive("v2-execution", "crash", actual)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := late.recordingFiles()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("late reply rewrote sealed epoch")
	}
	if _, err = f.driver.run(context.Background(), "v2-execution", "crash"); err != nil {
		t.Fatal(err)
	}
	held, err := f.store.custody("v2-execution", "crash")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, c := range held {
		if c.Kind == "result" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("late material not retained")
	}
}
func TestWorkflowV2StoreCASImmutableCustodyAndNoUnprovedStateRewrite(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	_, result := workflowV2RootResult(t, f)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	if err := f.store.create("cas", w); err != nil {
		t.Fatal(err)
	}
	next, err := observeWorkflowV2(w, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.save("cas", next, 1); err != nil {
		t.Fatal(err)
	}
	if err = f.store.save("cas", w, 1); !errors.Is(err, errWorkflowRevision) {
		t.Fatal("stale overwrite", err)
	}
	changed := next.copy()
	i, _ := changed.instance("root-a")
	i.State = "interrupted"
	changed.update(i)
	if err = f.store.save("cas", changed, 2); err == nil {
		t.Fatal("state rewritten without proof transition")
	}
	loaded, revision, err := f.store.load("v2-execution", "cas")
	if err != nil || revision != 2 || loaded.History["root-a"].State != "completed" {
		t.Fatal(err, revision)
	}
}

func TestWorkflowV2EvaluationCustodyRecoversCompleteReplyAfterResultWriteFailure(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	if err := f.store.create("evaluation-custody", w); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`CREATE TRIGGER refuse_v2_result BEFORE INSERT ON workflow_v2_custody WHEN NEW.kind='result' BEGIN SELECT RAISE(ABORT,'test result write failure'); END`); err != nil {
		t.Fatal(err)
	}
	real := f.driver.Evaluate
	calls := 0
	f.driver.Evaluate = func(ctx context.Context, r Release, in Input, key string) (workflowEvaluation, error) {
		calls++
		return real(ctx, r, in, key)
	}
	prefix, err := f.driver.run(context.Background(), "v2-execution", "evaluation-custody")
	var custodyErr *workflowV2CustodyError
	if !errors.As(err, &custodyErr) || custodyErr.Kind != "result" || len(custodyErr.Body) == 0 || prefix.Proof.closed() || calls != 1 {
		t.Fatal("lost failure custody", err)
	}
	var pending workflowV2Result
	if json.Unmarshal(custodyErr.Body, &pending) != nil || len(pending.Run) == 0 {
		t.Fatal("caller has no owned received result")
	}
	held, err := f.store.custody("v2-execution", "evaluation-custody")
	if err != nil {
		t.Fatal(err)
	}
	evaluations := 0
	var evaluation workflowV2EvaluationCustody
	for _, c := range held {
		if c.Kind == "evaluation" {
			evaluations++
			if json.Unmarshal(c.Body, &evaluation) != nil {
				t.Fatal("evaluation bytes")
			}
		}
	}
	if evaluations != 1 || len(evaluation.Evaluation.Audit) == 0 {
		t.Fatal("complete Runtime reply not durable")
	}
	if _, err = f.store.db.Exec(`DROP TRIGGER refuse_v2_result`); err != nil {
		t.Fatal(err)
	}
	f.driver.Evaluate = func(context.Context, Release, Input, string) (workflowEvaluation, error) {
		t.Fatal("replayed received evaluation")
		return workflowEvaluation{}, nil
	}
	done, err := f.driver.recover("v2-execution", "evaluation-custody")
	if err != nil || !done.Proof.closed() || done.History["root-a"].State != "completed" {
		t.Fatal(err)
	}
	files, _ := done.recordingFiles()
	if !bytes.Equal(files["instances/root-a/attempts/1/audit.json"], recordLine(evaluation.Evaluation.Audit)) {
		t.Fatal("recovery changed Runtime audit")
	}
}
func TestWorkflowV2CompleteRuntimeReplyWinsOverLaterCancellation(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	if err := f.store.create("cancel-after-reply", w); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	real := f.driver.Evaluate
	f.driver.Evaluate = func(ctx context.Context, r Release, in Input, key string) (workflowEvaluation, error) {
		answer, err := real(ctx, r, in, key)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		return answer, context.Canceled
	}
	done, err := f.driver.run(ctx, "v2-execution", "cancel-after-reply")
	if err != nil || done.History["root-a"].State != "completed" || len(done.Outputs) != 2 {
		t.Fatal("concealed complete evaluation", err)
	}
}

func TestWorkflowV2EarlierRetryCannotRemeasureLaterSiblingRefusal(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	f.root = encode(map[string]any{"items": []any{map[string]any{"items": []any{map[string]any{"name": "a"}}}, map[string]any{"items": []any{map[string]any{"name": "b", "detail": string(bytes.Repeat([]byte("x"), 32000))}}}}})
	root, rootResult := workflowV2RootResult(t, f)
	begun := workflowV2Children(t, f, root)
	if err := f.store.create("retry-material", begun); err != nil {
		t.Fatal(err)
	}
	realEvaluate := f.driver.Evaluate
	f.driver.Evaluate = func(ctx context.Context, r Release, in Input, key string) (workflowEvaluation, error) {
		if bytes.Contains(in.Source.Case, []byte(`"name":"a"`)) {
			return workflowEvaluation{}, errors.New("uncertain evaluator")
		}
		return realEvaluate(ctx, r, in, key)
	}
	if _, err := f.driver.run(context.Background(), "v2-execution", "retry-material"); err != nil {
		t.Fatal(err)
	}
	results := workflowV2Results(t, f, "retry-material")
	var comparison workflowV2Comparison
	build := func(limit int) workflowV2Inventory {
		w := workflowV2StartRoot(t, f.inventory(t, limit))
		var err error
		w, err = observeWorkflowV2(w, rootResult, nil)
		if err != nil {
			t.Fatal(err)
		}
		w = workflowV2Children(t, f, w)
		w, err = observeWorkflowV2(w, results["person-a"], nil)
		if err != nil {
			t.Fatal(err)
		}
		w.Trace = func(c workflowV2Comparison) { comparison = c }
		w, err = observeWorkflowV2(w, results["person-b"], nil)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	sum := func(files map[string][]byte) int {
		size := 0
		for _, raw := range files {
			size += len(raw)
		}
		return size
	}
	build(1 << 30)
	limit := sum(comparison.Acceptance) - 1
	for n := 0; n < 8; n++ {
		build(limit)
		corrected := sum(comparison.Acceptance) - 1
		if corrected == limit {
			break
		}
		limit = corrected
	}
	refused := build(limit)
	refusalTrace := comparison
	oldProof, _ := refused.Proof.bytes()
	oldSibling := encode(refused.History["person-b"])
	if refusalTrace.Accepted || refusalTrace.Cause != "aggregate-bound" || sum(refusalTrace.Acceptance) != limit+1 || sum(refusalTrace.Refusal) > limit {
		t.Fatal("not a proved byte refusal")
	}
	trace := workflowV2JourneyTrace(t, "earlier-retry-after-refusal")
	trace(refusalTrace)
	refused.Trace = trace
	workflowV2ExportJourney(t, "earlier-retry-after-refusal/refused", refused, nil)
	if err := f.store.create("retry-bound", refused); err != nil {
		t.Fatal(err)
	}
	retry, err := beginWorkflowV2Retry(refused, workflowV2Begin{"person-a", 2, &workflowRetry{Actor: "operator", At: "2026-10-17T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.save("retry-bound", retry, 1); err != nil {
		t.Fatal(err)
	}
	f.driver.Evaluate = realEvaluate
	f.driver.Trace = trace
	done, err := f.driver.run(context.Background(), "v2-execution", "retry-bound")
	if err != nil {
		t.Fatal(err)
	}
	if done.History["person-a"].State != "completed" || !bytes.Equal(oldSibling, encode(done.History["person-b"])) {
		t.Fatal("retry rewrote failed sibling")
	}
	e, _ := done.Proof.last()
	if e.PreviousProofDigest != digest(oldProof) || e.Kind != "retry" || len(done.Outputs) != 3 {
		t.Fatal("retry history")
	}
	if !bytes.Equal(refusalTrace.Proof, oldProof) || sum(refusalTrace.Acceptance) != limit+1 {
		t.Fatal("retroactive baseline")
	}
	workflowV2ExportJourney(t, "earlier-retry-after-refusal/retried", done, nil)
}

func TestWorkflowV2ConcurrentDriversCannotInvokeTheSameBegunAttempt(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	if err := f.store.create("contended", w); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var releaseOnce sync.Once
	stop := func() { releaseOnce.Do(func() { close(release) }) }
	waited := false
	t.Cleanup(func() {
		stop()
		if !waited {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("driver did not stop after releasing the test gate")
			}
		}
	})
	real := f.driver.Evaluate
	var calls atomic.Int32
	var stamps atomic.Int32
	at := time.Now().UTC().Truncate(time.Second)
	// The first clock read is after drive ownership but before the initial
	// checkpoint. Hold that exact former check-empty/write race window open.
	f.driver.Now = func() time.Time {
		if stamps.Add(1) == 1 {
			close(entered)
			<-release
		}
		return at
	}
	f.driver.Evaluate = func(ctx context.Context, r Release, in Input, key string) (workflowEvaluation, error) {
		calls.Add(1)
		return real(ctx, r, in, key)
	}
	first := f.driver
	go func() { _, err := first.run(context.Background(), "v2-execution", "contended"); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first driver did not enter its pre-checkpoint clock read")
	}
	held, err := f.store.custody("v2-execution", "contended")
	if err != nil || len(held) != 0 {
		stop()
		t.Fatal("test did not hold the empty-custody window", err)
	}
	second := f.driver
	if _, err := second.run(context.Background(), "v2-execution", "contended"); err == nil {
		stop()
		t.Fatal("second driver entered same attempt")
	}
	stop()
	err = <-done
	waited = true
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate host invocation", calls.Load())
	}
	var claims int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_drives WHERE execution='v2-execution' AND branch='contended'`).Scan(&claims); err != nil || claims != 1 {
		t.Fatal("durable first-drive claim", err)
	}
}
func TestWorkflowV2CrashAfterDriveClaimRequiresRecoveryAndExplicitRetry(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	if err := f.store.create("claimed", w); err != nil {
		t.Fatal(err)
	}
	if err := f.store.claimDrive("v2-execution", "claimed", "root-a", 1); err != nil {
		t.Fatal(err)
	} // Simulated crash before any checkpoint/call.
	real := f.driver.Evaluate
	calls := 0
	f.driver.Evaluate = func(ctx context.Context, r Release, in Input, key string) (workflowEvaluation, error) {
		calls++
		return real(ctx, r, in, key)
	}
	if _, err := f.driver.run(context.Background(), "v2-execution", "claimed"); err == nil || calls != 0 {
		t.Fatal("replayed claimed uncertainty")
	}
	recovered, err := f.driver.recover("v2-execution", "claimed")
	if err != nil || calls != 0 || recovered.History["root-a"].State != "interrupted" || recovered.History["root-a"].Attempts[0].RunDigest != nil {
		t.Fatal("recovery invented acquisition", err)
	}
	retry, err := beginWorkflowV2Retry(recovered, workflowV2Begin{"root-a", 2, &workflowRetry{Actor: "operator", At: "2026-10-17T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := f.store.load("v2-execution", "claimed")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.save("claimed", retry, revision); err != nil {
		t.Fatal(err)
	}
	completed, err := f.driver.run(context.Background(), "v2-execution", "claimed")
	if err != nil || calls != 1 || completed.History["root-a"].State != "completed" {
		t.Fatal("explicit retry did not get a fresh drive", err)
	}
}
