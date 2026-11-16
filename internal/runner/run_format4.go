package runner

// Formats 4.0.0 and 4.1.0 on the run path (docs/design/dialect-format-4.md,
// section 2). A release, a job and a run are on a dialect of either format
// as on one of format 3.0.0, with what the Spec's staged 0.6.0 records give
// them (spec PR #332): a run's preparation is written in the form of the
// dialect-recording schema of version 2, and read back from the store in the
// same form, and the release digest of a release on either format is RFC
// 0020's, the SHA-256 of the canon bytes of the whole retained release.

import (
	"encoding/json"
	"errors"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/dialect4"
)

// fourRelease says whether a release is on a dialect of format 4.0.0 or
// 4.1.0, by its retained inputMapping's formatVersion, as RFC 0020's
// amendment of 2026-11-15 tells the two encodings apart.
func fourRelease(r Release) bool {
	return r.InputMapping != nil && fourFamily(r.InputMapping.FormatVersion)
}

// fourReleaseForm holds a release on a dialect of format 4.0.0 or 4.1.0 to
// RFC 0020 section 1's open form (spec 0d5bdea): id a string, packDigest and
// runtimeDigest digests, inputMapping an object, every other member admitted
// as retained. It reads the release's own encoding, the bytes the digest is
// taken of, so a member that is missing there or of another form is refused
// input-malformed: such a release has no release digest.
func fourReleaseForm(r Release) error {
	members, err := exactMembers(encode(r))
	if err != nil {
		return codedProblem(codeMalformed, "A release on a dialect of format "+r.InputMapping.FormatVersion+" must be an object.")
	}
	var id string
	if raw, ok := members["id"]; !ok || json.Unmarshal(raw, &id) != nil {
		return codedProblem(codeMalformed, "A release on a dialect of format "+r.InputMapping.FormatVersion+" needs a string id.")
	}
	for _, name := range []string{"packDigest", "runtimeDigest"} {
		var d string
		if raw, ok := members[name]; !ok || json.Unmarshal(raw, &d) != nil || !validDigest(d) {
			return codedProblem(codeMalformed, "A release on a dialect of format "+r.InputMapping.FormatVersion+" needs a digest "+name+".")
		}
	}
	if raw, ok := members["inputMapping"]; !ok || len(raw) == 0 || raw[0] != '{' {
		return codedProblem(codeMalformed, "A release on a dialect of format "+r.InputMapping.FormatVersion+" needs an inputMapping object.")
	}
	return nil
}

// releaseDigestOf is a release's digest (RFC 0020 section 1). A release on
// a dialect of format 4.0.0 or 4.1.0 is digested over the canon bytes of the
// whole release as Runner retains it, every member as retained, the case's
// rule and a source's paging among them; one outside the canon domain, or
// not of the open release form (fourReleaseForm), has none, an error here,
// and is refused when it is saved (fourReleaseDigestible). Every other
// release keeps the pinned encoding, byte for byte.
func releaseDigestOf(r Release) (string, error) {
	if fourRelease(r) {
		if err := fourReleaseForm(r); err != nil {
			return "", err
		}
		b, err := derivation.Canon(encode(r))
		if err != nil {
			return "", codedProblem(codeMalformed, "A release on a dialect of format "+r.InputMapping.FormatVersion+" is outside the canon domain and has no release digest: "+err.Error())
		}
		return digest(b), nil
	}
	b, _ := canonical(encode(r))
	return digest(b), nil
}

// releaseDigestIs says whether r's digest is want: never for a release that
// has none, whatever want is.
func releaseDigestIs(r Release, want string) bool {
	d, err := releaseDigestOf(r)
	return err == nil && d == want
}

// fourReleaseDigestible refuses a release on a dialect of format 4.0.0 or
// 4.1.0 that is outside the canon domain, or not of the open release form,
// input-malformed: it has no release digest (RFC 0020 section 1, as staged
// for 0.6.0).
func fourReleaseDigestible(r Release) error {
	if !fourRelease(r) {
		return nil
	}
	if err := fourReleaseForm(r); err != nil {
		return err
	}
	if _, err := derivation.Canon(encode(r)); err != nil {
		return codedProblem(codeMalformed, "A release on a dialect of format "+r.InputMapping.FormatVersion+" is digested over the canon bytes of the whole release, and this one is outside the canon domain: "+err.Error())
	}
	return nil
}

// fourPreparationMembers reads a preparation as MarshalJSON writes it
// (Preparation.UnmarshalJSON): one of format 4.0.0 or 4.1.0 has
// formatVersion in place of version, the form of the dialect-recording
// schema of version 2, and its other members are returned to be read as
// every other preparation's are; format is "" for every other one, whose
// bytes are returned as they are. A run's preparation is read back from the
// store so, and recomputed before anything relies on it (evaluate).
func fourPreparationMembers(raw []byte) ([]byte, string, error) {
	members, err := exactMembers(raw)
	if err != nil {
		return nil, "", err
	}
	held, named := members["formatVersion"]
	if !named {
		return raw, "", nil
	}
	var format string
	if json.Unmarshal(held, &format) != nil || !fourFamily(format) {
		return nil, "", errors.New("a preparation's formatVersion is 4.0.0 or 4.1.0")
	}
	if _, ok := members["version"]; ok {
		return nil, "", errors.New("a preparation of format " + format + " has formatVersion in place of version")
	}
	delete(members, "formatVersion")
	rest, err := json.Marshal(members)
	return rest, format, err
}

// UnmarshalJSON reads a source's outcome with the members MarshalJSON adds
// in format 4.0.0 and 4.1.0: the case's rule and a paged source's paging,
// each kept as written.
func (o *SourceOutcome) UnmarshalJSON(raw []byte) error {
	type plain SourceOutcome
	members, err := exactMembers(raw)
	if err != nil {
		return err
	}
	rule, paging := members["rule"], members["paging"]
	delete(members, "rule")
	delete(members, "paging")
	rest, err := json.Marshal(members)
	if err != nil {
		return err
	}
	var q plain
	if err = strictJSON(rest, &q); err != nil {
		return err
	}
	*o = SourceOutcome(q)
	o.rule, o.paging = rule, paging
	return nil
}

// UnmarshalJSON reads a target's lineage with its derivation, the member
// MarshalJSON adds for a rule of version 2.0.0 in format 4.0.0 and 4.1.0.
func (l *TargetLineage) UnmarshalJSON(raw []byte) error {
	type plain TargetLineage
	members, err := exactMembers(raw)
	if err != nil {
		return err
	}
	held, named := members["derivation"]
	delete(members, "derivation")
	rest, err := json.Marshal(members)
	if err != nil {
		return err
	}
	var q plain
	if err = strictJSON(rest, &q); err != nil {
		return err
	}
	*l = TargetLineage(q)
	if named {
		var d struct {
			By         string   `json:"by"`
			From       string   `json:"from,omitempty"`
			Builtin    string   `json:"builtin,omitempty"`
			Derived    string   `json:"derived,omitempty"`
			Fields     []string `json:"fields,omitempty"`
			Parameters []string `json:"parameters,omitempty"`
		}
		if err = strictJSON(held, &d); err != nil {
			return err
		}
		l.derivation = &dialect4.Derivation{By: d.By, From: d.From, Builtin: d.Builtin, Derived: d.Derived, Fields: d.Fields, Parameters: d.Parameters}
	}
	return nil
}
