package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

func (r *workflowReader) output(pin string) (workflowReadOutput, bool) {
	for _, stage := range r.stages {
		for _, o := range stage.outputs {
			if o.pin == pin {
				return o, true
			}
		}
	}
	return workflowReadOutput{}, false
}
func (r *workflowReader) casesAndInfluence() error {
	r.inherited = make([]map[workflowTarget]workflowInfluence, len(r.w.Execution.Instances))
	r.admissionRefused = make([]bool, len(r.w.Execution.Instances))
	// Parent summaries come from recomputation, not from claimed envelope lineage.
	influences := map[string]workflowInfluence{}
	for i, instance := range r.w.Execution.Instances {
		stageIndex := r.w.stageIndex(instance.Stage)
		stage := r.w.Document.Stages[stageIndex]
		projected := r.in.Files["root-case.json"]
		inherited := map[workflowTarget]workflowInfluence{}
		var parent workflowReadOutput
		if !stage.Input.Root {
			var ok bool
			parent, ok = r.output(stringPin(instance.ParentRecord))
			if !ok {
				return workflowFinding("case-mismatch")
			}
			var err error
			projected, err = projectWorkflowCase(parent.payload, stage.CaseBindings)
			if err != nil {
				if errors.Is(err, errWorkflowBound) {
					return workflowFinding("case-mismatch")
				}
				if instance.State != "failed" || stringPin(instance.Failure) != "projection-invalid" || instance.CaseDigest != nil || len(instance.Attempts) != 0 {
					return workflowFinding("case-mismatch")
				}
				continue
			}
		}
		if stringPin(instance.Failure) == "projection-invalid" {
			return workflowFinding("case-mismatch")
		}
		if instance.CaseDigest == nil {
			if instance.State != "interrupted" && instance.State != "cancelled" {
				return workflowFinding("case-mismatch")
			}
			if len(instance.Attempts) != 0 {
				return workflowFinding("case-mismatch")
			}
			continue
		}
		_, canonical, err := workflowCase(projected)
		if err != nil || !bytes.Equal(canonical, r.in.Files["instances/"+instance.ID+"/case.json"]) {
			return workflowFinding("case-mismatch")
		}
		if !stage.Input.Root {
			summary, ok := influences[stringPin(instance.ParentRecord)]
			if !ok {
				return workflowFinding("influence-mismatch")
			}
			var mapping InputMapping
			_ = json.Unmarshal(r.stages[stageIndex].bound, &mapping)
			inherited, err = inheritedWorkflowInfluence(mapping, stage.CaseBindings, parent.payload, summary)
			if err != nil {
				if !errors.Is(err, errWorkflowInfluenceAdmission) || instance.State != "failed" || stringPin(instance.Failure) != "admission-refused" || len(instance.Attempts) != 0 {
					return workflowFinding("influence-mismatch")
				}
				r.admissionRefused[i] = true
				continue
			}
		}
		r.inherited[i] = inherited
		// Each instance's influence is compared before the next case, even when
		// another semantic output clause will later fail phase 4.
		var prepared *Input
		if n := len(r.attempts[i]); n > 0 {
			prepared = r.attempts[i][n-1].prepared
		}
		for _, output := range r.stages[stageIndex].outputs {
			if !strings.HasPrefix(output.path, "outputs/"+instance.Stage+"/"+instance.ID+"/") {
				continue
			}
			if prepared == nil {
				return workflowFinding("influence-mismatch")
			}
			pointer := stage.Outputs.Pointer
			if stage.Outputs.Cardinality == "many" {
				parts := strings.Split(strings.TrimSuffix(output.path, "/"), "/")
				index, indexErr := strconv.Atoi(parts[len(parts)-1])
				if indexErr != nil {
					return workflowFinding("influence-mismatch")
				}
				pointer += "/" + strconv.Itoa(index)
			}
			summary, err := outputWorkflowInfluence(pointer, prepared.Preparation.Lineage, inherited)
			if err != nil || !sameJSON(encode(summary), encode(output.record.Lineage.Influence)) {
				return workflowFinding("influence-mismatch")
			}
			influences[output.pin] = summary
		}
	}
	return nil
}
