package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Each identity comparison uses the artifact's own domain. In particular a
// run/error/pack is exact bytes, while cases preserve numeric literals and
// workflow envelopes use the restricted integer canon (including -0 -> 0).
func workflowArtifactDigest(kind string, raw []byte) (string, error) {
	var canonical []byte
	var err error
	switch kind {
	case "workflow", "execution", "selection", "set", "output":
		_, canonical, err = workflowValue(raw, 8<<20, 64)
	case "case", "payload":
		canonical = raw
	case "published", "bound", "binding", "profiles":
		canonical, err = proofCanon(raw)
	case "preparation":
		canonical = raw
	case "release":
		canonical, err = canonicalRelease(raw)
	default:
		canonical = raw
	}
	return digest(canonical), err
}
func workflowPinMatches(kind string, raw []byte, pin string) bool {
	got, err := workflowArtifactDigest(kind, raw)
	return err == nil && got == pin
}
func (r *workflowReader) identities() error {
	if digest(r.workflow) != r.in.WorkflowDigest || digest(r.execution) != r.in.ExecutionDigest || r.w.Execution.WorkflowDigest != r.in.WorkflowDigest || !bytes.Equal(r.file("workflow.sha256", false), []byte(r.in.WorkflowDigest+"\n")) || !bytes.Equal(r.file("execution.sha256", false), []byte(r.in.ExecutionDigest+"\n")) {
		return workflowFinding("digest-mismatch")
	}
	if !workflowPinMatches("case", r.in.Files["root-case.json"], r.w.Execution.RootCaseDigest) {
		return workflowFinding("digest-mismatch")
	}
	for i, stage := range r.w.Document.Stages {
		frozen, held := r.w.Execution.Frozen[i], &r.stages[i]
		for _, a := range []struct {
			kind string
			raw  []byte
			pin  string
		}{
			{"published", held.published, frozen.PublishedDialectDigest}, {"bound", held.bound, frozen.BoundDialectDigest},
			{"pack", held.pack, frozen.PackDigest}, {"release", held.release, frozen.ReleaseDigest},
		} {
			if !workflowPinMatches(a.kind, a.raw, a.pin) {
				return workflowFinding("digest-mismatch")
			}
		}
		if frozen.BindingDocumentDigest != nil && !workflowPinMatches("binding", held.binding, *frozen.BindingDocumentDigest) {
			return workflowFinding("digest-mismatch")
		}
		if pin := r.w.Execution.Selections[i]; pin != nil && !workflowPinMatches("selection", held.selectionRaw, *pin) {
			return workflowFinding("digest-mismatch")
		}
		snapshot, err := freezeWorkflowStage(stage, workflowFreezeInput{Published: held.published, Binding: held.binding, Release: held.releaseValue}, held.profiles, r.in.RuntimeDigest)
		if err != nil || !sameJSON(encode(snapshot.Frozen), encode(frozen)) || !sameWorkflowCanonical(snapshot.Bound, held.bound) || !bytes.Equal(snapshot.Pack, held.pack) {
			return workflowFinding("stage-binding-mismatch")
		}
		profiles := held.releaseValue.InputProfiles
		if profiles == nil {
			profiles = []InputProfile{}
		}
		if !sameWorkflowCanonical(encode(profiles), held.recordedProfiles) {
			return workflowFinding("stage-binding-mismatch")
		}
	}
	for i, instance := range r.w.Execution.Instances {
		stage := r.w.stageIndex(instance.Stage)
		held := &r.stages[stage]
		frozen := r.w.Execution.Frozen[stage]
		caseBytes := r.in.Files["instances/"+instance.ID+"/case.json"]
		if instance.CaseDigest != nil && !workflowPinMatches("case", caseBytes, *instance.CaseDigest) {
			return workflowFinding("digest-mismatch")
		}
		for j, attempt := range instance.Attempts {
			a := &r.attempts[i][j]
			for _, artifact := range []struct {
				kind string
				raw  []byte
				pin  *string
			}{
				{"run", a.run, attempt.RunDigest}, {"preparation", a.preparation, attempt.PreparationDigest},
				{"audit", a.audit, attempt.AuditDigest}, {"evaluation-error", a.evaluationError, attempt.EvaluationErrorDigest},
			} {
				if artifact.pin != nil && !workflowPinMatches(artifact.kind, artifact.raw, *artifact.pin) {
					return workflowFinding("digest-mismatch")
				}
			}
			if attempt.PreparationDigest != nil {
				base := fmt.Sprintf("instances/%s/attempts/%d/", instance.ID, attempt.Number)
				var p Preparation
				_ = strictJSON(a.preparation, &p)
				if !bytes.Equal(r.in.Files[base+"preparation.sha256"], []byte(*attempt.PreparationDigest+"\n")) || !bytes.Equal(r.in.Files[base+"verified-at.txt"], []byte(p.VerifiedAt+"\n")) {
					return workflowFinding("digest-mismatch")
				}
			}
		}
		for _, output := range held.outputs {
			if strings.HasPrefix(output.path, "outputs/"+instance.Stage+"/"+instance.ID+"/") && (output.pin != "" && !workflowPinMatches("output", output.raw, output.pin) || !workflowPinMatches("payload", output.payload, output.record.PayloadDigest)) {
				return workflowFinding("digest-mismatch")
			}
		}
		// Digest comparisons for this instance precede all binding comparisons.
		for j, attempt := range instance.Attempts {
			if attempt.RunDigest == nil {
				continue
			}
			a := &r.attempts[i][j]
			b := a.bundle
			source := b.Run.Input.Source
			sourceCase, sourceCaseErr := columnCaseBytes(source)
			if sourceCaseErr != nil || b.ReleaseDigest != frozen.ReleaseDigest || releaseDigest(b.Release) != frozen.ReleaseDigest || b.Run.ReleaseID != held.releaseValue.ID || !mappingMatchesRelease(held.releaseValue, b.Run.Input) || source == nil || !sameWorkflowCanonical(encode(source.Mapping), held.bound) || !sameJSON(sourceCase, caseBytes) {
				return workflowFinding("stage-binding-mismatch")
			}
			if len(held.binding) > 0 {
				if source.Import == nil || !sameWorkflowCanonical(source.Import.Dialect, held.published) || !sameWorkflowCanonical(source.Import.Binding, held.binding) {
					return workflowFinding("stage-binding-mismatch")
				}
			} else if source.Import != nil {
				return workflowFinding("stage-binding-mismatch")
			}
			sources := source.Sources
			if sources == nil {
				sources = map[string]SourceValue{}
			}
			if source.MappingDigest != "" && source.MappingDigest != frozen.BoundDialectDigest || len(source.Snapshot) > 0 || !sameJSON(a.sources, encode(sources)) {
				return workflowFinding("stage-binding-mismatch")
			}
			known := map[string]bool{}
			for _, s := range source.Mapping.Sources {
				known[s.Name] = true
			}
			for name := range source.Sources {
				if !known[name] {
					return workflowFinding("stage-binding-mismatch")
				}
			}
			if attempt.PreparationDigest != nil && (b.Run.Input.Preparation == nil || !sameJSON(a.preparation, encode(b.Run.Input.Preparation))) {
				return workflowFinding("stage-binding-mismatch")
			}
			var auditPackDigest string
			if len(a.audit) > 0 && (json.Unmarshal(member(a.audit, "pack", "digest"), &auditPackDigest) != nil || auditPackDigest != frozen.PackDigest) {
				return workflowFinding("stage-binding-mismatch")
			}
			if attempt.AuditDigest != nil && !bytes.Equal(a.audit, workflowRunAudit(b)) {
				return workflowFinding("stage-binding-mismatch")
			}
		}
	}
	return nil
}

func (r *workflowReader) selections() error {
	for i, stage := range r.w.Document.Stages {
		held := &r.stages[i]
		if !sameJSON(encode(held.set), encode(r.w.outputSet(i))) {
			return workflowFinding("selection-mismatch")
		}
		if held.selection == nil {
			continue
		}
		dispositions := map[string]json.RawMessage{}
		if held.selection.Mode == "disposition" {
			for _, output := range held.outputs {
				index := r.w.instanceIndex(output.record.Lineage.Instance)
				if index < 0 || len(r.attempts[index]) == 0 {
					return workflowFinding("selection-mismatch")
				}
				attempts := r.attempts[index]
				dispositions[output.pin] = member(attempts[len(attempts)-1].audit, "disposition")
			}
		}
		if _, _, err := readWorkflowSelection(held.selectionRaw, held.set, stage, r.w.Document.Limits.MaxRecords, dispositions); err != nil {
			return workflowFinding("selection-mismatch")
		}
	}
	return nil
}
