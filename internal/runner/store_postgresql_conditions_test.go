package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The conditions of docs/design/store-postgresql.md, section 10, on a server
// named to the test (JPACK_TEST_POSTGRESQL), as the roles an operator creates
// with an administrator that is not a superuser: each is skipped, and says
// so, when none is named.

// pgConditionWorld is a database of the test's own holding heldWorld's
// records (a job, a run and an entry in each of the scopes a and b and none),
// and the kind opened on it on the real run-time logins.
func pgConditionWorld(t *testing.T) (*pgTestServer, string, heldWorld, *pgStore) {
	t.Helper()
	s := pgServer(t)
	db := s.database(t)
	suite := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleSuite, db))
	must(t, suite.start(pgTestOpening))
	k := pgKindsUnderTest()[0]
	k.open = func(*testing.T) runStore { return suite }
	w := newHeldWorld(t, k)
	real := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleWorker, db))
	must(t, real.start(pgTestOpening))
	return s, db, w, real
}

// pgCode is the SQLSTATE of err, "" for none.
func pgCode(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// pgScopedTables are the kind's tables whose rows carry a scope.
func pgScopedTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(pgContext, "SELECT c.relname FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind='r' AND a.attname='scope' AND NOT a.attisdropped ORDER BY 1", postgresqlSchema)
	must(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		names = append(names, name)
	}
	if len(names) < 10 {
		t.Fatal("the kind holds fewer scoped tables than its schema makes:", names)
	}
	return names
}

