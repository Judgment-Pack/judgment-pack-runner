//go:build !linux && !darwin && !freebsd

package runner

import "errors"

// checkSigningKey refuses every key where a file's privacy cannot be read from
// unix ownership and modes, as the Runtime refuses to sign there.
func checkSigningKey(path, stateDir string) error {
	return errors.New("a signing key's privacy cannot be checked on this platform, and the Runtime signs nothing here")
}
