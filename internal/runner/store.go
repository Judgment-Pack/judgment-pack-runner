package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Dir, Runtime, Workspace, Owner string
	InputProfiles                  []InputProfile
	InputRoot                      string
	CloudConnections               []CloudConnection
	GatewayConnections             []GatewayConnection
	// RequireTestedReleases refuses to create a job from a release whose saved
	// tests never ran. Off by default, when an explicitly reviewed untested
	// release can become a job.
	RequireTestedReleases bool
	// SigningKey is the absolute path of an Ed25519 seed, held outside the
	// state directory, that each operational evaluation's Runtime signs its
	// audit record with (runtime ADR-0047 §2b). Runner passes the path, never
	// the key, as JPACK_SIGNING_KEY. Empty, nothing is signed.
	SigningKey        string
	disableAutomation bool
	// disableDispatcher leaves queued runs for a test to dispatch, one at a
	// time, with dispatchNext.
	disableDispatcher bool
}
type Service struct {
	unhealthy              atomic.Bool
	background             sync.WaitGroup
	cloudMu                sync.Mutex
	cloudProblems          map[string]string
	cloudFactory           func(context.Context, CloudConnection) (cloudTransport, error)
	preparing              map[string]bool
	automationMu           sync.Mutex
	automationDone         chan struct{}
	cfg                    Config
	db                     *sql.DB
	lock                   *os.File
	runtime, runtimeDigest string
	cancel                 context.CancelFunc
	done                   chan struct{}
	wake                   chan struct{}
	previewGate            chan struct{}
	closeOnce              sync.Once
}

