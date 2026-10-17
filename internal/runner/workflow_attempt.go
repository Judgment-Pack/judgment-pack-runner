package runner

// Private attempt custody. The host commits begin before acquisition and calls
// recover only after obtaining its exclusive installation lock. These functions
// perform no acquisition and never retry an uncertain side effect themselves.
import (
	"bytes"
	"errors"
	"time"
	"unicode/utf8"
)

func (w workflowInventory) instanceIndex(id string) int {
	for i, instance := range w.Execution.Instances {
		if instance.ID == id {
			return i
		}
	}
	return -1
}
func (w workflowInventory) consumed(stage int) bool {
	pin := w.Execution.Selections[stage]
	if pin == nil {
		return false
	}
	var selection workflowSelection
	return strictJSON(w.Artifacts["selection/"+*pin], &selection) != nil || selection.Refusal == ""
}
func beginWorkflowAttempt(w workflowInventory, id string, retry *workflowRetry) (workflowInventory, error) {
	index := w.instanceIndex(id)
	if index < 0 || w.Pending[id] != 0 {
		return w, errWorkflowTransition
	}
	instance := w.Execution.Instances[index]
	stage := w.stageIndex(instance.Stage)
	if stage < 0 || w.consumed(stage) {
		return w, errWorkflowConsumed
	}
	if !workflowPriorSettled(w, index) {
		return w, errWorkflowTransition
	}
	if instance.State != "interrupted" && instance.State != "failed" {
		return w, errWorkflowTransition
	}
	for _, earlier := range w.Execution.Instances[:index] {
		if earlier.Stage == instance.Stage && earlier.Failure != nil && *earlier.Failure == "limit-exceeded" {
			next := w.copy()
			next.Execution.Instances[index].State = "failed"
			next.Execution.Instances[index].Failure = workflowPin("limit-exceeded")
			if err := next.checkBounds(); err != nil {
				return w, err
			}
			return next, nil
		}
	}
	if instance.CaseDigest == nil {
		return w, errWorkflowTransition
	}
	if instance.Failure != nil && (*instance.Failure == "admission-refused" || *instance.Failure == "projection-invalid") {
		return w, errWorkflowTransition
	}
	if len(instance.Attempts) >= w.Document.Limits.MaxAttempts {
		return w, errWorkflowBound
	}
	if len(instance.Attempts) == 0 {
		if retry != nil {
			return w, errWorkflowForm
		}
	} else {
		if retry == nil || len(retry.Actor) < 1 || len(retry.Actor) > 256 || !utf8.ValidString(retry.Actor) {
			return w, errWorkflowForm
		}
		at, err := time.Parse("2006-01-02T15:04:05Z", retry.At)
		if err != nil || at.Format("2006-01-02T15:04:05Z") != retry.At {
			return w, errWorkflowForm
		}
	}
	// This private writer commits in instance order. It does not rewrite later
	// completed siblings to admit an earlier retry; that needs a new branch.
	for _, later := range w.Execution.Instances[index+1:] {
		if len(later.Attempts) > 0 || w.Pending[later.ID] != 0 {
			return w, errWorkflowTransition
		}
	}
	next := w.copy()
	attempt := workflowAttempt{Number: len(instance.Attempts) + 1, State: "interrupted"}
	if retry != nil {
		held := *retry
		attempt.Retry = &held
	}
	next.Execution.Instances[index].Attempts = append(next.Execution.Instances[index].Attempts, attempt)
	next.Execution.Instances[index].State = "interrupted"
	next.Execution.Instances[index].Failure = nil
	next.Pending[id] = attempt.Number
	if err := next.checkBounds(); err != nil {
		return w, err
	}
	return next, nil
}

// Global budgets are committed in document/instance order. A blocked earlier
// stage may later acquire instances, so absence from today's instance slice is
// not evidence that its work has finished. Parallel acquisition will require a
// separate durable result buffer; this writer conservatively serializes it.
func workflowPriorSettled(w workflowInventory, index int) bool {
	stage := w.stageIndex(w.Execution.Instances[index].Stage)
	for i := 0; i < stage; i++ {
		if w.Execution.OutputSets[i].Status != "closed" {
			return false
		}
	}
	for _, earlier := range w.Execution.Instances[:index] {
		if w.Pending[earlier.ID] != 0 || len(earlier.Attempts) == 0 && earlier.State == "interrupted" {
			return false
		}
	}
	return true
}

func recoverWorkflowAttempts(w workflowInventory) workflowInventory {
	next := w.copy()
	// Every active attempt was committed as interrupted before its side effects.
	// Clearing host activity cannot invent a completed answer or enqueue a retry.
	next.Pending = map[string]int{}
	return next
}

