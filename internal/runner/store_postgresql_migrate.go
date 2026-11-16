package runner

// The postgresql kind's migrations (docs/design/store-postgresql.md, section
// 5): plain SQL files shipped in the binary with migrations.sha256, applied in
// order by `jpack-runner store migrate` as the migration login, each file in
// its own transaction under SET LOCAL ROLE to the owner, never at a Runner's
// start. A second run applies nothing.

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed postgresql/migrations/*.sql postgresql/migrations.sha256
var pgMigrationFiles embed.FS

// pgGrantsVersion is the version of the grants file (D6), whose row records
// the logins it was applied with, which the start reads. A later file that
// grants to the logins records them on its row too (namesLogins), and is
// applied only with the logins the grants file recorded.
const pgGrantsVersion = "0002"

// pgMigration is one file of the set.
type pgMigration struct{ version, name, digest, text string }

// namesLogins says the file grants to the run-time logins: `jpack-runner store
// migrate` fills its {{request}} and {{worker}} with the logins' names, and
// records them on its row.
func (m pgMigration) namesLogins() bool {
	return strings.Contains(m.text, "{{request}}") || strings.Contains(m.text, "{{worker}}")
}

// postgresqlMigrations is the set this Runner ships, in order, each held to
// its line of migrations.sha256 (condition i).
func postgresqlMigrations() ([]pgMigration, error) {
	listed, err := pgMigrationFiles.ReadFile("postgresql/migrations.sha256")
	if err != nil {
		return nil, err
	}
	names, err := fs.Glob(pgMigrationFiles, "postgresql/migrations/*.sql")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(listed), "\n"), "\n")
	if len(lines) != len(names) {
		return nil, errors.New("the postgresql migrations do not match migrations.sha256")
	}
	var set []pgMigration
	for i, name := range names {
		b, err := pgMigrationFiles.ReadFile(name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		m := pgMigration{version: path.Base(name)[:4], name: path.Base(name), digest: hex.EncodeToString(sum[:]), text: string(b)}
		if lines[i] != m.digest+"  "+m.name {
			return nil, errors.New("the postgresql migrations do not match migrations.sha256")
		}
		set = append(set, m)
	}
	return set, nil
}

// PostgreSQLMigration is what `jpack-runner store migrate` is given: the
// migration login's connection string, and the names of the owner role and
// the two run-time logins (section 5). EvenWithLiveClaims applies the files
// while a Runner of the store holds an unexpired claim, which the command is
// otherwise refused (`--even-with-live-claims`).
type PostgreSQLMigration struct {
	Connection             string
	Owner, Request, Worker string
	EvenWithLiveClaims     bool
}

// pgLiveClaims counts the unexpired claims on the store, by the database's
// clock: the runs, and, once 0004_several_runners.sql is applied, the
// occurrences, a Runner holds under a lease. The store records no Runner's
// presence beyond its claims, so a Runner holding none is not counted.
func pgLiveClaims(db *sql.DB, owner string, occurrences bool) (runs, occs int, err error) {
	err = pgAsOwner(db, owner, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(pgContext, "SELECT count(*) FROM jpack_runner.runs WHERE claim_id IS NOT NULL AND lease_until>now()").Scan(&runs); err != nil {
			return err
		}
		if !occurrences {
			return nil
		}
		return tx.QueryRowContext(pgContext, "SELECT count(*) FROM jpack_runner.occurrences WHERE claim_id IS NOT NULL AND lease_until>now()").Scan(&occs)
	})
	return runs, occs, err
}

// pgRoleName is the form a role named to the kind takes: letters, digits and
// underscores, not starting with a digit, at most 63 bytes. The grants file
// takes each name as a quoted identifier, inside statements and inside the
// literals its DO blocks execute.
var pgRoleName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// errForeignTable is the refusal of a database holding a table the kind did
// not make (condition a).
var errForeignTable = errors.New("the database holds a table outside the postgresql store's schema: the store needs a database of its own")

// MigratePostgreSQL applies each file of the set not yet recorded, in order,
// and returns the versions it applied: none on a second run. It refuses a
// database holding a table in any schema but the kind's and the system's, a
// file recorded under another digest (applying nothing after it), a recorded
// file this Runner does not ship, grants recorded for other logins than the
// ones named, and, before applying a file, a store on which a Runner holds an
// unexpired claim on a run or an occurrence, unless EvenWithLiveClaims.
func MigratePostgreSQL(m PostgreSQLMigration) ([]string, error) {
	for _, name := range []string{m.Owner, m.Request, m.Worker} {
		if !pgRoleName.MatchString(name) {
			return nil, errors.New("a role named to the postgresql store is letters, digits and underscores, not starting with a digit, at most 63 of them")
		}
	}
	if m.Request == m.Worker || m.Request == m.Owner || m.Worker == m.Owner {
		return nil, errors.New("the owner, the request login and the worker login are three roles")
	}
	set, err := postgresqlMigrations()
	if err != nil {
		return nil, err
	}
	return migratePostgreSQL(m, set)
}

// migratePostgreSQL is MigratePostgreSQL, applying set, a prefix of the set
// shipped: a test makes with it a store an earlier release left.
func migratePostgreSQL(m PostgreSQLMigration, set []pgMigration) ([]string, error) {
	cfg, err := pgx.ParseConfig(m.Connection)
	if err != nil {
		return nil, errors.New("the migration login's connection string does not read")
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	db.SetMaxOpenConns(1)
	var foreign int
	if err = db.QueryRowContext(pgContext, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p','v','m','f') AND n.nspname<>$1 AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp\_%'`, postgresqlSchema).Scan(&foreign); err != nil {
		return nil, err
	}
	if foreign > 0 {
		return nil, errForeignTable
	}
	owner := pgx.Identifier{m.Owner}.Sanitize()
	type recorded struct{ digest, request, worker string }
	held := map[string]recorded{}
	var exists bool
	// Read from the catalog: the migration login holds no privilege on the
	// kind's schema, which to_regclass would need.
	if err = db.QueryRowContext(pgContext, "SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='schema_migrations')", postgresqlSchema).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		err = pgAsOwner(db, owner, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(pgContext, "SELECT version,digest,request_login,worker_login FROM jpack_runner.schema_migrations")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var version string
				var r recorded
				if err = rows.Scan(&version, &r.digest, &r.request, &r.worker); err != nil {
					return err
				}
				held[version] = r
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	shipped := map[string]bool{}
	for _, f := range set {
		shipped[f.version] = true
	}
	for version := range held {
		if !shipped[version] {
			return nil, fmt.Errorf("the store holds the migration %s, which this Runner does not ship", version)
		}
	}
	var applied []string
	claimsChecked := !exists || m.EvenWithLiveClaims
	for _, f := range set {
		if r, ok := held[f.version]; ok {
			if r.digest != f.digest {
				return applied, fmt.Errorf("the migration %s is recorded under another digest: nothing after it is applied", f.name)
			}
			if f.namesLogins() && (r.request != m.Request || r.worker != m.Worker) {
				return applied, fmt.Errorf("the grants were made to the request login %q and the worker login %q: replacing a login is not a migration", r.request, r.worker)
			}
			continue
		}
		if !claimsChecked {
			// A Runner checks the migrations only at its start: one still
			// running after a file is applied keeps claiming work under
			// the earlier schema. Before the first file to apply, refuse
			// while a Runner holds an unexpired claim.
			_, several := held["0004"]
			runs, occs, err := pgLiveClaims(db, owner, several)
			if err != nil {
				return applied, err
			}
			if runs+occs > 0 {
				return applied, fmt.Errorf("the store holds %d unexpired claims (%d runs, %d occurrences): a Runner of the store is running; stop every Runner of the store before migrating, or pass --even-with-live-claims (the store records no Runner beyond its claims)", runs+occs, runs, occs)
			}
			claimsChecked = true
		}
		text := f.text
		request, worker := "", ""
		if f.namesLogins() {
			request, worker = m.Request, m.Worker
			text = strings.NewReplacer("{{request}}", pgx.Identifier{m.Request}.Sanitize(), "{{worker}}", pgx.Identifier{m.Worker}.Sanitize()).Replace(text)
		}
		if err = pgAsOwner(db, owner, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(pgContext, text); err != nil {
				return fmt.Errorf("the migration %s: %w", f.name, err)
			}
			_, err := tx.ExecContext(pgContext, "INSERT INTO jpack_runner.schema_migrations(version,digest,request_login,worker_login) VALUES ($1,$2,$3,$4)", f.version, f.digest, request, worker)
			return err
		}); err != nil {
			return applied, err
		}
		applied = append(applied, f.version)
	}
	return applied, nil
}

// pgAsOwner runs fn in one transaction as the owner, with the migration's
// setting on for it alone.
func pgAsOwner(db *sql.DB, owner string, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(pgContext, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(pgContext, "SET LOCAL ROLE "+owner); err != nil {
		return err
	}
	if _, err = tx.ExecContext(pgContext, "SELECT set_config('jpack.migration', 'on', true)"); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// PostgreSQLConnectionsFile reads a postgresql store's connection file: one
// JSON object, {"request": ..., "worker": ..., "owner": ...}, the two
// run-time logins' connection strings and the owner role's name, each a
// string, and nothing else.
func PostgreSQLConnectionsFile(raw []byte) (PostgreSQLConnections, error) {
	var c struct {
		Request *string `json:"request"`
		Worker  *string `json:"worker"`
		Owner   *string `json:"owner"`
	}
	refused := errors.New(`the postgresql connection file is one JSON object with request, worker and owner, each a string, and nothing else`)
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || len(members) != 3 {
		return PostgreSQLConnections{}, refused
	}
	for name := range members {
		if name != "request" && name != "worker" && name != "owner" {
			return PostgreSQLConnections{}, refused
		}
	}
	if json.Unmarshal(raw, &c) != nil || c.Request == nil || c.Worker == nil || c.Owner == nil {
		return PostgreSQLConnections{}, refused
	}
	if !pgRoleName.MatchString(*c.Owner) {
		return PostgreSQLConnections{}, errors.New("the postgresql connection file's owner is letters, digits and underscores, not starting with a digit, at most 63 of them")
	}
	return PostgreSQLConnections{Request: *c.Request, Worker: *c.Worker, Owner: *c.Owner}, nil
}

// PostgreSQLMigrationFile reads `jpack-runner store migrate`'s connection
// file: one JSON object, {"migration": ..., "owner": ..., "request": ...,
// "worker": ...}, the migration login's connection string and the names of
// the owner role and the two run-time logins, each a string, and nothing else.
func PostgreSQLMigrationFile(raw []byte) (PostgreSQLMigration, error) {
	refused := errors.New(`the migration's connection file is one JSON object with migration, owner, request and worker, each a string, and nothing else`)
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || len(members) != 4 {
		return PostgreSQLMigration{}, refused
	}
	values := map[string]string{}
	for _, name := range []string{"migration", "owner", "request", "worker"} {
		var v string
		if json.Unmarshal(members[name], &v) != nil {
			return PostgreSQLMigration{}, refused
		}
		values[name] = v
	}
	return PostgreSQLMigration{Connection: values["migration"], Owner: values["owner"], Request: values["request"], Worker: values["worker"]}, nil
}
