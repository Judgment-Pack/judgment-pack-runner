package runner

import (
	"encoding/json"
	"errors"
	"time"
)

type VerificationBundle struct {
	Version       int     `json:"version"`
	ReleaseDigest string  `json:"releaseDigest"`
	Release       Release `json:"release"`
	Run           Run     `json:"run"`
}

func releaseDigest(r Release) string { b, _ := canonical(encode(r)); return digest(b) }

// VerifyRun replays input preparation offline. Profiles and releaseDigest must
// come from a separate trusted channel, never implicitly from the export.
// The result is an input-lineage finding, not verification of policy truth,
// sealed-session completeness, or a re-execution of Runtime.
func VerifyRun(raw []byte, profiles []InputProfile, trustedReleaseDigest string) error {
	if len(raw) > 8<<20 {
		return errors.New("verification bundle exceeds 8 MiB")
	}
	var b VerificationBundle
	if e := strictJSON(raw, &b); e != nil {
		return e
	}
	if b.Version != 2 || !validDigest(trustedReleaseDigest) || b.ReleaseDigest != trustedReleaseDigest || releaseDigest(b.Release) != trustedReleaseDigest {
		return errors.New("release does not match the independently trusted digest")
	}
	r := b.Run
	release := b.Release
	if r.ReleaseID != release.ID || digest([]byte(release.Pack)) != release.PackDigest || r.Input.Preparation == nil || !mappingMatchesRelease(release, r.Input) {
		return errors.New("run and frozen release differ")
	}
	for _, p := range release.InputProfiles {
		found := false
		for _, trusted := range profiles {
			found = found || profileHash(p) == profileHash(trusted)
		}
		if !found {
			return errors.New("release profile is not independently trusted")
		}
	}
	at, e := time.Parse("2006-01-02T15:04:05Z", r.Input.Preparation.VerifiedAt)
	if e != nil {
		return e
	}
	prepared, e := normalizeV2(r.Input, profiles, at)
	if e != nil {
		return e
	}
	if !sameJSON(encode(prepared.Preparation), encode(r.Input.Preparation)) {
		return errors.New("retained lineage does not match recomputation")
	}
	if r.State == "completed" {
		var audit struct {
			Kind   string `json:"kind"`
			Inputs struct {
				Facts    json.RawMessage `json:"facts"`
				Evidence json.RawMessage `json:"evidence"`
				Supplied bool            `json:"evidenceSupplied"`
			} `json:"inputs"`
			Pack struct {
				Digest string `json:"digest"`
			} `json:"pack"`
			Cites json.RawMessage `json:"cites"`
		}
		if json.Unmarshal(r.Audit, &audit) != nil || audit.Kind != "evaluation" || audit.Pack.Digest != release.PackDigest || !sameJSON(audit.Inputs.Facts, prepared.Facts) || audit.Inputs.Supplied != (len(prepared.Evidence) > 0) || len(prepared.Evidence) > 0 && !sameJSON(audit.Inputs.Evidence, prepared.Evidence) || !auditCitesMatch(audit.Cites, prepared.Preparation.Cites) {
			return errors.New("retained audit inputs or citations do not match verified inputs")
		}
	}
	return nil
}

// Runtime omits cites when there are none. Omission is equivalent only to an
// empty expected set: an omitted externally verified citation still fails.
func auditCitesMatch(raw json.RawMessage, expected []Citation) bool {
	if len(raw) == 0 && len(expected) == 0 {
		return true
	}
	return sameJSON(raw, encode(expected))
}
