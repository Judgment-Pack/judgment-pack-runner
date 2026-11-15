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
			// name no job: the Runner's own and previews, not refusals
			// (section 6).
			if table == "events" {
				var shared int
				must(t, suite.QueryRowContext(pgContext, "SELECT count(*) FROM events WHERE scope='' AND job_id='' AND kind<>'admission.refused'").Scan(&shared))
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

// Section 7: each run-time login holds on each of the store's tables the
// privileges the section's table names and no other, so the work for every
// scope, on the worker login, holds no more than the ticks, the dispatch and
// preparation write; a table the section does not name is held by neither.
func TestPostgreSQLLoginsHoldOnlyTheirTablesPrivileges(t *testing.T) {
	s, db, _, _ := pgConditionWorld(t)
	held := map[string]map[string]string{
		pgRoleRequest: {
			"releases": "SELECT INSERT", "metadata": "SELECT INSERT UPDATE",
			"jobs": "SELECT INSERT UPDATE", "triggers": "SELECT INSERT UPDATE", "occurrences": "SELECT INSERT UPDATE",
			"runs": "SELECT INSERT UPDATE", "briefs": "SELECT INSERT UPDATE",
			"job_creations": "SELECT INSERT", "job_revisions": "SELECT INSERT", "trigger_revisions": "INSERT",
			"occurrence_keys": "SELECT INSERT", "events": "SELECT INSERT", "run_chain": "SELECT",
			"journal_head": "SELECT UPDATE", "counts": "SELECT UPDATE",
			"pack_identities": "SELECT INSERT", "dialect_identities": "SELECT INSERT", "schema_migrations": "SELECT",
		},
		pgRoleWorker: {
			"releases": "SELECT", "metadata": "SELECT INSERT",
			"jobs": "SELECT", "triggers": "SELECT UPDATE", "occurrences": "SELECT INSERT UPDATE",
			"runs": "SELECT INSERT UPDATE", "job_creations": "SELECT", "job_revisions": "SELECT",
			"events": "SELECT INSERT", "run_chain": "SELECT INSERT", "journal_head": "SELECT UPDATE",
			"chain_head": "SELECT UPDATE", "counts": "SELECT UPDATE",
			"pack_identities": "SELECT", "dialect_identities": "SELECT", "schema_migrations": "SELECT",
		},
	}
	suite := s.pool(t, pgRoleSuite, db)
	rows, err := suite.QueryContext(pgContext, "SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p') ORDER BY c.relname", postgresqlSchema)
	must(t, err)
	var tables []string
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	must(t, rows.Err())
	rows.Close()
	if len(tables) == 0 {
		t.Fatal("the store holds no table")
	}
	for login, want := range held {
		for _, table := range tables {
			var privileges []string
			for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				var has bool
				must(t, suite.QueryRowContext(pgContext, "SELECT has_table_privilege($1, $2, $3)", s.role(login), postgresqlSchema+"."+table, privilege).Scan(&has))
				if has {
					privileges = append(privileges, privilege)
				}
			}
			if got := strings.Join(privileges, " "); got != want[table] {
				t.Errorf("the %s login holds %q on %s, want %q", login, got, table, want[table])
			}
		}
	}
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
	set, err := postgresqlMigrations()
	must(t, err)
	var shipped []string
	for _, f := range set {
		shipped = append(shipped, f.version)
	}
	applied, err := MigratePostgreSQL(migration)
	if err != nil || !slices.Equal(applied, shipped) {
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

// c and d. A store an earlier release left, migrated to the grants file
// (0002), is refused at the start until it is migrated; the migration then
// applies the files after it and nothing else, leaves the catalog a store
// migrated at once has, and applies nothing on a second run. A run naming
// other logins than the grants file recorded applies nothing.
func TestPostgreSQLMigratesAStoreAtTheGrantsForward(t *testing.T) {
	s := pgServer(t)
	set, err := postgresqlMigrations()
	must(t, err)
	grants := slices.IndexFunc(set, func(f pgMigration) bool { return f.version == pgGrantsVersion })
	if grants < 0 || grants == len(set)-1 {
		t.Fatal("the set holds no file after the grants file:", len(set))
	}
	var later []string
	for _, f := range set[grants+1:] {
		later = append(later, f.version)
	}
	suite := func(db string) {
		must(t, s.asOwner(db, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(pgContext, "GRANT USAGE ON SCHEMA jpack_runner TO "+s.role(pgRoleSuite)+"; GRANT SELECT ON jpack_runner.schema_migrations TO "+s.role(pgRoleSuite)+"; CREATE POLICY suite_rows ON jpack_runner.schema_migrations TO "+s.role(pgRoleSuite)+" USING (true)")
			return err
		}))
	}
	at := func(db string) PostgreSQLMigration {
		return PostgreSQLMigration{Connection: s.url(pgRoleMigration, db), Owner: s.role(pgRoleOwner), Request: s.role(pgRoleRequest), Worker: s.role(pgRoleWorker)}
	}
	once := pgFreshDatabase(t, s)
	if _, err = MigratePostgreSQL(at(once)); err != nil {
		t.Fatal(err)
	}
	suite(once)
	db := pgFreshDatabase(t, s)
	migration := at(db)
	if applied, err := migratePostgreSQL(migration, set[:grants+1]); err != nil || len(applied) != grants+1 {
		t.Fatal("the store an earlier release left was not made:", applied, err)
	}
	k := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleWorker, db))
	if err := k.checkMigrations(); err == nil || !strings.Contains(err.Error(), "run jpack-runner store migrate") {
		t.Fatal("a store an earlier release left was not refused at the start:", err)
	}
	other := migration
	other.Worker = s.role(pgRoleSuite)
	if applied, err := MigratePostgreSQL(other); err == nil || len(applied) != 0 || !strings.Contains(err.Error(), "replacing a login is not a migration") {
		t.Fatal("a run naming another login was not refused before the later files:", applied, err)
	}
	applied, err := MigratePostgreSQL(migration)
	if err != nil || !slices.Equal(applied, later) {
		t.Fatal("the migration did not apply the files after the grants file:", applied, err)
	}
	must(t, k.checkMigrations())
	suite(db)
	if forward, whole := pgCatalog(t, s, db), pgCatalog(t, s, once); forward != whole {
		t.Fatalf("the store migrated forward is not the store migrated at once:\n%s\n---\n%s", forward, whole)
	}
	before := pgCatalog(t, s, db)
	if applied, err = MigratePostgreSQL(migration); err != nil || len(applied) != 0 {
		t.Fatal("a second run applied", applied, err)
	}
	if after := pgCatalog(t, s, db); after != before {
		t.Fatalf("a second run changed the catalog:\n%s\n---\n%s", before, after)
	}
	var request, worker string
	must(t, s.pool(t, pgRoleSuite, db).QueryRowContext(pgContext, "SELECT request_login, worker_login FROM jpack_runner.schema_migrations WHERE version=$1", later[0]).Scan(&request, &worker))
	if request != s.role(pgRoleRequest) || worker != s.role(pgRoleWorker) {
		t.Fatal("the later file's row does not record the logins:", request, worker)
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
	// statement of its files is given the process's time, and a claimant's
	// fence (a live lease: lease_until > X, or X < lease_until) is not the
	// transaction's start, now(), which is before its wait for journal_head's
	// lock. The check first finds what it is to find.
	for src, faulty := range map[string]bool{
		"lease_until>statement_timestamp()":   false,
		"lease_until > clock_timestamp()":     false,
		"lease_until<=now()":                  false,
		"statement_timestamp() < lease_until": false,
		"now() >= lease_until":                false,
		"lease_until>now()":                   true,
		"lease_until >= now()":                true,
		"now() < lease_until":                 true,
		"now()<=lease_until":                  true,
		"lease_until > $3":                    true,
		"$3 < lease_until":                    true,
	} {
		if got := pgLeaseFaults(src); len(got) > 0 != faulty {
			t.Errorf("the lease check of %q finds %v", src, got)
		}
	}
	names, err := filepath.Glob("store_postgresql*.go")
	must(t, err)
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		must(t, err)
		if strings.Contains(string(b), "time.Now") {
			t.Errorf("%s reads the process's clock", name)
		}
		for _, fault := range pgLeaseFaults(string(b)) {
			t.Errorf("%s %s", name, fault)
		}
	}
}

