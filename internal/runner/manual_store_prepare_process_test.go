//go:build linux || darwin || freebsd

package runner

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type manualLostPrepareReply struct{}

func (manualLostPrepareReply) Write([]byte) (int, error) {
	fmt.Println("prepared")
	<-time.After(10 * time.Second)
	return 0, io.ErrClosedPipe
}

func manualPrepareInChild(t *testing.T, raw []byte, file *os.File) ManualStorePrepareResult {
	t.Helper()
	return manualPrepareInChildMode(t, raw, file, "committed")
}
func manualPrepareInChildMode(t *testing.T, raw []byte, file *os.File, mode string) ManualStorePrepareResult {
	t.Helper()
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualStorePrepareSubprocessHelper$")
	cmd.Env = append(os.Environ(), "JPACK_MANUAL_PREPARE_CHILD="+mode)
	cmd.ExtraFiles = []*os.File{file}
	cmd.Stdin = bytes.NewReader(append(bytes.Clone(raw), '\n'))
	var output, diagnostic bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &diagnostic
	err := cmd.Run()
	var result ManualStorePrepareResult
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Status != "ok" && result.Status != "refused" || (err == nil) != (result.Status == "ok") {
		t.Fatalf("actual preparation child: %v %s %s", err, output.String(), diagnostic.String())
	}
	return result
}

