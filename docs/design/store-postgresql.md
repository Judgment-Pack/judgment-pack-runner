# A second storage kind on PostgreSQL

Issue #244. Status: design. Nothing here is built; the slice that builds it follows this note. The
decision is recorded in [ADR 0001](../adr/0001-second-storage-kind.md).

A Runner keeps its state in one SQLite database and a few directories under its state directory
([store-interface.md](store-interface.md), section 9). This note fixes the second storage kind,
on PostgreSQL 16, so that building it is mechanical: its schema, its roles, its migrations, its
row policy per scope identifier, the order it keeps under concurrent writers, what stays on local
disk and what it costs, the driver, and the test that answers each of the issue's database
conditions and each decision here. SQLite stays the default kind, unchanged byte for byte
(`store_sqlite_bytes_test.go` and `store_sqlite_rows_test.go` hold it).

**Every decision a maintainer must make is marked D1 to D15, with its options; the recommendation
is the first option.** Section 14 lists them. Section 11 lists what the next slice changes in the
Runner before it writes the kind.

## 1. What the kind is

A kind registered in `storeKinds` as `postgresql`, implementing the store contract
(`store_contract.go`) and passing the conformance suite every kind runs. It keeps no state in the
process beyond its connection pools, one per run-time login (section 6): no process-wide lock, no
cache of records (issue #245, several Runners on one store, must not be made harder).

**D1. How a Runner selects it.** (a) The [service mode](service-mode.md) configuration file names
the kind and a file holding the connection string, read once at start; the local boot line keeps
SQLite and gains nothing. (b) A boot line flag. (c) An environment variable. (a) keeps credentials
out of argv and the environment, and keeps the single-owner local pilot as it is.

## 2. The contract it implements

Everything a Runner keeps goes through the contract (store-interface.md, section 9.5), but manual
admission (D4), which only the sqlite kind keeps, with the offline programs that open the sqlite
kind's database (relocation, manual store preparation), the workflow store, which no route
reaches, and the source worker's own database (store-interface.md, section 6). The kind implements each operation with one statement or one transaction, as the
sqlite kind does, with these differences:

- **Serializable transactions that retry.** `update(fn)` runs at `SERIALIZABLE` and runs `fn` again
  on a serialization failure (`40001`) or a deadlock (`40P01`), a bounded number of times, then
  reports `durability`. **D2.** (a) Keep the closure and retry it. (b) Replace `update(fn)` by one
  named operation per transaction the Runner makes, each a server-side unit. (a) keeps every
  transaction the Runner has, statement for statement; (b) is a redesign of the Runner's write
  paths. (a) holds only under the rule below, which the Runner does not keep everywhere today.
- **Faults.** `23505` (unique) is `taken`, `23514` (check) is `refused`, `42501` (insufficient
  privilege) and a row policy's refusal are `refused`, no row is `absent`; a statement that fails
  otherwise is `durability` for a write and `unavailable` for a read (store-interface.md, section
  3), and a record that does not decode is `unreadable`. A `taken` keeps the constraint's name in
  its cause, where the Runner answers two keys apart (D9).
- **The scope a transaction is held to** (section 6): the contract gains a way to say which scope
  a transaction, or a read outside one, is for. The sqlite kind ignores it; the in-memory kind
  enforces it, so the suite tests the policy on every kind that enforces one. A read outside a
  transaction runs at `READ COMMITTED`, its setting and its one statement in one transaction.

**The rule for `fn`.** A retried `fn` must do on its second run what it would have done had that
run been its first. So:

1. `fn` starts from copies of its inputs: it changes no argument, captured variable or record it
   did not make in that run, and it assigns what it hands back only at its end, after its last
   store call, or on a path that returns `errKeepNothing` or an error the kind does not retry.
2. `fn` reads the store, its arguments and the clock, and nothing else: no file, no network, no
   process state another goroutine writes.

These broke it, and were changed before the kind is written (section 11), each with a case that
makes, between the two runs of the twice mode below, the change that shows it
(`TestAClosureRunAgainDecidesFromTheStore`):

- `admitOccurrenceAs` (`triggers_store.go`) wrote through its `*Occurrence`: a full queue or an
  overlap set it `skipped` and dropped its `Input` and `PendingInput`. Its callers' closures
  captured the occurrence: `scheduleTick` and `fileTick` (`triggers_dispatch.go`), `receiveCloud`
  (`cloud.go`) and `admitBoundEvent` (`triggers_store.go`). Run again after the queue drained or
  the overlap ended, it admitted the occurrence skipped, its input gone. It now works on a copy and
  returns the occurrence it recorded; no caller's closure assigns it before its end.
- `admitBoundEvent` kept a full queue's refusal in a captured variable and answered it after
  `update`, though a later run had committed the occurrence: an event admitted was answered
  `429 queue_full`. The refusal is now `fn`'s own answer.
- `retryRunBy` (`retries.go`) read the attempt directories inside `fn` (`attemptsHoldNoAudit`).
  The check is made before the transaction, on the run as read then, and its answer returned
  inside `fn` where it was made, after the replay and the state checks; a run whose attempt inside
  `fn` is not the one checked, or that the read before did not find, is refused with
  `attempt_mismatch`, so that the check and the write concern one attempt.
- `createJobWith` (`store.go`) read the profiles set aside and the selected profiles
  (`refuseSetAsideMapping`), process state a registration writes under `profilesMu`. The refusal
  is worked out before the transaction and returned inside it at the same place, after the
  repeated creation's check, so that a job made before its profile was set aside is still returned
  as it was.
- `upgradeJob` (`job_revisions.go`) assigned the revised job to its input `j`, which `fn` reads:
  run after a commit that failed, it met its own revision and answered `409 job_changed`. And it
  checked each trigger with `validateTrigger`, whose `validateGatewayMapping` reads the registered
  and selected profiles. The revised job is now a variable of its own, assigned at the end;
  `validateGatewayMapping`'s answer is read before the transaction, and each trigger `fn` reads is
  checked against it (`checkTrigger`), refused as before with `trigger_upgrade_incompatible`.
- `submitBy` (`store.go`) answered a replay when any run of `fn` had found one: it now answers one
  only when the kept run did, and assigns its new run at its end.

Closures that assigned a captured variable on every run before reading it, and so kept the rule's
purpose, now also assign only at their end: `configureTriggerBy`'s prior state,
`writeTriggerChange`'s entry, `briefHandler`'s history and `retryRunBy`'s result. The chain's
append in `finishRunIn` (`run_chain.go`) reads the head inside `fn` and keeps the rule; two
finishers meeting at the head is D14.

**The gate.** A kind's twice mode (`store_twice_test.go`) runs `fn` inside a transaction, throws
its writes and its answer away as a serialization failure would, makes the change a test set
between the two runs if any, and runs `fn` again in a transaction of its own, keeping that run.
The conformance suite runs the in-memory kind under the mode as a third kind, `twice`, which
`unit-tests.sh` requires to run and pass beside `sqlite` and `memory`. The Service's flows run the
sqlite kind under it, since a Service opens no other: `TestEveryTransactionIsSafeToRunTwice` runs
the journal's every change of state, an upgrade, a retry, a brief, the registered profiles,
finishes at once and the cases below under it, and fails when a function of the package's source
that calls a store's `update`, found by the call and not listed, did not run there. Each break
above has a case that makes a change between the runs (the queue drained or the overlap ended, an
attempt directory changed, a profile selected or registered, another finisher's entry), since a
function that carries its first run's decision into its second gives the same writes when nothing
changed between them. The kind is not written until they pass.

**D3. The claim's fence without a lock.** The sqlite kind's lock means that, at start, no other
dispatcher is running, so a restart ends every run left running. A kind without a process-wide
lock cannot know that. (a) A lease per claim, as specified below. (b) One row per running Runner
with a heartbeat, a start refused while another's is fresh. (c) A session advisory lock. (a) is
the lease with fencing section 4 of the store note says a kind beyond one host needs, and it is
the step #245 needs; its cost is a renewal write per running run every 15 seconds, and a run left
by a Runner that died is interrupted one lease later, not at the next start. (c) is a
process-wide lock, which the issue rules out.

The lease, as (a) builds it:

- **The claim's token.** A claim writes `claim_id` (16 random bytes, new for each claim),
  `claim_attempt` (the run's attempt) and `lease_until` on the run's row, in the claim's statement,
  fenced by the run's state as now: `UPDATE runs SET state='running', record=$1, claim_id=$2,
  claim_attempt=$3, lease_until=now() + $4 WHERE id=$5 AND state='queued'`.
- **Expiry is the database's clock.** Every comparison with `lease_until` is with `now()` in the
  statement that makes it. No Runner's clock is compared with it or written into it.
- **Every write a claimant makes is fenced by its token.** `putRun` gains an expected claim beside
  its expected state: the contract's change, its `expected` carrying both. A finish, with its chain
  entry and its journal entry, writes only `WHERE id=$1 AND state='running' AND claim_id=$2 AND
  claim_attempt=$3 AND lease_until > now()`; no row is `conflict`, and the transaction keeps
  nothing. A renewal is a new operation, `renewClaim(run, claim)`: `UPDATE runs SET
  lease_until=now() + $1` under the same condition, no row being `conflict`. So a Runner whose lease
  ran out can neither finish nor extend a run that was interrupted and retried elsewhere: attempt
  2's claim holds another token. The sqlite kind takes the token and ignores it, its lock being the
  fence; the in-memory kind checks it, so the suite tests it.
- **Interruption.** The dispatcher's tick, and the restart, interrupt each running run whose lease
  ran out: they read those `WHERE state='running' AND lease_until <= now()`, and write each with
  the token read expected and `lease_until <= now()` again, so a renewal that landed between the
  two keeps the run. An interruption clears the token. A run whose lease is live is its holder's,
  whoever starts.
- **The margin.** The lease is 60 seconds, renewed every 15. A renewal fails when it returns an
  error or `conflict`, or takes longer than 15 seconds. The Runner begins stopping the run's
  Runtime (its process group) 30 seconds, by its monotonic clock, after the start of the last claim
  or renewal that succeeded. That renewal's `now()` is after its start, so the lease the database
  holds runs at least 60 seconds from it, and the Runtime is being stopped at least 30 seconds
  before the lease can run out, whatever the Runner's wall clock says. A Runner that loses a claim
  stops its dispatcher, as a failed finish does now; the run is interrupted one lease later and is
  retried only as D11 allows.

## 3. The schema

One schema, `jpack_runner` by default, owned by the owner role, in a database of the component's
own (condition a). Every table of section 9.1 of the store note, by the same name, with the same
columns and keys, and these changes:

| Change | Tables | Why |
|---|---|---|
| `bigint GENERATED ALWAYS AS IDENTITY` for SQLite's `AUTOINCREMENT` | `jobs.seq`, `runs.seq`, `triggers.seq`, `occurrences.seq` | The order of making, never reused, and under D13 the order of commits. A run-time login needs no privilege on the sequence to insert (section 6). |
| `events.sequence` taken from `journal_head` | `events` | D13. |
| Records as `text`, never `jsonb` | every `record`, `events.by_actor`, `concerns`, `detail` | The contract hands records as their encoded bytes; `jsonb` reorders members and drops duplicates. A record reads back as written. |
| `run_chain.line`, `briefs.record` as `bytea` | | SQLite keeps them as blobs. |
| A `scope text NOT NULL` column | every table with scoped rows (section 6) | The row policy compares a column, never a member inside a record. |
| `claim_id`, `claim_attempt`, `lease_until` | `runs` | D3. |
| `journal_head(last)` and `chain_head(last)`, one row each | | D13, D14. |
| `counts(name, value)`: `triggers`, `active-runs`, `pending-occurrences` | | D9. |
| `pack_identities(pack, version, digest)` and `dialect_identities(dialect, version, digest)`, keyed on their first two columns | | D9. |
| `attempt_volumes(run, attempt, volume, scope)` | | D11: diagnostics only. |
| `schema_migrations(version, digest, applied_at, request_login, worker_login)` | | Section 5; the logins the grants were made to (D6). |
| No `workflow_executions`, `workflow_artifacts`, `manual_attempts` | | Section 9 of the store note: the workflow store is reached by no route; manual admission is single-host and stays on SQLite (D4). |

**D4. Manual admission.** (a) It stays on the sqlite kind: a Runner configured with the
manual-admission gate refuses the second kind at start. (b) Its journal and preparation are ported.
The refusal is the store opening's, for any kind that hands over no manual-admission state
(`openStore`; `TestManualAdmissionStaysOnTheSQLiteKind`), so the second kind needs none of its own.
Manual admission holds a host's executor lease and a prepared database file; (a) keeps it where it
is single-host by design.

The scope column is the record's own scope, never a second copy that can differ. **D5.** (a) A
stored generated column, `scope text GENERATED ALWAYS AS (coalesce(record::json->>'scope',''))
STORED` (`detail` for `events`), on the tables whose rows carry their scope; and for the tables
whose rows do not (`occurrence_keys`, `briefs`), a column the insert fills from the row it belongs
to, in the same statement (`INSERT ... SELECT ..., o.scope FROM occurrences o WHERE o.id=$1`), with
a foreign key on `(id, scope)` that holds the two together (a brief keeps its subject's job or run
in one of two columns, each with its key). (b) A column the kind writes from the Go record. (a)
cannot diverge from the record and needs no change to the contract's arguments; its cost is a JSON
parse per write.

The cast is to `json`, never `jsonb`: a `jsonb` cast converts the whole record and refuses
`\u0000` in any string member, and a run's inputs are the caller's data, so a record the sqlite
kind keeps would fail to insert. `json`'s `->>` converts only the member it extracts, and a scope
identifier holds no control character (scope-identifiers.md, section 1). The same holds for every
expression below.

The SQLite expressions that have no PostgreSQL equal are the kind's to translate, each with a
conformance case already: `json_extract(record,'$.x')` is `record::json->>'x'`, each comparison
made with the member's JSON text (text compared to text, as `memberText` states for the in-memory
kind: `false` where SQLite compares `0`); the listings' search folds ASCII case only, as SQLite's
`lower()` does, so it is `translate(x, 'ABC…Z', 'abc…z')`, not `lower()`, which folds every
script; and the cloud binding's unique index is an expression index on the same members.

## 4. Order under concurrent writers

A reader that follows a cursor forward must never pass an entry that commits later: the journal
promises that a reader that has read through sequence n never later finds an entry at or below n
(activity-journal.md, section 4). SQLite keeps it by running one writer at a time. An identity
column does not: T1 takes 10, T2 takes 11 and commits, a reader reads 11 and moves its cursor past
it, then T1 commits 10, which that reader never reads.

**D13. The journal's order.** (a) Writers commit one at a time: `update` takes the lock of the
`journal_head` table (`LOCK TABLE journal_head IN EXCLUSIVE MODE`) as its first statement, before
the transaction's snapshot is taken, and `appendJournal` takes its sequence from the table's row
(`UPDATE journal_head SET last = last + 1 RETURNING last`). A writer that waits takes its snapshot
after the one before it commits, so it does not fail for it, and sequences follow the order of
commits. (b) The counter row without the lock first: the same order, but a writer that waited on
the row fails with `40001` and runs `fn` again, so a burst of writers can use up D2's retries, and a
finish that does stops the dispatcher. (c) An identity column, each entry recording its
transaction (`pg_current_xact_id()`), and a forward page holding only entries of transactions
older than every one still open (`xid < pg_snapshot_xmin(pg_current_snapshot())`): writers never
wait, and readers lag behind the oldest transaction open in the cluster, an operator's idle session
among them. (a) keeps the promise with the least machinery. Its cost: the installation's writes
serialize, one transaction at a time, as they do on SQLite, and a slow transaction holds up every
other writer, which D2's rule bounds by keeping files and the network out of every `fn`. Reads do
not take the lock.

Every other order a consumer follows, under (a):

- **The chain** (`run_chain.sequence`): its next sequence is the committed tail's plus one, so it
  follows the order of commits whatever D13 decides; D14 keeps two finishers from meeting on it.
- **The listings** of jobs, runs and occurrences page newest first by `seq` and are read again from
  the top (activity-journal.md, section 4): a row committed after one with a later `seq` is in the
  next read from the top, so no reader loses one for good; under (a) `seq` follows the order of
  commits anyway.
- **The queue and the admission order** (`runs.seq`, `occurrences.seq`) are orders served, not
  cursors a reader keeps; under (a) they are the order of commits, as on SQLite.

**D14. Two finishers and the chain.** `appendRunEntry` (`run_chain.go:145`) reads the chain's last
entry and writes the next sequence. Two finishing transactions at once read one tail and meet on
`run_chain`'s primary key: `40001`, which is retried, or `23505`, which is `taken`, so the finish
fails and the dispatcher stops. (a) The kind's `lastEntry` locks the chain's head row first
(`SELECT last FROM chain_head FOR UPDATE`), and its `createEntry` moves it: a second finisher waits,
then fails with `40001` and runs again on the new tail. (b) A `23505` on `run_chain`'s sequence key
is taken for a serialization failure and retried. (a) needs no reading of constraint names to keep
a run's second entry (`run_chain.run`'s key) `taken`, and holds whatever D13 decides; under D13 (a)
it never waits.

## 5. Roles and migrations

The issue's three roles are three kinds of role, and the run-time kind is two logins: four roles,
created by the operator beforehand and passed to the kind by name (condition b). The kind creates
none (condition d):

| Role | Attributes | Holds |
|---|---|---|
| The owner, `jpack_owner` by default | `NOLOGIN` | Owns the schema and every object in it. |
| The migration login | `LOGIN`, a member of the owner with `INHERIT FALSE, SET TRUE` | Nothing of its own: it reaches the owner only by `SET ROLE` (condition b). |
| The request login and the worker login (section 6) | `LOGIN`, none of `SUPERUSER`, `CREATEROLE`, `CREATEDB`, `BYPASSRLS` | `USAGE` on the schema and the table privileges of section 7, granted by the migration. |

A Runner holds a connection pool for each run-time login. At start the kind reads both logins'
attributes and memberships, and refuses to start if either is a superuser, bypasses row security,
can create roles or databases, or is a member of the owner: a login that could read every scope is
never taken for one that cannot.

**Migrations** are plain SQL files, `internal/runner/postgresql/migrations/NNNN_name.sql`, applied in
order by one command, `jpack-runner store migrate`, that release tooling runs as a release step
(condition c). The command connects as the migration login, runs `SET ROLE` to the owner, and
applies each file not yet recorded in `schema_migrations` in its own transaction, recording its
version and digest. A file recorded with another digest is refused, and nothing after it is
applied. A second run applies nothing and changes nothing (condition c). No file creates a role,
alters one, or stores a role's or the database's setting: every setting a session needs is set per
transaction with `SET LOCAL` or `set_config(..., true)` (condition d). The command refuses a
database that holds a table in any schema other than the kind's and the system's (condition a).

**The owner's policies.** Row security is forced on the owner too (section 6), so a statement of a
migration that changes rows would, with no policy for the owner, change none and pass. The owner
has two policies on every table: one admitting every row while the transaction's `jpack.migration`
setting is `on`, which the command sets in each file's transaction, and the removal's (section 7).
Neither run-time login is the owner, so neither reaches them by setting the value.

**A migration checks its own rows.** A statement that changes rows is marked in its file
(`-- jpack: rows`), and the command fails the file's transaction, naming the file and the
statement, when such a statement changes no row while its table holds any.

**D6. Grants to roles named at migration.** (a) A grants file, part of the set and its digest, with
two placeholders, the request login and the worker login, which the command fills with quoted
identifiers; the digest covers the file as shipped, and the file's row in `schema_migrations`
records the two names it was applied with. A later run naming other logins is refused, naming the
recorded ones: replacing a login is not a migration. (b) Grants to fixed group roles the operator
grants the logins into. (c) The operator runs the grants by hand. (a) keeps the files plain and
digested and the role names the operator's.

**D15. The schema row.** `metadata`'s `schema` row says which Runners may open the store; the sqlite
kind raises it at run time, when a store first holds a scoped job or a retry policy
(`raiseSchema`), so that an earlier Runner does not open it. (a) The migration writes it, at the
latest schema the set knows, and no run-time login changes it: every Runner that opens a
second-kind store knows the scoped and the retry schemas, which are older than the kind, so
`raiseSchema` finds the store raised and writes nothing, and a schema the migration has not
reached is `refused`. (b) The request login updates the row under a policy that admits only a later
schema: a policy compares the new row with one list (`WITH CHECK`) and the old row with another
(`USING`), never the two together, so "later" is a pair of lists kept in step with the code. (a)
holds the row with a privilege, not a list.

Each release ships its set with `migrations.sha256`, the digest of every file in order, and its
release notes state the versions it upgrades from (condition i). A migration only adds: a column
is added with a default, a table is never dropped or truncated (condition g).

## 6. Row security per scope identifier

**Enabled and forced on every table** (`ENABLE` and `FORCE ROW LEVEL SECURITY`), so that the owner
is held to the policies too (condition f). Every table with scoped rows has its `scope` column
(section 3): `jobs`, `job_creations`, `job_revisions`, `triggers`, `trigger_revisions`,
`occurrences`, `occurrence_keys`, `runs`, `events`, `briefs`, `attempt_volumes`. The other tables
hold no scoped row: `releases`, `metadata`, `schema_migrations`, `run_chain` (D10), `journal_head`,
`chain_head`, `counts`, `pack_identities`, `dialect_identities`. Each of those has, for each login
holding a privilege on it, a policy admitting every row, `metadata`'s narrowed by key (section 7).

**The setting.** A transaction made for a request is held to one scope by
`set_config('jpack.scope', 's:' || $scope, true)`, at its start, through the request login; every
read the request login makes, one statement or several, is such a transaction. The prefix is
there because a custom setting set once in a session reads `''` in every later transaction, not
unset: so `''` and unset must hold a transaction to no row, and the none scope needs a value of its
own, `s:`. The policy on each scoped table, for the request login:

```sql
USING (current_setting('jpack.scope', true) = 's:' || scope)
WITH CHECK (current_setting('jpack.scope', true) = 's:' || scope)
```

On `events` the request login's policy for reading also admits, to a transaction held to any
scope, the none scope's entries that name no job:

```sql
USING (current_setting('jpack.scope', true) = 's:' || scope
       OR (starts_with(current_setting('jpack.scope', true), 's:') AND scope = '' AND job_id = ''))
```

Those are the Runner's own entries (`journal.began`, `runner.started`, `runner.stopped`), previews
and refused creations. On SQLite a scoped job's journal shows its releases' previews and refused
creations, and every page says when the journal began (`journalBegan`); this keeps both. The
journal's filter (`journalFilter`) still chooses what a page holds, and writing stays held to the
transaction's scope.

A transaction that set no scope reads and writes no scoped row. **Unscoped rows** (a job created
under none, and every record about it; the Runner's own journal entries, a preview's, a refused
creation's) are the none scope: a transaction held to `s:` reaches them, one held to any other
scope does not, but for the journal entries above, as a request that names no scope reaches today
only a record that carries none (scope-identifiers.md, section 3).

**Work for every scope** runs on the worker login, whose policy on each table is `USING (true) WITH
CHECK (true)`: the dispatcher and its claims, renewals and finishes; the chain's appends; the
trigger ticks and the occurrences they admit and dispatch; preparation; cloud signals and their
refusals; the restart, the stop and the interruption of runs whose lease ran out; the store's
identity at opening; D12's report of the Runtimes held jobs need; and a store-wide listing or
journal read that names no scope, which lists every scope's records today (scope-identifiers.md,
section 5). **D7.** (a) A worker login of its own, with policies that admit every scope; the request
login is held to the scope each transaction sets. (b) One login, and a reserved setting value for
every scope. (c) A dispatcher that sets each scope in turn. (b) is ruled out: a scope identifier
may hold any character but a few (scope-identifiers.md, section 1), so no value is safe to
reserve, and a login that can widen its own setting holds nothing. (c) still needs the list of
scopes, read across scopes. A scope identifier is a label and a filter, not an authorization
boundary (scope-identifiers.md): the policy holds a request's transaction to the scope the Runner
set for it; which scopes a caller may name is not this issue's.

**Which login each route uses**, and the scope each of its transactions is held to:

| Routes | Login | Held to |
|---|---|---|
| Those naming a job, a run, a trigger or an occurrence in the path (`/v1/jobs/{job}/…`, `/v1/runs/{run}/…`, `/v1/triggers/{trigger}/…`, `/v1/occurrences/{occurrence}/…`), and `GET /v1/events` naming a job | Request | The scope the request names, `s:` when it names none (scope-identifiers.md, section 3); the scope check's own read (`recordScope`) too |
| `GET /v1/jobs`, `GET /v1/runs`, `GET /v1/events`, naming a scope | Request | That scope |
| The same, naming none | Worker | None: every scope's records, as today |
| `POST /v1/jobs` | Request | The scope the body makes the job under, `s:` for none |
| `POST /v1/previews` | Request | `s:`: a release and its entry carry no scope |
| `GET` and `POST /v1/input-profiles`, `/v1/selected-profiles` | Request | `s:`: the profile rows (section 7) |
| `GET /v1/run-chain` | Request | `s:`: the chain is unscoped (D10) |
| A refusal's entry (`journalRefusal`), in its own transaction | Request; worker for a cloud signal's | The scope of the job it names; `s:` for a refused creation, which names a release |
| `GET /v1/status`, `GET /v1/health`, `GET /v1/background-connections`, `POST /v1/inputs/next`, `/v1/inputs/preview`, `/v1/selected-inputs/next` | None | They read no row |
| `GET /v1/jobs/{job}/execution-binding` | | Manual admission, refused with this kind (D4) |

**Sequences, views, functions.** Identity columns take their next value without a privilege on
their sequence, and neither run-time login is granted one, so neither reads a sequence's value
(condition f). Sequence values are shared across scopes: a reader held to one scope sees its
entries' journal sequences, whose gaps say other entries exist, never what they are. **D8.** (a)
Document it. (b) A sequence per scope, which changes the journal's cursor. The kind defines no
view; one added later is `security_invoker`, so the reader's policy applies. It defines no
function, so no `SECURITY DEFINER` function (condition e); one added later is listed here with a
pinned `search_path` and its reason.

**D9. Rules across scopes.** A rule the Runner keeps over every scope's records (a release makes
one job, a pack identity is never reused with other bytes, 128 triggers, the queues' limits, one
trigger per cloud binding) reads rows a transaction held to one scope cannot see: under row
security each such read would count one scope's rows, and the rule would hold per scope, silently.
(a) Each is held for the installation by a key or a row no policy narrows, as the table below
says, and the Runner answers as it does on SQLite. (b) Each is held per scope on purpose: a client
in one scope could then make a job from a release another scope made one from, reuse a pack
identity with other bytes, and hold 128 triggers and a full queue of its own, none of which the
sqlite kind allows. (a) keeps one installation's rules the installation's. Its cost is the rows of
section 3, kept by the kind in the statements of its own operations, and that a request held to
one scope learns that some scope holds a release, a pack identity or a binding, never which, as
each of these rules tells today.

Every read in the code whose rows can lie in more than one scope:

| Read | Where | Under (a) | What a client sees |
|---|---|---|---|
| `jobCreation(release)` (a repeated creation) and `releaseUses(release)` | `POST /v1/jobs` (`store.go:357`, `:368`); `POST /v1/jobs/{job}/upgrade` (`job_revisions.go:119`) | The keys on `jobs.release_id`, `job_creations.release_id` and `job_revisions.release_id`, which no policy narrows: another scope's row reads `absent`, and the write meets the key as `taken` | `409 release_already_used`, with SQLite's message for the key that holds the release, a creation's or a revision's. The Runner is to answer those keys' `taken` so; it answers `500` today, where no such `taken` is met |
| `packConflicts`, `dialectConflicts` | Both routes (`admitJobPack`, `job_revisions.go:34`; `dialect_artifact.go:159`) | `pack_identities` and `dialect_identities`, unscoped: each revision's release's identities are inserted (`ON CONFLICT DO NOTHING`) in its transaction, and their held digests read back | `409 artifact_identity_conflict`, as on SQLite |
| `triggerCount()` | `POST /v1/jobs` with a trigger (`store.go:396`); `POST /v1/jobs/{job}/triggers` (`triggers_store.go:67`, outside the transaction) | `counts.triggers`, raised by `createTrigger` only while below 128 | `400 trigger_limit` at the installation's 128th trigger |
| `cloudBindings(except, connection, job)` | `POST /v1/jobs/{job}/triggers` (`triggers_store.go:77`, outside the transaction) | The binding's unique index on `triggers`, which no policy narrows: the write meets it as `taken` | `400 cloud_binding_used`, the Runner answering the index's `taken` so |
| `activeRuns()` | `POST /v1/jobs/{job}/runs` (`store.go:591`); `POST /v1/runs/{run}/retry` (`retries.go:364`) | `counts.active-runs`, moved by `createRun` and `putRun` as a run enters or leaves `queued` and `running` | `429 queue_full` at the installation's limit |
| `pendingOccurrences()` | `POST /v1/triggers/{trigger}/events` and `/deliveries` (`triggers_store.go:469`) | `counts.pending-occurrences`, moved by `createOccurrence` and `putOccurrence` as an occurrence enters or leaves `accepted`, `preparing` and `waiting` | `429 queue_full` at the installation's limit |
| `journalPage` with a job; `journalBegan()` | `GET /v1/jobs/{job}/events`, `GET /v1/events` | The read policy on `events` above | The page SQLite gives |
| `chainPage`, `entryLine`, `entrySequence` | `GET /v1/run-chain`; a run's routes | `run_chain` unscoped (D10) | The chain SQLite gives |
| `recordScope(records, id)` | The scope check of every record route (`scope.go:236`) | Per scope by construction: another scope's record reads `absent` | The route answers as for a record not held, which is what the check makes it answer on SQLite |
| `lastRefusal(key)` | Every journaled refusal | Per scope by construction: a key names its job, or, for a creation, only a release, journaled under `s:` | The same coalescing |
| `jobActivity`, `openOccurrences`, `triggerRecords`, `jobTriggers`, `identifiedOccurrence`, `keyedOccurrence`, `occurrencePage` | Their routes | Per scope by construction: each is keyed by a job or a trigger of the scope | The same |
| `oldestQueued`, `runningRuns`, `enabledTriggers`, `readyOccurrence`, `preparableOccurrences`, `preparingOccurrences`, `lastPreparing`, `cloudBoundTrigger`, `lastEntry`; the listings naming no scope | The dispatcher, the ticks, preparation, cloud signals, the restart, the chain, the listings | The worker login | The same |

No read is per scope with an answer that differs from SQLite's. The counts are the kind's: it moves
them in the statements of the operations named, so neither the contract nor the in-memory kind
changes, and the removal (section 7) takes a scope's rows out of them.

## 7. Privileges, append-only tables, removal

| Table | Request login | Worker login | Append-only |
|---|---|---|---|
| `releases` | `SELECT`, `INSERT` (a preview) | `SELECT` | Yes |
| `metadata` | `SELECT`; `INSERT`, `UPDATE` of the two profile rows | `SELECT`; `INSERT` of the identity rows | No: the profile rows are replaced |
| `jobs` | `SELECT`, `INSERT`, `UPDATE` | `SELECT` | No |
| `triggers` | `SELECT`, `INSERT`, `UPDATE` | `SELECT`, `UPDATE` (the ticks) | No |
| `occurrences` | `SELECT`, `INSERT`, `UPDATE` (a delivery, a cancel, a reconcile) | `SELECT`, `INSERT`, `UPDATE` | No |
| `runs` | `SELECT`, `INSERT`, `UPDATE` (a submission, a retry) | `SELECT`, `INSERT`, `UPDATE` | No |
| `briefs` | `SELECT`, `INSERT`, `UPDATE` | None | No |
| `job_creations`, `job_revisions` | `SELECT`, `INSERT` | `SELECT` | Yes |
| `trigger_revisions` | `INSERT` | None | Yes; never read by the Runner |
| `occurrence_keys` | `SELECT`, `INSERT` | None | Yes |
| `events` | `SELECT`, `INSERT` | `SELECT`, `INSERT` | Yes |
| `run_chain` | `SELECT` | `SELECT`, `INSERT` | Yes |
| `attempt_volumes` | `SELECT` | `SELECT`, `INSERT` | Yes |
| `pack_identities`, `dialect_identities` | `SELECT`, `INSERT` | `SELECT` | Yes |
| `journal_head`, `counts` | `SELECT`, `UPDATE` | `SELECT`, `UPDATE` | No: their rows move |
| `chain_head` | None | `SELECT`, `UPDATE` | No: its row moves |
| `schema_migrations` | `SELECT` | `SELECT` | Yes; written by the migration only |

`UPDATE` on `journal_head` is what `LOCK TABLE ... IN EXCLUSIVE MODE` requires (D13). **The policy
on `metadata`**: both logins read every row; the request login writes only the rows of the profile
registry and the selected profiles (`USING` and `WITH CHECK` on `key IN
('registered-input-profiles-v1', 'selected-input-profiles-v1')`); the worker login inserts only the
store's identity (`workspace`, `owner`, `inputRoot`) and, holding no `UPDATE`, never changes it; and
the `schema` row is the migration's (D15).

No run-time login holds `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER` or ownership: the Runner
deletes nothing, and no table is truncated or dropped at run time (condition g). Append-only is
held by the privileges, not only by the code.

**Removing one scope's rows** (condition h; the removal issue states what it is for) is one
command, `jpack-runner store remove-scope --scope <scope identifier>`, run by an operator as the
migration login, not at run time. It runs `SET ROLE` to the owner, holds the transaction to the
scope with the setting above, deletes from every scoped table of section 6 in the order of their
foreign keys, and takes the rows it deletes out of `counts`, in one transaction. The owner is held
by forced row security to a policy keyed on the same setting, so the removal reaches that scope's
rows and no other's. It leaves every unscoped table: the chain (D10), the pack and dialect
identities (a removed scope's identity stays bound to its bytes, as the rule intends), and the
releases. Journal entries about a scoped job that carry no scope (its releases' previews and
refused creations, and the Runner's own entries) are rows of the none scope, and the removal leaves
them too.

**D10. The chain of runs.** The chain is one chain for the installation: each entry names the
previous entry's digest, so removing a scope's entries breaks every later link. (a) `run_chain` is
unscoped and the removal keeps it: an entry holds a run id and its audit record's digest, not the
record, and the chain stays checkable. (b) `run_chain` is scoped, and the removal is refused while
the chain holds an entry of the scope. (c) A chain per scope, which changes the chain's format and
the specification. (a) removes every record and keeps the chain checkable.

## 8. What stays on local disk

| Path (store note, section 9.2) | In the second kind | Cost |
|---|---|---|
| `runner.lock` | Not used: D3. | |
| `releases/<id>/` | A scratch directory for the preview, as it is now: nothing reads it after the preview, and the release's record is in the database. | None. |
| `attempts/<run>[.<n>]/` | A scratch directory for the attempt's Runtime, as now; a retry takes a missing one to be unknown (D11). | An interrupted run is retried by the retry route only on a host that holds every one of its attempt directories; otherwise its caller submits a new run. Diagnostics of an attempt whose directory is gone read nothing, and name the volume it ran on. |
| `runtimes/<digest>` | Rebuilt at start from the pinned digests (below). | The image ships every Runtime a held release names. |

**The attempts and the retry check.** `attemptsHoldNoAudit` (`retries.go:398`) refuses to evaluate
an interrupted run again when an attempt's directory holds an audit trail, and takes a directory
that does not exist to hold none. On another host, or on a new volume, every directory is gone, and
that reading would let a retry evaluate again a run whose Runtime may have written an audit record:
a refusal turned into a pass. A volume's identifier does not close it: a volume cloned or restored
from a snapshot carries the identifier, and lacks what the attempt wrote after the copy. **D11.**
(a) In the second kind an attempt directory that does not exist is `errAuditUnknown`, always, as an
unreadable one is now. The volume each attempt ran on (a random identifier the attempts root holds)
is recorded in the claim's transaction, in `attempt_volumes`, outside the run's record, for
diagnostics only: it decides nothing. (b) The attempt directories on a durable shared volume, which
keeps local state the issue removes. (c) The Runtime's audit trail copied into the database as it
is written, which needs the Runtime's cooperation. (a) is the smallest that keeps the refusal; it
costs the retry of every interrupted run whose directories, an earlier attempt's among them, the
host does not hold. The sqlite kind keeps today's reading.

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

## 9. The driver

**Decision: `github.com/jackc/pgx/v5`, through its `database/sql` adapter (`pgx/v5/stdlib`).** Not
built in this slice. Its licence is MIT, with a licence file at the module root, as
`tools/third_party_notices.py` requires of every linked module; it is maintained and pure Go, so
the release programs stay statically linked. It brings a handful of modules (its connection
string and password file readers, and `golang.org/x/crypto`, `x/sync` and `x/text`), each with a
licence file; the slice that adds it runs the notices tool and records the list. The alternative,
`github.com/lib/pq`, is MIT and lighter, but its own documentation points new work to pgx. Using
`database/sql` keeps the kind's statements, scans and fault mapping shaped as the sqlite kind's.

## 10. The conditions and their tests

Each runs in CI against a PostgreSQL 16 service container whose administrator is not a superuser,
with the four roles created as an operator would; each is skipped, and says so, when no such
database is named to the test.

| Condition | Test |
|---|---|
| a. A database of its own | `TestPostgreSQLMigrationKeepsToItsOwnSchema`: the objects after migration are the kind's, in its schema; a database holding another table is refused before anything is made. |
| b. Three kinds of role, passed by name | `TestPostgreSQLRolesHoldOnlyWhatTheyNeed`: the owner cannot log in; the migration login reaches it only by `SET ROLE`; each run-time login holds only section 7's privileges and none of the four attributes; a start with a login that bypasses row security, or is the owner's member, is refused. |
| c. Migrations as one command, idempotent | `TestPostgreSQLMigrationIsIdempotent`: a second run applies nothing and the catalog is unchanged; a file recorded under another digest is refused, and so is a run naming other logins than the recorded ones. |
| d. No role made, no setting stored | `TestPostgreSQLMigrationStoresNoRoleOrSetting`: `pg_roles` and `pg_db_role_setting` are unchanged by the migration; no file holds `CREATE ROLE`, `CREATE USER` or `ALTER ... SET`. |
| e. No definer function | `TestPostgreSQLDefinesNoSecurityDefinerFunction`: the schema holds no function with `prosecdef`. |
| f. Row security per scope | `TestPostgreSQLHoldsATransactionToItsScope`: every table has row security enabled and forced; a request login held to one scope reads and writes no row of another, in every table, and no sequence's value; a transaction that set no scope reaches no scoped row, and a scope set in one transaction is not there in the next. |
| g. No truncation or drop at run time | `TestPostgreSQLRuntimeLoginsCannotRemove`: neither run-time login can delete, truncate or drop, and the append-only tables refuse an update. |
| h. One call removes a scope's rows | `TestPostgreSQLRemovingAScopeLeavesNoneOfItsRows`: after `remove-scope`, no scoped table holds a row of the scope, the unscoped tables and every other scope's rows are as they were, and `counts` equals what the tables hold. |
| i. A digested set per release | `TestPostgreSQLMigrationSetMatchesItsDigest`: the files shipped match `migrations.sha256`, and the release notes name the versions upgraded from. |

The decisions' own tests, on the same container:

| Decision | Test |
|---|---|
| D2 | The twice mode (section 2): `TestStoreKindsConform/twice`, `TestEveryTransactionIsSafeToRunTwice` and `TestAClosureRunAgainDecidesFromTheStore`, before the kind is written. |
| D3 | `TestPostgreSQLFencesEveryClaimantWriteByItsToken`: a finish or a renewal with a lease run out, or with another claim's token, writes nothing; a run interrupted and retried is not overwritten by its first claimant; the comparison is with the database's time. |
| D5 | `TestPostgreSQLKeepsARecordHoldingANullCharacter`: a run whose input holds `\u0000` in a string member is kept and read back byte for byte, its scope column filled, and every translated `json_extract` selects it as SQLite does. |
| D9 | `TestPostgreSQLHoldsEachRuleAcrossScopes`: each row of D9's table, the record held in one scope and the request held to another, answers as the sqlite kind does; after every conformance case each of `counts` equals its count read on the worker login. |
| D13 | `TestPostgreSQLJournalFollowsCommitOrder`: two writers, the one that would take the lower sequence committing last, and a reader following the cursor between them, which never passes an entry it has not read. |
| D14 | `TestPostgreSQLChainsFinishesMadeAtOnce`: finishers at once each give their run one entry, in order, and the dispatcher stays healthy. |
| D11 | `TestPostgreSQLRefusesARetryWhoseAttemptDirectoryIsMissing`, the attempts root holding the volume identifier the attempt recorded. |
| Section 5 | `TestPostgreSQLMigrationRefusesARowStatementThatChangesNoRow`: a marked statement meeting forced row security without the owner's policy fails its file. |
| Section 6 | `TestPostgreSQLRoutesUseTheirLogins`: every route of section 6's table makes its transactions on the login and scope it names. |

Beside them, the kind runs `TestStoreKindsConform` with the sqlite and in-memory kinds, and
`unit-tests.sh` requires it to run and pass when a database is named.

## 11. What the slice after this one builds

In this order. The first four change the Runner, are tested on the sqlite and in-memory kinds, and
leave SQLite's statements and rows as they are; the kind is written only after them.

1. D2's rule: the closures of section 2 that broke it, and the twice mode, with the conformance
   suite and the Service's flows under it. This is the gate. (Built, #244's second slice.)
2. The job and run listings (`list.go`) behind the contract, and every statement left outside it
   moved into the sqlite kind's files, the Service holding no database handle; the build fails on
   a statement or a `database/sql` value outside them (`TestNoStatementOutsideTheSQLiteKind`).
   (Built, #244's second slice.)
3. The contract's scope for a transaction or a read (section 2); the claim's token, `putRun`'s
   expected claim and `renewClaim` (D3); and the retry check's question to the kind, whether a
   missing attempt directory may be taken to hold no audit record (D11: yes for the sqlite kind,
   as now; no for the second kind).
4. The Runner's answers to the keys across scopes (D9): `taken` on a release's keys as
   `409 release_already_used`, on the cloud binding's index as `400 cloud_binding_used`.
5. The kind: its migrations and commands, the rows of section 3, its policies, the lease and its
   renewal, the attempts' volume rows (D11), the pinned Runtime directory (D12), the CI service
   container, and the tests of section 10.

SQLite's bytes and schema are not changed by any of it: the claim's columns and the tables of
section 3 are the second kind's own, and `store_sqlite_rows_test.go` holds the rows.

## 12. What the Runner, not the kind, holds today

The contract is safe for one Runner per store because the Runner, not any kind, holds the
following. This is recorded for several Runners on one store (#245); the contract does not change
for it in this issue.

- `automationMu` serializes every trigger write. `putTrigger` writes a trigger whatever it holds,
  and each writer read it under the mutex (`triggers_store.go:23`, `:93`, `:212`; the ticks). The
  trigger limit and the cloud binding are checked outside the write's transaction under it
  (`triggers_store.go:67`, `:77`). #245 must give `putTrigger` an expected revision, as `putRun`
  has an expected state, and move both checks into the write's transaction, which D9's rows do for
  the second kind. #244 leaves it so: a trigger's record revision does not change with every
  write (a tick, the file cursor and the pending state change none), so an expected
  revision needs a row version the sqlite kind's schema does not have.
- The profile rows are written whole from the process's copies (`storedProfiles` and the selected
  profiles, under `profilesMu`: `profile_registry.go:222-232`, `selected_profiles.go:236`), and a
  job's creation reads those copies (section 2). #245 must version the rows and read them in the
  transaction that writes them.
- The occurrences being prepared, the cloud bindings' problems, the profiles set aside and the
  unhealthy flag (store-interface.md, section 9.3).

## 13. Not in this issue

Several Runners on one store (#245), beyond not making it harder; which scopes a caller may name;
the removal issue's retention rules; a third kind.

## 14. Decisions for a maintainer

Each with the recommendation first.

| | Decision | Options |
|---|---|---|
| D1 | How a Runner selects the kind | Service-mode configuration file with a connection file; boot line flag; environment |
| D2 | How transactions compose | The closure, retried on serialization failure, under the rule of section 2 and its gate; named operations |
| D3 | The claim's fence without a lock | A lease per claim, its token fencing every claimant write by the database's clock; a heartbeat per Runner; an advisory lock (ruled out) |
| D4 | Manual admission | Stays on SQLite, refused with the second kind; ported |
| D5 | The scope column | Generated from the record through `json`, or filled from the parent row; written by the kind |
| D6 | Grants to named roles | A digested grants file with two placeholders, the names recorded; fixed group roles; by hand |
| D7 | Work across scopes | A worker login; a reserved setting (ruled out); a dispatcher per scope |
| D8 | Shared sequences | Documented; per-scope sequences |
| D9 | Rules across scopes | Held for the installation by keys and rows no policy narrows; held per scope |
| D10 | The chain on removal | Unscoped and kept; scoped, removal refused; a chain per scope |
| D11 | The attempts and the retry check | A missing attempt directory unknown, the volume for diagnostics; a durable shared volume; the trail in the database |
| D12 | The pinned Runtime | Rebuilt at start from shipped executables; in the database; an object store |
| D13 | The journal's order | Writers one at a time behind a table lock, the sequence from a counter row; the counter row alone; a visibility horizon |
| D14 | Two finishers and the chain | The chain's head row locked; a unique violation retried |
| D15 | The schema row | The migration's alone; raised at run time under a policy |