// pgLeaseFaults are the comparisons of a lease in src, lease_until <op> X
// or X <op> lease_until, that are not with the database's clock, or that
// fence a claimant's write (a lease still live) by the transaction's start.
func pgLeaseFaults(src string) []string {
	var faults []string
	check := func(use, op, clock string) {
		switch strings.TrimRight(clock, "()") {
		case "statement_timestamp", "clock_timestamp":
		case "now":
			if op == ">" || op == ">=" {
				faults = append(faults, "fences a claimant's write by the transaction's start: "+use)
			}
		default:
			faults = append(faults, "compares a lease with something else than the database's clock: "+use)
		}
	}
	for _, m := range regexp.MustCompile(`lease_until\s*(<=|>=|<|>)\s*([^\s),]+)`).FindAllStringSubmatch(src, -1) {
		check(m[0], m[1], m[2])
	}
	// X <op> lease_until is lease_until <op'> X, the operator turned round.
	turned := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<="}
	for _, m := range regexp.MustCompile(`([A-Za-z_$][\w$.]*(?:\(\))?)\s*(<=|>=|<|>)\s*lease_until`).FindAllStringSubmatch(src, -1) {
		check(m[0], turned[m[2]], m[1])
	}
	return faults
}

// A claimant's write that waits for journal_head's lock past its lease's
// end writes nothing: the fence reads the database's clock when its
// statement starts, after the wait, not when its transaction began. A
// finish and a renewal alike.
func TestPostgreSQLFencesAClaimantWriteThatWaitedPastItsLease(t *testing.T) {
	s, db, w, real := pgConditionWorld(t)
	suite := s.pool(t, pgRoleSuite, db)
	r, ok, err := claimRun(real, w.runs["a"].ID)
	if err != nil || !ok {
		t.Fatal("the run was not claimed:", ok, err)
	}
	for _, write := range []struct {
		what string
		do   func() error
	}{
		{"a finish", func() error { return finishRunIn(real, completed(r)) }},
		{"a renewal", func() error { return real.update(func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }) }},
	} {
		pgExec(t, suite, "UPDATE runs SET lease_until=clock_timestamp()+interval '1 second' WHERE id=$1", r.ID)
		end := pgHoldJournal(t, s, db)
		done := make(chan error, 1)
		go func() { done <- write.do() }()
		waitWaiting(t, s, db, pgRoleWorker, 1, write.what+" behind a held lock")
		var live bool
		for {
			if err := suite.QueryRowContext(pgContext, "SELECT lease_until>clock_timestamp() FROM runs WHERE id=$1", r.ID).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if !live {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		end()
		wantFault(t, <-done, faultConflict, write.what+" that waited past its lease's end")
		if got, _ := real.run(r.ID); got.State != "running" {
			t.Fatalf("%s that waited past its lease's end wrote the run: %s", write.what, got.State)
		}
	}
}

// The kind's search path puts the system's catalog before its schema: a
// function made in the schema with a built-in's name and arguments, even by
// the owner, whom the start's checks pass, hides no built-in from the kind's
// unqualified statements. A claim, a renewal and a finish read the
// database's clock, and a read held to a scope sets it, as before; and no
// relation of the schema has a name the catalog holds, so that every
// unqualified table is still found in the schema.
func TestPostgreSQLResolvesABuiltinBeforeTheSchema(t *testing.T) {
	s, db, w, real := pgConditionWorld(t)
	var shadows []string
	for _, f := range []string{"now() RETURNS timestamptz", "statement_timestamp() RETURNS timestamptz", "set_config(text, text, boolean) RETURNS text"} {
		shadows = append(shadows, "CREATE FUNCTION jpack_runner."+f+" LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'a function of the schema hid a built-in'; END$$")
	}
	pgOwnerDoes(t, s, db, shadows...)
	r, ok, err := claimRun(real, w.runs["a"].ID)
	if err != nil || !ok {
		t.Fatal("a claim called a function of the schema:", ok, err)
	}
	must(t, real.update(func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }))
	must(t, finishRunIn(real, completed(r)))
	if got, err := real.heldTo(scopeHeld("a")).run(r.ID); err != nil || got.State != "completed" {
		t.Fatal("a read held to a scope called a function of the schema:", got.State, err)
	}
	var shared string
	must(t, s.pool(t, pgRoleSuite, db).QueryRowContext(pgContext, `SELECT coalesce(string_agg(c.relname, ', '), '') FROM pg_class c
WHERE c.relnamespace='jpack_runner'::regnamespace AND EXISTS (SELECT 1 FROM pg_class k WHERE k.relnamespace='pg_catalog'::regnamespace AND k.relname=c.relname)`).Scan(&shared))
	if shared != "" {
		t.Fatal("a relation of the schema has a name the catalog holds:", shared)
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
	// A renewal that takes longer than its interval loses the claim
	// (claims.go), so the interval is one a renewal under the race detector
	// on a busy machine stays within, and the Runtime runs for several.
	shortClaimTimes(t, 300*time.Millisecond, 5*time.Second)
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
		time.Sleep(time.Second)
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
	if refused := serve(t, sv, "GET", "/v1/runs/"+queued.ID+"/diagnostics?scope=team-a", "", nil); refused.Code != 501 || !strings.Contains(refused.Body.String(), "store_route_unsupported") {
		t.Fatal("a route not held was served:", refused.Code, refused.Body.String())
	}
}

// Section 6, the journal: on a Runner opened on the kind, a scope's job's
// journal is read held to its scope, and another scope neither reads it nor
// learns it is there; a listing naming a scope holds that scope's entries
// alone; a refused creation, journaled under the none scope, shows in no
// scope's journal but the none scope's and the listing naming none.
func TestPostgreSQLHoldsTheJournalToItsScope(t *testing.T) {
	_, _, sv, jobs := pgScopedRunner(t)
	a, b := jobs["team-a"], jobs["team-b"]
	// A creation from team-a's release, naming team-b, is refused and
	// journaled under the none scope.
	if got := serve(t, sv, "POST", "/v1/jobs", `{"name":"Again","releaseId":"`+a.ReleaseID+`","reviewed":true,"scope":"team-b"}`, nil); got.Code != 409 {
		t.Fatal(got.Code, got.Body.String())
	}
	page := func(path string) (int, []Event) {
		t.Helper()
		got := serve(t, sv, "GET", path, "", nil)
		var p EventPage
		if got.Code == 200 {
			must(t, json.Unmarshal(got.Body.Bytes(), &p))
		}
		return got.Code, p.Items
	}
	refused := func(items []Event) bool {
		for _, e := range items {
			if e.Kind == "admission.refused" {
				return true
			}
		}
		return false
	}
	code, items := page("/v1/jobs/" + a.ID + "/events?scope=team-a")
	if code != 200 || len(items) == 0 || refused(items) {
		t.Fatal("team-a's job journal:", code, items)
	}
	for _, e := range items {
		if e.Concerns.Job != a.ID && e.Concerns.Job != "" {
			t.Fatal("team-a's job journal holds an entry of", e.Concerns.Job)
		}
	}
	for _, path := range []string{"/v1/jobs/" + a.ID + "/events?scope=team-b", "/v1/events?job=" + a.ID + "&scope=team-b", "/v1/events?job=" + a.ID, "/v1/jobs/" + a.ID + "/events"} {
		if code, _ := page(path); code != 404 {
			t.Errorf("GET %s: %d, want 404", path, code)
		}
	}
	code, items = page("/v1/events?scope=team-b")
	if code != 200 || len(items) == 0 || refused(items) {
		t.Fatal("team-b's journal:", code, items)
	}
	for _, e := range items {
		if e.Concerns.Job != b.ID {
			t.Fatal("team-b's journal holds an entry of", e.Concerns.Job, e.Kind)
		}
	}
	if code, items = page("/v1/events"); code != 200 || !refused(items) {
		t.Fatal("the journal naming no scope does not hold the refused creation:", code, items)
	}
	if got := serve(t, sv, "GET", "/v1/run-chain", "", nil); got.Code != 200 {
		t.Fatal("the chain:", got.Code, got.Body.String())
	}
}

// pgScopedRunner is a Runner opened on the kind, on a database of the test's
// own, with no dispatcher or automation (journalConfig), holding a job in
// each of the scopes team-a and team-b, each made through its route.
func pgScopedRunner(t *testing.T) (*pgTestServer, string, *Service, map[string]Job) {
	t.Helper()
	s := pgServer(t)
	db := s.database(t)
	cfg := journalConfig(t)
	connections := s.connections(db)
	cfg.PostgreSQL = &connections
	sv := openJournal(t, cfg)
	t.Cleanup(sv.Close)
	if !holdsScopes(sv.store) {
		t.Fatal("the kind opened is not the postgresql kind")
	}
	runtime := completingRuntime(t, cfg.Dir)
	jobs := map[string]Job{}
	for _, scope := range []string{"team-a", "team-b"} {
		release := journalRelease(t, sv, runtime)
		got := serve(t, sv, "POST", "/v1/jobs", `{"name":"Held","releaseId":"`+release.ID+`","reviewed":true,"scope":"`+scope+`"}`, nil)
		if got.Code != 201 {
			t.Fatal(got.Code, got.Body.String())
		}
		var j Job
		must(t, json.Unmarshal(got.Body.Bytes(), &j))
		jobs[scope] = j
	}
	return s, db, sv, jobs
}

// Section 6 and D9, triggers: on a Runner on the kind, another scope neither
// lists, reads nor changes a scope's trigger, guessing its id through a job
// of its own or naming it in the path; the scope's own requests change it.
// The trigger limit and the cloud binding hold across scopes: the binding's
// index refuses another scope's binding, 422 cloud_binding_used, and the
// installation's count refuses a trigger past the limit, 422 trigger_limit.
func TestPostgreSQLHoldsTriggersToTheirScope(t *testing.T) {
	s, db, sv, jobs := pgScopedRunner(t)
	a, b := jobs["team-a"], jobs["team-b"]
	event := TriggerConfig{Name: "Reviewed records", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	call := func(method, path string, body any, want int) []byte {
		t.Helper()
		raw := ""
		if body != nil {
			raw = string(encode(body))
		}
		got := serve(t, sv, method, path, raw, nil)
		if got.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, got.Code, got.Body.String(), want)
		}
		return got.Body.Bytes()
	}
	var ta Trigger
	must(t, json.Unmarshal(call("POST", "/v1/jobs/"+a.ID+"/triggers?scope=team-a", map[string]any{"config": event}, 200), &ta))
	listed := func(job, scope string) []Trigger {
		t.Helper()
		var page struct {
			Items []Trigger `json:"items"`
		}
		must(t, json.Unmarshal(call("GET", "/v1/jobs/"+job+"/triggers?scope="+scope, nil, 200), &page))
		return page.Items
	}
	if got := listed(b.ID, "team-b"); len(got) != 0 {
		t.Fatal("team-b's job lists another scope's trigger:", got)
	}
	pgHeldFrom(t, s, db, "team-b", "triggers")
	call("GET", "/v1/jobs/"+a.ID+"/triggers?scope=team-b", nil, 404)
	call("GET", "/v1/jobs/"+a.ID+"/triggers", nil, 404)
	// Guessing the id: through team-b's own job, or naming it in the path.
	call("POST", "/v1/jobs/"+b.ID+"/triggers?scope=team-b", map[string]any{"id": ta.ID, "revision": 1, "config": event}, 404)
	call("POST", "/v1/triggers/"+ta.ID+"/state?scope=team-b", map[string]any{"revision": 1, "paused": false, "reviewed": true}, 404)
	call("POST", "/v1/triggers/"+ta.ID+"/rotate-key?scope=team-b", map[string]any{"revision": 1}, 404)
	call("POST", "/v1/triggers/"+ta.ID+"/state", map[string]any{"revision": 1, "paused": false, "reviewed": true}, 404)
	call("POST", "/v1/jobs/"+b.ID+"/triggers/preview?scope=team-a", event, 404)
	if got := listed(a.ID, "team-a"); len(got) != 1 || got[0].ID != ta.ID || got[0].Revision != 1 || !got[0].Paused {
		t.Fatal("another scope's requests changed the trigger:", got)
	}
	// The scope's own requests change it, each journaled in the scope.
	call("POST", "/v1/jobs/"+a.ID+"/triggers/preview?scope=team-a", event, 200)
	call("POST", "/v1/jobs/"+a.ID+"/triggers?scope=team-a", map[string]any{"id": ta.ID, "revision": 1, "config": event}, 200)
	call("POST", "/v1/triggers/"+ta.ID+"/state?scope=team-a", map[string]any{"revision": 2, "paused": false, "reviewed": true}, 200)
	call("POST", "/v1/triggers/"+ta.ID+"/rotate-key?scope=team-a", map[string]any{"revision": 3}, 200)
	var page EventPage
	must(t, json.Unmarshal(call("GET", "/v1/events?scope=team-b", nil, 200), &page))
	for _, e := range page.Items {
		if e.Concerns.Trigger != "" {
			t.Fatal("team-b's journal holds a trigger's entry:", e.Kind, e.Concerns.Trigger)
		}
	}
	must(t, json.Unmarshal(call("GET", "/v1/jobs/"+a.ID+"/events?scope=team-a", nil, 200), &page))
	changes := 0
	for _, e := range page.Items {
		if e.Concerns.Trigger == ta.ID {
			changes++
		}
	}
	if changes != 4 {
		t.Fatal("team-a's journal holds", changes, "of its trigger's four changes")
	}

	// The cloud binding, across scopes: team-a's binding is refused to
	// team-b by the index, and to team-a by the Runner's own read.
	conn := CloudConnection{ID: "google", Subscription: "projects/example-project/subscriptions/desk-inbox", CredentialsFile: "/nonexistent/test-adc.json"}
	sv.cfg.CloudConnections = []CloudConnection{conn}
	cloud := TriggerConfig{Name: "Scheduled", Kind: "cloud", Cloud: &CloudBinding{Connection: conn.ID, Subscription: conn.Subscription, Job: "projects/example-project/locations/us-central1/jobs/daily"}, Input: &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}, Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	call("POST", "/v1/jobs/"+a.ID+"/triggers?scope=team-a", map[string]any{"config": cloud}, 200)
	for _, scope := range []string{"team-b", "team-a"} {
		job := jobs[scope]
		if got := call("POST", "/v1/jobs/"+job.ID+"/triggers?scope="+scope, map[string]any{"config": cloud}, 422); !strings.Contains(string(got), "cloud_binding_used") {
			t.Fatal(scope, "was not refused the binding:", string(got))
		}
	}
	// The trigger limit, across scopes: the installation's count.
	suite := s.pool(t, pgRoleSuite, db)
	var held int
	must(t, suite.QueryRowContext(pgContext, "SELECT value FROM counts WHERE name='triggers'").Scan(&held))
	if held != 2 {
		t.Fatal("the installation's trigger count is", held, "want 2")
	}
	_, err := suite.ExecContext(pgContext, "UPDATE counts SET value=$1 WHERE name='triggers'", defaultTriggerLimit)
	must(t, err)
	if got := call("POST", "/v1/jobs/"+b.ID+"/triggers?scope=team-b", map[string]any{"config": event}, 422); !strings.Contains(string(got), "trigger_limit") {
		t.Fatal("a trigger past the installation's limit was not refused:", string(got))
	}
}

