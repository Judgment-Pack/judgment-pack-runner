package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Supplied reviewed sets (runner #210, runtime ADR-0072).
//
// A release's own lock (Release.Lock) shows only that its pack has not changed
// since the release was made. A release check can also hold the pack to a
// reviewed-set lock made outside Runner, such as the lock of the project the
// pack was reviewed in: POST /v1/previews takes reviewedSet, with the lock's
// exact bytes and the decision id the pack is locked under in it. Runner then
// asks the pinned Runtime, through packs covers (Runtime 0.41.0 and later),
// whether that lock covers the pack's exact bytes under that id, and checks
// the release only when the Runtime says it does. Runner does not read the
// lock itself: the Runtime is the one reader of its format.
//
// Only the lock, the id and the pack's bytes are compared. packs covers is
// given no configuration, because the release's configuration is Runner's own
// and is never the one the lock pins. So the check says that the lock pins
// these bytes under this id. It does not say that a project's configuration
// declares them, or that the lock is the one a given project holds: the caller
// chooses the lock.
//
// The answer is read as packs covers writes it, and nothing else is taken for
// a yes:
//   - covered: exit 0, status valid, an empty list of findings, the id, the
//     pack's digest and the lock's digest Runner gave, and an entry whose
//     digest is the pack's;
//   - not covered: exit 1, status invalid, and findings, such as
//     lock-entry-missing or document-drift, about the id, pack and lock Runner
//     gave: refused as pack_not_in_reviewed_set, naming them;
//   - a lock the Runtime refuses to compare: status error at exit 1 or 4, or
//     unsupported at exit 2, with a diagnostic code: refused as
//     invalid_reviewed_set, naming the code;
//   - any other answer: refused as reviewed_set_unsupported.
//
// A Runtime before 0.41.0 has no packs covers and answers as packs does, with
// another command member, at exit 3. That member, written only by a Runtime
// that has the verb, is what tells Runner it has one. A version number would
// take a second invocation and says nothing of a development build, and exit 3
// alone is also the verb's own answer to an invocation it refuses.
//
// The release keeps the lock's bytes and digest (SuppliedReviewedSet). Every
// run of it whose result or record says reviewed is true is served with
// reviewedSetOrigin supplied and the lock's digest. packs covers is not asked
// again at each attempt. Its answer depends only on the lock's bytes, the id,
// the pack's bytes and the Runtime, and each attempt checks by digest that the
// pack and the lock are the bytes the release froze and that the Runtime is
// the one it pinned (evaluate). The run's Runtime is still given only the
// release's own configuration and lock, which pin the same pack bytes.

// ReviewedSetRequest is the reviewed set a release check holds its pack to:
// the exact bytes of a reviewed-set lock, as the Runtime's packs lock wrote
// it, and the decision id the pack is locked under in it.
type ReviewedSetRequest struct {
	Lock string `json:"lock"`
	ID   string `json:"id"`
}

// SuppliedReviewedSet is the reviewed set a release was checked against: the
// id, the lock's exact bytes, and their digest.
type SuppliedReviewedSet struct {
	ID         string `json:"id"`
	Lock       string `json:"lock"`
	LockDigest string `json:"lockDigest"`
}

const (
	// maxReviewedSetLock is the largest lock a release check takes: the bound
	// every Runtime reader of a lock applies (project.MaxLockBytes).
	maxReviewedSetLock = 1 << 20
	// maxReviewedSetID is the longest id a release check takes.
	maxReviewedSetID = 128
	// maxCoversAnswer is the largest packs covers answer Runner reads.
	maxCoversAnswer = 64 << 10
	// reviewedSetFile is the name the supplied lock is written under in the
	// release's directory, beside pack.json. It is not jpack.lock.json, the
	// release's own lock, which packs lock writes there afterwards.
	reviewedSetFile = "reviewed-set.lock.json"
)

// decisionID is the form of a decision id in a Runtime configuration, and so
// of an id a project's lock names (the Runtime's jpack.schema.json
// decisionId). It begins with a letter, so it is never read as a flag. A
// finding's name has the same form.
var decisionID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// runtimeCode is the form of a Runtime diagnostic code.
var runtimeCode = regexp.MustCompile(`^JPS-[A-Z0-9]+(?:-[A-Z0-9]+)*$`)

var errInvalidReviewedSet = bad("invalid_reviewed_set", "Supply reviewedSet as a reviewed-set lock of 1 byte to 1 MiB and the decision id the pack is locked under in it: lowercase letters and digits in words joined by single hyphens, at most 128 characters.")

var errReviewedSetUnsupported = bad("reviewed_set_unsupported", "The pinned Runtime did not answer as packs covers answers, which a reviewedSet needs; Runtime 0.41.0 and later have it. Pin such a Runtime, or check the release without reviewedSet.")

// errReviewedSetRefused is a supplied lock the pinned Runtime refused to
// compare, with the code it gave.
func errReviewedSetRefused(code string) error {
	return bad("invalid_reviewed_set", "The pinned Runtime refused to compare the supplied reviewed-set lock ("+code+"), and no release was saved. Supply the lock as the Runtime's packs lock wrote it.")
}

// errPackNotInReviewedSet is a supplied lock that does not cover the pack's
// exact bytes under the id given, with the findings the Runtime reported.
func errPackNotInReviewedSet(findings []string) error {
	return bad("pack_not_in_reviewed_set", "The supplied reviewed-set lock does not cover this pack's exact bytes under the id given: the pinned Runtime reports "+strings.Join(findings, ", ")+". No release was saved.")
}

// The refusals of an installation booted with requireSuppliedReviewedSet, of
// a job from a release not checked against a supplied reviewed set.
var (
	errJobReviewedSetRequired     = &apiError{409, "release_reviewed_set_required", "This installation creates jobs only from releases checked against a supplied reviewed-set lock. Check a new release with reviewedSet."}
	errUpgradeReviewedSetRequired = &apiError{409, "release_reviewed_set_required", "This installation upgrades jobs only to releases checked against a supplied reviewed-set lock. Check a new release with reviewedSet."}
)

// check holds a release check's reviewed set, when it has one, to its bounds,
// before anything is written: a lock of 1 byte to maxReviewedSetLock, and an
// id of the decision id form, at most maxReviewedSetID long.
func (set *ReviewedSetRequest) check() error {
	if set == nil {
		return nil
	}
	if len(set.Lock) == 0 || len(set.Lock) > maxReviewedSetLock || len(set.ID) > maxReviewedSetID || !decisionID.MatchString(set.ID) {
		return errInvalidReviewedSet
	}
	return nil
}

// heldToSuppliedReviewedSet says whether a release was checked against a
// supplied reviewed set whose lock is still the bytes its digest names.
func heldToSuppliedReviewedSet(r Release) bool {
	set := r.SuppliedReviewedSet
	return set != nil && decisionID.MatchString(set.ID) && len(set.Lock) > 0 && digest([]byte(set.Lock)) == set.LockDigest
}

// coveredBy writes set's lock into the release's directory, dir, which holds
// the release's pack as pack.json, and asks the pinned Runtime whether the
// lock covers that pack's exact bytes under set's id. It returns what the
// release keeps of set, and only when the Runtime says the lock covers them.
func (s *Service) coveredBy(ctx context.Context, dir string, r Release, set ReviewedSetRequest) (*SuppliedReviewedSet, error) {
	if err := saveFile(filepath.Join(dir, reviewedSetFile), []byte(set.Lock), 0600); err != nil {
		return nil, err
	}
	out, err := s.invoke(ctx, s.runtime, dir, "covers", "packs", "covers", "--lock", reviewedSetFile, "--id", set.ID, "--pack", "pack.json", "--format", "json")
	var ran *invocationError
	if err != nil && !errors.As(err, &ran) {
		return nil, err
	}
	kept := &SuppliedReviewedSet{ID: set.ID, Lock: set.Lock, LockDigest: digest([]byte(set.Lock))}
	if err = coversAnswer(out, exitStatus(err), kept, r.PackDigest); err != nil {
		return nil, err
	}
	return kept, nil
}

// exitStatus is the exit status of a Runtime invocation that ended with err:
// 0 when it succeeded, its exit code when it exited, and -1 when it did not
// exit, because it could not start or was stopped.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return exited.ExitCode()
	}
	return -1
}