// workflowAttemptInput reconstructs only the frozen mapping and projected case;
// no parent's entire facts, root case or earlier responses leak into a child.
func workflowAttemptInput(w workflowInventory, id string) (Input, error) {
	index := w.instanceIndex(id)
	if index < 0 || w.Pending[id] == 0 {
		return Input{}, errWorkflowTransition
	}
	instance := w.Execution.Instances[index]
	if instance.CaseDigest == nil {
		return Input{}, errWorkflowTransition
	}
	frozen := w.Execution.Frozen[w.stageIndex(instance.Stage)]
	var mapping InputMapping
	if strictJSON(w.Artifacts["bound/"+frozen.BoundDialectDigest], &mapping) != nil {
		return Input{}, errWorkflowTransition
	}
	source := &SourceInput{Case: bytes.Clone(w.Artifacts["case/"+*instance.CaseDigest]), Mapping: mapping, Sources: map[string]SourceValue{}}
	if frozen.BindingDocumentDigest != nil {
		source.Import = &DialectImport{Dialect: bytes.Clone(w.Artifacts["published/"+frozen.PublishedDialectDigest]), Binding: bytes.Clone(w.Artifacts["binding/"+*frozen.BindingDocumentDigest])}
	}
	return Input{Source: source}, nil
}

type workflowAttemptResult struct {
	Run []byte
	// Only an exact complete Runtime stdout can establish evaluation-error.
	EvaluationError []byte
	// Interrupted/cancelled and acquisition availability are host assertions.
	State, Failure string
}

