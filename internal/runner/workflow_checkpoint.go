package runner

// Checkpoints retain partial acquisitions in the ordinary failure export. They
// carry no new permission: a pending attempt may only be driven by the host
// which committed its admission, and recovery never resumes that driver.
import (
	"bytes"
	"time"
)

func workflowRunExtends(previous, current Run) bool {
	if previous.ID != current.ID || previous.ReleaseID != current.ReleaseID || previous.JobID != current.JobID || previous.Attempt != current.Attempt || previous.CreatedAt != current.CreatedAt || previous.StartedAt != current.StartedAt || previous.RequestedBy != current.RequestedBy || previous.Input.Source == nil || current.Input.Source == nil {
		return false
	}
	a, b := previous.Input.Source, current.Input.Source
	if !sameJSON(a.Case, b.Case) || !sameJSON(encode(a.Mapping), encode(b.Mapping)) || !sameJSON(encode(a.Import), encode(b.Import)) {
		return false
	}
	for name, value := range a.Sources {
		got, ok := b.Sources[name]
		if !ok || !sameJSON(encode(value), encode(got)) {
			return false
		}
	}
	// Once preparation has been retained, it and the complete acquisition set
	// are immutable. Evaluation may add its own artifacts, never revise inputs.
	if previous.Input.Preparation != nil && !sameJSON(encode(previous.Input), encode(current.Input)) {
		return false
	}
	return true
}

func checkpointWorkflowAttempt(w workflowInventory, id string, raw []byte, profiles []InputProfile) (workflowInventory, error) {
	var bundle VerificationBundle
	if strictJSON(raw, &bundle) != nil || bundle.Run.State != "interrupted" || len(bundle.Run.Result) > 0 || len(bundle.Run.Audit) > 0 {
		return w, errWorkflowTransition
	}
	next, err := finishWorkflowAttempt(w, id, workflowAttemptResult{Run: raw, State: "interrupted"}, profiles)
	if err != nil {
		return w, err
	}
	next.Pending[id] = w.Pending[id]
	return next, nil
}

// A preparation failure is recomputed at the attempt's recorded instant. A
// missing acquisition is not a preparation failure: planning must itself fail.
func workflowPreparationFailure(run Run, profiles []InputProfile) string {
	if run.State != "failed" || run.Input.Source == nil || run.Input.Preparation != nil || len(run.Input.Facts) > 0 || len(run.Input.Evidence) > 0 {
		return ""
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", run.FinishedAt)
	if err != nil || at.Format("2006-01-02T15:04:05Z") != run.FinishedAt {
		return ""
	}
	_, err = nextInput(run.Input, profiles, at)
	if err == nil {
		return ""
	}
	_, code := verificationOutcome(err)
	if code == codeAdmission {
		return "admission-refused"
	}
	return "preparation-failed"
}

func workflowCheckpointIdentity(w workflowInventory, id string, bundle VerificationBundle) bool {
	original, err := workflowAttemptInput(w, id)
	if err != nil || bundle.Run.Input.Source == nil {
		return false
	}
	source := bundle.Run.Input.Source
	if !sameJSON(encode(source.Import), encode(original.Source.Import)) || (source.MappingDigest != "" && source.MappingDigest != mappingV2Hash(source.Mapping)) || len(source.Snapshot) > 0 {
		return false
	}
	known := map[string]bool{}
	for _, s := range original.Source.Mapping.Sources {
		known[s.Name] = true
	}
	for name := range source.Sources {
		if !known[name] {
			return false
		}
	}
	index := w.instanceIndex(id)
	attempt := w.Execution.Instances[index].Attempts[w.Pending[id]-1]
	if attempt.RunDigest != nil {
		var previous VerificationBundle
		held := w.Artifacts["run/"+*attempt.RunDigest]
		if digest(held) != *attempt.RunDigest || strictJSON(held, &previous) != nil || !workflowRunExtends(previous.Run, bundle.Run) {
			return false
		}
	}
	// Input cannot acquire a differently encoded mapping or a different case.
	expected, err := proofCanon(encode(source.Mapping))
	bound := w.Artifacts["bound/"+w.Execution.Frozen[w.stageIndex(w.Execution.Instances[index].Stage)].BoundDialectDigest]
	canonicalBound, boundErr := proofCanon(bound)
	return err == nil && boundErr == nil && bytes.Equal(expected, canonicalBound)
}

// Sidecars belong to this retained attempt, even if audit bytes are reused.
func workflowSignatureKey(instance string, number int, runDigest string) string {
	return "audit-signatures/" + digest(encode(struct {
		Instance string
		Attempt  int
		Run      string
	}{instance, number, runDigest}))
}
