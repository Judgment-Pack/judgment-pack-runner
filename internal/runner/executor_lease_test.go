//go:build linux || darwin || freebsd

package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type executorLeaseFixture struct {
	root, state string
	identity    ExecutorLeaseIdentity
}

func executorFixture(t *testing.T) executorLeaseFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := executorLeaseFixture{root: filepath.Join(base, "host"), state: filepath.Join(base, "portable"), identity: ExecutorLeaseIdentity{Workspace: "workspace-1"}}
	for _, path := range []string{f.root, f.state} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.leaf(), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Test-only trusted provisioning. Production deliberately has no operation
	// that infers enrollment authority from the current path or restored store.
	f.identity.RootDevice, f.identity.RootInode = executorFixtureIdentity(t, f.root)
	f.identity.FileDevice, f.identity.FileInode = executorFixtureIdentity(t, f.leaf())
	return f
}

func executorFixtureIdentity(t *testing.T, path string) (uint64, uint64) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return uint64(st.Dev), uint64(st.Ino)
}

func (f executorLeaseFixture) leaf() string {
	return filepath.Join(f.root, f.identity.Workspace+".lease")
}

func (f executorLeaseFixture) acquire() (*ExecutorLease, error) {
	return AcquireExecutorLease(f.root, f.state, f.identity.Workspace, f.identity)
}

func executorMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecutorLeasePrivatePinnedExternalCustody(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testing.T, *executorLeaseFixture)
	}{
		{"missing root", func(t *testing.T, f *executorLeaseFixture) { executorMust(t, os.RemoveAll(f.root)) }},
		{"missing state", func(t *testing.T, f *executorLeaseFixture) { executorMust(t, os.Remove(f.state)) }},
		{"missing leaf", func(t *testing.T, f *executorLeaseFixture) { executorMust(t, os.Remove(f.leaf())) }},
		{"unbound workspace", func(t *testing.T, f *executorLeaseFixture) { f.identity.Workspace = "other" }},
		{"no root pin", func(t *testing.T, f *executorLeaseFixture) { f.identity.RootInode = 0 }},
		{"no file pin", func(t *testing.T, f *executorLeaseFixture) { f.identity.FileInode = 0 }},
		{"wrong root pin", func(t *testing.T, f *executorLeaseFixture) { f.identity.RootInode++ }},
		{"wrong file pin", func(t *testing.T, f *executorLeaseFixture) { f.identity.FileInode++ }},
		{"relative root", func(t *testing.T, f *executorLeaseFixture) { f.root = "host" }},
		{"unclean root", func(t *testing.T, f *executorLeaseFixture) { f.root += "/." }},
		{"relative state", func(t *testing.T, f *executorLeaseFixture) { f.state = "portable" }},
		{"root too long", func(t *testing.T, f *executorLeaseFixture) { f.root = "/" + strings.Repeat("a", 4096) }},
		{"root too deep", func(t *testing.T, f *executorLeaseFixture) { f.root = "/" + strings.Repeat("a/", 128) + "a" }},
		{"root symlink", func(t *testing.T, f *executorLeaseFixture) {
			path := f.root + "-link"
			executorMust(t, os.Symlink(f.root, path))
			f.root = path
		}},
		{"ancestor symlink", func(t *testing.T, f *executorLeaseFixture) {
			path := filepath.Dir(f.root) + "-link"
			executorMust(t, os.Symlink(filepath.Dir(f.root), path))
			t.Cleanup(func() { os.Remove(path) })
			f.root = filepath.Join(path, "host")
		}},
		{"state symlink", func(t *testing.T, f *executorLeaseFixture) {
			path := f.state + "-link"
			executorMust(t, os.Symlink(f.state, path))
			f.state = path
		}},
		{"root permissions", func(t *testing.T, f *executorLeaseFixture) { executorMust(t, os.Chmod(f.root, 0750)) }},
		{"state permissions", func(t *testing.T, f *executorLeaseFixture) { executorMust(t, os.Chmod(f.state, 0750)) }},
		{"leaf permissions", func(t *testing.T, f *executorLeaseFixture) { executorMust(t, os.Chmod(f.leaf(), 0640)) }},
		{"hardlink", func(t *testing.T, f *executorLeaseFixture) { executorMust(t, os.Link(f.leaf(), f.leaf()+"-alias")) }},
		{"leaf symlink", func(t *testing.T, f *executorLeaseFixture) {
			executorMust(t, os.Rename(f.leaf(), f.leaf()+"-original"))
			executorMust(t, os.Symlink(f.leaf()+"-original", f.leaf()))
		}},
		{"directory leaf", func(t *testing.T, f *executorLeaseFixture) {
			executorMust(t, os.Remove(f.leaf()))
			executorMust(t, os.Mkdir(f.leaf(), 0700))
		}},
		{"FIFO leaf", func(t *testing.T, f *executorLeaseFixture) {
			executorMust(t, os.Remove(f.leaf()))
			executorMust(t, syscall.Mkfifo(f.leaf(), 0600))
		}},
		{"same root and state", func(t *testing.T, f *executorLeaseFixture) { f.state = f.root }},
		{"state contains root", func(t *testing.T, f *executorLeaseFixture) { f.state = filepath.Dir(f.root) }},
		{"root contains state", func(t *testing.T, f *executorLeaseFixture) {
			f.state = filepath.Join(f.root, "portable")
			executorMust(t, os.Mkdir(f.state, 0700))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := executorFixture(t)
			tc.edit(t, &f)
			l, err := f.acquire()
			if err == nil {
				l.Close()
				t.Fatal("unsafe or missing custody was accepted")
			}
		})
	}
	t.Run("never recreates missing state", func(t *testing.T) {
		f := executorFixture(t)
		executorMust(t, os.Remove(f.leaf()))
		_, err := f.acquire()
		if err == nil {
			t.Fatal("missing lease accepted")
		}
		if _, err := os.Lstat(f.leaf()); !os.IsNotExist(err) {
			t.Fatalf("missing leaf was recreated: %v", err)
		}
	})
	t.Run("workspace mismatch", func(t *testing.T) {
		f := executorFixture(t)
		if _, err := AcquireExecutorLease(f.root, f.state, "other", f.identity); err == nil {
			t.Fatal("a different workspace used the expected identity")
		}
	})
	for _, id := range []string{"", ".", "..", "a/b", "a\\b", "with space", "é", strings.Repeat("a", 129)} {
		t.Run("invalid id "+strconv.Quote(id), func(t *testing.T) {
			f := executorFixture(t)
			f.identity.Workspace = id
			if l, err := f.acquire(); err == nil {
				l.Close()
				t.Fatal("invalid workspace id accepted")
			}
		})
	}
}