// coversAnswer reads out, the packs covers answer of a Runtime that exited
// with exit, about kept's lock and id and the pack whose digest is pack. It
// returns nil only when the answer says that the lock covers the pack's bytes.
func coversAnswer(out []byte, exit int, kept *SuppliedReviewedSet, pack string) error {
	var answer struct {
		OutputVersion string `json:"outputVersion"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		LockDigest    string `json:"lockDigest"`
		ID            string `json:"id"`
		PackDigest    string `json:"packDigest"`
		Entry         *struct {
			Digest string `json:"digest"`
		} `json:"entry"`
		Findings []struct {
			Name string `json:"name"`
		} `json:"findings"`
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	if len(out) > maxCoversAnswer || json.Unmarshal(out, &answer) != nil || answer.OutputVersion != "2" || answer.Command != "packs covers" {
		return errReviewedSetUnsupported
	}
	about := answer.ID == kept.ID && answer.PackDigest == pack && answer.LockDigest == kept.LockDigest
	switch {
	case exit == 0 && answer.Status == "valid":
		if about && answer.Findings != nil && len(answer.Findings) == 0 && answer.Entry != nil && answer.Entry.Digest == pack {
			return nil
		}
	case exit == 1 && answer.Status == "invalid":
		var names []string
		for _, f := range answer.Findings {
			if len(f.Name) > 64 || !decisionID.MatchString(f.Name) {
				return errReviewedSetUnsupported
			}
			if !slices.Contains(names, f.Name) {
				names = append(names, f.Name)
			}
		}
		if about && len(names) > 0 {
			return errPackNotInReviewedSet(names)
		}
	case answer.Status == "error" && (exit == 1 || exit == 4), answer.Status == "unsupported" && exit == 2:
		if len(answer.Diagnostics) > 0 && len(answer.Diagnostics[0].Code) <= 64 && runtimeCode.MatchString(answer.Diagnostics[0].Code) {
			return errReviewedSetRefused(answer.Diagnostics[0].Code)
		}
	}
	return errReviewedSetUnsupported
}
