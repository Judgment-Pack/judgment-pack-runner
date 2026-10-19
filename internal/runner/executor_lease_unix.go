//go:build linux || darwin || freebsd

package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var errExecutorLeaseUnsafe = errors.New("executor lease identity, placement or private custody is invalid")

func executorObjectIdentity(info os.FileInfo, device, inode uint64) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == device && uint64(st.Ino) == inode
}

func executorPrivate(info os.FileInfo, directory bool) bool {
	if info.Mode().Perm()&0077 != 0 || checkOwner(info) != nil {
		return false
	}
	if directory {
		return info.IsDir()
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && ok && st.Nlink == 1
}

// Walk from an opened / one component at a time, comparing lstat with the
// opened directory. os.Root pins each step; no ancestor symlink is accepted.
// Ancestors may include shared sticky temporary directories. Only the final
// directory must be private. Expected inode pins, rather than those ancestors,
// establish which external lease is eligible.
func openExecutorDirectory(path string) (*os.Root, []os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(path) > 4096 {
		return nil, nil, errExecutorLeaseUnsafe
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) > 128 {
		return nil, nil, errExecutorLeaseUnsafe
	}
	held, err := os.OpenRoot("/")
	if err != nil {
		return nil, nil, errExecutorLeaseUnsafe
	}
	var chain []os.FileInfo
	for _, part := range parts {
		looked, err := held.Lstat(part)
		if err != nil || !looked.IsDir() {
			held.Close()
			return nil, nil, errExecutorLeaseUnsafe
		}
		next, err := held.OpenRoot(part)
		if err != nil {
			held.Close()
			return nil, nil, errExecutorLeaseUnsafe
		}
		opened, err := next.Stat(".")
		held.Close()
		if err != nil || !os.SameFile(looked, opened) {
			next.Close()
			return nil, nil, errExecutorLeaseUnsafe
		}
		held = next
		chain = append(chain, opened)
	}
	if !executorPrivate(chain[len(chain)-1], true) {
		held.Close()
		return nil, nil, errExecutorLeaseUnsafe
	}
	return held, chain, nil
}

func executorDirectoriesOverlap(root, state []os.FileInfo) bool {
	for _, info := range root {
		if os.SameFile(info, state[len(state)-1]) {
			return true
		}
	}
	for _, info := range state {
		if os.SameFile(info, root[len(root)-1]) {
			return true
		}
	}
	return false
}

func openExecutorLeaseFile(root *os.Root, expected ExecutorLeaseIdentity) (*os.File, error) {
	name := expected.Workspace + ".lease"
	looked, err := root.Lstat(name)
	if err != nil || !executorPrivate(looked, false) || !executorObjectIdentity(looked, expected.FileDevice, expected.FileInode) {
		return nil, errExecutorLeaseUnsafe
	}
	// A substituted pipe is refused without blocking. No creation, truncation,
	// content-derived authority or explicit unlock occurs on this path.
	file, err := root.OpenFile(name, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errExecutorLeaseUnsafe
	}
	opened, err := file.Stat()
	if err != nil || !executorPrivate(opened, false) || !os.SameFile(looked, opened) || !executorObjectIdentity(opened, expected.FileDevice, expected.FileInode) {
		file.Close()
		return nil, errExecutorLeaseUnsafe
	}
	return file, nil
}

func acquireExecutorLease(rootPath, statePath, workspace string, expected ExecutorLeaseIdentity) (*ExecutorLease, error) {
	if !executorWorkspaceID(workspace) || workspace != expected.Workspace || expected.RootInode == 0 || expected.FileInode == 0 {
		return nil, errExecutorLeaseUnsafe
	}
	root, rootChain, err := openExecutorDirectory(rootPath)
	if err != nil {
		return nil, err
	}
	state, stateChain, err := openExecutorDirectory(statePath)
	if err != nil {
		root.Close()
		return nil, err
	}
	if executorDirectoriesOverlap(rootChain, stateChain) || !executorObjectIdentity(rootChain[len(rootChain)-1], expected.RootDevice, expected.RootInode) {
		root.Close()
		state.Close()
		return nil, errExecutorLeaseUnsafe
	}
	file, err := openExecutorLeaseFile(root, expected)
	if err != nil {
		root.Close()
		state.Close()
		return nil, err
	}
	l := &ExecutorLease{rootPath: rootPath, statePath: statePath, identity: expected, root: root, state: state, file: file}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		l.Close()
		return nil, errors.New("executor lease is already owned or locking is unavailable")
	}
	if err := verifyExecutorLease(l); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func verifyExecutorLease(l *ExecutorLease) error {
	heldRoot, err := l.root.Stat(".")
	if err != nil || !executorPrivate(heldRoot, true) || !executorObjectIdentity(heldRoot, l.identity.RootDevice, l.identity.RootInode) {
		return errExecutorLeaseUnsafe
	}
	heldState, err := l.state.Stat(".")
	if err != nil || !executorPrivate(heldState, true) {
		return errExecutorLeaseUnsafe
	}
	heldFile, err := l.file.Stat()
	if err != nil || !executorPrivate(heldFile, false) || !executorObjectIdentity(heldFile, l.identity.FileDevice, l.identity.FileInode) {
		return errExecutorLeaseUnsafe
	}
	root, rootChain, err := openExecutorDirectory(l.rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	state, stateChain, err := openExecutorDirectory(l.statePath)
	if err != nil {
		return err
	}
	defer state.Close()
	if executorDirectoriesOverlap(rootChain, stateChain) || !os.SameFile(heldRoot, rootChain[len(rootChain)-1]) || !os.SameFile(heldState, stateChain[len(stateChain)-1]) {
		return errExecutorLeaseUnsafe
	}
	current, err := openExecutorLeaseFile(root, l.identity)
	if err != nil {
		return err
	}
	return current.Close()
}