func TestManualStorePrepareSubprocessHelper(t *testing.T) {
	mode := os.Getenv("JPACK_MANUAL_PREPARE_CHILD")
	if mode == "" {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 16385))
	if err != nil || len(raw) < 2 || raw[len(raw)-1] != '\n' {
		os.Exit(2)
	}
	raw = raw[:len(raw)-1]
	if mode == "target-probe" {
		if err := manualProcessTargetProbe(raw); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode == "substituted-prepare" {
		var request ManualStorePrepareRequest
		if manualDecode(raw, &request) != nil {
			os.Exit(2)
		}
		result := prepareManualStoreUsing(raw, nil, func(lease *ExecutorLease) (string, io.Closer, error) {
			target, closer, err := manualProcessDatabaseTarget(lease)
			if err != nil {
				return "", nil, err
			}
			if err = manualSubstituteState(request.Dir); err != nil {
				closer.Close()
				return "", nil, err
			}
			return target, closer, nil
		})
		json.NewEncoder(os.Stdout).Encode(result)
		if result.Status != "ok" {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode == "lose-reply" {
		RunManualStorePreparationProcess(raw, manualLostPrepareReply{})
	}
	RunManualStorePreparationProcess(raw, os.Stdout)
}

func TestManualStorePrepareActualChildRetainsOriginalCustody(t *testing.T) {
	for _, mode := range []string{"committed", "refused", "lose-reply"} {
		t.Run(mode, func(t *testing.T) {
			q, lease, cfg := manualPreparationFixture(t)
			if mode == "refused" {
				db, e := sqlOpenManualTest(cfg.Dir)
				executorMust(t, e)
				_, e = db.Exec(`INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES('old','job','caller','key','digest','queued','{}')`)
				executorMust(t, e)
				db.Close()
			}
			o := *cfg.ExecutorOwnership
			// Independent fresh opens attempt acquisition throughout the actual helper
			// child's lifetime. This catches LOCK_UN on the shared description; the
			// existing lease death tests cover final-descriptor process lifetimes.
			stop, done, first := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var acquired atomic.Bool
			go func() {
				defer close(done)
				once := true
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					contender, e := AcquireExecutorLease(o.Root, cfg.Dir, o.MachineWorkspace, o.identity())
					if e == nil {
						acquired.Store(true)
						contender.Close()
					}
					if once {
						close(first)
						once = false
					}
					select {
					case <-stop:
						return
					case <-ticker.C:
					}
				}
			}()
			<-first
			defer func() {
				close(stop)
				<-done
				if acquired.Load() {
					t.Error("helper transiently unlocked the continuously held original lease")
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualStorePrepareSubprocessHelper$")
			cmd.Env = append(os.Environ(), "JPACK_MANUAL_PREPARE_CHILD="+mode)
			cmd.Stdin = bytes.NewReader(append(encode(q), '\n'))
			stdout, e := cmd.StdoutPipe()
			executorMust(t, e)
			var diagnostic bytes.Buffer
			cmd.Stderr = &diagnostic
			_, e = lease.Start(cmd)
			executorMust(t, e)
			waited := false
			defer func() {
				if !waited {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			line := make(chan []byte, 1)
			go func() { raw, _ := bufio.NewReader(stdout).ReadBytes('\n'); line <- raw }()
			var response []byte
			select {
			case response = <-line:
			case <-time.After(5 * time.Second):
				t.Fatal("bounded helper child synchronization failed")
			}
			if mode == "lose-reply" {
				if string(response) != "prepared\n" {
					t.Fatalf("helper did not prepare: %s %s", response, diagnostic.String())
				}
				cmd.Process.Kill()
				cmd.Wait()
				waited = true
				if out := manualPrepareInChild(t, encode(q), manualLeaseDuplicate(t, lease)); out.Status != "ok" || out.Delivery != "recovered" {
					t.Fatalf("lost helper reply was not observation only: %+v", out)
				}
			} else {
				e = cmd.Wait()
				waited = true
				var result ManualStorePrepareResult
				if json.Unmarshal(response, &result) != nil {
					t.Fatalf("helper response %s %s", response, diagnostic.String())
				}
				if mode == "committed" && (e != nil || result.Delivery != "committed") || mode == "refused" && (e == nil || result.Status != "refused") {
					t.Fatalf("child result %+v %v %s", result, e, diagnostic.String())
				}
			}
			if contender, e := AcquireExecutorLease(o.Root, cfg.Dir, o.MachineWorkspace, o.identity()); e == nil {
				contender.Close()
				t.Fatal("helper ended parent custody")
			}
		})
	}
}

// Runs only in the explicit target-probe child, then that child exits. Exercise
// SQLite after adapter setup and named-directory substitution, including actual
// rollback journal, WAL/SHM and rejected non-family ATTACH names.
func manualProcessTargetProbe(raw []byte) error {
	var q ManualStorePrepareRequest
	if err := manualDecode(raw, &q); err != nil {
		return err
	}
	o, err := q.Ownership.ownership()
	if err != nil {
		return err
	}
	lease, err := adoptManualPreparationLease(o, q.Dir, os.NewFile(3, "probe-lease"))
	if err != nil {
		return err
	}
	defer lease.Close()
	target, anchor, err := manualProcessDatabaseTarget(lease)
	if err != nil {
		return err
	}
	defer anchor.Close()
	if err = manualSubstituteState(q.Dir); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", target)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA temp_store=MEMORY; PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL;"); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO metadata VALUES ('anchor-probe-journal','original')"); err != nil {
		tx.Rollback()
		return err
	}
	if err = manualProbeSidecar(lease, q.Dir, "runs.sqlite-journal"); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if _, err = db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return err
	}
	tx, err = db.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO metadata VALUES ('anchor-probe-wal','original')"); err != nil {
		tx.Rollback()
		return err
	}
	for _, name := range []string{"runs.sqlite-wal", "runs.sqlite-shm"} {
		if err = manualProbeSidecar(lease, q.Dir, name); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, name := range []string{"other.sqlite", filepath.Join(filepath.Dir(q.Dir), "escaped.sqlite")} {
		if _, e := db.Exec("ATTACH DATABASE ? AS unexpected", name); e == nil {
			return fmt.Errorf("private VFS accepted unrelated filename %s", name)
		}
		if _, e := os.Stat(name); !os.IsNotExist(e) {
			return fmt.Errorf("unrelated database created: %s %v", name, e)
		}
	}
	var sequence int
	var schema, file string
	if err = db.QueryRow("PRAGMA database_list").Scan(&sequence, &schema, &file); err != nil || file != "runs.sqlite" {
		return fmt.Errorf("SQLite recanonicalized retained relative file: %q %v", file, err)
	}
	fmt.Println(`{"retainedMain":true,"journal":true,"wal":true,"shm":true,"unrelatedAttachRefused":true}`)
	return nil
}
func manualProbeSidecar(lease *ExecutorLease, namedState, name string) error {
	held, err := lease.state.Stat(name)
	if err != nil || !held.Mode().IsRegular() {
		return fmt.Errorf("missing retained sidecar %s: %v", name, err)
	}
	current, err := os.Stat(name)
	if err != nil || !os.SameFile(held, current) {
		return fmt.Errorf("sidecar escaped retained cwd: %s", name)
	}
	if _, err = os.Stat(filepath.Join(namedState, name)); !os.IsNotExist(err) {
		return fmt.Errorf("sidecar reached substituted copy: %s %v", name, err)
	}
	return nil
}

func manualSubstituteState(state string) error {
	parked := state + "-parked"
	if err := os.Rename(state, parked); err != nil {
		return err
	}
	if err := os.Mkdir(state, 0700); err != nil {
		return err
	}
	return os.CopyFS(state, os.DirFS(parked))
}
