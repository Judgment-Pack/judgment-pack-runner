package runner

// The postgresql kind under test (docs/design/store-postgresql.md, section
// 10). The tests run against the server JPACK_TEST_POSTGRESQL names, as a
// login that can create roles and databases and is not a superuser; each is
// skipped, and says so, when none is named, and fails instead when
// JPACK_TEST_POSTGRESQL_REQUIRED is set, as CI sets it. One case also needs
// the login to grant pg_write_all_data (WITH ADMIN TRUE, INHERIT FALSE, SET
// FALSE, as CI grants it), and is skipped or fails the same way without it.
//
// Once per test binary the harness creates the four roles of section 5 as an
// operator would, under names of their own, and a template database migrated
// by MigratePostgreSQL as the migration login. Each case clones the template
// into a database of its own and drops it after. The conformance suite also
// writes, through the store as it is opened, the records a request writes:
// that store is opened on a fifth, test-only login that holds every row of
// every table, so that the suite's records can be made, while every view held
// to a scope is made on the request login, held by its policies. The worker
// login is held to section 7 by the tests below, not by the suite.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	pgTestEnv         = "JPACK_TEST_POSTGRESQL"
	pgTestRequiredEnv = "JPACK_TEST_POSTGRESQL_REQUIRED"
)

// pgTestServer is the server the tests use, with the roles and the template
// database the harness made on it.
type pgTestServer struct {
	admin    *sql.DB
	base     *url.URL
	prefix   string
	password map[string]string
	template string
	next     atomic.Int64
}

var pgTest struct {
	once   sync.Once
	server *pgTestServer
	err    error
}

// The roles, by the harness's name for them.
const (
	pgRoleOwner     = "owner"
	pgRoleMigration = "migration"
	pgRoleRequest   = "request"
	pgRoleWorker    = "worker"
	pgRoleSuite     = "suite"
)

func (s *pgTestServer) role(name string) string { return s.prefix + "_" + name }

// url is the connection string of the login name to the database db.
func (s *pgTestServer) url(name, db string) string {
	u := *s.base
	u.User = url.UserPassword(s.role(name), s.password[name])
	u.Path = "/" + db
	q := u.Query()
	q.Del("options")
	u.RawQuery = q.Encode()
	return u.String()
}

// pgServer is the server the tests use, or skips the test when none is named.
func pgServer(t *testing.T) *pgTestServer {
	t.Helper()
	conn := os.Getenv(pgTestEnv)
	if conn == "" {
		if os.Getenv(pgTestRequiredEnv) != "" {
			t.Fatal("no PostgreSQL server is named to the test, and " + pgTestRequiredEnv + " requires one: set " + pgTestEnv)
		}
		t.Skip("no PostgreSQL server is named to the test: set " + pgTestEnv + " to the connection string of a login that can create roles and databases")
	}
	pgTest.once.Do(func() { pgTest.server, pgTest.err = startPGTestServer(conn) })
	if pgTest.err != nil {
		t.Fatal(pgTest.err)
	}
	return pgTest.server
}

