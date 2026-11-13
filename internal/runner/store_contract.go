package runner

// The store contract (docs/design/store-interface.md): the operations the
// Runner keeps its queue, releases, inputs and run records through, whatever
// kind of storage keeps them, and the faults a kind reports. The SQLite kind
// (store_sqlite.go) is the one that ships; every kind answers to the
// conformance suite (store_conformance_test.go), and an in-memory kind
// (store_memory_test.go) runs it beside SQLite. Nothing here names a path:
// where a kind keeps its state is the kind's.
//
// What a run, a release or a journal entry is, and when one may change, is
// the Runner's and not this file's: a kind keeps records and answers the
// questions the Runner asks of them, in one transaction when the Runner asks
// for one.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// storeFault is the kind of failure a storage operation met. A caller
// branches on it; the error's text stays its cause's own, so a message the
// Runner gave before a kind stood behind it reads as it did.
type storeFault string

const (
	// faultAbsent: no record is held under the id or key asked for.
	faultAbsent storeFault = "absent"
	// faultTaken: a create-once id or key is held already. An operation of
	// one statement writes nothing; one of several may have made its first
	// statements in the transaction, which its caller then abandons (runTx).
	faultTaken storeFault = "taken"
	// faultConflict: a versioned update found the record at another version
	// than the one its writer read. Nothing is written.
	faultConflict storeFault = "conflict"
	// faultDurability: a change was not made durable. A statement of the
	// change or its commit failed; the caller never takes it for a success.
	// A failed statement keeps nothing of the transaction; a failed commit
	// may or may not have kept it.
	faultDurability storeFault = "durability"
	// faultUnavailable: a read failed: the store did not answer, and nothing
	// is known of what it holds. A caller never takes it for a record, or for
	// none.
	faultUnavailable storeFault = "unavailable"
	// faultUnreadable: what is held does not read as the record it should
	// be.
	faultUnreadable storeFault = "unreadable"
	// faultRefused: an argument outside the contract, refused before
	// anything is written.
	faultRefused storeFault = "refused"
)

// storeError is every failure a kind reports.
type storeError struct {
	fault storeFault
	op    string
	err   error
}

func (e *storeError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("store: %s: %s", e.op, e.fault)
}

func (e *storeError) Unwrap() error { return e.err }

// storeFailure is a fault met by an operation, with its cause.
func storeFailure(fault storeFault, op string, err error) error {
	return &storeError{fault: fault, op: op, err: err}
}

// faultOf is the fault an error carries, or "" for an error no kind made.
func faultOf(err error) storeFault {
	var e *storeError
	if errors.As(err, &e) {
		return e.fault
	}
	return ""
}

// absent says whether err says nothing is held: a contract read's
// faultAbsent, or the no-rows error of a statement of the sqlite kind's that
// reaches the Runner as it is (store_sqlite_manual.go).
func absent(err error) bool {
	return faultOf(err) == faultAbsent || sqliteNoRows(err)
}

// errKeepNothing ends a transaction without keeping anything, when its
// function found what it needed and has nothing to write: update returns it,
// as it returns every error of the function's, and the caller expects it.
var errKeepNothing = errors.New("store: the transaction keeps nothing")

// anyState is putRun's expected state when a writer changes a run whatever
// state the store holds it in.
const anyState = ""

// runClaim is a dispatcher's claim of a run (docs/design/store-postgresql.md,
// D3): a token new for each claim, and the attempt claimed. A kind that keeps
// no lock against other dispatchers fences every write the claimant makes by
// it, and holds the claim for claimLease by its own clock, renewed by
// renewClaim. The zero value is no claim.
type runClaim struct {
	token   string
	attempt int
}

// claimLease is how long a claim is held, by the store's clock, from the
// claim or its last renewal; claimRenewal is how often its holder renews it.
const (
	claimLease   = 60 * time.Second
	claimRenewal = 15 * time.Second
)

// newRunClaim is a claim of attempt with a new token of 16 random bytes.
func newRunClaim(attempt int) (runClaim, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return runClaim{}, err
	}
	return runClaim{token: hex.EncodeToString(token), attempt: attempt}, nil
}

