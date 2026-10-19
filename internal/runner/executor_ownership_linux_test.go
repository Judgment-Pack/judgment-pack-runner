//go:build linux

package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type ownedRuntimeProcessFixture struct {
	Config           Config
	Trail, Directory string
}

// This helper exercises the real Service common invocation, with no wrapper
// between Runner and the exact pinned Runtime. It deliberately keeps a request
// invocation alive independently of Service.Close's dispatcher cancellation.
func TestExecutorOwnedRuntimeProcessHelper(t *testing.T) {
	raw := os.Getenv("JPACK_OWNED_RUNTIME_OWNER")
	if raw == "" {
		return
	}
	var fixture ownedRuntimeProcessFixture
	executorMust(t, json.Unmarshal([]byte(raw), &fixture))
	fixture.Config.disableAutomation = true
	fixture.Config.disableDispatcher = true
	service, err := Open(fixture.Config)
	executorMust(t, err)
	defer service.Close()
	fmt.Println("owner-open")
	done := make(chan error, 1)
	go func() {
		_, err := service.invoke(context.Background(), service.runtime, fixture.Directory, "blocked-audit", "audit", "verify", "--trail", fixture.Trail, "--format", "json")
		done <- err
	}()
	command, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if command != "close\n" {
		t.Fatal("unknown helper command")
	}
	service.Close()
	fmt.Println("owner-closed")
	executorMust(t, <-done)
}

func TestExecutorOwnedRealRuntimeSurvivesOwnerDeath(t *testing.T) {
	real := testConfig(t)
	// The observer is isolated so Linux subreaping cannot affect any other tests.
	// It reaps the real Runtime after SIGKILL or a failed assertion, even where
	// container PID 1 would otherwise leave an orphan zombie behind.
	if os.Getenv("JPACK_OWNED_RUNTIME_OBSERVER") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecutorOwnedRealRuntimeSurvivesOwnerDeath$", "-test.v")
		cmd.Env = append(os.Environ(), "JPACK_OWNED_RUNTIME_OBSERVER=1", "JPACK_TEST_BIN="+real.Runtime)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated ownership observer: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0, 0, 0, 0); errno != 0 {
		t.Fatalf("subreaper: %v", errno)
	}
	defer syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0, 0, 0, 0)
	for _, mode := range []string{"close", "kill"} {
		t.Run(mode, func(t *testing.T) { ownedRuntimeLifetime(t, real.Runtime, mode) })
	}
}