func TestExecutorLeaseForeignOwnership(t *testing.T) {
	for _, object := range []string{"root", "state", "leaf"} {
		t.Run(object, func(t *testing.T) {
			f := executorFixture(t)
			path := map[string]string{"root": f.root, "state": f.state, "leaf": f.leaf()}[object]
			foreign := os.Geteuid() + 1
			if err := os.Chown(path, foreign, -1); err != nil {
				t.Skipf("host privilege needed for foreign-owner fixture: %v", err)
			}
			t.Cleanup(func() { os.Chown(path, os.Geteuid(), -1) })
			if l, err := f.acquire(); err == nil {
				l.Close()
				t.Fatal("foreign-owned custody accepted")
			}
		})
	}
}

func TestExecutorLeaseRejectsReplacementWhileHeld(t *testing.T) {
	for _, object := range []string{"root", "state", "leaf", "unlinked leaf", "changed mode", "new hardlink"} {
		t.Run(object, func(t *testing.T) {
			f := executorFixture(t)
			l, err := f.acquire()
			executorMust(t, err)
			defer l.Close()
			originalLeaf := f.leaf()
			switch object {
			case "root":
				executorMust(t, os.Rename(f.root, f.root+"-original"))
				originalLeaf = filepath.Join(f.root+"-original", f.identity.Workspace+".lease")
				executorMust(t, os.Mkdir(f.root, 0700))
				executorMust(t, os.WriteFile(f.leaf(), nil, 0600))
			case "state":
				executorMust(t, os.Rename(f.state, f.state+"-original"))
				executorMust(t, os.Mkdir(f.state, 0700))
			case "leaf":
				originalLeaf += "-original"
				executorMust(t, os.Rename(f.leaf(), originalLeaf))
				executorMust(t, os.WriteFile(f.leaf(), nil, 0600))
			case "unlinked leaf":
				executorMust(t, os.Remove(f.leaf()))
			case "changed mode":
				executorMust(t, os.Chmod(f.leaf(), 0644))
			case "new hardlink":
				executorMust(t, os.Link(f.leaf(), f.leaf()+"-alias"))
			}
			// A valid executable makes this an actual custody refusal check;
			// an absent command would fail even if validation were removed.
			cmd := exec.Command(os.Args[0], "-test.run=^TestExecutorLeaseProcessHelper$")
			_, startErr := l.Start(cmd)
			if cmd.Process != nil {
				cmd.Wait()
			}
			if startErr == nil || cmd.Process != nil {
				t.Fatal("changed custody allowed a launch")
			}
			if object != "state" {
				if other, err := f.acquire(); err == nil {
					other.Close()
					t.Fatal("changed custody allowed another owner")
				}
			}
			if object != "unlinked leaf" {
				file, err := os.OpenFile(originalLeaf, os.O_RDWR, 0)
				executorMust(t, err)
				defer file.Close()
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
					t.Fatal("refusing changed custody released the original lock")
				}
			}
		})
	}
}

