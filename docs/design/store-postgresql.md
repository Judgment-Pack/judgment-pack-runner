# A second storage kind on PostgreSQL

Issue #244. Status: built in part. The Runner's changes before the kind (section 11, steps 1 to
4) are built, and the kind (step 5) is built for the routes section 6 lists as held; section 11
says what step 5 built, where it differs from this note, and what is left. Section 12 is the
first step of #245, several Runners on one store of this kind, and is built. The decision is
recorded in [ADR 0001](../adr/0001-second-storage-kind.md); the operator's page is
[docs/postgresql-store.md](../postgresql-store.md).

A Runner keeps its state in one SQLite database and a few directories under its state directory
([store-interface.md](store-interface.md), section 9). This note fixes the second storage kind,
on PostgreSQL 16, so that building it is mechanical: its schema, its roles, its migrations, its
row policy per scope identifier, the order it keeps under concurrent writers, what stays on local
disk and what it costs, the driver, and the test that answers each of the issue's database
conditions and each decision here. SQLite stays the default kind, unchanged byte for byte
(`store_sqlite_bytes_test.go` and `store_sqlite_rows_test.go` hold it).

**Every decision a maintainer must make is marked D1 to D20, with its options; the recommendation
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
  captured the occurrence: `scheduleDue` (the schedule tick's, `scheduleTick` and `sharedTick`'s)
  and `fileTick` (`triggers_dispatch.go`), `receiveCloud`
  (`cloud.go`) and `admitBoundEvent` (`triggers_store.go`). Run again after the queue drained or
  the overlap ended, it admitted the occurrence skipped, its input gone. It now works on a copy and
  returns the occurrence it recorded; no caller's closure assigns it before its end.
- `admitBoundEvent` kept a full queue's refusal in a captured variable and answered it after
  `update`, though a later run had committed the occurrence: an event admitted was answered
  `429 queue_full`. The refusal is now `fn`'s own answer.
