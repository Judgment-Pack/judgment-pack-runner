package runner

// The postgresql kind (docs/design/store-postgresql.md): the second storage
// kind, on PostgreSQL 16, in a database of its own and the schema
// jpack_runner, which `jpack-runner store migrate` makes and the run-time
// logins neither own nor can alter. A Runner holds one connection pool for
// each run-time login (section 6): the request login, whose every transaction
// and read is held to one scope by set_config('jpack.scope', ..., true), and
// the worker login, which does the work for every scope. It keeps no state in
// the process beyond those pools: no process-wide lock and no cache of
// records.
//
// Every statement of the kind is in its files (store_postgresql*.go;
// TestNoStatementOutsideTheSQLiteKind), each compared with the database's
// clock where it compares a lease (D3).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	// postgresqlKind is the kind's name in storeKinds and in a service
	// configuration's store member.
	postgresqlKind = "postgresql"
	// postgresqlSchema is the schema the migrations make.
	postgresqlSchema = "jpack_runner"
	// postgresqlRetries bounds how many times update runs a transaction's fn
	// again after a serialization failure or a deadlock (D2).
	postgresqlRetries = 10
)

// PostgreSQLConnections are the run-time logins a Runner of the postgresql
// kind connects as, each a connection string, and the owner role neither may
// be a member of (section 5).
type PostgreSQLConnections struct {
	Request, Worker string
	Owner           string
}

// pgQuerier is what a statement is made through: a pool, or a transaction.
type pgQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// pgPools are what every view of one store shares.
type pgPools struct {
	request, worker *sql.DB
	// closers close the driver's pools under request and worker, which
	// closing a *sql.DB leaves open.
	closers []func()
	// commit makes a transaction durable: (*sql.Tx).Commit, which a test
	// replaces to make a commit fail.
	commit func(*sql.Tx) error
}

// pgReads are the reads of a view held to sc: inside a transaction through
// tx, and otherwise each its own statement, held to the view's scope. Every
// statement is made under c, the context of a transaction held to a deadline
// (updateWithin), or pgContext.
type pgReads struct {
	p  *pgPools
	sc storeScope
	tx *sql.Tx
	c  context.Context
}

// ctx is the context the view's statements are made under.
func (r pgReads) ctx() context.Context {
	if r.c == nil {
		return pgContext
	}
	return r.c
}

// pgStore is a view of the postgresql kind, held to a scope, every scope's as
// it is opened.
type pgStore struct{ pgReads }

// pgTx is one transaction of the postgresql kind.
type pgTx struct{ pgReads }

var pgContext = context.Background()

// pgBounds are the time bounds every pooled connection of the kind holds,
// set as the session's start parameters (docs/postgresql-store.md, "Time
// bounds"). lock is lock_timeout: a statement waits no longer for a lock,
// journal_head's that every write takes first among them; a third of the
// renewal's interval, so that a renewal that meets a lock held fails in
// time for the claimant to stop its Runtime well inside the lease.
// statement is statement_timeout, above lock so that a wait for a lock is
// reported as one, and a bound on any statement that holds a pooled
// connection. idle is idle_in_transaction_session_timeout: a session left
// idle inside a transaction, holding journal_head's lock, is ended by the
// server, so that the writers waiting behind it meet at most two lock
// timeouts. Tests shorten them.
var pgBounds = struct{ lock, statement, idle time.Duration }{5 * time.Second, 30 * time.Second, 10 * time.Second}

// The refusals of a statement or a transaction that met a time bound, each
// within the bound it names (pgFault, updateWithin).
var (
	errPostgreSQLLockWait      = errors.New("the postgresql store waited longer than its lock bound (lock_timeout) for a lock another connection holds")
	errPostgreSQLStatementTime = errors.New("a statement of the postgresql store ran longer than its bound (statement_timeout), or was cancelled")
	errPostgreSQLIdleSession   = errors.New("the postgresql store's session was ended after it stayed idle inside a transaction longer than its bound (idle_in_transaction_session_timeout)")
	errPostgreSQLDeadline      = errors.New("a transaction of the postgresql store did not end within its deadline")
)

