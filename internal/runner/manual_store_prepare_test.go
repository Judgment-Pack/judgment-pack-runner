//go:build linux || darwin || freebsd

package runner

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sqlOpenManualTest(dir string) (*sql.DB, error) {
	return sql.Open("sqlite", filepath.Join(dir, "runs.sqlite"))
}

func manualPreparationFixture(t *testing.T) (ManualStorePrepareRequest, *ExecutorLease, Config) {
	t.Helper()
	f := executorFixture(t)
	old := f.leaf()
	f.identity.Workspace = strings.Repeat("a", 64)
	executorMust(t, os.Rename(old, f.leaf()))
	cfg := Config{Dir: f.state, Workspace: "runner-test", Owner: "local-admin", Runtime: os.Args[0], disableDispatcher: true, disableAutomation: true}
	s, err := Open(cfg)
	executorMust(t, err)
	s.Close()
	o := ExecutorOwnership{f.root, f.identity.Workspace, cfg.Workspace, f.identity.RootDevice, f.identity.RootInode, f.identity.FileDevice, f.identity.FileInode}
	d := ManualExecutionDemand{1, strings.Repeat("b", 64), f.identity.Workspace, cfg.Workspace, strings.Repeat("c", 64), ManualAdmissionProfile}
	executorMust(t, os.Mkdir(manualDemandDirectory(o), 0700))
	executorMust(t, os.WriteFile(filepath.Join(manualDemandDirectory(o), d.MachineWorkspace), encode(d), 0600))
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	q := ManualStorePrepareRequest{1, strings.Repeat("d", 64), cfg.Dir, cfg.Workspace, cfg.Owner, 3, ManualOwnershipWire{1, o.Root, o.MachineWorkspace, o.RunnerWorkspace, u(o.RootDevice), u(o.RootInode), u(o.FileDevice), u(o.FileInode)}, d}
	lease, err := f.acquire()
	executorMust(t, err)
	t.Cleanup(func() { lease.Close() })
	cfg.ExecutorOwnership = &o
	return q, lease, cfg
}
func manualLeaseDuplicate(t *testing.T, l *ExecutorLease) *os.File {
	t.Helper()
	fd, err := syscall.Dup(int(l.file.Fd()))
	executorMust(t, err)
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "prepare-lease")
}
func TestManualStorePreparationDurableAndNoDowngrade(t *testing.T) {
	q, lease, cfg := manualPreparationFixture(t)
	raw := encode(q)
	out := manualPrepareInChild(t, raw, manualLeaseDuplicate(t, lease))
	if out.Status != "ok" || out.Delivery != "committed" || out.RequestDigest != digest(raw) {
		t.Fatalf("prepare %+v", out)
	}
	if contender, err := AcquireExecutorLease(cfg.ExecutorOwnership.Root, cfg.Dir, q.Demand.MachineWorkspace, cfg.ExecutorOwnership.identity()); err == nil {
		contender.Close()
		t.Fatal("helper released original ownership")
	}
	again := manualPrepareInChild(t, raw, manualLeaseDuplicate(t, lease))
	if again.Status != "ok" || again.Delivery != "recovered" {
		t.Fatalf("exact recovered observation %+v", again)
	}
	lease.Close()
	if s, err := Open(cfg); err == nil {
		s.Close()
		t.Fatal("omitted admission ignored external demand")
	}
	legacy := cfg
	legacy.ExecutorOwnership = nil
	if s, err := Open(legacy); err == nil {
		s.Close()
		t.Fatal("schema4 downgraded without trusted boot")
	}
	old := os.Getenv("JPACK_OLD_RUNNER_TEST_BIN")
	if old != "" {
		image, err := os.ReadFile(old)
		executorMust(t, err)
		t.Logf("actual old Runner schema4 refusal: %s %s", old, digest(image))
		boot := map[string]any{"dir": cfg.Dir, "workspace": cfg.Workspace, "owner": cfg.Owner, "runtime": cfg.Runtime, "token": strings.Repeat("x", 32)}
		cmd := exec.Command(old)
		cmd.Stdin = bytes.NewReader(append(encode(boot), '\n'))
		output, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(output, []byte("another owner, workspace or schema")) {
			t.Fatalf("old Runner did not refuse schema4: %v %s", err, output)
		}
	}
}

