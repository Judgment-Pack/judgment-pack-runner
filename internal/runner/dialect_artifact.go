package runner

import (
	"bytes"
	"context"
	"database/sql"
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
	if (m.FormatVersion != "2.0.0" || m.Version != 2) && (m.FormatVersion != "3.0.0" || m.Version != 3) {
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
	return json.Marshal(struct {
		FormatVersion    string          `json:"formatVersion"`
		ID               string          `json:"id"`
		Version          string          `json:"version"`
		Case             *CaseMapping    `json:"case"`
		Sources          []MappingSource `json:"sources,omitempty"`
		Admits           *Admission      `json:"admits,omitempty"`
		Unmapped         []string        `json:"unmapped,omitempty"`
		UnmappedEvidence []string        `json:"unmappedEvidence,omitempty"`
	}{m.FormatVersion, m.ArtifactID, m.ArtifactVersion, m.Case, m.Sources, m.Admits, m.Unmapped, m.UnmappedEvidence})
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
	default:
		return errors.New("unsupported Dialect formatVersion")
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
	if err := mapping.validateArtifactIdentity(); err != nil {
		return nil, err
	}
	return &DialectIdentity{mapping.ArtifactID, mapping.ArtifactVersion, mappingV2Hash(mapping)}, nil
}
func admitJobDialect(tx *sql.Tx, r Release) error {
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
	var count int
	err = tx.QueryRow(`SELECT count(*) FROM job_revisions j JOIN releases r ON r.id=j.release_id WHERE json_extract(r.record,'$.dialect.id')=? AND json_extract(r.record,'$.dialect.version')=? AND json_extract(r.record,'$.dialect.digest')<>?`, actual.ID, actual.Version, actual.Digest).Scan(&count)
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