// inScope runs fn in a transaction on db, held to scope by the setting the
// kind sets, or setting none for unset, and rolls it back.
func inScope(t *testing.T, db *sql.DB, scope string, unset bool, fn func(*sql.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(pgContext, nil)
	must(t, err)
	defer tx.Rollback()
	if !unset {
		_, err = tx.ExecContext(pgContext, "SELECT set_config('jpack.scope', $1, true)", pgScopeValue(scope))
		must(t, err)
	}
	fn(tx)
}

// f. A request login held to one scope reads and writes no row of another,
// in any table, guessing its key, and no sequence's value; a transaction
// that set no scope reaches no scoped row; a scope set in one transaction is
// not there in the next on the same connection. Every table has row security
// enabled and forced, and the kind defines no view.
func TestPostgreSQLHoldsATransactionToItsScope(t *testing.T) {
	s, db, w, kind := pgConditionWorld(t)
	suite := s.pool(t, pgRoleSuite, db)
	request := s.pool(t, pgRoleRequest, db)
	request.SetMaxOpenConns(1)

	// The kind sets the scope for its transaction alone: after a read and a
	// transaction held to a, the same pooled connection reaches nothing.
	kind.p.request.SetMaxOpenConns(1)
	held := kind.heldTo(scopeHeld("a"))
	if _, err := held.run(w.runs["a"].ID); err != nil {
		t.Fatal(err)
	}
	must(t, held.update(func(tx runTx) error {
		_, err := tx.run(w.runs["a"].ID)
		return err
	}))
	var after int
	must(t, kind.p.request.QueryRowContext(pgContext, "SELECT count(*) FROM runs").Scan(&after))
	if after != 0 {
		t.Fatal("the kind's scope outlived its transaction on the pooled connection:", after)
	}

	var unforced []string
	rows, err := suite.QueryContext(pgContext, "SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p') AND NOT (c.relrowsecurity AND c.relforcerowsecurity)", postgresqlSchema)
	must(t, err)
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		unforced = append(unforced, name)
	}
	rows.Close()
	if len(unforced) != 0 {
		t.Fatal("tables without row security enabled and forced:", unforced)
	}
	var views int
	must(t, suite.QueryRowContext(pgContext, "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('v','m')", postgresqlSchema).Scan(&views))
	if views != 0 {
		t.Fatal("the kind defines a view:", views)
	}

	for _, table := range pgScopedTables(t, suite) {
		// A table the request login may not read at all (trigger_revisions,
		// which it only appends to) reaches no row of any scope.
		var readable bool
		must(t, request.QueryRowContext(pgContext, "SELECT has_table_privilege(current_user, $1, 'SELECT')", postgresqlSchema+"."+table).Scan(&readable))
		if !readable {
			continue
		}
		for _, scope := range []string{"a", "b"} {
			var want, got, other int
			must(t, suite.QueryRowContext(pgContext, "SELECT count(*) FROM "+table+" WHERE scope=$1", scope).Scan(&want))
			inScope(t, request, scope, false, func(tx *sql.Tx) {
				must(t, tx.QueryRowContext(pgContext, "SELECT count(*) FROM "+table).Scan(&got))
				must(t, tx.QueryRowContext(pgContext, "SELECT count(*) FROM "+table+" WHERE scope<>$1", scope).Scan(&other))
			})
			// The journal also shows a request the none scope's entries that
			// name no job: the Runner's own (section 6).
			if table == "events" {
				var shared int
				must(t, suite.QueryRowContext(pgContext, "SELECT count(*) FROM events WHERE scope='' AND job_id=''").Scan(&shared))
				want += shared
				other -= shared
			}
			if got != want || other != 0 {
				t.Errorf("%s held to %s: %d rows, %d of another scope; want %d, none", table, scope, got, other, want)
			}
		}
		var unsetRows int
		inScope(t, request, "", true, func(tx *sql.Tx) {
			must(t, tx.QueryRowContext(pgContext, "SELECT count(*) FROM "+table).Scan(&unsetRows))
		})
		if unsetRows != 0 {
			t.Errorf("%s: a transaction that set no scope reached %d rows", table, unsetRows)
		}
	}

	// Guessing the key of another scope's record: a read finds nothing, a
	// change changes nothing, and a row written into it is refused.
	b := w.runs["b"]
	inScope(t, request, "a", false, func(tx *sql.Tx) {
		var n int
		must(t, tx.QueryRowContext(pgContext, "SELECT count(*) FROM runs WHERE id=$1", b.ID).Scan(&n))
		res, err := tx.ExecContext(pgContext, "UPDATE runs SET record=record WHERE id=$1", b.ID)
		must(t, err)
		changed, _ := res.RowsAffected()
		if n != 0 || changed != 0 {
			t.Errorf("a request held to a read %d and changed %d of b's run", n, changed)
		}
	})
	inScope(t, request, "a", false, func(tx *sql.Tx) {
		r := b
		r.ID = "run_written_into_b"
		_, err := tx.ExecContext(pgContext, "INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES ($1,$2,'owner','k','h','queued',$3)", r.ID, r.JobID, string(encode(r)))
		if pgCode(err) != "42501" {
			t.Errorf("a request held to a wrote a row of b: %v", err)
		}
	})
	// The setting is the transaction's: the next transaction on the same
	// connection reaches nothing.
	inScope(t, request, "a", false, func(*sql.Tx) {})
	var leaked int
	inScope(t, request, "", true, func(tx *sql.Tx) {
		must(t, tx.QueryRowContext(pgContext, "SELECT count(*) FROM runs").Scan(&leaked))
	})
	var outside int
	must(t, request.QueryRowContext(pgContext, "SELECT count(*) FROM runs").Scan(&outside))
	if leaked != 0 || outside != 0 {
		t.Errorf("a scope set in one transaction reached %d rows in the next, %d outside one", leaked, outside)
	}
	// No sequence's value.
	rows, err = suite.QueryContext(pgContext, "SELECT n.nspname||'.'||c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind='S'", postgresqlSchema)
	must(t, err)
	var sequences []string
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		sequences = append(sequences, name)
	}
	rows.Close()
	if len(sequences) == 0 {
		t.Fatal("the kind holds no sequence to read")
	}
	for _, login := range []string{pgRoleRequest, pgRoleWorker} {
		p := s.pool(t, login, db)
		for _, seq := range sequences {
			for _, read := range []string{"SELECT last_value FROM " + seq, "SELECT nextval('" + seq + "')", "SELECT currval('" + seq + "')"} {
				if _, err := p.ExecContext(pgContext, read); pgCode(err) != "42501" && pgCode(err) != "55000" {
					t.Errorf("%s: %s: %v", login, read, err)
				}
			}
		}
	}
}