func finishWorkflowAttempt(w workflowInventory, id string, result workflowAttemptResult, profiles []InputProfile) (workflowInventory, error) {
	index := w.instanceIndex(id)
	if index < 0 || w.Pending[id] == 0 {
		return w, errWorkflowTransition
	}
	instance := w.Execution.Instances[index]
	stage := w.stageIndex(instance.Stage)
	frozen := w.Execution.Frozen[stage]
	if w.consumed(stage) || len(instance.Attempts) != w.Pending[id] {
		return w, errWorkflowTransition
	}
	if !workflowPriorSettled(w, index) {
		return w, errWorkflowTransition
	}
	next := w.copy()
	attempt := &next.Execution.Instances[index].Attempts[len(instance.Attempts)-1]
	if attempt.RunDigest != nil || attempt.PreparationDigest != nil || attempt.AuditDigest != nil {
		return w, errWorkflowTransition
	}
	attempt.State, attempt.Failure = result.State, nil
	var bundle VerificationBundle
	var prepared Input
	if len(result.Run) > 0 {
		if len(result.Run) > MaxExportSize {
			return w, errWorkflowBound
		}
		if strictJSON(result.Run, &bundle) != nil || !sameJSON(result.Run, encode(bundle)) || exportWithinLimits(result.Run, bundle) != nil || (bundle.Version != 2 && bundle.Version != 3) || bundle.ReleaseDigest != frozen.ReleaseDigest || releaseDigest(bundle.Release) != frozen.ReleaseDigest || bundle.Run.ReleaseID != bundle.Release.ID || bundle.Run.Input.Source == nil || !mappingMatchesRelease(bundle.Release, bundle.Run.Input) {
			return w, errWorkflowTransition
		}
		// Structural checks apply even when preparation never completed. An
		// unfinished export cannot smuggle an audit in its base64 side member.
		if checkAuditBytes(bundle) != nil || checkChainEntry(bundle) != nil || checkAuditSignatures(bundle) != nil {
			return w, errWorkflowTransition
		}
		if bundle.Run.Input.Preparation == nil && (bundle.Run.State == "completed" || len(bundle.Run.Audit) > 0 || len(bundle.Run.Result) > 0) {
			return w, errWorkflowTransition
		}
		_, caseBytes, err := workflowCase(bundle.Run.Input.Source.Case)
		if err != nil || instance.CaseDigest == nil || digest(caseBytes) != *instance.CaseDigest {
			return w, errWorkflowTransition
		}
		if bundle.Run.Input.Preparation != nil {
			verified, err := VerifyInputs(result.Run, profiles, frozen.ReleaseDigest)
			if err != nil {
				return w, err
			}
			prepared = verified.prepared
			if len(bundle.Run.Audit) > 0 && (verified.Binding == nil || verified.Binding.Status != BindingMatches) {
				return w, errWorkflowTransition
			}
			preparationBytes, err := canonical(encode(prepared.Preparation))
			if err != nil {
				return w, err
			}
			attempt.PreparationDigest = workflowPin(next.keep("preparation", preparationBytes))
		}
		attempt.RunDigest = workflowPin(next.keep("run", result.Run))
		if audit := workflowRunAudit(bundle); len(audit) > 0 {
			attempt.AuditDigest = workflowPin(next.keep("audit", audit))
		}
	}
	switch result.State {
	case "completed":
		if len(result.Run) == 0 || bundle.Run.State != "completed" || attempt.PreparationDigest == nil || attempt.AuditDigest == nil || len(result.EvaluationError) != 0 || result.Failure != "" {
			return w, errWorkflowTransition
		}
		inherited := map[workflowTarget]workflowInfluence{}
		if instance.ParentRecord != nil {
			parent, payload, err := w.record(*instance.ParentRecord)
			if err != nil {
				return w, err
			}
			inherited, err = inheritedWorkflowInfluence(bundle.Run.Input.Source.Mapping, w.Document.Stages[stage].CaseBindings, payload, parent.Lineage.Influence)
			if err != nil {
				return w, err
			}
		}
		records, payloads, err := workflowOutputRecords(w, instance, prepared.Preparation, workflowRunAudit(bundle), prepared.Facts, inherited)
		if err != nil {
			failure := "output-invalid"
			if errors.Is(err, errWorkflowBound) {
				failure = "limit-exceeded"
			}
			attempt.State, attempt.Failure = "failed", workflowPin(failure)
		} else {
			current := 0
			for _, set := range w.Execution.OutputSets {
				current += len(set.Records)
			}
			limited := current+len(records) > w.Document.Limits.MaxRecords
			for _, earlier := range w.Execution.Instances[:index] {
				if earlier.Stage == instance.Stage && earlier.Failure != nil && *earlier.Failure == "limit-exceeded" {
					limited = true
				}
			}
			if limited {
				attempt.State, attempt.Failure = "failed", workflowPin("limit-exceeded")
			} else {
				if len(records) == 0 {
					attempt.State = "completed-zero"
				}
				for i, record := range records {
					raw, err := workflowCanonical(record, 65536)
					if err != nil {
						if !errors.Is(err, errWorkflowBound) {
							return w, err
						}
						attempt.State, attempt.Failure = "failed", workflowPin("limit-exceeded")
						next.Execution.OutputSets[stage].Records = append([]string{}, w.Execution.OutputSets[stage].Records...)
						break
					}
					next.Execution.OutputSets[stage].Records = append(next.Execution.OutputSets[stage].Records, next.keep("output", raw))
					next.keep("payload", payloads[i])
				}
			}
		}
	case "interrupted", "cancelled":
		if result.Failure != "" || len(result.EvaluationError) != 0 || len(bundle.Run.Audit) > 0 || len(bundle.Run.Result) > 0 {
			return w, errWorkflowTransition
		}
	case "failed":
		switch result.Failure {
		case "evaluation-error":
			if !workflowEvaluationError(result.EvaluationError) || len(bundle.Run.Audit) > 0 || len(bundle.Run.Result) > 0 || attempt.PreparationDigest == nil {
				return w, errWorkflowTransition
			}
			attempt.EvaluationErrorDigest = workflowPin(next.keep("evaluation-error", result.EvaluationError))
		case "acquisition-failed":
			if len(result.EvaluationError) > 0 || attempt.PreparationDigest != nil || len(bundle.Run.Audit) > 0 || len(bundle.Run.Result) > 0 {
				return w, errWorkflowTransition
			}
		default:
			return w, errWorkflowTransition
		}
		attempt.Failure = workflowPin(result.Failure)
	default:
		return w, errWorkflowTransition
	}
	next.Execution.Instances[index].State, next.Execution.Instances[index].Failure = attempt.State, attempt.Failure
	delete(next.Pending, id)
	if err := next.checkBounds(); err != nil {
		if !errors.Is(err, errWorkflowBound) || result.State != "completed" {
			return w, err
		}
		// Never discard retained attempts to fit. Remove only this attempt's output
		// set additions and retain an explicit bound failure if its artifacts fit.
		next.Execution.OutputSets[stage].Records = append([]string{}, w.Execution.OutputSets[stage].Records...)
		attempt.State, attempt.Failure = "failed", workflowPin("limit-exceeded")
		next.Execution.Instances[index].State, next.Execution.Instances[index].Failure = attempt.State, attempt.Failure
		if err := next.checkBounds(); err != nil {
			return w, err
		}
	}
	return next, nil
}

func workflowRunAudit(bundle VerificationBundle) []byte {
	if bundle.Version == 2 {
		return bundle.Run.Audit
	}
	return bundle.Run.AuditBytes
}
