//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestManualGovernedActualRuntimeHasNoAuthorityFD(t *testing.T) {
	cfg := testConfig(t)
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	s, err := Open(cfg)
	executorMust(t, err)
	defer s.Close()
	r := testRelease(t, s)
	job, err := s.createJob("fd-real", r.ID)
	executorMust(t, err)
	f := executorFixture(t)
	old := f.leaf()
	f.identity.Workspace = strings.Repeat("d", 64)
	executorMust(t, os.Rename(old, f.leaf()))
	f.state = cfg.Dir
	s.executor, err = f.acquire()
	executorMust(t, err)
	o := ExecutorOwnership{f.root, f.identity.Workspace, cfg.Workspace, f.identity.RootDevice, f.identity.RootInode, f.identity.FileDevice, f.identity.FileInode}
	s.cfg.ExecutorOwnership = &o
	_, err = s.db.Exec(manualJournalSchema)
	executorMust(t, err)
	syscall.ForkLock.RLock()
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(pair[0])
		syscall.CloseOnExec(pair[1])
	}
	syscall.ForkLock.RUnlock()
	executorMust(t, err)
	left, right := os.NewFile(uintptr(pair[0]), "runner-authority"), os.NewFile(uintptr(pair[1]), "host-authority")
	leftInfo, err := left.Stat()
	executorMust(t, err)
	rightInfo, err := right.Stat()
	executorMust(t, err)
	a, err := net.FileConn(left)
	executorMust(t, err)
	b, err := net.FileConn(right)
	executorMust(t, err)
	left.Close()
	right.Close()
	var trail *os.File
	var runID string
	ready := make(chan error, 1)
	manualGateAuthorityConnections(t, s, a, b, func(method string, _ json.RawMessage) (string, any, bool) {
		if method == "commitStart" {
			path := filepath.Join(cfg.Dir, "attempts", runID, "audit")
			e := os.Mkdir(path, 0700)
			if e == nil {
				trail, e = os.OpenFile(filepath.Join(path, "evaluations.jsonl"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			}
			if e == nil {
				e = syscall.Flock(int(trail.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			}
			ready <- e
			if e != nil {
				return "", nil, true
			}
		}
		return "", nil, false
	})
	executorMust(t, os.Mkdir(manualDemandDirectory(o), 0700))
	executorMust(t, os.WriteFile(filepath.Join(manualDemandDirectory(o), o.MachineWorkspace), encode(s.cfg.ExecutorAdmission.demand()), 0600))
	raw := []byte(`{"facts":` + string(sample().Facts) + `,"evidence":` + string(sample().Evidence) + `}`)
	intent, err := captureManualIntent("intent_fd_real", "fd-key", raw)
	executorMust(t, err)
	run, _, err := s.submitInternalIntent(job.ID, "fd-key", intent.input(), nil, intent)
	executorMust(t, err)
	runID = run.ID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := s.dispatchNext(ctx); done <- e }()
	select {
	case e := <-ready:
		executorMust(t, e)
	case <-time.After(5 * time.Second):
		t.Fatal("commit boundary not reached")
	}
	defer func() {
		trail.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("actual Runtime did not reap")
		}
	}()
	image, err := os.Stat(s.runtime)
	executorMust(t, err)
	audit, err := trail.Stat()
	executorMust(t, err)
	pid := waitOwnedRuntimeAudit(t, os.Getpid(), image, audit, f.leaf())
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	executorMust(t, err)
	for _, fd := range fds {
		info, e := os.Stat(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		if e == nil && (os.SameFile(info, leftInfo) || os.SameFile(info, rightInfo)) {
			t.Fatal("actual pinned Runtime inherited authority endpoint", fd.Name())
		}
	}
	executorMust(t, trail.Close())
	select {
	case e := <-done:
		executorMust(t, e)
		done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("actual Runtime failed after audit release")
	}
	finished, err := s.run(runID)
	executorMust(t, err)
	if finished.State != "completed" {
		t.Fatal(finished.State, finished.Problem)
	}
}