- `retryRunBy` (`retries.go`) read the attempt directories inside `fn` (`attemptsHoldNoAudit`).
  The check is made before the transaction, on the run as read then and only once it reads
  interrupted, and its answer returned inside `fn` where it was made, after the replay and the
  state checks, only for the interruption checked (`sameInterruption`), so that the check and the
  write concern one interruption of one attempt. Otherwise `fn` answers as the read before
  would have: `run_not_retryable` for a run read still running, `attempt_mismatch` for another
  attempt; a read before that fails is answered as a failed read.
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
  claim_attempt=$3, lease_until=now() + $4 WHERE id=$5 AND state='queued'`. As built, two
  dispatchers that read one queued run meet here, or on the write that ends it in the queue
  (expired there, or naming a profile set aside; `endInQueue`): the second is `conflict`, and its
  dispatcher works nothing and goes on (`dispatchNext`), not stopping as for a failure of the
  store; any other conflict still stops it. Its worker then reads the queue again at once, as
  after a run worked, not a second later as after an empty queue (#245, section 12).
- **Expiry is the database's clock.** Every comparison with `lease_until` is with `now()` in the
  statement that makes it. No Runner's clock is compared with it or written into it. As built, a
  claimant's fence (`lease_until >`, a finish's and a renewal's) is with `statement_timestamp()`
  instead: `now()` is the transaction's start, before its wait for `journal_head`'s lock (D13), so
  a finish that waited past its lease's end would still commit. A lease set (a claim's, a
  renewal's) keeps `now()`, the earlier time and so the shorter lease, and so does the
  interruption's test of a lease run out, which then runs out later.
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
  ran out: they read the runs held running, and write each with the expectation `lapsed`
  (`runExpected.lapsed`), which the write's statement holds: `WHERE id=$1 AND state='running' AND
  (claim_id IS NULL OR lease_until <= now())`. A run whose lease is live is `conflict` and is left
  as it is, so a renewal that landed between the read and the write keeps the run. An interruption
  clears the token. A run whose lease is live is its holder's, whoever starts. The sqlite kind
  holds no run under a claim, and answers `lapsed` for every run held running: it is asked only at
  the restart, when its lock keeps every other dispatcher out and its own has claimed nothing.
- **No claim is not every claim.** A run read from the store carries no claim (the token is never
  read back). A write that expects a run `running` and holds no claim finds it under no claim
  (`claim_id IS NULL`), or is `conflict`: a writer that is not the claimant does not overwrite the
  claimant's run.
- **The margin.** The lease is 60 seconds, renewed every 15. A renewal fails when it returns an
  error or `conflict`, or takes longer than 15 seconds. The Runner begins stopping the run's
  Runtime (its process group) 30 seconds, by its monotonic clock, after the start of the last claim
  or renewal that succeeded. That renewal's `now()` is after its start, so the lease the database
  holds runs at least 60 seconds from it, and the Runtime is being stopped at least 30 seconds
  before the lease can run out, whatever the Runner's wall clock says. A Runner that loses a claim
  stops its dispatcher, as a failed finish does now; the run is interrupted one lease later and is
  retried only as D11 allows. As built (`claims.go`), a renewal that fails stops the Runtime at
  once, not 30 seconds after the last success; the 30 seconds bound the case where no renewal
  answers at all. The claimant then writes no finish, and its dispatch returns `errClaimLost`,
  which stops the dispatcher and makes the health route report it. Stopping the Runtime is the
  attempt's context cancelled, as every other stop of a Runtime is: the Runtime's process is
  killed and waited for before the dispatch returns. No Runtime is started in a process group of
  its own, so "its process group" above is not built: a process the Runtime itself started is
  not ended.
- **The tick's interruption, as built.** On a kind that leases claims, the dispatcher's tick,
  every 15 seconds, writes each run held running whose lease ran out as interrupted, with the
  expectation `lapsed` and an `interruption` whose `lastKnownRunning` is the run's start; unlike
  the restart's, it names no `restart`. The sqlite kind leases nothing, so its tick interrupts
  nothing.

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
| `pack_identities(release_id, pack, version, digest)` and `dialect_identities(release_id, dialect, version, digest)`, keyed on `release_id`, indexed on the identity's two columns | | D9. As built: one row for each revision's release, so that a conflict is counted over the revisions as the sqlite kind counts it, rather than one row per identity keyed on its first two columns. |
| `attempt_volumes(run, attempt, volume, scope)` | | D11: diagnostics only. Not built in step 5. |
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

Built with time bounds as well, which this note did not give: every pooled connection starts with
`lock_timeout` 5 s, `statement_timeout` 30 s and `idle_in_transaction_session_timeout` 10 s
(`pgBounds`; docs/postgresql-store.md, "Time bounds"). A writer held up by another session waits
at most the lock bound and is refused; a session left idle inside a transaction that holds the
lock is ended by the server; and the claim's renewal is held to a deadline of its interval
(`updateWithin`), so that an attempt's end never waits on one without a bound. The start reads the
bounds and the search path back on a connection of each pool and refuses one not held, by name
(`checkSettings`, `errPostgreSQLSettingNotHeld`), as a connection pooler that drops start
parameters would leave them. A request's write refused for the lock is not run again. The dispatcher takes the refusal for a store that is busy,
not failing (`lockWaited`): its claim, its end of a run in the queue and its tick for lapsed
leases made nothing and are tried at a later tick; its renewal is run again until its deadline;
and its finish is made again while the claim's lease is live, which its token and lease fence
(D3), so that a lock held a few seconds neither stops the dispatcher nor loses a run.

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

As built, the start also reads the live schema, which this note did not ask (`checkSchema`): it
refuses a schema, or a relation, function, operator or type in it (a table's own row type aside,
which is its relation's), that the owner the configuration names does not own
(`errSchemaNotOwned`), a table without row security enabled and forced (`errRowSecurityOff`), and
a run-time login that can use `DELETE`, `TRUNCATE`, `REFERENCES` or `TRIGGER` on a table of the
schema, any privilege on a sequence of it (the grants give none, and a sequence's value is shared
across scopes, D8), or `CREATE` on it (`errLoginPrivilegeTooWide`). The privileges are read with
`has_table_privilege`, with `has_any_column_privilege` for `REFERENCES` (a grant on one column is
enough for a foreign key to it), with `has_sequence_privilege`, and with `has_schema_privilege`,
for the login and every role it is a member of, so that a grant made after the migration, to a
predefined role such as `pg_write_all_data`, by a default privilege, or to a role reached only by
`SET ROLE`, is refused, not only one made to the login by name. A wrong owner name in the configuration, which the
membership check above passes, is refused by the first.

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
statement, when such a statement changes no row while its table holds any. (Not built in step 5:
the two files of the first set change rows only in tables they create.)

**As built.** The command is `jpack-runner store migrate --connection <path>`, the file holding
the migration login's connection string and the names of the owner and the two run-time logins
(the operator's page has its form); it prints the versions it applied. It takes the owner's role
with `SET LOCAL ROLE` in each file's transaction. It reads whether the set was ever applied from
the catalog, since the migration login holds no privilege on the schema that `to_regclass` would
need. A Runner's start applies nothing: it refuses a store whose applied set is not exactly its
own, an earlier one included.

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

A schema later than the kind is written by a migration of its own: `0005_schedules.sql` (#246)
raises the row to `7`, the schema of a store that may hold a cron or shared-interval schedule
([schedules.md](schedules.md), section 7), and changes nothing else.

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

Those are the Runner's own entries (`journal.began`, `runner.started`, `runner.stopped`) and
previews. On SQLite a scoped job's journal shows its releases' previews, and every page says when
the journal began (`journalBegan`); this keeps both. The journal's filter (`journalFilter`) still
chooses what a page holds, and writing stays held to the transaction's scope.

**D16. A refused creation in another scope's journal.** A refused job creation names a release,
not a job, and its caller's issuer and subject; it is journaled under `s:` (`refusedJobBy`), and on
SQLite the journal of the job made from that release shows it, whichever caller and scope the
refused body named. (a) A restrictive policy, `request_refusals_held` in `0003_held_routes.sql`,
keeps the none scope's refusals that name no job out of every transaction held to another scope:

```sql
AS RESTRICTIVE FOR SELECT
USING (current_setting('jpack.scope', true) = 's:' OR scope <> '' OR job_id <> '' OR kind <> 'admission.refused')
```

A refused creation then shows only to a request held to none, and to the listing naming no scope
on the worker login. (b) Journal the refusal under the scope the body names: a body naming none,
or a malformed one, still shows its caller to every scope's job made from the release, and the
sqlite kind's entry would carry the scope, which changes its bytes and what `GET /v1/events` with
`scope` lists there. (a) is the narrower: no scope but none reads another caller's refused
creation, the refusal's coalescing (`lastRefusal`) still reads it held to `s:`, and SQLite's bytes
and pages do not change. Its cost: a scoped job's journal on this kind no longer shows its
release's refused creations, which the sqlite kind's does. A refused cloud signal naming no
trigger is the only other entry it holds out, and no scope's page selected one. Previews, which
also name a caller, are not narrowed by this issue.

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
| `POST /v1/ticks` (#246) | Worker | None: it ticks the triggers of every scope, as the Runner's clock does; one naming a scope is refused before it reads ([schedules.md](schedules.md), section 5) |
| `POST /v1/jobs` | Request | The scope the body makes the job under, `s:` for none |
| `POST /v1/previews` | Request | `s:`: a release and its entry carry no scope |
| `GET` and `POST /v1/input-profiles`, `/v1/selected-profiles` | Request | `s:`: the profile rows (section 7) |
| `GET /v1/run-chain` | Request | `s:`: the chain is unscoped (D10) |
| A refusal's entry (`journalRefusal`), in its own transaction | Request; worker for a cloud signal's | The scope of the job it names; `s:` for a refused creation, which names a release, and which no other scope reads (D16) |
| `GET /v1/status`, `GET /v1/health`, `GET /v1/background-connections`, `POST /v1/inputs/next`, `/v1/inputs/preview`, `/v1/selected-inputs/next` | None | They read no row |
| `GET /v1/jobs/{job}/execution-binding` | | Manual admission, refused with this kind (D4) |

**The routes held in step 5.** On a kind that holds scopes (`holdsScopes`), the scope middleware
(`scoped`, `scope.go`) puts on each request's context the store held to the scope it names (none
when it names none), or the store itself for a listing that names no scope, and the routes below
reach rows only through it (`storeFor`), the scope check's read included:
`GET /v1/status`, `POST /v1/previews`, `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/runs`,
`GET /v1/jobs/{job}`, `GET /v1/jobs/{job}/runs`, `POST /v1/jobs/{job}/runs`, `GET /v1/runs/{run}`,
`POST /v1/jobs/{job}/upgrade` and `GET /v1/jobs/{job}/upgrade-receipt`; held in #244's fifth
slice, the journal, `GET /v1/events` and `GET /v1/jobs/{job}/events`, and `GET /v1/run-chain`;
a job's triggers, `GET` and `POST /v1/jobs/{job}/triggers` and `POST /v1/jobs/{job}/triggers/preview`,
and a trigger's `POST /v1/triggers/{trigger}/state` and `/rotate-key`; deliveries,
`POST /v1/triggers/{trigger}/events` and `/deliveries`, and their reads,
`GET /v1/triggers/{trigger}/occurrences/{occurrence}` and `/deliveries/{event}`; a job's occurrences,
`GET /v1/jobs/{job}/occurrences`, and `POST /v1/occurrences/{occurrence}/cancel` and `/reconcile`;
`POST /v1/runs/{run}/retry`; and the installation's profiles, `GET` and `POST /v1/input-profiles`
and `/v1/selected-profiles`, whose rows a registration writes held to none on any kind, on the
request login, which alone may write them (section 7), and whose listings read no row
(`heldRoutes`). A trigger the view does not hold answers an event
credential as an unknown trigger does, `401 invalid_trigger_token`, and a delivery's refusal is
journaled only for a trigger the view holds. The ticks, the dispatch of occurrences into runs and
preparation are the store's own, on the worker login, as the work for every scope above says; the
retry's reading of the attempt directories is the kind's (D11), not the view's. `GET /v1/events` naming a job is that job's route: held to the scope named, none
when it names none, as the table says, and the store for every scope only when it names neither a
scope nor a job. `GET /v1/health` reads no row. Every other route is refused on such a kind,
`501 store_route_unsupported`, before it reads anything, until it is held to its scope as the
table says. `TestAKindThatHoldsScopesServesItsRoutesOnlyThroughTheirHeldView` serves each held
route on a store that panics on any read or write not made through a held view, and each refused
route. A refusal's entry (`journalRefusal`) is written held to the scope of the job it names, a
cloud signal's included, so on the request login, not the worker login the table names; a
release's save is held to none. On the sqlite kind `heldTo` is the store itself and nothing
changes.

**Sequences, views, functions.** Identity columns take their next value without a privilege on
their sequence, and neither run-time login is granted one, so neither reads a sequence's value
(condition f). Sequence values are shared across scopes: a reader held to one scope sees its
entries' journal sequences, whose gaps say other entries exist, never what they are. **D8.** (a)
Document it. (b) A sequence per scope, which changes the journal's cursor. The kind defines no
view; one added later is `security_invoker`, so the reader's policy applies. It defines no
function, so no `SECURITY DEFINER` function (condition e); one added later is listed here with a
pinned `search_path` and its reason. As built, every pooled connection's `search_path` is
`pg_catalog, jpack_runner`, the catalog first (`pgOpenPool`): a function, operator or type made in
the schema with a built-in's name never hides the built-in from the kind's unqualified SQL
(`statement_timestamp()`, `now()`, `set_config`), and the tables, unqualified, are found in the
schema, the catalog holding none of their names. The migrations set their own path, the schema
first, since they create objects in it unqualified; the start refuses a function, operator or type
in the schema that another role owns (section 5).

**D9. Rules across scopes.** A rule the Runner keeps over every scope's records (a release makes
one job, a pack identity is never reused with other bytes, the trigger limit, the queues' limits, one
trigger per cloud binding) reads rows a transaction held to one scope cannot see: under row
security each such read would count one scope's rows, and the rule would hold per scope, silently.
(a) Each is held for the installation by a key or a row no policy narrows, as the table below
says, and the Runner answers as it does on SQLite. (b) Each is held per scope on purpose: a client
in one scope could then make a job from a release another scope made one from, reuse a pack
identity with other bytes, and hold the installation's limit of triggers and a full queue of its own, none of which the
sqlite kind allows. (a) keeps one installation's rules the installation's. Its cost is the rows of
section 3, kept by the kind in the statements of its own operations, and that a request held to
one scope learns that some scope holds a release, a pack identity or a binding, never which, as
each of these rules tells today.

Every read in the code whose rows can lie in more than one scope:

| Read | Where | Under (a) | What a client sees |
|---|---|---|---|
| `jobCreation(release)` (a repeated creation) and `releaseUses(release)` | `POST /v1/jobs` (`store.go:357`, `:368`); `POST /v1/jobs/{job}/upgrade` (`job_revisions.go:119`) | The keys on `jobs.release_id`, `job_creations.release_id` and `job_revisions.release_id`, which no policy narrows: another scope's row reads `absent`, and the write meets the key as `taken` | `409 release_already_used`, with SQLite's message for the key that holds the release, a creation's or a revision's. The Runner answers those keys' `taken` so (`releaseTaken`); the sqlite kind, whose reads before the write see every scope, never meets one |
| `packConflicts`, `dialectConflicts` | Both routes (`admitJobPack`, `job_revisions.go:34`; `dialect_artifact.go:159`) | `pack_identities` and `dialect_identities`, unscoped: each revision's release's identities are inserted (`ON CONFLICT DO NOTHING`) in its transaction, and their held digests read back | `409 artifact_identity_conflict`, as on SQLite |
| `triggerCount()` | `POST /v1/jobs` with a trigger (`store.go:396`); `POST /v1/jobs/{job}/triggers` (`triggers_store.go:68`, outside the transaction) | `counts.triggers`, raised by `createTrigger` only while below the limit of the Runner that raises it (128 unless its configuration's `limits` sets it; [schedules](schedules.md), section 9) | `422 trigger_limit` at the installation's last trigger, stating the limit, as the Runner answers on SQLite: `triggerCount()` reads the row in any view. Runners of one store configured with different limits each refuse at their own |
| `cloudBindings(except, connection, job)` | `POST /v1/jobs/{job}/triggers` (`triggers_store.go:78`, outside the transaction) | The binding's unique index on `triggers`, which no policy narrows: the write meets it as `taken` | `422 cloud_binding_used`, as the Runner answers on SQLite: `configureTriggerBy` answers the index's `taken` (`keyCloudBinding`) so. `POST /v1/jobs` with a cloud trigger checks no binding before its write, on either kind, and meets the index; `createJobWith` answers its `taken` as the trigger route does, `422 cloud_binding_used` (#279), where it answered `500` before on both kinds |
| `activeRuns()` | `POST /v1/jobs/{job}/runs` (`store.go:637`); `POST /v1/runs/{run}/retry` (`retries.go:395`) | `counts.active-runs`, moved by `createRun` and `putRun` as a run enters or leaves `queued` and `running` | `429 queue_full` at the installation's limit |
| `pendingOccurrences()` | `POST /v1/triggers/{trigger}/events` and `/deliveries` (`triggers_store.go:503`); the ticks, on the worker login | `counts.pending-occurrences`, moved by `createOccurrence` and `putOccurrence` as an occurrence enters or leaves `accepted`, `preparing` and `waiting` | `429 queue_full` at the installation's limit |
| `journalPage` with a job; `journalBegan()` | `GET /v1/jobs/{job}/events`, `GET /v1/events` | The read policy on `events` above | The page SQLite gives |
| `chainPage`, `entryLine`, `entrySequence` | `GET /v1/run-chain`; a run's routes | `run_chain` unscoped (D10) | The chain SQLite gives |
| `recordScope(records, id)` | The scope check of every record route (`scope.go:236`) | Per scope by construction: another scope's record reads `absent` | The route answers as for a record not held, which is what the check makes it answer on SQLite |
| `lastRefusal(key)` | Every journaled refusal | Per scope by construction: a key names its job, or, for a creation, only a release, journaled under `s:` | The same coalescing |
| `jobActivity`, `openOccurrences`, `triggerRecords`, `jobTriggers`, `identifiedOccurrence`, `keyedOccurrence`, `occurrencePage` | Their routes | Per scope by construction: each is keyed by a job or a trigger of the scope | The same |
| `oldestQueued`, `runningRuns`, `enabledTriggers`, `readyOccurrence`, `preparableOccurrences`, `preparingOccurrences`, `lastPreparing`, `cloudBoundTrigger`, `lastEntry`; the listings naming no scope | The dispatcher, the ticks, preparation, cloud signals, the restart, the chain, the listings | The worker login | The same |

**The order of the release's keys.** A creation meets the release's keys in the order
`job_creations`, `job_revisions`, `jobs`: the second kind inserts the creation's row first, so that a
release another scope made a job from meets `job_creations.release_id` and is answered with the
creation's message (`errReleaseCreated`), and a revision's release meets
`job_revisions.release_id` before `jobs.release_id` and is answered with the revision's. The sqlite
kind inserts into `jobs` first and is unchanged: its reads before the write see every scope, so it
never meets a key.

No read is per scope with an answer that differs from SQLite's. The counts are the kind's: it moves
them in the statements of the operations named, so neither the contract nor the in-memory kind
changes, and the removal (section 7) takes a scope's rows out of them.

Section 11, step 4, read the `400` of the table above, as first written, as a change to make; the
Runner answers `trigger_limit` and `cloud_binding_used` with `422` on every kind, and the table
now says so rather than change what the sqlite kind answers.

**The input root is the installation's.** A file input names a path under the Runner's one input
root (`inputRoot`, the store's identity), on both storage kinds; the root has no subtree per scope.
A trigger preview held to one scope (`POST /v1/jobs/{job}/triggers/preview`) may name a file
another scope's jobs use, and its answer carries the file's facts and evidence. Row security holds
the store's rows to a scope, not the files under the input root, which every Runner of one store
shares (the store records one `inputRoot` that every Runner of it must match): scopes whose
inputs must stay apart are given separate installations (#279).

**What a request held to one scope can infer.** Row security keeps another scope's rows from a
request held to one scope; it does not keep that request from learning that other scopes are
active. The known channels, each accepted (D8, D9, D10):

- The chain of runs, held to no scope (D10): `GET /v1/run-chain` lists every scope's entries, each
  a run id and its audit record's digest, so a request in any scope sees how many runs the
  installation recorded, and in what order among its own.
- Gaps in the journal's sequence (D8): an entry's sequence is the installation's, so the gaps
  between a scope's own entries say how many entries other scopes wrote between them.
- The installation-wide counts and keys (D9): `422 trigger_limit` at the installation's trigger
  limit; `429 queue_full` at the pending-occurrence limit (a delivery) and at the run queue's limit
  (a submission or a retry); `422 cloud_binding_used` for a binding another scope holds; and
  `409 release_already_used` and `409 artifact_identity_conflict` for a release or a pack or
  dialect identity another scope holds. Each says that some scope holds the rows, never which.

D9 and D10 accept these as the cost of one installation's rules and one checkable chain; a
deployment that needs scopes not to see each other's activity gives them separate installations.

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
('registered-input-profiles-v1', 'selected-input-profiles-v1')`), and only in a transaction held to
the none scope: a restrictive policy for `INSERT` and one for `UPDATE`, in
`0006_profile_rows_unscoped.sql` (#279), require `current_setting('jpack.scope', true) = 's:'`, so
that a later code path held to a scope identifier, or setting none, writes no profile row, not
only the routes that refuse a scope; the worker login inserts only the
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
them too. (Not built in step 5: `jpack-runner store` takes `migrate` alone.)

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

**The attempts and the retry check.** `attemptsHoldNoAudit` (`retries.go:445`) refuses to evaluate
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
host does not hold. The sqlite kind keeps today's reading. (Step 5 built the reading, the kind's
`missingAttemptHoldsNoAudit` answering no; not the volume rows. The fifth slice serves the retry
route held to its scope, and the kind's answer refuses a retry whose directory is missing,
`409 run_not_retryable`.)

