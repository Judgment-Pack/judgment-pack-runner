package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	// ExecutorAdmission is snapshotted strictly but remains inactive in Phase A.
	ExecutorAdmission *ManualAdmissionBinding
	// ExecutorOwnership opts this workspace into trusted external ownership.
	// Open snapshots it before taking the lease; nil preserves legacy behavior.
	ExecutorOwnership              *ExecutorOwnership
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
	// audit record with (runtime ADR-0047 §2b). Runner reads it once, at
	// start, for its public key, the expected key, and passes the path, never
	// the key, as JPACK_SIGNING_KEY, with the expected key as
	// JPACK_SIGNING_PUBLIC_KEY. Empty, nothing is signed.
	SigningKey string
	// SigningPublicKey is the public key the signing key is expected to
	// have, 64 lowercase hexadecimal characters, supplied independently of
	// the key's file, as Desk can from its list of public keys. Optional.
	// When given, the key read at start must have it, or Runner refuses to
	// start, and it is the expected key for as long as Runner runs. Without
	// it, the expected key is the one read at start.
	SigningPublicKey  string
	disableAutomation bool
	// disableDispatcher leaves queued runs for a test to dispatch, one at a
	// time, with dispatchNext.
	disableDispatcher bool
}
type Service struct {
	profilesMu             sync.RWMutex
	registeredProfiles     atomic.Pointer[[]InputProfile]
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
	runtime, runtimeDigest string

	// store keeps the queue, releases, inputs and run records
	// (store_contract.go). db is the sqlite kind's database handle, for the
	// state the contract does not cover yet (store_sqlite.go).
	store runStore
	// executor is fixed at Open and remains non-nil after Close.
	executor *ExecutorLease
	// authority is the immutable original inherited channel/session. It never
	// becomes a nil fallback after Close or transport failure.
	authority *manualAuthorityClient
	// expectedKey is the public key of the signing key as Runner read it at
	// start, fixed for as long as it runs: the key each run's record must be
	// signed under. Nil without a signing key.
	expectedKey ed25519.PublicKey
	// attempts is the store's attempts directory, held from start to close:
	// diagnostics are read through it (diagnostics.go).
	attempts    *os.Root
	cancel      context.CancelFunc
	done        chan struct{}
	wake        chan struct{}
	previewGate chan struct{}
	closeOnce   sync.Once
	// storedProfiles is each profile the store's row holds, as it holds it,
	// admitted or set aside; setAside is each set aside when the store opened
	// (profile_set_aside.go), fixed from then on. storedProfiles changes only
	// under profilesMu, with a registration.
	storedProfiles []json.RawMessage
	setAside       []SetAsideProfile
	// selectedProfiles is the selected-profile namespace's admitted profiles
	// (selected_profiles.go), published as registeredProfiles is;
	// selectedStored is each profile its row holds, as it holds it, and
	// selectedSetAside each set aside when the store opened. They change
	// only under profilesMu, with an admission.
	selectedProfiles atomic.Pointer[[]InputProfile]
	selectedStored   []json.RawMessage
	selectedSetAside []SetAsideProfile
}

