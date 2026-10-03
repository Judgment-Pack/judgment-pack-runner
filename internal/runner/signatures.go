package runner

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
)

// Record signatures (runtime ADR-0047 §2b). This follows the rule the
// Runtime's guide writes out for a second implementation, "Record signatures,
// exactly", and not the Runtime's code.
//
// Given a signing key, the Runtime appends one line to signatures.jsonl, beside
// its trail, for each chained record it writes: the record's trail, its
// sequence and the SHA-256 of its exact line bytes, signed with Ed25519. An
// attempt's trail holds one record, so its sidecar holds that record's
// signature, or nothing when signing is off or the signature could not be
// written. Runner keeps the sidecar's bytes exactly, as run.auditSignatures,
// and a version-5 export carries them. A verifier checks them under a public
// key it trusts by the whole rule, the trail being known: it is the record's
// one line, run.auditBytes.
//
// What a valid signature establishes: whoever held the key signed these exact
// bytes as that record of that trail. Nothing against the operator, who holds
// the key, and nothing once the key is copied.

const (
	recordSignaturePrefix = "judgment-pack-runtime/record-signature/1:"
	keyRotationPrefix     = "judgment-pack-runtime/key-rotation/1:"
	// maxSidecarLine is the longest sidecar line the rule reads, its newline
	// not counted.
	maxSidecarLine = 4096
	// maxSidecar bounds the sidecar Runner keeps of an attempt, whose one
	// record takes one line of about 420 bytes.
	maxSidecar = 16 << 10
)

// The names of the checks a record's signature can fail: the Runtime's.
const (
	FindingSignatureInvalid        = "signature-invalid"
	FindingSignatureRecordMismatch = "signature-record-mismatch"
	FindingSignatureNoRecord       = "signature-no-record"
	FindingRotationInvalid         = "rotation-invalid"
	FindingSidecarOutOfOrder       = "sidecar-out-of-order"
)

var (
	publicKeyForm = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyIDForm     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	signatureForm = regexp.MustCompile(`^[0-9a-f]{128}$`)
)

// The field and curve of Ed25519 (RFC 8032 §5.1): p = 2^255 - 19, and
// d = -121665/121666 mod p.
var (
	fieldP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	curveD = func() *big.Int {
		d := new(big.Int).ModInverse(big.NewInt(121666), fieldP)
		d.Mul(d, big.NewInt(-121665))
		return d.Mod(d, fieldP)
	}()
)

// smallOrderKeys are the canonical encodings of the eight points whose order
// divides 8, as the guide lists them.
var smallOrderKeys = [8]string{
	"0100000000000000000000000000000000000000000000000000000000000000",
	"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	"0000000000000000000000000000000000000000000000000000000000000000",
	"0000000000000000000000000000000000000000000000000000000000000080",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
}

// The key check's refusals.
var (
	errKeyForm         = errors.New("a public key is 64 lowercase hexadecimal characters")
	errKeyNotCanonical = errors.New("the public key is not the canonical encoding of a point")
	errKeyNoPoint      = errors.New("the public key is no point of the curve")
	errKeySmallOrder   = errors.New("the public key is a point of small order, under which anyone can sign")
)

// ParsePublicKey reads a public key, 64 lowercase hexadecimal characters, and
// holds it to the guide's key check before anything signed with it is read:
// its 32 bytes must be the canonical encoding (RFC 8032 §5.1.2) of a point of
// the curve whose order does not divide 8.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	if !publicKeyForm.MatchString(text) {
		return nil, errKeyForm
	}
	key, _ := hex.DecodeString(text)
	if err := checkPublicKey(key); err != nil {
		return nil, err
	}
	return ed25519.PublicKey(key), nil
}

