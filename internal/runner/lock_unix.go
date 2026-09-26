//go:build linux || darwin || freebsd

package runner

import (
	"errors"
	"os"
	"syscall"
)

func instanceLock(path string) (*os.File, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another runner owns this workspace")
	}
	return f, nil
}

func checkOwner(st os.FileInfo) error {
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("runner state must belong to the current operating-system user")
	}
	return nil
}