// g. Neither run-time login can delete, truncate, drop or alter, or make a
// table anywhere; the append-only tables refuse an update.
func TestPostgreSQLRuntimeLoginsCannotRemove(t *testing.T) {
	s, db, w, _ := pgConditionWorld(t)
	for _, login := range []string{pgRoleRequest, pgRoleWorker} {
		p := s.pool(t, login, db)
		p.SetMaxOpenConns(1)
		for _, statement := range []string{
			"DELETE FROM runs",
			"DELETE FROM events",
			"TRUNCATE runs",
			"TRUNCATE events",
			"DROP TABLE runs",
			"ALTER TABLE runs ADD COLUMN extra int",
			"ALTER TABLE runs DISABLE ROW LEVEL SECURITY",
			"DROP POLICY request_scope ON runs",
			"CREATE TABLE jpack_runner.extra(a int)",
			"CREATE TABLE public.extra(a int)",
			"CREATE SCHEMA extra",
			"UPDATE events SET kind=kind",
			"UPDATE releases SET record=record",
			"UPDATE run_chain SET line=line",
			"UPDATE job_revisions SET record=record",
			"UPDATE job_creations SET record=record",
		} {
			inScope(t, p, w.jobs["a"].Scope, false, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(pgContext, statement); err == nil {
					t.Errorf("%s ran %s", login, statement)
				}
			})
		}
	}
}

// b. The owner cannot log in; each run-time login holds none of the
// attributes that widen it; a start with a run-time login that is the
// owner's member, or the worker login as the request login, is refused.
func TestPostgreSQLRolesHoldOnlyWhatTheyNeed(t *testing.T) {
	s, db, _, _ := pgConditionWorld(t)
	var login bool
	must(t, s.admin.QueryRowContext(pgContext, "SELECT rolcanlogin FROM pg_roles WHERE rolname=$1", s.role(pgRoleOwner)).Scan(&login))
	if login {
		t.Fatal("the owner can log in")
	}
	for _, name := range []string{pgRoleRequest, pgRoleWorker} {
		var wide bool
		must(t, s.admin.QueryRowContext(pgContext, "SELECT rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR rolreplication FROM pg_roles WHERE rolname=$1", s.role(name)).Scan(&wide))
		if wide {
			t.Fatal(name, "holds an attribute that widens it")
		}
	}
	// The migration login reaches the owner only by SET ROLE.
	var inherits bool
	must(t, s.admin.QueryRowContext(pgContext, "SELECT pg_has_role($1, $2, 'USAGE')", s.role(pgRoleMigration), s.role(pgRoleOwner)).Scan(&inherits))
	if inherits {
		t.Fatal("the migration login inherits the owner's privileges")
	}
	// A wider login is refused at the start.
	member := s.prefix + "_member"
	s.password["member"] = pgTestSecret()
	pgExec(t, s.admin, "CREATE ROLE "+member+" LOGIN PASSWORD '"+s.password["member"]+"'")
	t.Cleanup(func() { s.admin.ExecContext(pgContext, "DROP ROLE IF EXISTS "+member) })
	pgExec(t, s.admin, "GRANT "+s.role(pgRoleOwner)+" TO "+member)
	for _, c := range []struct {
		name            string
		request, worker string
	}{
		{"a request login that is the owner's member", "member", pgRoleWorker},
		{"a worker login that is the owner's member", pgRoleRequest, "member"},
		{"the worker login as the request login", pgRoleWorker, pgRoleWorker},
	} {
		k := newPostgreSQLStore(s.pool(t, c.request, db), s.pool(t, c.worker, db))
		if err := k.checkLogins(s.role(pgRoleOwner)); !errors.Is(err, errLoginTooWide) {
			t.Errorf("%s: %v, want errLoginTooWide", c.name, err)
		}
	}
	k := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleWorker, db))
	must(t, k.checkLogins(s.role(pgRoleOwner)))
}

// pgFreshDatabase is an empty database of the test's own, in which the owner
// may make its schema.
func pgFreshDatabase(t *testing.T, s *pgTestServer) string {
	t.Helper()
	name := s.database(t)
	pgExec(t, s.admin, "GRANT CREATE ON DATABASE "+name+" TO "+s.role(pgRoleOwner))
	// The clone holds the template's schema: drop it for an empty one.
	must(t, s.asOwner(name, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(pgContext, "DROP SCHEMA jpack_runner CASCADE")
		return err
	}))
	return name
}

