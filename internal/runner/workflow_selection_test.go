package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func exampleWorkflowSelection(t *testing.T) (workflowStage, workflowSet, workflowSelection) {
	t.Helper()
	d, doc, err := readWorkflow(workflowExample())
	if err != nil {
		t.Fatal(err)
	}
	set := workflowSet{digest(doc), "execution-one", d.Stages[0].ID, []string{digest([]byte("first")), digest([]byte("second")), digest([]byte("third"))}}
	_, canonical, err := readWorkflowSet(encode(set), set.WorkflowDigest, set.Execution, set.Stage, 5)
	if err != nil {
		t.Fatal(err)
	}
	selection := workflowSelection{SelectionVersion: 1, WorkflowDigest: set.WorkflowDigest, Execution: set.Execution, UpstreamStage: set.Stage, OutputSetDigest: digest(canonical), Selected: []string{set.Records[0], set.Records[2]}, Mode: "reviewer", Revision: 1, Review: &workflowReview{"local-reviewer", "Keep these companies for people research.", "2026-10-16T20:00:00Z"}}
	return d.Stages[0], set, selection
}
func TestWorkflowSelectionBindsWholeOrderedSet(t *testing.T) {
	stage, set, selected := exampleWorkflowSelection(t)
	_, canonical, err := readWorkflowSelection(encode(selected), set, stage, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := readWorkflowSelection(canonical, set, stage, 5, nil)
	if err != nil || !bytes.Equal(canonical, again) {
		t.Fatal("roundtrip", err)
	}
	changed := set
	changed.Records = append([]string{}, set.Records...)
	changed.Records[1] = digest([]byte("different unselected record"))
	if _, _, err := readWorkflowSelection(canonical, changed, stage, 5, nil); err == nil {
		t.Fatal("ignored unselected record change")
	}
	for _, records := range [][]string{{set.Records[2], set.Records[0]}, {set.Records[0], set.Records[0]}, {digest([]byte("foreign"))}, nil} {
		bad := selected
		bad.Selected = records
		if _, _, err := readWorkflowSelection(encode(bad), set, stage, 5, nil); err == nil {
			t.Fatal("accepted bad subset", records)
		}
	}
	empty := selected
	empty.Selected = []string{}
	if _, _, err := readWorkflowSelection(encode(empty), set, stage, 5, nil); err != nil {
		t.Fatal("explicit empty selection", err)
	}
	refused := selected
	refused.Refusal = "limit-exceeded"
	if _, _, err := readWorkflowSelection(encode(refused), set, stage, 5, nil); err != nil {
		t.Fatal("retained admission refusal", err)
	}
}
func TestWorkflowSelectionClosedFormsAndReview(t *testing.T) {
	stage, set, selected := exampleWorkflowSelection(t)
	for _, mutate := range []func(*workflowSelection){func(v *workflowSelection) { v.Revision = 0 }, func(v *workflowSelection) { v.Revision = 17 }, func(v *workflowSelection) { v.Review = nil }, func(v *workflowSelection) { v.Refusal = ""; v.Rule = &workflowRule{Kind: "unresolved"} }, func(v *workflowSelection) { v.Refusal = "permission" }, func(v *workflowSelection) { v.Execution = "different" }, func(v *workflowSelection) { v.Review.Actor = strings.Repeat("é", 129) }, func(v *workflowSelection) { v.Review.At = "2026-02-30T00:00:00Z" }} {
		copy := selected
		review := *selected.Review
		copy.Review = &review
		mutate(&copy)
		if _, _, err := readWorkflowSelection(encode(copy), set, stage, 5, nil); err == nil {
			t.Fatal("accepted invalid selection", string(encode(copy)))
		}
	}
	for _, replace := range []struct{ old, new string }{{`"selectionVersion":1`, `"SelectionVersion":1`}, {`"revision":1`, `"revision":1,"revision":1`}, {`"mode":"reviewer"`, `"mode":"reviewer","refusal":null`}, {`"mode":"reviewer"`, `"mode":"reviewer","rule":null`}} {
		raw := strings.Replace(string(encode(selected)), replace.old, replace.new, 1)
		if _, _, err := readWorkflowSelection([]byte(raw), set, stage, 5, nil); err == nil {
			t.Fatal("accepted extra/missing/null member", raw)
		}
	}
}
func TestWorkflowDispositionSelectionNeedsEveryAudit(t *testing.T) {
	stage, set, selected := exampleWorkflowSelection(t)
	stage.Outputs.Cardinality = "one"
	stage.Selection = &workflowSelectionPolicy{Mode: "disposition", Rule: &workflowRule{Kind: "outcome", OutcomeID: "keep"}}
	selected.Mode = "disposition"
	selected.Review = nil
	selected.Rule = stage.Selection.Rule
	dispositions := map[string]json.RawMessage{set.Records[0]: json.RawMessage(`{"kind":"outcome","outcomeId":"keep"}`), set.Records[1]: json.RawMessage(`{"kind":"unresolved","reasons":["unknown"]}`), set.Records[2]: json.RawMessage(`{"kind":"outcome","outcomeId":"keep"}`)}
	if _, _, err := readWorkflowSelection(encode(selected), set, stage, 5, dispositions); err != nil {
		t.Fatal(err)
	}
	delete(dispositions, set.Records[1])
	if _, _, err := readWorkflowSelection(encode(selected), set, stage, 5, dispositions); err == nil {
		t.Fatal("accepted missing unselected audit")
	}
	dispositions[set.Records[1]] = json.RawMessage(`{"kind":"outcome","outcomeId":"keep"}`)
	if _, _, err := readWorkflowSelection(encode(selected), set, stage, 5, dispositions); err == nil {
		t.Fatal("accepted wrong predicate result")
	}
	dispositions[set.Records[1]] = json.RawMessage(`{"kind":"evaluation-error"}`)
	if _, _, err := readWorkflowSelection(encode(selected), set, stage, 5, dispositions); err == nil {
		t.Fatal("accepted evaluation failure as disposition")
	}
}
func TestWorkflowSetClosedForms(t *testing.T) {
	_, set, _ := exampleWorkflowSelection(t)
	for _, records := range [][]string{nil, {"not-digest"}, {set.Records[0], set.Records[0]}} {
		copy := set
		copy.Records = records
		if _, _, err := readWorkflowSet(encode(copy), set.WorkflowDigest, set.Execution, set.Stage, 5); err == nil {
			t.Fatal("accepted invalid set", records)
		}
	}
	set.Records = []string{}
	if _, _, err := readWorkflowSet(encode(set), set.WorkflowDigest, set.Execution, set.Stage, 5); err != nil {
		t.Fatal("explicit empty set", err)
	}
}

func TestWorkflowNonOutcomeAndEmptySelections(t *testing.T) {
	stage, set, selected := exampleWorkflowSelection(t)
	stage.Outputs.Cardinality = "one"
	dispositions := map[string]json.RawMessage{set.Records[0]: json.RawMessage(`{"kind":"unresolved","reasons":["unknown"]}`), set.Records[1]: json.RawMessage(`{"kind":"not-applicable"}`), set.Records[2]: json.RawMessage(`{"kind":"outcome","outcomeId":"keep"}`)}
	for i, kind := range []string{"unresolved", "not-applicable"} {
		stage.Selection = &workflowSelectionPolicy{Mode: "disposition", Rule: &workflowRule{Kind: kind}}
		selected.Mode = "disposition"
		selected.Review = nil
		selected.Rule = stage.Selection.Rule
		selected.Selected = []string{set.Records[i]}
		if _, _, err := readWorkflowSelection(encode(selected), set, stage, 5, dispositions); err != nil {
			t.Fatal(kind, err)
		}
	}
	set.Records = []string{}
	_, canonical, err := readWorkflowSet(encode(set), set.WorkflowDigest, set.Execution, set.Stage, 5)
	if err != nil {
		t.Fatal(err)
	}
	selected.OutputSetDigest = digest(canonical)
	selected.Selected = []string{}
	if _, _, err := readWorkflowSelection(encode(selected), set, stage, 5, map[string]json.RawMessage{}); err != nil {
		t.Fatal("empty disposition selection", err)
	}
}