// checkPublicKey is the key check, on the encoding as written, before anything
// reduces it: y is its low 255 bits, little-endian, and its top bit is the
// sign of x.
func checkPublicKey(key []byte) error {
	sign := key[31] >> 7
	little := bytes.Clone(key)
	little[31] &= 0x7f
	for i, j := 0, len(little)-1; i < j; i, j = i+1, j-1 {
		little[i], little[j] = little[j], little[i]
	}
	y := new(big.Int).SetBytes(little)
	one := big.NewInt(1)
	minusOne := new(big.Int).Sub(fieldP, one)
	switch {
	case y.Cmp(fieldP) >= 0:
		return errKeyNotCanonical
	case sign == 1 && (y.Cmp(one) == 0 || y.Cmp(minusOne) == 0):
		// x is 0, and its sign cannot be set.
		return errKeyNotCanonical
	}
	// x² = (y² - 1) / (d·y² + 1) must have a square root modulo p.
	y2 := new(big.Int).Mul(y, y)
	y2.Mod(y2, fieldP)
	u := new(big.Int).Sub(y2, one)
	u.Mod(u, fieldP)
	v := new(big.Int).Mul(curveD, y2)
	v.Add(v, one)
	v.Mod(v, fieldP)
	x2 := new(big.Int).Mul(u, new(big.Int).ModInverse(v, fieldP))
	x2.Mod(x2, fieldP)
	if x2.Sign() != 0 && new(big.Int).Exp(x2, new(big.Int).Rsh(minusOne, 1), fieldP).Cmp(one) != 0 {
		return errKeyNoPoint
	}
	encoded := hex.EncodeToString(key)
	for _, small := range smallOrderKeys {
		if encoded == small {
			return errKeySmallOrder
		}
	}
	return nil
}

// KeyID is a public key's keyId: the first 32 lowercase hexadecimal characters
// of the SHA-256 of its 32 bytes, the gateway's keyId.
func KeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:32]
}

// sidecarLine is a readable line of a signature sidecar: a record signature
// for a sequence, or a key rotation at a line of the trail.
type sidecarLine struct {
	rotation  bool
	keyID     string
	trail     string
	at        int64  // a record signature's sequence, or a rotation's at
	value     string // a record signature's record, or a rotation's next
	signature []byte
}

// readSidecarLine reads one line of a sidecar, without its newline, by the
// rule: one JSON object of exactly the seven members of a record signature or
// of a key rotation, each once and of its form, whatever its whitespace and
// its escapes, and at most 4096 bytes. Names and strings are what JSON decodes
// them to, and the forms are held on the decoded values. Any other line is
// unreadable, which signs nothing and is not a failure.
func readSidecarLine(line []byte) (sidecarLine, bool) {
	var l sidecarLine
	if len(line) == 0 || len(line) > maxSidecarLine || !json.Valid(line) {
		return l, false
	}
	members, err := exactMembers(line)
	if err != nil || len(members) != 7 {
		return l, false
	}
	var kind, version, signature string
	if memberString(members["kind"], &kind) != nil {
		return l, false
	}
	place, value, valueForm := "sequence", "record", chainDigestForm
	switch kind {
	case "record-signature":
	case "key-rotation":
		l.rotation, place, value, valueForm = true, "at", "next", publicKeyForm
	default:
		return l, false
	}
	readable := memberString(members["sidecarVersion"], &version) == nil && version == "1" &&
		memberString(members["keyId"], &l.keyID) == nil && keyIDForm.MatchString(l.keyID) &&
		memberString(members["trail"], &l.trail) == nil && chainTrailForm.MatchString(l.trail) &&
		memberInteger(members[place], &l.at) == nil && l.at >= 1 && l.at < maxSafeInteger &&
		memberString(members[value], &l.value) == nil && valueForm.MatchString(l.value) &&
		memberString(members["signature"], &signature) == nil && signatureForm.MatchString(signature)
	if !readable {
		return sidecarLine{}, false
	}
	l.signature, _ = hex.DecodeString(signature)
	return l, true
}

// signedBytes are the bytes a line's signature signs: its domain's prefix and
// the canonical form of its trail, place and value, which for these forms is
// exactly the text below.
func (l sidecarLine) signedBytes() []byte {
	if l.rotation {
		return fmt.Appendf(nil, `%s{"at":%d,"next":"%s","trail":"%s"}`, keyRotationPrefix, l.at, l.value, l.trail)
	}
	return fmt.Appendf(nil, `%s{"record":"%s","sequence":%d,"trail":"%s"}`, recordSignaturePrefix, l.value, l.at, l.trail)
}

// verifiedUnder says whether the line's signature verifies under key over its
// signed bytes. crypto/ed25519 checks exactly the rule's equation: sigS below
// L, and the encoding of [sigS]B - [h]A equal to sigR byte for byte, without
// the cofactor. The key has passed the key check, which it does not make.
func (l sidecarLine) verifiedUnder(key ed25519.PublicKey) bool {
	return l.keyID == KeyID(key) && ed25519.Verify(key, l.signedBytes(), l.signature)
}

