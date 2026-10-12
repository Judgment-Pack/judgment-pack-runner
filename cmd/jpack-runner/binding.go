package main

import (
	"errors"
	"fmt"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// errBindingMismatch is the refusal of a record whose binding is of Runner's
// scheme and is not the binding of the export's preparation.
var errBindingMismatch = errors.New("binding-mismatch")

// errBindingForeignKind is the refusal of a record whose binding is not of
// Runner's scheme: no digest is compared.
var errBindingForeignKind = errors.New("binding-foreign-kind")

// errUnbound is --require-bound's refusal of a run whose record binds no
// preparation.
var errUnbound = errors.New("unbound")

// bindingRefusal is the error the record's binding answers, with the status
// its report carries; nil when there is none. A binding of another scheme is
// refused before any digest is compared, and a binding of Runner's scheme that
// is not the export's preparation's is refused, whatever else is asked. A
// record with no binding is reported, and refused only with --require-bound,
// as is a run without lineage, which has no preparation to bind.
func bindingRefusal(b *runner.BindingReport, inputs string, requireBound bool) (string, error) {
	switch {
	case b != nil && b.Status == runner.BindingForeignKind:
		return "binding-foreign-kind", fmt.Errorf("%w: the record's binding is not of Runner's scheme, %s, so it commits nothing verify-run can check, and no digest was compared", errBindingForeignKind, runner.PreparationBindingKind)
	case b != nil && b.Status == runner.BindingMismatch:
		return "binding-mismatch", fmt.Errorf("%w: the record's binding is not the SHA-256 of the export's preparation: the preparation, preparation.verifiedAt included, is not the one the record binds", errBindingMismatch)
	case !requireBound:
		return "", nil
	case inputs == runner.InputsNotMapped:
		return "unbound", fmt.Errorf("%w: the run's job has no mapping v2, so its export has no preparation for its record to bind, and --require-bound refuses it", errUnbound)
	case b == nil || b.Status != runner.BindingMatches:
		return "unbound", fmt.Errorf("%w: the run's audit record carries no binding of its preparation, and --require-bound refuses it: nothing in the record commits the preparation, or preparation.verifiedAt", errUnbound)
	}
	return "", nil
}

// committed says whether the preparation, its instant included, is committed
// in the signed record: the record's binding is the export's preparation's,
// and a valid signature under the key given covers the record.
func committed(b *runner.BindingReport, s *runner.SignatureReport) bool {
	return b != nil && b.Status == runner.BindingMatches && s != nil && s.Status == runner.SignatureSigned
}

// bindingSentence is what the line on standard error says of the record's
// binding of the preparation, after what it says of the record's signature.
// It adds nothing for an export without lineage. Only a record whose binding
// matches, or that carries none, is reported on that line: the others are
// refused.
func bindingSentence(b *runner.BindingReport, s *runner.SignatureReport) string {
	switch {
	case b == nil:
		return ""
	case b.Status == runner.BindingAbsent:
		return " The run's audit record carries no binding of its preparation: the Runtime that evaluated it took none (before 0.31.0), or Runner passed none then. --require-bound refuses it."
	case b.Status != runner.BindingMatches:
		return ""
	case committed(b, s):
		return " The record's binding is the SHA-256 of the export's preparation, and the record's signature verifies: the preparation, its instant included, is the one Runner handed the Runtime for the record that was signed. Against the operator, who holds the key, that is no more than the signature is: a preparation recomputed, and its record signed again with the new binding, verifies as well; only a witnessed chain (--require-witnessed), against a checkpoint held independently from before, shows it. Nor does the binding carry a time: nothing establishes that the instant was the admission instant."
	}
	return " The record's binding is the SHA-256 of the export's preparation, but no signature checked here covers the record: the binding commits the preparation only under a record signature that verifies (--public-key)."
}