// runExpected is what putRun must find held before it writes: the run's state,
// or anyState, and the claim the writer holds, or none.
type runExpected struct {
	state string
	claim runClaim
}

// heldRequest is a run as the queue holds it under its request key: its id,
// the digest of the request that made it, and its record, read when asked,
// so that a caller compares the digest before it reads the record, and a
// caller that needs only the id never reads it.
type heldRequest struct {
	id     string
	digest string
	record []byte
}

// run reads the held record.
func (h heldRequest) run() (Run, error) { return decodeRun(h.record) }

// storeReads are the reads a store answers both outside a transaction, each
// one statement as it stands, and inside one, where it sees the
// transaction's own writes.
type storeReads interface {
	// release is the release held under id, or faultAbsent.
	release(id string) (Release, error)
	// job is the job held under id, or faultAbsent.
	job(id string) (Job, error)
	// jobCreation is the job a release made, as it was made, or faultAbsent:
	// a release makes one job, and a repeated creation returns it.
	jobCreation(release string) (Job, error)
	// releaseUses counts the job revisions that name a release.
	releaseUses(release string) (int, error)
	// jobRevision is a job's revision as it was made, with the digest of the
	// upgrade request that made it ("" for a job's creation), or
	// faultAbsent.
	jobRevision(job string, revision int) (Job, string, error)
	// packConflicts counts the job revisions whose release names the pack
	// identity and version with other content than digest.
	packConflicts(pack, version, digest string) (int, error)
	// dialectConflicts counts the job revisions whose release names the
	// dialect identity and version with other content than digest.
	dialectConflicts(dialect, version, digest string) (int, error)
	// requestedRun is the run held under its request key, the job, the
	// caller and the caller's idempotency key, or faultAbsent.
	requestedRun(jobID, caller, key string) (heldRequest, error)
	// entrySequence is the sequence of a run's entry in the installation's
	// chain of runs, or faultAbsent.
	entrySequence(run string) (int64, error)
	// run is the run held under id, or faultAbsent. Inside a transaction it
	// is the run as the transaction holds it, so that a retry's checks and
	// its write see one record (retries.go).
	run(id string) (Run, error)
	// recordScope is the scope identifier the record of a kind of record
	// (scopeCarriers) held under id carries, "" for none, or faultAbsent.
	// faultRefused for another kind of record.
	recordScope(records, id string) (string, error)
	// profileRow is the row of input profiles held under key
	// (profileRowKeys), as it was written, or faultAbsent. faultRefused for
	// another key.
	profileRow(key string) (string, error)
	// brief is the brief history held for subject, as it was written, or
	// faultAbsent.
	brief(subject string) ([]byte, error)
	// trigger is the trigger held under id, with its file observer's cursor
	// and the revision its delivery key was issued at, and the digest of
	// that key ("" for none), or faultAbsent.
	trigger(id string) (Trigger, string, error)
	// triggerCount counts the triggers held.
	triggerCount() (int, error)
	// cloudBindings counts the triggers other than except whose cloud
	// binding names connection and job.
	cloudBindings(except, connection, job string) (int, error)
	// occurrence is the occurrence held under id, or faultAbsent.
	occurrence(id string) (Occurrence, error)
	// identifiedOccurrence is the occurrence trigger admitted under identity
	// (its event, schedule time, file change or signal), with the digest of
	// the payload it was admitted with, or faultAbsent.
	identifiedOccurrence(trigger, identity string) (Occurrence, string, error)
}

// scopeCarriers are the kinds of record that carry a scope identifier, by
// the name recordScope takes.
var scopeCarriers = map[string]bool{"jobs": true, "runs": true, "triggers": true, "occurrences": true}

// profileRowKeys are the keys profile rows are held under.
var profileRowKeys = map[string]bool{registeredProfilesKey: true, selectedProfilesKey: true}

// journalReads are the journal's reads, outside a transaction.
type journalReads interface {
	// journalPage is up to r.limit entries f keeps, in the order r reads
	// them, and whether f keeps another beyond them, which is not decoded.
	journalPage(f journalFilter, r journalRange) ([]Event, bool, error)
	// journalBegan is when the journal began: its first journal.began
	// entry's time, or faultAbsent.
	journalBegan() (string, error)
}

