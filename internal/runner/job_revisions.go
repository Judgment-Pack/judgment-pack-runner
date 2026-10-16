package runner

// A job revision chooses a checked, immutable release. The change, its receipt,
// and journal event commit together. Existing runs keep their original pins.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"time"
)

const jobRevisionSchema = `CREATE TABLE job_creations (release_id TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE job_revisions (job_id TEXT NOT NULL, revision INTEGER NOT NULL, release_id TEXT UNIQUE NOT NULL, record TEXT NOT NULL, request_digest TEXT NOT NULL DEFAULT '', PRIMARY KEY(job_id,revision));
 INSERT INTO job_creations SELECT release_id,record FROM jobs;
 INSERT INTO job_revisions(job_id,revision,release_id,record) SELECT id,json_extract(record,'$.revision'),release_id,record FROM jobs;`

type UpgradeJobRequest struct {
	ReleaseID     string `json:"releaseId"`
	BaseReleaseID string `json:"baseReleaseId"`
	BaseRevision  int    `json:"baseRevision"`
	Reviewed      bool   `json:"reviewed"`
}

// A preview is disposable. Once used by a job, its pack identity cannot be
// reused with different bytes, even when another job is being created.
func admitJobPack(tx *sql.Tx, r Release) error {
	if err := admitJobDialect(tx, r); err != nil {
		return err
	}
	var count int
	err := tx.QueryRow(`SELECT count(*) FROM job_revisions j JOIN releases r ON r.id=j.release_id WHERE json_extract(r.record,'$.packId')=? AND json_extract(r.record,'$.packVersion')=? AND json_extract(r.record,'$.packDigest')<>?`, r.PackID, r.PackVersion, r.PackDigest).Scan(&count)
	if err != nil {
		return err
	}
	if count != 0 {
		return &apiError{409, "artifact_identity_conflict", "This pack identity and version already belongs to different job content. Choose a new artifact version and check it again."}
	}
	return nil
}

func recordInitialJob(tx *sql.Tx, j Job) error {
	if _, err := tx.Exec("INSERT INTO job_creations(release_id,record) VALUES(?,?)", j.ReleaseID, string(encode(j))); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO job_revisions(job_id,revision,release_id,record) VALUES(?,?,?,?)", j.ID, j.Revision, j.ReleaseID, string(encode(j)))
	return err
}

