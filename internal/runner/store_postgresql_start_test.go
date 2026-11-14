package runner

// The start's checks of the live schema (checkSchema), on the server named
// to the test (JPACK_TEST_POSTGRESQL): a store changed by hand after its
// migration, or a configuration that names another owner, is refused at
// start, each by its name, while a store as the migration left it starts.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// pgStartOn opens the kind on db as a Runner starts it, with the owner its
// configuration names.
func pgStartOn(s *pgTestServer, db, owner string) error {
	o := pgTestOpening
	o.postgresql = s.connections(db)
	o.postgresql.Owner = owner
	k, err := openPostgreSQLStore(o)
	if err == nil {
		k.close()
	}
	return err
}

// wantStartRefused fails the test unless err is the refusal want, naming
// what it found.
func wantStartRefused(t *testing.T, err, want error, names string) {
	t.Helper()
	if !errors.Is(err, want) || faultOf(err) != faultRefused || !strings.Contains(err.Error(), names) {
		t.Fatalf("the start was not refused with %q naming %s: %v", want, names, err)
	}
}

// pgOwnerDoes runs statements as the owner on db.
func pgOwnerDoes(t *testing.T, s *pgTestServer, db string, statements ...string) {
	t.Helper()
	must(t, s.asOwner(db, func(tx *sql.Tx) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(pgContext, statement); err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
		return nil
	}))
}

// pgAdminDoes runs a statement as the administrator, and undo after the
// test.
func pgAdminDoes(t *testing.T, s *pgTestServer, statement, undo string) {
	t.Helper()
	if _, err := s.admin.ExecContext(pgContext, statement); err != nil {
		t.Fatal(statement, ": ", err)
	}
	t.Cleanup(func() {
		if _, err := s.admin.ExecContext(pgContext, undo); err != nil {
			t.Error(undo, ": ", err)
		}
	})
}

// The schema and every relation, function, operator and type in it are the
// owner's the configuration names.
func TestPostgreSQLStartRefusesASchemaNotOwnedByTheNamedOwner(t *testing.T) {
	s := pgServer(t)
	db := s.database(t)
	must(t, pgStartOn(s, db, s.role(pgRoleOwner)))
	t.Run("another owner named", func(t *testing.T) {
		wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleSuite)), errSchemaNotOwned, "schema "+postgresqlSchema)
	})
	t.Run("a relation another role owns", func(t *testing.T) {
		db := s.database(t)
		migration := pgx.Identifier{s.role(pgRoleMigration)}.Sanitize()
		pgOwnerDoes(t, s, db, "GRANT USAGE, CREATE ON SCHEMA jpack_runner TO "+migration)
		cfg, err := pgx.ParseConfig(s.url(pgRoleMigration, db))
		must(t, err)
		conn := stdlib.OpenDB(*cfg)
		defer conn.Close()
		for _, statement := range []string{"CREATE TABLE jpack_runner.stray (x int)", "ALTER TABLE jpack_runner.stray ENABLE ROW LEVEL SECURITY", "ALTER TABLE jpack_runner.stray FORCE ROW LEVEL SECURITY"} {
			pgExec(t, conn, statement)
		}
		wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errSchemaNotOwned, "stray")
	})
	for name, c := range map[string]struct{ statement, names string }{
		"a function another role owns":  {"CREATE FUNCTION jpack_runner.statement_timestamp() RETURNS timestamptz LANGUAGE sql AS $$SELECT 'infinity'::timestamptz$$", "function statement_timestamp"},
		"an operator another role owns": {"CREATE OPERATOR jpack_runner.=== (LEFTARG = int, RIGHTARG = int, FUNCTION = int4eq)", "operator ==="},
		"a type another role owns":      {"CREATE TYPE jpack_runner.stray_kind AS ENUM ('a')", "type _stray_kind"},
	} {
		t.Run(name, func(t *testing.T) {
			db := s.database(t)
			migration := pgx.Identifier{s.role(pgRoleMigration)}.Sanitize()
			pgOwnerDoes(t, s, db, "GRANT USAGE, CREATE ON SCHEMA jpack_runner TO "+migration)
			cfg, err := pgx.ParseConfig(s.url(pgRoleMigration, db))
			must(t, err)
			conn := stdlib.OpenDB(*cfg)
			defer conn.Close()
			pgExec(t, conn, c.statement)
			wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errSchemaNotOwned, c.names)
		})
	}
}