func TestExecutorLeaseStartFailureAndClose(t *testing.T) {
	f := executorFixture(t)
	l, err := f.acquire()
	executorMust(t, err)
	if _, err := l.Start(nil); err == nil {
		t.Fatal("nil command accepted")
	}
	cmd := exec.Command(filepath.Join(f.state, "missing-command"))
	marker, err := os.Open(f.state)
	executorMust(t, err)
	defer marker.Close()
	cmd.ExtraFiles = []*os.File{marker, nil}
	if fd, err := l.Start(cmd); err == nil || fd != -1 {
		t.Fatalf("missing command started: fd=%d error=%v", fd, err)
	}
	if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != marker || cmd.ExtraFiles[1] != nil {
		t.Fatal("Start failure changed existing ExtraFiles")
	}
	if other, err := f.acquire(); err == nil {
		other.Close()
		t.Fatal("failed Start released the parent lease")
	}
	executorMust(t, l.Close())
	executorMust(t, l.Close())
	if _, err := l.Start(exec.Command(os.Args[0], "-test.run=^TestExecutorLeaseProcessHelper$")); err == nil {
		t.Fatal("closed lease allowed Start")
	}
	var absent *ExecutorLease
	executorMust(t, absent.Close())
	if _, err := absent.Start(cmd); err == nil {
		t.Fatal("nil lease allowed Start")
	}
	other, err := f.acquire()
	executorMust(t, err)
	executorMust(t, other.Close())
}

func TestExecutorLeaseCopiedExternalCustodyRefused(t *testing.T) {
	f := executorFixture(t)
	copyRoot := f.root + "-copy"
	executorMust(t, os.Mkdir(copyRoot, 0700))
	executorMust(t, os.WriteFile(filepath.Join(copyRoot, f.identity.Workspace+".lease"), nil, 0600))
	if l, err := AcquireExecutorLease(copyRoot, f.state, f.identity.Workspace, f.identity); err == nil {
		l.Close()
		t.Fatal("copied root/file established ownership against the original trusted pin")
	}
}

const executorHelperMode = "JPACK_EXECUTOR_LEASE_TEST_MODE"

