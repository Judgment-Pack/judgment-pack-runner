package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The installation supplies acquisition and evaluation through its existing
// boundaries. In particular a research callback must use Desk's shared AI
// execution/approval machinery; this driver chooses no model or permissions.
// Callbacks receive owned copies and may not retry an uncertain request.
type workflowAcquisition struct {
	Execution, Instance string
	Attempt             int
	Input               Input
	Next                PlannedSource
}
type workflowEvaluation struct {
	Result, Audit, Signatures, ErrorEnvelope []byte
}
type workflowDriver struct {
	Store    *workflowStore
	Profiles []InputProfile
	Now      func() time.Time
	Acquire  func(context.Context, workflowAcquisition) (SourceValue, error)
	Evaluate func(context.Context, Release, Input, string) (workflowEvaluation, error)
}

// Only a definite, host-observed refusal is acquisition-failed. Timeouts,
// transport errors, absent replies and unknown effects remain interrupted.
var errWorkflowAcquisitionUnavailable = errors.New("workflow source unavailable")

func (d workflowDriver) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}
func workflowOwnedInput(input Input) Input {
	var out Input
	_ = json.Unmarshal(encode(input), &out)
	return out
}

// run admits exactly one attempt, never an automatic retry. Every received
// answer is checkpointed before planning the next source or evaluating. On a
// failed write the driver stops immediately; its persisted attempt remains
// interrupted and pending until the installation's exclusive recovery pass.
func (d workflowDriver) run(ctx context.Context, execution, instanceID string, retry *workflowRetry) (workflowInventory, error) {
	w, revision, err := d.Store.load(execution)
	if err != nil {
		return w, err
	}
	w, err = beginWorkflowAttempt(w, instanceID, retry)
	if err != nil {
		return w, err
	}
	if err = d.Store.save(w, revision); err != nil {
		return w, err
	}
	revision++
	if w.Pending[instanceID] == 0 {
		return w, nil
	} // inherited bound: no acquisition
	save := func(next workflowInventory, transitionErr error) error {
		if transitionErr != nil {
			return transitionErr
		}
		if err := d.Store.save(next, revision); err != nil {
			return err
		}
		revision++
		w = next
		return nil
	}
	err = d.driveAdmitted(ctx, w, instanceID,
		func(raw []byte) error { return save(checkpointWorkflowAttempt(w, instanceID, raw, d.Profiles)) },
		func(result workflowAttemptResult) error {
			return save(finishWorkflowAttempt(w, instanceID, result, d.Profiles))
		}, nil, nil, false)
	return w, err
}

