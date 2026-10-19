//go:build linux || darwin || freebsd

package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestManualInheritedAuthorityDescriptorHelper(t *testing.T) {
	role := os.Getenv("JPACK_MANUAL_FD_TEST")
	if role == "" {
		return
	}
	if role == "runtime" {
		info, err := os.NewFile(3, "inherited-lease").Stat()
		if err != nil || !info.Mode().IsRegular() {
			t.Fatal("Runtime did not inherit ownership file", err)
		}
		expected, err := os.Stat(os.Getenv("JPACK_EXPECTED_OWNERSHIP"))
		if err != nil || !os.SameFile(info, expected) {
			t.Fatal("worker inherited a different ownership file", err)
		}
		descriptors, err := os.ReadDir("/dev/fd")
		executorMust(t, err)
		for _, entry := range descriptors {
			fd, err := strconv.Atoi(entry.Name())
			if err != nil || fd < 3 {
				continue
			}
			if address, err := syscall.Getsockname(fd); err == nil {
				if _, ok := address.(*syscall.SockaddrUnix); ok {
					t.Fatalf("Runtime inherited authority socket %d", fd)
				}
			}
		}
		return
	}
	var fixture struct {
		Binding     ManualAdmissionBinding
		Root, State string
		Identity    ExecutorLeaseIdentity
	}
	if json.Unmarshal([]byte(os.Getenv("JPACK_MANUAL_FD_FIXTURE")), &fixture) != nil {
		t.Fatal("fixture")
	}
	authority, err := OpenInheritedManualAuthority(fixture.Binding)
	executorMust(t, err)
	defer authority.Close()
	// The supplied original fd3 is closed; Runtime will later reuse that child
	// slot solely for the independently opened ownership lease.
	if _, err = syscall.Getsockname(3); err != syscall.EBADF {
		t.Fatal("inherited original remained open", err)
	}
	lease, err := AcquireExecutorLease(fixture.Root, fixture.State, fixture.Identity.Workspace, fixture.Identity)
	executorMust(t, err)
	defer lease.Close()
	_, err = authority.Activate(context.Background(), ManualActivateRequest{fixture.Binding.Challenge, ManualSigning{Mode: "off"}})
	executorMust(t, err)
	cmd := exec.Command(os.Args[0], "-test.run=^TestManualInheritedAuthorityDescriptorHelper$")
	cmd.Env = []string{"JPACK_MANUAL_FD_TEST=runtime", "JPACK_EXPECTED_OWNERSHIP=" + fixture.Root + "/" + fixture.Identity.Workspace + ".lease"}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_, err = lease.Start(cmd)
	executorMust(t, err)
	if err = cmd.Wait(); err != nil {
		t.Fatal("actual worker descriptor check", err, output.String())
	}
}
func TestManualInheritedAuthoritySocketClosesBeforeRuntimeExec(t *testing.T) {
	f := manualFixture(t)
	lease := executorFixture(t)
	oldLeaf := lease.leaf()
	lease.identity.Workspace = f.Synthetic.Boot.MachineWorkspace
	executorMust(t, os.Rename(oldLeaf, lease.leaf()))
	// Go fork uses ForkLock's writer lock; exclude every concurrent fork until
	// both raw sockets have CLOEXEC. Only ExtraFiles may deliberately inherit.
	syscall.ForkLock.RLock()
	sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(sockets[0])
		syscall.CloseOnExec(sockets[1])
	}
	syscall.ForkLock.RUnlock()
	executorMust(t, err)
	parent := os.NewFile(uintptr(sockets[0]), "authority-parent")
	child := os.NewFile(uintptr(sockets[1]), "authority-child")
	defer parent.Close()
	defer child.Close()
	conn, err := net.FileConn(parent)
	executorMust(t, err)
	defer conn.Close()
	fixture := struct {
		Binding     ManualAdmissionBinding
		Root, State string
		Identity    ExecutorLeaseIdentity
	}{f.Synthetic.Boot, lease.root, lease.state, lease.identity}
	raw, _ := json.Marshal(fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualInheritedAuthorityDescriptorHelper$")
	cmd.ExtraFiles = []*os.File{child}
	cmd.Env = append(os.Environ(), "JPACK_MANUAL_FD_TEST=owner", "JPACK_MANUAL_FD_FIXTURE="+string(raw))
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	executorMust(t, cmd.Start())
	child.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal(err, output.String())
	}
	var request manualRequestEnvelope
	executorMust(t, manualDecode(line, &request))
	if request.Method != "activate" || request.Sequence != strconv.Itoa(1) {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("wrong activation")
	}
	var body ManualActivateRequest
	executorMust(t, manualDecode(request.Body, &body))
	requestDigest, _ := manualRequestDigest("activate", body)
	reply := manualReplyEnvelope{1, "1", "activate", "ok", encode(ManualActivateResult{"committed", "1", requestDigest})}
	_, err = fmt.Fprintln(conn, string(encode(reply)))
	executorMust(t, err)
	if err = cmd.Wait(); err != nil {
		t.Fatal("descriptor fixture", err, output.String())
	}
}