**The pinned Runtime.** A release names the digest of the Runtime it was made with, and every
evaluation reads `runtimes/<digest>` and refuses a file that is missing or changed
(`runtime.go:357`). At start a Runner pins the Runtime it was given. The second kind's Runner pins,
in addition, each executable of a read-only directory the configuration names, by its digest; a
release whose digest is not among them fails its runs closed, as a missing file does now, and the
health route names the digests held releases need that are not pinned. **D12.** (a) Rebuilt at
start from executables the image ships, by digest. (b) The executables in the database, up to
128 MiB each. (c) An object store behind a small interface. (a) adds no storage and no interface,
and keeps the digest check where it is; its cost is that an image must carry every Runtime a held
job's release names, or the job is revised to a release of a Runtime it carries. (Not built in
step 5: a Runner on the kind pins the one Runtime it is started with, as on the sqlite kind.)

## 9. The driver

**Decision: `github.com/jackc/pgx/v5`, through its `database/sql` adapter (`pgx/v5/stdlib`).**
Built in step 5 at v5.12.0, each run-time login's pool a `pgxpool` opened through the adapter. Its licence is MIT, with a licence file at the module root, as
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

**As built in step 5.** The server is named to the tests by `JPACK_TEST_POSTGRESQL`, an
administrator's connection string (a login with `CREATEROLE` and `CREATEDB`, not a superuser);
each test makes roles and a database of its own and drops them. CI runs them in a job of their
own, `PostgreSQL store`, on a `postgres:16` service container, through the `postgresql-store`
step of `dev.json` (`scripts/checks/postgresql-store.sh`), which sets
`JPACK_TEST_POSTGRESQL_REQUIRED` so that a skip fails, and requires the `postgresql` and
`postgresql-twice` conformance runs and every `TestPostgreSQL` test to pass. `unit-tests.sh` is
unchanged; the Runner legs skip the step.