// journalFilter selects the journal's entries a read keeps; the zero filter
// keeps every entry. A kind applies it where it reads, so that a page holds
// only entries the filter keeps.
type journalFilter struct {
	// job keeps the entries naming the job, and those naming no job and one
	// of the job's revisions' releases (a preview, a refused creation).
	job string
	// runnerOwn keeps, beside job's, the Runner's own entries (journal.began,
	// runner.started, runner.stopped). It means nothing without job.
	runnerOwn bool
	// scope keeps, of those, only the entries whose detail carries this
	// scope identifier, compared byte for byte.
	scope string
	// results keeps, of those, the entries of the outcomes, error classes
	// and kinds named (run_results.go).
	results resultFilter
}

// journalRange is where a read of the journal starts and which way it goes:
// forward, the entries after after, oldest first; backward, the newest
// (tail) or those before before, newest first.
type journalRange struct {
	after    int64
	backward bool
	tail     bool
	before   int64
	limit    int
}

// journalRow is an entry as every kind keeps it: a change encoded once
// (journalRowOf), in the sqlite kind's columns, so that the journal reads
// the same whatever kind keeps it.
type journalRow struct {
	version, kind, at, by, concerns string
	revision                        nullInt
	from, to                        nullText
	reason, detail                  string
	// job, release and refusal are what a read selects on: the concerned
	// job and release, and a refusal's key (journalRefusal).
	job, release, refusal string
}

// nullInt and nullText are a journal column that may hold nothing, as the
// sqlite kind's NULL: held says whether it holds value.
type (
	nullInt struct {
		value int64
		held  bool
	}
	nullText struct {
		value string
		held  bool
	}
)

// storeScope is the scope a transaction, or a read outside one, is held to
// (docs/design/store-postgresql.md, sections 2 and 6). A store as a kind
// opens it does the work for every scope: the dispatcher's, the ticks', the
// restart's, and a listing's or a journal read's that names no scope. A
// request's transactions and reads are made through heldTo, held to the
// scope identifier the request names, or to none.
//
// A transaction held to a scope reads and writes only the records that carry
// that scope identifier ("" being none), and the records no scope narrows:
// releases, the chain of runs, the profile rows, and the rules held for the
// installation (the counts of runs, triggers and occurrences, the pack and
// dialect identities, the keys a record is created once under: D9). A record
// of another scope reads absent; a write of one is refused. The journal is
// read as section 6 says: a scope's entries, and the none scope's entries
// that name no job.
//
// The zero value is the none scope, never every scope: a transaction held to
// it by mistake reaches no other scope's record.
type storeScope struct {
	// every is the work for every scope; id means nothing with it.
	every bool
	// id is the scope identifier, "" for none.
	id string
}

// everyScope is the work for every scope.
var everyScope = storeScope{every: true}

// scopeHeld is a transaction held to the scope identifier id, "" for none.
func scopeHeld(id string) storeScope { return storeScope{id: id} }

// reaches says whether sc reaches a record that carries scope ("" for none).
func (sc storeScope) reaches(scope string) bool { return sc.every || sc.id == scope }

