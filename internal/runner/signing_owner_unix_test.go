//go:build linux || darwin || freebsd

package runner

import (
	"os"
	"syscall"
)

// fileOwner gives a file's owning user and its number of names, read from
// its unix status.
func fileOwner(info os.FileInfo) (uid uint32, links uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, uint64(st.Nlink), true
}
