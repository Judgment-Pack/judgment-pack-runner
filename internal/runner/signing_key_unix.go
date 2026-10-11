//go:build linux || darwin || freebsd

package runner

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// checkSigningKey holds the signing key Runner hands each attempt's Runtime to
// the placement the Runtime's guide states, and to being outside Runner's state
// directory, which holds every attempt's directory. The path must be absolute
// and clean, with no symbolic link anywhere in it; no directory on the way to
// it may be the state directory, compared by device and inode; and the key
// must be one regular file with one name, owned by the user Runner runs as,
// and neither readable nor writable by its group or by others. It answers what
// it saw at the path. A refusal never names the path or anything of the key.
func checkSigningKey(path, stateDir string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("its path must be absolute and clean")
	}
	state, err := os.Stat(stateDir)
	if err != nil {
		return nil, errors.New("Runner's state directory could not be read")
	}
	walked := "/"
	parts := strings.Split(path[1:], "/")
	for _, part := range parts[:len(parts)-1] {
		walked = filepath.Join(walked, part)
		info, err := os.Lstat(walked)
		switch {
		case err != nil:
			return nil, errors.New("a directory on its path could not be read")
		case info.Mode()&os.ModeSymlink != 0:
			return nil, errors.New("no symbolic link may be anywhere in its path: name it by its real path")
		case !info.IsDir():
			return nil, errors.New("its path passes through something that is not a directory")
		case os.SameFile(info, state):
			return nil, errors.New("it is inside Runner's state directory, which holds every attempt's directory")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("there is no file at its path")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("no symbolic link may be anywhere in its path: name it by its real path")
	}
	return info, keyFilePrivate(info)
}

// keyFilePrivate holds a key file's own attributes to the placement: one
// regular file with one name, its owner Runner's user, and private to them.
func keyFilePrivate(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
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

// signingPublicKey holds the signing key to its placement (checkSigningKey),
// then reads it, once, and answers its public key. The file opened must be
// the one checked, by device and inode, and still hold to the placement; it is
// opened without following a link and without blocking, and read up to the
// bound the Runtime reads a seed to (seedPublicKey). Nothing read is kept,
// logged or put in an error: a refusal never names the path or any byte of the
// key.
func signingPublicKey(path, stateDir string) (ed25519.PublicKey, error) {
	seen, err := checkSigningKey(path, stateDir)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("it could not be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	switch {
	case err != nil:
		return nil, errors.New("it could not be opened")
	case !os.SameFile(seen, info):
		return nil, errors.New("it was replaced while it was opened")
	}
	if err = keyFilePrivate(info); err != nil {
		return nil, err
	}
	return seedPublicKey(file)
}
