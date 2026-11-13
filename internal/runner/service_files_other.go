//go:build !linux && !darwin && !freebsd

package runner

import "errors"

// MaxServiceConfig bounds a service configuration, as the boot line is
// bounded.
const MaxServiceConfig = 128 << 10

// errNoServiceMode is service mode's refusal where a file's privacy cannot be
// read from unix ownership and modes, as the signing key is refused there.
var errNoServiceMode = errors.New("service mode is not available on this platform: its configuration file's privacy cannot be checked here")

// ReadServiceConfig refuses every configuration on this platform.
func ReadServiceConfig(path string) ([]byte, error) { return nil, errNoServiceMode }

// ServiceConfigOutsideState refuses every configuration on this platform.
func ServiceConfigOutsideState(path, stateDir string) error { return errNoServiceMode }

// readKeySet refuses every key set on this platform.
func readKeySet(path, stateDir string) ([]byte, error) { return nil, errNoServiceMode }
