package runner

// The sqlite kind (docs/design/store-interface.md): the store the Runner
// ships with, one SQLite database in the state directory the boot line names,
// in WAL mode with synchronous FULL, held by one process at a time through an
// exclusive lock on a file beside it. Its statements, transactions and
// pragmas are the ones the Runner made before the contract stood in front of
// them, moved here.
//
// The kind also keeps the state the contract does not cover yet, in the same
// database: jobs as they are made and revised, triggers, occurrences, briefs,
// workflow executions, the manual-admission journal, the profile registry and
// the store's metadata, and the journal's own reads. The Runner reaches that
// state through the database handle the kind gives it at opening
// (storeOpening.opened), and, inside a contract transaction, through
// sqliteTxOf.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// sqliteStoreFile is the kind's database, in the state directory.
	sqliteStoreFile = "runs.sqlite"
	// sqliteLockFile is the file whose exclusive lock holds the store for
	// one process.
	sqliteLockFile = "runner.lock"
)

// sqliteQuerier is what a read is made through: the database, or a
// transaction.
type sqliteQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

// sqliteReads are the reads both outside and inside a transaction: the same
// statement, made through the database or through the transaction.
type sqliteReads struct{ q sqliteQuerier }

// sqliteStore is the sqlite kind.
type sqliteStore struct {
	sqliteReads
	db   *sql.DB
	lock *os.File
	// commit makes a transaction durable: (*sql.Tx).Commit, which a test
	// replaces to make a commit fail.
	commit func(*sql.Tx) error
}

// sqliteTx is one transaction of the sqlite kind.
type sqliteTx struct {
	sqliteReads
	tx *sql.Tx
}

// sqliteFault types an error a statement of op returned: nothing held, a
// key held, a check refused, and otherwise a write that was not made or a
// read that failed.
func sqliteFault(op string, err error, write bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return storeFailure(faultAbsent, op, err)
	}
	var e *sqlite.Error
	if errors.As(err, &e) {
		switch e.Code() {
		case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
			return storeFailure(faultTaken, op, err)
		case sqlite3.SQLITE_CONSTRAINT_CHECK:
			return storeFailure(faultRefused, op, err)
		}
	}
	if write {
		return storeFailure(faultDurability, op, err)
	}
	return storeFailure(faultUnreadable, op, err)
}

// sqlitePreparedStore checks that a manual-admission store was prepared
// before the Runner starts on it: its database is there, private and the
// current user's.
func sqlitePreparedStore(dir string) error {
	return privateFile(filepath.Join(dir, sqliteStoreFile))
}

