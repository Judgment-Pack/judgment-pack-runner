package runner

import (
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
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
	return s.startRuntimeAs(cmd, manualUtilityInvocation, nil)
}

type manualInvocationKind uint8

const (
	manualUtilityInvocation manualInvocationKind = iota
	manualOperationalInvocation
)

func manualUtilityCommand(cmd *exec.Cmd) bool {
	if cmd == nil || len(cmd.Args) < 2 || !slices.Equal(cmd.Env, []string{"LANG=C", "LC_ALL=C"}) || cmd.Stdin != nil || len(cmd.ExtraFiles) != 0 {
		return false
	}
	args := cmd.Args[1:]
	if slices.Equal(args, []string{"version", "--format", "json"}) {
		return true
	}
	if len(args) < 2 {
		return false
	}
	switch args[0] + " " + args[1] {
	case "spec validate":
		return slices.Equal(args, []string{"spec", "validate", "pack.json", "--format", "json"})
	case "packs lock":
		return slices.Equal(args, []string{"packs", "lock", "--config", "jpack.json", "--format", "json"})
	case "packs test":
		return slices.Equal(args, []string{"packs", "test", "--id", "target", "--config", "jpack.json", "--format", "json"})
	case "packs list":
		return slices.Equal(args, []string{"packs", "list", "--config", "jpack.json", "--format", "json"})
	case "audit verify":
		return len(args) == 6 && args[2] == "--trail" && args[3] != "" && args[4] == "--format" && args[5] == "json"
	case "experimental evaluate":
		base := []string{"experimental", "evaluate", "--pack-id", "target", "--config", "jpack.json", "--facts", "facts.json", "--format", "json"}
		if slices.Equal(args, append(base, "--rehearsal")) {
			return true
		}
		return slices.Equal(args, append(base, "--evidence", "evidence.json", "--rehearsal"))
	}
	return false
}

// All Service paths arrive here. Enabled operational invocation requires the
// original session's verified, durable, one-use launch; alternate evaluate and
// private workflow paths cannot inherit a utility classification accidentally.
func (s *Service) startRuntimeAs(cmd *exec.Cmd, kind manualInvocationKind, launch *manualLaunch) error {
	if s.cfg.ExecutorAdmission != nil {
		if kind == manualOperationalInvocation {
			if launch == nil || launch.client != s.authority || launch.material == nil {
				return errManualProtocol
			}
			return launch.start(cmd, s.executor)
		}
		if kind != manualUtilityInvocation || launch != nil || !manualUtilityCommand(cmd) {
			return errManualProtocol
		}
	}
	if s.cfg.ExecutorOwnership != nil || s.executor != nil {
		_, err := s.executor.Start(cmd)
		return err
	}
	return cmd.Start()
}
