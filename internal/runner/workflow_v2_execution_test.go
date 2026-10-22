package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These fixtures execute actual Runner preview/preparation and the pinned
// Runtime with synthetic asserted data. They are not Gateway/provider journeys.
type workflowV2RuntimeTest struct {
	document workflowDocument
	inputs   []workflowFreezeInput
	root     []byte
	runtime  string
	store    *workflowV2Store
	driver   workflowV2Driver
}

func newWorkflowV2RuntimeTest(t *testing.T) workflowV2RuntimeTest {
	t.Helper()
	cfg := testConfig(t)
	cfg.disableAutomation = true
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	mapping := InputMapping{Version: 3, Case: &CaseMapping{Parameters: map[string]Parameter{}, Facts: []FactMapping{{Target: "/items", Source: "/items"}}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{}, Unmapped: []string{"/request/appropriateness", "/request/completeness", "/request/embargoedInformationToUnauthorizedRecipients", "/request/type"}, UnmappedEvidence: []string{"intake-form", "sponsor-endorsement", "sensitive-data-approvals"}}
	root := []byte(`{"items":[{"items":[{"name":"a","n":-0,"text":"<&>"}]},{"items":[{"name":"b"}]}]}`)
	release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: Input{Source: &SourceInput{Mapping: mapping, Case: root, Sources: map[string]SourceValue{}}}})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := readWorkflow(workflowExample())
	if err != nil {
		t.Fatal(err)
	}
	doc.WorkflowVersion = 2
	for i := range doc.Stages {
		stage := &doc.Stages[i]
		stage.Dialect = workflowDialectRef{Version: []byte("3"), Digest: mappingV2Hash(mapping)}
		stage.Pack.ID = release.PackID
		stage.Pack.Digest = release.PackDigest
		stage.Outputs = workflowOutputs{Pointer: "/items", Cardinality: "many"}
		if i > 0 {
			stage.CaseBindings = []workflowBinding{{From: "/items", To: "/items", Type: "array", Required: true}}
		}
	}
	doc.Limits = workflowLimits{MaxStages: 2, MaxRecords: 4096, MaxInstances: 4096, MaxAttempts: 16, MaxBytes: 1 << 30}
	store, err := newWorkflowV2Store(s.db)
	if err != nil {
		t.Fatal(err)
	}
	return workflowV2RuntimeTest{doc, []workflowFreezeInput{{Published: encode(mapping), Release: release}, {Published: encode(mapping), Release: release}}, root, release.RuntimeDigest, store, s.workflowV2Driver(store)}
}
func (f workflowV2RuntimeTest) inventory(t *testing.T, limit int) workflowV2Inventory {
	t.Helper()
	doc := f.document
	doc.Limits.MaxBytes = limit
	w, err := newWorkflowV2Inventory(encode(doc), f.root, "v2-execution", "root-a", f.inputs, nil, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func workflowV2StartRoot(t *testing.T, w workflowV2Inventory) workflowV2Inventory {
	t.Helper()
	next, err := beginWorkflowV2Group(w, 1, []workflowV2Begin{{Instance: w.Proof.RootInstance, Number: 1}}, []string{}, []workflowV2Cancel{})
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func workflowV2Selection(t *testing.T, w workflowV2Inventory, stage int, selected []string, revision int) []byte {
	t.Helper()
	set := w.Core.outputSet(stage)
	raw, err := workflowCanonical(set, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	return encode(workflowSelection{SelectionVersion: 1, WorkflowDigest: set.WorkflowDigest, Execution: set.Execution, UpstreamStage: set.Stage, OutputSetDigest: digest(raw), Selected: selected, Mode: "reviewer", Revision: revision, Review: &workflowReview{Actor: "test operator", Assertion: "Explicit synthetic-data selection", At: "2026-10-17T00:00:00Z"}})
}
func workflowV2RootResult(t *testing.T, f workflowV2RuntimeTest) (workflowV2Inventory, workflowV2Result) {
	t.Helper()
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	if err := f.store.create("root-result", w); err != nil {
		t.Fatal(err)
	}
	done, err := f.driver.run(context.Background(), w.Core.Execution.Execution, "root-result")
	if err != nil {
		t.Fatal(err)
	}
	held, err := f.store.custody(w.Core.Execution.Execution, "root-result")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range held {
		if c.Kind == "result" {
			var r workflowV2Result
			if json.Unmarshal(c.Body, &r) != nil {
				t.Fatal("result custody")
			}
			return done, r
		}
	}
	t.Fatal("missing actual result")
	return done, workflowV2Result{}
}
func workflowV2Size(t *testing.T, w workflowV2Inventory) int {
	t.Helper()
	files, err := w.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, raw := range files {
		n += len(raw)
	}
	return n
}
func TestWorkflowV2ActualRuntimeRecordingAndIndependentVersionBoundary(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	w, result := workflowV2RootResult(t, f)
	files, err := w.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = readWorkflow(files["workflow.json"]); !errors.Is(err, errWorkflowVersion) {
		t.Fatal("v1 accepted v2", err)
	}
	proof, err := readWorkflowV2Proof(files["admission-proof.json"])
	if err != nil || !proof.closed() {
		t.Fatal(err)
	}
	var execution map[string]json.RawMessage
	_ = json.Unmarshal(files["execution.json"], &execution)
	if string(execution["admissionProofDigest"]) != string(encode(digest(files["admission-proof.json"]))) {
		t.Fatal("proof pin")
	}
	base := "instances/root-a/attempts/1/"
	if !bytes.Equal(files[base+"run.json"], result.Run) {
		t.Fatal("changed actual run bytes")
	}
	checked, err := VerifyInputs(result.Run, nil, w.Core.Execution.Frozen[0].ReleaseDigest)
	if err != nil || !checked.Checked() {
		t.Fatal(err)
	}
	if !bytes.Equal(files[base+"audit.json"], workflowRunAudit(checked.bundle)) {
		t.Fatal("changed actual audit bytes")
	}
	if !bytes.Contains(files["outputs/companies/root-a/0/payload.json"], []byte(`"n":-0`)) || !bytes.Contains(files["outputs/companies/root-a/0/payload.json"], []byte(`\u003c\u0026\u003e`)) {
		t.Fatal("payload literals")
	}
	if len(w.Outputs) != 2 || len(w.History) != 1 {
		t.Fatal("retained counts")
	}
	if bytes.Equal(files["root-case.json"], nil) || !bytes.Equal(files["root-case.json"], files["instances/root-a/case.json"]) {
		t.Fatal("distinct root paths")
	}
	files[base+"run.json"][0] = '!'
	again, _ := w.recordingFiles()
	if again[base+"run.json"][0] == '!' {
		t.Fatal("lent custody")
	}
}
func TestWorkflowV2ExactActualByteBoundaryIncludesProofAndRefusalCarrier(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	_, result := workflowV2RootResult(t, f)
	var trace func(workflowV2Comparison)
	candidate := func(limit int) workflowV2Inventory {
		w := workflowV2StartRoot(t, f.inventory(t, limit))
		w.Trace = trace
		next, err := observeWorkflowV2(w, result, nil)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	limit := workflowV2Size(t, candidate(1<<30))
	for n := 0; n < 8; n++ {
		measured := workflowV2Size(t, candidate(limit))
		if measured == limit {
			break
		}
		limit = measured
	}
	trace = workflowV2JourneyTrace(t, "boundary/accepted")
	accepted := candidate(limit)
	trace = workflowV2JourneyTrace(t, "boundary/refused")
	refused := candidate(limit - 1)
	workflowV2ExportJourney(t, "boundary/accepted", accepted, nil)
	workflowV2ExportJourney(t, "boundary/refused", refused, nil)
	if accepted.History["root-a"].State != "completed" || refused.History["root-a"].State != "failed" || *refused.History["root-a"].Failure != "limit-exceeded" {
		t.Fatal("boundary states")
	}
	if workflowV2Size(t, accepted) != limit || workflowV2Size(t, refused) > limit-1 || len(refused.Outputs) != 0 {
		t.Fatal("exact bytes/refusal")
	}
	a, _ := accepted.recordingFiles()
	r, _ := refused.recordingFiles()
	proofCost := len(a["admission-proof.json"]) + len(a["admission-proof.sha256"])
	if workflowV2Size(t, accepted)-proofCost > limit-1 {
		t.Fatal("proof crossing example")
	}
	if !bytes.Equal(a["admission-proof.json"], r["admission-proof.json"]) {
		// Workflow maxBytes differs here. For one decision its candidate prefix
		// contains the same actual observation, never an admission outcome.
		var p, q workflowV2Proof
		_ = json.Unmarshal(a["admission-proof.json"], &p)
		_ = json.Unmarshal(r["admission-proof.json"], &q)
		pe, _ := p.last()
		qe, _ := q.last()
		if !reflect.DeepEqual(pe.Results, qe.Results) {
			t.Fatal("outcome in observation")
		}
	}
	if _, err := accepted.finalize(map[string][]byte{"aux/note": []byte("x")}); !errors.Is(err, errWorkflowBound) {
		t.Fatal("exempt auxiliary", err)
	}
}
func TestWorkflowV2AtomicGroupsStopsAndExplicitRetryKeepPriorPrefix(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	w, _ := workflowV2RootResult(t, f)
	selected := w.Core.Execution.OutputSets[0].Records
	w, err := selectWorkflowV2(w, workflowV2Selection(t, w, 0, selected, 1), []string{"person-a", "person-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	original := encode(w)
	bad, err := beginWorkflowV2Group(w, w.Reservations["person-a"], []workflowV2Begin{{"person-b", 1, nil}, {"person-a", 1, nil}}, []string{}, []workflowV2Cancel{})
	if err == nil || !bytes.Equal(original, encode(bad)) || !bytes.Equal(original, encode(w)) {
		t.Fatal("noncanonical/nonatomic begin", err)
	}
	w, err = beginWorkflowV2Group(w, w.Reservations["person-a"], []workflowV2Begin{{"person-b", 1, nil}}, []string{"person-a"}, []workflowV2Cancel{})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.History["person-a"].Attempts) != 0 || len(w.History["person-b"].Attempts) != 1 {
		t.Fatal("reservation became attempt")
	}
	w, err = observeWorkflowV2(w, workflowV2Result{"person-b", 1, workflowAttemptResult{State: "interrupted"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldProof, _ := w.Proof.bytes()
	oldAttempt := encode(w.History["person-b"])
	retry, err := beginWorkflowV2Retry(w, workflowV2Begin{"person-b", 2, &workflowRetry{Actor: "operator", At: "2026-10-17T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldAttempt, encode(w.History["person-b"])) {
		t.Fatal("mutated input history")
	}
	epoch, _ := retry.Proof.last()
	if epoch.PreviousProofDigest != digest(oldProof) || epoch.Kind != "retry" {
		t.Fatal("retry linkage")
	}
	retry, err = observeWorkflowV2(retry, workflowV2Result{"person-b", 2, workflowAttemptResult{State: "interrupted"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := beginWorkflowV2Group(retry, retry.Reservations["person-a"], []workflowV2Begin{{"person-a", 1, nil}}, []string{}, []workflowV2Cancel{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.History["person-b"].Attempts) != 2 || len(resumed.History["person-a"].Attempts) != 1 {
		t.Fatal("implicit sibling retry")
	}
}
func TestWorkflowV2CountRefusalHasNoIDsAndEmptyConsumedRootGuardsFirstBegin(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	_, result := workflowV2RootResult(t, f)
	f.document.Limits.MaxInstances = 1
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	w, err := observeWorkflowV2(w, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	selected := w.Core.Execution.OutputSets[0].Records
	if _, err := selectWorkflowV2(w, workflowV2Selection(t, w, 0, selected, 1), []string{"x", "y"}, false); err == nil {
		t.Fatal("count refusal allocated IDs")
	}
	refused, err := selectWorkflowV2(w, workflowV2Selection(t, w, 0, selected, 1), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _ := refused.Proof.last()
	if epoch.ChildIDs != nil || len(refused.History) != 1 || len(refused.ProspectiveIDs) != 1 {
		t.Fatal("count refusal identity capacity")
	}
	fresh := f.inventory(t, 1<<30)
	empty, err := selectWorkflowV2(fresh, workflowV2Selection(t, fresh, 0, []string{}, 1), []string{}, false)
	if err != nil {
		t.Fatal(err)
	}
	before := encode(empty)
	if _, err = beginWorkflowV2Group(empty, 1, []workflowV2Begin{{"root-a", 1, nil}}, []string{}, []workflowV2Cancel{}); !errors.Is(err, errWorkflowConsumed) {
		t.Fatal("consumed empty root first begin", err)
	}
	if _, err = beginWorkflowV2Group(empty, 1, []workflowV2Begin{}, []string{}, []workflowV2Cancel{{"root-a", "operator", "2026-10-17T00:00:00Z"}}); !errors.Is(err, errWorkflowConsumed) {
		t.Fatal("consumed root cancellation", err)
	}
	if !bytes.Equal(before, encode(empty)) {
		t.Fatal("guard mutated state")
	}
}

func TestWorkflowV2CountRefusalRecordsActualArtifactsWithoutChildIDs(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	_, result := workflowV2RootResult(t, f)
	f.document.Limits.MaxInstances = 1
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	w, err := observeWorkflowV2(w, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	priorProof, _ := w.Proof.bytes()
	var comparisons []workflowV2Comparison
	exportTrace := workflowV2JourneyTrace(t, "count-refusal")
	w.Trace = func(c workflowV2Comparison) {
		comparisons = append(comparisons, c)
		exportTrace(c)
	}
	selected := w.Core.Execution.OutputSets[0].Records
	refused, err := selectWorkflowV2(w, workflowV2Selection(t, w, 0, selected, 1), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _ := refused.Proof.last()
	if epoch.ChildIDs != nil || epoch.PreviousProofDigest != digest(priorProof) || !reflect.DeepEqual(w.History, refused.History) || !reflect.DeepEqual(w.ProspectiveIDs, refused.ProspectiveIDs) || !reflect.DeepEqual(w.Outputs, refused.Outputs) {
		t.Fatal("count refusal changed retained capacity or earlier result")
	}
	var selection workflowSelection
	if pin := refused.Core.Execution.Selections[0]; pin == nil || strictJSON(refused.Core.Artifacts["selection/"+*pin], &selection) != nil || selection.Refusal != "limit-exceeded" {
		t.Fatal("count refusal was not recorded")
	}
	files, err := refused.recordingFiles()
	if err != nil || !bytes.Equal(files["instances/root-a/attempts/1/run.json"], result.Run) {
		t.Fatal("count refusal lost actual Runtime material", err)
	}
	if len(comparisons) != 1 || comparisons[0].Kind != "selection" || comparisons[0].Cause != "instance-count" || comparisons[0].Accepted || !comparisons[0].RecordingAvailable || comparisons[0].Acceptance != nil || comparisons[0].Refusal == nil {
		t.Fatal("count refusal trace incorrectly invented a child candidate")
	}
	workflowV2ExportJourney(t, "count-refusal/recorded", refused, nil)
}
func TestWorkflowV2BranchForkRetainsActualHistoryAndOriginalHead(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	w, _ := workflowV2RootResult(t, f)
	trace := workflowV2JourneyTrace(t, "history-fork")
	w.Trace, f.driver.Trace = trace, trace
	selected := w.Core.Execution.OutputSets[0].Records
	w, err := selectWorkflowV2(w, workflowV2Selection(t, w, 0, selected, 1), []string{"person-a", "person-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	w, err = beginWorkflowV2Group(w, w.Reservations["person-a"], []workflowV2Begin{{"person-a", 1, nil}, {"person-b", 1, nil}}, []string{}, []workflowV2Cancel{})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.create("parent", w); err != nil {
		t.Fatal(err)
	}
	w, err = f.driver.run(context.Background(), w.Core.Execution.Execution, "parent")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := w.recordingFiles()
	_, revision, err := f.store.load(w.Core.Execution.Execution, "parent")
	if err != nil {
		t.Fatal(err)
	}
	workflowV2ExportJourney(t, "history-fork/parent", w, nil)
	next, err := f.store.fork(w.Core.Execution.Execution, "parent", "fork", revision, workflowV2Selection(t, w, 0, selected, 2), []string{"person-new-a", "person-new-b"}, trace)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.History) != 5 || len(next.Outputs) != 4 || len(next.Core.Execution.Instances) != 3 {
		t.Fatal("retained vs active counts")
	}
	workflowV2ExportJourney(t, "history-fork/fork", next, nil)
	after, _ := next.recordingFiles()
	for path, raw := range before {
		if bytes.HasPrefix([]byte(path), []byte("instances/")) || bytes.HasPrefix([]byte(path), []byte("outputs/")) || bytes.HasPrefix([]byte(path), []byte("selections/")) {
			if !bytes.Equal(raw, after[path]) {
				t.Fatal("retired immutable history", path)
			}
		}
	}
	original, _, err := f.store.load(w.Core.Execution.Execution, "parent")
	if err != nil {
		t.Fatal(err)
	}
	originalFiles, _ := original.recordingFiles()
	if !reflect.DeepEqual(before, originalFiles) {
		t.Fatal("fork rewrote parent")
	}
	if _, err = f.store.fork(w.Core.Execution.Execution, "parent", "stale", revision-1, workflowV2Selection(t, w, 0, selected, 2), []string{"other-a", "other-b"}); !errors.Is(err, errWorkflowRevision) {
		t.Fatal(err)
	}
	// Every earlier prospective ID is unavailable even when its instance retired.
	if _, err = selectWorkflowV2(w, workflowV2Selection(t, w, 0, selected, 2), []string{"person-a", "fresh"}, true); err == nil {
		t.Fatal("reused retired ID")
	}
}

func TestWorkflowV2TraceOwnsExactComparisonBytesWithoutChangingAdmission(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	_, result := workflowV2RootResult(t, f)
	plain := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	expected, err := observeWorkflowV2(plain, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	traces := []workflowV2Comparison{}
	plain.Trace = func(c workflowV2Comparison) { traces = append(traces, c) }
	actual, err := observeWorkflowV2(plain, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected.Core, actual.Core) || !reflect.DeepEqual(expected.Proof, actual.Proof) || len(traces) != 1 {
		t.Fatal("trace changed semantics")
	}
	comparison := traces[0]
	if comparison.Kind != "result" || comparison.Cause != "fits" || !comparison.Accepted || !comparison.RecordingAvailable {
		t.Fatal("trace meaning")
	}
	if !bytes.Equal(comparison.Acceptance["admission-proof.json"], comparison.Refusal["admission-proof.json"]) || !bytes.Equal(comparison.Proof, comparison.Acceptance["admission-proof.json"]) {
		t.Fatal("different counterfactual proof bytes")
	}
	files, _ := actual.recordingFiles()
	if !reflect.DeepEqual(files, comparison.Acceptance) {
		t.Fatal("trace approximate bytes")
	}
	comparison.Acceptance["workflow.json"][0] = '!'
	comparison.Refusal["admission-proof.json"][0] = '!'
	comparison.Proof[0] = '!'
	after, _ := actual.recordingFiles()
	if after["workflow.json"][0] == '!' || after["admission-proof.json"][0] == '!' {
		t.Fatal("trace lent state bytes")
	}
	if bytes.Contains(workflowV2StoreBody(actual), []byte("Trace")) {
		t.Fatal("persisted hook")
	}
}
func TestWorkflowV2TraceDoesNotRequireLargerRefusalForFittingEmptySuccess(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	f.root = []byte(`{"items":[]}`)
	_, result := workflowV2RootResult(t, f)
	build := func(limit int, trace func(workflowV2Comparison)) workflowV2Inventory {
		w := workflowV2StartRoot(t, f.inventory(t, limit))
		w.Trace = trace
		next, err := observeWorkflowV2(w, result, nil)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	limit := workflowV2Size(t, build(1<<30, nil))
	for i := 0; i < 8; i++ {
		size := workflowV2Size(t, build(limit, nil))
		if size == limit {
			break
		}
		limit = size
	}
	var comparison workflowV2Comparison
	accepted := build(limit, func(c workflowV2Comparison) { comparison = c })
	sum := func(files map[string][]byte) int {
		n := 0
		for _, raw := range files {
			n += len(raw)
		}
		return n
	}
	if accepted.History["root-a"].State != "completed-zero" || sum(comparison.Acceptance) != limit || sum(comparison.Refusal) <= limit || !comparison.Accepted || !comparison.RecordingAvailable {
		t.Fatal("unneeded refusal carrier controlled acceptance")
	}
}
func TestWorkflowV2ProofClosedFormsRejectUnknownMembersAndChangedPrefixLinks(t *testing.T) {
	p := workflowV2Proof{1, digest([]byte("workflow")), "execution", digest([]byte("case")), digest([]byte("frozen")), "root", [][]json.RawMessage{}}
	if err := p.append("root", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.append("group", map[string]any{"reservation": 1, "starts": []workflowV2Begin{{"root", 1, nil}}, "deferred": []string{}, "cancelled": []workflowV2Cancel{}, "results": []workflowV2Observation{}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := p.bytes()
	if _, err := readWorkflowV2Proof(raw); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(v map[string]any) { v["extra"] = true },
		func(v map[string]any) {
			v["epochBlocks"].([]any)[0].([]any)[1].(map[string]any)["previousProofDigest"] = digest([]byte("forged"))
		},
		func(v map[string]any) {
			v["epochBlocks"].([]any)[0].([]any)[1].(map[string]any)["starts"].([]any)[0].(map[string]any)["extra"] = true
		},
		func(v map[string]any) { v["epochBlocks"].([]any)[0].([]any)[1].(map[string]any)["deferred"] = nil },
	} {
		v := decoded(t, raw).(map[string]any)
		change(v)
		changed, err := workflowCanonical(v, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = readWorkflowV2Proof(changed); err == nil {
			t.Fatal("accepted changed closed form")
		}
	}
}

func TestWorkflowV2RefusedProspectiveIDsAndLengthUseActualCandidateBytes(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	f.document.Limits.MaxInstances = 11
	items := []any{}
	short, long := []string{}, []string{}
	for n := 0; n < 10; n++ {
		items = append(items, map[string]any{"items": []any{map[string]any{"name": n, "detail": string(bytes.Repeat([]byte("x"), 500))}}})
		short = append(short, "p"+string(rune('a'+n)))
		long = append(long, "p"+string(bytes.Repeat([]byte{byte('a' + n)}, 63)))
	}
	f.root = encode(map[string]any{"items": items})
	_, rootResult := workflowV2RootResult(t, f)
	var trace workflowV2Comparison
	build := func(limit int, ids []string, hook func(workflowV2Comparison)) workflowV2Inventory {
		root := workflowV2StartRoot(t, f.inventory(t, limit))
		var err error
		root, err = observeWorkflowV2(root, rootResult, nil)
		if err != nil {
			t.Fatal(err)
		}
		root.Trace = func(c workflowV2Comparison) {
			trace = c
			if hook != nil {
				hook(c)
			}
		}
		next, err := selectWorkflowV2(root, workflowV2Selection(t, root, 0, root.Core.Execution.OutputSets[0].Records, 1), ids, false)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	sum := func(files map[string][]byte) int {
		size := 0
		for _, raw := range files {
			size += len(raw)
		}
		return size
	}
	build(1<<30, short, nil)
	limit := sum(trace.Acceptance)
	for n := 0; n < 8; n++ {
		build(limit, short, nil)
		measured := sum(trace.Acceptance)
		if measured == limit {
			break
		}
		limit = measured
	}
	accepted := build(limit, short, workflowV2JourneyTrace(t, "prospective-ids/short"))
	shortTrace := trace
	refused := build(limit, long, workflowV2JourneyTrace(t, "prospective-ids/long"))
	longTrace := trace
	if !shortTrace.Accepted || longTrace.Accepted || sum(shortTrace.Acceptance) != limit || sum(longTrace.Acceptance) <= limit || sum(longTrace.Refusal) > limit {
		t.Fatal("IDs did not affect actual bytes")
	}
	if len(refused.History) != 1 || len(refused.ProspectiveIDs) != 11 {
		t.Fatal("refused names reserved capacity")
	}
	frozen := encode(refused)
	selection := workflowV2Selection(t, refused, 0, refused.Core.Execution.OutputSets[0].Records[:1], 2)
	if _, err := selectWorkflowV2(refused, selection, long[:1], false); err == nil || !bytes.Equal(frozen, encode(refused)) {
		t.Fatal("reused refused name or changed prefix")
	}
	refused.Trace = workflowV2JourneyTrace(t, "prospective-ids/revision")
	revised, err := selectWorkflowV2(refused, selection, []string{"fresh-child"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(revised.History) != 2 || len(revised.ProspectiveIDs) <= revised.Core.Document.Limits.MaxInstances {
		t.Fatal("prospective names consumed reserved capacity")
	}
	workflowV2ExportJourney(t, "prospective-ids/short", accepted, nil)
	workflowV2ExportJourney(t, "prospective-ids/long", refused, nil)
	workflowV2ExportJourney(t, "prospective-ids/revision", revised, nil)
}

func TestWorkflowV2UnsupportedRunVersionPrecedesSupportedClosedForm(t *testing.T) {
	f := newWorkflowV2RuntimeTest(t)
	_, result := workflowV2RootResult(t, f)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	before := encode(w)
	for _, version := range []int{4, 5} {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(result.Run, &raw)
		raw["version"] = encode(version)
		raw["unknown-in-supported-form"] = []byte("true")
		r := result
		r.Run = encode(raw)
		if _, err := observeWorkflowV2(w, r, nil); !errors.Is(err, errWorkflowVersion) {
			t.Fatal("read unsupported run form", version, err)
		}
		if !bytes.Equal(before, encode(w)) {
			t.Fatal("rewrote unsupported run or old prefix")
		}
	}
}

func TestWorkflowV2ActualRuntimeSidecarIsSeparatelyPinnedAndCounted(t *testing.T) {
	workflowV2ActualRuntimeSidecar(t, "runtime-signatures")
}

func TestWorkflowV2ActualRuntimeSidecarUnderSymlinkedTempRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	physical, alias := filepath.Join(base, "physical"), filepath.Join(base, "alias")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	// A fresh subtest chooses its own temp root after TMPDIR changes. This
	// exercises the /var -> /private/var placement common on macOS on Unix.
	t.Run("signed-attempt", func(t *testing.T) {
		temporary := t.TempDir()
		resolved, err := filepath.EvalSymlinks(temporary)
		if err != nil || temporary == resolved {
			t.Fatal("fixture did not use a symlinked temporary root", err)
		}
		workflowV2ActualRuntimeSidecar(t, "runtime-signatures-symlink-temp")
	})
}

func workflowV2ActualRuntimeSidecar(t *testing.T, recording string) {
	t.Helper()
	f := newWorkflowV2RuntimeTest(t)
	w := workflowV2StartRoot(t, f.inventory(t, 1<<30))
	if err := f.store.create("signed-runtime", w); err != nil {
		t.Fatal(err)
	}
	// Runtime requires the signing key's physical path, including on macOS
	// where a temporary root can pass through /var -> /private/var.
	keyFile, public := signingKey(t), mustKey(t, vectorKey1)
	directory := t.TempDir()
	var actualSidecar []byte
	evaluationCalls := 0
	f.driver.Evaluate = func(ctx context.Context, release Release, input Input, runID string) (workflowEvaluation, error) {
		evaluationCalls++
		result, audit, sidecar, _, err := evaluateWith(ctx, os.Getenv("JPACK_TEST_BIN"), release, input, filepath.Join(directory, runID), false, signingInputs{path: keyFile, publicKey: vectorKey1})
		actualSidecar = bytes.Clone(sidecar)
		return workflowEvaluation{Result: result, Audit: audit, Signatures: sidecar}, err
	}
	f.driver.Trace = workflowV2JourneyTrace(t, recording)
	done, err := f.driver.run(context.Background(), "v2-execution", "signed-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if evaluationCalls != 1 || len(actualSidecar) == 0 {
		t.Fatalf("expected one signed Runtime evaluation: calls=%d sidecar bytes=%d", evaluationCalls, len(actualSidecar))
	}
	files, err := done.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	sidecar := files["instances/root-a/attempts/1/audit-signatures.json"]
	if len(sidecar) == 0 || !bytes.Equal(sidecar, actualSidecar) {
		t.Fatal("lost exact Runtime sidecar")
	}
	e, _ := done.Proof.last()
	if e.Results[0].AuditSignaturesDigest == nil || *e.Results[0].AuditSignaturesDigest != digest(sidecar) {
		t.Fatal("sidecar not in observation")
	}
	var bundle VerificationBundle
	if strictJSON(files["instances/root-a/attempts/1/run.json"], &bundle) != nil || bundle.Version != 3 || len(bundle.Run.AuditSignatures) != 0 {
		t.Fatal("invented embedded sidecar/version")
	}
	checked := checkSignatures([][]byte{files["instances/root-a/attempts/1/audit.json"]}, sidecar, public)
	if len(checked.findings) != 0 || len(checked.signed) != 1 {
		t.Fatal("actual Runtime sidecar did not verify", checked.findings)
	}
	total := workflowV2Size(t, done)
	without := 0
	for path, raw := range files {
		if path != "instances/root-a/attempts/1/audit-signatures.json" {
			without += len(raw)
		}
	}
	if without+len(sidecar) != total {
		t.Fatal("sidecar byte exemption")
	}
	workflowV2ExportJourney(t, recording+"/completed", done, nil)
}
