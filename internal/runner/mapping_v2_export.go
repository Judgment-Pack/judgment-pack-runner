package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	_, _, e := verifyInputs(raw, profiles, trustedReleaseDigest)
	return e
}

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
	b, prepared, e := verifyInputs(raw, profiles, trustedReleaseDigest)
	if e != nil {
		return e
	}
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
	out, _, e := evaluateWith(ctx, bin, b.Release, Input{Facts: prepared.Facts, Evidence: prepared.Evidence}, work, true)
	if e != nil {
		return e
	}
	if _, e = os.Lstat(filepath.Join(work, "audit")); !errors.Is(e, fs.ErrNotExist) {
		return errors.New("the re-execution left an audit record")
	}
	var fresh, result, audit struct {
		Disposition json.RawMessage `json:"disposition"`
	}
	if json.Unmarshal(out, &fresh) != nil || json.Unmarshal(b.Run.Result, &result) != nil || json.Unmarshal(b.Run.Audit, &audit) != nil {
		return errors.New("a disposition could not be read")
	}
	if !sameJSON(fresh.Disposition, result.Disposition) || !sameJSON(fresh.Disposition, audit.Disposition) {
		return ErrDispositionDiffers
	}
	return nil
}

// verifyInputs is VerifyRun's check. It returns the export and the inputs it
// recomputed, which a re-execution evaluates.
func verifyInputs(raw []byte, profiles []InputProfile, trustedReleaseDigest string) (b VerificationBundle, prepared Input, e error) {
	if len(raw) > 8<<20 {
		return b, prepared, errors.New("verification bundle exceeds 8 MiB")
	}
	if e = strictJSON(raw, &b); e != nil {
		return b, prepared, e
	}
	if b.Version != 2 || !validDigest(trustedReleaseDigest) || b.ReleaseDigest != trustedReleaseDigest || releaseDigest(b.Release) != trustedReleaseDigest {
		return b, prepared, errors.New("release does not match the independently trusted digest")
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
			return b, prepared, errors.New("retained audit inputs or citations do not match verified inputs")
		}
	}
	return b, prepared, nil
}

// Runtime omits cites when there are none. Omission is equivalent only to an
// empty expected set: an omitted externally verified citation still fails.
func auditCitesMatch(raw json.RawMessage, expected []Citation) bool {
	if len(raw) == 0 && len(expected) == 0 {
		return true
	}
	return sameJSON(raw, encode(expected))
}
