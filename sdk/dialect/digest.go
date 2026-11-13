package dialect

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/dialect4"
)

// HMACSHA256 is the label of HMAC (RFC 2104) over SHA-256, the one keyed
// digest algorithm KeyedParameterDigest computes: the label the Runtime's
// keyed decision record names (its ADR-0087).
const HMACSHA256 = "hmac-sha256"

// MaxKeyIDLength is the bound on a key identifier's length.
const MaxKeyIDLength = 128

// ParameterText is the parameters the case gave, by name, each the value it
// took (a list with its elements), in the gateway canonical form
// spec/dialect.md §1 names canon. It holds the case's parameters alone: not
// runAt, which is the instant of preparation and is the preparation's
// verifiedAt, and which a case does not give.
func (p *Preparation) ParameterText() []byte {
	text, err := dialect4.Canon(p.parameters)
	if err != nil {
		// Every parameter value is of one of the six types, all in the
		// canon domain: an integer parameter is one within it.
		panic(err)
	}
	return text
}

// ParameterDigest is the SHA-256 of ParameterText, written sha256:<hex>. It
// changes with any parameter's value, and not with how the case was
// written: the order of its members, its whitespace or its escapes. Two
// cases that give their parameters the same values have the same parameter
// digest, whatever else they hold. A list is bound with its elements in
// their order, and a decimal with its digits as written, so "0.75" and
// "0.750", which a rule compares as equal, have different digests. An
// integer is bound in canon's form, by its value: one written -0 is bound
// as 0.
func (p *Preparation) ParameterDigest() string { return digestOf(p.ParameterText()) }

// KeyedDigest is a keyed digest of ParameterText: the algorithm's label, the
// caller's identifier for the key, and the 32 bytes of HMAC-SHA-256.
type KeyedDigest struct {
	Algorithm string
	KeyID     string
	Digest    []byte
}

// ErrKeyedDigest is returned for a key identifier that is not 1 to 128
// characters from A-Z, a-z, 0-9, dot, underscore, hyphen, colon and slash,
// for one that holds a plain digest of the preparation (see
// KeyedParameterDigest), or for an empty key. The error repeats neither the
// identifier nor the key.
var ErrKeyedDigest = errors.New("dialect: a keyed parameter digest needs a key identifier of 1 to 128 characters from A-Z, a-z, 0-9, dot, underscore, hyphen, colon and slash that holds no plain digest of the preparation, and a key that is not empty")

// KeyedParameterDigest is HMAC-SHA-256 of ParameterText under key, which
// the caller holds and which this package keeps nowhere, labelled with
// keyID, the caller's identifier for the key, in the form of the Runtime's
// keyed decision record: algorithm hmac-sha256 and a key identifier of the
// same grammar. Without the key, the digest does not let a reader test a
// guess at the parameters.
//
// The key identifier is carried in clear beside the keyed digest. As the
// Runtime's keyed record does, this refuses, with ErrKeyedDigest, an
// identifier that holds a plain digest the keyed digest replaces: the
// SHA-256 of ParameterText (the digest ParameterDigest names) or of Bytes,
// as hexadecimal digits of either case or as unpadded base64 of either
// alphabet, anywhere in the identifier. That catches one plausible mistake
// and is not a defence: an identifier derived from the parameters any other
// way (a truncated digest, a digest of another spelling of them, of the
// case or of its source data) is not detected, and re-creates the
// pseudonym the key removes. Use an opaque identifier, such as a random
// string or a version number, never one derived from the case or the key.
func (p *Preparation) KeyedParameterDigest(keyID string, key []byte) (KeyedDigest, error) {
	if !validKeyID(keyID) || len(key) == 0 || keyIDHolds(keyID, p.ParameterText(), p.bytes) {
		return KeyedDigest{}, ErrKeyedDigest
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(p.ParameterText())
	return KeyedDigest{Algorithm: HMACSHA256, KeyID: keyID, Digest: mac.Sum(nil)}, nil
}

func validKeyID(id string) bool {
	if len(id) == 0 || len(id) > MaxKeyIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == '-', c == ':', c == '/':
		default:
			return false
		}
	}
	return true
}

// keyIDHolds reports whether a key identifier holds the SHA-256 of one of
// the texts, as hexadecimal digits of either case or as unpadded base64 of
// either alphabet: the forms of a digest the identifier's characters can
// spell. It is the Runtime's rule for its keyed record (keyIDHolds).
func keyIDHolds(id string, texts ...[]byte) bool {
	lower := strings.ToLower(id)
	for _, text := range texts {
		sum := sha256.Sum256(text)
		if strings.Contains(lower, hex.EncodeToString(sum[:])) ||
			strings.Contains(id, base64.RawURLEncoding.EncodeToString(sum[:])) ||
			strings.Contains(id, base64.RawStdEncoding.EncodeToString(sum[:])) {
			return true
		}
	}
	return false
}
