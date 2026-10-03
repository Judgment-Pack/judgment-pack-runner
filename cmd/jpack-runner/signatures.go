package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// errSignatureInvalid is a record signature check that failed.
var errSignatureInvalid = errors.New("signature-invalid")

// errUnsigned is --require-signed's refusal of a run whose audit record no
// valid signature covers.
var errUnsigned = errors.New("unsigned")

// publicKeyOf reads --public-key, held to the key check before anything signed
// with it is read. --require-signed needs a key to check a signature under.
func publicKeyOf(text string, requireSigned bool) (ed25519.PublicKey, error) {
	if text == "" {
		if requireSigned {
			return nil, errors.New("--require-signed needs --public-key, the key a signature is checked under")
		}
		return nil, nil
	}
	key, err := runner.ParsePublicKey(text)
	if err != nil {
		return nil, fmt.Errorf("--public-key is refused: %w", err)
	}
	return key, nil
}

// checkSignature checks the run's record signature under key. It answers nil
// when there is nothing to say: no key, and an export that carries no
// signature sidecar.
func checkSignature(v runner.VerifiedRun, key ed25519.PublicKey) *runner.SignatureReport {
	if key == nil && len(v.Signatures()) == 0 {
		return nil
	}
	report := v.CheckSignature(key)
	return &report
}

// signatureRefusal is the error a failed signature check, or --require-signed,
// answers, with the status its report carries; nil when there is none.
func signatureRefusal(s *runner.SignatureReport, requireSigned bool) (string, error) {
	switch {
	case s == nil:
		return "", nil
	case s.Status == runner.SignatureInvalid:
		f := s.Findings[0]
		more := ""
		if s.FindingsTotal > 1 {
			more = fmt.Sprintf(" (and %d more)", s.FindingsTotal-1)
		}
		return "signature-invalid", fmt.Errorf("%w: %s at line %d: %s%s", errSignatureInvalid, f.Name, f.Line, f.Detail, more)
	case requireSigned && s.Status != runner.SignatureSigned:
		return "unsigned", fmt.Errorf("%w: no valid signature under key %s covers the run's audit record, and --require-signed refuses it", errUnsigned, s.KeyID)
	}
	return "", nil
}

// signatureSentence is what the line on standard error says of the run's
// record signature, after what it says of its chain entry.
func signatureSentence(s *runner.SignatureReport, version int) string {
	switch {
	case s == nil:
		return ""
	case s.Status == runner.SignatureNotChecked:
		return " The export carries the signature sidecar of the run's audit record, which was not checked: --public-key names the key to check it under."
	case s.Status == runner.SignatureSigned:
		return fmt.Sprintf(" The record's signature verifies under key %s: whoever held that key signed these exact bytes as the attempt's record. That establishes nothing against the operator, who holds the key, and nothing once the key is copied.", s.KeyID)
	case version < 5:
		return fmt.Sprintf(" A version-%d export carries no record signatures, so as far as it shows no signature under key %s covers the record: ask for version 5 to check one. An unsigned record is refused only with --require-signed.", version, s.KeyID)
	}
	return fmt.Sprintf(" No valid signature under key %s covers the record: its signature sidecar holds none. That is not a failure by itself, since a signature the Runtime could not write leaves its decision recorded; --require-signed refuses it.", s.KeyID)
}