// runStore is the Runner's durable state for the queue, releases, inputs and
// run records, behind one contract.
type runStore interface {
	storeReads
	journalReads
	// heldTo is the store with every read and transaction held to sc
	// (storeScope). A kind that holds no policy of its own, the sqlite kind,
	// returns the store as it is: its one process holds every scope.
	heldTo(sc storeScope) runStore
	// missingAttemptHoldsNoAudit says whether the retry check
	// (attemptsHoldNoAudit) may take an attempt directory that does not exist
	// to hold no audit record (docs/design/store-postgresql.md, D11). The
	// sqlite kind says yes: its state directory holds every attempt's
	// directory, kept with its database. A kind whose store outlives the
	// host's disk says no, and the check takes a missing directory to be
	// unknown, as it takes one it cannot read.
	missingAttemptHoldsNoAudit() bool
	// update runs fn in one transaction and keeps what it wrote only if fn
	// returns nil and the commit succeeds. Transactions are serializable: one
	// sees every write of each that committed before it began, and none of
	// one still open. fn's error is returned as fn gave it, and nothing fn
	// wrote is kept; a commit that fails is faultDurability. fn must not call
	// the store outside its transaction: a kind may wait for the transaction
	// to end first.
	update(fn func(runTx) error) error
	// oldestQueued is the queued run queued first, or faultAbsent when none
	// is queued: the queue is served in the order runs were queued.
	oldestQueued() (Run, error)
	// entryLine is the exact line of a run's entry in the chain, or
	// faultAbsent.
	entryLine(run string) ([]byte, error)
	// chainPage is up to limit entry lines with a sequence after after, in
	// order of sequence, and the sequence of the last (after when none).
	chainPage(after int64, limit int) ([][]byte, int64, error)
	// enabledTriggers are the ids of the triggers held not paused, in the
	// order they were made.
	enabledTriggers() ([]string, error)
	// readyOccurrence is the accepted occurrence admitted first that has no
	// input left to prepare, or faultAbsent.
	readyOccurrence() (Occurrence, error)
	// preparableOccurrences gives each, in the order they were admitted,
	// the occurrences accepted with input left to prepare and those waiting,
	// until each returns true or an error, which it returns. each must not
	// call the store.
	preparableOccurrences(each func(Occurrence) (bool, error)) error
	// cloudBoundTrigger is the id of a trigger whose cloud binding names
	// connection and job, or faultAbsent.
	cloudBoundTrigger(connection, job string) (string, error)
	// jobTriggers are a job's triggers, in the order they were made, each
	// saying whether it holds a delivery key.
	jobTriggers(job string) ([]Trigger, error)
	// occurrencePage is up to limit of a job's occurrences admitted before
	// the order before, newest first, and with preparations only those that
	// hold a preparation and are not submitted; and, when the job has
	// another beyond them, which is not decoded, the order of the last one
	// read, the next page's before, and 0 otherwise.
	occurrencePage(job string, before int64, preparations bool, limit int) ([]Occurrence, int64, error)
	// keyedOccurrence is the occurrence held under id, admitted by trigger
	// and bound to the delivery key whose digest is hash, while that key is
	// the trigger's, or faultAbsent: one read, so that a key rotated after
	// its caller checked it still refuses (eventResult).
	keyedOccurrence(id, trigger, hash string) (Occurrence, error)
	// listedJobs are up to limit of the jobs made before the order before,
	// newest first, whose release is held and that f keeps (its Scope and
	// Search), each labelled with its release's title and pack version.
	listedJobs(before int64, f recordFilter, limit int) ([]listedRecord, error)
	// listedRuns are up to limit of the runs queued before the order
	// before, newest first, of job ("" for every job's), whose job is held
	// and that f keeps, each labelled with its job's name.
	listedRuns(job string, before int64, f recordFilter, limit int) ([]listedRecord, error)
	// recentRuns are the newest per runs of each of jobs, by job id and
	// newest first.
	recentRuns(jobs []string, per int) ([]recentRun, error)
	// listedTriggers are the triggers of jobs, in the order they were made,
	// each with the job it is held under.
	listedTriggers(jobs []string) ([]listedTrigger, error)
	// close gives the store up. A kind that holds the store exclusively
	// releases it last.
	close() error
}

// listedRecord is a job or a run as a listing reads it (list.go): its order,
// its record as written, which the listing projects, and the labels shown
// beside it, read from the record it names: a job's release's title and
// pack version, a run's job's name ("" for version). A listing's filter is
// applied where a kind reads, as SQLite applies it:
//
//   - Scope keeps a record whose scope member is the scope as text, compared
//     byte for byte;
//   - Search keeps a job whose name holds it, and a run whose id, a space
//     and its job's name hold it, with ASCII letters folded to lower case
//     and no other;
//   - State keeps a run held in that state; Review a run failed or
//     interrupted, or completed with an unresolved disposition or a
//     handoff requested; Results a run as resultFilter says, its outcomes
//     only of a completed run.
//
// A member is read as json_extract reads it: text when the record holds it
// as a JSON string, and otherwise compared to no text.
type listedRecord struct {
	seq            int64
	record         []byte
	label, version string
}

// recentRun is one of a job's newest runs, as a job's listing shows it.
type recentRun struct {
	job, id, state, createdAt string
}

