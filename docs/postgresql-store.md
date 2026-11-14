# The postgresql store kind

A Runner in [service mode](../README.md#service-mode) can keep its queue and records in a
PostgreSQL 16 database instead of the SQLite database under its state directory. This page is for
the operator who sets it up. The design, with every decision and its reasons, is
[docs/design/store-postgresql.md](design/store-postgresql.md); the decision record is
[ADR 0001](adr/0001-second-storage-kind.md).

SQLite stays the default. A Runner started without a `store` member, and the companion Desk
starts, keep the sqlite kind exactly as before.

**This kind serves part of the control API.** It holds the routes that make a job and run it, and
refuses every other route with `501 store_route_unsupported` (below). Run one Runner per store:
several Runners on one store is issue #245.

## What it needs

- A PostgreSQL 16 server, and a database of the store's own. Migration refuses a database that
  holds a table in any schema other than the store's (`jpack_runner`) and the system's.
- Four roles, made by the operator beforehand. The Runner makes no role, alters none and stores no
  setting on a role or the database.

| Role | Attributes | What it is for |
| --- | --- | --- |
| The owner | `NOLOGIN` | Owns the schema and every object in it. Needs `CREATE` on the database to make the schema. |
| The migration login | `LOGIN`, a member of the owner `WITH INHERIT FALSE, SET TRUE` | Runs `jpack-runner store migrate`; reaches the owner only by `SET ROLE`. |
| The request login | `LOGIN`; not `SUPERUSER`, `CREATEROLE`, `CREATEDB` or `BYPASSRLS`; not a member of the owner or of the worker login | A request's transactions, each held to the one scope identifier the request names. |
| The worker login | `LOGIN`; not `SUPERUSER`, `CREATEROLE`, `CREATEDB` or `BYPASSRLS`; not a member of the owner | The work done for every scope: the dispatcher, claims and renewals, finishes, the run chain, the ticks, the restart, and a listing that names no scope. |

For example, as an administrator that may create roles and databases (role names are the
operator's choice: letters, digits and underscores, not starting with a digit, at most 63):

```sql
CREATE ROLE jpack_owner NOLOGIN;
CREATE ROLE jpack_migration LOGIN PASSWORD '...';
CREATE ROLE jpack_request LOGIN PASSWORD '...';
CREATE ROLE jpack_worker LOGIN PASSWORD '...';
GRANT jpack_owner TO jpack_migration WITH INHERIT FALSE, SET TRUE;
CREATE DATABASE jpack_runner_store;
GRANT CREATE ON DATABASE jpack_runner_store TO jpack_owner;
```

## Migrate

Migrations are plain SQL files shipped inside `jpack-runner`
(`internal/runner/postgresql/migrations/`, with `migrations.sha256`, the digest of each file). They
are applied by one command, run as a release step, never by a Runner's start:

```sh
jpack-runner store migrate --connection /srv/jobs/store-migration.json
```

The file is one JSON object with exactly these members, each a string:

```json
{"migration":"postgres://jpack_migration:...@db.internal:5432/jpack_runner_store?sslmode=verify-full",
 "owner":"jpack_owner","request":"jpack_request","worker":"jpack_worker"}
```

It is held to the service configuration's file rules: an absolute path, no symbolic link in it,
one regular file owned by Runner's user that neither its group nor others may read or write, at
most 128 KiB. The command connects as the migration login, takes the owner's role for each file's
transaction, applies each file not yet recorded in `jpack_runner.schema_migrations`, and prints
the versions it applied, `{"applied":["0001","0002"]}` the first time and `{"applied":[]}` on a
second run, which changes nothing.

It refuses, and applies nothing more:

- a database that holds a table outside the store's schema and the system's;
- a file recorded under another digest (nothing after it is applied);
- a recorded migration this `jpack-runner` does not ship;
- a request or worker login other than the ones the grants were first made to: replacing a login
  is not a migration.

The grants file (`0002_grants.sql`) gives the two run-time logins `USAGE` on the schema and the
table privileges of the design's section 7, and no privilege on a sequence, and no `DELETE`,
`TRUNCATE`, `REFERENCES` or `TRIGGER`: at run time the Runner deletes, truncates and drops nothing.

## Configure

The service configuration gains one optional member, `store`:

```json
{"dir":"/srv/jobs/state","runtime":"/srv/jobs/bin/jpack","workspace":"ops","owner":"ops-installation","listen":"127.0.0.1:8470",
 "store":{"kind":"postgresql","connection":"/srv/jobs/store-connection.json"}}
```

`kind` is `postgresql`, the only kind the member names; `connection` is the absolute path of the
connection file, held to the same file rules as above and refused inside the state directory. The
connection file is one JSON object with exactly these members, each a string:

```json
{"request":"postgres://jpack_request:...@db.internal:5432/jpack_runner_store?sslmode=verify-full",
 "worker":"postgres://jpack_worker:...@db.internal:5432/jpack_runner_store?sslmode=verify-full",
 "owner":"jpack_owner"}
```

The connection strings are read by the `pgx` driver: a URL or keyword/value form, with its TLS
settings. The files are read once, at start.

At start the Runner refuses to open the store when:

- either run-time login is a superuser, bypasses row security, can create roles or databases, or
  is a member of the owner; or the request login is the worker login or a member of it;
- the store is not migrated, or its applied migrations are not exactly this Runner's set: run
  `jpack-runner store migrate` with the matching `jpack-runner` first;
- the grants were made to other logins than the ones configured;
- the store's identity (workspace, owner, input root) is another Runner's;
- the configuration asks for manual admission, which stays on the sqlite kind.

The state directory is still required. It holds what stays on the host's disk: the attempt
directories, the pinned Runtime and a preview's scratch directory (below). It holds no
`runner.db` and takes no `runner.lock` for this kind.

## Scope identifiers and row security

Row security is enabled and forced on every table, the owner included. Every transaction the
request login makes sets `jpack.scope` for that transaction alone (`set_config(..., true)`), to
the scope identifier the request names, or to none; the policy on each table with scoped rows
admits only the rows of that scope. A transaction that sets no scope reaches no scoped row. The
journal also shows a request the entries of no scope that name no job (the Runner's own entries,
previews and refused creations), as the sqlite kind does.

The installation's rules (a release makes one job, a pack or dialect identity is never reused
with other bytes, at most 128 triggers, the queue's limits, one trigger per cloud binding) are
held across every scope, by keys and counter rows no policy narrows, and answered as on the
sqlite kind. A request held to one scope can learn that some scope holds a release or an
identity, never which. Journal sequences are shared across scopes: a page shows gaps where other
scopes' entries are.

## Claims and leases

There is no process-wide lock. A run is held by its claim: a token and a lease of 60 seconds,
compared with the database's clock when each write's statement starts, so a finish or a renewal
that waited for another writer past its lease's end writes nothing. The Runner renews the lease every 15 seconds while the
attempt runs. When a renewal fails, conflicts, or takes longer than 15 seconds, or none has
succeeded within 30 seconds of the last that did, the Runner stops the attempt's Runtime, writes
nothing more for that run, and stops its dispatcher: `GET /v1/health` then reports it unhealthy,
and the Runner needs a restart. A run left running by a Runner that stopped is interrupted by the
dispatcher's tick once its lease has run out, about a minute later, not at the next start.

## Time bounds

Every connection of the two run-time pools starts with three settings. They are the session's
start parameters, so they override what the connection string or the login's own settings name:

| Setting | Value | Why |
|---|---|---|
| `lock_timeout` | 5 s | Every write first takes the lock of `journal_head`, so a session that holds it holds up every writer. A statement that waits longer than 5 s for a lock is refused. 5 s is a third of the 15 s between renewals: a renewal that meets a held lock fails within 5 s of its start, well inside the 30 s after which the Runner stops the attempt in any case. |
| `statement_timeout` | 30 s | Above the lock bound, so that a wait for a lock is refused as one; a bound on any statement that holds a pooled connection. |
| `idle_in_transaction_session_timeout` | 10 s | A session left idle inside a transaction keeps the journal's lock. The server ends it after 10 s, so a writer behind it meets at most two lock refusals. The Runner's own transactions read no file and make no network call inside a transaction, so they are not idle that long. |

Each renewal of a claim is also held to a deadline of 15 s, the interval between renewals: one
not done by then is rolled back, its statement cancelled on the server, and the claim is lost, as
a renewal that took longer than 15 s already was. An attempt that ends while a renewal is in
flight waits for it at most that long.

A write refused for one of these bounds writes nothing. Its error names the bound: "waited longer
than its lock bound (lock_timeout)", "ran longer than its bound (statement_timeout)", "stayed idle
inside a transaction longer than its bound (idle_in_transaction_session_timeout)" or "did not end
within its deadline". A request answers `500` with the code `store_error`, as for any other store
failure. A write of the dispatcher refused so stops it, as any store failure does, and
`GET /v1/health` reports the Runner unhealthy. A lock wait is not run again: an operator's session
that holds the journal's lock is the thing to end.

## What is refused

These routes are served:

- `GET /v1/health` and `GET /v1/status`
- `POST /v1/previews`
- `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{job}`
- `POST /v1/jobs/{job}/upgrade`, `GET /v1/jobs/{job}/upgrade-receipt`
- `GET /v1/runs`, `GET /v1/jobs/{job}/runs`, `POST /v1/jobs/{job}/runs`, `GET /v1/runs/{run}`

Every other route of the control API answers `501` with the code `store_route_unsupported`
before it reads anything: among them the journal (`GET /v1/events`, `GET /v1/jobs/{job}/events`),
the run chain, input and selected profiles, triggers and occurrences, a run's retry, diagnostics,
verification and brief, the execution binding, and the input preparation and background
connection routes.

Also refused with this kind: manual admission (above), and any `store` member other than
`{"kind":"postgresql","connection":...}`. `jpack-runner store` takes `migrate` and nothing else;
removing one scope's rows is not built.

## What stays on the host's disk

- **Attempt directories** (`attempts/` in the state directory). The kind takes an attempt
  directory that is missing to be unknown, never to hold no audit record, so a run whose
  directories are gone is not evaluated again. The retry route is refused with this kind in any
  case: a caller submits a new run.
- **The pinned Runtime** (`runtimes/<digest>`). A Runner pins the Runtime it was started with. A
  release made with another Runtime fails its runs closed, as on the sqlite kind; a new host must
  be started with the same Runtime the held jobs' releases name.
- **A preview's scratch directory** (`releases/<id>/`), which nothing reads after the preview.

## Backups

The database holds the queue, the jobs, the runs, the releases, the journal and the run chain.
Back it up with the server's own tools. Row security is forced on every table, the owner
included: `pg_dump` run as a role that row security holds stops with "query would be affected by
row-level security policy" (or, with `--enable-row-security`, dumps no rows). Take a dump as a
role that bypasses row security, kept for backups and never named to the Runner, which refuses
such a login at start.

Restoring a store from a backup is not tested. A restored store holds the claims and leases it
held when it was dumped: a run held running in it is interrupted by the tick once its lease has
run out.

The state directory's attempt directories are scratch for the host that ran them; the pinned
Runtime is made again at start from the configured `runtime`.

## Upgrades

Each release ships its migration set and its digests. Before starting a newer `jpack-runner` on a
store, run its `store migrate`; a Runner refuses a store whose set is not its own, an older set
and a newer one alike. A migration only adds: no table is dropped or truncated.
