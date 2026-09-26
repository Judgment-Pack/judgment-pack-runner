//go:build !linux && !darwin && !freebsd

package runner

import (
	"errors"
	"os"
)

func instanceLock(path string) (*os.File, error) {
	return nil, errors.New("the local Jobs pilot currently requires Linux, macOS or FreeBSD")
}

func checkOwner(st os.FileInfo) error { return errors.New("unsupported host") }