// pgScopeValue is the value of jpack.scope that holds a transaction to the
// scope identifier id: "s:" and id, "s:" alone for none (section 6). An unset
// or empty setting holds a transaction to no row.
func pgScopeValue(id string) string { return "s:" + id }

// pool is the login a view's work is made on.
func (r pgReads) pool() *sql.DB {
	if r.sc.every {
		return r.p.worker
	}
	return r.p.request
}

// read makes fn's statements: in the transaction, or for a view held to a
// scope in a read transaction of its own at READ COMMITTED with the scope set
// for it, or on the worker login as one statement. A failure to begin, to set
// the scope or to commit is faultUnavailable; fn maps its own.
func (r pgReads) read(op string, fn func(q pgQuerier) error) error {
	if r.tx != nil {
		return fn(r.tx)
	}
	if r.sc.every {
		return fn(r.p.worker)
	}
	tx, err := r.p.request.BeginTx(r.ctx(), &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true})
	if err != nil {
		return storeFailure(faultUnavailable, op, err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.ctx(), "SELECT set_config('jpack.scope', $1, true)", pgScopeValue(r.sc.id)); err != nil {
		return storeFailure(faultUnavailable, op, err)
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return storeFailure(faultUnavailable, op, err)
	}
	return nil
}

// pgFault types an error a statement of op returned, as sqliteFault does: a
// unique key held (23505), naming the key where the Runner tells keys apart;
// a check, a row policy or a privilege refused (23514, 23503, 42501); and
// otherwise a write that was not made or a read that failed.
func pgFault(op string, err error, write bool) error {
	if err == nil {
		return nil
	}
	if faultOf(err) != "" {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return storeFailure(faultAbsent, op, err)
	}
	fault := faultUnavailable
	if write {
		fault = faultDurability
	}
	var e *pgconn.PgError
	if errors.As(err, &e) {
		switch e.Code {
		case "23505":
			if key := pgKeys[e.ConstraintName]; key != "" {
				return takenOn(op, key, err)
			}
			return storeFailure(faultTaken, op, err)
		case "23514", "23503", "42501":
			return storeFailure(faultRefused, op, err)
		// A time bound met: the statement made nothing, and its transaction
		// keeps nothing (pgBounds).
		case "55P03":
			return storeFailure(fault, op, fmt.Errorf("%w: %v", errPostgreSQLLockWait, err))
		case "57014":
			return storeFailure(fault, op, fmt.Errorf("%w: %v", errPostgreSQLStatementTime, err))
		case "25P03":
			return storeFailure(fault, op, fmt.Errorf("%w: %v", errPostgreSQLIdleSession, err))
		}
	}
	return storeFailure(fault, op, err)
}

// pgKeys are the keys a faultTaken names (storeKey), by the constraint the
// schema names (0001_schema.sql), for every key D9 holds across scopes. A
// creation meets them in the order job_creations, job_revisions, jobs.
var pgKeys = map[string]storeKey{
	"job_creations_pkey":           keyJobCreation,
	"job_revisions_release_id_key": keyJobRevision,
	"jobs_release_id_key":          keyJobRelease,
	"triggers_cloud_binding":       keyCloudBinding,
}

// pgRetried says whether err is a serialization failure or a deadlock, after
// which update runs fn again (D2).
func pgRetried(err error) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && (e.Code == "40001" || e.Code == "40P01")
}