// D9 and #279: on a Runner on the kind, POST /v1/jobs with a cloud trigger
// whose binding a trigger of any scope holds meets the binding's index, and
// is refused as the trigger route refuses it, 422 cloud_binding_used,
// making nothing; under another scope, the same scope and none alike.
func TestPostgreSQLCreatingAJobWithAUsedCloudBindingIsRefused(t *testing.T) {
	s, db, sv, jobs := pgScopedRunner(t)
	suite := s.pool(t, pgRoleSuite, db)
	usedCloudBindingRefused(t, sv, completingRuntime(t, sv.cfg.Dir), jobs["team-a"], "team-a", []string{"team-b", "team-a", ""}, func() (n [2]int) {
		must(t, suite.QueryRowContext(pgContext, "SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM triggers)").Scan(&n[0], &n[1]))
		return n
	})
}

// pgHeldFrom says that a request login held to scope neither reads nor
// changes the rows of another scope that each table holds, which the suite
// login sees: the policy itself holds them, not the Runner's scope check in
// front of it, which answers a record of another scope as absent too.
func pgHeldFrom(t *testing.T, s *pgTestServer, db, scope string, tables ...string) {
	t.Helper()
	suite := s.pool(t, pgRoleSuite, db)
	request := s.pool(t, pgRoleRequest, db)
	for _, table := range tables {
		var others int
		must(t, suite.QueryRowContext(pgContext, "SELECT count(*) FROM "+table+" WHERE scope<>$1", scope).Scan(&others))
		if others == 0 {
			t.Fatalf("%s holds no row of another scope than %s", table, scope)
		}
		var updatable bool
		must(t, request.QueryRowContext(pgContext, "SELECT has_table_privilege(current_user, $1, 'UPDATE')", postgresqlSchema+"."+table).Scan(&updatable))
		inScope(t, request, scope, false, func(tx *sql.Tx) {
			var read int
			must(t, tx.QueryRowContext(pgContext, "SELECT count(*) FROM "+table+" WHERE scope<>$1", scope).Scan(&read))
			var changed int64
			if updatable {
				res, err := tx.ExecContext(pgContext, "UPDATE "+table+" SET record=record WHERE scope<>$1", scope)
				must(t, err)
				changed, _ = res.RowsAffected()
			}
			if read != 0 || changed != 0 {
				t.Errorf("a request held to %s read %d and changed %d of %d rows of another scope in %s", scope, read, changed, others, table)
			}
		})
	}
}