// recordLink is a chained record's three chain members.
type recordLink struct {
	trail    string
	sequence int64
	previous string
}

// readRecordLink reads whether a trail's line is a chained record, by its
// members, as the Runtime recognises one: one JSON object naming trail (32
// lowercase hexadecimal characters), sequence (an integer from 1 to 2^53-2)
// and previous ("sha256:" and 64 lowercase hexadecimal characters), each once.
func readRecordLink(line []byte) (recordLink, bool) {
	var l recordLink
	if len(line) == 0 || !json.Valid(line) {
		return l, false
	}
	members, err := exactMembers(line)
	if err != nil ||
		memberString(members["trail"], &l.trail) != nil || !chainTrailForm.MatchString(l.trail) ||
		memberInteger(members["sequence"], &l.sequence) != nil || l.sequence < 1 || l.sequence >= maxSafeInteger ||
		memberString(members["previous"], &l.previous) != nil || !chainDigestForm.MatchString(l.previous) {
		return recordLink{}, false
	}
	return l, true
}

// sidecarLines are a sidecar's lines that a newline ends, without it. A last
// line with no newline after it, as a failed write leaves one, is not among
// them: it was never written, as a verifier reads it.
func sidecarLines(sidecar []byte) [][]byte {
	lines := bytes.Split(sidecar, []byte("\n"))
	return lines[:len(lines)-1]
}

// sidecarLineCounts counts a sidecar's readable lines and its unreadable ones,
// a last line with no newline after it among them.
func sidecarLineCounts(sidecar []byte) (readable, unreadable int) {
	for _, line := range sidecarLines(sidecar) {
		if _, ok := readSidecarLine(line); ok {
			readable++
		} else {
			unreadable++
		}
	}
	if !bytes.HasSuffix(sidecar, []byte("\n")) && len(sidecar) > 0 {
		unreadable++
	}
	return readable, unreadable
}

// signatureCheck is what checking a sidecar under a key found.
type signatureCheck struct {
	findings   []ChainFinding
	total      int
	signed     map[int64]bool
	through    int64 // the signed coverage: the highest sequence it reaches
	readable   int
	unreadable int
	inForce    ed25519.PublicKey
}

func (c *signatureCheck) find(name string, line int64, detail string) {
	c.total++
	if len(c.findings) < maxChainFindings {
		c.findings = append(c.findings, ChainFinding{Name: name, Line: line, Detail: detail})
	}
}

// checkSignatures checks a sidecar against its trail, given as its lines
// without their newlines, under key, the one public key the trail was first
// signed with, and no revocations: the rule's verification, with n = 1. The
// trail is read as chained from its first line, as an attempt's always is: a
// line that is not a chained record at its place, linked to the line before it
// (the first to the SHA-256 of nothing) and of its trail, stops the signed
// coverage there, and is a finding unless it is not chained at all. A legacy
// prefix and a repair's discontinuity are for the Runtime's audit verify.
// Findings are placed at the trail's line they concern.
func checkSignatures(trail [][]byte, sidecar []byte, key ed25519.PublicKey) signatureCheck {
	c := signatureCheck{signed: map[int64]bool{}, inForce: key}
	links := make([]recordLink, len(trail))
	chained := make([]bool, len(trail))
	sound := int64(0)
	for i, line := range trail {
		number := int64(i + 1)
		links[i], chained[i] = readRecordLink(line)
		previous := digest(nil)
		if i > 0 {
			previous = digest(trail[i-1])
		}
		switch {
		case !chained[i]:
		case links[i].sequence != number:
			c.find(FindingSequenceMismatch, number, fmt.Sprintf("the record's sequence is %d", links[i].sequence))
		case links[i].previous != previous:
			c.find(FindingPreviousMismatch, number, "previous is not the digest of the line before it")
		case i > 0 && chained[i-1] && links[i].trail != links[i-1].trail:
			c.find(FindingTrailMismatch, number, "the record names another trail than the line before it")
		default:
			if sound == number-1 {
				sound = number
			}
		}
	}
	c.readable, c.unreadable = sidecarLineCounts(sidecar)
	place := int64(0)
	for number, raw := range sidecarLines(sidecar) {
		l, ok := readSidecarLine(raw)
		if !ok {
			continue
		}
		at := fmt.Sprintf("sidecar line %d", number+1)
		next := 2 * l.at
		if l.rotation {
			next++
		}
		if next < place || !l.rotation && next == place {
			c.find(FindingSidecarOutOfOrder, l.at, at+": behind a line already read")
			continue
		}
		place = next
		if !l.rotation {
			s := l.at
			switch {
			case s > int64(len(trail)) || !chained[s-1] || links[s-1].trail != l.trail:
				c.find(FindingSignatureNoRecord, s, at+": the trail has no chained record of this trail at this sequence")
			case digest(trail[s-1]) != l.value:
				c.find(FindingSignatureRecordMismatch, s, at+": the signature is for another record than the one at its sequence")
			case !l.verifiedUnder(c.inForce):
				c.find(FindingSignatureInvalid, s, at+": the signature does not verify under the key in force")
			default:
				c.signed[s] = true
			}
			continue
		}
		a := l.at
		if a > int64(len(trail)) {
			c.find(FindingSignatureNoRecord, a, at+": the trail has no line at the rotation's place")
			continue
		}
		anchored := false
		for i := a - 1; i >= 0; i-- {
			if chained[i] {
				anchored = links[i].trail == l.trail
				break
			}
		}
		next2, err := ParsePublicKey(l.value)
		switch {
		case !anchored:
			c.find(FindingRotationInvalid, a, at+": the rotation is not of the trail of the last chained line at or before its place")
		case !l.verifiedUnder(c.inForce):
			c.find(FindingRotationInvalid, a, at+": the rotation is not signed by the key in force")
		case err != nil:
			c.find(FindingRotationInvalid, a, at+": the rotation is to a key a verifier refuses")
		default:
			c.inForce = next2
		}
	}
	for s := sound; s >= 1; s-- {
		if c.signed[s] {
			c.through = s
			break
		}
	}
	return c
}

