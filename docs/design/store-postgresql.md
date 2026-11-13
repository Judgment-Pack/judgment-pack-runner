# A second storage kind on PostgreSQL

Issue #244. Status: design. Nothing here is built; the slice that builds it follows this note. The
decision is recorded in [ADR 0001](../adr/0001-second-storage-kind.md).

A Runner keeps its state in one SQLite database and a few directories under its state directory
([store-interface.md](store-interface.md), section 9). This note fixes the second storage kind,
on PostgreSQL 16, so that building it is mechanical: its schema, its roles, its migrations, its
row policy per scope identifier, what stays on local disk and what it costs, the driver, and the
test that answers each of the issue's database conditions. SQLite stays the default kind,
unchanged byte for byte (`store_sqlite_bytes_test.go` holds it).

**Every decision a maintainer must make is marked D1 to D12, with its options; the recommendation
is the first option.** Section 12 lists them.

## 1. What the kind is

A kind registered in `storeKinds` as `postgresql`, implementing the store contract
(`store_contract.go`) and passing the conformance suite every kind runs. It keeps no state in the
process beyond its connection pool: no process-wide lock, no cache of records (issue #245, several
Runners on one store, must not be made harder).

**D1. How a Runner selects it.** (a) The [service mode](service-mode.md) configuration file names
the kind and a file holding the connection string, read once at start; the local boot line keeps
SQLite and gains nothing. (b) A boot line flag. (c) An environment variable. (a) keeps credentials
out of argv and the environment, and keeps the single-owner local pilot as it is.

## 2. The contract it implements