// pgCall is a request to sv's routes that must answer want; it answers the
// body.
func pgCall(t *testing.T, sv *Service) func(method, path string, body any, headers map[string]string, want int) []byte {
	return func(method, path string, body any, headers map[string]string, want int) []byte {
		t.Helper()
		raw := ""
		if body != nil {
			raw = string(encode(body))
		}
		got := serve(t, sv, method, path, raw, headers)
		if got.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, got.Code, got.Body.String(), want)
		}
		return got.Body.Bytes()
	}
}

// Section 6 and D9, occurrences: on a Runner on the kind, a scope's event
// credential delivers held to its scope, and another scope, or a request
// naming none, neither delivers to its trigger, reads an occurrence's
// result, lists, cancels or reconciles its occurrences, nor reads a record
// delivery, guessing the ids: the trigger answers as an unknown one, the
// rest as absent. The tick, on the worker login, admits a schedule's
// occurrence in its job's scope and dispatches every scope's into a run of
// that scope. The queue's limit is the installation's count.
func TestPostgreSQLHoldsOccurrencesToTheirScope(t *testing.T) {
	s, db, sv, jobs := pgScopedRunner(t)
	a, b := jobs["team-a"], jobs["team-b"]
	call := pgCall(t, sv)
	suite := s.pool(t, pgRoleSuite, db)
	pending := func() int {
		t.Helper()
		var n int
		must(t, suite.QueryRowContext(pgContext, "SELECT value FROM counts WHERE name='pending-occurrences'").Scan(&n))
		return n
	}
	var created Trigger
	must(t, json.Unmarshal(call("POST", "/v1/jobs/"+a.ID+"/triggers?scope=team-a", map[string]any{"config": eventTriggerConfig()}, nil, 200), &created))
	var resumed struct {
		Trigger Trigger `json:"trigger"`
		Token   string  `json:"token"`
	}
	must(t, json.Unmarshal(call("POST", "/v1/triggers/"+created.ID+"/state?scope=team-a", map[string]any{"revision": created.Revision, "paused": false, "reviewed": true}, nil, 200), &resumed))
	token := map[string]string{"X-Trigger-Token": resumed.Token}
	var delivered struct {
		Occurrence Occurrence `json:"occurrence"`
	}
	must(t, json.Unmarshal(call("POST", "/v1/triggers/"+created.ID+"/events?scope=team-a", delivery("first"), token, 202), &delivered))
	admitted := delivered.Occurrence
	if admitted.State != "accepted" || admitted.Scope != "team-a" {
		t.Fatalf("the event was not admitted in its scope: %+v", admitted)
	}
	call("POST", "/v1/triggers/"+created.ID+"/events?scope=team-a", delivery("first"), token, 200)
	if n := pending(); n != 1 {
		t.Fatal("the installation's pending count is", n, "want 1")
	}
	pgHeldFrom(t, s, db, "team-b", "occurrences", "occurrence_keys")
	record := RecordDelivery{ReleaseID: a.ReleaseID, Event: delivery("record")}
	for _, q := range []string{"?scope=team-b", ""} {
		call("POST", "/v1/triggers/"+created.ID+"/events"+q, delivery("second"), token, 401)
		call("GET", "/v1/triggers/"+created.ID+"/occurrences/"+admitted.ID+q, nil, token, 401)
		call("POST", "/v1/triggers/"+created.ID+"/deliveries"+q, record, nil, 404)
		call("GET", "/v1/triggers/"+created.ID+"/deliveries/first"+q, nil, nil, 404)
		call("GET", "/v1/jobs/"+a.ID+"/occurrences"+q, nil, nil, 404)
		call("POST", "/v1/occurrences/"+admitted.ID+"/cancel"+q, nil, nil, 404)
		call("POST", "/v1/occurrences/"+admitted.ID+"/reconcile"+q, nil, nil, 404)
	}
	var page struct {
		Items []Occurrence `json:"items"`
	}
	must(t, json.Unmarshal(call("GET", "/v1/jobs/"+b.ID+"/occurrences?scope=team-b", nil, nil, 200), &page))
	if len(page.Items) != 0 {
		t.Fatal("team-b's job lists another scope's occurrence:", page.Items)
	}
	// The scope's own requests: its credential reads the result, its
	// record delivery is admitted and read, its occurrences listed; an
	// accepted occurrence is neither cancelled nor reconciled.
	call("GET", "/v1/triggers/"+created.ID+"/occurrences/"+admitted.ID+"?scope=team-a", nil, token, 200)
	call("POST", "/v1/triggers/"+created.ID+"/deliveries?scope=team-a", record, nil, 202)
	call("GET", "/v1/triggers/"+created.ID+"/deliveries/record?scope=team-a", nil, nil, 200)
	must(t, json.Unmarshal(call("GET", "/v1/jobs/"+a.ID+"/occurrences?scope=team-a", nil, nil, 200), &page))
	if len(page.Items) != 2 {
		t.Fatal("team-a's job lists", len(page.Items), "occurrences, want 2")
	}
	call("POST", "/v1/occurrences/"+admitted.ID+"/cancel?scope=team-a", nil, nil, 409)
	call("POST", "/v1/occurrences/"+admitted.ID+"/reconcile?scope=team-a", nil, nil, 409)
	if n := pending(); n != 2 {
		t.Fatal("the installation's pending count is", n, "want 2")
	}

	// The tick, on the worker login: team-b's schedule falls due, and every
	// scope's accepted occurrence is dispatched into a run of its scope.
	must(t, json.Unmarshal(call("POST", "/v1/jobs/"+b.ID+"/triggers?scope=team-b", map[string]any{"config": scheduleTriggerConfig(time.Now())}, nil, 200), &created))
	must(t, json.Unmarshal(call("POST", "/v1/triggers/"+created.ID+"/state?scope=team-b", map[string]any{"revision": created.Revision, "paused": false, "reviewed": true}, nil, 200), &resumed))
	due, err := time.Parse(time.RFC3339Nano, resumed.Trigger.NextAt)
	must(t, err)
	must(t, sv.automationTick(due.Add(time.Second)))
	if n := pending(); n != 0 {
		t.Fatal("the tick left", n, "occurrences pending")
	}
	for _, scope := range []string{"team-a", "team-b"} {
		job := jobs[scope]
		must(t, json.Unmarshal(call("GET", "/v1/jobs/"+job.ID+"/occurrences?scope="+scope, nil, nil, 200), &page))
		for _, o := range page.Items {
			if o.State != "submitted" || o.RunID == "" {
				t.Fatalf("%s's occurrence was not dispatched: %+v", scope, o)
			}
			var held string
			must(t, suite.QueryRowContext(pgContext, "SELECT o.scope||'/'||r.scope FROM runs r JOIN occurrences o ON o.record::json->>'runId'=r.id WHERE o.id=$1", o.ID).Scan(&held))
			if held != scope+"/"+scope {
				t.Fatalf("%s's occurrence was dispatched into a run of %q", scope, held)
			}
			call("GET", "/v1/runs/"+o.RunID+"?scope="+scope, nil, nil, 200)
			other := map[string]string{"team-a": "team-b", "team-b": "team-a"}[scope]
			call("GET", "/v1/runs/"+o.RunID+"?scope="+other, nil, nil, 404)
		}
		if want := map[string]int{"team-a": 2, "team-b": 1}[scope]; len(page.Items) != want {
			t.Fatal(scope, "holds", len(page.Items), "occurrences, want", want)
		}
	}

	// The queue's limit is the installation's: past it, team-a's event is
	// refused, and team-b's schedule, which has no sender, skipped.
	_, err = suite.ExecContext(pgContext, "UPDATE counts SET value=$1 WHERE name='pending-occurrences'", defaultPendingOccurrences)
	must(t, err)
	if got := call("POST", "/v1/triggers/"+admitted.TriggerID+"/events?scope=team-a", delivery("third"), token, 429); !strings.Contains(string(got), "queue_full") {
		t.Fatal("an event past the installation's queue was not refused:", string(got))
	}
	var listed struct {
		Items []Trigger `json:"items"`
	}
	must(t, json.Unmarshal(call("GET", "/v1/jobs/"+b.ID+"/triggers?scope=team-b", nil, nil, 200), &listed))
	due, err = time.Parse(time.RFC3339Nano, listed.Items[0].NextAt)
	must(t, err)
	must(t, sv.automationTick(due.Add(time.Second)))
	var reason string
	must(t, suite.QueryRowContext(pgContext, "SELECT state||':'||(record::json->>'reason') FROM occurrences WHERE trigger_id=$1 ORDER BY seq DESC LIMIT 1", created.ID).Scan(&reason))
	if reason != "skipped:queue-full" {
		t.Fatal("team-b's schedule past the installation's queue:", reason)
	}
}