// pgCatalog is the kind's objects, policies and grants, as text.
func pgCatalog(t *testing.T, s *pgTestServer, db string) string {
	t.Helper()
	p := s.pool(t, pgRoleSuite, db)
	var catalog string
	must(t, p.QueryRowContext(pgContext, `SELECT coalesce(string_agg(x, E'\n' ORDER BY x), '') FROM (
SELECT c.relname||' '||c.relkind::text||' '||coalesce(c.relacl::text,'')||' '||c.relrowsecurity||c.relforcerowsecurity AS x FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
UNION ALL SELECT p.polname||' '||p.polrelid::regclass::text||' '||p.polroles::text||' '||coalesce(pg_get_expr(p.polqual,p.polrelid),'')||' '||coalesce(pg_get_expr(p.polwithcheck,p.polrelid),'') FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
UNION ALL SELECT version||' '||digest FROM jpack_runner.schema_migrations) q`, postgresqlSchema).Scan(&catalog))
	return catalog
}

// c, a and d. The migration applies its set once: a second run applies
// nothing and leaves the catalog as it was; a file recorded under another
// digest is refused, and so is a run naming other logins. A database holding
// another table is refused before anything is made. It makes no role and
// stores no setting.
func TestPostgreSQLMigrationIsIdempotent(t *testing.T) {
	s := pgServer(t)
	db := pgFreshDatabase(t, s)
	migration := PostgreSQLMigration{Connection: s.url(pgRoleMigration, db), Owner: s.role(pgRoleOwner), Request: s.role(pgRoleRequest), Worker: s.role(pgRoleWorker)}
	count := func(query string) int {
		var n int
		must(t, s.admin.QueryRowContext(pgContext, query).Scan(&n))
		return n
	}
	roles, settings := count("SELECT count(*) FROM pg_roles"), count("SELECT count(*) FROM pg_db_role_setting")
	applied, err := MigratePostgreSQL(migration)
	if err != nil || !slices.Equal(applied, []string{"0001", "0002"}) {
		t.Fatal("the first run did not apply the set:", applied, err)
	}
	if count("SELECT count(*) FROM pg_roles") != roles || count("SELECT count(*) FROM pg_db_role_setting") != settings {
		t.Fatal("the migration made a role or stored a setting")
	}
	// The suite's login reads the catalog through grants of its own.
	must(t, s.asOwner(db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(pgContext, "GRANT USAGE ON SCHEMA jpack_runner TO "+s.role(pgRoleSuite)+"; GRANT SELECT ON jpack_runner.schema_migrations TO "+s.role(pgRoleSuite)+"; CREATE POLICY suite_rows ON jpack_runner.schema_migrations TO "+s.role(pgRoleSuite)+" USING (true)")
		return err
	}))
	before := pgCatalog(t, s, db)
	if applied, err = MigratePostgreSQL(migration); err != nil || len(applied) != 0 {
		t.Fatal("a second run applied", applied, err)
	}
	if after := pgCatalog(t, s, db); after != before {
		t.Fatalf("a second run changed the catalog:\n%s\n---\n%s", before, after)
	}
	other := migration
	other.Request = s.role(pgRoleSuite)
	if _, err = MigratePostgreSQL(other); err == nil || !strings.Contains(err.Error(), "replacing a login is not a migration") {
		t.Fatal("a run naming another login was not refused:", err)
	}
	must(t, s.asOwner(db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(pgContext, "UPDATE jpack_runner.schema_migrations SET digest='0' WHERE version='0001'")
		return err
	}))
	if _, err = MigratePostgreSQL(migration); err == nil || !strings.Contains(err.Error(), "recorded under another digest") {
		t.Fatal("a file recorded under another digest was not refused:", err)
	}

	// a. A database holding another table is refused before anything is
	// made.
	foreign := pgFreshDatabase(t, s)
	must(t, s.asOwner(foreign, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(pgContext, "CREATE SCHEMA elsewhere; CREATE TABLE elsewhere.held(a int)")
		return err
	}))
	migration.Connection = s.url(pgRoleMigration, foreign)
	if _, err = MigratePostgreSQL(migration); !errors.Is(err, errForeignTable) {
		t.Fatal("a database holding another table was not refused:", err)
	}
	var made bool
	must(t, s.pool(t, pgRoleSuite, foreign).QueryRowContext(pgContext, "SELECT to_regnamespace('jpack_runner') IS NOT NULL").Scan(&made))
	if made {
		t.Fatal("the refused migration made the kind's schema")
	}
}

