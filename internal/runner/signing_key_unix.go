//go:build linux || darwin || freebsd

package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// checkSigningKey holds the signing key Runner hands each attempt's Runtime to
// the placement the Runtime's guide states, and to being outside Runner's state
// directory, which holds every attempt's directory. Runner never opens the key:
// the Runtime reads it, checks it again as it opens it, and signs nothing with
// a key it refuses. The path must be absolute and clean, with no symbolic link
// anywhere in it; no directory on the way to it may be the state directory,
// compared by device and inode; and the key must be one regular file with one
// name, owned by the user Runner runs as, and neither readable nor writable by
// its group or by others. A refusal never names the path or anything of the
// key.
func checkSigningKey(path, stateDir string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("its path must be absolute and clean")
	}
	state, err := os.Stat(stateDir)
	if err != nil {
		return errors.New("Runner's state directory could not be read")
	}
	walked := "/"
	parts := strings.Split(path[1:], "/")
	for _, part := range parts[:len(parts)-1] {
		walked = filepath.Join(walked, part)
		info, err := os.Lstat(walked)
		switch {
		case err != nil:
			return errors.New("a directory on its path could not be read")
		case info.Mode()&os.ModeSymlink != 0:
			return errors.New("no symbolic link may be anywhere in its path: name it by its real path")
		case !info.IsDir():
			return errors.New("its path passes through something that is not a directory")
		case os.SameFile(info, state):
			return errors.New("it is inside Runner's state directory, which holds every attempt's directory")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("there is no file at its path")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return errors.New("no symbolic link may be anywhere in its path: name it by its real path")
	case !info.Mode().IsRegular() || !ok:
		return errors.New("it must be a regular file")
	case st.Nlink != 1:
		return errors.New("it must have one name, with no hard link elsewhere")
	case st.Uid != uint32(os.Getuid()):
		return errors.New("it must be owned by the user Runner runs as")
	case info.Mode().Perm()&0o066 != 0:
		return errors.New("neither its group nor others may read or write it")
	}
	return nil
}
