package runner

// The sqlite kind's manual-admission state and its offline programs
// (docs/design/store-interface.md, section 6): the manual-admission journal
// and demand, which only this kind keeps (docs/design/store-postgresql.md,
// D4), and the database as relocation and manual store preparation open it,
// each in a program of its own. The statements are the ones the Runner made
// before they moved here.

import (
	"database/sql"
	"errors"
)

const manualJournalSchema = `CREATE TABLE manual_attempts(run_id TEXT PRIMARY KEY,intent_id TEXT NOT NULL UNIQUE,record TEXT NOT NULL);`

// sqliteQuestions are the statements of the questions manual admission,
// its preparation and relocation ask.
var sqliteQuestions = map[storeQuestion]string{
	askTriggers:         "SELECT count(*) FROM triggers",
	askUnpausedTriggers: "SELECT COUNT(*) FROM triggers WHERE json_extract(record,'$.paused') IS NOT 1",
	askJobTriggers:      "SELECT count(*) FROM triggers WHERE job_id=?",
	askActiveRuns:       "SELECT count(*) FROM runs WHERE state IN ('queued','running')",
	askOpenOccurrences:  "SELECT count(*) FROM occurrences WHERE state IN ('accepted','preparing','waiting','needs-attention')",
	askManualAttempts:   "SELECT count(*) FROM manual_attempts",
	askRunAttempts:      "SELECT count(*) FROM manual_attempts WHERE run_id=? OR intent_id=?",
}

// sqliteCount answers a question through q.
func sqliteCount(q sqliteQuerier, question storeQuestion, args ...any) (int, error) {
	statement, ok := sqliteQuestions[question]
	if !ok {
		return 0, storeFailure(faultRefused, "count", errors.New("no such question"))
	}
	var n int
	if err := q.QueryRow(statement, args...).Scan(&n); err != nil {
		return 0, sqliteFault("count", err, false)
	}
	return n, nil
}

// sqliteValue is the metadata row held under key, through q.
func sqliteValue(q sqliteQuerier, key string) (string, error) {
	var value string
	if err := q.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&value); err != nil {
		return "", sqliteFault("metadata", err, false)
	}
	return value, nil
}

// sqliteNoRows says whether err is a statement's no-rows error.
func sqliteNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// sqliteManual is the manual-admission state, read through the database or
// one read transaction.
type sqliteManual struct {
	q  sqliteQuerier
	db *sql.DB
}

func (m sqliteManual) manualValue(key string) (string, error) { return sqliteValue(m.q, key) }

func (m sqliteManual) manualCount(question storeQuestion, args ...any) (int, error) {
	return sqliteCount(m.q, question, args...)
}

func (m sqliteManual) manualRecord(records, id string) ([]byte, error) {
	var statement string
	switch records {
	case "jobs":
		statement = "SELECT record FROM jobs WHERE id=?"
	case "releases":
		statement = "SELECT record FROM releases WHERE id=?"
	default:
		return nil, storeFailure(faultRefused, "record", errors.New("no such records"))
	}
	var raw []byte
	if err := m.q.QueryRow(statement, id).Scan(&raw); err != nil {
		return nil, sqliteFault("record", err, false)
	}
	return raw, nil
}

func (m sqliteManual) queuedRunRecords(each func(record []byte, err error) error) error {
	rows, err := m.q.Query("SELECT record FROM runs WHERE state='queued'")
	if err != nil {
		return sqliteFault("run", err, false)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		err := rows.Scan(&raw)
		if err = each([]byte(raw), err); err != nil {
			return err
		}
	}
	return sqliteFault("run", rows.Err(), false)
}

func (m sqliteManual) readManual(fn func(manualReads) error) error {
	tx, err := m.db.Begin()
	if err != nil {
		return sqliteFault("begin", err, false)
	}
	defer tx.Rollback()
	if err = fn(sqliteManual{q: tx}); err != nil {
		return err
	}
	return sqliteFault("commit", tx.Commit(), false)
}

func (m sqliteManual) createManualAttempt(run, intent, record string) error {
	_, err := m.q.Exec("INSERT INTO manual_attempts(run_id,intent_id,record) VALUES(?,?,?)", run, intent, record)
	return sqliteFault("manual attempt", err, true)
}

func (m sqliteManual) replaceManualAttempt(run, before, after string) (int64, error) {
	result, err := m.q.Exec("UPDATE manual_attempts SET record=? WHERE run_id=? AND record=?", after, run, before)
	if err != nil {
		return 0, sqliteFault("manual attempt", err, true)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return -1, nil
	}
	return n, nil
}

// sqliteOffline is the sqlite kind's database as a program of its own opens
// it, offline, under the store's lock or a lease, in one transaction:
// relocation and manual store preparation.
type sqliteOffline struct {
	db *sql.DB
	tx *sql.Tx
}

// openSQLiteOffline opens database with one connection and pragmas, and
// begins its transaction.
func openSQLiteOffline(database, pragmas string) (_ *sqliteOffline, err error) {
	o := &sqliteOffline{}
	if o.db, err = sql.Open("sqlite", database); err != nil {
		return nil, err
	}
	o.db.SetMaxOpenConns(1)
	if _, err = o.db.Exec(pragmas); err == nil {
		o.tx, err = o.db.Begin()
	}
	if err != nil {
		o.db.Close()
		return nil, err
	}
	return o, nil
}

// openSQLiteRelocation opens the database for relocation.
func openSQLiteRelocation(database string) (*sqliteOffline, error) {
	return openSQLiteOffline(database, "PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;")
}

// openSQLiteManualPreparation opens the database for manual store
// preparation.
func openSQLiteManualPreparation(database string) (*sqliteOffline, error) {
	return openSQLiteOffline(database, "PRAGMA temp_store=MEMORY; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;")
}

// close gives the transaction up, keeping nothing not committed, and closes
// the database.
func (o *sqliteOffline) close() {
	o.tx.Rollback()
	o.db.Close()
}

func (o *sqliteOffline) value(key string) (string, error) { return sqliteValue(o.tx, key) }

func (o *sqliteOffline) count(question storeQuestion) (int, error) {
	return sqliteCount(o.tx, question)
}

// relocate moves the input root binding from previous to current, and keeps
// the relocation's record under key.
func (o *sqliteOffline) relocate(previous, current, key, record string) error {
	if _, err := o.tx.Exec("UPDATE metadata SET value=? WHERE key='inputRoot' AND value=?", current, previous); err != nil {
		return err
	}
	_, err := o.tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", key, record)
	return err
}

// prepareManual makes the manual-admission journal, keeps the demand and
// raises the schema to manualStoreSchema.
func (o *sqliteOffline) prepareManual(demand string) error {
	if _, err := o.tx.Exec(manualJournalSchema); err != nil {
		return err
	}
	if _, err := o.tx.Exec("INSERT INTO metadata VALUES ('manualExecutionDemand',?)", demand); err != nil {
		return err
	}
	_, err := o.tx.Exec("UPDATE metadata SET value=? WHERE key='schema'", manualStoreSchema)
	return err
}

func (o *sqliteOffline) commit() error { return o.tx.Commit() }