func executorHelperCommand(f executorLeaseFixture, mode string, status, release *os.File) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecutorLeaseProcessHelper$")
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "JPACK_EXECUTOR_LEASE_TEST_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	identity, _ := json.Marshal(f.identity)
	cmd.Env = append(cmd.Env, executorHelperMode+"="+mode, "JPACK_EXECUTOR_LEASE_TEST_ROOT="+f.root,
		"JPACK_EXECUTOR_LEASE_TEST_STATE="+f.state, "JPACK_EXECUTOR_LEASE_TEST_IDENTITY="+string(identity))
	cmd.ExtraFiles = []*os.File{status, release}
	return cmd
}

// Real subprocesses, including an actual grandchild, hold the inherited open
// description. Pipes coordinate observations; test deadlines never determine
// lock eligibility. EOF on the status pipe is a process-exit observation.
func TestExecutorLeaseProcessHelper(t *testing.T) {
	mode := os.Getenv(executorHelperMode)
	if mode == "" {
		return
	}
	status, release := os.NewFile(3, "status"), os.NewFile(4, "release")
	if mode == "child" {
		lease := os.NewFile(5, "inherited-lease")
		if _, err := lease.Stat(); err != nil {
			os.Exit(21)
		}
		fmt.Fprintln(status, "child-ready")
		var one [1]byte
		_, _ = release.Read(one[:])
		// No explicit close or unlock: exit closes the last inherited copy.
		os.Exit(0)
	}
	var identity ExecutorLeaseIdentity
	if err := json.Unmarshal([]byte(os.Getenv("JPACK_EXECUTOR_LEASE_TEST_IDENTITY")), &identity); err != nil {
		os.Exit(22)
	}
	f := executorLeaseFixture{root: os.Getenv("JPACK_EXECUTOR_LEASE_TEST_ROOT"), state: os.Getenv("JPACK_EXECUTOR_LEASE_TEST_STATE"), identity: identity}
	l, err := f.acquire()
	if err != nil {
		os.Exit(23)
	}
	cmd := executorHelperCommand(f, "child", status, release)
	if fd, err := l.Start(cmd); err != nil || fd != 5 {
		os.Exit(24)
	}
	fmt.Fprintln(status, "owner-ready")
	status.Close()
	release.Close()
	// The test kills this intermediate owner while its child is still alive.
	_ = cmd.Wait()
	l.Close()
	os.Exit(0)
}

type executorProcessPipes struct {
	statusR, statusW, releaseR, releaseW *os.File
	lines                                <-chan string
}

func executorPipes(t *testing.T) executorProcessPipes {
	t.Helper()
	r, w, err := os.Pipe()
	executorMust(t, err)
	rr, rw, err := os.Pipe()
	executorMust(t, err)
	p := executorProcessPipes{statusR: r, statusW: w, releaseR: rr, releaseW: rw}
	for _, file := range []*os.File{r, w, rr, rw} {
		t.Cleanup(func() { file.Close() })
	}
	lines := make(chan string, 4)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	p.lines = lines
	return p
}

func executorReadLine(t *testing.T, lines <-chan string) (string, bool) {
	t.Helper()
	select {
	case line, ok := <-lines:
		return line, ok
	case <-time.After(10 * time.Second):
		t.Fatal("process fixture did not reach its synchronization point")
		return "", false
	}
}

func executorAssertBusy(t *testing.T, f executorLeaseFixture) {
	t.Helper()
	if l, err := f.acquire(); err == nil {
		l.Close()
		t.Fatal("another executor acquired the still-live child's lease")
	}
	// A copied/restored portable store with the same trusted external identity
	// is subject to the very same lock; a store path is not lease authority.
	copyState := f.state + "-copy"
	executorMust(t, os.MkdirAll(copyState, 0700))
	if l, err := AcquireExecutorLease(f.root, copyState, f.identity.Workspace, f.identity); err == nil {
		l.Close()
		t.Fatal("copied portable store acquired the live child's lease")
	}
}

func executorAssertReleased(t *testing.T, f executorLeaseFixture) {
	t.Helper()
	l, err := f.acquire()
	executorMust(t, err)
	executorMust(t, l.Close())
}