func Open(cfg Config) (_ *Service, err error) {
	// The host's paths are in the path domain (path_domain.go), before any
	// lease, directory or store is touched.
	if err = checkPaths("dir", cfg.Dir, "workspace", cfg.Workspace, "runtime", cfg.Runtime, "inputRoot", cfg.InputRoot); err != nil {
		return nil, err
	}
	if cfg.ExecutorAdmission != nil {
		binding := *cfg.ExecutorAdmission
		cfg.ExecutorAdmission = &binding
		if binding.Validate(cfg.ExecutorOwnership, cfg.Workspace) != nil {
			return nil, errManualProtocol
		}
		// Do not expose an enabled profile before registration and the actual
		// operational starter have been integrated and proved together.
		if !ManualAdmissionAvailable() {
			return nil, errManualInactive
		}
	}
	// Snapshot the binding before any side effects. Never derive pins from state.
	var executor *ExecutorLease
	if cfg.ExecutorOwnership != nil {
		owned := *cfg.ExecutorOwnership
		cfg.ExecutorOwnership = &owned
		if err = owned.Validate(cfg.Workspace); err != nil {
			return nil, err
		}
		executor, err = AcquireExecutorLease(owned.Root, cfg.Dir, owned.MachineWorkspace, owned.identity())
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				executor.Close()
			}
		}()
	}
	// Sticky external demand remains binding when admission was omitted from a
	// later boot. It is never inferred from a portable store or repaired here.
	if err = checkManualDemand(cfg.ExecutorOwnership, cfg.ExecutorAdmission); err != nil {
		return nil, err
	}
	var authority *manualAuthorityClient
	if cfg.ExecutorAdmission != nil {
		channel, e := OpenInheritedManualAuthority(*cfg.ExecutorAdmission)
		if e != nil {
			return nil, e
		}
		authority = channel.(*manualAuthorityClient)
		defer func() {
			if err != nil {
				authority.Close()
			}
		}()
	}

	if err = validateProfiles(cfg.InputProfiles); err != nil {
		return nil, profileRefusal("configured", cfg.InputProfiles, err)
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
	if cfg.ExecutorAdmission == nil {
		if _, e := os.Lstat(filepath.Join(cfg.Dir, manualPrepareMarker)); !os.IsNotExist(e) {
			return nil, errManualProtocol
		}
	}
	if cfg.ExecutorAdmission != nil {
		if sqlitePreparedStore(cfg.Dir) != nil {
			return nil, errManualProtocol
		}
	}
	if err = privateDir(cfg.Dir); err != nil {
		return nil, err
	}
	expectedKey, err := expectedSigningKey(cfg)
	if err != nil {
		return nil, err
	}
	s := &Service{cloudProblems: map[string]string{}, automationDone: make(chan struct{}), cfg: cfg, executor: executor, authority: authority, expectedKey: expectedKey, wake: make(chan struct{}, 1), done: make(chan struct{}), previewGate: make(chan struct{}, 1)}
	defer func() {
		if err != nil {
			if s.store != nil {
				s.store.close()
			}
			if s.attempts != nil {
				s.attempts.Close()
			}
		}
	}()
	// The boot line's directory reaches the store only through the store
	// kind's constructor, which takes the store's lock, then runs afterLock,
	// then opens the database, and applies the schema after opened.
	s.store, err = openStore(defaultStoreKind, storeOpening{
		dir: cfg.Dir, workspace: cfg.Workspace, owner: cfg.Owner, inputRoot: cfg.InputRoot,
		migrate: cfg.ExecutorAdmission == nil,
		afterLock: func() error {
			for _, n := range []string{"releases", "attempts", "runtimes"} {
				if err := privateDir(filepath.Join(cfg.Dir, n)); err != nil {
					return err
				}
			}
			var err error
			s.attempts, err = openHeldRoot(filepath.Join(cfg.Dir, "attempts"))
			return err
		},
		opened: func(db *sql.DB) error {
			s.db = db
			if cfg.ExecutorAdmission != nil {
				return s.validateManualStore()
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	if err = s.loadRegisteredProfiles(); err != nil {
		return nil, err
	}
	if err = s.loadSelectedProfiles(); err != nil {
		return nil, err
	}
	// A configured Gateway connection names an installed profile, and one
	// that names a set-aside profile is refused by that profile's name.
	for _, g := range cfg.GatewayConnections {
		for _, p := range s.setAside {
			if g.Profile == p.ID {
				return nil, fmt.Errorf("a Gateway connection names the registered input profile %q, which is set aside: %s", p.ID, p.Reason)
			}
		}
	}
	backgroundConfig := cfg
	backgroundConfig.InputProfiles = s.inputProfiles()
	if err = validateBackgroundConfig(backgroundConfig); err != nil {
		return nil, err
	}
	s.runtime, s.runtimeDigest, err = s.pinRuntime(cfg.Runtime)
	if err != nil {
		return nil, err
	}
	if err = s.activateManualAuthority(); err != nil {
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
	if cfg.disableAutomation || cfg.ExecutorAdmission != nil {
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
		if s.authority != nil {
			s.authority.Close()
		}
		<-s.done
		<-s.automationDone
		s.background.Wait()
		s.recordStop()
		if s.attempts != nil {
			s.attempts.Close()
		}
		s.store.close()
		if s.executor != nil {
			s.executor.Close()
		}
	})
}
func (s *Service) release(id string) (Release, error) { return s.store.release(id) }
func (s *Service) job(id string) (Job, error)         { return s.store.job(id) }
func (s *Service) run(id string) (Run, error)         { return s.store.run(id) }
func (s *Service) createJob(name, releaseID string) (Job, error) {
	return s.createJobConfigured(name, releaseID, nil)
}
func (s *Service) createJobConfigured(name, releaseID string, c *TriggerConfig) (Job, error) {
	if s.cfg.ExecutorAdmission != nil && c != nil {
		return Job{}, manualUnsupported()
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	r, err := s.release(releaseID)
	if err != nil {
		return Job{}, err
	}
	if s.cfg.ExecutorAdmission != nil && !manualRelease(r) {
		return Job{}, manualUnsupported()
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
	err = tx.QueryRow("SELECT record FROM job_creations WHERE release_id=?", releaseID).Scan(&held)
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
	var used int
	if err = tx.QueryRow("SELECT count(*) FROM job_revisions WHERE release_id=?", releaseID).Scan(&used); err != nil {
		return Job{}, err
	}
	if used != 0 {
		return Job{}, &apiError{409, "release_already_used", "This preview already belongs to a job revision. Check a fresh release."}
	}
	// Only a new job is refused: a job made before the setting was on is returned
	// as any repeated creation is, and keeps running.
	if s.cfg.RequireTestedReleases && r.Tests == "not-run" {
		return Job{}, &apiError{409, "release_untested", "This installation creates jobs only from releases whose saved tests ran and passed. Save tests for this pack, then check a new release."}
	}
	// Likewise, a release that names a profile set aside since it was made
	// makes no new job; a job made from it before is returned as it was.
	if err = s.refuseSetAsideMapping(r.InputMapping); err != nil {
		return Job{}, err
	}
	if err = admitJobPack(tx, r); err != nil {
		return Job{}, err
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
	if err = recordInitialJob(tx, j); err != nil {
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
	return s.submitInternalIntent(jobID, key, input, origin, nil)
}
func (s *Service) submitInternalIntent(jobID, key string, input Input, origin *TriggerOrigin, manual *manualIntentInput) (Run, bool, error) {
	if s.cfg.ExecutorAdmission != nil {
		if origin != nil || input.Source != nil || input.Preparation != nil || manual == nil || manual.key != key || !bytes.Equal(input.Facts, []byte(manual.facts)) || !bytes.Equal(input.Evidence, []byte(manual.evidence)) {
			return Run{}, false, manualUnsupported()
		}
		var err error
		manual, err = s.inspectManualSubmission(jobID, manual)
		if err != nil {
			return Run{}, false, err
		}
	} else if manual != nil {
		return Run{}, false, errManualInactive
	}
	caller := s.cfg.Owner
	if origin != nil {
		caller = "trigger:" + origin.TriggerID
	}
	if s.unhealthy.Load() {
		return Run{}, false, &apiError{503, "dispatcher_stopped", "The dispatcher stopped after a storage error. Restart Desk after checking the runner storage."}
	}
	// An import that names a set-aside profile is refused by its name before
	// it is bound. A mapping that names one is refused once an identical
	// retry has been answered (checkReleaseProfiles), which still returns
	// its original acceptance.
	if input.Source != nil && input.Source.Import != nil {
		if err := s.refuseSetAside(input); err != nil {
			return Run{}, false, err
		}
	}
	// An import is bound before the mapping is compared with the release's:
	// the bound dialect is the mapping the run is held to.
	input, err := importedInput(input, s.inputProfiles())
	if err != nil {
		return Run{}, false, err
	}
	v2 := input.Source != nil && input.Source.Mapping.dialect()
	var requestHash string
	if v2 {
		input.Preparation = nil
		raw, e := canonical(encode(input))
		if e != nil {
			return Run{}, false, e
		}
		requestHash = digest(raw)
		// Identical retries return their original acceptance, even after freshness expires.
		held, e := s.store.requestedRun(jobID, caller, key)
		if e == nil {
			if held.digest != requestHash {
				return Run{}, false, &apiError{409, "idempotency_conflict", "This idempotency key was already used with different inputs."}
			}
			r, e := held.run()
			return r, true, e
		}
		if !absent(e) {
			return Run{}, false, e
		}
		// A mapping that names a selected profile is refused by that name
		// before the job, its release or the mapping's comparison with the
		// release is read (selected_profiles.go). An identical retry has been
		// answered above, as it is after a profile was set aside.
		if e = s.refuseSelectedIDs(mappingProfileIDs(input.Source.Mapping)); e != nil {
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
	if manual != nil {
		hash = digest([]byte(manual.request))
	}
	if v2 {
		hash = requestHash
	}
	var r Run
	var replayed bool
	var replayErr error
	err = s.store.update(func(tx runTx) error {
		held, err := tx.requestedRun(jobID, caller, key)
		if err == nil {
			if held.digest != hash {
				return &apiError{409, "idempotency_conflict", "This idempotency key was already used with different inputs."}
			}
			r, replayErr = held.run()
			replayed = true
			return errKeepNothing
		}
		if !absent(err) {
			return err
		}
		j, err := tx.job(jobID)
		if err != nil {
			return err
		}
		release, err := tx.release(j.ReleaseID)
		if err != nil {
			return err
		}
		if manual != nil {
			preimage, e := executionPayloadCanonical(release, maxExecutionRelease)
			if e != nil || manual.resource == nil || manual.registration == nil || manual.resource.JobID != j.ID || manual.resource.JobRevision != decimalRevision(j.Revision) || manual.resource.ReleaseID != j.ReleaseID || manual.resource.ReleaseDigest != digest(preimage) || !manualRelease(release) {
				return errManualProtocol
			}
		}
		if !mappingMatchesRelease(release, input) {
			return bad("mapping_mismatch", "Use this job’s reviewed input mapping. Create a new job to change its input source or mapping.")
		}
		count, err := tx.activeRuns()
		if err != nil {
			return err
		}
		if count >= queueLimit {
			return &apiError{429, "queue_full", "The local queue is full. Try again after pending runs finish."}
		}
		r = Run{manual: manual, Trigger: origin, SchemaVersion: "1", ID: id("run_"), JobID: j.ID, ReleaseID: j.ReleaseID, Revision: j.Revision, State: "queued", Input: input, CreatedAt: now(), RequestedBy: caller}
		if err = tx.createRun(r, caller, key, hash); err != nil {
			return err
		}
		by := s.installation()
		if origin != nil {
			by = triggerActor(origin.TriggerID, origin.TriggerRevision)
		}
		_, err = tx.appendJournal(runChange(r, "", by, EventDetail{}))
		return err
	})
	if replayed {
		return r, true, replayErr
	}
	if err != nil {
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
	r, err := s.store.oldestQueued()
	if absent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.manual != nil && s.cfg.ExecutorAdmission == nil || r.manual == nil && s.cfg.ExecutorAdmission != nil {
		return false, errManualInactive
	}
	if r.Trigger != nil && r.Trigger.ExpiresAt != "" {
		deadline, e := time.Parse(time.RFC3339Nano, r.Trigger.ExpiresAt)
		if e != nil || !time.Now().Before(deadline) {
			r.State = "failed"
			r.FinishedAt = now()
			r.Problem = "The automatic run expired in the queue before evaluation."
			r.Reason = runQueueExpired
			return true, s.changeRun(r)
		}
	}
	// A run queued before a profile its release names was set aside never
	// starts: it ends, failed, naming the profile.
	if release, e := s.release(r.ReleaseID); e == nil {
		if e = s.refuseSetAsideMapping(release.InputMapping); e != nil {
			r.State = "failed"
			r.FinishedAt = now()
			r.Problem = e.Error()
			r.Reason = runProfileSetAside
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
	if err == nil && r.Trigger != nil && r.Input.Source != nil && r.Input.Source.Mapping.dialect() {
		// Queue time must not silently outlive source freshness. Verify now,
		// while retaining the exact input snapshot frozen at admission.
		err = s.checkReleaseProfiles(release)
		if err == nil {
			_, err = s.normalizeInput(r.Input)
		}
	}
	if err == nil {
		var trail, signatures []byte
		var binding *RunBinding
		if s.cfg.ExecutorAdmission != nil {
			r.Result, trail, signatures, binding, err = s.governedEvaluation(ctx, r, release)
		} else {
			r.Result, trail, signatures, binding, err = s.evaluate(ctx, release, r.Input, filepath.Join(s.cfg.Dir, "attempts", r.ID), false)
		}
		// A sidecar that could not be read, with a signing key, fails the
		// record's signing and not the run (evaluateWith).
		unread := errors.Is(err, errSidecarUnread)
		if unread {
			err = nil
		}
		r.retainAudit(trail, signatures)
		if err == nil && s.expectedKey != nil {
			r.Signing = s.signingOf(r.AuditBytes, r.AuditSignatures, unread)
		}
		if err == nil {
			r.Binding = binding
		}
		// A binding the Runtime refused is Runner's defect, named as the
		// reason the run ended without an evaluation; it is never retried
		// without the binding.
		if errors.Is(err, errBindingRefused) {
			r.Reason = runBindingRefused
		}
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
