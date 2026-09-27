//go:build !windows

package runner

import (
	"os"
	"syscall"
)

func openAutomaticFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