// openSQLiteStore is the sqlite kind's constructor, and the one place the
// boot line's directory reaches the store. It takes the lock, then runs the
// Runner's afterLock, opens the database with one connection, gives it to
// the Runner's opened, applies the schema, holds the store to its identity
// and, when asked, migrates it. On a failure it gives up what it took.
func openSQLiteStore(o storeOpening) (_ *sqliteStore, err error) {
	k := &sqliteStore{commit: (*sql.Tx).Commit}
	defer func() {
		if err != nil {
			if k.db != nil {
				k.db.Close()
			}
			if k.lock != nil {
				k.lock.Close()
			}
		}
	}()
	k.lock, err = instanceLock(filepath.Join(o.dir, sqliteLockFile))
	if err != nil {
		return nil, err
	}
	if o.afterLock != nil {
		if err = o.afterLock(); err != nil {
			return nil, err
		}
	}
	dbpath := filepath.Join(o.dir, sqliteStoreFile)
	f, e := os.OpenFile(dbpath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e == nil {
		f.Close()
	} else if !os.IsExist(e) {
		return nil, e
	}
	if err = privateFile(dbpath); err != nil {
		return nil, err
	}
	k.db, err = sql.Open("sqlite", dbpath)
	if err != nil {
		return nil, err
	}
	k.db.SetMaxOpenConns(1)
	if o.opened != nil {
		if err = o.opened(k.db); err != nil {
			return nil, err
		}
	}
	_, err = k.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS briefs (subject TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS releases (id TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS jobs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, release_id TEXT UNIQUE NOT NULL, record TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS runs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, job_id TEXT NOT NULL, caller TEXT NOT NULL, idem TEXT NOT NULL, request_digest TEXT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL, UNIQUE(job_id,caller,idem));
 CREATE INDEX IF NOT EXISTS runs_state ON runs(state,seq);`)
	if err != nil {
		return nil, err
	}
	if _, err = k.db.Exec(triggerSchema); err != nil {
		return nil, err
	}
	if _, err = k.db.Exec(runChainSchema); err != nil {
		return nil, err
	}
	for _, m := range []struct{ key, value string }{{"workspace", o.workspace}, {"owner", o.owner}, {"inputRoot", o.inputRoot}} {
		if _, err = k.db.Exec("INSERT OR IGNORE INTO metadata VALUES (?,?)", m.key, m.value); err != nil {
			return nil, err
		}
		var held string
		if err = k.db.QueryRow("SELECT value FROM metadata WHERE key=?", m.key).Scan(&held); err != nil {
			return nil, err
		}
		// A stored binding outside the path domain is refused by name,
		// whatever the host names now (path_domain.go).
		if m.key == "inputRoot" {
			if err = checkStoredBinding(held); err != nil {
				return nil, err
			}
		}
		if held != m.value {
			return nil, errors.New("runner store belongs to another owner, workspace or schema")
		}
	}
	// A store an earlier Runner kept, at schema "1", or a new one, begins its
	// journal here and is raised to schema "2", which every earlier Runner
	// refuses (docs/design/activity-journal.md, section 4).
	if o.migrate {
		if err = k.migrate(); err != nil {
			return nil, err
		}
	}
	k.sqliteReads = sqliteReads{k.db}
	return k, nil
}

// migrate begins the journal of a store that has none, a new store or one an
// earlier Runner kept, in one transaction: it writes journal.began, with what
// the store held then and nothing back-filled, adds the column that names the
// revision a trigger's key was issued at, and raises the store's schema to
// storeSchema.
func (k *sqliteStore) migrate() error {
	tx, err := k.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var schema string
	switch err = tx.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	}
	switch schema {
	case storeSchema:
		return nil
	case "", "1", "2":
	default:
		return errors.New("runner store belongs to another owner, workspace or schema")
	}
	if schema != "2" {
		// SQLite's schema changes are transactional: a migration that fails
		// leaves no table, column or entry behind.
		if _, err = tx.Exec(eventsSchema); err != nil {
			return err
		}
		var held EventHeld
		for table, count := range map[string]*int{"jobs": &held.Jobs, "triggers": &held.Triggers, "occurrences": &held.Occurrences, "runs": &held.Runs} {
			if err = tx.QueryRow("SELECT count(*) FROM " + table).Scan(count); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("ALTER TABLE triggers ADD COLUMN key_revision INTEGER"); err != nil {
			return err
		}
		if _, err = appendEvent(tx, change{kind: "journal.began", by: runnerActor, detail: EventDetail{Held: &held}}); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(jobRevisionSchema); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO metadata VALUES ('schema',?)", storeSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// close closes the database, then gives the lock up.
func (k *sqliteStore) close() error {
	err := k.db.Close()
	if e := k.lock.Close(); err == nil {
		err = e
	}
	return err
}

func (k *sqliteStore) update(fn func(runTx) error) error {
	tx, err := k.db.Begin()
	if err != nil {
		return storeFailure(faultDurability, "begin", err)
	}
	defer tx.Rollback()
	if err = fn(&sqliteTx{sqliteReads{tx}, tx}); err != nil {
		return err
	}
	if err = k.commit(tx); err != nil {
		return storeFailure(faultDurability, "commit", err)
	}
	return nil
}

// sqliteTxOf is the SQL transaction under a sqlite kind's transaction, for
// the state outside the contract that the Runner changes in the same
// transaction as the contract's (a restart's occurrences).
func sqliteTxOf(tx runTx) (*sql.Tx, error) {
	if t, ok := tx.(*sqliteTx); ok {
		return t.tx, nil
	}
	return nil, storeFailure(faultRefused, "sqlite", errors.New("the state outside the store contract is kept only by the sqlite kind"))
}

func (r sqliteReads) release(id string) (Release, error) {
	var rel Release
	var b []byte
	if err := r.q.QueryRow("SELECT record FROM releases WHERE id=?", id).Scan(&b); err != nil {
		return rel, sqliteFault("release", err, false)
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return rel, storeFailure(faultUnreadable, "release", err)
	}
	if e := validateStoredRelease(rel); e != nil {
		return rel, storeFailure(faultUnreadable, "release", e)
	}
	return rel, nil
}

func (r sqliteReads) job(id string) (Job, error) {
	var j Job
	var b []byte
	if err := r.q.QueryRow("SELECT record FROM jobs WHERE id=?", id).Scan(&b); err != nil {
		return j, sqliteFault("job", err, false)
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, storeFailure(faultUnreadable, "job", err)
	}
	return j, nil
}

func (r sqliteReads) requestedRun(jobID, caller, key string) (heldRequest, error) {
	var held heldRequest
	var record string
	if err := r.q.QueryRow("SELECT request_digest,record FROM runs WHERE job_id=? AND caller=? AND idem=?", jobID, caller, key).Scan(&held.digest, &record); err != nil {
		return heldRequest{}, sqliteFault("run", err, false)
	}
	held.record = []byte(record)
	return held, nil
}

func (r sqliteReads) entrySequence(run string) (int64, error) {
	var sequence int64
	if err := r.q.QueryRow("SELECT sequence FROM run_chain WHERE run=?", run).Scan(&sequence); err != nil {
		return 0, sqliteFault("chain", err, false)
	}
	return sequence, nil
}

func (k *sqliteStore) run(id string) (Run, error) {
	var b []byte
	if err := k.db.QueryRow("SELECT record FROM runs WHERE id=?", id).Scan(&b); err != nil {
		return Run{}, sqliteFault("run", err, false)
	}
	r, err := decodeRun(b)
	if err != nil {
		return r, storeFailure(faultUnreadable, "run", err)
	}
	return r, nil
}

func (k *sqliteStore) oldestQueued() (Run, error) {
	var b []byte
	if err := k.db.QueryRow("SELECT record FROM runs WHERE state='queued' ORDER BY seq LIMIT 1").Scan(&b); err != nil {
		return Run{}, sqliteFault("queue", err, false)
	}
	r, err := decodeRun(b)
	if err != nil {
		return r, storeFailure(faultUnreadable, "queue", err)
	}
	return r, nil
}

func (k *sqliteStore) entryLine(run string) ([]byte, error) {
	var line []byte
	if err := k.db.QueryRow("SELECT line FROM run_chain WHERE run=?", run).Scan(&line); err != nil {
		return nil, sqliteFault("chain", err, false)
	}
	return line, nil
}

// chainPage reads the chain's rows in order. Rows are only ever appended, so
// pages read in turn are a prefix of the chain as it stood at some moment
// during the reading.
func (k *sqliteStore) chainPage(after int64, limit int) ([][]byte, int64, error) {
	rows, err := k.db.Query("SELECT sequence,line FROM run_chain WHERE sequence>? ORDER BY sequence LIMIT ?", after, limit)
	if err != nil {
		return nil, 0, sqliteFault("chain", err, false)
	}
	defer rows.Close()
	var lines [][]byte
	last := after
	for rows.Next() {
		var line []byte
		if err = rows.Scan(&last, &line); err != nil {
			return nil, 0, sqliteFault("chain", err, false)
		}
		lines = append(lines, line)
	}
	return lines, last, sqliteFault("chain", rows.Err(), false)
}

func (t *sqliteTx) activeRuns() (int, error) {
	var count int
	if err := t.tx.QueryRow("SELECT count(*) FROM runs WHERE state IN ('queued','running')").Scan(&count); err != nil {
		return 0, sqliteFault("queue", err, false)
	}
	return count, nil
}

func (t *sqliteTx) createRun(r Run, caller, key, requestDigest string) error {
	if e := validateStoredRun(r); e != nil {
		return e
	}
	_, err := t.tx.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", r.ID, r.JobID, caller, key, requestDigest, r.State, runText(r))
	return sqliteFault("run", err, true)
}

func (t *sqliteTx) putRun(r Run, expected string) (string, error) {
	if e := validateStoredRun(r); e != nil {
		return "", e
	}
	var from string
	if err := t.tx.QueryRow("SELECT state FROM runs WHERE id=?", r.ID).Scan(&from); err != nil {
		return "", sqliteFault("run", err, false)
	}
	if expected != anyState && from != expected {
		return from, storeFailure(faultConflict, "run", fmt.Errorf("the run %s is %s, not %s", r.ID, from, expected))
	}
	if _, err := t.tx.Exec("UPDATE runs SET state=?,record=? WHERE id=?", r.State, runText(r), r.ID); err != nil {
		return from, sqliteFault("run", err, true)
	}
	return from, nil
}

func (t *sqliteTx) runningRuns() ([]Run, error) {
	var runs []Run
	if err := eachRecord(t.tx, "SELECT record FROM runs WHERE state='running' ORDER BY seq", func(b []byte) error {
		r, e := decodeRun(b)
		runs = append(runs, r)
		return e
	}); err != nil {
		return runs, storeFailure(faultUnreadable, "queue", err)
	}
	return runs, nil
}

func (t *sqliteTx) lastEntry() (int64, []byte, error) {
	var last int64
	var line []byte
	if err := t.tx.QueryRow("SELECT sequence,line FROM run_chain ORDER BY sequence DESC LIMIT 1").Scan(&last, &line); err != nil {
		return 0, nil, sqliteFault("chain", err, false)
	}
	return last, line, nil
}

func (t *sqliteTx) createEntry(sequence int64, run string, line []byte) error {
	_, err := t.tx.Exec("INSERT INTO run_chain(sequence,run,line) VALUES (?,?,?)", sequence, run, line)
	return sqliteFault("chain", err, true)
}

func (t *sqliteTx) createRelease(r Release) error {
	if e := validateStoredRelease(r); e != nil {
		return e
	}
	_, err := t.tx.Exec("INSERT INTO releases(id,record) VALUES (?,?)", r.ID, string(encode(r)))
	return sqliteFault("release", err, true)
}

func (t *sqliteTx) appendJournal(c change) (int64, error) {
	sequence, err := appendEvent(t.tx, c)
	return sequence, sqliteFault("journal", err, true)
}
