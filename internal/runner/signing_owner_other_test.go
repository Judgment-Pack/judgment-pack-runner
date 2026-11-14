//go:build !linux && !darwin && !freebsd

package runner

import "os"

// fileOwner reads no owner where files carry no unix ownership.
func fileOwner(os.FileInfo) (uid uint32, links uint64, ok bool) {
	return 0, 0, false
}