// listedTrigger is a trigger as a job's listing shows it, under the job it
// is held under.
type listedTrigger struct {
	job     string
	trigger Trigger
}

// runTx is one transaction of a store. A write that returns a fault may have
// made part of its statements in the transaction: its caller abandons the
// transaction, returning the fault (or another error) from update's
// function, and update keeps nothing of it.
type runTx interface {
	storeReads
	// activeRuns counts the runs queued or running.
	activeRuns() (int, error)
	// createRun queues r under its id and its request key (r.JobID, caller,
	// key), with the request's digest: faultTaken when either is held.
	createRun(r Run, caller, key, requestDigest string) error
	// lastRefusal is the time of the newest entry journaled under a
	// refusal's key, which is never empty (journalRefusal), or faultAbsent.
	lastRefusal(key string) (string, error)
	// putProfileRow writes the row of input profiles held under key,
	// whatever it held. faultRefused for a key outside profileRowKeys.
	putProfileRow(key, row string) error
	// putBrief writes the brief history held for subject, whatever it held.
	putBrief(subject string, record []byte) error
	// putRun writes r's state and record over the run held under r.ID, and
	// returns the state it held before. With expected.state other than
	// anyState, the run must be held in that state, or nothing is written and
	// the fault is faultConflict: the versioned update by which a queued run
	// is claimed once, and by which an interrupted run is queued again by a
	// retry (retries.go). With expected.claim, the run must also be held under
	// that claim, its lease not run out by the store's clock, or nothing is
	// written and the fault is faultConflict: a claimant's write after its
	// claim was lost. faultAbsent when no run is held under r.ID.
	//
	// After the write the run is held under r.claim: a claim other than the
	// one held starts its lease, the one held keeps it, and none clears it, as
	// an interruption does. The sqlite kind takes the claim and ignores it:
	// its lock keeps every other dispatcher out, and is the fence.
	putRun(r Run, expected runExpected) (string, error)
	// renewClaim holds the run's claim for another claimLease from now, by the
	// store's clock, if the run is held running under claim and its lease has
	// not run out; otherwise it writes nothing and the fault is
	// faultConflict. The sqlite kind ignores it, as it ignores the claim.
	renewClaim(run string, claim runClaim) error
	// runningRuns are the runs held running, in the order they were queued.
	runningRuns() ([]Run, error)
	// lastEntry is the sequence and line of the chain's last entry, or
	// faultAbsent when the chain holds none.
	lastEntry() (int64, []byte, error)
	// createEntry appends a run's entry to the chain at sequence: faultTaken
	// when the sequence or the run has an entry, faultRefused for a sequence
	// below 1.
	createEntry(sequence int64, run string, line []byte) error
	// createRelease keeps r under its id: faultTaken when one is held.
	createRelease(r Release) error
	// createTrigger keeps t under its id, with no delivery key and no file
	// cursor: faultTaken when one is held, or when t is a cloud trigger
	// whose binding another cloud trigger holds.
	createTrigger(t Trigger) error
	// putTrigger writes t, its delivery key's digest hash and its file
	// cursor over the trigger held under t.ID, and with history keeps the
	// snapshot of t's revision: faultTaken when that snapshot, or t's cloud
	// binding, is held already, and faultAbsent when no trigger is held
	// under t.ID. On either its caller abandons the transaction: the
	// trigger's columns may be written in it already.
	putTrigger(t Trigger, hash string, history bool) error
	// createOccurrence keeps o, admitted under identity with the payload
	// whose digest is payload: faultTaken when its id, or the identity for
	// its trigger, is held.
	createOccurrence(o Occurrence, identity, payload string) error
	// putOccurrence writes o's state and record over the occurrence held
	// under o.ID, and returns the state it held before. With expected other
	// than anyState, the occurrence must be held in that state, or nothing
	// is written and the fault is faultConflict: a final state is not
	// overwritten by a preparation still in flight. faultAbsent when no
	// occurrence is held under o.ID.
	putOccurrence(o Occurrence, expected string) (string, error)
	// createOccurrenceKey binds an occurrence to the delivery key that
	// created it, by the key's digest: faultTaken when it is bound.
	createOccurrenceKey(occurrence, hash string) error
	// pendingOccurrences counts the occurrences accepted, preparing or
	// waiting: the automatic queue (queueLimit).
	pendingOccurrences() (int, error)
	// jobActivity counts a job's runs queued or running and its occurrences
	// accepted, preparing, waiting or needing attention.
	jobActivity(job string) (int, error)
	// preparingOccurrences are the occurrences held preparing, in the order
	// they were admitted.
	preparingOccurrences() ([]Occurrence, error)
	// lastPreparing is the time of the newest occurrence.preparing entry
	// concerning an occurrence, or faultAbsent.
	lastPreparing(occurrence string) (string, error)
	// createJob keeps j, made from j.ReleaseID: the job, its creation and
	// its first revision. faultTaken when its id, its release's creation or
	// a revision naming its release is held; its caller then abandons the
	// transaction, in which the job's row may be made already.
	createJob(j Job) error
	// reviseJob writes j over the job held under j.ID and keeps its
	// revision j.Revision, made by the upgrade request whose digest is
	// request: faultAbsent when no job is held under j.ID, and faultTaken
	// when that revision, or one naming j's release, is held; its caller
	// then abandons the transaction, in which the job may be written
	// already.
	reviseJob(j Job, request string) error
	// openOccurrences counts a job's occurrences accepted, preparing,
	// waiting or needing attention.
	openOccurrences(job string) (int, error)
	// triggerRecords are a job's triggers, each as its record was written,
	// in the order they were made.
	triggerRecords(job string) ([]Trigger, error)
	// reviseTrigger writes t's record over the trigger held under t.ID, its
	// key and file cursor left as they are, and keeps the snapshot of t's
	// revision: faultAbsent when no trigger is held under t.ID, faultTaken
	// when the snapshot is held; its caller then abandons the transaction,
	// in which the record may be written already.
	reviseTrigger(t Trigger) error
	// appendJournal appends a change's entry to the journal of job activity,
	// and returns its sequence; entries are only ever appended.
	appendJournal(c change) (int64, error)
	// raiseSchema marks the store as holding records that only a Runner
	// that knows schema keeps, so that no earlier Runner opens it: the
	// sqlite kind raises its schema to scopedStoreSchema from storeSchema,
	// and to retryStoreSchema from either; a store raised already is left
	// as it is. faultRefused for a schema the kind does not know.
	raiseSchema(schema string) error
}