func TestManualStorePreparationSubstitutedStateRefuses(t *testing.T) {
	for _, phase := range []string{"before-adapter", "after-adapter"} {
		t.Run(phase, func(t *testing.T) {
			q, parent, _ := manualPreparationFixture(t)
			parked := q.Dir + "-parked"
			defer func() { os.RemoveAll(q.Dir); os.Rename(parked, q.Dir) }()
			var out ManualStorePrepareResult
			if phase == "before-adapter" {
				o, err := q.Ownership.ownership()
				executorMust(t, err)
				adopted, err := adoptManualPreparationLease(o, q.Dir, manualLeaseDuplicate(t, parent))
				executorMust(t, err)
				defer adopted.Close()
				executorMust(t, manualSubstituteState(q.Dir))
				out = prepareManualStoreHeld(q, encode(q), adopted)
			} else {
				out = manualPrepareInChildMode(t, encode(q), manualLeaseDuplicate(t, parent), "substituted-prepare")
			}
			if out.Status != "refused" || out.Code != "unsafe-custody" {
				t.Fatalf("substituted state %+v", out)
			}
			for _, state := range []string{parked, q.Dir} {
				if _, err := os.Stat(filepath.Join(state, manualPrepareMarker)); !os.IsNotExist(err) {
					t.Fatal("replaced state published marker", state, err)
				}
				db, err := sqlOpenManualTest(state)
				executorMust(t, err)
				var schema string
				executorMust(t, db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema))
				var demand, tables int
				executorMust(t, db.QueryRow("SELECT count(*) FROM metadata WHERE key='manualExecutionDemand'").Scan(&demand))
				executorMust(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='manual_attempts'").Scan(&tables))
				db.Close()
				if schema != storeSchema || demand != 0 || tables != 0 {
					t.Fatalf("substitution published usable state in %s: schema=%s demand=%d journal=%d", state, schema, demand, tables)
				}
			}
		})
	}
}

func TestManualStoreSQLiteTargetUsesHeldDirectory(t *testing.T) {
	q, parent, _ := manualPreparationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualStorePrepareSubprocessHelper$")
	cmd.Env = append(os.Environ(), "JPACK_MANUAL_PREPARE_CHILD=target-probe")
	cmd.Stdin = bytes.NewReader(append(encode(q), '\n'))
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_, err := parent.Start(cmd)
	executorMust(t, err)
	if err = cmd.Wait(); err != nil {
		t.Fatalf("process retained target: %v %s", err, output.String())
	}
	if !bytes.Contains(output.Bytes(), []byte(`"shm":true`)) {
		t.Fatal("actual native sidecar probe missing", output.String())
	}
	t.Logf("actual native relative VFS: %s", output.String())
	parked := q.Dir + "-parked"
	defer func() { os.RemoveAll(q.Dir); os.Rename(parked, q.Dir) }()
	for _, state := range []string{parked, q.Dir} {
		db, e := sqlOpenManualTest(state)
		executorMust(t, e)
		var n int
		executorMust(t, db.QueryRow("SELECT count(*) FROM metadata WHERE key IN ('anchor-probe-journal','anchor-probe-wal')").Scan(&n))
		db.Close()
		if state == parked && n != 2 || state == q.Dir && n != 0 {
			t.Fatal("SQLite descriptor target redirected", state, n)
		}
	}
}
func TestManualStorePreparationPendingAndUnsupported(t *testing.T) {
	for _, kind := range []string{"queued", "paused-trigger", "marker", "separate-description", "unheld-description", "uint64-overflow"} {
		t.Run(kind, func(t *testing.T) {
			q, lease, cfg := manualPreparationFixture(t)
			var file *os.File
			switch kind {
			case "queued", "paused-trigger":
				db, err := sqlOpenManualTest(cfg.Dir)
				executorMust(t, err)
				if kind == "queued" {
					_, err = db.Exec(`INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES('old','job','caller','key','digest','queued','{}')`)
				} else {
					_, err = db.Exec(`INSERT INTO triggers(id,job_id,record) VALUES('trigger','job','{"paused":true}')`)
				}
				executorMust(t, err)
				db.Close()
			case "marker":
				executorMust(t, os.WriteFile(filepath.Join(cfg.Dir, manualPrepareMarker), []byte(`{}`), 0600))
			case "uint64-overflow":
				q.Ownership.RootInode = "18446744073709551616"
			case "separate-description", "unheld-description":
				if kind == "unheld-description" {
					lease.Close()
				}
				var err error
				file, err = os.OpenFile(filepath.Join(q.Ownership.Root, q.Demand.MachineWorkspace+".lease"), os.O_RDWR, 0)
				executorMust(t, err)
			}
			if file == nil {
				file = manualLeaseDuplicate(t, lease)
			}
			defer file.Close()
			out := manualPrepareInChild(t, encode(q), file)
			if out.Status != "refused" {
				t.Fatalf("unsupported transition %+v", out)
			}
			db, err := sqlOpenManualTest(cfg.Dir)
			executorMust(t, err)
			defer db.Close()
			var schema string
			executorMust(t, db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema))
			if schema != storeSchema {
				t.Fatal("refusal published schema4")
			}
		})
	}
}

