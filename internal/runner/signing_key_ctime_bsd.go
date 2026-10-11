//go:build darwin || freebsd

package runner

import "syscall"

// changeTime is a file's change time, as its attributes give it.
func changeTime(st *syscall.Stat_t) syscall.Timespec { return st.Ctimespec }