// storeOpening is what the Runner gives a kind's constructor.
type storeOpening struct {
	// dir is the state directory the boot line names. Only the sqlite kind
	// reads it.
	dir string
	// workspace, owner and inputRoot are the store's identity: a store
	// another workspace, owner or input root holds is refused.
	workspace, owner, inputRoot string
	// migrate raises an earlier Runner's store to this Runner's schema and
	// begins its journal. A manual-admission store is never migrated.
	migrate bool
	// afterLock runs once the kind holds the store exclusively and before
	// it opens its database: the Runner prepares its working directories
	// there, in the order it always has.
	afterLock func() error
	// manual runs on the kind's manual-admission state once its database
	// is open and before its schema is applied: the Runner keeps it, for the
	// execution binding and manual admission, and checks a manual-admission
	// store there. Only the sqlite kind keeps that state
	// (docs/design/store-postgresql.md, D4); another kind does not run it.
	manual func(manualState) error
	// manualAdmission is set when the Runner runs under manual admission:
	// a kind that hands it no manual-admission state is then refused.
	manualAdmission bool
}

// manualState is the manual-admission state the sqlite kind keeps beside
// the contract's: the store's manual demand and schema, the manual-admission
// journal, and the reads manual admission checks a store and a job by.
type manualState interface {
	manualReads
	// queuedRunRecords gives each the record of every run held queued, with
	// the error of a row that does not read, until each returns an error,
	// which it returns.
	queuedRunRecords(each func(record []byte, err error) error) error
	// readManual runs fn in one read transaction.
	readManual(fn func(manualReads) error) error
	// createManualAttempt journals a run's manual attempt under its intent:
	// faultTaken when either holds one.
	createManualAttempt(run, intent, record string) error
	// replaceManualAttempt writes after over a run's attempt held as
	// before, and returns how many it wrote, -1 when that is not known.
	replaceManualAttempt(run, before, after string) (int64, error)
}

