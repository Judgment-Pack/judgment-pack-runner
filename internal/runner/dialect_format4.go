package runner

import (
	"encoding/json"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/dialect4"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
)

// format4 is the proposed dialect format 4.0.0 (spec/dialect.md §2.2, the
// Spec's v0.5.2). Runner reads it in verify-preparation alone, through the
// engine of the sdk/dialect package (internal/dialect4): a dialect of this
// format is read as one of format 3.0.0 is, with the case's rule, the
// parameter types, rule version 2.0.0 and its derived values added.
const format4 = "4.0.0"

// codeFormatNotAdmitted is the refusal of a dialect of format 4.0.0 on every
// path but verify-preparation's: a release, a job, a preview, a delivery or
// a workflow on one. The Spec's workflow and recording schemas do not admit
// the format yet (spec issue #324), so no record can name a preparation of
// it.
const codeFormatNotAdmitted = "dialect_format_not_admitted"

func formatNotAdmitted() error {
	return bad(codeFormatNotAdmitted, "This dialect is of the proposed format 4.0.0. Runner reads it only in jpack-runner verify-preparation, and the sdk/dialect package prepares one with no source in process: a release, a job, a preview or a delivery on it waits for the Spec's workflow and recording schemas to admit the format (spec issue #324).")
}

// profileReason is why verify-preparation refuses a dialect's profile, code
// being the refusal's: for format 4.0.0, the extended profile waits for the
// Spec to decide its literal bound (spec issue #324). The profile is read
// before anything else (§11's first phase), so the format is read here only
// to name the reason.
func profileReason(dialect []byte, code string) string {
	var top struct {
		FormatVersion string `json:"formatVersion"`
	}
	if code == "profile-unsupported" && json.Unmarshal(dialect, &top) == nil && top.FormatVersion == format4 {
		return "the extended profile of format 4.0.0 is not read until the Spec decides its literal bound (spec issue #324): verify-preparation reads the base profile"
	}
	return "retained-column profile is unsupported or malformed"
}

// maxProfileScan is the size up to which §11's first phase scans a dialect
// of format 4.0.0 for its profile declaration: twice the base profile's
// dialect bound, the extended profile's (Core §4.1). The Spec's reader
// scans no larger dialect (its admit_envelope), and the sdk/dialect package
// does the same.
const maxProfileScan = 2 * maxPublishedDialect

// profileUnscanned reports whether dialect is of format 4.0.0 and over
// maxProfileScan bytes: its profile declaration is not read, and §12's byte
// bounds refuse it input-over-bound, as the Spec's reader and the
// sdk/dialect package do. A dialect of any other format is scanned at any
// size, as before.
func profileUnscanned(dialect []byte) bool {
	if len(dialect) <= maxProfileScan {
		return false
	}
	var top struct {
		FormatVersion string `json:"formatVersion"`
	}
	return json.Unmarshal(dialect, &top) == nil && top.FormatVersion == format4
}

// engineError is err, a refusal or finding of the engine, with the code it
// names (input-malformed when it names none).
func engineError(err error) error {
	if err == nil {
		return nil
	}
	code := dialect4.CodeOf(err)
	if code == "" {
		code = codeMalformed
	}
	return withCode(code, err)
}

// rule2 reads a rule of version 2.0.0 as written and holds it to its form
// (spec/dialect.md §2.2.4), each parameter it declares to scope's type when
// scope is given.
func rule2(raw json.RawMessage, scope map[string]string, what string) (*dialect4.Rule, error) {
	v, err := dialect4.ReadJSON(raw, maxDocumentDepth)
	if err != nil {
		return nil, withCode(codeMalformed, err)
	}
	r, err := dialect4.CheckRule(v, scope, what)
	return r, engineError(err)
}

// rule2Targets are the targets a rule of version 2.0.0 writes, each once,
// in code point order, as readTargets returns a rule's.
func rule2Targets(raw json.RawMessage) ([]FactMapping, []EvidenceMapping, error) {
	r, err := rule2(raw, nil, "rule")
	if err != nil {
		return nil, nil, err
	}
	f := make([]FactMapping, 0, len(r.FactTargets))
	for _, t := range distinctSorted(r.FactTargets) {
		f = append(f, FactMapping{Target: t})
	}
	ev := make([]EvidenceMapping, 0, len(r.EvidenceTargets))
	for _, t := range distinctSorted(r.EvidenceTargets) {
		ev = append(ev, EvidenceMapping{Requirement: t})
	}
	return f, ev, nil
}

func distinctSorted(a []string) []string {
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	return sortedKeys(set)
}

// paramOf is whether v is of the parameter type typ: in format 4.0.0, of
// the types §2.2.2 gives (dialect4.OfType); otherwise paramValue's.
func paramOf(four bool, v any, typ string) bool {
	if four {
		return dialect4.OfType(v, typ)
	}
	return paramValue(v, typ)
}

// paramBounds holds a list parameter's value to §2.2.7's bounds before its
// type is checked, in format 4.0.0: input-over-bound.
func paramBounds(four bool, v any, typ string) error {
	if !four || !dialect4.IsList(typ) {
		return nil
	}
	return engineError(dialect4.ListBounds(v))
}

