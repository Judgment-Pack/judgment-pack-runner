//go:build !linux && !darwin && !freebsd

package runner

import (
	"crypto/ed25519"
	"errors"
)

// signingPublicKey refuses every key where a file's privacy cannot be read
// from unix ownership and modes, as the Runtime refuses to sign there.
func signingPublicKey(path, stateDir string) (ed25519.PublicKey, error) {
	return nil, errors.New("a signing key's privacy cannot be checked on this platform, and the Runtime signs nothing here")
}
