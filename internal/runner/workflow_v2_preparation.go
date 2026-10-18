package runner

import (
	"errors"
	"time"
)

// RFC 0022's proposed writer records the exact terminal nextInput instant in
// finishedAt. Ordinary archived Runner exports do not acquire that guarantee.
// Only the closed terminal derivation codes establish a preparation failure.
func workflowV2PreparationFailure(run Run, profiles []InputProfile) string {
	if run.State != "failed" || run.Input.Source == nil || run.Input.Preparation != nil || len(run.Input.Facts) > 0 || len(run.Input.Evidence) > 0 {
		return ""
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", run.FinishedAt)
	if err != nil || at.Format("2006-01-02T15:04:05Z") != run.FinishedAt {
		return ""
	}
	plan, err := nextInput(run.Input, profiles, at)
	if err == nil || plan.Next != nil || plan.Input != nil {
		return ""
	}
	var coded verificationCoded
	if !errors.As(err, &coded) {
		return ""
	}
	switch coded.verificationCode() {
	case codeAdmission:
		return "admission-refused"
	case codeMismatch, codeParameter, codeReadFailed, codeUnexpected, codeInvalid, codeStale, codeSessions, codeCalculation:
		return "preparation-failed"
	case codeMissing:
		// Planning returns Next (without an error) for a source still to acquire.
		// Its acquisition-missing error is for a response inside a retained source.
		if len(run.Input.Source.Sources) > 0 {
			return "preparation-failed"
		}
	}
	return ""
}
