//go:build linux || darwin || freebsd

package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

func readManualDemand(o ExecutorOwnership) (*ManualExecutionDemand, error) {
	path := manualDemandDirectory(o)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	root, _, err := openExecutorDirectory(path)
	if err != nil {
		return nil, errManualProtocol
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, errManualProtocol
	}
	defer dir.Close()
	entries, err := dir.ReadDir(257)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) == 0 || len(entries) > 256 {
		return nil, errManualProtocol
	}
	var current *ManualExecutionDemand
	for _, entry := range entries {
		if !manualHex(entry.Name()) {
			return nil, errManualProtocol
		}
		looked, err := root.Lstat(entry.Name())
		if err != nil || !executorPrivate(looked, false) {
			return nil, errManualProtocol
		}
		f, err := root.OpenFile(entry.Name(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, errManualProtocol
		}
		opened, statErr := f.Stat()
		raw, readErr := io.ReadAll(io.LimitReader(f, 4097))
		closeErr := f.Close()
		if statErr != nil || readErr != nil || closeErr != nil || len(raw) > 4096 || !executorPrivate(opened, false) || !os.SameFile(looked, opened) {
			return nil, errManualProtocol
		}
		var d ManualExecutionDemand
		if manualDecode(raw, &d) != nil || !d.valid() || d.MachineWorkspace != entry.Name() {
			return nil, errManualProtocol
		}
		if d.MachineWorkspace == o.MachineWorkspace {
			if d.RunnerWorkspace != o.RunnerWorkspace {
				return nil, errManualProtocol
			}
			copy := d
			current = &copy
		}
	}
	return current, nil
}

// adoptManualPreparationLease takes an intentional inherited duplicate. The
// trusted parent retains its original continuously. A fresh open must contend,
// while relocking the inherited open description must succeed. Neither check
// identifies a parent PID or substitutes for that trusted custody contract.
func adoptManualPreparationLease(o ExecutorOwnership, statePath string, file *os.File) (*ExecutorLease, error) {
	if file == nil {
		return nil, errManualProtocol
	}
	syscall.CloseOnExec(int(file.Fd()))
	root, rootChain, err := openExecutorDirectory(o.Root)
	if err != nil {
		file.Close()
		return nil, err
	}
	state, stateChain, err := openExecutorDirectory(statePath)
	if err != nil {
		file.Close()
		root.Close()
		return nil, err
	}
	l := &ExecutorLease{rootPath: o.Root, statePath: statePath, identity: o.identity(), root: root, state: state, file: file}
	fail := func() (*ExecutorLease, error) { l.Close(); return nil, errManualProtocol }
	if executorDirectoriesOverlap(rootChain, stateChain) || !executorObjectIdentity(rootChain[len(rootChain)-1], o.RootDevice, o.RootInode) || verifyExecutorLease(l) != nil {
		return fail()
	}
	probe, err := openExecutorLeaseFile(root, o.identity())
	if err != nil {
		return fail()
	}
	contended := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	probe.Close() // never explicitly unlock a shared open description
	if !errors.Is(contended, syscall.EWOULDBLOCK) && !errors.Is(contended, syscall.EAGAIN) {
		return fail()
	}
	if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return fail()
	}
	return l, nil
}

func manualPreparationInstanceLock(lease *ExecutorLease) (*os.File, error) {
	f, err := lease.state.OpenFile("runner.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !executorPrivate(info, false) || syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, errManualProtocol
	}
	return f, nil
}
func readManualStateFile(lease *ExecutorLease, name string, limit int64) ([]byte, error) {
	f, err := lease.state.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !executorPrivate(info, false) {
		return nil, errManualProtocol
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errManualProtocol
	}
	return raw, nil
}
func writeManualPrepareMarker(lease *ExecutorLease, raw []byte) error {
	f, err := lease.state.OpenFile(manualPrepareMarker, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	dir, err := lease.state.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func verifyManualDatabasePath(lease *ExecutorLease, expected os.FileInfo) error {
	if verifyManualLease(lease) != nil {
		return errManualProtocol
	}
	held, err := lease.state.OpenFile("runs.sqlite", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer held.Close()
	info, err := held.Stat()
	if err != nil || !executorPrivate(info, false) || expected != nil && !os.SameFile(info, expected) {
		return errManualProtocol
	}
	current, err := os.Lstat(filepath.Join(lease.statePath, "runs.sqlite"))
	if err != nil || !executorPrivate(current, false) || !os.SameFile(info, current) {
		return errManualProtocol
	}
	return nil
}

// SQLite needs a filename for its DB/journal/WAL family. Retain an actual
// directory fd and use its OS descriptor path, rather than reusing statePath.
// Refuse hosts without a working descriptor-relative directory path.
func manualDatabaseTarget(lease *ExecutorLease) (string, *os.File, error) {
	dir, err := lease.state.Open(".")
	if err != nil {
		return "", nil, err
	}
	base := "/dev/fd"
	if runtime.GOOS == "linux" {
		base = "/proc/self/fd"
	}
	path := filepath.Join(base, strconv.FormatUint(uint64(dir.Fd()), 10), "runs.sqlite")
	target, err := os.Stat(path)
	held, heldErr := lease.state.Stat("runs.sqlite")
	if err != nil || heldErr != nil || !executorPrivate(target, false) || !os.SameFile(target, held) {
		dir.Close()
		return "", nil, errManualProtocol
	}
	return path, dir, nil
}