// driveAdmitted shares actual planning, acquisition and Runtime invocation. The
// writer-specific hooks own durable custody and accounting; this never begins,
// retries or independently authorizes an attempt.
func (d workflowDriver) driveAdmitted(ctx context.Context, w workflowInventory, instanceID string,
	checkpointRun func([]byte) error, finishRun func(workflowAttemptResult) error,
	received func(workflowAcquisition, SourceValue) error,
	receivedEvaluation func(Run, workflowEvaluation, error) error, preferComplete bool) error {
	var err error
	index := w.instanceIndex(instanceID)
	frozen := w.Execution.Frozen[w.stageIndex(w.Execution.Instances[index].Stage)]
	var release Release
	if err = strictJSON(w.Artifacts["release/"+frozen.ReleaseDigest], &release); err != nil {
		return err
	}
	input, err := workflowAttemptInput(w, instanceID)
	if err != nil {
		return err
	}
	stamp := d.now().Format("2006-01-02T15:04:05Z")
	run := Run{SchemaVersion: "1", ID: id("workflow-run_"), ReleaseID: release.ID, Revision: 1, State: "interrupted", Input: input, CreatedAt: stamp, StartedAt: stamp, RequestedBy: "workflow", Attempt: w.Pending[instanceID]}
	checkpoint := func() error {
		return checkpointRun(encode(verificationExport(release, run, 3)))
	}
	finish := func(state, failure string, coreError []byte) error {
		run.State = state
		if !preferComplete || (failure != "preparation-failed" && failure != "admission-refused") {
			run.FinishedAt = d.now().Format("2006-01-02T15:04:05Z")
		}
		result := workflowAttemptResult{Run: encode(verificationExport(release, run, 3)), State: state, Failure: failure, EvaluationError: coreError, Signatures: run.AuditSignatures}
		return finishRun(result)
	}
	if err = checkpoint(); err != nil {
		return err
	}
	for step := 0; step <= len(input.Source.Mapping.Sources); step++ {
		if ctx.Err() != nil {
			return finish("cancelled", "", nil)
		}
		planAt := d.now()
		plan, planErr := nextInput(run.Input, d.Profiles, planAt)
		if planErr != nil {
			run.State = "failed"
			failure := ""
			if preferComplete {
				run.FinishedAt = planAt.Format("2006-01-02T15:04:05Z")
				failure = workflowV2PreparationFailure(run, d.Profiles)
			} else {
				run.FinishedAt = d.now().Format("2006-01-02T15:04:05Z")
				failure = workflowPreparationFailure(run, d.Profiles)
			}
			if failure == "" {
				return planErr
			}
			return finish("failed", failure, nil)
		}
		if plan.Input != nil {
			run.Input = *plan.Input
			if err = checkpoint(); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return finish("cancelled", "", nil)
			}
			if d.Evaluate == nil {
				return finish("interrupted", "", nil)
			}
			// Release/sample input, arguments and facts given to the host are copies.
			var ownedRelease Release
			_ = json.Unmarshal(encode(release), &ownedRelease)
			evaluated, evalErr := d.Evaluate(ctx, ownedRelease, workflowOwnedInput(run.Input), run.ID)
			evaluated = workflowEvaluation{bytes.Clone(evaluated.Result), bytes.Clone(evaluated.Audit), bytes.Clone(evaluated.Signatures), bytes.Clone(evaluated.ErrorEnvelope)}
			if receivedEvaluation != nil {
				if err := receivedEvaluation(run, evaluated, evalErr); err != nil {
					return err
				}
			}
			if preferComplete {
				if len(evaluated.Signatures) > 0 && len(evaluated.Audit) == 0 {
					return errWorkflowTransition
				}
				if len(evaluated.ErrorEnvelope) > 0 && (len(evaluated.Result) > 0 || len(evaluated.Audit) > 0) {
					return errWorkflowTransition
				}
				if len(evaluated.Audit) > 0 && len(evaluated.ErrorEnvelope) == 0 {
					run.Result = evaluated.Result
					run.retainAudit(evaluated.Audit, evaluated.Signatures)
					return finish("completed", "", nil)
				}
				if workflowEvaluationError(evaluated.ErrorEnvelope) {
					return finish("failed", "evaluation-error", evaluated.ErrorEnvelope)
				}
			}
			if errors.Is(evalErr, errSidecarUnread) && len(evaluated.Result) > 0 && len(evaluated.Audit) > 0 {
				evalErr = nil
			}
			if evalErr != nil {
				if ctx.Err() != nil {
					return finish("cancelled", "", nil)
				}
				if workflowEvaluationError(evaluated.ErrorEnvelope) {
					return finish("failed", "evaluation-error", evaluated.ErrorEnvelope)
				}
				return finish("interrupted", "", nil)
			}
			if len(evaluated.ErrorEnvelope) > 0 {
				return errWorkflowTransition
			}
			run.Result = evaluated.Result
			run.retainAudit(evaluated.Audit, evaluated.Signatures)
			return finish("completed", "", nil)
		}
		if plan.Next == nil || d.Acquire == nil {
			return finish("failed", "acquisition-failed", nil)
		}
		if _, exists := run.Input.Source.Sources[plan.Next.Name]; exists {
			return errWorkflowTransition
		}
		// The checkpoint already committed above contains every earlier answer.
		request := workflowAcquisition{w.Execution.Execution, instanceID, run.Attempt, workflowOwnedInput(run.Input), *plan.Next}
		request.Next.Arguments = append(json.RawMessage(nil), plan.Next.Arguments...)
		value, acquireErr := d.Acquire(ctx, request)
		value = SourceValue{Response: bytes.Clone(value.Response), Snapshot: bytes.Clone(value.Snapshot)}
		if received != nil && (len(value.Response) > 0 || len(value.Snapshot) > 0) {
			if err := received(request, value); err != nil {
				return err
			}
		}
		// A callback may return a response alongside an error. Preserve any complete
		// JSON response before deciding whether the operation was interrupted.
		if len(value.Response) > 0 || len(value.Snapshot) > 0 {
			if len(value.Response) > 0 && !json.Valid(value.Response) || len(value.Snapshot) > 0 && !json.Valid(value.Snapshot) {
				return fmt.Errorf("source returned an unretainable response: %w", errWorkflowTransition)
			}
			run.Input.Source.Sources[plan.Next.Name] = value
			if err = checkpoint(); err != nil {
				return err
			}
		}
		if acquireErr != nil {
			if ctx.Err() != nil {
				return finish("cancelled", "", nil)
			}
			if errors.Is(acquireErr, errWorkflowAcquisitionUnavailable) {
				return finish("failed", "acquisition-failed", nil)
			}
			return finish("interrupted", "", nil)
		}
		if len(value.Response) == 0 && len(value.Snapshot) == 0 {
			return finish("interrupted", "", nil)
		}
	}
	return errWorkflowTransition
}