func Open(cfg Config) (_ *Service, err error) {
	if err = validateProfiles(cfg.InputProfiles); err != nil {
		return nil, err
	}
	if err = validateBackgroundConfig(cfg); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(encode(cfg.CloudConnections), &cfg.CloudConnections); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(encode(cfg.GatewayConnections), &cfg.GatewayConnections); err != nil {
		return nil, err
	}
	// Snapshot trusted host settings; callers cannot mutate a running service.
	if err = json.Unmarshal(encode(cfg.InputProfiles), &cfg.InputProfiles); err != nil {
		return nil, err
	}
	if cfg.InputRoot != "" && !filepath.IsAbs(cfg.InputRoot) {
		return nil, errors.New("absolute input root required")
	}
	if cfg.Workspace == "" || cfg.Owner == "" || !filepath.IsAbs(cfg.Dir) || !filepath.IsAbs(cfg.Runtime) {
		return nil, errors.New("absolute paths and workspace owner required")
	}
	if err = privateDir(cfg.Dir); err != nil {
		return nil, err
	}
	if cfg.SigningKey != "" {
		if err = checkSigningKey(cfg.SigningKey, cfg.Dir); err != nil {
			return nil, fmt.Errorf("the signing key is refused: %w", err)
		}
	}
	s := &Service{cloudProblems: map[string]string{}, automationDone: make(chan struct{}), cfg: cfg, wake: make(chan struct{}, 1), done: make(chan struct{}), previewGate: make(chan struct{}, 1)}
	defer func() {
		if err != nil {
			if s.db != nil {
				s.db.Close()
			}
			if s.lock != nil {
				s.lock.Close()
			}
		}
	}()
	s.lock, err = instanceLock(filepath.Join(cfg.Dir, "runner.lock"))
	if err != nil {
		return nil, err
	}
	for _, n := range []string{"releases", "attempts", "runtimes"} {
		if err = privateDir(filepath.Join(cfg.Dir, n)); err != nil {
			return nil, err
		}
	}
	dbpath := filepath.Join(cfg.Dir, "runs.sqlite")
	f, e := os.OpenFile(dbpath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e == nil {
		f.Close()
	} else if !os.IsExist(e) {
		return nil, e
	}
	if err = privateFile(dbpath); err != nil {
		return nil, err
	}
	s.db, err = sql.Open("sqlite", dbpath)
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	_, err = s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS briefs (subject TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS releases (id TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS jobs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, release_id TEXT UNIQUE NOT NULL, record TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS runs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, job_id TEXT NOT NULL, caller TEXT NOT NULL, idem TEXT NOT NULL, request_digest TEXT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL, UNIQUE(job_id,caller,idem));
 CREATE INDEX IF NOT EXISTS runs_state ON runs(state,seq);`)
	if err != nil {
		return nil, err
	}
	if _, err = s.db.Exec(triggerSchema); err != nil {
		return nil, err
	}
	if _, err = s.db.Exec(runChainSchema); err != nil {
		return nil, err
	}
	if _, err = s.db.Exec(eventsSchema); err != nil {
		return nil, err
	}
	for _, k := range []struct{ key, value string }{{"workspace", cfg.Workspace}, {"owner", cfg.Owner}, {"inputRoot", cfg.InputRoot}} {
		if _, err = s.db.Exec("INSERT OR IGNORE INTO metadata VALUES (?,?)", k.key, k.value); err != nil {
			return nil, err
		}
		var held string
		if err = s.db.QueryRow("SELECT value FROM metadata WHERE key=?", k.key).Scan(&held); err != nil {
			return nil, err
		}
		if held != k.value {
			return nil, errors.New("runner store belongs to another owner, workspace or schema")
		}
	}
	// A store an earlier Runner kept, at schema "1", or a new one, begins its
	// journal here and is raised to schema "2", which every earlier Runner
	// refuses (docs/design/activity-journal.md, section 4).
	if err = s.migrateStore(); err != nil {
		return nil, err
	}
	s.runtime, s.runtimeDigest, err = s.pinRuntime(cfg.Runtime)
	if err != nil {
		return nil, err
	}
	// Holding the process lock proves the old dispatcher is gone. Never replay an
	// invocation that may have appended an operational audit before crashing.
	if err = s.restart(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if cfg.disableDispatcher {
		close(s.done)
	} else {
		go s.worker(ctx)
	}
	if cfg.disableAutomation {
		close(s.automationDone)
	} else {
		go s.automation(ctx)
		s.startBackground(ctx)
	}
	return s, nil
}
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		<-s.automationDone
		s.background.Wait()
		s.recordStop()
		s.db.Close()
		s.lock.Close()
	})
}
func (s *Service) release(id string) (Release, error) {
	var r Release
	var b []byte
	e := s.db.QueryRow("SELECT record FROM releases WHERE id=?", id).Scan(&b)
	if e == nil {
		e = json.Unmarshal(b, &r)
	}
	return r, e
}
func (s *Service) job(id string) (Job, error) {
	var j Job
	var b []byte
	e := s.db.QueryRow("SELECT record FROM jobs WHERE id=?", id).Scan(&b)
	if e == nil {
		e = json.Unmarshal(b, &j)
	}
	return j, e
}
func (s *Service) run(id string) (Run, error) {
	var r Run
	var b []byte
	e := s.db.QueryRow("SELECT record FROM runs WHERE id=?", id).Scan(&b)
	if e == nil {
		r, e = decodeRun(b)
	}
	return r, e
}
func (s *Service) createJob(name, releaseID string) (Job, error) {
	return s.createJobConfigured(name, releaseID, nil)
}
func (s *Service) createJobConfigured(name, releaseID string, c *TriggerConfig) (Job, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	r, err := s.release(releaseID)
	if err != nil {
		return Job{}, err
	}
	if !releaseReady(r) {
		return Job{}, &apiError{409, "release_not_ready", "Saved tests failed or could not complete. Correct the pack or cases and check a new release before creating a job."}
	}
	initialDigest := ""
	if c != nil {
		initialDigest = digest(encode(c))
		if err = s.validateTrigger(c, r, time.Now()); err != nil {
			return Job{}, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	var held []byte
	err = tx.QueryRow("SELECT record FROM jobs WHERE release_id=?", releaseID).Scan(&held)
	if err == nil {
		var stored Job
		if err = json.Unmarshal(held, &stored); err != nil {
			return stored, err
		}
		if stored.Name != name || stored.InitialTriggerDigest != initialDigest {
			return Job{}, &apiError{409, "release_already_used", "This preview already created a different job. Preview again to create another job."}
		}
		return stored, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, err
	}
	// Only a new job is refused: a job made before the setting was on is returned
	// as any repeated creation is, and keeps running.
	if s.cfg.RequireTestedReleases && r.Tests == "not-run" {
		return Job{}, &apiError{409, "release_untested", "This installation creates jobs only from releases whose saved tests ran and passed. Save tests for this pack, then check a new release."}
	}
	j := Job{SchemaVersion: "1", ID: id("job_"), Name: name, ReleaseID: r.ID, Revision: 1, CreatedAt: now(), Workspace: s.cfg.Workspace, Owner: s.cfg.Owner, InitialTriggerDigest: initialDigest}
	if c != nil {
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM triggers").Scan(&count); err != nil {
			return Job{}, err
		}
		if count >= 128 {
			return Job{}, bad("trigger_limit", "This local runner supports up to 128 triggers.")
		}
		t := Trigger{ID: id("trg_"), JobID: j.ID, Revision: 1, Authority: "local", Config: *c, Paused: true, CreatedAt: now(), UpdatedAt: now()}
		if c.Kind == "cloud" {
			t.Authority = "google-cloud"
		}
		j.InitialTriggerID = t.ID
		if _, err = tx.Exec("INSERT INTO triggers(id,job_id,record)VALUES(?,?,?)", t.ID, j.ID, string(encode(t))); err != nil {
			return Job{}, err
		}
		if err = saveTrigger(tx, t, "", true); err != nil {
			return Job{}, err
		}
	}
	if _, err = tx.Exec("INSERT INTO jobs(id,release_id,record)VALUES(?,?,?)", j.ID, j.ReleaseID, string(encode(j))); err != nil {
		return Job{}, err
	}
	if _, err = appendEvent(tx, change{kind: "job.created", by: s.installation(), concerns: EventConcerns{Job: j.ID, Release: j.ReleaseID}}); err != nil {
		return Job{}, err
	}
	if c != nil {
		if _, err = appendEvent(tx, change{kind: "trigger.configured", by: s.installation(), concerns: EventConcerns{Job: j.ID, Release: j.ReleaseID, Trigger: j.InitialTriggerID}, revision: 1, states: true, to: "paused", detail: EventDetail{PreviousRevision: revisionValue(0), TriggerKind: c.Kind}}); err != nil {
			return Job{}, err
		}
	}
	return j, tx.Commit()
}
func (s *Service) submit(jobID, key string, input Input) (Run, bool, error) {
	return s.submitInternal(jobID, key, input, nil)
}
func (s *Service) submitInternal(jobID, key string, input Input, origin *TriggerOrigin) (Run, bool, error) {
	caller := s.cfg.Owner
	if origin != nil {
		caller = "trigger:" + origin.TriggerID
	}
	if s.unhealthy.Load() {
		return Run{}, false, &apiError{503, "dispatcher_stopped", "The dispatcher stopped after a storage error. Restart Desk after checking the runner storage."}
	}
	v2 := input.Source != nil && input.Source.Mapping.Version == 2
	var requestHash string
	if v2 {
		input.Preparation = nil
		raw, e := canonical(encode(input))
		if e != nil {
			return Run{}, false, e
		}
		requestHash = digest(raw)
		// Identical retries return their original acceptance, even after freshness expires.
		var held, before string
		e = s.db.QueryRow("SELECT request_digest,record FROM runs WHERE job_id=? AND caller=? AND idem=?", jobID, caller, key).Scan(&held, &before)
		if e == nil {
			if held != requestHash {
				return Run{}, false, &apiError{409, "idempotency_conflict", "This idempotency key was already used with different inputs."}
			}
			r, e := decodeRun([]byte(before))
			return r, true, e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return Run{}, false, e
		}
		job, e := s.job(jobID)
		if e != nil {
			return Run{}, false, e
		}
		release, e := s.release(job.ReleaseID)
		if e != nil {
			return Run{}, false, e
		}
		if !mappingMatchesRelease(release, input) {
			return Run{}, false, bad("mapping_mismatch", "Use this job’s reviewed input mapping.")
		}
		if e = s.checkReleaseProfiles(release); e != nil {
			return Run{}, false, e
		}
	}
	normalized, err := s.normalizeInput(input)
	if err != nil {
		return Run{}, false, err
	}
	input = normalized
	b, err := canonical(encode(input))
	if err != nil {
		return Run{}, false, err
	}
	hash := digest(b)
	if v2 {
		hash = requestHash
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, false, err
	}
	defer tx.Rollback()
	var held, before string
	err = tx.QueryRow("SELECT request_digest,record FROM runs WHERE job_id=? AND caller=? AND idem=?", jobID, caller, key).Scan(&held, &before)
	if err == nil {
		if held != hash {
			return Run{}, false, &apiError{409, "idempotency_conflict", "This idempotency key was already used with different inputs."}
		}
		r, err := decodeRun([]byte(before))
		return r, true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, err
	}
	var jb []byte
	if err = tx.QueryRow("SELECT record FROM jobs WHERE id=?", jobID).Scan(&jb); err != nil {
		return Run{}, false, err
	}
	var j Job
	if err = json.Unmarshal(jb, &j); err != nil {
		return Run{}, false, err
	}
	var releaseBytes []byte
	if err = tx.QueryRow("SELECT record FROM releases WHERE id=?", j.ReleaseID).Scan(&releaseBytes); err != nil {
		return Run{}, false, err
	}
	var release Release
	if err = json.Unmarshal(releaseBytes, &release); err != nil {
		return Run{}, false, err
	}
	if !mappingMatchesRelease(release, input) {
		return Run{}, false, bad("mapping_mismatch", "Use this job’s reviewed input mapping. Create a new job to change its input source or mapping.")
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM runs WHERE state IN ('queued','running')").Scan(&count); err != nil {
		return Run{}, false, err
	}
	if count >= queueLimit {
		return Run{}, false, &apiError{429, "queue_full", "The local queue is full. Try again after pending runs finish."}
	}
	r := Run{Trigger: origin, SchemaVersion: "1", ID: id("run_"), JobID: j.ID, ReleaseID: j.ReleaseID, Revision: j.Revision, State: "queued", Input: input, CreatedAt: now(), RequestedBy: caller}
	_, err = tx.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", r.ID, j.ID, caller, key, hash, r.State, runText(r))
	if err != nil {
		return Run{}, false, err
	}
	by := s.installation()
	if origin != nil {
		by = triggerActor(origin.TriggerID, origin.TriggerRevision)
	}
	if _, err = appendEvent(tx, runChange(r, "", by, EventDetail{})); err != nil {
		return Run{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Run{}, false, err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return r, false, nil
}
func (s *Service) worker(ctx context.Context) {
	defer close(s.done)
	defer func() {
		if ctx.Err() == nil {
			s.unhealthy.Store(true)
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		worked, err := s.dispatchNext(ctx)
		if err != nil {
			return
		}
		if !worked {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-ticker.C:
			}
		}
	}
}

// dispatchNext takes the oldest queued run, if any, through evaluation to its
// end, and says whether there was one. An error is a storage error, which
// stops the dispatcher.
func (s *Service) dispatchNext(ctx context.Context) (bool, error) {
	var b []byte
	err := s.db.QueryRow("SELECT record FROM runs WHERE state='queued' ORDER BY seq LIMIT 1").Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r, err := decodeRun(b)
	if err != nil {
		return false, err
	}
	if r.Trigger != nil && r.Trigger.ExpiresAt != "" {
		deadline, e := time.Parse(time.RFC3339Nano, r.Trigger.ExpiresAt)
		if e != nil || !time.Now().Before(deadline) {
			r.State = "failed"
			r.FinishedAt = now()
			r.Problem = "The automatic run expired in the queue before evaluation."
			return true, s.changeRun(r)
		}
	}
	r.State = "running"
	r.StartedAt = now()
	r.Attempt = 1
	if err = s.changeRun(r); err != nil {
		return true, err
	}
	release, err := s.release(r.ReleaseID)
	if err == nil && r.Trigger != nil && r.Input.Source != nil && r.Input.Source.Mapping.Version == 2 {
		// Queue time must not silently outlive source freshness. Verify now,
		// while retaining the exact input snapshot frozen at admission.
		err = s.checkReleaseProfiles(release)
		if err == nil {
			_, err = s.normalizeInput(r.Input)
		}
	}
	if err == nil {
		var trail, signatures []byte
		r.Result, trail, signatures, err = s.evaluate(ctx, release, r.Input, filepath.Join(s.cfg.Dir, "attempts", r.ID), false)
		r.retainAudit(trail, signatures)
	}
	r.FinishedAt = now()
	if err == nil {
		r.State = "completed"
	} else {
		r.State = "failed"
		r.Problem = err.Error()
		if ctx.Err() != nil {
			// Runner saw this stop: the interruption has its own time.
			r.State = "interrupted"
			r.Problem = "The runner stopped during evaluation. This run was not automatically repeated."
			r.InterruptedAt = r.FinishedAt
		}
	}
	// A completed run is recorded with its entry in the installation's
	// chain of runs, or not at all: a failed append stops the dispatcher as
	// any storage error does, and the run reads interrupted after a restart.
	return true, s.finishRun(r)
}
func (s *Service) records(kind, jobID string, after int64) ([]json.RawMessage, int64, error) {
	return s.filteredRecords(kind, jobID, after, recordFilter{})
}