func pgTestSecret() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func startPGTestServer(conn string) (*pgTestServer, error) {
	base, err := url.Parse(conn)
	if err != nil || base.Scheme == "" {
		return nil, fmt.Errorf("%s is not a connection URL", pgTestEnv)
	}
	cfg, err := pgx.ParseConfig(conn)
	if err != nil {
		return nil, err
	}
	s := &pgTestServer{admin: stdlib.OpenDB(*cfg), base: base, prefix: "jpt_" + pgTestSecret()[:10], password: map[string]string{}}
	s.admin.SetMaxOpenConns(2)
	var super bool
	if err = s.admin.QueryRowContext(pgContext, "SELECT rolsuper FROM pg_roles WHERE rolname=current_user").Scan(&super); err != nil {
		return nil, err
	}
	if super {
		return nil, fmt.Errorf("%s names a superuser: the tests run as an administrator that is not one", pgTestEnv)
	}
	statements := []string{"CREATE ROLE " + s.role(pgRoleOwner) + " NOLOGIN"}
	for _, name := range []string{pgRoleMigration, pgRoleRequest, pgRoleWorker, pgRoleSuite} {
		s.password[name] = pgTestSecret()
		statements = append(statements, "CREATE ROLE "+s.role(name)+" LOGIN PASSWORD '"+s.password[name]+"'")
	}
	s.template = s.prefix + "_template"
	statements = append(statements,
		"GRANT "+s.role(pgRoleOwner)+" TO "+s.role(pgRoleMigration)+" WITH INHERIT FALSE, SET TRUE",
		"CREATE DATABASE "+s.template,
		"GRANT CREATE ON DATABASE "+s.template+" TO "+s.role(pgRoleOwner))
	for _, statement := range statements {
		if _, err = s.admin.ExecContext(pgContext, statement); err != nil {
			s.teardown()
			return nil, fmt.Errorf("%s: %w", statement, err)
		}
	}
	if _, err = MigratePostgreSQL(PostgreSQLMigration{Connection: s.url(pgRoleMigration, s.template), Owner: s.role(pgRoleOwner), Request: s.role(pgRoleRequest), Worker: s.role(pgRoleWorker)}); err != nil {
		s.teardown()
		return nil, err
	}
	// The suite's login: every row of every table, in the template only.
	if err = s.asOwner(s.template, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(pgContext, `GRANT USAGE ON SCHEMA jpack_runner TO `+s.role(pgRoleSuite)+`;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA jpack_runner TO `+s.role(pgRoleSuite)+`;
DO $$ DECLARE t text; BEGIN
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='jpack_runner' LOOP
    EXECUTE format('CREATE POLICY suite_rows ON jpack_runner.%I TO `+s.role(pgRoleSuite)+` USING (true) WITH CHECK (true)', t);
  END LOOP;
END $$;`)
		return err
	}); err != nil {
		s.teardown()
		return nil, err
	}
	return s, nil
}

// asOwner runs fn as the owner, through the migration login, on db.
func (s *pgTestServer) asOwner(db string, fn func(*sql.Tx) error) error {
	cfg, err := pgx.ParseConfig(s.url(pgRoleMigration, db))
	if err != nil {
		return err
	}
	conn := stdlib.OpenDB(*cfg)
	defer conn.Close()
	return pgAsOwner(conn, pgx.Identifier{s.role(pgRoleOwner)}.Sanitize(), fn)
}

// teardown drops what the harness made: its databases, then its roles.
func (s *pgTestServer) teardown() {
	rows, err := s.admin.QueryContext(pgContext, "SELECT datname FROM pg_database WHERE starts_with(datname, $1)", s.prefix+"_")
	if err == nil {
		var names []string
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				names = append(names, name)
			}
		}
		rows.Close()
		for _, name := range names {
			s.admin.ExecContext(pgContext, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		}
	}
	for _, name := range []string{pgRoleSuite, pgRoleWorker, pgRoleRequest, pgRoleMigration, pgRoleOwner} {
		s.admin.ExecContext(pgContext, "DROP ROLE IF EXISTS "+s.role(name))
	}
	s.admin.Close()
}

// pgTestTeardown drops what the harness made on the server, once the tests
// ran.
func pgTestTeardown() {
	if pgTest.server != nil {
		pgTest.server.teardown()
	}
}

// database clones the template into a database of the test's own, dropped
// after it.
func (s *pgTestServer) database(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("%s_%d", s.prefix, s.next.Add(1))
	if _, err := s.admin.ExecContext(pgContext, "CREATE DATABASE "+name+" TEMPLATE "+s.template); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A connection closed leaves its server process for a moment: the
		// database is dropped once none is left, which the administrator,
		// who may not end other roles' processes, needs.
		for range 200 {
			var left int
			if err := s.admin.QueryRowContext(pgContext, "SELECT count(*) FROM pg_stat_activity WHERE datname=$1", name).Scan(&left); err != nil || left == 0 {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if _, err := s.admin.ExecContext(pgContext, "DROP DATABASE "+name); err != nil {
			t.Error(err)
		}
	})
	return name
}