// D11 and section 6, the retry: on a Runner on the kind, another scope, or a
// request naming none, does not retry a scope's interrupted run; the scope's
// own retry is refused while the attempt's directory is missing, which this
// kind does not take to hold no audit record, and is queued once it holds no
// trail.
func TestPostgreSQLRetriesARunHeldToItsScope(t *testing.T) {
	_, _, sv, jobs := pgScopedRunner(t)
	a := jobs["team-a"]
	call := pgCall(t, sv)
	var queued Run
	must(t, json.Unmarshal(call("POST", "/v1/jobs/"+a.ID+"/runs?scope=team-a", journalInput(), map[string]string{"Idempotency-Key": "one"}, 202), &queued))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sv.dispatchNext(ctx); err != nil {
		t.Fatal(err)
	}
	var r Run
	must(t, json.Unmarshal(call("GET", "/v1/runs/"+queued.ID+"?scope=team-a", nil, nil, 200), &r))
	if r.State != "interrupted" || r.Attempt != 1 {
		t.Fatalf("the run was not interrupted: %+v", r)
	}
	for _, q := range []string{"?scope=team-b", ""} {
		call("POST", "/v1/runs/"+r.ID+"/retry"+q, map[string]any{"attempt": 1}, nil, 404)
	}
	directory := filepath.Join(sv.cfg.Dir, "attempts", r.ID)
	must(t, os.RemoveAll(directory))
	if got := call("POST", "/v1/runs/"+r.ID+"/retry?scope=team-a", map[string]any{"attempt": 1}, nil, 409); !strings.Contains(string(got), "cannot be read") {
		t.Fatal("a retry whose attempt directory is missing:", string(got))
	}
	must(t, os.MkdirAll(directory, 0o700))
	var retried struct {
		State    string    `json:"state"`
		Attempts []Attempt `json:"attempts"`
	}
	must(t, json.Unmarshal(call("POST", "/v1/runs/"+r.ID+"/retry?scope=team-a", map[string]any{"attempt": 1}, nil, 202), &retried))
	if retried.State != "queued" || len(retried.Attempts) != 1 {
		t.Fatalf("the retry: %+v", retried)
	}
	var page EventPage
	must(t, json.Unmarshal(call("GET", "/v1/events?scope=team-b", nil, nil, 200), &page))
	for _, e := range page.Items {
		if e.Concerns.Run == r.ID {
			t.Fatal("team-b's journal holds the retried run's entry:", e.Kind)
		}
	}
}

