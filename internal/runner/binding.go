package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The preparation's binding (runner #55, runtime ADR-0050).
//
// A mapping-v2 run's preparation judges freshness against one instant, which
// it retains as preparation.verifiedAt, and nothing Runner keeps signs it: the
// record signature covers the audit record's line, which carries the facts,
// the evidence and the citations, not the preparation. From Runtime 0.31.0 on,
// an operational evaluation takes a caller's binding, {"kind","digest"}, and
// the record carries it as given, under the chain and the record signature.
//
// Runner passes, on every operational evaluation of a mapping-v2 run, the
// binding of kind PreparationBindingKind whose digest is the SHA-256 of the
// run's preparation in the canonical form a frozen release is digested by
// (canonical: members sorted by name, no insignificant whitespace, numbers as
// written, strings escaped as Go's encoding/json escapes them). A verifier
// recomputes it from the export's run.input.preparation alone. It never passes
// one on a rehearsal, which records nothing, nor to a Runtime before 0.31.0,
// which has no such input: such a record carries none.
//
// verify-run compares the record's binding as recorded: the member's bytes,
// in the record's own order (kind, then digest), never re-canonicalised. A
// binding of another kind is refused before any digest is compared. What a
// binding that matches establishes is what the record's signature does,
// against the operator no more: the operator holds the key, and can recompute
// a preparation, write a record with its binding and sign it again. Only a
// checkpoint held independently from before shows that. The binding carries no
// time of its own.

// PreparationBindingKind is the kind of the binding Runner passes: the name of
// its scheme, which the Runtime records as given.
const PreparationBindingKind = "judgment-pack-runner/preparation/1"

// bindingFile is the binding's file in an attempt's private directory, beside
// the facts and citations, kept as they are.
const bindingFile = "binding.json"

// preparationDigest is the SHA-256 of a preparation in the canonical form
// Runner digests a frozen release by (releaseDigest).
func preparationDigest(p *Preparation) string {
	b, _ := canonical(encode(p))
	return digest(b)
}

// bindingDocument is the binding Runner passes for a preparation's digest,
// exactly as the Runtime records it: compact, kind then digest. Both values
// are ASCII that JSON writes without an escape, so these bytes are the
// record's member.
func bindingDocument(preparationDigest string) []byte {
	return []byte(`{"kind":"` + PreparationBindingKind + `","digest":"` + preparationDigest + `"}`)
}

// RunBinding says what Runner passed the Runtime of a mapping-v2 run's
// operational evaluation: the preparation's binding, which the record then
// carries, or none, to a Runtime that takes none.
type RunBinding struct {
	Status string `json:"status"`
}

// The statuses of a run's binding.
const (
	// RunBound is a run whose Runtime was given the preparation's binding
	// and whose record carries it, byte for byte.
	RunBound = "bound"
	// RunBindingUnsupported is a run whose Runtime names a release before
	// 0.31.0, or names none: it takes no binding, and was given none.
	RunBindingUnsupported = "unsupported-runtime"
)

// runBindingRefused is the Reason of a run whose Runtime refused the binding
// Runner passed, and so evaluated nothing.
const runBindingRefused = "binding-refused"

// errBindingRefused is an operational evaluation whose Runtime refused the
// binding Runner passed (JPS-INVOCATION-BINDING): a defect of Runner, since
// it writes the binding to the Runtime's grammar. The run fails, and is never
// evaluated again without the binding.
var errBindingRefused = errors.New("The Runtime refused the preparation's binding Runner passed, which is a defect of Runner: the run was not evaluated. Inspect the retained attempt diagnostics.")

// errRuntimeVersionUnread is an operational evaluation of a mapping-v2 run
// whose Runtime did not say its release: whether it takes a binding cannot be
// told, so the run is not evaluated, rather than evaluated unbound.
var errRuntimeVersionUnread = errors.New("The Runtime did not say its release, so whether it takes the preparation's binding cannot be told: the run was not evaluated. Inspect the retained attempt diagnostics.")

// errRecordBinding is an operational record whose binding is not the one
// Runner passed: another, or one when none was passed, or none when one was.
var errRecordBinding = errors.New("operational audit binding does not match this invocation")

