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
	"database/sql"
	"errors"
	"fmt"
)

// storeFault is the kind of failure a storage operation met. A caller
// branches on it; the error's text stays its cause's own, so a message the
// Runner gave before a kind stood behind it reads as it did.
type storeFault string

const (
	// faultAbsent: no record is held under the id or key asked for.
	faultAbsent storeFault = "absent"
	// faultTaken: a create-once id or key is held already. Nothing is
	// written.
	faultTaken storeFault = "taken"
	// faultConflict: a versioned update found the record at another version
	// than the one its writer read. Nothing is written.
	faultConflict storeFault = "conflict"
	// faultDurability: a change was not made durable. A statement of the
	// change or its commit failed; the caller never takes it for a success.
	// A failed statement keeps nothing of the transaction; a failed commit
	// may or may not have kept it.
	faultDurability storeFault = "durability"
	// faultUnreadable: a read failed, or what is held does not read as the
	// record it should be.
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
// faultAbsent, or the no-rows error of a SQLite statement the Runner makes
// outside the contract (store_sqlite.go names that state).
func absent(err error) bool {
	return faultOf(err) == faultAbsent || errors.Is(err, sql.ErrNoRows)
}

// errKeepNothing ends a transaction without keeping anything, when its
// function found what it needed and has nothing to write: update returns it,
// as it returns every error of the function's, and the caller expects it.
var errKeepNothing = errors.New("store: the transaction keeps nothing")

// anyState is putRun's expected state when a writer changes a run whatever
// state the store holds it in.
const anyState = ""

// heldRequest is a run as the queue holds it under its request key: the
// digest of the request that made it, and its record, read when asked, so
// that a caller compares the digest before it reads the record.
type heldRequest struct {
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
	// job is the job held under id, or faultAbsent. Jobs are made and
	// revised outside the contract (store_sqlite.go).
	job(id string) (Job, error)
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
	revision                        sql.NullInt64
	from, to                        sql.NullString
	reason, detail                  string
	// job, release and refusal are what a read selects on: the concerned
	// job and release, and a refusal's key (journalRefusal).
	job, release, refusal string
}

// runStore is the Runner's durable state for the queue, releases, inputs and
// run records, behind one contract.
type runStore interface {
	storeReads
	journalReads
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
	// close gives the store up. A kind that holds the store exclusively
	// releases it last.
	close() error
}

// runTx is one transaction of a store.
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
	// returns the state it held before. With expected other than anyState,
	// the run must be held in that state, or nothing is written and the
	// fault is faultConflict: the versioned update by which a queued run is
	// claimed once, and by which an interrupted run is queued again by a
	// retry (retries.go). faultAbsent when no run is held under r.ID.
	putRun(r Run, expected string) (string, error)
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
	// under t.ID. Nothing is written on either.
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
	// opened runs on the sqlite kind's database handle before its schema is
	// applied: the Runner keeps the handle for the state outside the
	// contract, and checks a manual-admission store there.
	opened func(*sql.DB) error
}

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

// openStore opens a store of the named kind.
func openStore(kind string, o storeOpening) (runStore, error) {
	open, ok := storeKinds[kind]
	if !ok {
		return nil, storeFailure(faultRefused, "open", fmt.Errorf("no store kind %q", kind))
	}
	return open(o)
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