// d, e and i, read from the files the Runner ships: the set matches
// migrations.sha256; no file makes a role, a user, a stored setting or a
// definer function.
func TestPostgreSQLMigrationSetMatchesItsDigest(t *testing.T) {
	set, err := postgresqlMigrations()
	if err != nil {
		t.Fatal(err)
	}
	names, err := filepath.Glob("postgresql/migrations/*.sql")
	must(t, err)
	if len(set) != len(names) || len(set) < 2 {
		t.Fatal("the set shipped is not every file:", len(set), names)
	}
	forbidden := regexp.MustCompile(`(?i)create\s+(role|user)\b|alter\s+(role|user|database)\b[^;]*\bset\b|security\s+definer|\bset\s+role\b`)
	for i, m := range set {
		b, err := os.ReadFile(names[i])
		must(t, err)
		if string(b) != m.text {
			t.Fatal(names[i], "is not the file shipped")
		}
		if found := forbidden.FindString(m.text); found != "" {
			t.Errorf("%s holds %q", m.name, found)
		}
	}
}

// e. The schema holds no definer function.
func TestPostgreSQLDefinesNoSecurityDefinerFunction(t *testing.T) {
	s, db, _, _ := pgConditionWorld(t)
	var n int
	must(t, s.pool(t, pgRoleSuite, db).QueryRowContext(pgContext, "SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1 AND p.prosecdef", postgresqlSchema).Scan(&n))
	if n != 0 {
		t.Fatal("the schema holds a definer function:", n)
	}
}

// D3. Two dispatchers on the worker login, each its own pool: one claims the
// run, the other meets conflict and writes nothing. Once the lease runs out
// by the database's clock, the run is interrupted and claimed again for its
// next attempt, and the first claimant's finish and renewal write nothing.
func TestPostgreSQLFencesEveryClaimantWriteByItsToken(t *testing.T) {
	s, db, w, first := pgConditionWorld(t)
	second := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleWorker, db))
	must(t, second.start(pgTestOpening))
	queued := w.runs["a"]
	byFirst, ok, err := claimRun(first, queued.ID)
	if err != nil || !ok {
		t.Fatal("the first dispatcher did not claim the run:", ok, err)
	}
	if _, ok, err = claimRun(second, queued.ID); ok || faultOf(err) != faultConflict {
		t.Fatal("the second dispatcher claimed a claimed run:", ok, err)
	}
	must(t, first.update(func(tx runTx) error { return tx.renewClaim(queued.ID, byFirst.claim) }))
	// The second interrupts nothing while the lease is live.
	sv := &Service{store: second}
	must(t, sv.interruptLapsed())
	if got, _ := second.run(queued.ID); got.State != "running" {
		t.Fatal("a run under a live lease was interrupted:", got.State)
	}
	pgExec(t, s.pool(t, pgRoleSuite, db), "UPDATE runs SET lease_until=now()-interval '1 second' WHERE id=$1", queued.ID)
	must(t, sv.interruptLapsed())
	if got, _ := second.run(queued.ID); got.State != "interrupted" {
		t.Fatal("a run whose lease ran out was not interrupted:", got.State)
	}
	wantFault(t, finishRunIn(first, completed(byFirst)), faultConflict, "the first claimant's finish after its lease ran out")
	wantFault(t, first.update(func(tx runTx) error { return tx.renewClaim(queued.ID, byFirst.claim) }), faultConflict, "the first claimant's renewal after its lease ran out")
	if got, _ := second.run(queued.ID); got.State != "interrupted" {
		t.Fatal("the first claimant wrote its run after losing it:", got.State)
	}
	// The kind compares a lease only with the database's clock: no
	// statement of its files is given the process's time.
	for _, name := range []string{"store_postgresql.go", "store_postgresql_writes.go", "store_postgresql_reads.go"} {
		b, err := os.ReadFile(name)
		must(t, err)
		if strings.Contains(string(b), "time.Now") {
			t.Errorf("%s reads the process's clock", name)
		}
		for _, use := range regexp.MustCompile(`lease_until\s*(<=|>|<|>=)\s*[^ ),]+`).FindAllString(string(b), -1) {
			if !strings.HasSuffix(use, "now()") && !strings.HasSuffix(use, "now(") {
				t.Errorf("%s compares a lease with something else than now(): %s", name, use)
			}
		}
	}
}