// takesBinding says whether the Runtime at bin takes --binding, from the
// release it names in its own `version` answer, invoked in dir: 0.31.0 and
// later do. A Runtime that names no release, as one built without its
// release's version does (0.0.0-dev), is taken to name one before. An answer
// that cannot be read is an error, never taken for an old Runtime.
func takesBinding(ctx context.Context, bin, dir string) (bool, error) {
	out, err := invoke(ctx, bin, dir, "version", "version", "--format", "json")
	var answer struct {
		Tool struct {
			Version string `json:"version"`
		} `json:"tool"`
	}
	if err != nil || json.Unmarshal(out, &answer) != nil {
		return false, errRuntimeVersionUnread
	}
	var major, minor, patch int
	if _, err = fmt.Sscanf(answer.Tool.Version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return false, errRuntimeVersionUnread
	}
	return major > 0 || minor >= 31, nil
}

// refusedBinding says whether a Runtime's answer refuses its binding: one of
// its diagnostics is JPS-INVOCATION-BINDING.
func refusedBinding(out []byte) bool {
	var answer struct {
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(out, &answer) != nil {
		return false
	}
	for _, d := range answer.Diagnostics {
		if d.Code == "JPS-INVOCATION-BINDING" {
			return true
		}
	}
	return false
}

// ErrBindingMemberName means an audit record has a member that equals binding
// except for case, which a case-insensitive JSON reader could read instead.
var ErrBindingMemberName = errors.New("binding-member-name: the audit record has a member that equals binding except for case")

// recordBinding is the binding an audit record carries, as its bytes are: the
// value of its member named exactly binding, nil when it has none. A record
// whose members cannot be read, one member named twice, or one with a member
// named binding but for case, is an error: never taken for no binding.
func recordBinding(record []byte) (json.RawMessage, error) {
	members, err := exactMembers(record)
	if err != nil {
		return nil, fmt.Errorf("the audit record's members cannot be read, so its binding cannot be: %w", err)
	}
	for name := range members {
		if name != "binding" && strings.EqualFold(name, "binding") {
			return nil, ErrBindingMemberName
		}
	}
	return members["binding"], nil
}

// BindingReport is what verify-run found of a mapping-v2 run's record's
// binding of its preparation.
type BindingReport struct {
	Status string `json:"status"`
}

// The statuses of a record's binding.
const (
	// BindingMatches is a record whose binding is, byte for byte, the one
	// Runner passes for the export's preparation.
	BindingMatches = "matches"
	// BindingMismatch is a record whose binding is of Runner's kind and is not
	// that one: its digest is another's, or its bytes are not as Runner passes
	// them.
	BindingMismatch = "mismatch"
	// BindingAbsent is a record with no binding: recorded by a Runtime before
	// 0.31.0, or before Runner passed one, or a run with no record.
	BindingAbsent = "absent"
	// BindingForeignKind is a record whose binding's kind is not Runner's, or
	// cannot be read as a kind: no digest is compared.
	BindingForeignKind = "foreign-kind"
)

// checkBinding holds a mapping-v2 export's record's binding to the export's
// preparation: the record's exact bytes in versions 3 to 5, and in version 2,
// which carries none, the record as the export carries it. The kind is read
// first, and a binding of another kind is reported so before any digest is
// compared. Of Runner's kind, the member's bytes are compared, as recorded,
// with the binding Runner passes for the SHA-256 of the export's preparation.
func checkBinding(b VerificationBundle) (*BindingReport, error) {
	record := []byte(b.Run.Audit)
	if b.Version >= 3 {
		record = b.Run.AuditBytes
	}
	if len(record) == 0 {
		return &BindingReport{Status: BindingAbsent}, nil
	}
	raw, err := recordBinding(record)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return &BindingReport{Status: BindingAbsent}, nil
	}
	var kind string
	members, err := exactMembers(raw)
	if err != nil || memberString(members["kind"], &kind) != nil || kind != PreparationBindingKind {
		return &BindingReport{Status: BindingForeignKind}, nil
	}
	if !bytes.Equal(raw, bindingDocument(preparationDigest(b.Run.Input.Preparation))) {
		return &BindingReport{Status: BindingMismatch}, nil
	}
	return &BindingReport{Status: BindingMatches}, nil
}