// Section 7 and 0006_profile_rows_unscoped.sql: the request login writes the
// profile rows only in a transaction held to the none scope, by the
// database, not by the routes alone. Held to a scope identifier, or holding
// none, it inserts no profile row and updates none, by a plain update or by
// an insert that meets the row's key; held to the none scope it does both.
func TestPostgreSQLHoldsTheProfileRowsToTheNoneScope(t *testing.T) {
	s := pgServer(t)
	db := s.database(t)
	request := s.pool(t, pgRoleRequest, db)
	suite := s.pool(t, pgRoleSuite, db)
	upsert := "INSERT INTO metadata(key,value) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value=excluded.value"
	refused := func(scope string, unset bool, query string, args ...any) {
		t.Helper()
		inScope(t, request, scope, unset, func(tx *sql.Tx) {
			res, err := tx.ExecContext(pgContext, query, args...)
			if err == nil {
				if n, _ := res.RowsAffected(); n == 0 {
					return
				}
			}
			if pgCode(err) != "42501" {
				t.Fatalf("held to %q (unset %v), %s was not refused: %v", scope, unset, query, err)
			}
		})
	}
	written := func(query string, args ...any) {
		t.Helper()
		inScope(t, request, "", false, func(tx *sql.Tx) {
			res, err := tx.ExecContext(pgContext, query, args...)
			must(t, err)
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("held to the none scope, %s wrote %d rows", query, n)
			}
		})
	}
	for _, key := range []string{registeredProfilesKey, selectedProfilesKey} {
		// No row yet: an insert.
		refused("team-a", false, upsert, key, "{}")
		refused("", true, upsert, key, "{}")
		written(upsert, key, "{}")
		// A row held: an update, and an insert that meets its key.
		pgExec(t, suite, "INSERT INTO metadata(key,value) VALUES ($1,'{}')", key)
		for _, c := range []struct {
			query string
			args  []any
		}{{"UPDATE metadata SET value='[]' WHERE key=$1", []any{key}}, {upsert, []any{key, "[]"}}} {
			refused("team-a", false, c.query, c.args...)
			refused("", true, c.query, c.args...)
			written(c.query, c.args...)
		}
		var row string
		must(t, suite.QueryRowContext(pgContext, "SELECT value FROM metadata WHERE key=$1", key).Scan(&row))
		if row != "{}" {
			t.Fatalf("a refused or rolled-back write changed the %s row: %s", key, row)
		}
	}
}

