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
}

// runStore is the Runner's durable state for the queue, releases, inputs and
// run records, behind one contract.
type runStore interface {
	storeReads
	// update runs fn in one transaction and keeps what it wrote only if fn
	// returns nil and the commit succeeds. Transactions are serializable: one
	// sees every write of each that committed before it began, and none of
	// one still open. fn's error is returned as fn gave it, and nothing fn
	// wrote is kept; a commit that fails is faultDurability. fn must not call
	// the store outside its transaction: a kind may wait for the transaction
	// to end first.
	update(fn func(runTx) error) error
	// run is the run held under id, or faultAbsent.
	run(id string) (Run, error)
	// oldestQueued is the queued run queued first, or faultAbsent when none
	// is queued: the queue is served in the order runs were queued.
	oldestQueued() (Run, error)
	// entryLine is the exact line of a run's entry in the chain, or
	// faultAbsent.
	entryLine(run string) ([]byte, error)
	// chainPage is up to limit entry lines with a sequence after after, in
	// order of sequence, and the sequence of the last (after when none).
	chainPage(after int64, limit int) ([][]byte, int64, error)
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
	// putRun writes r's state and record over the run held under r.ID, and
	// returns the state it held before. With expected other than anyState,
	// the run must be held in that state, or nothing is written and the
	// fault is faultConflict: the versioned update by which a queued run is
	// claimed once. faultAbsent when no run is held under r.ID.
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
	// appendJournal appends a change's entry to the journal of job activity,
	// and returns its sequence; entries are only ever appended.
	appendJournal(c change) (int64, error)
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