// update runs fn in one serializable transaction, held to the view's scope,
// and runs it again on a serialization failure or a deadlock, up to
// postgresqlRetries times, then reports faultDurability. Its first statement
// takes journal_head's lock, before the transaction's snapshot is taken, so
// that writers commit one at a time and the journal's sequence follows the
// order of commits (D13); its second sets the scope, for the transaction
// only.
func (k *pgStore) update(fn func(runTx) error) error {
	for attempt := 0; ; attempt++ {
		err := k.once(fn)
		if !pgRetried(err) {
			return err
		}
		if attempt == postgresqlRetries {
			return storeFailure(faultDurability, "commit", err)
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
}

func (k *pgStore) once(fn func(runTx) error) error {
	tx, err := k.pool().BeginTx(k.ctx(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return storeFailure(faultDurability, "begin", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(k.ctx(), "LOCK TABLE journal_head IN EXCLUSIVE MODE"); err != nil {
		return pgFault("begin", err, true)
	}
	if !k.sc.every {
		if _, err = tx.ExecContext(k.ctx(), "SELECT set_config('jpack.scope', $1, true)", pgScopeValue(k.sc.id)); err != nil {
			return pgFault("begin", err, true)
		}
	}
	if err = fn(&pgTx{pgReads{k.p, k.sc, tx, k.c}}); err != nil {
		return err
	}
	if err = k.p.commit(tx); err != nil {
		if pgRetried(err) {
			return err
		}
		return storeFailure(faultDurability, "commit", err)
	}
	return nil
}

// updateWithin is update with its transaction, every retry of it and every
// statement in it held to d from now: a transaction not ended by then is
// rolled back and refused as errPostgreSQLDeadline, its statement in flight
// cancelled on the server. The claim's renewal is made so (claims.go), so
// that a claimant never waits on one longer than its interval.
func (k *pgStore) updateWithin(d time.Duration, fn func(runTx) error) error {
	ctx, cancel := context.WithTimeout(k.ctx(), d)
	defer cancel()
	err := (&pgStore{pgReads{k.p, k.sc, nil, ctx}}).update(fn)
	if f := faultOf(err); err != nil && ctx.Err() != nil && f != faultConflict && f != faultRefused {
		return storeFailure(faultDurability, "deadline", fmt.Errorf("%w (%s): %v", errPostgreSQLDeadline, d, err))
	}
	return err
}

// heldTo is a view of the same pools, held to sc.
func (k *pgStore) heldTo(sc storeScope) runStore {
	return &pgStore{pgReads{k.p, sc, nil, k.c}}
}

// missingAttemptHoldsNoAudit is no: the store outlives the host's disk, so an
// attempt directory that does not exist may have been written elsewhere
// (D11).
func (k *pgStore) missingAttemptHoldsNoAudit() bool { return false }

// close closes both pools.
func (k *pgStore) close() error {
	err := k.p.request.Close()
	if e := k.p.worker.Close(); err == nil {
		err = e
	}
	for _, c := range k.p.closers {
		c()
	}
	return err
}

// pgOpenPool opens a pool for one login, with the kind's schema first in its
// search path and the system's after it, and pgBounds, and the function that
// closes the driver's pool under it. The settings are the session's start
// parameters, stored nowhere, and override what the connection string says.
// A statement whose context ends (updateWithin) has its connection closed by
// the driver, which sends the server a cancel request first, so that the
// statement does not go on waiting there for a lock.
func pgOpenPool(conn string) (*sql.DB, func(), error) {
	cfg, err := pgxpool.ParseConfig(conn)
	if err != nil {
		return nil, nil, errors.New("a connection string of the postgresql store does not read")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = postgresqlSchema + ", pg_catalog"
	cfg.ConnConfig.RuntimeParams["application_name"] = "jpack-runner"
	for name, d := range map[string]time.Duration{"lock_timeout": pgBounds.lock, "statement_timeout": pgBounds.statement, "idle_in_transaction_session_timeout": pgBounds.idle} {
		cfg.ConnConfig.RuntimeParams[name] = strconv.FormatInt(d.Milliseconds(), 10)
	}
	pool, err := pgxpool.NewWithConfig(pgContext, cfg)
	if err != nil {
		return nil, nil, err
	}
	return stdlib.OpenDBFromPool(pool), pool.Close, nil
}

// newPostgreSQLStore is a store over two pools, every scope's, closing
// closers when it closes.
func newPostgreSQLStore(request, worker *sql.DB, closers ...func()) *pgStore {
	return &pgStore{pgReads{&pgPools{request: request, worker: worker, closers: closers, commit: (*sql.Tx).Commit}, everyScope, nil, nil}}
}

// openPostgreSQLStore is the kind's constructor. It opens a pool for each
// run-time login, refuses logins that could read every scope or alter the
// schema, and a store whose migrations are not this Runner's, then holds the
// store to its identity and, when asked, begins its journal. It takes no
// lock: a run is held by its claim's lease (D3). Manual admission's state is
// the sqlite kind's alone (D4): o.manual is not run.
func openPostgreSQLStore(o storeOpening) (_ *pgStore, err error) {
	c := o.postgresql
	if c.Request == "" || c.Worker == "" || c.Owner == "" {
		return nil, storeFailure(faultRefused, "open", errors.New("the postgresql store needs its request and worker logins and its owner role"))
	}
	request, closeRequest, err := pgOpenPool(c.Request)
	if err != nil {
		return nil, err
	}
	worker, closeWorker, err := pgOpenPool(c.Worker)
	if err != nil {
		request.Close()
		closeRequest()
		return nil, err
	}
	k := newPostgreSQLStore(request, worker, closeRequest, closeWorker)
	defer func() {
		if err != nil {
			k.close()
		}
	}()
	if err = k.checkLogins(c.Owner); err != nil {
		return nil, err
	}
	if err = k.checkMigrations(); err != nil {
		return nil, err
	}
	if err = k.start(o); err != nil {
		return nil, err
	}
	return k, nil
}

// start runs the Runner's afterLock, holds the store to its identity and,
// when asked, begins its journal.
func (k *pgStore) start(o storeOpening) error {
	if o.afterLock != nil {
		if err := o.afterLock(); err != nil {
			return err
		}
	}
	if err := k.holdIdentity(o); err != nil {
		return err
	}
	if o.migrate {
		return k.beginJournal()
	}
	return nil
}

// errLoginTooWide is the refusal of a run-time login that could read every
// scope or change the schema.
var errLoginTooWide = errors.New("a run-time login of the postgresql store is a superuser, bypasses row security, can create roles or databases, or is a member of the owner role; or the request login is the worker login or a member of it")

// checkLogins refuses a run-time login that is a superuser, bypasses row
// security, can create roles or databases, or is a member of the owner; and
// a request login that is the worker login or a member of it, which would
// reach every scope.
func (k *pgStore) checkLogins(owner string) error {
	names := map[*sql.DB]string{}
	for _, db := range []*sql.DB{k.p.request, k.p.worker} {
		var name string
		var wide bool
		err := db.QueryRowContext(pgContext, `SELECT current_user, rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR pg_has_role(current_user, (SELECT oid FROM pg_roles WHERE rolname=$1), 'MEMBER') FROM pg_roles WHERE rolname=current_user`, owner).Scan(&name, &wide)
		if err != nil {
			return pgFault("open", err, false)
		}
		if wide {
			return storeFailure(faultRefused, "open", errLoginTooWide)
		}
		names[db] = name
	}
	var member bool
	if err := k.p.request.QueryRowContext(pgContext, "SELECT pg_has_role(current_user, (SELECT oid FROM pg_roles WHERE rolname=$1), 'MEMBER')", names[k.p.worker]).Scan(&member); err != nil {
		return pgFault("open", err, false)
	}
	if member {
		return storeFailure(faultRefused, "open", errLoginTooWide)
	}
	// The logins are the ones the grants were made to (D6).
	var request, worker string
	if err := k.p.worker.QueryRowContext(pgContext, "SELECT request_login, worker_login FROM schema_migrations WHERE version=$1", pgGrantsVersion).Scan(&request, &worker); err != nil {
		return storeFailure(faultRefused, "open", errors.New("the postgresql store is not migrated: run jpack-runner store migrate"))
	}
	if request != names[k.p.request] || worker != names[k.p.worker] {
		return storeFailure(faultRefused, "open", errors.New("the postgresql store's grants were made to other logins than the ones configured"))
	}
	return nil
}

// checkMigrations refuses a store whose applied migrations are not exactly
// this Runner's set, each by its digest: an earlier set is migrated first,
// and a later one is another Runner's.
func (k *pgStore) checkMigrations() error {
	set, err := postgresqlMigrations()
	if err != nil {
		return err
	}
	rows, err := k.p.worker.QueryContext(pgContext, "SELECT version, digest FROM schema_migrations ORDER BY version")
	if err != nil {
		return pgFault("open", err, false)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var version, digest string
		if err = rows.Scan(&version, &digest); err != nil {
			return pgFault("open", err, false)
		}
		if i >= len(set) || set[i].version != version || set[i].digest != digest {
			return storeFailure(faultRefused, "open", errors.New("the postgresql store holds a migration this Runner does not ship"))
		}
		i++
	}
	if err = rows.Err(); err != nil {
		return pgFault("open", err, false)
	}
	if i != len(set) {
		return storeFailure(faultRefused, "open", errors.New("the postgresql store is not migrated to this Runner's schema: run jpack-runner store migrate"))
	}
	return nil
}

// holdIdentity keeps the store's identity the first time, on the worker
// login, and refuses a store another workspace, owner or input root holds.
func (k *pgStore) holdIdentity(o storeOpening) error {
	for _, m := range []struct{ key, value string }{{"workspace", o.workspace}, {"owner", o.owner}, {"inputRoot", o.inputRoot}} {
		if _, err := k.p.worker.ExecContext(pgContext, "INSERT INTO metadata(key,value) VALUES ($1,$2) ON CONFLICT (key) DO NOTHING", m.key, m.value); err != nil {
			return pgFault("open", err, true)
		}
		var held string
		if err := k.p.worker.QueryRowContext(pgContext, "SELECT value FROM metadata WHERE key=$1", m.key).Scan(&held); err != nil {
			return pgFault("open", err, false)
		}
		if m.key == "inputRoot" {
			if err := checkStoredBinding(held); err != nil {
				return err
			}
		}
		if held != m.value {
			return errors.New("runner store belongs to another owner, workspace or schema")
		}
	}
	return nil
}

// beginJournal writes journal.began, with what the store holds, in a store
// whose journal has not begun, in one transaction under the journal's lock.
func (k *pgStore) beginJournal() error {
	return k.update(func(tx runTx) error {
		t := tx.(*pgTx)
		var began int
		if err := t.tx.QueryRowContext(t.ctx(), "SELECT count(*) FROM events WHERE kind='journal.began'").Scan(&began); err != nil {
			return pgFault("journal", err, false)
		}
		if began > 0 {
			return nil
		}
		var held EventHeld
		if err := t.tx.QueryRowContext(t.ctx(), "SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM triggers), (SELECT count(*) FROM occurrences), (SELECT count(*) FROM runs)").Scan(&held.Jobs, &held.Triggers, &held.Occurrences, &held.Runs); err != nil {
			return pgFault("journal", err, false)
		}
		_, err := tx.appendJournal(change{kind: "journal.began", by: runnerActor, detail: EventDetail{Held: &held}})
		return err
	})
}

// pgArgs builds a statement's numbered placeholders.
type pgArgs []any

func (a *pgArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

// pgJSON is the text of a record's member at path, as json_extract reads it
// at '$.a.b': the member's JSON text, or NULL (D5: json, never jsonb).
func pgJSON(column string, path ...string) string {
	expr := column + "::json"
	for i, p := range path {
		op := "->"
		if i == len(path)-1 {
			op = "->>"
		}
		expr += op + "'" + p + "'"
	}
	return "(" + expr + ")"
}

// pgFold folds ASCII letters to lower case and no other, as SQLite's lower()
// does.
func pgFold(expr string) string {
	return "translate(" + expr + ", 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz')"
}

// pgLease is a claim's lease as an interval's seconds.
var pgLease = claimLease.Seconds()

// pgRefusedKind is the refusal of a record kind outside scopeCarriers.
func pgRefusedKind(records string) error {
	return storeFailure(faultRefused, "scope", fmt.Errorf("no record of kind %q carries a scope", records))
}

// pgSubject is the table and id a brief's subject names (job:<id>,
// run:<id>), or "" for another subject.
func pgSubject(subject string) (table, id string) {
	kind, id, _ := strings.Cut(subject, ":")
	switch kind {
	case "job":
		return "jobs", id
	case "run":
		return "runs", id
	}
	return "", ""
}

// leasesClaims is yes: a claim holds for its lease by the database's clock,
// renewed by its holder (D3).
func (k *pgStore) leasesClaims() bool { return true }

// holdsScopes is yes: each view's transactions and reads are held to its
// scope by the row policy (section 6).
func (k *pgStore) holdsScopes() bool { return true }
