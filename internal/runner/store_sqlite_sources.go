package runner

// The source worker's database (source_worker.go): sources.sqlite in the
// worker's own directory, held by one worker process through the worker's
// lock. It is caller custody beside the Gateway, not the Runner's store, and
// stays outside the store contract (docs/design/store-interface.md, section
// 8). The statements are the ones the worker made before they moved here, and
// each error is the database's own.

import "database/sql"

// sqliteSources is the source worker's database.
type sqliteSources struct{ db *sql.DB }

// openSQLiteSources opens the worker's database at path with one connection,
// makes its tables, keeps gateway as the worker's Gateway when none is held,
// and returns the Gateway held. On an error it closes what it opened.
func openSQLiteSources(path, gateway string) (_ *sqliteSources, held string, err error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS worker_metadata (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS source_operations (id TEXT PRIMARY KEY,request TEXT NOT NULL,state TEXT NOT NULL,status TEXT NOT NULL);`)
	if err != nil {
		return nil, "", err
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO worker_metadata VALUES ('gateway',?)", gateway); err != nil {
		return nil, "", err
	}
	if err = db.QueryRow("SELECT value FROM worker_metadata WHERE key='gateway'").Scan(&held); err != nil {
		return nil, "", err
	}
	return &sqliteSources{db: db}, held, nil
}

func (k *sqliteSources) close() error { return k.db.Close() }

// interruptRunning marks every operation held running as needing
// attention: a durable claim is never reassigned after a process
// disappears.
func (k *sqliteSources) interruptRunning() error {
	_, err := k.db.Exec(`UPDATE source_operations SET state='needs-attention',status=json_set(status,'$.state','needs-attention','$.reason','The acquisition worker stopped before retaining the response. The source was not called again.') WHERE state='running'`)
	return err
}

// operation is the request and status held under id; a missing one is the
// no-rows error (sqliteNoRows).
func (k *sqliteSources) operation(id string) (request, status string, err error) {
	err = k.db.QueryRow("SELECT request,status FROM source_operations WHERE id=?", id).Scan(&request, &status)
	return request, status, err
}

// request is the request held under id; a missing one is the no-rows error
// (sqliteNoRows).
func (k *sqliteSources) request(id string) (request string, err error) {
	err = k.db.QueryRow("SELECT request FROM source_operations WHERE id=?", id).Scan(&request)
	return request, err
}

// count is how many operations are held.
func (k *sqliteSources) count() (n int, err error) {
	err = k.db.QueryRow("SELECT count(*) FROM source_operations").Scan(&n)
	return n, err
}

// add keeps a new operation.
func (k *sqliteSources) add(id, request, state, status string) error {
	_, err := k.db.Exec("INSERT INTO source_operations VALUES (?,?,?,?)", id, request, state, status)
	return err
}

// setStatus writes an operation's state and status, whatever it holds.
func (k *sqliteSources) setStatus(id, state, status string) error {
	_, err := k.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=?", state, status, id)
	return err
}

// claimQueued writes the state and status of an operation held queued.
func (k *sqliteSources) claimQueued(id, state, status string) error {
	_, err := k.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=? AND state='queued'", state, status, id)
	return err
}

// settleRunning writes the state and status of an operation held running.
func (k *sqliteSources) settleRunning(id, state, status string) error {
	_, err := k.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=? AND state='running'", state, status, id)
	return err
}
