//go:build !linux && !darwin && !freebsd

package runner

import "errors"

func acquireExecutorLease(root, stateDir, workspace string, expected ExecutorLeaseIdentity) (*ExecutorLease, error) {
	return nil, errors.New("executor ownership requires Linux, macOS or FreeBSD local filesystem locking")
}

func verifyExecutorLease(l *ExecutorLease) error {
	return errors.New("executor ownership is unsupported on this host")
}
