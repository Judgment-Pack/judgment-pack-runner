package runner

// A job revision chooses a checked, immutable release. The change, its receipt,
// and journal event commit together. Existing runs keep their original pins.
import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

type UpgradeJobRequest struct {
	ReleaseID     string `json:"releaseId"`
	BaseReleaseID string `json:"baseReleaseId"`
	BaseRevision  int    `json:"baseRevision"`
	Reviewed      bool   `json:"reviewed"`
}

// A preview is disposable. Once used by a job, its pack identity cannot be
// reused with different bytes, even when another job is being created.
func admitJobPack(tx runTx, r Release) error {
	if err := admitJobDialect(tx, r); err != nil {
		return err
	}
	count, err := tx.packConflicts(r.PackID, r.PackVersion, r.PackDigest)
	if err != nil {
		return err
	}
	if count != 0 {
		return &apiError{409, "artifact_identity_conflict", "This pack identity and version already belongs to different job content. Choose a new artifact version and check it again."}
	}
	return nil
}

func (s *Service) upgradeJob(ctx context.Context, jobID string, req UpgradeJobRequest) (Job, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Job{}, manualUnsupported()
	}
	if !req.Reviewed || req.BaseRevision < 1 || req.BaseRevision == math.MaxInt || req.ReleaseID == "" || req.BaseReleaseID == "" {
		return Job{}, bad("review_required", "Review the checked release and the job revision being replaced.")
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	hash := digest(encode(req))
	// Reconcile a completed request before current readiness or automation checks.
	// Returning its immutable receipt does not replay the upgrade, even if the job
	// has since advanced again.
	if prior, found, err := s.upgradeReceipt(jobID, req); err != nil || found {
		return prior, err
	}
	if s.unhealthy.Load() {
		return Job{}, &apiError{503, "dispatcher_stopped", "Check Runner storage before changing a job."}
	}
	j, err := s.job(jobID)
	if err != nil {
		return Job{}, err
	}
	if j.Revision != req.BaseRevision || j.ReleaseID != req.BaseReleaseID {
		return Job{}, &apiError{409, "job_changed", "The job changed after review. Refresh it and review the new release again."}
	}
	if req.ReleaseID == j.ReleaseID {
		return Job{}, bad("release_unchanged", "Choose a different checked release.")
	}
	old, err := s.release(j.ReleaseID)
	if err != nil {
		return Job{}, err
	}
	next, err := s.release(req.ReleaseID)
	if err != nil {
		return Job{}, err
	}
	// A job is never upgraded to a release that names a set-aside profile;
	// upgrading away from one is how its job moves to a new profile.
	if err = s.refuseSetAsideMapping(next.InputMapping); err != nil {
		return Job{}, err
	}
	if old.PackID != next.PackID {
		return Job{}, bad("pack_identity_changed", "A job upgrade must keep the same pack identity. Create another job for a different pack.")
	}
	if !releaseReady(next) {
		return Job{}, &apiError{409, "release_not_ready", "Saved tests failed or could not complete. Check a new release before upgrading this job."}
	}
	if s.cfg.RequireTestedReleases && next.Tests == "not-run" {
		return Job{}, &apiError{409, "release_untested", "This installation requires passing saved tests before upgrading a job."}
	}
	if s.cfg.RequireComparableFacts && !releaseRefusesIncomparableFacts(next) {
		return Job{}, errUpgradeComparableFactsRequired
	}
	if s.cfg.RequireSuppliedReviewedSet && !heldToSuppliedReviewedSet(next) {
		return Job{}, errUpgradeReviewedSetRequired
	}
	if old.PackDigest != next.PackDigest {
		if err = s.checkUpgradeVersion(ctx, old.PackVersion, next.PackVersion); err != nil {
			return Job{}, err
		}
	}
	if err = s.checkDialectUpgrade(ctx, old, next); err != nil {
		return Job{}, err
	}
	by := s.actor(requesterFrom(ctx))
	// Whether the release's mapping can be acquired in the background reads
	// the profiles a registration writes: it is read once, here, and each
	// trigger the transaction reads is checked against that answer, so that
	// the transaction reads only the store (docs/design/store-postgresql.md,
	// D2).
	gateway := s.validateGatewayMapping(next)
	var upgraded Job
	// Held to the scope the request names (docs/design/store-postgresql.md,
	// section 6); a release another scope's job holds is met at its key (D9).
	err = s.store.heldTo(scopeHeld(scopeNamed(ctx))).update(func(tx runTx) error {
		// The revision is re-read in the same transaction that updates it.
		current, err := tx.job(jobID)
		if err != nil {
			return err
		}
		if current.Revision != j.Revision || current.ReleaseID != j.ReleaseID {
			return &apiError{409, "job_changed", "The job changed after review. Review it again."}
		}
		used, err := tx.releaseUses(next.ID)
		if err != nil {
			return err
		}
		if used > 0 {
			return errReleaseRevised
		}
		// Pending source preparation can otherwise finish against an obsolete mapping.
		// Pausing alone does not cancel accepted work; that work must settle first.
		if used, err = tx.openOccurrences(jobID); err != nil {
			return err
		}
		if used > 0 {
			return &apiError{409, "job_work_pending", "Finish, cancel or reconcile pending occurrences before upgrading this job."}
		}
		triggers, err := tx.triggerRecords(jobID)
		if err != nil {
			return err
		}
		for _, t := range triggers {
			if !t.Paused {
				return &apiError{409, "job_triggers_active", "Pause all triggers before upgrading this job. They remain paused after the upgrade."}
			}
			c := t.Config
			if err = s.checkTrigger(&c, next, time.Now(), gateway); err != nil {
				return &apiError{409, "trigger_upgrade_incompatible", "An existing trigger is incompatible with this release. Keep this job unchanged and create a new reviewed job for the new configuration."}
			}
		}
		if err = admitJobPack(tx, next); err != nil {
			return err
		}
		revised := j
		revised.ReleaseID = next.ID
		revised.Revision++
		if err = tx.reviseJob(revised, hash); err != nil {
			return err
		}
		if _, err = tx.appendJournal(change{kind: "job.upgraded", by: by, concerns: EventConcerns{Job: revised.ID, Release: revised.ReleaseID}, revision: revised.Revision, detail: EventDetail{Scope: revised.Scope, PreviousRevision: revisionValue(req.BaseRevision)}}); err != nil {
			return err
		}
		for _, t := range triggers {
			if t.Revision == math.MaxInt {
				return bad("trigger_revision_limit", "The trigger revision limit has been reached.")
			}
			previous := t.Revision
			t.Revision++
			t.UpdatedAt = now()
			if err = tx.reviseTrigger(t); err != nil {
				return err
			}
			if _, err = tx.appendJournal(change{kind: "trigger.configured", by: by, concerns: EventConcerns{Job: revised.ID, Release: revised.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: "paused", to: "paused", detail: EventDetail{Scope: revised.Scope, PreviousRevision: revisionValue(previous), TriggerKind: t.Config.Kind}}); err != nil {
				return err
			}
		}
		upgraded = revised
		return nil
	})
	if err != nil {
		return Job{}, releaseTaken(err)
	}
	return upgraded, nil
}

func (s *Service) checkUpgradeVersion(ctx context.Context, base, candidate string) error {
	dir, err := os.MkdirTemp(s.cfg.Dir, "version-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	out, err := s.invoke(ctx, s.runtime, dir, "version", "artifact", "version", "--base", base, "--candidate", candidate, "--format", "json")
	var r struct{ OutputVersion, Command, Status, Base, Candidate, Comparison string }
	if err != nil || json.Unmarshal(out, &r) != nil || r.OutputVersion != "2" || r.Command != "artifact version" || r.Status != "available" || r.Base != base || r.Candidate != candidate {
		return &apiError{409, "version_check_unavailable", "Runtime could not check the artifact version. Use a Runtime with artifact version tooling and check a new release."}
	}
	if r.Comparison != "greater" {
		return &apiError{409, "artifact_version_required", "Changed artifact content must have a greater reviewed version. Choose its compatibility impact and check a new release."}
	}
	return nil
}

func (s *Service) upgradeJobHandler(w http.ResponseWriter, r *http.Request) {
	var req UpgradeJobRequest
	if err := decode(w, r, &req); err != nil {
		failure(w, err)
		return
	}
	j, err := s.upgradeJob(r.Context(), r.PathValue("job"), req)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, j)
}

// upgradeReceipt never performs readiness checks or changes a job. Hosts use it
// to reconcile a lost acknowledgement without granting a new write.
func (s *Service) upgradeReceipt(jobID string, req UpgradeJobRequest) (Job, bool, error) {
	job, hash, err := s.store.jobRevision(jobID, req.BaseRevision+1)
	if absent(err) {
		return Job{}, false, nil
	}
	// A revision whose record does not decode was read, digest and all, and
	// is compared by its digest first, as it was. A read that failed
	// (faultUnavailable) read no digest, and is the store's failure.
	if err != nil && faultOf(err) != faultUnreadable {
		return Job{}, false, err
	}
	if hash != digest(encode(req)) {
		return Job{}, false, &apiError{409, "job_changed", "This job revision belongs to a different reviewed upgrade."}
	}
	return job, err == nil, err
}
func (s *Service) upgradeReceiptHandler(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	// A scope identifier names the job (scope.go), and is no part of the
	// upgrade the receipt is for.
	members := len(q)
	if q.Has("scope") {
		members--
	}
	if err != nil || members != 3 || len(q["baseRevision"]) != 1 || len(q["baseReleaseId"]) != 1 || len(q["releaseId"]) != 1 {
		failure(w, bad("invalid_request", "Choose the exact job upgrade to reconcile."))
		return
	}
	revision, err := strconv.Atoi(q.Get("baseRevision"))
	if err != nil || revision < 1 || revision == math.MaxInt || q.Get("baseReleaseId") == "" || q.Get("releaseId") == "" {
		failure(w, bad("invalid_request", "Choose the exact job upgrade to reconcile."))
		return
	}
	req := UpgradeJobRequest{ReleaseID: q.Get("releaseId"), BaseReleaseID: q.Get("baseReleaseId"), BaseRevision: revision, Reviewed: true}
	job, found, err := s.upgradeReceipt(r.PathValue("job"), req)
	if err != nil {
		failure(w, err)
		return
	}
	if !found {
		failure(w, &apiError{404, "upgrade_not_completed", "No completed receipt exists for this upgrade."})
		return
	}
	write(w, 200, job)
}
