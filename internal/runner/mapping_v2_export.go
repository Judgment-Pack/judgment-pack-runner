package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type VerificationBundle struct {
	Version       int     `json:"version"`
	ReleaseDigest string  `json:"releaseDigest"`
	Release       Release `json:"release"`
	Run           Run     `json:"run"`
	// Inputs is InputsNotMapped in the export of a run whose job has no input
	// mapping or a v1 file mapping, which has no lineage (notMappedExport). It
	// is omitted from a mapping-v2 run's export, which is byte for byte what it
	// was before it existed.
	Inputs string `json:"inputs,omitempty"`
	// Chain is the run's entry in the installation's chain of runs, in
	// versions 4 and 5. It is omitted from versions 2 and 3, which are byte for
	// byte what they were before it existed.
	Chain *RunChainExport `json:"chain,omitempty"`
}

func releaseDigest(r Release) string { b, _ := canonical(encode(r)); return digest(b) }

// VerifyRun replays input preparation offline. Profiles and releaseDigest must
// come from a separate trusted channel, never implicitly from the export.
// The result is an input-lineage finding, not verification of policy truth,
// sealed-session completeness, or a re-execution of Runtime.
func VerifyRun(raw []byte, profiles []InputProfile, trustedReleaseDigest string) error {
	_, e := VerifyInputs(raw, profiles, trustedReleaseDigest)
	return e
}

// InputClasses counts a run's fact and evidence targets by the class its
// lineage records for each: the class of the source the target is mapped from.
// Every target is counted, including one the run has no value for and one whose
// source was skipped because a dependency was unavailable, which has no receipt.
// An asserted target was typed into the case or read from a local file: nothing
// but the export vouches for it. A record or generated target of a source that
// was acquired was derived from a signed response. Verification checks the
// signature and derives the target again from that response and the retained
// parameters. A parameter that a rule reads is not signed unless the source's
// own request carries it unambiguously, as a whole value and not in text that
// names another parameter, or a calculator's signed answer echoes it; one that
// is not can change such a target without failing verification. Parameters are
// dependencies, not targets, and are not counted.
type InputClasses struct {
	Asserted  int `json:"asserted"`
	Record    int `json:"record"`
	Generated int `json:"generated"`
}

// classesOf counts lineage entries by class. Preparation records no other class:
// the case and a local file are asserted, and a profile is record or generated.
func classesOf(lineage []TargetLineage) (c InputClasses) {
	for _, l := range lineage {
		switch l.Class {
		case "asserted":
			c.Asserted++
		case "record":
			c.Record++
		case "generated":
			c.Generated++
		}
	}
	return c
}

// VerifiedRun is an export whose inputs verified: its targets by class, the
// parameters its acquired sources' rules read unsigned, and the inputs that
// were recomputed, which a re-execution evaluates.
type VerifiedRun struct {
	Classes  InputClasses
	Unsigned []UnsignedParameters
	bundle   VerificationBundle
	prepared Input
}

// VerifyInputs is VerifyRun, and returns what it verified. The classes are
// counted from the recomputed lineage, which is the retained lineage: an export
// whose lineage differs from the recomputation is refused.
func VerifyInputs(raw []byte, profiles []InputProfile, trustedReleaseDigest string) (VerifiedRun, error) {
	b, prepared, e := verifyInputs(raw, profiles, trustedReleaseDigest)
	if e != nil {
		return VerifiedRun{}, e
	}
	if b.Inputs == InputsNotMapped {
		// No lineage records a class for any target, and no rule reads a
		// parameter: there is nothing to count.
		return VerifiedRun{bundle: b, prepared: prepared}, nil
	}
	return VerifiedRun{classesOf(prepared.Preparation.Lineage), unsignedParameters(b.Run.Input.Source.Mapping, prepared.Preparation), b, prepared}, nil
}

// Inputs is InputsNotMapped for an export of a run whose job has no input
// mapping or a v1 file mapping: it has no lineage, its Classes count nothing
// and nothing derived its inputs from a source. It is empty for a mapping-v2
// run's export, whose inputs were derived again from its lineage.
func (v VerifiedRun) Inputs() string { return v.bundle.Inputs }

// ErrAuditDispositionMismatch means the retained answer differs from the audit record.
var ErrAuditDispositionMismatch = errors.New("audit-disposition-mismatch: the retained result disposition differs from the audit record")

// ErrDispositionMemberName means a result or audit object has a case-sibling
// of disposition, which case-insensitive JSON readers could read instead.
var ErrDispositionMemberName = errors.New("disposition-member-name: a run result or audit member equals disposition except for case")

// ErrDispositionDiffers means that the release's Runtime, given a run's
// verified inputs again, decided other than the run's record says it did.
var ErrDispositionDiffers = errors.New("the retained disposition differs from a re-execution of the verified inputs by the release's Runtime")

