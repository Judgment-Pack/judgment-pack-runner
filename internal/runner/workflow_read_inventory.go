package runner

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// The offline reader checks terminal inventory directly. It never replays the
// writer's private Pending column, generated instance ids or scheduling rules.
func (r *workflowReader) inventoryAndOutputs() error {
	if len(r.w.Execution.Instances) == 0 {
		return workflowFinding("inventory-mismatch")
	}
	for stageIndex, stage := range r.w.Document.Stages {
		held := &r.stages[stageIndex]
		expected := []string{""}
		blocked := false
		selectionPin := ""
		if !stage.Input.Root {
			upstream := r.w.stageIndex(stage.Input.FromStage)
			selection := r.stages[upstream].selection
			blocked = r.w.Execution.OutputSets[upstream].Status != "closed" || selection == nil || selection.Refusal != ""
			expected = []string{}
			if !blocked {
				expected = selection.Selected
				selectionPin = stringPin(r.w.Execution.Selections[upstream])
			}
		}
		status := "closed"
		if blocked {
			status = "blocked"
		}
		if r.w.Execution.OutputSets[stageIndex].Status != status || blocked && (len(held.outputs) > 0 || held.selection != nil) || stage.Selection == nil && held.selection != nil {
			return workflowFinding("inventory-mismatch")
		}
		actual := []int{}
		for i, instance := range r.w.Execution.Instances {
			if instance.Stage == stage.ID {
				actual = append(actual, i)
			}
		}
		if len(actual) != len(expected) {
			return workflowFinding("inventory-mismatch")
		}
		for at, index := range actual {
			instance := r.w.Execution.Instances[index]
			if stringPin(instance.ParentRecord) != expected[at] || stringPin(instance.SelectionDigest) != selectionPin || (instance.ParentRecord == nil) != (instance.SelectionDigest == nil) || stage.Input.Root && instance.ParentRecord != nil {
				return workflowFinding("inventory-mismatch")
			}
			inputDigest := r.w.Execution.RootCaseDigest
			if !stage.Input.Root {
				raw, err := workflowCanonical(map[string]any{"parentRecord": expected[at], "selectionDigest": selectionPin, "caseBindings": stage.CaseBindings}, 262144)
				if err != nil {
					return workflowReferenceError(err)
				}
				inputDigest = digest(raw)
			}
			if instance.InputDigest != inputDigest {
				return workflowFinding("inventory-mismatch")
			}
		}
		if held.selection != nil && held.selection.Refusal != "" {
			// Instance ceilings and projection bounds have an independent proof in the
			// retained branch. Byte-only refusal cannot be inferred from missing child
			// artifacts or by replaying one writer's private id allocation.
			children := []int{}
			for i, s := range r.w.Document.Stages {
				if s.Input.FromStage == stage.ID {
					children = append(children, i)
				}
			}
			known := len(r.w.Execution.Instances)+len(children)*len(held.selection.Selected) > r.w.Document.Limits.MaxInstances
			for _, child := range children {
				for _, pin := range held.selection.Selected {
					parent, ok := r.output(pin)
					if !ok {
						return workflowFinding("inventory-mismatch")
					}
					_, err := projectWorkflowCase(parent.payload, r.w.Document.Stages[child].CaseBindings)
					known = known || errors.Is(err, errWorkflowBound)
				}
			}
			if !known {
				return errors.New("byte-only selection refusal requires a specified admission-size proof; the draft contract does not retain one")
			}
		}
	}
	records := 0
	limited := map[string]bool{}
	expectedOutputs := make([][]workflowReadOutput, len(r.stages))
	for i, instance := range r.w.Execution.Instances {
		failure := stringPin(instance.Failure)
		if (instance.State == "failed") != (instance.Failure != nil) {
			return workflowFinding("inventory-mismatch")
		}
		stageIndex := r.w.stageIndex(instance.Stage)
		if limited[instance.Stage] {
			if instance.State != "failed" || failure != "limit-exceeded" {
				return workflowFinding("inventory-mismatch")
			}
		}
		if len(instance.Attempts) == 0 {
			switch instance.State {
			case "interrupted", "cancelled":
			case "failed":
				if failure != "projection-invalid" && !(failure == "admission-refused" && r.admissionRefused[i]) && !(failure == "limit-exceeded" && limited[instance.Stage]) {
					return workflowFinding("inventory-mismatch")
				}
			default:
				return workflowFinding("inventory-mismatch")
			}
			continue
		}
		if instance.CaseDigest == nil || failure == "projection-invalid" || r.admissionRefused[i] {
			return workflowFinding("inventory-mismatch")
		}
		for j, attempt := range instance.Attempts {
			last := j == len(instance.Attempts)-1
			if attempt.Number != j+1 || (j == 0) != (attempt.Retry == nil) || (attempt.State == "failed") != (attempt.Failure != nil) || !last && attempt.State != "failed" && attempt.State != "interrupted" || last && (attempt.State != instance.State || stringPin(attempt.Failure) != failure) {
				return workflowFinding("inventory-mismatch")
			}
			a := &r.attempts[i][j]
			af := stringPin(attempt.Failure)
			hasAudit := len(a.audit) > 0
			if (af == "evaluation-error") != (attempt.EvaluationErrorDigest != nil) || (attempt.PreparationDigest != nil) != (a.bundle.Run.Input.Preparation != nil) || (attempt.AuditDigest != nil) != (len(workflowRunAudit(a.bundle)) > 0) {
				return workflowFinding("inventory-mismatch")
			}
			if attempt.RunDigest == nil {
				if attempt.PreparationDigest != nil || attempt.AuditDigest != nil || attempt.EvaluationErrorDigest != nil || attempt.State != "interrupted" && attempt.State != "cancelled" && af != "acquisition-failed" {
					return workflowFinding("inventory-mismatch")
				}
				continue
			}
			if a.prepared != nil && preparationDigest(a.prepared.Preparation) != stringPin(attempt.PreparationDigest) {
				return workflowFinding("inventory-mismatch")
			}
			if hasAudit && a.prepared == nil || !hasAudit && (len(a.bundle.Run.Audit) > 0 || len(a.bundle.Run.AuditBytes) > 0 || len(a.bundle.Run.Result) > 0) {
				return workflowFinding("inventory-mismatch")
			}
			switch attempt.State {
			case "interrupted", "cancelled":
				if hasAudit || attempt.EvaluationErrorDigest != nil || a.bundle.Run.State != attempt.State {
					return workflowFinding("inventory-mismatch")
				}
			case "failed":
				switch af {
				case "evaluation-error":
					if a.prepared == nil || hasAudit || a.bundle.Run.State != "failed" {
						return workflowFinding("inventory-mismatch")
					}
				case "preparation-failed", "admission-refused":
					if a.prepared != nil || hasAudit || workflowPreparationFailure(a.bundle.Run, r.stages[stageIndex].profiles) != af {
						return workflowFinding("inventory-mismatch")
					}
				case "acquisition-failed":
					if a.prepared != nil || hasAudit || a.bundle.Run.State != "failed" {
						return workflowFinding("inventory-mismatch")
					}
				case "output-invalid", "limit-exceeded":
					if a.prepared == nil || !hasAudit || a.bundle.Run.State != "completed" {
						return workflowFinding("inventory-mismatch")
					}
				default:
					return workflowFinding("inventory-mismatch")
				}
			case "completed", "completed-zero":
				if a.prepared == nil || !hasAudit || a.bundle.Run.State != "completed" || len(a.bundle.Run.Result) == 0 {
					return workflowFinding("inventory-mismatch")
				}
			}
			if !hasAudit {
				continue
			}
			resultDisposition, resultErr := exactDisposition(a.bundle.Run.Result)
			auditDisposition, auditErr := exactDisposition(a.audit)
			if resultErr != nil || auditErr != nil || len(resultDisposition) == 0 || !sameJSON(resultDisposition, auditDisposition) {
				return workflowFinding("inventory-mismatch")
			}
			outputRecords, payloads, err := workflowOutputRecords(r.w, instance, a.prepared.Preparation, a.audit, a.prepared.Facts, r.inherited[i])
			if err != nil {
				want := "output-invalid"
				if errors.Is(err, errWorkflowBound) {
					want = "limit-exceeded"
				}
				if attempt.State != "failed" || af != want {
					return workflowFinding("inventory-mismatch")
				}
				if last && want == "limit-exceeded" {
					limited[instance.Stage] = true
				}
				continue
			}
			over := records+len(outputRecords) > r.w.Document.Limits.MaxRecords || limited[instance.Stage]
			encoded := make([]workflowReadOutput, len(outputRecords))
			for k, record := range outputRecords {
				raw, encodeErr := workflowCanonical(record, 65536)
				if encodeErr != nil {
					if !errors.Is(encodeErr, errWorkflowBound) {
						return workflowFinding("inventory-mismatch")
					}
					over = true
					break
				}
				encoded[k] = workflowReadOutput{record: record, raw: raw, payload: payloads[k], path: fmt.Sprintf("outputs/%s/%s/%d/", instance.Stage, instance.ID, k), pin: digest(raw)}
			}
			if attempt.State == "failed" {
				if af != "limit-exceeded" {
					return workflowFinding("inventory-mismatch")
				}
				if !over {
					return errors.New("byte-only output refusal requires a specified admission-size proof; the draft contract does not retain one")
				}
				if last {
					limited[instance.Stage] = true
				}
				continue
			}
			if !last || over || (attempt.State == "completed-zero") != (len(encoded) == 0) || attempt.State == "completed-zero" && r.w.Document.Stages[stageIndex].Outputs.Cardinality != "many" {
				return workflowFinding("inventory-mismatch")
			}
			records += len(encoded)
			expectedOutputs[stageIndex] = append(expectedOutputs[stageIndex], encoded...)
		}
	}
	for _, path := range sortedKeys(r.in.Files) {
		if r.used[path] || r.ignored[path] || path == "expected-report.json" || path == "trust.json" || path == "README.md" || strings.HasPrefix(path, "selections/") {
			continue
		}
		if strings.HasPrefix(path, "outputs/") {
			continue
		}
		return workflowFinding("inventory-mismatch")
	}

	// Only after every inventory clause passes compare outputs, preserving the
	// prescribed phase ordering even when an earlier payload is also wrong.
	for i, want := range expectedOutputs {
		actual := r.stages[i].outputs
		if len(want) != len(actual) || len(want) != len(r.w.Execution.OutputSets[i].Records) {
			return workflowFinding("output-mismatch")
		}
		for j, output := range want {
			got := actual[j]
			if output.path != got.path || output.pin != got.pin || !sameJSON(output.raw, got.raw) || !bytes.Equal(output.payload, got.payload) {
				return workflowFinding("output-mismatch")
			}
		}
	}
	for _, path := range sortedKeys(r.in.Files) {
		if !r.used[path] && !r.ignored[path] && strings.HasPrefix(path, "outputs/") {
			return workflowFinding("output-mismatch")
		}
	}
	return nil
}
