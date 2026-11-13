//go:build linux || darwin || freebsd

package runner

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// The files service mode reads at start (docs/design/service-mode.md): its
// configuration, which holds the owner bearer or the identity Runner holds
// callers to, and an identity's key set. Each is read once, through one
// handle, and a refusal never names its path or anything it holds.

// MaxServiceConfig bounds a service configuration, as the boot line is
// bounded.
const MaxServiceConfig = 128 << 10

// ReadServiceConfig reads service mode's configuration file, held to the
// signing key's rules (checkSigningKey): an absolute, clean path with no
// symbolic link anywhere in it, through directories alone, to one regular
// file with one name, owned by the user Runner runs as, and neither readable
// nor writable by its group or by others. It is opened without following a
// link and without blocking, must be the file checked at the path, by device
// and inode, and is read up to MaxServiceConfig; if its attributes change
// before the end of the read, it is refused. That it is not inside Runner's
// state directory, which the configuration names, is checked once it has been
// read (ServiceConfigOutsideState).
func ReadServiceConfig(path string) ([]byte, error) {
	seen, err := placedFile(path, nil)
	if err != nil {
		return nil, err
	}
	if err = keyFilePrivate(seen); err != nil {
		return nil, err
	}
	return readPlaced(path, seen, keyFilePrivate, MaxServiceConfig)
}

// ServiceConfigOutsideState refuses a service configuration inside Runner's
// state directory, as the signing key is refused there. A state directory
// that does not exist yet holds nothing.
func ServiceConfigOutsideState(path, stateDir string) error {
	state, err := os.Stat(stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return errors.New("Runner's state directory could not be read")
	}
	_, err = placedFile(path, state)
	return err
}

// readKeySet reads an identity's key set file: placed as the configuration
// is (placedFile), and outside Runner's state directory when that exists; one
// regular file, owned by the user Runner runs as or by root, that neither its
// group nor others may write. The keys are public, so others may read it. It
// is read once, through one handle, up to maxKeySetBytes, as
// ReadServiceConfig reads.
func readKeySet(path, stateDir string) ([]byte, error) {
	var state os.FileInfo
	switch info, err := os.Stat(stateDir); {
	case err == nil:
		state = info
	case !errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("Runner's state directory could not be read")
	}
	seen, err := placedFile(path, state)
	if err != nil {
		return nil, err
	}
	if err = keySetFileHeld(seen); err != nil {
		return nil, err
	}
	return readPlaced(path, seen, keySetFileHeld, maxKeySetBytes)
}

// keySetFileHeld holds a key set file's own attributes to its placement: a
// regular file, its owner Runner's user or root, written by nobody else.
func keySetFileHeld(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular() || !ok:
		return errors.New("it must be a regular file")
	case st.Uid != uint32(os.Getuid()) && st.Uid != 0:
		return errors.New("it must be owned by the user Runner runs as, or by root")
	case info.Mode().Perm()&0o022 != 0:
		return errors.New("neither its group nor others may write it")
	}
	return nil
}

// readPlaced reads the file seen at path once: opened without following a
// link and without blocking, the file seen by device and inode, its handle's
// attributes held by hold, then read up to limit bytes; its attributes are
// read again after, and a file that changed between is refused.
func readPlaced(path string, seen os.FileInfo, hold func(os.FileInfo) error, limit int) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("it could not be opened")
	}
	defer file.Close()
	checked, err := file.Stat()
	switch {
	case err != nil:
		return nil, errors.New("it could not be opened")
	case !os.SameFile(seen, checked):
		return nil, errors.New("it was replaced while it was opened")
	}
	if err = hold(checked); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, errors.New("it could not be read")
	}
	if after, err := file.Stat(); err != nil || !unchangedFile(checked, after) {
		return nil, errChangedWhileRead
	}
	if len(data) > limit {
		return nil, fmt.Errorf("it is longer than %d bytes", limit)
	}
	return data, nil
}