// SignatureReport is what checking a run's record signature found. Its
// members are in name order, as the rest of verify-run's report.
type SignatureReport struct {
	Findings        []ChainFinding `json:"findings"`
	FindingsTotal   int            `json:"findingsTotal"`
	KeyID           string         `json:"keyId,omitempty"`
	PublicKey       string         `json:"publicKey,omitempty"`
	ReadableLines   int            `json:"readableLines"`
	Status          string         `json:"status"`
	UnreadableLines int            `json:"unreadableLines"`
}

// The statuses of a record signature check.
const (
	// SignatureSigned is a record a valid signature under the key covers, with
	// no failed check.
	SignatureSigned = "signed"
	// SignatureUnsigned is a record no valid signature covers, with no failed
	// check: no sidecar line signs it, which is never a failure by itself.
	SignatureUnsigned = "unsigned"
	// SignatureInvalid is a check that failed.
	SignatureInvalid = "invalid"
	// SignatureNotChecked is a sidecar no key was given to check.
	SignatureNotChecked = "not-checked"
)

// Signatures is the record's signature sidecar a version-5 export carries,
// exactly as the attempt's Runtime wrote it, and nil for another version.
func (v VerifiedRun) Signatures() []byte { return v.bundle.Run.AuditSignatures }

// CheckSignature checks the record of a verified export under key: the
// sidecar a version-5 export carries, against the attempt's trail, which is
// the record's one line. Without a key, a sidecar is reported not checked. An
// export of another version carries no sidecar, and its record is unsigned
// as far as it shows.
func (v VerifiedRun) CheckSignature(key ed25519.PublicKey) SignatureReport {
	sidecar := v.bundle.Run.AuditSignatures
	r := SignatureReport{Findings: []ChainFinding{}, Status: SignatureUnsigned}
	if key == nil {
		r.ReadableLines, r.UnreadableLines = sidecarLineCounts(sidecar)
		r.Status = SignatureNotChecked
		return r
	}
	r.KeyID, r.PublicKey = KeyID(key), hex.EncodeToString(key)
	if len(v.bundle.Run.AuditBytes) == 0 || len(sidecar) == 0 {
		return r
	}
	c := checkSignatures([][]byte{v.bundle.Run.AuditBytes}, sidecar, key)
	r.Findings, r.FindingsTotal, r.ReadableLines, r.UnreadableLines = c.findings, c.total, c.readable, c.unreadable
	switch {
	case c.total > 0:
		r.Status = SignatureInvalid
	case c.through >= 1:
		r.Status = SignatureSigned
	}
	if r.Findings == nil {
		r.Findings = []ChainFinding{}
	}
	return r
}