// VerifyDisposition verifies a run's inputs as VerifyRun does, evaluates them
// again with the executable at runtime, and compares the canonical disposition
// with the one the run's result and its audit record retain. The executable
// must be the Runtime the release froze: its bytes are hashed, and the copy
// that was hashed is the one run. The evaluation is a rehearsal in a private
// temporary directory, removed after, so nothing is written to a runner's store
// and no audit record is appended; a Runtime that leaves one anyway is refused.
func VerifyDisposition(ctx context.Context, raw []byte, profiles []InputProfile, trustedReleaseDigest, runtime string) error {
	v, e := VerifyInputs(raw, profiles, trustedReleaseDigest)
	if e != nil {
		return e
	}
	return v.Disposition(ctx, runtime)
}

// Disposition is VerifyDisposition's check of a run whose inputs verified.
func (v VerifiedRun) Disposition(ctx context.Context, runtime string) error {
	b, prepared := v.bundle, v.prepared
	if b.Run.State != "completed" {
		return errors.New("only a completed run has a disposition to check")
	}
	dir, e := os.MkdirTemp("", "jpack-verify-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	bin, hash, e := pinExecutable(runtime, dir)
	if e != nil {
		return e
	}
	if hash != b.Release.RuntimeDigest {
		return errors.New("this executable is not the Runtime the release froze")
	}
	work := filepath.Join(dir, "evaluation")
	out, _, _, e := evaluateWith(ctx, bin, b.Release, Input{Facts: prepared.Facts, Evidence: prepared.Evidence}, work, true, "")
	if e != nil {
		return e
	}
	if _, e = os.Lstat(filepath.Join(work, "audit")); !errors.Is(e, fs.ErrNotExist) {
		return errors.New("the re-execution left an audit record")
	}
	fresh := member(out, "disposition")
	if len(fresh) == 0 {
		return errors.New("the re-execution returned no disposition")
	}
	if !sameJSON(fresh, member(b.Run.Result, "disposition")) || !sameJSON(fresh, member(b.Run.Audit, "disposition")) {
		return ErrDispositionDiffers
	}
	return nil
}

// member reads a member of a retained JSON object by its exact name, as a
// reader of the export reads it. Go's decoder would also take a member whose
// name differs only in case, and the last of two such members.
func member(raw json.RawMessage, path ...string) json.RawMessage {
	for _, name := range path {
		var o map[string]json.RawMessage
		if json.Unmarshal(raw, &o) != nil {
			return nil
		}
		raw = o[name]
	}
	return raw
}

// verifyInputs is VerifyRun's check. It returns the export and the inputs it
// recomputed, which a re-execution evaluates.
func verifyInputs(raw []byte, profiles []InputProfile, trustedReleaseDigest string) (b VerificationBundle, prepared Input, e error) {
	if len(raw) > MaxExportSize {
		return b, prepared, errors.New("verification bundle exceeds 8 MiB")
	}
	if e = strictJSON(raw, &b); e != nil {
		return b, prepared, e
	}
	if e = exportWithinLimits(raw, b); e != nil {
		return b, prepared, e
	}
	// What is verified must be what a reader reads. The decoder takes a member
	// named like a field but for case as that field, so the export must be
	// exactly what the runner encodes.
	if !sameJSON(raw, encode(b)) {
		return b, prepared, errors.New("verification bundle is not in the runner's own encoding")
	}
	if b.Version < 2 || b.Version > 5 || !validDigest(trustedReleaseDigest) || b.ReleaseDigest != trustedReleaseDigest || releaseDigest(b.Release) != trustedReleaseDigest {
		return b, prepared, errors.New("release does not match the independently trusted digest")
	}
	resultDisposition, e := exactDisposition(b.Run.Result)
	if e != nil {
		return b, prepared, e
	}
	auditDisposition, e := exactDisposition(b.Run.Audit)
	if e != nil {
		return b, prepared, e
	}
	if e = checkAuditBytes(b); e != nil {
		return b, prepared, e
	}
	if e = checkChainEntry(b); e != nil {
		return b, prepared, e
	}
	if e = checkAuditSignatures(b); e != nil {
		return b, prepared, e
	}
	// Bind the retained answer before either mapped or unmapped input checks,
	// even if no Runtime is supplied for replay. An unfinished historical run
	// can have neither disposition; one present on either side must match.
	if b.Run.State == "completed" || len(resultDisposition) > 0 || len(auditDisposition) > 0 {
		if string(resultDisposition) == "null" && string(auditDisposition) == "null" {
			return b, prepared, errors.New("a completed run's result and audit disposition must not be null")
		}
		if len(resultDisposition) == 0 || string(resultDisposition) == "null" || !sameJSON(resultDisposition, auditDisposition) {
			return b, prepared, ErrAuditDispositionMismatch
		}
	}
	switch b.Inputs {
	case "":
	case InputsNotMapped:
		prepared, e = verifyNotMapped(b)
		return b, prepared, e
	default:
		return b, prepared, errors.New(`the export's inputs member is not one Runner writes: only "not-mapped" is`)
	}
	r := b.Run
	release := b.Release
	if r.ReleaseID != release.ID || digest([]byte(release.Pack)) != release.PackDigest || r.Input.Preparation == nil || !mappingMatchesRelease(release, r.Input) {
		return b, prepared, errors.New("run and frozen release differ")
	}
	for _, p := range release.InputProfiles {
		found := false
		for _, trusted := range profiles {
			found = found || profileHash(p) == profileHash(trusted)
		}
		if !found {
			return b, prepared, errors.New("release profile is not independently trusted")
		}
	}
	at, e := time.Parse("2006-01-02T15:04:05Z", r.Input.Preparation.VerifiedAt)
	if e != nil {
		return b, prepared, e
	}
	prepared, e = normalizeV2(r.Input, profiles, at)
	if e != nil {
		return b, prepared, e
	}
	if !sameJSON(encode(prepared.Preparation), encode(r.Input.Preparation)) {
		return b, prepared, errors.New("retained lineage does not match recomputation")
	}
	if r.State == "completed" && !auditRecordsInputs(r.Audit, release, prepared, prepared.Preparation.Cites) {
		return b, prepared, errors.New("retained audit inputs or citations do not match verified inputs")
	}
	return b, prepared, nil
}

func exactDisposition(raw json.RawMessage) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil, nil
	}
	for name := range object {
		if name != "disposition" && strings.EqualFold(name, "disposition") {
			return nil, ErrDispositionMemberName
		}
	}
	return object["disposition"], nil
}

