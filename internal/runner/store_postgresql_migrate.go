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
// the logins it was applied with.
const pgGrantsVersion = "0002"

// pgMigration is one file of the set.
type pgMigration struct{ version, name, digest, text string }

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
// the two run-time logins (section 5).
type PostgreSQLMigration struct {
	Connection             string
	Owner, Request, Worker string
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
// file this Runner does not ship, and grants recorded for other logins than
// the ones named.
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
	for _, f := range set {
		if r, ok := held[f.version]; ok {
			if r.digest != f.digest {
				return applied, fmt.Errorf("the migration %s is recorded under another digest: nothing after it is applied", f.name)
			}
			if f.version == pgGrantsVersion && (r.request != m.Request || r.worker != m.Worker) {
				return applied, fmt.Errorf("the grants were made to the request login %q and the worker login %q: replacing a login is not a migration", r.request, r.worker)
			}
			continue
		}
		text := f.text
		request, worker := "", ""
		if f.version == pgGrantsVersion {
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