// claimOf is the engine's claim in the form a read's claim is merged in.
func claimOf(c dialect4.Claim) derivedClaim {
	basis := c.Basis
	if basis == nil {
		basis = []string{}
	}
	return derivedClaim{encode(c.Facts), c.Evidence, c.Status, c.Reason, basis}
}

// distinctBasis is a copy's basis as format 4.0.0 writes it: a set, in code
// point order (spec/dialect.md §6.2). Format 3.0.0 keeps each source's
// pointer once per entry, as it always has.
func distinctBasis(basis []string) []string {
	return distinctSorted(basis)
}

// read4 is what a read of a dialect of format 4.0.0 adds to its lineage
// (spec/dialect.md §2.2.6): the digest the case's copy and its rule share,
// and, for a rule of version 2.0.0, the rule and how each target was
// written.
type read4 struct {
	digest      string
	rule        *dialect4.Rule
	derivations dialect4.Derivations
}

// MarshalJSON writes a preparation of format 4.0.0 with formatVersion in
// place of version (spec/dialect.md §2.2.6); every other preparation as it
// always was.
func (p Preparation) MarshalJSON() ([]byte, error) {
	type plain Preparation
	if p.formatVersion == "" {
		return json.Marshal(plain(p))
	}
	return json.Marshal(struct {
		plain
		Version       *int   `json:"version,omitempty"`
		FormatVersion string `json:"formatVersion"`
	}{plain: plain(p), FormatVersion: p.formatVersion})
}

// MarshalJSON writes a target's lineage, with its derivation when its read
// is a rule of version 2.0.0 in format 4.0.0 (spec/dialect.md §2.2.6).
func (l TargetLineage) MarshalJSON() ([]byte, error) {
	type plain TargetLineage
	if l.derivation == nil {
		return json.Marshal(plain(l))
	}
	return json.Marshal(struct {
		plain
		Derivation *dialect4.Derivation `json:"derivation"`
	}{plain(l), l.derivation})
}

// MarshalJSON writes a source's outcome, with the case's rule's outcome on
// the case's in format 4.0.0 (spec/dialect.md §2.2.6).
func (o SourceOutcome) MarshalJSON() ([]byte, error) {
	type plain SourceOutcome
	if o.rule == nil {
		return json.Marshal(plain(o))
	}
	return json.Marshal(struct {
		plain
		Rule json.RawMessage `json:"rule"`
	}{plain(o), o.rule})
}

// caseRuleOutcome is the case outcome's rule member: the rule's claim, its
// status and its reason.
func caseRuleOutcome(c derivedClaim) json.RawMessage {
	return encode(map[string]any{"claim": c, "status": c.Status, "reason": c.Reason})
}

// format4Form holds what format 4.0.0 reads differently from format 3.0.0
// to its form, before the rest of the dialect is read as format 3.0.0's is:
// it is read offline alone, and a retained-column leaf is format 3.0.0's.
func format4Form(m InputMapping, offline bool) error {
	if !offline {
		return formatNotAdmitted()
	}
	if hasColumnMapping(m) {
		return codedProblem(codeMalformed, "Mapping v3: a copy of format 4.0.0 names a target and a source, or a requirement and a source: a retained-column leaf is read in format 3.0.0 alone")
	}
	return nil
}

// mergeRead is prepareV2Mode's merge of one read's claim into the
// preparation's facts, evidence and lineage.
type mergeRead func(name, class string, gen bool, read SourceRead, claim derivedClaim, deps []Dependency, cite *Citation, receipts *[]Citation, calc *CalculationLineage, transformations map[string]*retainedcolumns.Transformation, x *read4) error

// prepareCase4 prepares the case of a dialect of format 4.0.0
// (spec/dialect.md §2.2.1, §2.2.6): its copy, then its rule over the case
// with the case's parameters and runAt, each merged as a read of the class
// asserted whose read digest is the digest of the two together, and the
// case's outcome, with the rule's outcome as its rule.
func prepareCase4(m InputMapping, read SourceRead, caseValue any, caseParams map[string]any, prep *Preparation, merge mergeRead) error {
	both, err := proofCanon(encode(SourceRead{Copy: read.Copy, Rule: m.caseRule}))
	if err != nil {
		return withCode(codeMalformed, err)
	}
	shared := digest(both)
	copied, err := deriveRead(read, caseValue, caseParams)
	if err != nil {
		return err
	}
	copied.Basis = distinctBasis(copied.Basis)
	if err = merge("case", "asserted", false, read, copied, nil, nil, nil, nil, nil, &read4{digest: shared}); err != nil {
		return err
	}
	outcome := SourceOutcome{Name: "case", Status: copied.Status, Reason: copied.Reason, Parameters: encode(caseParams), Claim: encode(copied)}
	if m.caseRule != nil {
		rule, err := rule2(m.caseRule, nil, "the case")
		if err != nil {
			return err
		}
		ruled, derivations, err := rule.Apply(caseValue, caseParams)
		if err != nil {
			return engineError(err)
		}
		claim := claimOf(ruled)
		if err = merge("case", "asserted", false, SourceRead{Rule: m.caseRule}, claim, nil, nil, nil, nil, nil, &read4{digest: shared, rule: rule, derivations: derivations}); err != nil {
			return err
		}
		outcome.rule = caseRuleOutcome(claim)
	}
	prep.Outcomes = append(prep.Outcomes, outcome)
	return nil
}
