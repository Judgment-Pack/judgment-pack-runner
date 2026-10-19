package runner

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

const manualStoreSchema = "4"
const manualPrepareMarker = "manual-execution-prepare.json"

type ManualOwnershipWire struct {
	Version          int    `json:"version"`
	Root             string `json:"root"`
	MachineWorkspace string `json:"machineWorkspace"`
	RunnerWorkspace  string `json:"runnerWorkspace"`
	RootDevice       string `json:"rootDevice"`
	RootInode        string `json:"rootInode"`
	FileDevice       string `json:"fileDevice"`
	FileInode        string `json:"fileInode"`
}

func (w ManualOwnershipWire) ownership() (ExecutorOwnership, error) {
	o := ExecutorOwnership{Root: w.Root, MachineWorkspace: w.MachineWorkspace, RunnerWorkspace: w.RunnerWorkspace}
	if w.Version != 1 || !manualHex(w.MachineWorkspace) {
		return o, errManualProtocol
	}
	for _, field := range []struct {
		value string
		dst   *uint64
	}{{w.RootDevice, &o.RootDevice}, {w.RootInode, &o.RootInode}, {w.FileDevice, &o.FileDevice}, {w.FileInode, &o.FileInode}} {
		n, err := strconv.ParseUint(field.value, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != field.value {
			return o, errManualProtocol
		}
		*field.dst = n
	}
	return o, o.Validate(w.RunnerWorkspace)
}

type ManualStorePrepareRequest struct {
	Version   int                   `json:"version"`
	PrepareID string                `json:"prepareId"`
	Dir       string                `json:"dir"`
	Workspace string                `json:"workspace"`
	Owner     string                `json:"owner"`
	LeaseFD   int                   `json:"leaseFd"`
	Ownership ManualOwnershipWire   `json:"executorOwnership"`
	Demand    ManualExecutionDemand `json:"executionDemand"`
}
type ManualStorePrepareResult struct {
	Version       int    `json:"version"`
	Status        string `json:"status"`
	PrepareID     string `json:"prepareId,omitempty"`
	RequestDigest string `json:"requestDigest,omitempty"`
	Delivery      string `json:"delivery,omitempty"`
	StoreSchema   string `json:"storeSchema,omitempty"`
	Code          string `json:"code,omitempty"`
}
type manualPrepareRecord struct {
	Version       int                   `json:"version"`
	PrepareID     string                `json:"prepareId"`
	RequestDigest string                `json:"requestDigest"`
	Demand        ManualExecutionDemand `json:"executionDemand"`
}

const manualJournalSchema = `CREATE TABLE manual_attempts(run_id TEXT PRIMARY KEY,intent_id TEXT NOT NULL UNIQUE,record TEXT NOT NULL);`

func prepareRefusal(code string) ManualStorePrepareResult {
	return ManualStorePrepareResult{Version: 1, Status: "refused", Code: code}
}

// PrepareManualStore is a nonexecuting library adapter. raw excludes LF.
// It never changes cwd and refuses hosts without a held-descriptor SQLite path.
// It takes ownership of fd3 only after the complete trusted request is valid.
func PrepareManualStore(raw []byte) ManualStorePrepareResult {
	return prepareManualStore(raw, nil)
}

// RunManualStorePreparationProcess is the dedicated nonexecuting CLI entry.
// It ALWAYS exits the process after its single reply. Only this entry may use
// a retained cwd/private SQLite VFS; it never returns to a service or library.
func RunManualStorePreparationProcess(raw []byte, output io.Writer) {
	result := prepareManualStoreUsing(raw, nil, manualProcessDatabaseTarget)
	status := 0
	if json.NewEncoder(output).Encode(result) != nil || result.Status != "ok" {
		status = 1
	}
	os.Exit(status)
}

type manualStoreDatabaseTarget func(*ExecutorLease) (string, io.Closer, error)

func manualLibraryDatabaseTarget(lease *ExecutorLease) (string, io.Closer, error) {
	name, dir, err := manualDatabaseTarget(lease)
	return name, dir, err
}
func prepareManualStore(raw []byte, inherited *os.File) ManualStorePrepareResult {
	return prepareManualStoreUsing(raw, inherited, manualLibraryDatabaseTarget)
}
func prepareManualStoreUsing(raw []byte, inherited *os.File, target manualStoreDatabaseTarget) ManualStorePrepareResult {
	var q ManualStorePrepareRequest
	if len(raw) == 0 || len(raw)+1 > 16384 || manualDecode(raw, &q) != nil || q.Version != 1 || !manualHex(q.PrepareID) || !manualWorkspace(q.Workspace) || len(q.Owner) == 0 || len(q.Owner) > 256 || q.LeaseFD != 3 || !q.Demand.valid() || q.Demand.RunnerWorkspace != q.Workspace || q.Ownership.RunnerWorkspace != q.Workspace || q.Ownership.MachineWorkspace != q.Demand.MachineWorkspace || !filepath.IsAbs(q.Dir) || filepath.Clean(q.Dir) != q.Dir {
		return prepareRefusal("invalid-request")
	}
	o, err := q.Ownership.ownership()
	if err != nil {
		return prepareRefusal("invalid-request")
	}
	if inherited == nil {
		inherited = os.NewFile(3, "manual-store-lease")
	}
	lease, err := adoptManualPreparationLease(o, q.Dir, inherited)
	if err != nil {
		return prepareRefusal("unsafe-custody")
	}
	defer lease.Close()
	return prepareManualStoreHeldUsing(q, raw, lease, target)
}

func prepareManualStoreHeld(q ManualStorePrepareRequest, raw []byte, lease *ExecutorLease) ManualStorePrepareResult {
	return prepareManualStoreHeldUsing(q, raw, lease, manualLibraryDatabaseTarget)
}
func prepareManualStoreHeldUsing(q ManualStorePrepareRequest, raw []byte, lease *ExecutorLease, target manualStoreDatabaseTarget) ManualStorePrepareResult {
	o, err := q.Ownership.ownership()
	if err != nil || verifyManualLease(lease) != nil {
		return prepareRefusal("unsafe-custody")
	}
	demand, err := readManualDemand(o)
	if err != nil || demand == nil || *demand != q.Demand {
		return prepareRefusal("unsafe-custody")
	}
	lock, err := manualPreparationInstanceLock(lease)
	if err != nil {
		return prepareRefusal("busy")
	}
	defer lock.Close()
	heldDatabase, err := lease.state.Open("runs.sqlite")
	if err != nil {
		return prepareRefusal("unsupported-store")
	}
	defer heldDatabase.Close()
	databaseIdentity, err := heldDatabase.Stat()
	if err != nil || verifyManualDatabasePath(lease, databaseIdentity) != nil {
		return prepareRefusal("unsupported-store")
	}
	database, targetDirectory, err := target(lease)
	if err != nil {
		return prepareRefusal("unsafe-custody")
	}
	defer targetDirectory.Close()
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return prepareRefusal("storage-failed")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA temp_store=MEMORY; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;"); err != nil {
		return prepareRefusal("storage-failed")
	}
	tx, err := db.Begin()
	if err != nil {
		return prepareRefusal("storage-failed")
	}
	defer tx.Rollback()
	// SQLite uses either a retained descriptor path or the isolated process's
	// retained cwd and private relative VFS. Also
	// compare the configured placement and DB inode around connection opening
	// and before every durable boundary. This is not adversarial same-OS-user
	// atomicity; detected replacement refuses and cannot finalize Desk enable.
	if verifyManualDatabasePath(lease, databaseIdentity) != nil {
		return prepareRefusal("unsafe-custody")
	}
	for key, want := range map[string]string{"workspace": q.Workspace, "owner": q.Owner} {
		var got string
		if tx.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&got) != nil || got != want {
			return prepareRefusal("unsupported-store")
		}
	}
	var schema string
	if tx.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema) != nil {
		return prepareRefusal("unsupported-store")
	}
	record := manualPrepareRecord{1, q.PrepareID, digest(raw), q.Demand}
	old, markerErr := readManualStateFile(lease, manualPrepareMarker, 16384)
	if markerErr == nil {
		var held manualPrepareRecord
		if manualDecode(old, &held) != nil || held != record || schema != manualStoreSchema {
			return prepareRefusal("pending-transition")
		}
		var retained string
		if tx.QueryRow("SELECT value FROM metadata WHERE key='manualExecutionDemand'").Scan(&retained) != nil || retained != string(encode(q.Demand)) {
			return prepareRefusal("pending-transition")
		}
		var attempts int
		if tx.QueryRow("SELECT count(*) FROM manual_attempts").Scan(&attempts) != nil || verifyManualDatabasePath(lease, databaseIdentity) != nil {
			return prepareRefusal("pending-transition")
		}
		return ManualStorePrepareResult{Version: 1, Status: "ok", PrepareID: q.PrepareID, RequestDigest: record.RequestDigest, Delivery: "recovered", StoreSchema: manualStoreSchema}
	}
	if !errors.Is(markerErr, os.ErrNotExist) {
		return prepareRefusal("pending-transition")
	}
	if schema != storeSchema {
		return prepareRefusal("unsupported-store")
	}
	for _, query := range []string{"SELECT count(*) FROM triggers", "SELECT count(*) FROM runs WHERE state IN ('queued','running')", "SELECT count(*) FROM occurrences WHERE state IN ('accepted','preparing','waiting','needs-attention')"} {
		var count int
		if tx.QueryRow(query).Scan(&count) != nil {
			return prepareRefusal("unsupported-store")
		}
		if count != 0 {
			return prepareRefusal("unsupported-profile")
		}
	}
	if verifyManualDatabasePath(lease, databaseIdentity) != nil {
		return prepareRefusal("unsafe-custody")
	}
	if writeManualPrepareMarker(lease, encode(record)) != nil {
		return prepareRefusal("storage-failed")
	}
	if _, err = tx.Exec(manualJournalSchema); err != nil {
		return prepareRefusal("storage-failed")
	}
	if _, err = tx.Exec("INSERT INTO metadata VALUES ('manualExecutionDemand',?)", string(encode(q.Demand))); err != nil {
		return prepareRefusal("storage-failed")
	}
	if _, err = tx.Exec("UPDATE metadata SET value=? WHERE key='schema'", manualStoreSchema); err != nil {
		return prepareRefusal("storage-failed")
	}
	if verifyManualDatabasePath(lease, databaseIdentity) != nil {
		return prepareRefusal("unsafe-custody")
	}
	if err = tx.Commit(); err != nil {
		return prepareRefusal("storage-failed")
	}
	return ManualStorePrepareResult{Version: 1, Status: "ok", PrepareID: q.PrepareID, RequestDigest: record.RequestDigest, Delivery: "committed", StoreSchema: manualStoreSchema}
}
