package runner

import (
	"errors"
	"os"
	"os/exec"
	"sync"
)

// ExecutorLeaseIdentity is an existing host-provisioned workspace lease's
// identity. The trusted host must retain it independently of portable Runner
// state. Looking up these values in a restored store or caller-selected file
// does not establish authority. There is deliberately no enrollment or reset
// operation here.
type ExecutorLeaseIdentity struct {
	Workspace             string
	RootDevice, RootInode uint64
	FileDevice, FileInode uint64
}

// ExecutorLease is an inactive ownership primitive, not permission to execute
// work. It holds an exclusive lock on an independently pinned external file.
// Close releases this process's descriptors; an inheriting child keeps its
// copy, and therefore the lock, until that copy closes. Do not copy this value.
type ExecutorLease struct {
	mu                  sync.Mutex
	rootPath, statePath string
	identity            ExecutorLeaseIdentity
	root, state         *os.Root
	file                *os.File
}

// AcquireExecutorLease opens, but never creates or repairs, root/<workspace>.lease.
// Both root and stateDir must already exist as private real-path directories,
// and neither may contain the other. workspace is 1–128 ASCII letters, digits,
// underscores or hyphens. expected must come from trusted host provisioning,
// independently of the portable store and the request that would execute work.
// Supported hosts require local Linux, macOS or FreeBSD flock semantics.
func AcquireExecutorLease(root, stateDir, workspace string, expected ExecutorLeaseIdentity) (*ExecutorLease, error) {
	return acquireExecutorLease(root, stateDir, workspace, expected)
}

// Start appends the lease to cmd.ExtraFiles while starting cmd, returning its
// child descriptor number (3 + the original number of ExtraFiles). It preserves
// the caller's ExtraFiles slice. Like exec.Cmd.Start, cmd must not be used
// concurrently. Start and Close on this lease are synchronized through Start's
// inheritance boundary. A failed Start leaves the parent's lease held.
//
// The child must preserve this descriptor throughout the protected work and
// must never explicitly unlock it. Wrappers must pass it on to the actual
// worker. That host/Runtime integration is not implemented by this primitive.
func (l *ExecutorLease) Start(cmd *exec.Cmd) (int, error) {
	if l == nil {
		return -1, errors.New("executor lease is closed")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return -1, errors.New("executor lease is closed")
	}
	if cmd == nil || cmd.Process != nil {
		return -1, errors.New("executor lease requires an unstarted command")
	}
	if err := verifyExecutorLease(l); err != nil {
		return -1, err
	}
	original := cmd.ExtraFiles
	files := make([]*os.File, len(original)+1)
	copy(files, original)
	files[len(original)] = l.file
	cmd.ExtraFiles = files
	defer func() { cmd.ExtraFiles = original }()
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	return 3 + len(original), nil
}

// Close closes only this process's descriptor copies. It deliberately never
// calls LOCK_UN: inherited descriptors share the same locked open description.
// Close is idempotent and may run concurrently with Start.
func (l *ExecutorLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := errors.Join(l.file.Close(), l.root.Close(), l.state.Close())
	l.file, l.root, l.state = nil, nil, nil
	return err
}