// claimRun claims the queued run id on st as the dispatcher claims it: in a
// transaction that expects it queued.
func claimRun(st runStore, id string) (Run, bool, error) {
	r, err := st.run(id)
	if err != nil {
		return Run{}, false, err
	}
	r.State, r.StartedAt = "running", now()
	r.Attempt++
	if r.claim, err = newRunClaim(r.Attempt); err != nil {
		return Run{}, false, err
	}
	if err = changeQueuedRun(st, r); err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// Section 6, end to end: a Runner opened on the kind (Config.PostgreSQL)
// creates a job and a run through its held routes on the request login, its
// dispatcher claims, renews and finishes the run on the worker login, and a
// route not held is refused.
func TestPostgreSQLServesARunThroughItsHeldRoutes(t *testing.T) {
	s := pgServer(t)
	db := s.database(t)
	cfg := journalConfig(t)
	connections := s.connections(db)
	cfg.PostgreSQL = &connections
	shortClaimTimes(t, 30*time.Millisecond, 2*time.Second)
	sv := openJournal(t, cfg)
	defer sv.Close()
	if !leases(sv.store) || !holdsScopes(sv.store) {
		t.Fatal("the kind opened is not the postgresql kind")
	}
	gate := filepath.Join(t.TempDir(), "gate")
	release := journalRelease(t, sv, gatedRuntime(t, cfg.Dir, gate))
	got := serve(t, sv, "POST", "/v1/jobs", `{"name":"Held","releaseId":"`+release.ID+`","reviewed":true,"scope":"team-a"}`, nil)
	if got.Code != 201 {
		t.Fatal(got.Code, got.Body.String())
	}
	var j Job
	must(t, json.Unmarshal(got.Body.Bytes(), &j))
	got = serve(t, sv, "POST", "/v1/jobs/"+j.ID+"/runs?scope=team-a", string(encode(journalInput())), map[string]string{"Idempotency-Key": "one"})
	if got.Code != 202 {
		t.Fatal(got.Code, got.Body.String())
	}
	var queued struct {
		ID string `json:"id"`
	}
	must(t, json.Unmarshal(got.Body.Bytes(), &queued))
	// Another scope does not reach the job.
	if other := serve(t, sv, "GET", "/v1/jobs/"+j.ID+"?scope=team-b", "", nil); other.Code != 404 {
		t.Fatal("another scope reached the job:", other.Code, other.Body.String())
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(gate, nil, 0o600)
	}()
	if ran, err := sv.dispatchNext(context.Background()); !ran || err != nil {
		t.Fatal("the run was not dispatched:", ran, err)
	}
	// The dispatcher renewed its claim on the worker login while the
	// Runtime ran: the lease the finish kept runs from a renewal after the
	// claim, by the database's clock (the claim's own ran 60 seconds from
	// it).
	var state string
	var renewed bool
	must(t, s.pool(t, pgRoleSuite, db).QueryRowContext(pgContext, "SELECT state, claim_id IS NOT NULL AND lease_until > (record::json->>'startedAt')::timestamptz + interval '60.1 seconds' FROM runs WHERE id=$1", queued.ID).Scan(&state, &renewed))
	if state != "completed" || !renewed {
		t.Fatal("the run was not renewed and finished under its claim:", state, renewed)
	}
	got = serve(t, sv, "GET", "/v1/runs/"+queued.ID+"?scope=team-a", "", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"state":"completed"`) {
		t.Fatal(got.Code, got.Body.String())
	}
	if refused := serve(t, sv, "GET", "/v1/events", "", nil); refused.Code != 501 || !strings.Contains(refused.Body.String(), "store_route_unsupported") {
		t.Fatal("a route not held was served:", refused.Code, refused.Body.String())
	}
}