Everything a Runner keeps goes through the contract (store-interface.md, section 9.5, and the
slices of #244 after it). The kind implements each operation with one statement or one
transaction, as the sqlite kind does, with three differences the contract already allows:

- **Serializable transactions that retry.** `update(fn)` runs at `SERIALIZABLE` and runs `fn` again
  on a serialization failure (`40001`) or a deadlock (`40P01`), a bounded number of times, then
  reports `durability`. So `fn` must read nothing but the store and do nothing it cannot do twice:
  the Runner's functions already decide inside `fn` and act after it, and a brief's command is now
  read before its transaction (store-interface.md, section 8). **D2.** (a) Keep the closure and
  retry it. (b) Replace `update(fn)` by one named operation per transaction the Runner makes,
  each a server-side unit. (a) keeps every transaction the Runner has, statement for statement;
  (b) is a redesign of the Runner's write paths.
- **Faults.** `23505` (unique) is `taken`, `23514` (check) is `refused`, `42501` (insufficient
  privilege) and a row policy's refusal are `refused`, no row is `absent`, and the rest are
  `durability` for a write and `unreadable` for a read.
- **The scope a transaction is held to** (section 5): the contract gains a way to say which scope
  a transaction is for. The sqlite kind ignores it; the in-memory kind enforces it, so the suite
  tests the policy on every kind that enforces one.

**The claim's fence without a lock.** The sqlite kind's lock proves at start that no other
dispatcher is running, so a restart ends every run left running. A kind without a process-wide
lock cannot know that. **D3.** (a) A claim records a holder and a lease time in columns of the
kind's own (`claimed_by`, `lease_until`), outside the run's record; the dispatcher renews the lease
while the Runtime runs; a restart interrupts only runs whose lease has run out. (b) One row per
running Runner with a heartbeat, a start refused while another's is fresh. (c) A session advisory
lock. (a) is the lease with fencing section 4 of the store note says a kind beyond one host needs,
and it is the step #245 needs; its cost is a renewal write per running run, and a run left by a
Runner that died is interrupted one lease later, not at the next start. (c) is a process-wide
lock, which the issue rules out.

## 3. The schema

One schema, `jpack_runner` by default, owned by the owner role, in a database of the component's
own (condition a). Every table of section 9.1 of the store note, by the same name, with the same
columns and keys, and these changes:

| Change | Tables | Why |
|---|---|---|
| `bigint GENERATED ALWAYS AS IDENTITY` for SQLite's `AUTOINCREMENT` | `jobs.seq`, `runs.seq`, `triggers.seq`, `occurrences.seq`, `events.sequence` | The order of making, never reused. A run-time login needs no privilege on the sequence to insert (section 5). |
| Records as `text`, never `jsonb` | every `record`, `events.by_actor`, `concerns`, `detail` | The contract hands records as their encoded bytes; `jsonb` reorders members and drops duplicates. A record reads back as written. |
| `run_chain.line`, `briefs.record` as `bytea` | | SQLite keeps them as blobs. |
| A `scope text NOT NULL` column | every table with scoped rows (section 5) | The row policy compares a column, never a member inside a record. |
| `claimed_by`, `lease_until` | `runs` | D3. |
| `attempt_volumes(run, attempt, volume, scope)` | | D11. |
| `schema_migrations(version, digest, applied_at)` | | Section 4. |
| No `workflow_executions`, `workflow_artifacts`, `manual_attempts` | | Section 9 of the store note: the workflow store is reached by no route; manual admission is single-host and stays on SQLite (D4). |

**D4. Manual admission.** (a) It stays on the sqlite kind: a Runner configured with the
manual-admission gate refuses the second kind at start. (b) Its journal and preparation are ported.
Manual admission holds a host's executor lease and a prepared database file; (a) keeps it where it
is single-host by design.

The scope column is the record's own scope, never a second copy that can differ. **D5.** (a) A
stored generated column, `scope text GENERATED ALWAYS AS (coalesce(record::jsonb->>'scope',''))
STORED` (`detail` for `events`), on the tables whose rows carry their scope; and for the tables
whose rows do not (`occurrence_keys`, `run_chain`, `briefs`), a column the insert fills from the
row it belongs to, in the same statement (`INSERT ... SELECT ..., o.scope FROM occurrences o WHERE
o.id=$1`), with a foreign key on `(id, scope)` that holds the two together. (b) A column the kind
writes from the Go record. (a) cannot diverge from the record and needs no change to the
contract's arguments; its cost is a JSON parse per write.

The SQLite expressions that have no PostgreSQL equal are the kind's to translate, each with a
conformance case already: `json_extract(record,'$.x')` is `record::jsonb->>'x'` (text compared to
text, as `memberText` states for the in-memory kind); the listings' search folds ASCII case only,
as SQLite's `lower()` does, so it is `translate(x, 'ABC…Z', 'abc…z')`, not `lower()`, which folds
every script; and the cloud binding's unique index is an expression index on the same members.

## 4. Roles and migrations

Three kinds of role, created by the operator beforehand and passed to the kind by name (condition
b); the kind creates none (condition d):

| Role | Attributes | Holds |
|---|---|---|
| The owner, `jpack_owner` by default | `NOLOGIN` | Owns the schema and every object in it. |
| The migration login | `LOGIN`, a member of the owner with `INHERIT FALSE, SET TRUE` | Nothing of its own: it reaches the owner only by `SET ROLE` (condition b). |
| The run-time logins: the request login and the worker login (section 5) | `LOGIN`, none of `SUPERUSER`, `CREATEROLE`, `CREATEDB`, `BYPASSRLS` | `USAGE` on the schema and the table privileges of section 6, granted by the migration. |

At start the kind reads its run-time login's attributes and memberships, and refuses to start if
it is a superuser, bypasses row security, can create roles or databases, or is a member of the
owner: a login that could read every scope is never taken for one that cannot.

**Migrations** are plain SQL files, `internal/runner/postgresql/migrations/NNNN_name.sql`, applied in
order by one command, `jpack-runner store migrate`, that release tooling runs as a release step
(condition c). The command connects as the migration login, runs `SET ROLE` to the owner, and
applies each file not yet recorded in `schema_migrations` in its own transaction, recording its
version and digest. A file recorded with another digest is refused, and nothing after it is
applied. A second run applies nothing and changes nothing (condition c). No file creates a role,
alters one, or stores a role's or the database's setting: every setting a session needs is set per
transaction with `SET LOCAL` or `set_config(..., true)` (condition d). The command refuses a
database that holds a table in any schema other than the kind's and the system's (condition a).

**D6. Grants to roles named at migration.** (a) A grants file, part of the set and its digest, with
two placeholders (the request login and the worker login) the command fills with quoted
identifiers; the digest covers the file as shipped. (b) Grants to fixed group roles the operator
grants the logins into. (c) The operator runs the grants by hand. (a) keeps the files plain and
digested and the role names the operator's.

Each release ships its set with `migrations.sha256`, the digest of every file in order, and its
release notes state the versions it upgrades from (condition i). A migration only adds: a column
is added with a default, a table is never dropped or truncated (condition g).

## 5. Row security per scope identifier

**Enabled and forced on every table** (`ENABLE` and `FORCE ROW LEVEL SECURITY`), so that the owner
is held to the policies too (condition f). Every table with scoped rows has its `scope` column
(section 3): `jobs`, `job_creations`, `job_revisions`, `triggers`, `trigger_revisions`,
`occurrences`, `occurrence_keys`, `runs`, `run_chain`, `events`, `briefs`, `attempt_volumes`.
`releases`, `metadata` and `schema_migrations`
hold no scoped row.

**The setting.** A transaction made for a request is held to one scope by
`set_config('jpack.scope', 's:' || $scope, true)`, at its start, through the request login. The
prefix is there because a custom setting set once in a session reads `''` in every later
transaction, not unset: so `''` and unset must hold a transaction to no row, and the none scope
needs a value of its own, `s:`. The policy on each scoped table, for the request login:

```sql
USING (current_setting('jpack.scope', true) = 's:' || scope)
WITH CHECK (current_setting('jpack.scope', true) = 's:' || scope)
```

A transaction that set no scope reads and writes no scoped row. **Unscoped rows** (a job created
under none, and every record about it; the Runner's own journal entries, a preview's, a refused
creation's) are the none scope: a transaction held to `s:` reaches them, one held to any other
scope does not, as a request that names no scope reaches today only a record that carries none
(scope-identifiers.md, section 3). `releases` is readable by the request login, which a job's
creation reads, and written only by the worker login; `metadata` likewise, except the schema row
(section 6).

**Work for every scope** (the dispatcher, the trigger ticks, the restart, preparation, the chain,
the health route's counts, and a store-wide listing or journal read that names no scope, which
lists every scope's records today: scope-identifiers.md, section 5) runs on the worker login,
whose policy on each table is `USING (true) WITH CHECK (true)`. **D7.** (a) A worker login of its
own, with policies that admit every scope; the request login is held to the scope each
transaction sets. (b) One login, and a reserved setting value for every scope. (c) A dispatcher
that sets each scope in turn. (b) is ruled out: a scope identifier may hold any character but a
few (scope-identifiers.md, section 1), so no value is safe to reserve, and a login that can widen
its own setting holds nothing. (c) still needs the list of scopes, read across scopes. A scope
identifier is a label and a filter, not an authorization boundary (scope-identifiers.md): the
policy holds a request's transaction to the scope the Runner set for it; which scopes a caller may
name is not this issue's.

**Sequences, views, functions.** Identity columns take their next value without a privilege on
their sequence, and neither run-time login is granted one, so neither reads a sequence's value
(condition f). Sequence values are shared across scopes: a reader held to one scope sees its
entries' journal sequences, whose gaps say other entries exist, never what they are. **D8.** (a)
Document it. (b) A sequence per scope, which changes the journal's cursor. The kind defines no
view; one added later is `security_invoker`, so the reader's policy applies. It defines no
function, so no `SECURITY DEFINER` function (condition e); one added later is listed here with a
pinned `search_path` and its reason.

**Keys across scopes.** A unique key over installation-wide values answers `taken` whatever scope
holds the row: one job per release (`jobs.release_id`, `job_creations`), and one trigger per cloud
binding. A request held to one scope learns that some other scope holds that release or that
binding, never which. **D9.** (a) Document it: the release and the binding are named by the caller
and are installation-wide already. (b) Keys per scope, which changes what the Runner refuses.

## 6. Privileges, append-only tables, removal

| Table | Request login | Worker login | Append-only |
|---|---|---|---|
| `releases` | `SELECT` | `SELECT`, `INSERT` | Yes |
| `metadata` | `SELECT`; `UPDATE` of the schema row only, to a later schema | `SELECT`, `INSERT`, `UPDATE` | No |
| `jobs`, `triggers`, `occurrences`, `runs`, `briefs` | `SELECT`, `INSERT`, `UPDATE` | `SELECT`, `INSERT`, `UPDATE` | No |
| `job_creations`, `job_revisions`, `trigger_revisions`, `occurrence_keys`, `run_chain`, `events` | `SELECT`, `INSERT` | `SELECT`, `INSERT` | Yes |
| `attempt_volumes` | `SELECT` | `SELECT`, `INSERT` | Yes |
| `schema_migrations` | `SELECT` | `SELECT` | Yes; written by the migration only |

No run-time login holds `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER` or ownership: the Runner
deletes nothing, and no table is truncated or dropped at run time (condition g). Append-only is
held by the privileges, not only by the code.

**Removing one scope's rows** (condition h; the removal issue states what it is for) is one
command, `jpack-runner store remove-scope --scope <scope identifier>`, run by an operator as the
migration login, not at run time. It runs `SET ROLE` to the owner, holds the transaction to the
scope with the setting above, and deletes from every scoped table in the order of their foreign
keys, in one transaction. The owner is held by forced row security to a policy keyed on the same
setting, so the removal reaches that scope's rows and no other's. **D10. The chain of runs.** The
chain is one chain for the installation: each entry names the previous entry's digest, so removing
a scope's entries breaks every later link. (a) The removal keeps the scope's chain entries (each
holds a run id and its audit record's digest, not the record) and says so. (b) The removal is
refused while the chain holds an entry of the scope. (c) A chain per scope, which changes the
chain's format and the specification. (a) removes every record and keeps the chain checkable.

## 7. What stays on local disk

| Path (store note, section 9.2) | In the second kind | Cost |
|---|---|---|
| `runner.lock` | Not used: D3. | |
| `releases/<id>/` | A scratch directory for the preview, as it is now: nothing reads it after the preview, and the release's record is in the database. | None. |
| `attempts/<run>[.<n>]/` | A scratch directory for the attempt's Runtime, as now, with the retry check made to fail closed (below). | An interrupted run whose attempt ran on a volume that is gone cannot be retried by the retry route; its caller submits a new run. Diagnostics of such an attempt read nothing. |
| `runtimes/<digest>` | Rebuilt at start from the pinned digests (below). | The image ships every Runtime a held release names. |

**The attempts and the retry check.** `attemptsHoldNoAudit` (`retries.go:406`) refuses to evaluate
an interrupted run again when an attempt's directory holds an audit trail, and takes a directory
that does not exist to hold none. On another host, or on a new volume, every directory is gone, and
that reading would let a retry evaluate again a run whose Runtime may have written an audit record:
a refusal turned into a pass. So the second kind's Runner keeps, at the attempts root, a file
naming the volume by a random identifier made when the root is first made, and records it for each
attempt in the claim's transaction, in a table of the kind's own (`attempt_volumes`, outside the
run's record, which is unchanged). A retry whose interrupted attempt names another
volume, or none, is refused with `errAuditUnknown`, as an unreadable directory is now. **D11.** (a)
That. (b) The attempt directories on a durable shared volume, which keeps local state the issue
removes. (c) The Runtime's audit trail copied into the database as it is written, which needs the
Runtime's cooperation. (a) is the smallest that keeps the refusal; it costs the retry of a run
interrupted on a lost volume.

**The pinned Runtime.** A release names the digest of the Runtime it was made with, and every
evaluation reads `runtimes/<digest>` and refuses a file that is missing or changed
(`runtime.go:357`). At start a Runner pins the Runtime it was given. The second kind's Runner pins,
in addition, each executable of a read-only directory the configuration names, by its digest; a
release whose digest is not among them fails its runs closed, as a missing file does now, and the
health route names the digests held releases need that are not pinned. **D12.** (a) Rebuilt at
start from executables the image ships, by digest. (b) The executables in the database, up to
128 MiB each. (c) An object store behind a small interface. (a) adds no storage and no interface,
and keeps the digest check where it is; its cost is that an image must carry every Runtime a held
job's release names, or the job is revised to a release of a Runtime it carries.

## 8. The driver

**Decision: `github.com/jackc/pgx/v5`, through its `database/sql` adapter (`pgx/v5/stdlib`).** Not
built in this slice. Its licence is MIT, with a licence file at the module root, as
`tools/third_party_notices.py` requires of every linked module; it is maintained and pure Go, so
the release programs stay statically linked. It brings a handful of modules (its connection
string and password file readers, and `golang.org/x/crypto`, `x/sync` and `x/text`), each with a
licence file; the slice that adds it runs the notices tool and records the list. The alternative,
`github.com/lib/pq`, is MIT and lighter, but its own documentation points new work to pgx. Using
`database/sql` keeps the kind's statements, scans and fault mapping shaped as the sqlite kind's.

## 9. The conditions and their tests

Each runs in CI against a PostgreSQL 16 service container whose administrator is not a superuser,
with the three roles created as an operator would; each is skipped, and says so, when no such
database is named to the test.

| Condition | Test |
|---|---|
| a. A database of its own | `TestPostgreSQLMigrationKeepsToItsOwnSchema`: the objects after migration are the kind's, in its schema; a database holding another table is refused before anything is made. |
| b. Three roles, passed by name | `TestPostgreSQLRolesHoldOnlyWhatTheyNeed`: the owner cannot log in; the migration login reaches it only by `SET ROLE`; each run-time login holds only section 6's privileges and none of the four attributes; a start with a login that bypasses row security, or is the owner's member, is refused. |
| c. Migrations as one command, idempotent | `TestPostgreSQLMigrationIsIdempotent`: a second run applies nothing and the catalog is unchanged; a file recorded under another digest is refused. |
| d. No role made, no setting stored | `TestPostgreSQLMigrationStoresNoRoleOrSetting`: `pg_roles` and `pg_db_role_setting` are unchanged by the migration; no file holds `CREATE ROLE`, `CREATE USER` or `ALTER ... SET`. |
| e. No definer function | `TestPostgreSQLDefinesNoSecurityDefinerFunction`: the schema holds no function with `prosecdef`. |
| f. Row security per scope | `TestPostgreSQLHoldsATransactionToItsScope`: every table has row security enabled and forced; a request login held to one scope reads and writes no row of another, in every table, and no sequence's value; a transaction that set no scope reaches no scoped row, and a scope set in one transaction is not there in the next. |
| g. No truncation or drop at run time | `TestPostgreSQLRuntimeLoginsCannotRemove`: neither run-time login can delete, truncate or drop, and the append-only tables refuse an update. |
| h. One call removes a scope's rows | `TestPostgreSQLRemovingAScopeLeavesNoneOfItsRows`: after `remove-scope`, no table holds a row of the scope (D10's chain rule aside), and every other scope's rows are as they were. |
| i. A digested set per release | `TestPostgreSQLMigrationSetMatchesItsDigest`: the files shipped match `migrations.sha256`, and the release notes name the versions upgraded from. |

Beside them, the kind runs `TestStoreKindsConform` with the sqlite and in-memory kinds, and
`unit-tests.sh` requires it to run and pass when a database is named.

## 10. What the slice after this one builds

The contract's scope for a transaction (section 2), the claim's lease (D3), the kind, its
migrations and commands, the attempts volume identity (D11), the pinned Runtime directory (D12),
the CI service container, and the tests of section 9, in that order. SQLite's bytes and schema are
not changed by any of it: the columns of D3 and the table of D11 are the second kind's own.

## 11. Not in this issue

Several Runners on one store (#245), beyond not making it harder; which scopes a caller may name;
the removal issue's retention rules; a third kind.

## 12. Decisions for a maintainer

Each with the recommendation first.

| | Decision | Options |
|---|---|---|
| D1 | How a Runner selects the kind | Service-mode configuration file with a connection file; boot line flag; environment |
| D2 | How transactions compose | The closure, retried on serialization failure; named operations |
| D3 | The claim's fence without a lock | A lease per claim; a heartbeat per Runner; an advisory lock (ruled out) |
| D4 | Manual admission | Stays on SQLite, refused with the second kind; ported |
| D5 | The scope column | Generated from the record, or filled from the parent row; written by the kind |
| D6 | Grants to named roles | A digested grants file with two placeholders; fixed group roles; by hand |
| D7 | Work across scopes | A worker login; a reserved setting (ruled out); a dispatcher per scope |
| D8 | Shared sequences | Documented; per-scope sequences |
| D9 | Installation-wide unique keys | Documented; keys per scope |
| D10 | The chain on removal | Kept and said so; removal refused; a chain per scope |
| D11 | The attempts and the retry check | A volume identity per attempt, failing closed; a durable shared volume; the trail in the database |
| D12 | The pinned Runtime | Rebuilt at start from shipped executables; in the database; an object store |