// manualReads are the reads of the manual-admission state, outside a
// transaction or in one.
type manualReads interface {
	// manualValue is the store's metadata row held under key, or
	// faultAbsent.
	manualValue(key string) (string, error)
	// manualCount answers question, with its arguments.
	manualCount(question storeQuestion, args ...any) (int, error)
	// manualRecord is the record of a job or a release held under id, as
	// written, or faultAbsent.
	manualRecord(records, id string) ([]byte, error)
}

// storeQuestion is a count manual admission, its preparation and relocation
// ask of the sqlite kind's database, outside the contract.
type storeQuestion int

const (
	// askTriggers counts the triggers held; askUnpausedTriggers those not
	// paused; askJobTriggers a job's.
	askTriggers storeQuestion = iota + 1
	askUnpausedTriggers
	askJobTriggers
	// askActiveRuns counts the runs queued or running.
	askActiveRuns
	// askOpenOccurrences counts the occurrences accepted, preparing,
	// waiting or needing attention.
	askOpenOccurrences
	// askManualAttempts counts the manual attempts journaled; askRunAttempts
	// those of a run or an intent.
	askManualAttempts
	askRunAttempts
)

// defaultStoreKind is the kind every Runner opens. The boot line names no
// kind, so no other is selectable yet.
const defaultStoreKind = "sqlite"

// storeKinds are the kinds a store can be opened as, by name.
var storeKinds = map[string]func(storeOpening) (runStore, error){
	"sqlite": func(o storeOpening) (runStore, error) {
		k, err := openSQLiteStore(o)
		if err != nil {
			return nil, err
		}
		return k, nil
	},
}

// openStore opens a store of the named kind. Under manual admission, a
// kind that hands over no manual-admission state never checked the store
// (store-postgresql.md, D4), and is refused.
func openStore(kind string, o storeOpening) (runStore, error) {
	open, ok := storeKinds[kind]
	if !ok {
		return nil, storeFailure(faultRefused, "open", fmt.Errorf("no store kind %q", kind))
	}
	handed := false
	if manual := o.manual; manual != nil {
		o.manual = func(m manualState) error {
			handed = true
			return manual(m)
		}
	}
	st, err := open(o)
	if err != nil {
		return nil, err
	}
	if o.manualAdmission && !handed {
		st.close()
		return nil, errManualProtocol
	}
	return st, nil
}

// validateStoredInput rechecks the installed selection at each persistence
// boundary. Stores keep the existing transaction and CAS contract unchanged.
func validateStoredInput(i Input) error {
	c := inputConversion(i)
	if e := c.validate("document-validation", "preview", "preparation"); e != nil {
		return e
	}
	if i.Preparation != nil && !sameConversion(c, i.Preparation.ConversionContract) {
		return conversionFailure("capability-unsupported")
	}
	if c == nil {
		return nil
	}
	if i.Source == nil || !hasColumnMapping(i.Source.Mapping) || i.Source.Mapping.Profile != "base" {
		return conversionFailure("capability-unsupported")
	}
	if i.Preparation != nil && (!sameConversion(c, i.Preparation.ConversionContract) || len(encode(i.Preparation)) > columnPreparationLimit) {
		return codedProblem(codeOverBound, "stored preparation exceeds selected limit or differs in selection")
	}
	return columnInputBounds(i.Source)
}
func validateStoredRelease(r Release) error {
	if e := validateStoredInput(r.Sample); e != nil {
		return storeFailure(faultRefused, "release", e)
	}
	if inputConversion(r.Sample) != nil && len(encode(r)) > columnStoredLimit {
		return storeFailure(faultRefused, "release", codedProblem(codeOverBound, "retained release exceeds selected storage limit"))
	}
	return nil
}
func validateStoredRun(r Run) error {
	if e := validateStoredInput(r.Input); e != nil {
		return storeFailure(faultRefused, "run", e)
	}
	if inputConversion(r.Input) != nil && (len(runText(r)) > columnStoredLimit || len(r.AuditBytes) > columnRecordLimit) {
		return storeFailure(faultRefused, "run", codedProblem(codeOverBound, "retained run or audit record exceeds selected storage limit"))
	}
	return nil
}
