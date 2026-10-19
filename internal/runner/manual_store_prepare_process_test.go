//go:build linux || darwin || freebsd

package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestManualStorePrepareSubprocessHelper(t *testing.T) {
	mode := os.Getenv("JPACK_MANUAL_PREPARE_CHILD")
	if mode == "" {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 16385))
	if err != nil || len(raw) < 2 || raw[len(raw)-1] != '\n' {
		os.Exit(2)
	}
	result := PrepareManualStore(raw[:len(raw)-1])
	if mode == "lose-reply" {
		fmt.Println("prepared")
		<-time.After(10 * time.Second)
		os.Exit(2)
	}
	json.NewEncoder(os.Stdout).Encode(result)
	if result.Status != "ok" {
		os.Exit(1)
	}
	os.Exit(0)
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
			cmd := exec.Command(os.Args[0], "-test.run=^TestManualStorePrepareSubprocessHelper$")
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
				if out := prepareManualStore(encode(q), manualLeaseDuplicate(t, lease)); out.Status != "ok" || out.Delivery != "recovered" {
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
