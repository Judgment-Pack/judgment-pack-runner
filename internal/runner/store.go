package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct{ Dir, Runtime, Workspace, Owner string }
type Service struct {
	unhealthy              atomic.Bool
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
	if cfg.Workspace == "" || cfg.Owner == "" || !filepath.IsAbs(cfg.Dir) || !filepath.IsAbs(cfg.Runtime) {
		return nil, errors.New("absolute paths and workspace owner required")
	}
	if err = privateDir(cfg.Dir); err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, wake: make(chan struct{}, 1), done: make(chan struct{}), previewGate: make(chan struct{}, 1)}
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
	for k, v := range map[string]string{"schema": "1", "workspace": cfg.Workspace, "owner": cfg.Owner} {
		if _, err = s.db.Exec("INSERT OR IGNORE INTO metadata VALUES (?,?)", k, v); err != nil {
			return nil, err
		}
		var held string
		if err = s.db.QueryRow("SELECT value FROM metadata WHERE key=?", k).Scan(&held); err != nil {
			return nil, err
		}
		if held != v {
			return nil, errors.New("runner store belongs to another owner, workspace or schema")
		}
	}
	s.runtime, s.runtimeDigest, err = s.pinRuntime(cfg.Runtime)
	if err != nil {
		return nil, err
	}
	// Holding the process lock proves the old dispatcher is gone. Never replay an
	// invocation that may have appended an operational audit before crashing.
	rows, e := s.db.Query("SELECT record FROM runs WHERE state='running'")
	if e != nil {
		return nil, e
	}
	var interrupted []Run
	for rows.Next() {
		var b []byte
		var r Run
		if e = rows.Scan(&b); e != nil {
			break
		}
		if e = json.Unmarshal(b, &r); e != nil {
			break
		}
		interrupted = append(interrupted, r)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return nil, e
	}
	for _, r := range interrupted {
		r.State = "interrupted"
		r.FinishedAt = now()
		r.Problem = "The runner stopped during evaluation. Retained attempt files may contain an audit record. This run was not automatically repeated."
		if err = s.saveRun(r); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.worker(ctx)
	return s, nil
}
func (s *Service) Close() {
	s.closeOnce.Do(func() { s.cancel(); <-s.done; s.db.Close(); s.lock.Close() })
}
func (s *Service) saveRun(r Run) error {
	_, err := s.db.Exec("UPDATE runs SET state=?,record=? WHERE id=?", r.State, string(encode(r)), r.ID)
	return err
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
		e = json.Unmarshal(b, &r)
	}
	return r, e
}
func (s *Service) createJob(name, releaseID string) (Job, error) {
	r, err := s.release(releaseID)
	if err != nil {
		return Job{}, err
	}
	if !releaseReady(r) {
		return Job{}, &apiError{409, "release_not_ready", "Saved tests failed or could not complete. Correct the pack or cases and check a new release before creating a job."}
	}
	// One immutable revision in this pilot. Pack edits never change this pointer.
	j := Job{SchemaVersion: "1", ID: id("job_"), Name: name, ReleaseID: r.ID, Revision: 1, CreatedAt: now(), Workspace: s.cfg.Workspace, Owner: s.cfg.Owner}
	_, err = s.db.Exec("INSERT OR IGNORE INTO jobs(id,release_id,record) VALUES (?,?,?)", j.ID, j.ReleaseID, string(encode(j)))
	if err != nil {
		return Job{}, err
	}
	var held []byte
	if err = s.db.QueryRow("SELECT record FROM jobs WHERE release_id=?", j.ReleaseID).Scan(&held); err != nil {
		return Job{}, err
	}
	var stored Job
	if err = json.Unmarshal(held, &stored); err != nil {
		return Job{}, err
	}
	if stored.Name != name {
		return Job{}, &apiError{409, "release_already_used", "This preview already created a different job. Preview again to create another job."}
	}
	return stored, nil
}
func (s *Service) submit(jobID, key string, input Input) (Run, bool, error) {
	if s.unhealthy.Load() {
		return Run{}, false, &apiError{503, "dispatcher_stopped", "The dispatcher stopped after a storage error. Restart Desk after checking the runner storage."}
	}
	normalized, err := normalizeInput(input)
	if err != nil {
		return Run{}, false, err
	}
	input = normalized
	b, err := canonical(encode(input))
	if err != nil {
		return Run{}, false, err
	}
	hash := digest(b)
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, false, err
	}
	defer tx.Rollback()
	var held, before string
	err = tx.QueryRow("SELECT request_digest,record FROM runs WHERE job_id=? AND caller=? AND idem=?", jobID, s.cfg.Owner, key).Scan(&held, &before)
	if err == nil {
		if held != hash {
			return Run{}, false, &apiError{409, "idempotency_conflict", "This idempotency key was already used with different inputs."}
		}
		var r Run
		err = json.Unmarshal([]byte(before), &r)
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
	r := Run{SchemaVersion: "1", ID: id("run_"), JobID: j.ID, ReleaseID: j.ReleaseID, Revision: j.Revision, State: "queued", Input: input, CreatedAt: now(), RequestedBy: s.cfg.Owner}
	_, err = tx.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", r.ID, j.ID, s.cfg.Owner, key, hash, r.State, string(encode(r)))
	if err != nil {
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
		var b []byte
		err := s.db.QueryRow("SELECT record FROM runs WHERE state='queued' ORDER BY seq LIMIT 1").Scan(&b)
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-ticker.C:
			}
			continue
		}
		if err != nil {
			return
		}
		var r Run
		if json.Unmarshal(b, &r) != nil {
			return
		}
		r.State = "running"
		r.StartedAt = now()
		r.Attempt = 1
		if s.saveRun(r) != nil {
			return
		}
		release, err := s.release(r.ReleaseID)
		if err == nil {
			r.Result, r.Audit, err = s.evaluate(ctx, release, r.Input, filepath.Join(s.cfg.Dir, "attempts", r.ID), false)
		}
		r.FinishedAt = now()
		if err == nil {
			r.State = "completed"
		} else {
			r.State = "failed"
			r.Problem = err.Error()
			if ctx.Err() != nil {
				r.State = "interrupted"
				r.Problem = "The runner stopped during evaluation. This run was not automatically repeated."
			}
		}
		if s.saveRun(r) != nil {
			return
		}
	}
}
func (s *Service) records(kind, jobID string, after int64) ([]json.RawMessage, int64, error) {
	table := "jobs"
	if after == 0 {
		after = math.MaxInt64
	}
	where := "seq<?"
	args := []any{after}
	if kind == "runs" {
		table = "runs"
		where += " AND job_id=?"
		args = append(args, jobID)
	}
	rows, e := s.db.Query(fmt.Sprintf("SELECT seq,record FROM %s WHERE %s ORDER BY seq DESC LIMIT 51", table, where), args...)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	result := []json.RawMessage{}
	var last, next int64
	for rows.Next() {
		var seq int64
		var b []byte
		if e = rows.Scan(&seq, &b); e != nil {
			return nil, 0, e
		}
		if len(result) == 50 {
			next = last
			break
		}
		if kind == "runs" {
			var summary map[string]json.RawMessage
			if e = json.Unmarshal(b, &summary); e != nil {
				return nil, 0, e
			}
			delete(summary, "input")
			delete(summary, "audit")
			if raw := summary["result"]; len(raw) > 0 {
				var result map[string]json.RawMessage
				if e = json.Unmarshal(raw, &result); e != nil {
					return nil, 0, e
				}
				summary["result"] = encode(map[string]json.RawMessage{"disposition": result["disposition"], "handoffTarget": result["handoffTarget"]})
			}
			b = encode(summary)
		}
		result = append(result, json.RawMessage(b))
		last = seq
	}
	return result, next, rows.Err()
}