// pool is a pool of the login name to db, closed after the test.
func (s *pgTestServer) pool(t *testing.T, name, db string) *sql.DB {
	t.Helper()
	p, closePool, err := pgOpenPool(s.url(name, db))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		closePool()
	})
	return p
}

// connections are the run-time logins' connection strings to db, as a
// Runner is configured with them.
func (s *pgTestServer) connections(db string) PostgreSQLConnections {
	return PostgreSQLConnections{Request: s.url(pgRoleRequest, db), Worker: s.url(pgRoleWorker, db), Owner: s.role(pgRoleOwner)}
}

var pgTestOpening = storeOpening{workspace: "conformance", owner: "owner", migrate: true}

// openPGForTest is the kind on a database of the test's own, opened on the
// suite's login for every scope and on the request login for each scope.
func openPGForTest(t *testing.T) *pgStore {
	t.Helper()
	s := pgServer(t)
	db := s.database(t)
	k := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleSuite, db))
	if err := k.start(pgTestOpening); err != nil {
		t.Fatal(err)
	}
	return k
}

// pgOf is the postgresql kind st is, or runs on under the twice mode.
func pgOf(st runStore) *pgStore { return underlying(st).(*pgStore) }

func pgExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(pgContext, query, args...); err != nil {
		t.Fatal(query, ": ", err)
	}
}

// pgKindsUnderTest are the postgresql kind, and the kind under the twice mode.
func pgKindsUnderTest() []storeKindUnderTest {
	kind := storeKindUnderTest{
		holdsScopes:    true,
		fencesClaims:   true,
		migratedSchema: true,
		passTime: func(st runStore, d time.Duration) {
			// The lease is the database's: a lease d nearer its end is d
			// later by its clock.
			if _, err := pgOf(st).p.worker.ExecContext(pgContext, "UPDATE runs SET lease_until=lease_until-make_interval(secs => $1) WHERE lease_until IS NOT NULL", d.Seconds()); err != nil {
				panic(err)
			}
		},
		seedJob: func(t *testing.T, st runStore, j Job) {
			// Two of the statements createJob makes.
			db := pgOf(st).p.worker
			pgExec(t, db, "INSERT INTO jobs(id,release_id,record) VALUES ($1,$2,$3)", j.ID, j.ReleaseID, string(encode(j)))
			pgExec(t, db, "INSERT INTO job_revisions(job_id,revision,release_id,record) VALUES ($1,$2,$3,$4)", j.ID, j.Revision, j.ReleaseID, string(encode(j)))
			// And the identities the revision's release holds, as createJob
			// holds them (D9).
			must(t, pgOf(st).update(func(tx runTx) error { return tx.(*pgTx).holdIdentities(j.ReleaseID) }))
		},
		schema: func(t *testing.T, st runStore) string {
			var schema string
			if err := pgOf(st).p.worker.QueryRowContext(pgContext, "SELECT value FROM metadata WHERE key='schema'").Scan(&schema); err != nil {
				t.Fatal(err)
			}
			return schema
		},
		failCommits: func(st runStore, err error) {
			k := pgOf(st)
			if err == nil {
				k.p.commit = (*sql.Tx).Commit
				return
			}
			k.p.commit = func(*sql.Tx) error { return err }
		},
		spoil: func(t *testing.T, st runStore, run string) {
			// A record the generated scope column reads, and no run.
			pgExec(t, pgOf(st).p.worker, "UPDATE runs SET record='[]' WHERE id=$1", run)
		},
	}
	direct, twice := kind, kind
	direct.name = "postgresql"
	direct.open = func(t *testing.T) runStore { return openPGForTest(t) }
	twice.name = "postgresql-twice"
	twice.open = func(t *testing.T) runStore {
		return &twiceStore{runStore: openPGForTest(t), record: newTwiceRecord()}
	}
	return []storeKindUnderTest{direct, twice}
}