// Every table has row security enabled and forced.
func TestPostgreSQLStartRefusesATableWithoutForcedRowSecurity(t *testing.T) {
	s := pgServer(t)
	for name, statement := range map[string]string{
		"not forced": "ALTER TABLE jpack_runner.runs NO FORCE ROW LEVEL SECURITY",
		"disabled":   "ALTER TABLE jpack_runner.runs DISABLE ROW LEVEL SECURITY",
	} {
		t.Run(name, func(t *testing.T) {
			db := s.database(t)
			pgOwnerDoes(t, s, db, statement)
			wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errRowSecurityOff, "runs")
		})
	}
}

// Neither run-time login holds DELETE, TRUNCATE, REFERENCES or TRIGGER on a
// table of the schema, REFERENCES on a column of one, a privilege on a
// sequence of it, or CREATE on the schema: granted to it, to a role it inherits or may SET, to a predefined
// role it is a member of, or by a default privilege.
func TestPostgreSQLStartRefusesARuntimeLoginWithWiderPrivileges(t *testing.T) {
	s := pgServer(t)
	worker, request := pgx.Identifier{s.role(pgRoleWorker)}.Sanitize(), pgx.Identifier{s.role(pgRoleRequest)}.Sanitize()
	for _, privilege := range []string{"DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
		t.Run(privilege+" granted to the worker login", func(t *testing.T) {
			db := s.database(t)
			pgOwnerDoes(t, s, db, "GRANT "+privilege+" ON jpack_runner.runs TO "+worker)
			wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, privilege+" on runs")
		})
	}
	for _, privilege := range []string{"SELECT", "UPDATE", "USAGE"} {
		t.Run(privilege+" on a sequence granted to the worker login", func(t *testing.T) {
			db := s.database(t)
			pgOwnerDoes(t, s, db, "GRANT "+privilege+" ON SEQUENCE jpack_runner.runs_seq_seq TO "+worker)
			wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, privilege+" on sequence runs_seq_seq")
		})
	}
	t.Run("REFERENCES on a column granted to the worker login", func(t *testing.T) {
		db := s.database(t)
		pgOwnerDoes(t, s, db, "GRANT REFERENCES (id) ON jpack_runner.runs TO "+worker)
		wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, "REFERENCES on runs")
	})
	t.Run("DELETE granted to the request login", func(t *testing.T) {
		db := s.database(t)
		pgOwnerDoes(t, s, db, "GRANT DELETE ON jpack_runner.jobs TO "+request)
		wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, s.role(pgRoleRequest)+": DELETE on jobs")
	})
	for name, option := range map[string]string{"a role it inherits": "", "a role it may SET": " WITH INHERIT FALSE, SET TRUE"} {
		t.Run("DELETE granted to "+name, func(t *testing.T) {
			role := s.role("wide_" + pgTestSecret()[:6])
			quoted := pgx.Identifier{role}.Sanitize()
			pgAdminDoes(t, s, "CREATE ROLE "+quoted+" NOLOGIN", "DROP ROLE "+quoted)
			db := s.database(t)
			pgOwnerDoes(t, s, db, "GRANT DELETE ON jpack_runner.runs TO "+quoted)
			pgAdminDoes(t, s, "GRANT "+quoted+" TO "+worker+option, "REVOKE "+quoted+" FROM "+worker)
			wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, "DELETE on runs")
		})
	}
	t.Run("a default privilege", func(t *testing.T) {
		db := s.database(t)
		pgOwnerDoes(t, s, db,
			"ALTER DEFAULT PRIVILEGES IN SCHEMA jpack_runner GRANT DELETE ON TABLES TO "+worker,
			"CREATE TABLE jpack_runner.later (x int)",
			"ALTER TABLE jpack_runner.later ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE jpack_runner.later FORCE ROW LEVEL SECURITY")
		wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, "DELETE on later")
	})
	t.Run("pg_write_all_data", func(t *testing.T) {
		db := s.database(t)
		grant := "GRANT pg_write_all_data TO " + worker
		if _, err := s.admin.ExecContext(pgContext, grant); err != nil {
			if os.Getenv(pgTestRequiredEnv) != "" {
				t.Fatal("the administrator cannot grant pg_write_all_data; grant it WITH ADMIN TRUE, INHERIT FALSE, SET FALSE:", err)
			}
			t.Skip("the administrator cannot grant pg_write_all_data:", err)
		}
		t.Cleanup(func() {
			if _, err := s.admin.ExecContext(pgContext, "REVOKE pg_write_all_data FROM "+worker); err != nil {
				t.Error(err)
			}
		})
		wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, ": DELETE on ")
	})
	t.Run("CREATE on the schema", func(t *testing.T) {
		db := s.database(t)
		pgOwnerDoes(t, s, db, "GRANT CREATE ON SCHEMA jpack_runner TO "+request)
		wantStartRefused(t, pgStartOn(s, db, s.role(pgRoleOwner)), errLoginPrivilegeTooWide, "CREATE on schema "+postgresqlSchema)
	})
}