func (s *Service) upgradeJob(ctx context.Context, jobID string, req UpgradeJobRequest) (Job, error) {
	if !req.Reviewed || req.BaseRevision < 1 || req.BaseRevision == math.MaxInt || req.ReleaseID == "" || req.BaseReleaseID == "" {
		return Job{}, bad("review_required", "Review the checked release and the job revision being replaced.")
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	hash := digest(encode(req))
	// Reconcile a completed request before current readiness or automation checks.
	// Returning its immutable receipt does not replay the upgrade, even if the job
	// has since advanced again.
	var prior, priorHash string
	err := s.db.QueryRow("SELECT record,request_digest FROM job_revisions WHERE job_id=? AND revision=?", jobID, req.BaseRevision+1).Scan(&prior, &priorHash)
	if err == nil && priorHash == hash {
		var j Job
		err = json.Unmarshal([]byte(prior), &j)
		return j, err
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Job{}, err
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
	if old.PackID != next.PackID {
		return Job{}, bad("pack_identity_changed", "A job upgrade must keep the same pack identity. Create another job for a different pack.")
	}
	if !releaseReady(next) {
		return Job{}, &apiError{409, "release_not_ready", "Saved tests failed or could not complete. Check a new release before upgrading this job."}
	}
	if s.cfg.RequireTestedReleases && next.Tests == "not-run" {
		return Job{}, &apiError{409, "release_untested", "This installation requires passing saved tests before upgrading a job."}
	}
	if old.PackDigest != next.PackDigest {
		if err = s.checkUpgradeVersion(ctx, old.PackVersion, next.PackVersion); err != nil {
			return Job{}, err
		}
	}
	if err = s.checkDialectUpgrade(ctx, old, next); err != nil {
		return Job{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	// The revision is re-read in the same transaction that updates it.
	var raw string
	if err = tx.QueryRow("SELECT record FROM jobs WHERE id=?", jobID).Scan(&raw); err != nil {
		return Job{}, err
	}
	var current Job
	if err = json.Unmarshal([]byte(raw), &current); err != nil {
		return Job{}, err
	}
	if current.Revision != j.Revision || current.ReleaseID != j.ReleaseID {
		return Job{}, &apiError{409, "job_changed", "The job changed after review. Review it again."}
	}
	var used int
	if err = tx.QueryRow("SELECT count(*) FROM job_revisions WHERE release_id=?", next.ID).Scan(&used); err != nil {
		return Job{}, err
	}
	if used > 0 {
		return Job{}, &apiError{409, "release_already_used", "This preview already belongs to a job revision. Check a fresh release."}
	}
	// Pending source preparation can otherwise finish against an obsolete mapping.
	// Pausing alone does not cancel accepted work; that work must settle first.
	if err = tx.QueryRow("SELECT count(*) FROM occurrences WHERE job_id=? AND state IN ('accepted','preparing','waiting','needs-attention')", jobID).Scan(&used); err != nil {
		return Job{}, err
	}
	if used > 0 {
		return Job{}, &apiError{409, "job_work_pending", "Finish, cancel or reconcile pending occurrences before upgrading this job."}
	}
	rows, err := tx.Query("SELECT record FROM triggers WHERE job_id=?", jobID)
	if err != nil {
		return Job{}, err
	}
	var triggers []Trigger
	for rows.Next() {
		var b []byte
		var t Trigger
		if err = rows.Scan(&b); err != nil {
			break
		}
		if err = json.Unmarshal(b, &t); err != nil {
			break
		}
		triggers = append(triggers, t)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return Job{}, err
	}
	for _, t := range triggers {
		if !t.Paused {
			return Job{}, &apiError{409, "job_triggers_active", "Pause all triggers before upgrading this job. They remain paused after the upgrade."}
		}
		c := t.Config
		if err = s.validateTrigger(&c, next, time.Now()); err != nil {
			return Job{}, &apiError{409, "trigger_upgrade_incompatible", "An existing trigger is incompatible with this release. Keep this job unchanged and create a new reviewed job for the new configuration."}
		}
	}
	if err = admitJobPack(tx, next); err != nil {
		return Job{}, err
	}
	j.ReleaseID = next.ID
	j.Revision++
	if _, err = tx.Exec("UPDATE jobs SET release_id=?,record=? WHERE id=?", j.ReleaseID, string(encode(j)), j.ID); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec("INSERT INTO job_revisions(job_id,revision,release_id,record,request_digest) VALUES(?,?,?,?,?)", j.ID, j.Revision, j.ReleaseID, string(encode(j)), hash); err != nil {
		return Job{}, err
	}
	if _, err = appendEvent(tx, change{kind: "job.upgraded", by: s.installation(), concerns: EventConcerns{Job: j.ID, Release: j.ReleaseID}, revision: j.Revision, detail: EventDetail{PreviousRevision: revisionValue(req.BaseRevision)}}); err != nil {
		return Job{}, err
	}
	for _, t := range triggers {
		if t.Revision == math.MaxInt {
			return Job{}, bad("trigger_revision_limit", "The trigger revision limit has been reached.")
		}
		previous := t.Revision
		t.Revision++
		t.UpdatedAt = now()
		if _, err = tx.Exec("UPDATE triggers SET record=? WHERE id=?", string(encode(t)), t.ID); err != nil {
			return Job{}, err
		}
		if _, err = tx.Exec("INSERT INTO trigger_revisions(trigger_id,revision,record) VALUES(?,?,?)", t.ID, t.Revision, string(encode(t))); err != nil {
			return Job{}, err
		}
		if _, err = appendEvent(tx, change{kind: "trigger.configured", by: s.installation(), concerns: EventConcerns{Job: j.ID, Release: j.ReleaseID, Trigger: t.ID}, revision: t.Revision, states: true, from: "paused", to: "paused", detail: EventDetail{PreviousRevision: revisionValue(previous), TriggerKind: t.Config.Kind}}); err != nil {
			return Job{}, err
		}
	}
	return j, tx.Commit()
}

func (s *Service) checkUpgradeVersion(ctx context.Context, base, candidate string) error {
	dir, err := os.MkdirTemp(s.cfg.Dir, "version-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	out, err := invoke(ctx, s.runtime, dir, "version", "artifact", "version", "--base", base, "--candidate", candidate, "--format", "json")
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