// auditRecordsInputs says whether a completed run's audit record is an
// evaluation of the release's pack whose inputs and citations are these.
func auditRecordsInputs(a json.RawMessage, release Release, inputs Input, cites []Citation) bool {
	var kind, packDigest string
	var supplied bool
	if json.Unmarshal(member(a, "kind"), &kind) != nil || json.Unmarshal(member(a, "pack", "digest"), &packDigest) != nil || json.Unmarshal(member(a, "inputs", "evidenceSupplied"), &supplied) != nil || kind != "evaluation" || packDigest != release.PackDigest || !sameJSON(member(a, "inputs", "facts"), inputs.Facts) || supplied != (len(inputs.Evidence) > 0) || len(inputs.Evidence) > 0 && !sameJSON(member(a, "inputs", "evidence"), inputs.Evidence) || !auditCitesMatch(member(a, "cites"), cites) {
		return false
	}
	return true
}

// verifyNotMapped is verifyInputs's check of an export without lineage, once
// its record's bytes, chain entry and signature sidecar have been held to it
// as any export's are. Such an export is of a completed run, from version 3
// on, of a release that froze no mapping or a v1 file mapping, and the run
// names that release and mapping. Nothing derives the run's inputs again:
// they are the facts and evidence the run retains, as given or as its v1
// mapping read them from the file it retains, and its audit record must name
// them as the inputs it evaluated, with no citation. Those inputs are what a
// re-execution evaluates.
func verifyNotMapped(b VerificationBundle) (Input, error) {
	r, release := b.Run, b.Release
	switch {
	case b.Version < 3:
		return Input{}, fmt.Errorf("an export without lineage carries its audit record's bytes, from version 3 on: a version-%d export of it is not one Runner makes", b.Version)
	case r.Input.Preparation != nil:
		return Input{}, errors.New(`an export with lineage is not "not-mapped"`)
	case release.InputMapping != nil && release.InputMapping.Version == 2:
		return Input{}, errors.New("the release froze a mapping v2, and its runs are exported with their lineage")
	case r.ReleaseID != release.ID || digest([]byte(release.Pack)) != release.PackDigest || !mappingMatchesRelease(release, r.Input):
		return Input{}, errors.New("run and frozen release differ")
	}
	inputs := Input{Facts: r.Input.Facts, Evidence: r.Input.Evidence}
	if !auditRecordsInputs(r.Audit, release, inputs, nil) {
		return Input{}, errors.New("retained audit inputs or citations do not match the run's inputs")
	}
	return inputs, nil
}

// Runtime omits cites when there are none. Omission is equivalent only to an
// empty expected set: an omitted externally verified citation still fails.
func auditCitesMatch(raw json.RawMessage, expected []Citation) bool {
	if len(raw) == 0 && len(expected) == 0 {
		return true
	}
	return sameJSON(raw, encode(expected))
}
