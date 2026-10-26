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
| Inputs | `runs` (`job_id`, `caller`, `idem`, `request_digest`, `record`) | The input snapshot frozen into a run's record at admission, held under the request key (job, caller, idempotency key) with the digest of the request that made it. An occurrence's input, held until its run is queued, is kept with the occurrence, outside the contract (section 6). |
| Releases | `releases` | A release a preview made, kept once and never changed. |
| Run records | `runs` (`record`), `run_chain`, and the `events` entries written with them | A run's record, the installation's chain of runs, and the journal entries the Runner writes through the contract (a run's or a release's change, the Runner's start and stop), each in the transaction of what it records. |
| Jobs, read only | `jobs` | The job a run is queued for. Jobs are made and revised outside the contract (section 6). |

## 2. Operations

Every write is made inside `update`, one transaction. The reads in the first table are answered
both outside a transaction, each one statement, and inside one, where they see the transaction's
own writes.

| Outside or inside a transaction | The sqlite kind's statement | Semantics |
|---|---|---|
| `release(id)` | `SELECT record FROM releases WHERE id=?` | The release, or `absent`. A record that does not decode is `unreadable`. |
| `job(id)` | `SELECT record FROM jobs WHERE id=?` | The job, or `absent`. |
| `requestedRun(job, caller, key)` | `SELECT request_digest,record FROM runs WHERE job_id=? AND caller=? AND idem=?` | The request's digest and the run's record, decoded only when asked, so a caller compares the digest first, as it did; the decoding's error is `decodeRun`'s own. `absent` when nothing is held under the key. |
| `entrySequence(run)` | `SELECT sequence FROM run_chain WHERE run=?` | The sequence of the run's entry, or `absent`. |

| Outside a transaction | The sqlite kind's statement | Semantics |
|---|---|---|
| `run(id)` | `SELECT record FROM runs WHERE id=?` | The run, or `absent`. |
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
  run, and one under a new key is a new run.
- **Restart** (`restart`). One transaction, under the lock: the `runner.started` entry, every run
  `runningRuns` reads ended `interrupted` with an entry naming the start, and the occurrences the
  sqlite kind keeps outside the contract (section 6), through `sqliteTxOf`.
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
| `unreadable` | A read failed, or a record does not decode. | Any other failed read; a decoding error | A decoding error. |
| `refused` | An argument outside the contract, refused before anything is written. | `SQLITE_CONSTRAINT_CHECK` (a chain sequence below 1); an unknown kind; `sqliteTxOf` for another kind | A chain sequence below 1. |

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

**Versions.** The only record the contract updates is a run, and its state is its version.
Releases, chain entries and journal entries are created once and never changed. Jobs are revised
outside the contract.

## 5. The kind registry and the boot line's directory

`storeKinds` names the kinds a store can be opened as; it holds `sqlite`, which is
`defaultStoreKind`. The boot line names no kind, so every Runner opens `sqlite`, and no other kind
is selectable from the boot line. The in-memory kind is in a test file and is not registered.

The boot line's directory reaches the store only through the sqlite kind: `openSQLiteStore`,
which takes the lock (`runner.lock`) and opens the database (`runs.sqlite`), and
`sqlitePreparedStore`, which checks that a manual-admission store was prepared. The rest of the
Runner keeps the queue, releases, inputs and run records through the contract. Opening keeps its
order: the lock; the Runner's working directories (`storeOpening.afterLock`); the database with
one connection; the manual-admission store's check (`storeOpening.opened`); the pragmas and the
schema; the store's identity (workspace, owner, input root); the migration.

The directory is also the Runner's working directory for what is not the store: the files a
Runtime process runs on and writes (`releases/`, `attempts/`, `runtimes/`,
`workflow-attempts/`, `version-check-*`), the manual-admission marker and the executor lease, the
check that a signing key is kept outside it, and the health route's `stateDirectory`. A Runtime
launched as a process needs those as files on its host; they stay where they are.

## 6. What stays SQLite-specific

The kind itself: the exclusive process lock (`flock` on `runner.lock`), WAL, `synchronous=FULL`,
the busy timeout, one connection, the schema and its migration, and the store's identity in
`metadata`.

The state the contract does not cover yet, kept in the same database and reached through the
database handle the kind gives the Runner at opening (and `sqliteTxOf` inside a contract
transaction): triggers and their revisions; occurrences and their keys; job creation and
revision (`jobs` writes, `job_creations`, `job_revisions`); briefs; workflow executions and their
artifacts; the manual-admission journal; `metadata` (beside the store's identity and schema: the profile
registry, the manual-admission demand, the relocation history); the journal's reads, and every
entry written with the state above.

The statements left on `runs`, `releases` and `jobs` outside the kind, found by searching the
package for those tables:

- the job and run listings (`list.go`): joins, SQLite's ASCII case folding and a window function;
- an occurrence's run by its request key (`triggers_dispatch.go`) and an occurrence's admission
  counting a job's pending runs with its occurrences in one statement (`triggers_store.go`);
- job creation's and revision's checks over job revisions joined to releases
  (`job_revisions.go`, `dialect_artifact.go`), and a revision's read and write of its job;
- manual admission (`manual_admission_activation.go`, `manual_execution_binding.go`,
  `manual_store_prepare.go`).

Programs and modes that hold the SQLite files themselves: manual store preparation, its own
process opening `runs.sqlite` through a held directory; offline workspace relocation
(`relocate.go`); and the source worker's own store (`sources.sqlite`).

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
