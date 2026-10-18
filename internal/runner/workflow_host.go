package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// This adapter uses Runner's existing Gateway connection and pinned Runtime.
// It is private until the full workflow reader and public routes are ready.
// It does not implement research: Desk must use its shared executor and retain
// the resulting source record through an explicitly authorized host adapter.
func (s *Service) workflowDriver(store *workflowStore) workflowDriver {
	return workflowDriver{Store: store, Profiles: s.inputProfiles(),
		Acquire: func(ctx context.Context, request workflowAcquisition) (SourceValue, error) {
			if request.Next.Kind != "operation" {
				return SourceValue{}, errWorkflowAcquisitionUnavailable
			}
			return s.acquirePlannedOperation(ctx, request.Next)
		},
		Evaluate: func(ctx context.Context, release Release, input Input, key string) (workflowEvaluation, error) {
			root := filepath.Join(s.cfg.Dir, "workflow-attempts")
			if err := os.MkdirAll(root, 0700); err != nil {
				return workflowEvaluation{}, err
			}
			directory := filepath.Join(root, key) // key is minted internally by the driver
			result, audit, signatures, _, err := s.evaluate(ctx, release, input, directory, false)
			answer := workflowEvaluation{Result: result, Audit: audit, Signatures: signatures}
			if errors.Is(err, errSidecarUnread) {
				return answer, nil
			} // same completed decision rule as ordinary runs
			if err != nil {
				// The exact complete stdout is retained independently; diagnostics are
				// never parsed into an invented Core evaluation error.
				if raw, readErr := readLimit(filepath.Join(directory, "evaluation.stdout"), maxOutput); readErr == nil && workflowEvaluationError(raw) {
					answer.ErrorEnvelope = raw
				}
			}
			return answer, err
		},
	}
}
