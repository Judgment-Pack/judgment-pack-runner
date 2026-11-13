package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// Named formats retain the original format family internally; this never
// coerces an artifact version into a preparation version.
var artifactVersionGrammar = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func (m InputMapping) validateArtifactIdentity() error {
	if m.FormatVersion == "" {
		if m.ArtifactID != "" || m.ArtifactVersion != "" {
			return inputProblem("Artifact identity requires a named format.")
		}
		return nil
	}
	if (m.FormatVersion != "2.0.0" || m.Version != 2) && (m.FormatVersion != "3.0.0" || m.Version != 3) && (m.FormatVersion != format4 || m.Version != 3) {
		return inputProblem("Unsupported Dialect formatVersion.")
	}
	if m.ArtifactID == "" || len(m.ArtifactID) > 1024 || strings.IndexFunc(m.ArtifactID, func(c rune) bool { return c <= 32 || c == 127 }) >= 0 || !artifactVersionGrammar.MatchString(m.ArtifactVersion) {
		return inputProblem("Invalid Dialect artifact identity or version.")
	}
	if m.Case == nil {
		return inputProblem("A named Dialect requires case.")
	}
	return nil
}
func (m InputMapping) marshalArtifact() ([]byte, error) {
	if err := m.validateArtifactIdentity(); err != nil {
		return nil, err
	}
	// Format 4.0.0's case has a rule (spec/dialect.md §2.2.1), kept as
	// written; no other format's case has one.
	var caseDocument any = m.Case
	if m.FormatVersion == format4 && m.caseRule != nil {
		caseDocument = struct {
			*CaseMapping
			Rule json.RawMessage `json:"rule"`
		}{m.Case, m.caseRule}
	}
	return json.Marshal(struct {
		Profile          string          `json:"profile,omitempty"`
		FormatVersion    string          `json:"formatVersion"`
		ID               string          `json:"id"`
		Version          string          `json:"version"`
		Case             any             `json:"case"`
		Sources          []MappingSource `json:"sources,omitempty"`
		Admits           *Admission      `json:"admits,omitempty"`
		Unmapped         []string        `json:"unmapped,omitempty"`
		UnmappedEvidence []string        `json:"unmappedEvidence,omitempty"`
	}{m.Profile, m.FormatVersion, m.ArtifactID, m.ArtifactVersion, caseDocument, m.Sources, m.Admits, m.Unmapped, m.UnmappedEvidence})
}
func (m *InputMapping) UnmarshalJSON(raw []byte) error {
	// DisallowUnknownFields still applies inside this custom decoder.
	type plain InputMapping
	// Preserve the legacy carrier boundary: explicit null is rejected by the
	// source/import agreement check, with its existing typed diagnostic.
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*m = InputMapping{}
		return nil
	}
	members, err := exactMembers(raw)
	if err != nil {
		return err
	}
	for key := range members {
		if strings.EqualFold(key, "profile") && key != "profile" {
			return errors.New("retained-column profile member must be case-sensitive")
		}
	}
	if code := columnProfileBytes(raw); code != "" && code != "profile-unsupported" {
		return withCode(code, errors.New("unknown or malformed retained-column profile"))
	}
	if _, named := members["formatVersion"]; !named {
		var legacy plain
		if err := strictJSON(raw, &legacy); err != nil {
			return err
		}
		*m = InputMapping(legacy)
		return nil
	}
	var format, id, version string
	if memberString(members["formatVersion"], &format) != nil || memberString(members["id"], &id) != nil || memberString(members["version"], &version) != nil {
		return errors.New("named Dialect requires formatVersion, id and string version")
	}
	family := 0
	switch format {
	case "2.0.0":
		family = 2
	case "3.0.0":
		family = 3
	case format4:
		// Read as format 3.0.0 is, its case's rule apart: no other format's
		// case has one, so the strict reading of the rest refuses it there.
		family = 3
	default:
		return errors.New("unsupported Dialect formatVersion")
	}
	var caseRule json.RawMessage
	if format == format4 {
		if held, ok := members["case"]; ok {
			if caseMembers, err := exactMembers(held); err == nil {
				if rule, ok := caseMembers["rule"]; ok {
					caseRule = append(json.RawMessage(nil), rule...)
					delete(caseMembers, "rule")
					if members["case"], err = json.Marshal(caseMembers); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, key := range []string{"provider", "facts", "evidence"} {
		if _, ok := members[key]; ok {
			return errors.New("named Dialect cannot carry legacy root fields")
		}
	}
	delete(members, "formatVersion")
	delete(members, "id")
	members["version"] = encode(family)
	projected, err := json.Marshal(members)
	if err != nil {
		return err
	}
	var decoded plain
	if err = strictJSON(projected, &decoded); err != nil {
		return err
	}
	candidate := InputMapping(decoded)
	candidate.FormatVersion, candidate.ArtifactID, candidate.ArtifactVersion = format, id, version
	candidate.caseRule = caseRule
	if err = candidate.validateArtifactIdentity(); err != nil {
		return err
	}
	// Includes exact member spelling and every optional-empty identity rule.
	if err = asWritten(raw, candidate); err != nil {
		return err
	}
	*m = candidate
	return nil
}

type DialectIdentity struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

func releaseDialectIdentity(r Release) (*DialectIdentity, error) {
	if r.InputMapping == nil {
		return nil, nil
	}
	mapping := *r.InputMapping
	if r.Sample.Source != nil && r.Sample.Source.Import != nil {
		if err := strictJSON(r.Sample.Source.Import.Dialect, &mapping); err != nil {
			return nil, err
		}
	}
	if mapping.FormatVersion == "" {
		return nil, nil
	}
	// No release, job or workflow is on a dialect of format 4.0.0 until the
	// Spec's workflow and recording schemas admit it (dialect_format4.go).
	if mapping.FormatVersion == format4 {
		return nil, formatNotAdmitted()
	}
	if err := mapping.validateArtifactIdentity(); err != nil {
		return nil, err
	}
	return &DialectIdentity{mapping.ArtifactID, mapping.ArtifactVersion, mappingV2Hash(mapping)}, nil
}
func admitJobDialect(tx runTx, r Release) error {
	actual, err := releaseDialectIdentity(r)
	if err != nil {
		return err
	}
	if actual == nil {
		if r.Dialect != nil {
			return inputProblem("Dialect release identity has no document.")
		}
		return nil
	}
	if r.Dialect == nil || *actual != *r.Dialect {
		return inputProblem("Dialect release identity does not match its retained document.")
	}
	count, err := tx.dialectConflicts(actual.ID, actual.Version, actual.Digest)
	if err != nil {
		return err
	}
	if count != 0 {
		return &apiError{409, "artifact_identity_conflict", "This Dialect identity and version already belongs to different job content. Publish a new version and check it again."}
	}
	return nil
}
func (s *Service) checkDialectUpgrade(ctx context.Context, old, next Release) error {
	if old.Dialect == nil {
		return nil
	}
	if next.Dialect == nil || old.Dialect.ID != next.Dialect.ID {
		return bad("dialect_identity_changed", "A job upgrade must retain its Dialect identity. Create another job for a different Dialect.")
	}
	if old.Dialect.Digest != next.Dialect.Digest {
		return s.checkUpgradeVersion(ctx, old.Dialect.Version, next.Dialect.Version)
	}
	return nil
}