// Section 5: a store migrated to the file before
// 0006_profile_rows_unscoped.sql is refused at the start until it is
// migrated, and the migration then applies that file and the ones after it.
func TestPostgreSQLRefusesAStoreWithoutTheProfileRowsFile(t *testing.T) {
	pgRefusesAStoreWithout(t, "0006_")
}

// Section 6 and 7, the installation's profiles: on a Runner on the kind, an
// input profile and a selected profile are registered held to the none
// scope, on the request login, which alone may write their rows; a request
// naming a scope is refused, and a repeated registration changes nothing.
func TestPostgreSQLRegistersTheInstallationsProfiles(t *testing.T) {
	s, db, sv, _ := pgScopedRunner(t)
	call := pgCall(t, sv)
	_, ordinary, _, _ := v2Fixture(t)
	selected := selectedProducer(t).profile
	conversion := map[string]string{"JPack-Conversion-Contract": selectedHeader(selectedContract())}
	call("POST", "/v1/input-profiles?scope=team-a", ordinary, nil, 400)
	call("POST", "/v1/input-profiles", ordinary, nil, 201)
	call("POST", "/v1/input-profiles", ordinary, nil, 200)
	call("POST", "/v1/retained-column/1/selected-profiles", selected, conversion, 201)
	call("POST", "/v1/retained-column/1/selected-profiles", selected, conversion, 200)
	var listed []struct {
		Profile InputProfile `json:"profile"`
	}
	must(t, json.Unmarshal(call("GET", "/v1/input-profiles", nil, nil, 200), &listed))
	found := false
	for _, p := range listed {
		found = found || p.Profile.ID == ordinary.ID
	}
	if !found {
		t.Fatal("the registered profile is not listed:", listed)
	}
	call("GET", "/v1/retained-column/1/selected-profiles", nil, conversion, 200)
	suite := s.pool(t, pgRoleSuite, db)
	for key, id := range map[string]string{registeredProfilesKey: ordinary.ID, selectedProfilesKey: selected.ID} {
		var row string
		must(t, suite.QueryRowContext(pgContext, "SELECT value FROM metadata WHERE key=$1", key).Scan(&row))
		if !strings.Contains(row, `"id":"`+id+`"`) {
			t.Fatalf("the %s row does not hold %s: %s", key, id, row)
		}
	}
}