func TestManualStoreLibraryNeverChangesCWD(t *testing.T) {
	q, lease, _ := manualPreparationFixture(t)
	before, err := os.Stat(".")
	executorMust(t, err)
	out := prepareManualStore(encode(q), manualLeaseDuplicate(t, lease))
	if runtime.GOOS == "linux" {
		if out.Status != "ok" {
			t.Fatal(out)
		}
	} else if out.Status != "refused" || out.Code != "unsafe-custody" {
		t.Fatal("library must refuse unsupported anchored path", out)
	}
	after, err := os.Stat(".")
	executorMust(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("library changed caller cwd")
	}
}

func TestManualExternalDemandCannotBeOmitted(t *testing.T) {
	q, lease, cfg := manualPreparationFixture(t)
	lease.Close()
	if _, err := Open(cfg); err != errManualProtocol {
		t.Fatalf("missing admission must refuse before runtime/recovery: %v", err)
	}
	o := *cfg.ExecutorOwnership
	current := filepath.Join(manualDemandDirectory(o), q.Demand.MachineWorkspace)
	executorMust(t, os.Remove(current))
	if checkManualDemand(&o, nil) == nil {
		t.Fatal("empty fixed directory accepted")
	}
	other := q.Demand
	other.MachineWorkspace = strings.Repeat("e", 64)
	executorMust(t, os.WriteFile(filepath.Join(manualDemandDirectory(o), other.MachineWorkspace), encode(other), 0600))
	if err := checkManualDemand(&o, nil); err != nil {
		t.Fatal("other workspace changed legacy current workspace", err)
	}
	b := ManualAdmissionBinding{Version: 1, FD: 3, InstallationID: q.Demand.InstallationID, MachineWorkspace: q.Demand.MachineWorkspace, RunnerWorkspace: q.Demand.RunnerWorkspace, ExecutorID: q.Demand.ExecutorID, Challenge: strings.Repeat("f", 64), Profile: ManualAdmissionProfile}
	if checkManualDemand(&o, &b) == nil {
		t.Fatal("enabled current workspace without demand accepted")
	}
	executorMust(t, os.WriteFile(current, encode(q.Demand), 0600))
	if err := checkManualDemand(&o, &b); err != nil {
		t.Fatal(err)
	}
	executorMust(t, os.Chmod(current, 0644))
	if checkManualDemand(&o, &b) == nil {
		t.Fatal("public demand accepted")
	}
}