| Test | State |
|---|---|
| `TestStoreKindsConform/postgresql` and `/postgresql-twice` | Built. |
| `TestPostgreSQLHoldsATransactionToItsScope` (f) | Built, with a scope set by the kind itself not outliving its transaction on a pooled connection. |
| `TestPostgreSQLRuntimeLoginsCannotRemove` (g) | Built. |
| `TestPostgreSQLRolesHoldOnlyWhatTheyNeed` (b) | Built. A login that bypasses row security cannot be made by an administrator that is not a superuser, so the refusal of one is held by the start's query alone, not by a test. |
| `TestPostgreSQLMigrationIsIdempotent` (c, with a and d) | Built: the conditions of a and d are cases of it, not tests of their own. |
| `TestPostgreSQLMigrationSetMatchesItsDigest` (i) | Built for the files and their digest; release notes are not read. |
| `TestPostgreSQLDefinesNoSecurityDefinerFunction` (e) | Built. |
| `TestPostgreSQLFencesEveryClaimantWriteByItsToken` (D3) | Built, with a check that the kind's files (every `store_postgresql*.go` but the tests) read no process clock, compare a lease, on either side of the operator, only with the database's clock, and fence no claimant's write by `now()`; the check is first run on comparisons it must find and must pass. |
| `TestPostgreSQLStartRefusesASchemaNotOwnedByTheNamedOwner`, `TestPostgreSQLStartRefusesATableWithoutForcedRowSecurity`, `TestPostgreSQLStartRefusesARuntimeLoginWithWiderPrivileges` (section 5) | Built: a start is refused for another owner named, a relation, function, operator or type another role owns, a table with row security not forced or disabled, and each of the four table privileges, `REFERENCES` on one column, each of the three sequence privileges, and `CREATE` on the schema, granted by name, through a role inherited or reached by `SET ROLE`, by a default privilege and through `pg_write_all_data`. The last needs the administrator to grant `pg_write_all_data` (CI's is given `ADMIN TRUE, INHERIT FALSE, SET FALSE`). |
| `TestPostgreSQLADispatcherThatLosesAClaimGoesOn` (D3) | Built: two Runners on one store read one queued run; exactly one works it, the other returns no error and works the next run. |
| `TestPostgreSQLADispatcherThatLosesARunEndedInTheQueueGoesOn` (D3) | Built: the same race on a run expired in the queue and on one whose release names a profile set aside; exactly one ends it, the other returns no error. |
| `TestPostgreSQLFencesAClaimantWriteThatWaitedPastItsLease` (D3) | Built: a finish and a renewal that wait for `journal_head`'s lock past their lease's end write nothing. |
| `TestPostgreSQLServesARunThroughItsHeldRoutes` | Built: a Runner on the kind serves a job and a run end to end through its held routes. |
| `TestPostgreSQLHoldsTheJournalToItsScope` (section 6, D16) | Built in the fifth slice: on a Runner on the kind, a scope's job's journal is served held to its scope; another scope, or a request naming none, is answered `404` for it, by either route; a listing naming a scope holds that scope's entries alone; a refused creation shows in the listing naming no scope and in no scope's page. The conformance case "a refused creation is read only held to the none scope" holds D16 on the in-memory kind and the kind, and SQLite's pages unchanged. |
| `TestPostgreSQLHoldsTriggersToTheirScope` (section 6, D9) | Built in the fifth slice: another scope neither lists, reads nor changes a scope's trigger, naming its id in the path or through a job of its own, and the trigger is as it was; the scope's own configuration, preview, resumption and key rotation are served and journaled in its scope; a binding another scope holds is refused by the index, `422 cloud_binding_used`, as the Runner's own read refuses it in one scope; the installation's count refuses a trigger in any scope at the limit, `422 trigger_limit`, and a refused trigger leaves the count as it was. |
| `TestPostgreSQLHoldsOccurrencesToTheirScope` (section 6, D9) | Built in the fifth slice: another scope, or a request naming none, neither delivers to a scope's trigger, reads an occurrence's result, delivers or reads a record, lists its job's occurrences, nor cancels or reconciles one: the trigger answers `401 invalid_trigger_token`, as an unknown one, the rest `404`; the scope's own requests are served; the tick, on the worker login, admits a schedule's occurrence in its job's scope and dispatches each scope's occurrence into a run of that scope, which another scope does not read; past the installation's pending count, an event is refused `429 queue_full` and a schedule's occurrence skipped. |
| `TestPostgreSQLRetriesARunHeldToItsScope` (section 6, D11) | Built in the fifth slice: another scope, or a request naming none, does not retry a scope's interrupted run; the scope's retry is refused while the attempt's directory is missing, `409 run_not_retryable`, and queued once the directory holds no trail; the entry is not in another scope's journal. |
| `TestPostgreSQLRegistersTheInstallationsProfiles` (sections 6 and 7) | Built in the fifth slice: an input profile and a selected profile are registered through their routes, their rows written on the request login held to none; a request naming a scope is refused `400`; a repeated registration answers `200`. |
| `TestPostgreSQLLoginsHoldOnlyTheirTablesPrivileges` (section 7) | Built in the fifth slice: each run-time login holds on each of the store's tables the privileges section 7's table names and no other, a table it does not name none, so a grant widened in a later file is caught. |
| `TestPostgreSQLMigratesAStoreAtTheGrantsForward` (c) | Built in the fifth slice: a store migrated to `0002`, as the previous release left it, is refused at the start; a run naming other logins applies nothing; the migration applies the later files alone, leaves the catalog a store migrated at once has, records the logins on the later file's row, and applies nothing on a second run. |
| `TestPostgreSQLResolvesABuiltinBeforeTheSchema` | Built: with `now()`, `statement_timestamp()` and `set_config` made in the schema, by the owner, to fail, a claim, a renewal, a finish and a read held to a scope still call the built-ins; no relation of the schema has a name the catalog holds. |
| `TestPostgreSQLHoldsEveryConnectionToItsTimeBounds`, `TestPostgreSQLStartRefusesAConnectionWithoutItsSettings`, `TestPostgreSQLRefusesAWriteThatWaitsOnAHeldLock`, `TestPostgreSQLADispatcherBehindAHeldLockGoesOn`, `TestPostgreSQLHoldsARenewalToItsDeadline`, `TestPostgreSQLEndsATransactionLeftIdle` (D13's time bounds) | Built: both pools hold the bounds whatever the connection string names; a start over a pool whose connection was opened without the search path or one of the bounds is refused, naming it and the login; with another connection holding `journal_head`'s lock, a write is refused within the lock bound; a renewal is tried again, keeps the claim when the lock is let go before its deadline, and is refused at its deadline, its wait on the server ended, the claim reported lost; a dispatcher's end of a run in the queue, its tick for lapsed leases and its claim make nothing and its loop goes on, its finish lands once the lock is let go, the run completed and not interrupted, and the Runner stays healthy; a transaction left idle is ended by the server and the writer behind it goes on. |
| `TestPostgreSQLTwoRunnersTickingOneScheduleMakeOneOccurrence` (D17, #245) | Built: two Runners tick one schedule trigger at once, each having read it at one version; one writes it and admits the due time's occurrence, the other's write meets the version moved on and makes nothing; neither fails, and the occurrence is dispatched once. |
| `TestPostgreSQLTwoRunnersWorkOneReadyOccurrenceOnce` (D18) | Built: two Runners dispatch one ready occurrence both read; one claims and works it, the other passes it by, making nothing and failing nothing. |
| `TestPostgreSQLASlowHolderLosesItsOccurrenceAndItsLateWriteIsRefused` (D18) | Built: a Runner holding a ready occurrence is slow past its lease; the other claims and dispatches it, and the slow holder's late write writes nothing; neither fails. |
| `TestPostgreSQLARunnerStoppedMidAttemptHasItsRunInterruptedAfterTheLease` (D3, D11, D19) | Built: a Runner stopped while its run's attempt is under way has the run interrupted by the other's tick once the lease runs out by the database's clock, and not before; the run is not worked again by the other's dispatcher or a later start; its retry on a host without the attempt's directory is refused, `409 run_not_retryable`; the stopped Runner's late finish writes nothing. |
| `TestPostgreSQLASecondRunnersStartLeavesTheFirstsWorkToIt` (D3, D18) | Built: a Runner's start leaves another Runner's run under a live claim running, and its preparation under a live claim preparing, and the holder finishes both. |
| `TestPostgreSQLARunnerStartedEarlierKeepsAnotherRunnersProfiles` (section 12, #282) | Built: a Runner opened before another registered keeps the other's profiles in the store when it registers, and holds both after; the other holds them once it reads the rows again; so for selected profiles; an id taken in one namespace through one Runner is refused to the other through the other, and another definition under a taken id is `409 input_profile_conflict`; registrations through both at once all land. |
| `TestPostgreSQLAnOccurrenceBindsAProfileRegisteredThroughAnotherRunner` (section 12, #282) | Built: a profile and a job whose release binds it, made through one Runner; a Runner opened before claims the job's event occurrence and submits its run, and a third opened before dispatches the run to the Runtime, each reading the profiles again rather than refusing the profile. |
| `TestPostgreSQLAnOccurrenceWithNoInputIsFailedOnceAndTheOtherRunnerGoesOn` (D18, #282) | Built: an accepted occurrence that holds no input is failed once by the Runner that claims it, journaled once, its claim released, and the other Runner's tick finds nothing to do and stays healthy. |
| `TestPostgreSQLAStoreErrorUnderAClaimLeavesTheOccurrenceToTheNextClaimant` (D18, #282) | Built: a store error under a Runner's claim is returned with the claim released and the occurrence accepted as read; the other Runner claims it and submits its run. The default kind's `TestAnOccurrenceWhoseJobOrReleaseIsUnreadableIsFailedAndTheAutomationGoesOn` and `TestAnAcceptedOccurrenceWithNoReadableExpiryIsFailedAndTheAutomationGoesOn` hold the failures of the occurrence's own records. |
| `TestPostgreSQLAFullQueueReleasesTheOccurrencesClaim` (D18, #282) | Built: with the run queue full, the Runner that claims an accepted occurrence finds no room, releases the claim and leaves the occurrence accepted as read, returning no error; once there is room, the other Runner claims it and submits its run. |
| `TestPostgreSQLRefusesAStoreWithoutTheSeveralRunnersFile` (section 5, #245) | Built: a store migrated to `0003` is refused at the start until migrated, and the migration then applies `0004_several_runners.sql` and the files after it. |
| `TestPostgreSQLRefusesAStoreWithoutTheSchedulesFile` (D15, #246) | Built: a store migrated to `0004` is refused at the start until migrated, and the migration then applies `0005_schedules.sql` and the files after it. |
| `TestPostgreSQLCreatingAJobWithAUsedCloudBindingIsRefused` (D9, #279) | Built: `POST /v1/jobs` with a cloud trigger whose binding a trigger of team-a holds is refused `422 cloud_binding_used` under team-b, team-a and none, and makes no job or trigger; `TestCreatingAJobWithAUsedCloudBindingIsRefused` holds the same on the sqlite kind. |
| `TestPostgreSQLHoldsTheProfileRowsToTheNoneScope` (section 7, #279) | Built: on the request login directly, a transaction held to a scope identifier, or setting none, inserts no profile row and updates none, by a plain update or an insert meeting the row's key; one held to the none scope does both. `TestPostgreSQLRefusesAStoreWithoutTheProfileRowsFile`: a store migrated to `0005` is refused at the start until `0006_profile_rows_unscoped.sql` is applied. |
| `TestPostgreSQLTwoRunnersCalledForOneSharedTickMakeOneOccurrence` (D17, #246) | Built: two Runners called for one shared tick at once, each having read the trigger at one version, record one occurrence between them, and neither fails; a later call for the tick on either records none, and one naming a scope identifier is refused. |
| The conformance cases of D17 and D18 | Built, on every kind: a write that read a trigger before another wrote it is `conflict`; an occurrence is claimed in the state read, by one claimant at a time; a holder's write is fenced by its claim and lease and keeps the claim only when asked; a request's write overrules the claim and the holder's next write is refused; the restart and the tick fail only the preparations no live claim holds. `TestADispatcherThatLosesARunReadsTheQueueAgainAtOnce` holds the worker's lost race. |
| h, D5, D9's own test, D11, D13, D14, section 5's marker, `TestPostgreSQLRoutesUseTheirLogins` | Not built. D9's rules are held by conformance cases on every kind; section 6's routes by `TestAKindThatHoldsScopesServesItsRoutesOnlyThroughTheirHeldView` and the end-to-end test. |

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
   as now; no for the second kind). (Built, #244's third slice: `heldTo(storeScope)`, the store
   as a kind opens it doing the work for every scope; `runClaim`, `runExpected` and
   `renewClaim`, the dispatcher's claim fencing its finish; `missingAttemptHoldsNoAudit`. The
   in-memory kind holds the scope's policy and the claim's lease by a clock of its own, and the
   suite tests both. The interruption's expectation, `runExpected.lapsed`, by which the restart
   interrupts only the runs no live claim holds (D3), and the fence of a `running` expectation
   that holds no claim, each with a conformance case, and one case over every read of a scope's
   rows, held to one scope (#263). The routes of section 6's table held so far, the tick's
   interruption, and the renewal loop and the margin are step 5's.)
4. The Runner's answers to the keys across scopes (D9): `taken` on a release's keys as
   `409 release_already_used`, on the cloud binding's index as `cloud_binding_used`. (The
   release's keys are built, #244's third slice: a `taken` names its key, `storeKey`, and
   `releaseTaken` answers it. The binding's answer is built in the fifth slice: `configureTriggerBy`
   answers the index's `taken` as its own check of the binding does, `422 cloud_binding_used`;
   `trigger_limit` is `422` too, which section 6 now states. `POST /v1/jobs` with a cloud trigger
   answers the index's `taken` so too, on both kinds, #279.)
5. The kind: its migrations and commands, the rows of section 3, its policies, the lease and its
   renewal, the attempts' volume rows (D11), the pinned Runtime directory (D12), the CI service
   container, and the tests of section 10. (Built, #244's fourth slice: the kind
   (`store_postgresql*.go`) with its two migration files, its digests and
   `jpack-runner store migrate`; selection by the service configuration's `store` member (D1);
   the policies of sections 6 and 7; the lease, its renewal every 15 seconds and the stop on a
   lost claim (`claims.go`), and the tick's interruption of lapsed runs; the routes of section 6
   listed there as held, every other route refused on the kind; the CI job; the tests section 10
   marks built. Left: the routes not yet held, the attempts' volume rows, the pinned Runtime
   directory, `store remove-scope`, the migration's row marker, the binding's answer, and the
   tests section 10 marks not built.)
6. The routes section 6 lists and step 5 left refused, each held to its scope as the table says,
   with a migration file of their own (`0003_held_routes.sql`) for the policies they need; a
   store at `0002` is migrated forward by `jpack-runner store migrate`, which the start requires.
   (#244's fifth slice, in progress: the journal and the run chain, with D16; a job's triggers,
   their preview, state and key, with the binding's answer of step 4; deliveries, occurrences and
   a run's retry, with `counts.pending-occurrences` and `occurrence_keys`. These routes needed no
   policy and no grant beyond `0002_grants.sql`: the request login already writes `triggers`,
   `trigger_revisions`, `occurrences`, `occurrence_keys`, `runs` and `counts`, and the ticks, the
   dispatch and preparation, on the worker login, write only what `0002` grants it; and the input
   and selected profiles, written held to none. Left: diagnostics, verification, briefs, the
   execution binding (D4), `GET /v1/background-connections` and the input preparation routes; one
   occurrence's lease across several Runners is #245's, section 12.)

SQLite's bytes and schema are not changed by any of it: the claim's columns and the tables of
section 3 are the second kind's own, and `store_sqlite_rows_test.go` holds the rows.

## 12. Several Runners on one store (#245)

#244 left the kind safe for one Runner per store: the Runner, not the kind, held in its process
what a second Runner on the store would race. #245's first step makes several Runners on one store
of this kind safe for the routes the kind serves, with no lock and no row per Runner: each piece
of shared work is held by a claim with a lease, in D3's shape, or fenced by a row version, in the
store. The sqlite kind is unchanged: its lock (`runner.lock`) still keeps one Runner per state
directory, and it holds no claim or version.

| What one Runner held in its process | On this kind, with several Runners |
|---|---|
| `automationMu` serializes every trigger write, a tick's and a request's | D17: each write expects the row version it read. |
| The trigger limit and the cloud binding, checked outside the write's transaction | D9's rows, `counts` and the binding's index, in the write's transaction (#244's fifth slice). |
| Which Runner prepares an occurrence, or dispatches a ready one | D18: the occurrence's claim. |
| The restart fails every occurrence held `preparing` | D18: the restart, and the dispatcher's tick, fail only those no live claim holds. |
| The restart interrupts every run held running | D3, built in #244: only those no live claim holds. |
| A dispatcher that lost a queued run waited a second | It reads the queue again at once (section 2, D3). |
| The profile rows, written whole from the process's copies | Each registration applied to the rows as its transaction reads them (#282): below. |
| The cloud bindings' problems, the profiles set aside, the unhealthy flag | Each Runner's own, as before; each reports its own health. |

**D17. A trigger's write against another's.** Every trigger write read the trigger under
`automationMu`, which holds one process. Two Runners' ticks of one due time each read the trigger,
and the second's write lands over the first's; so does a request on one Runner over a tick on
another. The schedule's identity (`schedule:<revision>:<due>`, unique per trigger) keeps the second
occurrence out, but the second tick's transaction then fails `taken`, which stops that Runner's
automation and reports it unhealthy. (a) A row version, `triggers.version`, read with the trigger
and expected by every `putTrigger`, which moves it on: a write that meets a write made since its
read is `conflict` and writes nothing. A tick writes its trigger first in its transaction, so a
tick that lost admits nothing and returns no error (`tickLost`); the next tick reads the trigger
again. A request answers `409 trigger_changed`, as for a revision changed since the caller read
it. (b) The record's revision as the expected value: a tick, the file cursor and the pending state
do not move it, so it fences none of them. (c) A lock row per trigger, taken for the tick: a
process-wide lock in another shape. (a) is D3's fence for a trigger; its cost is one column, and a
`409` for a request that met another Runner's tick between its read and its write, the caller's
revision still current, so that its reload reads the same revision. The sqlite kind holds no
version and reads 0, its lock keeping one writer; the in-memory kind holds one, so the suite
tests it.

**D18. An occurrence's claim.** `readyOccurrence` hands the dispatch the oldest accepted
occurrence, and `preparableOccurrences` hands out the preparations, to whichever Runner reads
them. A run's submission is idempotent by its request key, but two preparations of one occurrence
make two acquisitions, and a restart failed every occurrence held `preparing`, another Runner's in
flight among them. (a) A claim per occurrence, in D3's shape:

- **The claim.** `claimOccurrence(id, state, claim)` writes a token (16 random bytes, new for each
  claim) and a lease of 60 seconds on the occurrence's row, only in the state its claimant read and
  only while no live claim holds it (`claim_id IS NULL OR lease_until<=now()`); otherwise it is
  `conflict`, and the claimant passes the occurrence by. A Runner claims an occurrence before it
  dispatches it and before each step of its preparation. The readers that hand out work pass by an
  occurrence a live claim holds.
- **Every write is fenced.** `putOccurrence` takes an expectation (`occurrenceExpected`) beside the
  state: a write under a claim lands only while that claim holds the row and its lease is live by
  `statement_timestamp()`, as a run's finish (D3); a write under none lands only while no live
  claim holds the row, so a writer that is not the holder does not overwrite the holder's work; a
  request, a cancellation or a reconciliation, overrules the claim, still fenced by the state it
  read, and clears it, so the holder's next write is refused. A write keeps the claim only when
  asked (`keep`); otherwise it releases it. An occurrence read from the store carries no claim (the
  token is never read back).
- **The lease.** A legacy preparation's acquisition renews the lease every 15 seconds
  (`renewOccurrenceClaim`, by `holdLease`, D3's loop and margin); a renewal that fails stops the
  acquisition and the worker, as a lost run claim does. A durable preparation's step and a
  dispatch take no renewal: each is one bounded step (an operation call is held to 3 seconds) well
  inside the lease.
- **A refused write.** A claimant whose claim was taken, whose lease ran out, or whose occurrence a
  request ended writes nothing, and goes on with no error (`settleOccurrence`), as a dispatcher that
  lost a run does.
- **An error under the claim (#282).** A dispatch returns no error with the claim held. An
  occurrence whose own record cannot be dispatched, an accepted one that holds no input or no
  readable expiry, or whose job's or release's record does not read as one (`unreadable`), is
  failed (`failHeld`, class `internal`, journaled `occurrence.failed`) and its claim released with
  the write: every Runner's read meets the same error. An error of the store's or of the Runner's
  (the store not answering, a conflict, a timeout, a submission that fails otherwise than by a
  refusal) releases the claim, the occurrence written as read, before it is returned
  (`releasedWith`), as a dispatch the queue had no room for does. Kept, the claim would hand the occurrence to the next Runner one lease later,
  to meet the same error and stop that Runner's automation too.
- **Interruption.** The restart, and the dispatcher's tick (`interruptLapsed`), fail each
  occurrence held `preparing` that no live claim holds (`restartOccurrences`), with the reason the
  restart gives; one under a live claim is its holder's. An accepted or a waiting occurrence whose
  claim ran out is handed out again, and its dispatch or its next step is made by whichever Runner
  claims it; a run's submission made before is found by its request key, not made again.

(b) One row per Runner with a heartbeat. (c) An advisory lock. (a) is D3's, and the issue rules
out the others; its cost is two columns on `occurrences`, a claim's write before each dispatch and
each preparation step, and an occurrence left by a Runner that stopped worked again, or failed,
one lease later. The sqlite kind checks the state a claim expects and holds no claim, its lock
being the fence; the in-memory kind holds the claim by its own clock, so the suite tests it.

**D19. What each host keeps.** Each Runner keeps its attempt directories, its pinned Runtime and a
preview's scratch directory on its own host, and reads its project's files there (section 8).
(a) Each stays on its host, and no decision takes another host's state to be known:

- A retry of an interrupted run whose attempt directory the serving host does not hold is refused,
  `409 run_not_retryable` (D11, unchanged): another Runner's directory is a missing directory.
- A run's diagnostics, when the kind serves them (refused now, section 6), read the attempt
  directory on the host that serves the request, and read nothing for another host's attempt.
- Every Runner of one store pins the Runtime the held jobs' releases name and holds the same
  project (one image): an input read from the project is read on whichever Runner admits it.
- A file trigger reads its watched path on whichever Runner ticks: a file that differs between
  hosts reads as a change each time the tick moves from one to another. Several Runners serve a
  file trigger only on one host, or with the watched path on storage they share.
- Every Runner of one store runs one release: the start refuses a store whose migrations are not
  exactly the Runner's own (section 5), and a store is migrated with every Runner of it stopped,
  since a Runner of the earlier release writes no claim and no version.

(b) The attempt directories and the project on a durable shared volume. (c) The audit trail and
the files in the database. (a) builds nothing; (b) and (c) are shared storage, which this step
does not build. Its cost is the retry of a run interrupted on another host, refused where its
caller submits a new run, and a file trigger held to one host.

**D20. The clocks of several Runners.** Every lease is compared with the database's clock (D3,
D18), and each Runner's margin before its lease ends is its own monotonic clock, so no claim
depends on Runners agreeing on the time. These decisions read the wall clock of the Runner that
makes them, against a time that another Runner may have written by its own:

- a schedule's due time (`nextAt`, from the clock of the Runner that ticked last) and its lateness
  (more than 30 seconds late is missed, below);
- a file's stability (`pendingAt`, written by the Runner that saw the change, against
  `stableSeconds`, 1 to 3600);
- an occurrence's expiry in the queue (`expiresAt`, the admitting Runner's time and
  `queueSeconds`, 60 at least), and a run's from a trigger (`runQueueExpired`);
- a durable preparation's deadline (`preparationSeconds`, 60 at least), its next check and its
  ready window, and the next acquisition attempt (`nextAttemptAt`).

(a) A bound: the Runners of one store keep their wall clocks within 1 second of each other, which
any time service keeps with room to spare. Under it each decision above moves by at most that
second: a due time is ticked at most a second early, and never twice (D17); the 30 seconds of
lateness, and the queue's and a preparation's windows, of 60 seconds at least, move by at most
1 second; a file's stability window is shortened by at most 1 second, so a file trigger served by
Runners on several hosts takes `stableSeconds` of 2 or more. (b) Each such decision on the
database's clock, the tick reading `now()` once for its time: no bound, at the cost of a read per
tick and of ticks that differ between the two kinds. (c) One Runner ticks for every other, under
a lease on a row: a lock in another shape. (a) changes nothing; its cost is a requirement the
operator's page states, and journal entries whose times come from several clocks, so that their
times need not follow the journal's order, which is the order of commits (D13).

**The missed-tick rule with several Runners.** Each Runner's automation ticks every second while
it is healthy, and a due time is ticked by the first Runner whose clock reaches it, once (D17). The
trigger's `missed` rule (`skip` records the due time skipped, `missed`; `latest` admits the latest
due time, `latest-after-missed`) applies when that tick comes more than 30 seconds after the due
time by the ticking Runner's clock: when no Runner of the store ticked the trigger within 30
seconds, each being stopped, unhealthy, or refused its write by the store. A Runner that stops
makes no due time missed while another goes on. Under D20's bound a due time some Runner ticked
within 29 seconds of it is not missed.

**The installation's profiles (#282).** A Runner reads the registered and the selected profiles
at its start, and a registration wrote the row whole from the Runner's copy, so that one on a
Runner whose copy was older than the row wrote the newer profiles out of it. (a) The merge: a
registration, which adds one profile to one row, reads both rows in its transaction
(`currentProfiles`) and is applied to them, the immutability of an id, the two namespaces' ids
and the bounds checked against them, and the Runner's copy is then the rows as written. (b) An
expected value, the row's bytes as the Runner last read them, a stale write answered `409`: it
refuses a registration that the rows as read admit, and the caller sends it again unchanged. (a)
is chosen: rows only grow and a profile is immutable, so applying a registration to the current
rows is what the caller asked for, and no schema change is needed. Two registrations at once are
serialized by the transaction (section 2), the second reading the first's row. Both kinds behave
alike for one Runner: its rows are its copy. A profile stored since the Runner opened the store
that it would set aside, which only a Runner of another release stores (D19), refuses the
registration, since the profiles set aside are fixed from the start. A Runner's copy is read again
at each registration through it and, on this kind, whenever a binding names a profile the copy
neither holds nor sets aside (`refreshProfiles`), before it is refused: every binding first
passes the refusal of a set-aside profile (`refuseSetAsideIDs`), and the read is made there, so a
release, a job or its upgrade, a trigger's configuration, preview or occurrence, planning and
preparation, a run's submission and its start, through any Runner, bind a profile registered
through another; selected planning reads it as well. A set-aside refusal is given without the read,
which cannot change it. The read is made outside the registration lock, and a copy is replaced
only by a longer row, so that a registration through the Runner meanwhile is kept. The listings
are the Runner's copy, and show another Runner's registration once the copy is read again.
`TestPostgreSQLARunnerStartedEarlierKeepsAnotherRunnersProfiles`,
`TestPostgreSQLAnOccurrenceBindsAProfileRegisteredThroughAnotherRunner` and
`TestARegistrationKeepsAProfileWrittenSinceTheRunnerReadTheRow` hold it.

**As built.** `0004_several_runners.sql` adds `triggers.version` and `occurrences.claim_id` and
`lease_until`, each with a default, and no policy and no grant: `0002_grants.sql`'s table
privileges cover the columns. A store without it is refused at the start until it is migrated.
The contract's changes: `putTrigger` expects `Trigger.version`; `putOccurrence(o, expected)`;
`claimOccurrence` and `renewOccurrenceClaim`; `readyOccurrence` and `preparableOccurrences` pass
by a live claim; `restartOccurrences` spares it; the dispatcher's tick fails lapsed preparations
in the transaction that interrupts lapsed runs. The tests are section 10's.

## 13. Not in this issue

Beyond #245's first step (section 12): several Runners on the sqlite kind; shared storage for the
attempt directories and the project (D19 (b) and (c)); a refresh of a Runner's profiles before
each listing of them (section 12); the routes the kind refuses; which scopes a caller may name;
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
| D16 | A refused creation in another scope's journal | Read only held to none, by a restrictive policy; journaled under the scope the body names |
| D17 | A trigger's write against another's | A row version every write expects and moves on; the record's revision; a lock row per trigger |
| D18 | An occurrence's claim | A lease per claim in D3's shape, fencing every write, a request overruling it; a heartbeat per Runner; an advisory lock (ruled out) |
| D19 | What each host keeps | Each on its host, no decision taking another host's state as known; a durable shared volume; the trail and the files in the database |
| D20 | The clocks of several Runners | A bound of 1 second between the Runners' clocks; the database's clock for every such decision; one Runner ticking for all |
