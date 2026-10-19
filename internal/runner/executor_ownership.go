package runner

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// ExecutorOwnership is trusted host configuration, retained independently of
// portable Runner state. MachineWorkspace names the external lease leaf;
// RunnerWorkspace binds it to this Runner store's distinct workspace identity.
// Neither request data nor a restored artifact may supply these pins.
type ExecutorOwnership struct {
	Root, MachineWorkspace, RunnerWorkspace string
	RootDevice, RootInode                   uint64
	FileDevice, FileInode                   uint64
}

// Validate checks the binding's syntax and exact Runner workspace, without
// discovering identities or opening any object. Open validates custody itself.
func (o ExecutorOwnership) Validate(workspace string) error {
	if !filepath.IsAbs(o.Root) || filepath.Clean(o.Root) != o.Root || o.Root == "/" || len(o.Root) > 4096 || strings.Count(o.Root, "/") > 128 || strings.IndexByte(o.Root, 0) >= 0 || !executorWorkspaceID(o.MachineWorkspace) || o.RunnerWorkspace == "" || o.RunnerWorkspace != workspace || o.RootInode == 0 || o.FileInode == 0 {
		return errors.New("invalid executor ownership binding")
	}
	return nil
}

func (o ExecutorOwnership) identity() ExecutorLeaseIdentity {
	return ExecutorLeaseIdentity{Workspace: o.MachineWorkspace, RootDevice: o.RootDevice, RootInode: o.RootInode, FileDevice: o.FileDevice, FileInode: o.FileInode}
}

// startRuntime is every Service Runtime process's inheritance boundary. The
// configured lease pointer is immutable, including after Close: a stopped or
// invalid lease refuses launch rather than falling back to an unowned command.
func (s *Service) startRuntime(cmd *exec.Cmd) error {
	if s.cfg.ExecutorOwnership != nil || s.executor != nil {
		_, err := s.executor.Start(cmd)
		return err
	}
	return cmd.Start()
}
