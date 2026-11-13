# The Runner's store behind an interface

Issue #173. The Runner keeps its durable state in one SQLite database, in the state directory
the boot line names, under an exclusive process lock. The Desk's operational jobs plan
(`docs/design/operational-jobs-plan.md` in judgment-pack-desk) names storage and dispatch as the
seams to define for use beyond a single host, without promising a free migration to distributed
execution. This note puts the queue, releases, inputs and run records behind one contract, with
SQLite as the kind that ships, its statements, transactions, pragmas and lock unchanged (section 8
names what differs), an in-memory kind for tests, and a conformance suite both kinds run.
It states each operation's semantics as the SQLite code has them; it is not a new design.

Code: `internal/runner/store_contract.go` (the contract, the faults, the kind registry),
`internal/runner/store_sqlite.go` (the sqlite kind), `internal/runner/store_memory_test.go` (the
in-memory kind) and `internal/runner/store_conformance_test.go` (the suite).

## 1. What the contract covers

| Area | Kept by the sqlite kind in | What it is |
|---|---|---|
| The queue | `runs` (`seq`, `state`) | A run's state is its place in the queue: `queued`, `running`, then `completed`, `failed` or `interrupted`. The queue is served by `seq`, the order runs were queued. |
| Inputs | `runs` (`job_id`, `caller`, `idem`, `request_digest`, `record`) | The input snapshot frozen into a run's record at admission, held under the request key (job, caller, idempotency key) with the digest of the request that made it. An occurrence's input, held until its run is queued, is kept with the occurrence (section 9.5). |
| Releases | `releases` | A release a preview made, kept once and never changed. |
| Run records | `runs` (`record`), `run_chain`, and the `events` entries written with them | A run's record, the installation's chain of runs, and the journal entries the Runner writes through the contract (a run's or a release's change, the Runner's start and stop), each in the transaction of what it records. |
| Jobs | `jobs`, `job_creations`, `job_revisions` | The job a run is queued for, made once from its release and revised once per revision, each revision naming a release no other names (section 9.5). |

## 2. Operations

Every write is made inside `update`, one transaction. The reads in the first table are answered
both outside a transaction, each one statement, and inside one, where they see the transaction's
own writes.

| Outside or inside a transaction | The sqlite kind's statement | Semantics |
|---|---|---|
| `release(id)` | `SELECT record FROM releases WHERE id=?` | The release, or `absent`. A record that does not decode is `unreadable`. |
| `job(id)` | `SELECT record FROM jobs WHERE id=?` | The job, or `absent`. |
| `requestedRun(job, caller, key)` | `SELECT id,request_digest,record FROM runs WHERE job_id=? AND caller=? AND idem=?` | The run's id, the request's digest and the run's record, decoded only when asked, so a caller compares the digest first, as it did; the decoding's error is `decodeRun`'s own. `absent` when nothing is held under the key. |
| `entrySequence(run)` | `SELECT sequence FROM run_chain WHERE run=?` | The sequence of the run's entry, or `absent`. |
| `run(id)` | `SELECT record FROM runs WHERE id=?` | The run, or `absent`. Inside a transaction, the run as the transaction holds it (later, with [retries](retries.md), so that a retry's checks and its write read one record). |

| Outside a transaction | The sqlite kind's statement | Semantics |
|---|---|---|
| `oldestQueued()` | `SELECT record FROM runs WHERE state='queued' ORDER BY seq LIMIT 1` | The run queued first among those still queued, or `absent`. The order is the order of queueing, never the ids'. |
| `entryLine(run)` | `SELECT line FROM run_chain WHERE run=?` | The entry's exact line, or `absent`. |
| `chainPage(after, limit)` | `SELECT sequence,line FROM run_chain WHERE sequence>? ORDER BY sequence LIMIT ?` | Up to `limit` lines after `after`, in order of sequence, and the last sequence read (`after` when none). Entries are only appended, so pages read in turn are a prefix of the chain as it stood at some moment of the reading. |
| `update(fn)` | `BEGIN` ... `COMMIT` | Runs `fn` in one transaction. What `fn` wrote is kept only if `fn` returns nil and the commit succeeds. `fn`'s error is returned as `fn` gave it. Transactions are serializable. |
| `close()` | | Closes the database, then gives the lock up. |

| In a transaction | The sqlite kind's statement | Semantics |
|---|---|---|
| `activeRuns()` | `SELECT count(*) FROM runs WHERE state IN ('queued','running')` | The runs queued or running. |
| `createRun(r, caller, key, digest)` | `INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (...)` | Queues a run, created once: `taken` when its id or its request key is held. |
| `putRun(r, expected)` | `SELECT state FROM runs WHERE id=?`, then `UPDATE runs SET state=?,record=? WHERE id=?` | Writes the run's state and record, and returns the state it held. With `expected` other than `anyState`, the run must be held in that state, or nothing is written and the fault is `conflict` (section 4). `absent` when no run is held under the id. |
| `runningRuns()` | `SELECT record FROM runs WHERE state='running' ORDER BY seq` | The runs left running, in the order they were queued. |
| `lastEntry()` | `SELECT sequence,line FROM run_chain ORDER BY sequence DESC LIMIT 1` | The chain's last entry, or `absent` when it holds none. |
| `createEntry(sequence, run, line)` | `INSERT INTO run_chain(sequence,run,line) VALUES (?,?,?)` | Appends an entry, created once: `taken` when the sequence or the run has one, `refused` for a sequence below 1. |
| `createRelease(r)` | `INSERT INTO releases(id,record) VALUES (?,?)` | Keeps a release, created once: `taken` when its id is held. |
| `appendJournal(c)` | `INSERT INTO events(...)` (`appendEvent`) | Appends a journal entry and returns its sequence. |
| `raiseSchema(schema)` | `UPDATE metadata SET value=? WHERE key='schema' AND value IN (?,?)` | Later, with [retries](retries.md): marks the store as holding records only a Runner that knows `schema` keeps, so that no earlier Runner opens it. `refused` for a schema the kind does not know; the sqlite kind knows `"6"`. |

### The Runner's operations over the contract

Each is the transaction it was, with the same statements in the same order.

- **Enqueue** (`submitInternalIntent`). One transaction: the request key; when a run is held under
  it, the run is returned if the request's digest is the same (an identical retry), and the
  transaction ends keeping nothing (`errKeepNothing`, the rollback it was), or the request is
  refused with 409 `idempotency_conflict`. Otherwise the job, its release, the Runner's checks
  (the manual-admission binding, the reviewed mapping), `activeRuns` against the limit of 100
  (429 `queue_full`), `createRun`, and the `run.queued` entry. A mapping-v2 request reads the key
  once before, outside the transaction, as it did.
- **Claim** (`dispatchNext`, `changeRun`). `oldestQueued` outside a transaction, then one
  transaction: `putRun` to `running` with `queued` expected, and the `run.started` entry. A run
  that expired in the queue, or whose release names a profile set aside, ends `failed` the same
  way, with `queued` expected.
- **Complete, fail, interrupt** (`finishRun`). One transaction: for a completed run, its entry in
  the chain (`appendRunEntry`: `lastEntry`, the next entry, `createEntry`) and its sequence
  (`entrySequence`); then `putRun` whatever the state held, and the entry of the run's end. A
  completion without its record's exact bytes is refused with `errNoAuditBytes` and nothing is
  written.
- **Retry.** None. A run is never queued again; a resubmission under the same key returns the
  run, and one under a new key is a new run. (Later, with [retries](retries.md): `retryRunBy`.
  One transaction: the run read with `run`, its checks (its state, the attempt named, its audit
  record in the record and in every attempt's directory, the bound of attempts, the trigger's
  deadline), `activeRuns` against the limit, `raiseSchema`, `putRun` to `queued` with
  `interrupted` expected, keeping the interrupted attempt in the record, and the `run.queued`
  entry from `interrupted`. A repeated request reads the run and writes nothing.)
- **Restart** (`restart`). One transaction, under the lock: the `runner.started` entry, every run
  `runningRuns` reads ended `interrupted` with an entry naming the start, and every occurrence
  `preparingOccurrences` reads ended `failed`, with an entry whose window starts at its last
  `occurrence.preparing` entry (`lastPreparing`).
- **Release** (`saveRelease`). One transaction: `createRelease` and the `release.previewed` entry.
- **Stop** (`recordStop`). The `runner.stopped` entry; a failure is not reported, as before.

## 3. Faults

Every failure a contract operation reports is a `storeError` carrying one fault. Its text stays
its cause's, so every message reads as it did, and `errors.Is` reaches the cause: the sqlite
kind's `absent` still answers `errors.Is(err, sql.ErrNoRows)`. `absent(err)` answers for both
kinds and for the SQLite statements outside the contract.

| Fault | Meaning | sqlite kind | in-memory kind |
|---|---|---|---|
| `absent` | Nothing is held under the id or key. | `sql.ErrNoRows` | Nothing is held. |
| `taken` | A create-once id or key is held. Nothing is written. | `SQLITE_CONSTRAINT_PRIMARYKEY`, `SQLITE_CONSTRAINT_UNIQUE` | The id, key or sequence is held. |
| `conflict` | A versioned update found another state than expected. Nothing is written. | `putRun`'s comparison | The same comparison. |
| `durability` | A change was not made durable: a write statement, `BEGIN` or `COMMIT` failed. Never a success. | Any other failed write, a failed `BEGIN` or `COMMIT` | Its commit seam. |
| `unavailable` | A read failed: the store did not answer, and nothing is known of what it holds. Never a record, and never none. | Any other failed read | None: it holds what it holds. |
| `unreadable` | A record does not decode. | A decoding error | A decoding error. |
| `refused` | An argument outside the contract, refused before anything is written. | `SQLITE_CONSTRAINT_CHECK` (a chain sequence below 1); an unknown kind; a schema not raised through the contract | A chain sequence below 1. |

An error the Runner's own function returns inside `update` (an `apiError`, `errNoAuditBytes`,
`errManualProtocol`) is returned unchanged. Opening a store is the kind's own: the sqlite kind's
opening errors (the lock, its files, the schema, the store's identity, the migration) are returned
as they were.

## 4. Durability, order, exclusivity, versions

**Durability.** The sqlite kind runs in WAL mode with `synchronous=FULL`: a transaction is on disk
when its `COMMIT` returns, and `update` reports success only after it. A failed statement keeps
nothing of its transaction; a failed `COMMIT` is `durability`, and the caller takes nothing as
kept. By operation: an enqueued run and its entry are durable together before the submission is
answered; a claim's `running` is durable before the Runtime is invoked; a run's end, its chain
entry and its journal entry are durable together, and a failure stops the dispatcher, after which
the run reads interrupted at the next start; a release and its entry are durable together before
the preview is answered; a restart's entries and interruptions are durable together before the
dispatcher starts. The in-memory kind keeps nothing past its process.

**Order.** The queue and the runs left running are read by `seq`, SQLite's `AUTOINCREMENT`
column: the order of queueing. The chain is read by sequence. The journal's sequence is its own
`AUTOINCREMENT`.

**Exclusivity.** Transactions are serializable. The sqlite kind holds one connection
(`SetMaxOpenConns(1)`), so one transaction runs at a time in the process, and the exclusive lock
keeps every other process out. The in-memory kind holds its own lock from a transaction's start to
its end and works on a copy, which replaces its state only at the commit.

**The claim's fence.** A run leaves `queued` once and never returns to it, so its state is the
claim's fence: `putRun` with `queued` expected writes only if the run is still queued, and a
second writer that read it queued meets `conflict` and writes nothing. Before this change the
claim was exclusive because one dispatcher runs per store, under the lock; it still is, and the
comparison, made between the two statements the claim already made, never refuses it. A claim
does not expire, so there is no lease: a run left running is ended by the next process's restart,
whose lock proves the last dispatcher is gone. A kind for use beyond one host would need a lease
with fencing for the claim; this contract has none.

Later, with [retries](retries.md), an interrupted run can return to `queued` when a caller asks:
`putRun` with `interrupted` expected, so a second retry that read it interrupted meets `conflict`.
A retry reads the run inside its transaction, so its checks and its write see one record; the
`interrupted` expected by `putRun` remains as the fence the contract states. The claim's fence is unchanged: a claim reads and writes in one transaction, so no claim carries a
read from an earlier time the run was queued, and each time it is queued it is claimed once.

**Versions.** The only record the contract updates is a run, and its state is its version.
Releases, chain entries and journal entries are created once and never changed. A job is revised
once per revision: `reviseJob` keeps the revision under its key, so a second writer of one
revision is `taken` (section 9.5).

## 5. The kind registry and the boot line's directory

`storeKinds` names the kinds a store can be opened as; it holds `sqlite`, which is
`defaultStoreKind`. The boot line names no kind, so every Runner opens `sqlite`, and no other kind
is selectable from the boot line. The in-memory kind is in a test file and is not registered.

The boot line's directory reaches the store only through the sqlite kind: `openSQLiteStore`,
which takes the lock (`runner.lock`) and opens the database (`runs.sqlite`), and
`sqlitePreparedStore`, which checks that a manual-admission store was prepared. The rest of the
Runner keeps the queue, releases, inputs and run records through the contract. Opening keeps its
order: the lock; the Runner's working directories (`storeOpening.afterLock`); the database with
one connection; the manual-admission state handed to the Runner, and its check of a
manual-admission store (`storeOpening.manual`); the pragmas and the schema; the store's identity
(workspace, owner, input root); the migration. Under manual admission, `openStore` refuses a kind
that hands over no manual-admission state, which would leave the store unchecked.

The directory is also the Runner's working directory for what is not the store: the files a
Runtime process runs on and writes (`releases/`, `attempts/`, `runtimes/`,
`workflow-attempts/`, `version-check-*`), the manual-admission marker and the executor lease, the
check that a signing key is kept outside it, and the health route's `stateDirectory`. A Runtime
launched as a process needs those as files on its host; they stay where they are.

## 6. What stays SQLite-specific

The kind itself: the exclusive process lock (`flock` on `runner.lock`), WAL, `synchronous=FULL`,
the busy timeout, one connection, the schema and its migration, and the store's identity in
`metadata`.

The kind gives the Runner no database handle. What the contract does not cover is kept in the
kind's files, each statement as the Runner made it, and reached through types of the kind's own:

- The manual-admission state (`store_sqlite_manual.go`): the manual-admission journal
  (`manual_attempts`), the demand and schema in `metadata`, and the reads manual admission checks
  a store and a job by. The kind hands it to the Runner at opening as a `manualState`; it is the
  sqlite kind's alone ([store-postgresql.md](store-postgresql.md), D4), and a Runner under manual
  admission on a kind that hands over none is refused (section 5).
- The programs that open the kind's database themselves, offline, each under the store's lock or
  the executor lease: workspace relocation (`relocate.go`, the history in `metadata`) and manual
  store preparation, its own process, through a held directory and its private file system layer
  (`store_sqlite_manual_process_unix.go`). Both open the database through the kind
  (`openSQLiteRelocation`, `openSQLiteManualPreparation`) and ask it the counts they ask.
- The workflow store (`store_sqlite_workflows.go`: `workflow_executions`, `workflow_artifacts`).
  It stays outside the contract: no route reaches it, its tables are made only where it is
  opened, and the second kind has none. It moves behind the contract when a route reaches it.
- The source worker's database (`store_sqlite_sources.go`: `sources.sqlite`). It stays outside
  the contract: it is not the Runner's store but the worker's own, in its own directory and
  process, under its own lock, as caller custody beside the Gateway. A second kind for it would be
  a second worker store, not a Runner's.

`TestNoStatementOutsideTheSQLiteKind` holds this, read from the module's source and not from a
list: outside the files `store_sqlite*.go` of `internal/runner`, no file imports `database/sql` or
a sqlite package, or holds statement text in a string, and no file of the package selects a member
the kind declares over `database/sql` or names a function, variable or type it declares over it.
With #244, the journal's reads, refusals, profile rows, briefs, triggers, occurrences, jobs as they
are made and revised, and the job and run listings are the contract's (section 9.5).

## 7. The in-memory kind and the conformance suite

The in-memory kind keeps the contract's semantics in memory: records as their encoded bytes,
through the same encoding as the sqlite kind; runs in the order queued; a transaction on a copy
under the kind's lock, which replaces the state at the commit; and a commit seam that fails every
commit while set.

`TestStoreKindsConform` runs every case against both kinds. A case uses only the contract and the
Runner's functions over it (`changeQueuedRun`, `finishRunIn`, `appendRunEntry`); what the
contract does not cover (a job made outside it, the journal's entries, a failed commit, a record
that does not decode) it asks of the kind under test.

- A release is created once.
- A run is queued once under its id and once under its request key.
- The queue is served in the order runs were queued, with ids that sort against it.
- A run read queued is claimed once; an update expecting another state writes nothing.
- An interrupted run is queued again once, keeping its attempts, and is claimed again; a schema
  the kind does not know is `refused`.
- Concurrent claimers grant every run once and lose none, with producers queueing at the same
  time; a claimer that meets `conflict` reads the queue again. The claimers share nothing but the
  store.
- A transaction that fails keeps nothing, run, entry, release or chain entry.
- A commit that fails is `durability` with its cause's text, never a success, and keeps nothing.
- The chain gives a run one entry and is read in order; a completion without its record's bytes
  is not recorded.
- Chain entries are read in order of sequence whatever order they were made in, under runs whose
  ids sort against it.
- Nothing held reads `absent`.
- A record that does not read is `unreadable`.
- The sqlite kind only: a reopened store holds what it held (a claimed run still running, a
  completed run with its entry, the queue's order, the release, the request, the journal), and one
  process holds the store at a time.

The suite runs in the `unit-tests` step, under `go test -race ./...`, and the step then requires
both kinds' cases to have run and passed.

## 8. What changed in behaviour

- A queued run's change (`changeRun`) now requires the store to hold the run queued. With one
  dispatcher per store it always does.
- `Close` closes the attempts directory before the store, where it closed it between the database
  and the lock. The lock is still given up last.
- Errors from the contract's operations carry a fault; their text and what `errors.Is` reaches are
  unchanged.
- With #244 (section 9.5): a brief's command is read from the request before the brief's
  transaction, not inside it, so that the transaction reads nothing but the store. A request whose
  body is refused and whose subject's stored history does not decode now answers the body's
  refusal first.
- With #244: writing a trigger that is not held (`putTrigger`) is `absent`, where the statement
  changed no row and the write passed; every writer writes a trigger it read in the same
  transaction or under `automationMu`, so none meets it. A trigger configured through another
  job is refused as `absent`, as it was refused as SQL's no-rows error. A dispatched occurrence
  reads its run's id alone through `requestedRun`, as it did: a run whose record does not decode
  holds up no automatic occurrence. A record that does not decode, of a trigger or an
  occurrence, carries `unreadable`.
- With #244: a read that fails carries `unavailable`, and a record that does not decode
  `unreadable`; the contract before #244 gave both `unreadable`. Only an upgrade's receipt tells
  them apart: a revision whose record does not decode is compared by its digest first, as it
  was, and a read that failed answers `500 store_error`, as it did.
- With #244: revising a job that is not held (`reviseJob`) or a trigger that is not held
  (`reviseTrigger`) is `absent`, where the statement changed no row and the next one went on; a
  revision reads the job in its own transaction, so none meets it. A job's creation whose
  release made a job before, and whose record does not decode, answers no job beside its error,
  where it answered the part that decoded. Neither changes a statement or a row.
- With #244, every transaction's function reads only the store, its arguments and the clock, and
  keeps what it makes only once it ends, so that a kind may run it again
  ([store-postgresql.md](store-postgresql.md), section 2). What a transaction read elsewhere is
  now read before it: a retry's audit check of the attempt's directory, a job's creation's and
  an upgrade's profiles set aside or selected, and an upgrade's check of a gateway mapping. Each
  answers as it did when nothing changes between that read and the transaction. A retry's
  directories are read only on a run read interrupted, and their answer is given only for that
  interruption (`sameInterruption`: the same attempt, begun and ended at the same instants).
  Otherwise the retry is answered as it was at the read before: a run read still running is
  refused `409 run_not_retryable`, where a run interrupted by the transaction's read was checked
  then; a run whose attempt changed between them is answered `409 attempt_mismatch`, as a retry
  of an attempt no longer held is. A read before that fails is answered as a failed read was,
  `500 store_error`, and is not journaled as a refusal. No statement or row changes.
- With #244: a Runner configured for manual admission is refused at opening with the manual
  protocol error when its store's kind hands over no manual-admission state. The sqlite kind
  hands it over, so no Runner today meets it. The errors of manual admission, its preparation
  and relocation keep their text and their answers; the database's errors now carry a fault.

## 9. Everything a Runner keeps

Issue #244 puts everything a Runner keeps behind the contract, so that a second storage kind
([store-postgresql.md](store-postgresql.md)) is a kind and not a second Runner. This section is
the inventory it starts from: every piece of state a Runner writes, read from the statements on
main at v0.31.0 (found by searching every non-test file of `internal/runner` for `INSERT`,
`UPDATE`, `DELETE`, `CREATE` and for paths joined to the state directory), not from this note's
earlier sections.

"Written by" names the writers at v0.31.0, by file and line, or the contract operation that now
holds the statement. "Behind the contract" is the state with this slice of #244 (section 9.5). "Scope" says whether a row carries a scope
identifier ([scope-identifiers.md](scope-identifiers.md)) a second storage kind can key a row
policy on. "Append-only" says whether a row, once written, is never changed or removed by the
Runner.

### 9.1 The database (`runs.sqlite`)

| State | Table (columns) | Written by | Behind the contract | Scope | Append-only |
|---|---|---|---|---|---|
| The store's identity and schema | `metadata` (`workspace`, `owner`, `inputRoot`, `schema`) | Opening and migration (`store_sqlite.go:161`, `:241`); the scoped and the retry schema raised at a job's creation (`raiseSchema`; at v0.31.0 `scope.go:278`, `retries.go:33`) | Identity and migration: the kind's own at opening. The raises: yes (`raiseSchema`) | No | No: `schema` is raised |
| The profile registry and the selected profiles | `metadata` (`registered-input-profiles-v1`, `selected-input-profiles-v1`) | The registry routes (`profile_registry.go:231`, `selected_profiles.go:238`) | Yes (`profileRow`, `putProfileRow`) | No | No: replaced |
| The manual-admission demand | `metadata` (`manualExecutionDemand`; schema `4`) | Manual store preparation, its own program (at v0.31.0 `manual_store_prepare.go:229`; now `prepareManual`) | No: the sqlite kind's offline programs (section 6) | No | No |
| The relocation history | `metadata` (a history key) | Offline relocation (at v0.31.0 `relocate.go:152`; now `relocate`) | No: the sqlite kind's offline programs (section 6) | No | No |
| Briefs | `briefs` (`subject`, `record`) | The brief routes, upsert (`briefs.go:106`) | Yes (`brief`, `putBrief`) | No: the subject (`job:<id>`, `run:<id>`) carries it | No: replaced |
| Releases | `releases` | A preview (`createRelease`) | Yes | No | Yes |
| Jobs | `jobs` (`seq`, `id`, `release_id`, `record`) | Creation (`createJob`; at v0.31.0 `store.go:422`); revision (`reviseJob`; `job_revisions.go:189`) | Yes | Yes: `record.scope` | No: revised in place |
| Job creations | `job_creations` | Creation (`createJob`; at v0.31.0 `recordInitialJob`, `job_revisions.go:48`); migration (`jobRevisionSchema`) | Yes, but the migration | In the record | Yes |
| Job revisions | `job_revisions` (`request_digest`) | Creation (`createJob`; at v0.31.0 `job_revisions.go:51`); revision (`reviseJob`; `:192`); migration (`jobRevisionSchema`) | Yes, but the migration | In the record | Yes |
| Triggers | `triggers` (`record`, `key_hash`, `observed`, `pending`, `pending_at`, `key_revision`) | Creation with a job (`store.go:415`) and alone (`createTrigger`); `putTrigger` for a state change, a key rotation, the schedule and file ticks and the cloud binding; a job's revision (`reviseTrigger`; at v0.31.0 `job_revisions.go:205`) | Yes | Yes: `record.scope` | No |
| Trigger revisions | `trigger_revisions` | `putTrigger` with history; a job's revision (`reviseTrigger`; at v0.31.0 `job_revisions.go:208`) | Yes | In the record | Yes. Never read by the Runner |
| Occurrences, their attempts and `nextAttemptAt` | `occurrences` (`identity`, `payload_digest`, `state`, `record`) | Admission (`createOccurrence`); every state change, preparation, retry and expiry, and the restart (`recordOccurrence` through `putOccurrence`) | Yes | Yes: `record.scope` | No |
| Occurrence keys | `occurrence_keys` | An event's admission (`createOccurrenceKey`) | Yes | No: keyed by its occurrence | Yes |
| Runs (the queue, inputs, records) | `runs` (`state`, `record`) | `createRun`, `putRun` | Yes | Yes: `record.scope` | No: the state advances |
| Idempotency keys | `runs` (`job_id`, `caller`, `idem`, `request_digest`) | `createRun` | Yes | Through the run | Yes: a key is never bound again |
| The chain of runs | `run_chain` | `createEntry` | Yes | No | Yes |
| The journal | `events` | `appendEvent` (`events.go:284`): through `appendJournal` for runs, releases, jobs, triggers, occurrences, admission refusals (`journalRefusal`) and the Runner's start and stop; directly only for the journal's first entry at opening | Yes; every read through `journalPage` | Yes: `detail.scope` when the job has one | Yes |
| The manual-admission journal | `manual_attempts` | Manual mode (at v0.31.0 `manual_admission_journal.go:46`, `:52`; now `createManualAttempt`, `replaceManualAttempt`) | No: the sqlite kind's manual-admission state (section 6) | No | No: an attempt is updated |
| Workflow executions | `workflow_executions`, `workflow_artifacts` | `store_sqlite_workflows.go` (at v0.31.0 `workflow_store.go`), reached by no route; its tables are made only where it is opened | No (section 6) | No | No |

`sqlite_sequence` is SQLite's own. The source worker keeps a second database, `sources.sqlite`
(`worker_metadata`, `source_operations`), in its own process under its own lock
(`source_worker.go`, its statements in `store_sqlite_sources.go`; section 6).

### 9.2 Files under the state directory

| State | Path | Written by | Read later by | Survives a restart on another host |
|---|---|---|---|---|
| The store's lock | `runner.lock` | The sqlite kind (`flock`) | The kind | Not needed: a second kind has its own exclusivity |
| A preview's working directory | `releases/<id>/` | A preview (`runtime.go:258`) | Nothing: the release's record holds the pack, configuration, lock, validation, preview and tests | Not needed |
| An attempt's directory | `attempts/<run>[.<n>]/` | A run's evaluation (`store.go:720`); manual mode (`manual_admission_journal.go:117`) | The diagnostics route (`diagnostics.go:106`); a retry's audit check (`attemptsHoldNoAudit`, `retries.go:406`) | No. A missing directory reads as holding no audit record |
| The pinned Runtime | `runtimes/<digest>` | At start (`pinExecutable`, `runtime.go:81`) | Every evaluation, by the release's `runtimeDigest` (`runtime.go:357`, which refuses a missing or changed file); manual binding (`manual_execution_binding.go:59`) | No. A missing file is refused |
| Workflow attempts | `workflow-attempts/` | `workflow_host.go:29`, reached by no route | | |
| A version check's copy | `version-check-*` | The upgrade check, removed after | Nothing | Not needed |
| The manual-admission marker and the executor lease | `manual-execution-prepare.json`, the lease | Manual mode | Manual mode | Manual mode is single-host |
| The source worker's state | `sources.sqlite`, `source-worker.lock`, `source-worker.token` | The source worker | The source worker | Its own process |

### 9.3 State held in the process

Not durable and not in the store, so not a storage kind's concern, but each assumes one Runner per
store (issue #245): `automationMu`, which serializes the trigger ticks; the occurrences being
prepared (`preparing`); the profile registry's and the selected profiles' caches; the cloud
bindings' problems; the profiles set aside; and the unhealthy flag the dispatcher raises on a
failed commit.

### 9.4 Where the issue's premise is imprecise

- Jobs are read through the contract already (`job`); only their creation and revision are
  outside it.
- The journal's writes for runs, releases and the Runner's start and stop are inside the contract
  already. Outside are the writes for jobs, triggers, occurrences and refusals, and every read.
- Runs are inside, except four statements: the listings, an occurrence's run by its request key
  (`triggers_dispatch.go:254`), the admission count over runs and occurrences
  (`triggers_store.go:533`), and manual activation.
- A release's record is in the database, through the contract. Only a preview's working directory
  is on local disk, and nothing reads it after the preview.
- The pinned Runtime and the attempt directories are on local disk, as the issue says. The
  pinned Runtime fails closed when missing; an attempt directory does not (section 9.2), which is
  the one that bears on a restart on another host.
- Outside the premise and outside the contract: briefs, the profile registry's and selected
  profiles' metadata rows, the scoped schema raise, and the manual-admission and workflow tables.
  `trigger_revisions` is written and never read.

### 9.5 What #244 puts behind the contract

Each operation keeps the statement it replaces, moved into the sqlite kind
(`store_sqlite.go`, `store_sqlite_triggers.go`, `store_sqlite_jobs.go`, `store_sqlite_lists.go`,
`store_sqlite_journal.go`, `store_sqlite_schema.go`);
the in-memory kind implements it; and `store_conformance_widened_test.go`,
`store_conformance_triggers_test.go`, `store_conformance_jobs_test.go` and
`store_conformance_lists_test.go` hold the cases every kind runs.

| Operation | Where | The sqlite kind's statement | Semantics |
|---|---|---|---|
| `journalPage(filter, range)` | Outside a transaction | `SELECT ... FROM events WHERE sequence>? AND (condition) ORDER BY sequence LIMIT ?`, or `... ORDER BY sequence DESC` backward | Up to `limit` entries the filter keeps, in the range's order, and whether another follows, read and not decoded. The filter (`journalFilter`) is a job (its entries, and those of its revisions' releases that name no job), the Runner's own entries beside a job's, a scope identifier compared byte for byte, and the outcomes, error classes and kinds of [run results](run-results.md): each kind applies it where it reads, so that a page holds only what it keeps. |
| `journalBegan()` | Outside | `SELECT at FROM events WHERE kind='journal.began' ORDER BY sequence LIMIT 1` | When the journal began, or `absent`. |
| `lastRefusal(key)` | In a transaction | `SELECT at FROM events WHERE refusal=? ORDER BY sequence DESC LIMIT 1` | The newest refusal under the key, or `absent`. A refusal is journaled (`journalRefusal`) in one transaction with this read, once per key in its window. |
| `recordScope(records, id)` | Both | `SELECT coalesce(json_extract(record,'$.scope'),'') FROM <records> WHERE id=?` | The scope identifier of a job, run, trigger or occurrence, `""` for none, or `absent`; `refused` for another kind of record, so that no other name reaches the statement. |
| `profileRow(key)`, `putProfileRow(key, row)` | Both; in a transaction | `SELECT value FROM metadata WHERE key=?`; `INSERT ... ON CONFLICT(key) DO UPDATE` | The registry's and the selected profiles' rows, read and written whole, as written; `refused` for any other key, so that no metadata row of the store's identity or schema is reached. |
| `brief(subject)`, `putBrief(subject, record)` | Both; in a transaction | `SELECT record FROM briefs WHERE subject=?`; `INSERT ... ON CONFLICT(subject) DO UPDATE` | A subject's brief history, as the bytes written. |
| `raiseSchema(schema)` | In a transaction | `raiseScopedSchema`, `raiseRetrySchema`, both now the kind's | Now also `scopedStoreSchema`, from `storeSchema` only; a store at a later schema keeps it. A job's creation raises both through it. |
| `trigger(id)` | Both | `SELECT record,key_hash,observed,pending,pending_at,key_revision FROM triggers WHERE id=?` | The trigger with its file cursor and key revision, and its key's digest, or `absent`. |
| `triggerCount()`, `cloudBindings(except, connection, job)` | Both | `SELECT count(*) FROM triggers [WHERE id<>? AND <binding>]` | The installation's trigger bound, and a cloud binding's other holders. |
| `occurrence(id)`, `identifiedOccurrence(trigger, identity)` | Both | `SELECT record[,payload_digest] FROM occurrences WHERE ...` | An occurrence by id, or by its trigger's identity with its payload's digest; `absent`. |
| `enabledTriggers()`, `readyOccurrence()`, `preparableOccurrences(each)` | Outside | The automation tick's and the preparer's statements | Triggers not paused; the oldest accepted occurrence with no input left to prepare; the accepted ones with input to prepare and the waiting ones, in order, to `each` until it chooses one. |
| `cloudBoundTrigger(connection, job)`, `jobTriggers(job)` | Outside | The cloud signal's and the trigger route's statements | A binding's trigger, or `absent`; a job's triggers, saying whether each holds a key. |
| `occurrencePage(job, before, preparations, limit)` | Outside | `SELECT seq,record FROM occurrences WHERE job_id=? AND seq<? AND (...) ORDER BY seq DESC LIMIT limit+1` | A page of a job's occurrences, newest first, the filter applied where the kind reads, and the next page's cursor; the extra row is not decoded. |
| `keyedOccurrence(id, trigger, hash)` | Outside | The three-table join of `eventResult` | One read, so that a key rotated after its caller's check still refuses. |
| `createTrigger(t)`, `putTrigger(t, hash, history)` | In a transaction | `INSERT INTO triggers(id,job_id,record)`; `saveTrigger`'s `UPDATE` and `INSERT INTO trigger_revisions` | `taken` for a held id, snapshot or cloud binding (`triggers_cloud_binding`); `absent` for a trigger not held. |
| `createOccurrence(o, identity, payload)`, `putOccurrence(o, expected)`, `createOccurrenceKey(occurrence, hash)` | In a transaction | The admission's `INSERT`s; `recordOccurrence`'s `SELECT state` and `UPDATE` | `taken` for a held id, identity or key; `putOccurrence` is versioned as `putRun` is, and `recordOccurrence` turns its `conflict` into `errPreparationStopped`. |
| `pendingOccurrences()`, `jobActivity(job)` | In a transaction | The admission's two counts | The automatic queue (`queueLimit`), and a job's runs and occurrences in flight (overlap). |
| `preparingOccurrences()`, `lastPreparing(occurrence)` | In a transaction | The restart's statements | What the last process left preparing, and when each began. `sqliteTxOf` is gone. |
| `jobCreation(release)`, `releaseUses(release)` | Both | `SELECT record FROM job_creations WHERE release_id=?`; `SELECT count(*) FROM job_revisions WHERE release_id=?` | The job a release made, or `absent`, so that a repeated creation is answered with it; how many revisions name the release. |
| `jobRevision(job, revision)` | Both | `SELECT record,request_digest FROM job_revisions WHERE job_id=? AND revision=?` | A revision as made, with the digest of the upgrade request that made it (`""` for a creation), or `absent`. A record that does not decode is `unreadable` and still returns the digest, so that an upgrade's receipt compares the digest first, as before. |
| `packConflicts(pack, version, digest)`, `dialectConflicts(dialect, version, digest)` | Both | The two counts over `job_revisions` joined to `releases` | How many releases a job revision names carry the pack, or the dialect, at that version with other content. A release that names no dialect conflicts with none. |
| `createJob(j)` | In a transaction | `INSERT INTO jobs`, `job_creations` and `job_revisions` | One operation: `taken` for a held id, a release that made a job, a held revision or a release another revision names. |
| `reviseJob(j, request)` | In a transaction | `UPDATE jobs`, then `INSERT INTO job_revisions` with the request's digest | `absent` for a job not held; `taken` for a held revision or a release another revision names. |
| `listedJobs(before, filter, limit)`, `listedRuns(job, before, filter, limit)` | Outside | The listings' statement (`store_sqlite_lists.go`): the jobs joined to their releases, or the runs to their jobs, `seq<?`, the filter's conditions, `ORDER BY t.seq DESC LIMIT limit` | Up to `limit` jobs or runs, newest first, each as its record was written with its labels (a job's release's title and pack version, a run's job's name), the filter applied where the kind reads: a scope compared byte for byte, a search that folds ASCII case only and matches no pattern, a run's state, review and [results](run-results.md). A job whose release, or a run whose job, is not held is not listed. The Runner projects each record and pages by 50, as before. |
| `recentRuns(jobs, per)`, `listedTriggers(jobs)` | Outside | The window function over `runs` and the listing's trigger read | A listed job's newest `per` runs, by job id; the listed jobs' triggers in the order they were made. |
| `openOccurrences(job)`, `triggerRecords(job)`, `reviseTrigger(t)` | In a transaction | The upgrade's count, its trigger read (now `ORDER BY seq`, the order SQLite's index gave it), and its `UPDATE triggers SET record` with `INSERT INTO trigger_revisions` | The job's occurrences still `accepted`, `preparing`, `waiting` or `needs-attention`; its triggers as written, in the order they were made; a trigger's record revised alone, its key, cursor and pending state kept, `taken` for a held snapshot and `absent` for a trigger not held. |

Every kind keeps a journal entry as one encoding, `journalRowOf`, with the sqlite kind's columns,
and reads it back through `journalRow.event`, which the sqlite kind uses on each row it scans
(`scanJournalRow`), so an
entry reads the same whatever kind keeps it. A new in-memory store begins its journal, as a new
sqlite store does at opening. The conformance suite's journal is read through `journalPage`, and
the kind-specific journal reader is gone; the sqlite kind's own cases also count the `events`
rows themselves, so that a failed transaction's keeping no entry does not rest on `journalPage`
alone.

The sqlite kind's rows are held to v0.31.0's: `store_sqlite_rows_test.go` makes the journal's
steps (every kind of change), a brief, a profile's registration, a job with a trigger upgraded,
a scoped job and a job with a retry policy, reads every table's rows with `typeof()` and
`quote()`, and compares them with `testdata/store-rows-v0.31.0.txt`, which the same test wrote
in a checkout of v0.31.0. Ids, times, digests, keys, signatures and the state directory become
placeholders numbered by first appearance; every other byte is compared as it is.