// An orphan's status descriptor can close a few instructions before its lease
// descriptor during exit. Only successful kernel acquisition establishes that
// ownership ended; this deadline merely bounds a failed test.
func executorAwaitReleased(t *testing.T, f executorLeaseFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := f.acquire()
		if err == nil {
			executorMust(t, l.Close())
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child exit did not release the kernel lease: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func executorCleanupProcess(t *testing.T, cmd *exec.Cmd, p executorProcessPipes) {
	t.Helper()
	t.Cleanup(func() {
		p.releaseW.Close()
		if cmd.ProcessState == nil {
			cmd.Wait()
		}
	})
}

func TestExecutorLeaseChildSurvivesParentClose(t *testing.T) {
	f := executorFixture(t)
	l, err := f.acquire()
	executorMust(t, err)
	defer l.Close()
	p := executorPipes(t)
	cmd := executorHelperCommand(f, "child", p.statusW, p.releaseR)
	original := cmd.ExtraFiles
	fd, err := l.Start(cmd)
	executorMust(t, err)
	executorCleanupProcess(t, cmd, p)
	if fd != 5 || len(cmd.ExtraFiles) != 2 || &cmd.ExtraFiles[0] != &original[0] {
		t.Fatal("Start did not preserve ExtraFiles or report the inherited descriptor")
	}
	p.statusW.Close()
	p.releaseR.Close()
	if line, ok := executorReadLine(t, p.lines); !ok || line != "child-ready" {
		t.Fatalf("unexpected child status %q", line)
	}
	executorMust(t, l.Close())
	executorAssertBusy(t, f)
	p.releaseW.Close()
	if line, ok := executorReadLine(t, p.lines); ok {
		t.Fatalf("expected child exit, got %q", line)
	}
	executorMust(t, cmd.Wait())
	executorAssertReleased(t, f)
}

func TestExecutorLeaseChildSurvivesKilledIntermediateOwner(t *testing.T) {
	f := executorFixture(t)
	p := executorPipes(t)
	cmd := executorHelperCommand(f, "owner", p.statusW, p.releaseR)
	executorMust(t, cmd.Start())
	t.Cleanup(func() {
		p.releaseW.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	p.statusW.Close()
	p.releaseR.Close()
	seen := map[string]bool{}
	for len(seen) < 2 {
		line, ok := executorReadLine(t, p.lines)
		if !ok || line != "owner-ready" && line != "child-ready" {
			t.Fatalf("unexpected owner/child status %q", line)
		}
		seen[line] = true
	}
	executorMust(t, cmd.Process.Kill())
	err := cmd.Wait()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("intermediate owner was not killed: %v", err)
	}
	executorAssertBusy(t, f)
	p.releaseW.Close()
	if line, ok := executorReadLine(t, p.lines); ok {
		t.Fatalf("expected actual child's exit, got %q", line)
	}
	executorAwaitReleased(t, f)
}

func TestExecutorLeaseCloseStartRace(t *testing.T) {
	for i := 0; i < 8; i++ {
		f := executorFixture(t)
		l, err := f.acquire()
		executorMust(t, err)
		p := executorPipes(t)
		cmd := executorHelperCommand(f, "child", p.statusW, p.releaseR)
		start := make(chan struct{})
		closed := make(chan error, 1)
		go func() {
			<-start
			closed <- l.Close()
		}()
		close(start)
		_, startErr := l.Start(cmd)
		executorMust(t, <-closed)
		if cmd.Process != nil {
			executorCleanupProcess(t, cmd, p)
		}
		p.statusW.Close()
		p.releaseR.Close()
		if startErr != nil {
			if cmd.Process != nil {
				t.Fatalf("failed Start retained a process: %v", startErr)
			}
			p.releaseW.Close()
			executorAssertReleased(t, f)
			continue
		}
		if line, ok := executorReadLine(t, p.lines); !ok || line != "child-ready" {
			t.Fatalf("unexpected child status %q", line)
		}
		executorAssertBusy(t, f)
		p.releaseW.Close()
		if line, ok := executorReadLine(t, p.lines); ok {
			t.Fatalf("expected child exit, got %q", line)
		}
		executorMust(t, cmd.Wait())
		executorAssertReleased(t, f)
	}
}
