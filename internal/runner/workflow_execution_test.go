package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func workflowInventoryFixture(t *testing.T, limit int, configure ...func(map[string]any)) workflowInventory {
	t.Helper()
	f := newImportFixture(t)
	_, in, runtime := workflowFreezeFixture(t, encode(f.direct), nil, []InputProfile{f.profile})
	doc := decoded(t, workflowExample()).(map[string]any)
	for _, value := range doc["stages"].([]any) {
		stage := value.(map[string]any)
		stage["dialect"] = map[string]any{"version": 3, "digest": mappingV2Hash(f.direct)}
		stage["pack"] = map[string]any{"id": in.Release.PackID, "digest": in.Release.PackDigest}
	}
	doc["limits"].(map[string]any)["maxInstances"] = limit
	for _, apply := range configure {
		apply(doc)
	}
	inputs := make([]workflowFreezeInput, len(doc["stages"].([]any)))
	for i := range inputs {
		inputs[i] = in
	}
	w, err := newWorkflowInventory(encode(doc), []byte(`{"id":7,"notInherited":"private"}`), "execution-a", inputs, []InputProfile{f.profile}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture supplies already checked preparation/audit data to exercise
	// writer record construction. It is not an offline verification fixture.
	preparation := &Preparation{Lineage: []TargetLineage{{Kind: "fact", Target: "/companies", Class: "record"}}}
	records, payloads, err := workflowOutputRecords(w, w.Execution.Instances[0], preparation, []byte(`{"recordVersion":"fixture"}`), []byte(`{"companies":[{"name":"A"},{"name":"B"},{"name":"A"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, record := range records {
		raw, err := workflowCanonical(record, 65536)
		if err != nil {
			t.Fatal(err)
		}
		w.Execution.OutputSets[0].Records = append(w.Execution.OutputSets[0].Records, w.keep("output", raw))
		w.keep("payload", payloads[i])
	}
	w.Execution.Instances[0].State = "completed"
	return w
}
func workflowSelectionFor(t *testing.T, w workflowInventory, selected []string, revision int) []byte {
	t.Helper()
	set := w.outputSet(0)
	raw, err := workflowCanonical(set, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	return encode(workflowSelection{SelectionVersion: 1, WorkflowDigest: set.WorkflowDigest, Execution: set.Execution, UpstreamStage: set.Stage, OutputSetDigest: digest(raw), Selected: selected, Mode: "reviewer", Revision: revision, Review: &workflowReview{Actor: "operator", Assertion: "Use selected companies", At: "2026-10-16T12:00:00Z"}})
}

func TestWorkflowSelectionReservesAllChildrenAndPreservesDuplicates(t *testing.T) {
	w := workflowInventoryFixture(t, 3)
	offered := w.Execution.OutputSets[0].Records
	first, a, err := w.record(offered[0])
	if err != nil {
		t.Fatal(err)
	}
	third, b, err := w.record(offered[2])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) || first.PayloadDigest != third.PayloadDigest || offered[0] == offered[2] {
		t.Fatal("equal payloads lost distinct output identities")
	}
	raw := workflowSelectionFor(t, w, []string{offered[0], offered[2]}, 1)
	next, err := selectWorkflow(w, raw, nil)
	if err != nil || len(next.Execution.Instances) != 3 {
		t.Fatal(err)
	}
	if len(w.Execution.Instances) != 1 || w.Execution.Selections[0] != nil {
		t.Fatal("transition mutated original state")
	}
	for i, instance := range next.Execution.Instances[1:] {
		if *instance.ParentRecord != []string{offered[0], offered[2]}[i] || instance.State != "interrupted" || len(instance.Attempts) != 0 {
			t.Fatal(instance)
		}
		if got := next.Artifacts["case/"+*instance.CaseDigest]; string(got) != `{"company":{"name":"A"}}` {
			t.Fatal("projection leaked unrelated root data", string(got))
		}
	}
	again, err := selectWorkflow(next, raw, nil)
	if err != nil || !bytes.Equal(encode(next), encode(again)) {
		t.Fatal("same selection did not return existing admission", err)
	}
	if _, err := selectWorkflow(next, workflowSelectionFor(t, next, []string{}, 2), nil); !errors.Is(err, errWorkflowConsumed) {
		t.Fatal("consumed selection changed", err)
	}
}

func TestWorkflowSelectionBoundRefusalHasNoPartialAdmission(t *testing.T) {
	w := workflowInventoryFixture(t, 2)
	offered := w.Execution.OutputSets[0].Records
	refused, err := selectWorkflow(w, workflowSelectionFor(t, w, []string{offered[0], offered[2]}, 1), nil)
	if err != nil || len(refused.Execution.Instances) != 1 || refused.Execution.Selections[0] == nil || refused.Execution.OutputSets[1].Status != "blocked" {
		t.Fatal(refused.Execution, err)
	}
	var selection workflowSelection
	if json.Unmarshal(refused.Artifacts["selection/"+*refused.Execution.Selections[0]], &selection) != nil || selection.Refusal != "limit-exceeded" {
		t.Fatal(selection)
	}
	admitted, err := selectWorkflow(refused, workflowSelectionFor(t, refused, []string{offered[2]}, 2), nil)
	if err != nil || len(admitted.Execution.Instances) != 2 {
		t.Fatal(err)
	}
	if _, kept := admitted.Artifacts["selection/"+*refused.Execution.Selections[0]]; !kept {
		t.Fatal("earlier refusal was not retained")
	}
}

func TestWorkflowEmptySelectionClosesExplicitZeroInstanceStage(t *testing.T) {
	w := workflowInventoryFixture(t, 3)
	next, err := selectWorkflow(w, workflowSelectionFor(t, w, []string{}, 1), nil)
	if err != nil || len(next.Execution.Instances) != 1 || next.Execution.OutputSets[1].Status != "closed" || len(next.Execution.OutputSets[1].Records) != 0 {
		t.Fatal(next.Execution, err)
	}
}

func TestWorkflowProjectionFailureStillReservesInstance(t *testing.T) {
	w := workflowInventoryFixture(t, 3, func(doc map[string]any) {
		binding := doc["stages"].([]any)[1].(map[string]any)["caseBindings"].([]any)[0].(map[string]any)
		binding["type"], binding["required"] = "integer", false
	})
	raw := workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:1], 1)
	next, err := selectWorkflow(w, raw, nil)
	if err != nil || len(next.Execution.Instances) != 2 {
		t.Fatal(err)
	}
	instance := next.Execution.Instances[1]
	if instance.State != "failed" || instance.Failure == nil || *instance.Failure != "projection-invalid" || instance.CaseDigest != nil || len(instance.Attempts) != 0 || !workflowDigest.MatchString(instance.InputDigest) {
		t.Fatal(instance)
	}
}

func TestWorkflowSelectionForgedWholeSetIsRefusedWithoutMutation(t *testing.T) {
	w := workflowInventoryFixture(t, 3)
	raw := workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:1], 1)
	w.Execution.OutputSets[0].Records[2] = digest([]byte("different unselected envelope"))
	before := encode(w)
	_, err := selectWorkflow(w, raw, nil)
	if err == nil || !bytes.Equal(before, encode(w)) {
		t.Fatal("unselected member not committed", err)
	}
}

func TestWorkflowInventoryAcceptsAbsentBindingRepresentedByEmptyBytes(t *testing.T) {
	f := newImportFixture(t)
	raw, in, runtime := workflowFreezeFixture(t, encode(f.direct), []byte{}, []InputProfile{f.profile})
	w, err := newWorkflowInventory(raw, []byte(`{"id":7}`), "empty-binding", []workflowFreezeInput{in}, []InputProfile{f.profile}, runtime)
	if err != nil || w.Execution.Frozen[0].BindingDocumentDigest != nil {
		t.Fatal(err)
	}
	for key := range w.Artifacts {
		if len(key) >= 8 && key[:8] == "binding/" {
			t.Fatal("invented absent binding artifact")
		}
	}
}

func TestWorkflowSelectionAcceptsLargeValidInputDigestPreimage(t *testing.T) {
	w := workflowInventoryFixture(t, 3, func(doc map[string]any) {
		bindings := make([]workflowBinding, 64)
		for i := range bindings {
			bindings[i] = workflowBinding{From: "/name", To: "/" + strings.Repeat("a", 1020) + fmt.Sprintf("%03d", i), Type: "string", Required: true}
		}
		doc["stages"].([]any)[1].(map[string]any)["caseBindings"] = bindings
	})
	selection := workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:1], 1)
	next, err := selectWorkflow(w, selection, nil)
	if err != nil || len(next.Execution.Instances) != 2 {
		t.Fatal(err)
	}
	preimage := map[string]any{"parentRecord": *next.Execution.Instances[1].ParentRecord, "selectionDigest": *next.Execution.Instances[1].SelectionDigest, "caseBindings": w.Document.Stages[1].CaseBindings}
	raw, err := workflowCanonical(preimage, 262144)
	if err != nil || len(raw) <= 65536 || digest(raw) != next.Execution.Instances[1].InputDigest {
		t.Fatal(len(raw), err)
	}
	if next.Execution.Instances[1].Failure != nil {
		t.Fatal(next.Execution.Instances[1])
	}
}

func TestWorkflowProjectionDepthBoundRefusesEntireSelection(t *testing.T) {
	w := workflowInventoryFixture(t, 3, func(doc map[string]any) {
		doc["stages"].([]any)[1].(map[string]any)["caseBindings"] = []workflowBinding{{From: "/name", To: "/company/name", Type: "object", Required: true}}
	})
	var nested any = "leaf"
	for i := 0; i < 31; i++ {
		nested = map[string]any{"n": nested}
	}
	_, payload, err := workflowCase(encode(map[string]any{"name": nested}))
	if err != nil {
		t.Fatal("payload itself must fit", err)
	}
	for _, i := range []int{0, 1} {
		record, _, err := w.record(w.Execution.OutputSets[0].Records[i])
		if err != nil {
			t.Fatal(err)
		}
		record.PayloadDigest = w.keep("payload", payload)
		raw, err := workflowCanonical(record, 65536)
		if err != nil {
			t.Fatal(err)
		}
		w.Execution.OutputSets[0].Records[i] = w.keep("output", raw)
	}
	before := encode(w)
	next, err := selectWorkflow(w, workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:2], 1), nil)
	if err != nil || len(next.Execution.Instances) != 1 || next.Execution.OutputSets[1].Status != "blocked" {
		t.Fatal(next.Execution, err)
	}
	var selection workflowSelection
	if next.Execution.Selections[0] == nil || strictJSON(next.Artifacts["selection/"+*next.Execution.Selections[0]], &selection) != nil || selection.Refusal != "limit-exceeded" {
		t.Fatal(selection)
	}
	if !bytes.Equal(before, encode(w)) {
		t.Fatal("refusal mutated original")
	}
	// A reduced selection is still reviewable; no partial child consumed it.
	next, err = selectWorkflow(next, workflowSelectionFor(t, next, []string{}, 2), nil)
	if err != nil || next.Execution.OutputSets[1].Status != "closed" {
		t.Fatal(err)
	}
}

func TestWorkflowRecordingCountsSharedFilesSeparately(t *testing.T) {
	w := workflowInventoryFixture(t, 3)
	files, err := w.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(files["stages/companies/pack.json"], files["stages/people/pack.json"]) || !bytes.Equal(files["outputs/companies/instance-1/0/payload.json"], files["outputs/companies/instance-1/2/payload.json"]) {
		t.Fatal("fixture must share stage and payload bytes")
	}
	total, unique := 0, 0
	seen := map[string]bool{}
	for _, raw := range files {
		total += len(raw)
		pin := digest(raw)
		if !seen[pin] {
			unique += len(raw)
			seen[pin] = true
		}
	}
	if total <= unique {
		t.Fatal("recording failed to count duplicates")
	}
	// Isolate accounting's limit boundary from document admission. No modified
	// document is exported or used for a transition in these direct checks.
	w.Document.Limits.MaxBytes = total
	if !w.withinBounds() {
		t.Fatal("exact file budget refused")
	}
	w.Document.Limits.MaxBytes = total - 1
	if !errors.Is(w.checkBounds(), errWorkflowBound) {
		t.Fatal("one byte over admitted")
	}
	w.Document.Limits.MaxBytes = unique
	if w.withinBounds() {
		t.Fatal("blob deduplication undercounted recording")
	}
	// Export views cannot mutate bytes held by the writer.
	original := bytes.Clone(w.Artifacts["pack/"+w.Execution.Frozen[0].PackDigest])
	files["stages/companies/pack.json"][0] = 'x'
	if !bytes.Equal(original, w.Artifacts["pack/"+w.Execution.Frozen[0].PackDigest]) {
		t.Fatal("recording aliases custody bytes")
	}
}

func TestWorkflowRecordingWithoutProfilesWritesEmptyArray(t *testing.T) {
	mapping := InputMapping{Version: 3, Case: &CaseMapping{Facts: []FactMapping{{Source: "/companies", Target: "/companies"}}, Evidence: []EvidenceMapping{}}}
	raw, in, runtime := workflowFreezeFixture(t, encode(mapping), nil, nil)
	if in.Release.InputProfiles != nil && len(in.Release.InputProfiles) != 0 {
		t.Fatal("fixture has profiles")
	}
	w, err := newWorkflowInventory(raw, []byte(`{"companies":[]}`), "case-only", []workflowFreezeInput{in}, nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	files, err := w.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	if string(files["stages/companies/profiles.json"]) != "[]" {
		t.Fatal(string(files["stages/companies/profiles.json"]))
	}
}
