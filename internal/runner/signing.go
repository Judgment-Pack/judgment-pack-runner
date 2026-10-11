package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
)

// The expected key, and each run's signing (runner #54).
//
// Runner reads the signing key once, at start, and fixes its public key as
// the expected key for as long as it runs. Each operational evaluation's
// Runtime is given the key's path, as JPACK_SIGNING_KEY, and the expected key,
// as JPACK_SIGNING_PUBLIC_KEY. A Runtime that knows the variable (0.30.0 and
// later) refuses to sign with a seed whose public key is another, so a key
// replaced at its path signs nothing: the record is left unsigned and the
// decision recorded. An earlier Runtime ignores the variable and signs with
// whatever key is at the path. Either way, Runner then holds the record to the
// expected key: a completed run carries its signing, signed under the expected
// key or failed with its reason. The decision is never withheld for it. Where
// no line signs the record, Runner reads the key at the path again, only to
// say why; the expected key stays the one read at start.

// maxSeedFile is the most of a seed file the Runtime reads: a longer file is
// not a seed.
const maxSeedFile = 4096

// errNotASeed is a key file that is not one Ed25519 seed in the Runtime's
// form. It says nothing of what the file holds.
var errNotASeed = errors.New("it is not an Ed25519 seed: 64 hexadecimal characters, with nothing else but surrounding whitespace")

// seedPublicKey reads a seed file's contents, as the Runtime reads them, and
// answers the seed's public key: 64 hexadecimal characters of a 32-byte seed,
// in either case, with nothing else but surrounding whitespace, in at most
// maxSeedFile bytes. What it read is cleared before it returns, and no error
// carries any of it.
func seedPublicKey(r io.Reader) (ed25519.PublicKey, error) {
	data := make([]byte, maxSeedFile+1)
	defer clear(data)
	n, err := io.ReadFull(r, data)
	switch {
	case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
		return nil, errors.New("it could not be read")
	case n > maxSeedFile:
		return nil, errNotASeed
	}
	text := bytes.TrimSpace(data[:n])
	if len(text) != hex.EncodedLen(ed25519.SeedSize) {
		return nil, errNotASeed
	}
	seed := make([]byte, ed25519.SeedSize)
	defer clear(seed)
	if _, err = hex.Decode(seed, text); err != nil {
		return nil, errNotASeed
	}
	private := ed25519.NewKeyFromSeed(seed)
	defer clear(private)
	return bytes.Clone(private[ed25519.SeedSize:]), nil
}

// RunSigning is what Runner found of a completed run's record signature, for
// an installation with a signing key: signed, under the expected key, which it
// names; or failed, with the reason. Runner records it with the run, beside
// the sidecar, which it does not change. A run recorded without a key, one
// that did not complete, and one recorded before Runner kept it, have none.
type RunSigning struct {
	Status    string `json:"status"`
	PublicKey string `json:"publicKey,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// The statuses of a run's signing.
const (
	// SigningSigned is a record that a valid signature under the expected key
	// covers, over the record's bytes the run keeps, with no check failed:
	// what `verify-run --public-key` reports signed under that key.
	SigningSigned = "signed"
	// SigningFailed is any other record of an installation with a key.
	SigningFailed = "failed"
)

// The reasons a run's signing failed. They name no path and no byte of any
// key.
const (
	// signingSidecarUnread is a sidecar that is there and could not be read
	// as Runner reads one: never taken for one that is not there.
	signingSidecarUnread = "The record's signature sidecar could not be read."
	// signingOtherKey is a sidecar with a line under a key that is not the
	// expected one, as an earlier Runtime writes after a key is replaced at
	// its path. The line is not checked under that key, which Runner does not
	// hold: it names it.
	signingOtherKey = "The record's signature sidecar names a key that is not the expected one."
	// signingNotVerified is a sidecar whose lines all name the expected key,
	// and that does not show the record signed under it.
	signingNotVerified = "The record's signature sidecar does not verify under the expected key."
	// signingRuntimeSignedNothing is a record no line signs, when the key at
	// the path, read after the evaluation, is not the expected key: a Runtime
	// that knows the expected key refuses to sign with it.
	signingRuntimeSignedNothing = "The Runtime signed nothing: the key at the signing key's path is not the expected key."
	// signingKeyUnread is a record no line signs, when the key at the path,
	// read after the evaluation, could not be read as a key.
	signingKeyUnread = "The Runtime signed nothing, and the key at the signing key's path could not be read as a key."
	// signingNoSignature is a record no line signs, when the key at the path
	// is the expected key: a Runtime that cannot sign, or that refused the
	// key for another reason, writes none.
	signingNoSignature = "The record carries no signature."
)

// signingOf holds a completed run's record, the bytes the run keeps, to the
// expected key, by the check `verify-run --public-key` makes: its signature
// sidecar, as kept, must hold a valid signature under that key that covers
// the record, with no check failed. A sidecar that could not be read is never
// read as absent. Where no line signs the record, the key at the path is read
// again, only to say why.
func (s *Service) signingOf(record, sidecar []byte, unread bool) *RunSigning {
	failed := func(reason string) *RunSigning { return &RunSigning{Status: SigningFailed, Reason: reason} }
	if unread {
		return failed(signingSidecarUnread)
	}
	check := checkSignatures([][]byte{record}, sidecar, s.expectedKey)
	if signatureStatus(check) == SignatureSigned {
		return &RunSigning{Status: SigningSigned, PublicKey: hex.EncodeToString(s.expectedKey)}
	}
	if check.readable > 0 {
		expected := KeyID(s.expectedKey)
		for _, raw := range sidecarLines(sidecar) {
			if line, ok := readSidecarLine(raw); ok && line.keyID != expected {
				return failed(signingOtherKey)
			}
		}
		return failed(signingNotVerified)
	}
	switch key, err := signingPublicKey(s.cfg.SigningKey, s.cfg.Dir); {
	case err != nil:
		return failed(signingKeyUnread)
	case !key.Equal(s.expectedKey):
		return failed(signingRuntimeSignedNothing)
	}
	return failed(signingNoSignature)
}
