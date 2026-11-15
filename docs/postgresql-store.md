# The postgresql store kind

A Runner in [service mode](../README.md#service-mode) can keep its queue and records in a
PostgreSQL 16 database instead of the SQLite database under its state directory. This page is for
the operator who sets it up. The design, with every decision and its reasons, is
[docs/design/store-postgresql.md](design/store-postgresql.md); the decision record is
[ADR 0001](adr/0001-second-storage-kind.md).

SQLite stays the default. A Runner started without a `store` member, and the companion Desk
starts, keep the sqlite kind exactly as before.

**This kind serves part of the control API.** It holds the routes that make a job and run it, its
triggers, event and record deliveries, occurrences, a run's retry, the installation's input and
selected profiles, the journal and the run chain, and refuses every other route with
`501 store_route_unsupported`
(below). Several Runners may share one store, each on a host of its own or side by side, within
the limits [below](#several-runners-on-one-store).

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
the versions it applied, `{"applied":["0001","0002","0003"]}` the first time and `{"applied":[]}`
on a second run, which changes nothing.

**Before starting a new release, run its `jpack-runner store migrate`.** A release may ship
migration files the store does not hold yet, and the Runner refuses to open a store whose applied
migrations are not exactly its own set (below). The command applies the files after the ones
recorded, and nothing else: on a store the previous release migrated, it prints the new versions,
`{"applied":["0003"]}` from a store at `0002`. Name the same logins as the first time.

It refuses, and applies nothing more:

- a database that holds a table outside the store's schema and the system's;
- a file recorded under another digest (nothing after it is applied);
- a recorded migration this `jpack-runner` does not ship;
- a request or worker login other than the ones the grants were first made to: replacing a login
  is not a migration.

The grants file (`0002_grants.sql`) gives the two run-time logins `USAGE` on the schema and the
table privileges of the design's section 7, and no privilege on a sequence, and no `DELETE`,
`TRUNCATE`, `REFERENCES` or `TRIGGER`: at run time the Runner deletes, truncates and drops nothing.
A later file that names the logins (`0003_held_routes.sql`, the policies of the routes held in
this release) is applied only with the logins the grants were made to, and records them too.

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
- the owner the configuration names does not own the schema, or a relation, function, operator or
  type in it ("not owned by the owner role the configuration names", naming the object). Every
  pooled connection searches the system's catalog before the schema, so that nothing made in the
  schema hides a built-in;
- a table of the schema does not have row security both enabled and forced ("does not have row
  security enabled and forced", naming the table): `NO FORCE ROW LEVEL SECURITY` set by hand is
  refused;
- either run-time login holds `DELETE`, `TRUNCATE`, `REFERENCES` or `TRIGGER` on a table of the
  schema, `REFERENCES` on any column of one, `SELECT`, `UPDATE` or `USAGE` on a sequence of the
  schema, or `CREATE` on the schema ("holds DELETE, TRUNCATE, REFERENCES or TRIGGER ...", naming
  the login, the privilege and the table or sequence). What counts is what the login can use: privileges
  granted to it, to `PUBLIC`, or to any role it is a member of, whether it inherits that role or
  may only `SET ROLE` to it, a predefined role such as `pg_write_all_data` and a default privilege
  among them. Revoke the grant, or the membership, and start again;
- a connection of either pool does not hold the search path or a time bound the Runner starts it
  with ("does not hold a setting the Runner starts it with", naming the setting and the login): a
  connection pooler between the Runner and the server that drops start parameters is not
  supported;
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
journal also shows a request the entries of no scope that name no job (the Runner's own entries
and previews), as the sqlite kind does. A refused job creation names a release and its caller,
not a job, and is journaled under no scope: only a request that names no scope reads it. On the
sqlite kind a job's journal also shows the refused creations of its release.

The installation's rules (a release makes one job, a pack or dialect identity is never reused
with other bytes, at most 128 triggers, the queue's limits, one trigger per cloud binding) are
held across every scope, by keys and counter rows no policy narrows, and answered as on the
sqlite kind. A request held to one scope can learn that some scope holds a release, an identity
or a cloud binding, never which. Journal sequences are shared across scopes: a page shows gaps where other
scopes' entries are.

## Claims and leases

There is no process-wide lock. A run is held by its claim: a token and a lease of 60 seconds,
compared with the database's clock when each write's statement starts, so a finish or a renewal
that waited for another writer past its lease's end writes nothing. The Runner renews the lease
every 15 seconds while the attempt runs. When a renewal fails, conflicts, or takes longer than 15
seconds, or none has succeeded within 30 seconds of the last that did, the Runner stops the
attempt's Runtime, writes nothing more for that run, and stops its dispatcher: `GET /v1/health`
then reports it unhealthy, and the Runner needs a restart. A run left running by a Runner that
stopped is interrupted by the dispatcher's tick once its lease has run out, about a minute later,
not at the next start.

Runners on one store may read the same queued run. The one whose write comes second, a claim or
the end of a run that expired in the queue or names a profile set aside, finds the run no longer
queued: it works nothing, stays healthy and reads the queue again at once.

A trigger's occurrence is held the same way. A Runner claims an occurrence, with a token and a
lease of 60 seconds by the database's clock, before it dispatches it into a run and before each
step of its preparation, and every write it makes of the occurrence is refused once the claim is
gone: taken by another Runner after the lease ran out, or cleared by a cancellation or a
reconciliation, which always win. A Runner whose write is refused writes nothing and goes on. An
occurrence held `preparing` while its sources are acquired has its lease renewed every 15 seconds,
and a Runner whose renewal fails stops that acquisition and its preparation worker, and
`GET /v1/health` reports it unhealthy, as for a run. An occurrence
left by a Runner that stopped is handed to another one lease later: a preparation in progress is
failed, `Source acquisition was interrupted`, and not repeated; one waiting to be dispatched, or
for its next step, is taken up, and a run already submitted for it is found again by its request
key, not made twice.

A Runner's start interrupts only the runs, and fails only the preparations, whose lease has run
out: work another Runner holds is left to it.

## Time bounds

Every connection of the two run-time pools starts with three settings. They are the session's
start parameters, so they override what the connection string or the login's own settings name.
At start the Runner reads them back, with the search path, on a connection of each pool, and
refuses to open the store when one is not held (below):

| Setting | Value | Why |
|---|---|---|
| `lock_timeout` | 5 s | Every write first takes the lock of `journal_head`, so a session that holds it holds up every writer. A statement that waits longer than 5 s for a lock is refused. 5 s is a third of the 15 s between renewals: a renewal that meets a held lock is refused within 5 s of its start and tried again until its deadline, well inside the 30 s after which the Runner stops the attempt in any case. |
| `statement_timeout` | 30 s | Above the lock bound, so that a wait for a lock is refused as one; a bound on any statement that holds a pooled connection. |
| `idle_in_transaction_session_timeout` | 10 s | A session left idle inside a transaction keeps the journal's lock. The server ends it after 10 s, so a writer behind it meets at most two lock refusals. The Runner's own transactions read no file and make no network call inside a transaction, so they are not idle that long. |

Each renewal of a claim is also held to a deadline of 15 s, the interval between renewals: one
refused for a lock wait is tried again until then, and one not done by then is rolled back, its
statement cancelled on the server, and the claim is lost, as a renewal that took longer than 15 s
already was. An attempt that ends while a renewal is in flight waits for it at most that long.

A write refused for one of these bounds writes nothing. Its error names the bound: "waited longer
than its lock bound (lock_timeout)", "ran longer than its bound (statement_timeout)", "stayed idle
inside a transaction longer than its bound (idle_in_transaction_session_timeout)" or "did not end
within its deadline". A request answers `500` with the code `store_error`, as for any other store
failure, and is not run again.

The dispatcher takes a lock wait for a store that is busy, not failing, and stays healthy:

- a claim, the end of a run that expired in the queue or names a profile set aside, and the tick
  that interrupts runs whose lease ran out make nothing; the run stays queued, and a later tick
  tries again;
- a renewal is tried again until its deadline, and the claim is lost only when the lock is held
  past it;
- the claimant's finish is made again while its claim's lease is live. Its claim token and lease
  fence it, so it lands once or not at all.

A finish still refused when the lease has run out, and any refusal for the statement or idle
bound or the renewal's deadline, stops the dispatcher, as any store failure does, and
`GET /v1/health` reports the Runner unhealthy. While an operator's session holds the journal's
lock, no run is claimed or finished: that session is the thing to end.

## What is refused

These routes are served:

- `GET /v1/health` and `GET /v1/status`
- `POST /v1/previews`
- `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{job}`
- `POST /v1/jobs/{job}/upgrade`, `GET /v1/jobs/{job}/upgrade-receipt`
- `GET /v1/runs`, `GET /v1/jobs/{job}/runs`, `POST /v1/jobs/{job}/runs`, `GET /v1/runs/{run}`
- the journal, `GET /v1/events` and `GET /v1/jobs/{job}/events`, and `GET /v1/run-chain`
- a job's triggers, `GET` and `POST /v1/jobs/{job}/triggers`, `POST /v1/jobs/{job}/triggers/preview`,
  and `POST /v1/triggers/{trigger}/state` and `/rotate-key`
- deliveries, `POST /v1/triggers/{trigger}/events`, `GET /v1/triggers/{trigger}/occurrences/{occurrence}`,
  `POST /v1/triggers/{trigger}/deliveries` and `GET /v1/triggers/{trigger}/deliveries/{event}`
- occurrences, `GET /v1/jobs/{job}/occurrences`, and `POST /v1/occurrences/{occurrence}/cancel` and
  `/reconcile`
- a run's retry, `POST /v1/runs/{run}/retry`
- the installation's profiles, `GET` and `POST /v1/input-profiles` and `/v1/selected-profiles`, which
  take no scope

A trigger another scope holds answers an event credential as an unknown trigger does, `401
invalid_trigger_token`; every other record another scope holds answers `404`, as one that does not
exist. The schedule, file and cloud ticks and the dispatch of occurrences into runs work for every
scope, on the worker login.

Every other route of the control API answers `501` with the code `store_route_unsupported`
before it reads anything: a run's diagnostics and verification, the briefs of a job or a run,
the execution binding, `GET /v1/background-connections`, and the input preparation routes
(`POST /v1/inputs/next`, `/v1/inputs/preview` and `/v1/selected-inputs/next`).

Also refused with this kind: manual admission (above), and any `store` member other than
`{"kind":"postgresql","connection":...}`. `jpack-runner store` takes `migrate` and nothing else;
removing one scope's rows is not built.

## What stays on the host's disk

- **Attempt directories** (`attempts/` in the state directory). The kind takes an attempt
  directory that is missing to be unknown, never to hold no audit record, so a run whose
  directories are gone is not evaluated again: its retry is refused, `409 run_not_retryable`, and
  a caller submits a new run.
- **The pinned Runtime** (`runtimes/<digest>`). A Runner pins the Runtime it was started with. A
  release made with another Runtime fails its runs closed, as on the sqlite kind; a new host must
  be started with the same Runtime the held jobs' releases name.
- **A preview's scratch directory** (`releases/<id>/`), which nothing reads after the preview.

## Several Runners on one store

Several Runners may share one store, on one host or on several. Give each a state directory of its
own, and the same `store` member, workspace, owner and input root: the store's identity is
checked at every start. Any of them may serve a request. One due time of a schedule trigger makes
one occurrence whichever Runners tick it, and each queued run and each occurrence is worked by one
Runner at a time ([Claims and leases](#claims-and-leases)). A request that changes a trigger while
another Runner's tick writes it answers `409 trigger_changed`: read the trigger again and send the
request again, at the same revision.

The Runners must hold to these:

- **One release.** Every Runner of one store runs the same `jpack-runner` release. To upgrade,
  stop every Runner of the store, run the new release's `store migrate`, then start the new
  release's Runners. A migration does not stop a Runner that is running, and a Runner of an
  earlier release keeps no claim on an occurrence and no version on a trigger.
- **Clocks within 1 second of each other.** Leases use the database's clock. These use the wall
  clock of the Runner that decides: a schedule's due time and whether it was missed, a watched
  file's stability, an occurrence's or a run's expiry in the queue, a preparation's deadline and
  its checks, and the time of an acquisition's next attempt. Keep the hosts' clocks within 1
  second of each other with a time service: each of these is then off by at most that second. The
  times of the journal's entries come from several clocks, and need not follow the journal's
  order.
- **The same project and Runtime on every host.** Each Runner reads the project's files, and pins
  its Runtime, on its own host: give every Runner of the store the same image.
- **Profiles registered through one Runner.** A Runner reads the installation's input and selected
  profiles at its start, and a registration writes them from its own copy. Register profiles
  through one Runner, and start the others again after it: another Runner does not see the new
  profile until it starts again, and a registration on a Runner started before another Runner's
  registration writes that registration out of the store.

What several Runners do not change, or do not do:

- **A missed schedule.** A due time is missed, and the trigger's `missed` rule applies (`skip`, or
  `latest`), only when no Runner of the store ticked it within 30 seconds of it: every Runner
  stopped, unhealthy, or refused by the store. One Runner stopping misses no due time while
  another runs.
- **Attempt directories stay on their host.** A retry of an interrupted run is refused,
  `409 run_not_retryable`, on a host that does not hold every attempt directory of the run: a run
  interrupted on one host can be retried only there, if its directories are still there; otherwise
  its caller submits a new run. A run's diagnostics, refused on this kind for now, are read only
  from the serving host's directories.
- **File triggers.** A file trigger reads its watched path on whichever Runner ticks. On several
  hosts the path must be on storage they share, with `stableSeconds` of 2 or more; a file that
  differs between hosts reads as a change each time the tick moves from one to another.
- **No shared storage.** Nothing on a host's disk is shared by the Runners or copied to the
  database.
- **The sqlite kind** keeps one Runner per state directory, under its lock.

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
store, stop every Runner of the store and run the newer release's `store migrate`; a Runner
refuses a store whose set is not its own, an older set and a newer one alike, but only at its
start. A migration only adds: no table is dropped or truncated. A store of an earlier release is
refused until `0004_several_runners.sql` is applied.
