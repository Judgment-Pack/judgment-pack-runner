package runner

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// ManualExecutionBinding is an immutable description exported under the normal
// owner API. Base64 release bytes are the existing Runner canonical preimage;
// callers cannot supply a different encoding/profile or infer registration.
type ManualExecutionBinding struct {
	Version        int    `json:"version"`
	Workspace      string `json:"workspace"`
	JobID          string `json:"jobId"`
	JobRevision    string `json:"jobRevision"`
	ReleaseID      string `json:"releaseId"`
	ReleaseProfile string `json:"releaseProfile"`
	ReleaseDigest  string `json:"releaseDigest"`
	ReleaseBytes   []byte `json:"releaseBytes"`
}

// manualExecutionBinding reads the job, its release and its triggers in one
// read transaction of the manual-admission state, which only the sqlite kind
// keeps (store-postgresql.md, D4): with another kind, no job is bound.
func (s *Service) manualExecutionBinding(jobID string) (ManualExecutionBinding, error) {
	var out ManualExecutionBinding
	if s.manual == nil {
		return out, manualUnsupported()
	}
	var binding ManualExecutionBinding
	err := s.manual.readManual(func(tx manualReads) error {
		raw, err := tx.manualRecord("jobs", jobID)
		if err != nil {
			return err
		}
		var job Job
		if json.Unmarshal(raw, &job) != nil || job.ID != jobID || job.Workspace != s.cfg.Workspace || job.Revision < 1 {
			return errManualProtocol
		}
		if raw, err = tx.manualRecord("releases", job.ReleaseID); err != nil {
			return err
		}
		var release Release
		if json.Unmarshal(raw, &release) != nil || release.ID != job.ReleaseID || !manualRelease(release) {
			return manualUnsupported()
		}
		triggers, err := tx.manualCount(askJobTriggers, jobID)
		if err != nil {
			return err
		}
		if triggers != 0 || job.InitialTriggerID != "" || job.InitialTriggerDigest != "" {
			return manualUnsupported()
		}
		preimage, err := executionPayloadCanonical(release, maxExecutionRelease)
		if err != nil {
			return err
		}
		// Export only existing verified bytes; never copy, fix or repin a Runtime.
		image, err := readLimit(filepath.Join(s.cfg.Dir, "runtimes", strings.TrimPrefix(release.RuntimeDigest, "sha256:")), 128<<20)
		if err != nil || digest(image) != release.RuntimeDigest {
			return errManualProtocol
		}
		binding = ManualExecutionBinding{1, s.cfg.Workspace, job.ID, strconv.Itoa(job.Revision), release.ID, runnerCanonicalProfile, digest(preimage), preimage}
		return nil
	})
	if err != nil {
		return out, err
	}
	return binding, nil
}
func manualRelease(r Release) bool {
	var pack struct {
		ID          string `json:"id"`
		Version     string `json:"version"`
		SpecVersion string `json:"specVersion"`
	}
	if json.Unmarshal([]byte(r.Pack), &pack) != nil || pack.ID != r.PackID || pack.Version != r.PackVersion || pack.SpecVersion != "0.2.0-draft" {
		return false
	}
	// A release checked against a supplied reviewed set, before the
	// installation admitted manually, still holds the lock it was checked
	// against, as evaluate requires of every other attempt (reviewed_set.go).
	if r.SuppliedReviewedSet != nil && !heldToSuppliedReviewedSet(r) {
		return false
	}
	return r.SchemaVersion == "1" && r.Dialect == nil && len(r.InputProfiles) == 0 && len(r.MappingWarnings) == 0 && r.InputMapping == nil && r.Sample.Source == nil && r.Sample.Preparation == nil && validDigest(r.PackDigest) && digest([]byte(r.Pack)) == r.PackDigest && validDigest(r.RuntimeDigest) && releaseReady(r)
}
func manualUnsupported() error {
	return &apiError{409, "manual_profile_unsupported", "This profile requires an exact manual single-Pack job without triggers, mappings or workflow input."}
}
func (s *Service) manualExecutionBindingHandler(w http.ResponseWriter, r *http.Request) {
	binding, err := s.manualExecutionBinding(r.PathValue("job"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, binding)
}