func ownedRuntimeLifetime(t *testing.T, runtime, mode string) {
	t.Helper()
	f := executorFixture(t)
	cfg := executorOwnedConfig(f, runtime)
	directory := t.TempDir()
	trail, err := os.OpenFile(filepath.Join(directory, "held-audit.jsonl"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	executorMust(t, err)
	defer trail.Close() // observer's independent lock, never the executor lease
	executorMust(t, syscall.Flock(int(trail.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	trailInfo, err := trail.Stat()
	executorMust(t, err)
	fixture := ownedRuntimeProcessFixture{cfg, trail.Name(), directory}
	raw, err := json.Marshal(fixture)
	executorMust(t, err)
	owner := exec.Command(os.Args[0], "-test.run=^TestExecutorOwnedRuntimeProcessHelper$")
	owner.Env = append(os.Environ(), "JPACK_OWNED_RUNTIME_OWNER="+string(raw))
	input, err := owner.StdinPipe()
	executorMust(t, err)
	defer input.Close()
	output, err := owner.StdoutPipe()
	executorMust(t, err)
	var stderr bytes.Buffer
	owner.Stderr = &stderr
	executorMust(t, owner.Start())
	ownerWaited := false
	runtimePID := 0
	defer func() {
		if !ownerWaited {
			owner.Process.Kill()
			owner.Wait()
		}
		// This isolated subreaper has no unrelated children. Kill and reap any
		// orphan, including a Runtime whose readiness observation failed.
		for _, id := range ownedProcessChildren(os.Getpid()) {
			pid, err := strconv.Atoi(id)
			if err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
				reapOwnedRuntime(t, pid, 3*time.Second)
			}
		}
	}()
	reader := bufio.NewReader(output)
	readOwnedLine(t, reader, "owner-open")
	// Verify the launched image is the exact digest-pinned Runtime, not a mock
	// child. Seeing its open audit inode proves real Runtime startup reached the
	// audit snapshot boundary, where the observer's held flock blocks progress.
	image, err := os.ReadFile(runtime)
	executorMust(t, err)
	pinned := filepath.Join(cfg.Dir, "runtimes", strings.TrimPrefix(digest(image), "sha256:"))
	runtimeInfo, err := os.Stat(pinned)
	executorMust(t, err)
	runtimePID = waitOwnedRuntimeAudit(t, owner.Process.Pid, runtimeInfo, trailInfo, f.leaf())
	copied := filepath.Join(filepath.Dir(f.state), "copied-portable")
	copyOwnedStore(t, f.state, copied)
	if mode == "close" {
		_, err = io.WriteString(input, "close\n")
		executorMust(t, err)
		readOwnedLine(t, reader, "owner-closed")
	} else {
		executorMust(t, owner.Process.Kill())
		if err = owner.Wait(); err == nil {
			t.Fatal("killed owner exited successfully")
		}
		ownerWaited = true
	}
	for _, state := range []string{f.state, copied} {
		contender := cfg
		contender.Dir = state
		if service, err := Open(contender); err == nil {
			service.Close()
			t.Fatal("real Runtime lost ownership while owner stopped")
		}
	}
	// Release only the independent audit blocker. Eligibility is decided by
	// actual exclusive executor acquisition after Runtime exits, never by a PID,
	// sleep, process state or an elapsed deadline.
	executorMust(t, syscall.Flock(int(trail.Fd()), syscall.LOCK_UN))
	if mode == "close" {
		if err = owner.Wait(); err != nil {
			t.Fatalf("owner: %v %s", err, stderr.String())
		}
		ownerWaited = true
	} else {
		reapOwnedRuntime(t, runtimePID, 10*time.Second)
		runtimePID = 0
	}
	held, err := f.acquire()
	executorMust(t, err)
	held.Close()
}

func readOwnedLine(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err == nil && strings.TrimSpace(line) != want {
			err = fmt.Errorf("got %q, want %q", line, want)
		}
		done <- err
	}()
	select {
	case err := <-done:
		executorMust(t, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out awaiting %s", want)
	}
}

func waitOwnedRuntimeAudit(t *testing.T, owner int, image, trail os.FileInfo, lease string) int {
	t.Helper()
	leaseInfo, err := os.Stat(lease)
	executorMust(t, err)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, id := range ownedProcessChildren(owner) {
			pid, err := strconv.Atoi(id)
			if err != nil {
				continue
			}
			executable, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
			if err != nil || !os.SameFile(image, executable) {
				continue
			}
			fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
			auditOpen, leaseOpen := false, false
			for _, fd := range fds {
				info, err := os.Stat(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
				if err != nil {
					continue
				}
				auditOpen = auditOpen || os.SameFile(trail, info)
				leaseOpen = leaseOpen || os.SameFile(leaseInfo, info)
			}
			if auditOpen {
				if !leaseOpen {
					t.Fatal("actual Runtime closed its inherited executor descriptor during startup")
				}
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("actual pinned Runtime never reached the held audit-file boundary")
	return 0
}

func reapOwnedRuntime(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status syscall.WaitStatus
		got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if got == pid || err == syscall.ECHILD {
			return
		}
		if err != nil {
			t.Errorf("reap Runtime: %v", err)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("actual Runtime did not exit before fixture deadline")
}

func copyOwnedStore(t *testing.T, source, target string) {
	t.Helper()
	executorMust(t, filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.Mkdir(destination, 0700)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, output.Close())
	}))
}

// A Go subprocess may be started from any OS thread. Linux reports children
// per task, so observe every thread rather than assuming the main thread.
func ownedProcessChildren(pid int) []string {
	tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	var children []string
	for _, task := range tasks {
		raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, task.Name()))
		children = append(children, strings.Fields(string(raw))...)
	}
	return children
}
