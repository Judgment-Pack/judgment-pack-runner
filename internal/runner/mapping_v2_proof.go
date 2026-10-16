package runner

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func validHex(s string, n int) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == n && hex.EncodeToString(b) == s
}
func validDigest(s string) bool {
	return strings.HasPrefix(s, "sha256:") && validHex(strings.TrimPrefix(s, "sha256:"), 32)
}
func keyID(public string) string {
	b, _ := hex.DecodeString(public)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:32]
}
func flatToken(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// adapterShapes are the gateway's adapter shapes (gateway SPEC.md §1.2a,
// acquisition.shape; §6, "Adapter sources"). An operation is read from a
// retained record of any of them alike: its receipt is held to the shape its
// profile pins, which the record declares, and the bytes the receipt signs
// are derived the same way whatever the shape, so a preparation's form does
// not depend on it. The request is what differs: an MCP operation's is a
// tool call, held to a tool its profile allows (mcpToolCall); a request to a
// source of another shape is formed by the mapping's author and held to the
// template rules alone, as its receipt commits to it. "command", a bare
// command with no adapter, is a shape a profile may name and an operation
// may not. A shape the gateway does not define is refused.
var adapterShapes = map[string]bool{"airbyte": true, "mcp": true, "http": true}

func validateProfiles(profiles []InputProfile) error {
	_, err := validateProfileKeys(profiles)
	return err
}

// validateProfileKeys admits profiles and retains the public keys that passed
// admission, so receipt verification cannot accidentally use a merely decoded
// key if a future caller reaches it by another path.
//
// A class is a profile's, and a binding takes it only where the source's
// kind admits that class (spec/dialect.md §5.1, RFC 0018 round 13): an
// operation of any shape takes its profile's class, record or generated; a
// research source's capability bindings take generated; a web-kind binding
// is record. So one gateway source, its key, source and authority, may be
// held by profiles of different classes, as it may by profiles of one class,
// each with its own id. A source names its profile by id and digest and
// takes that profile's class. Runner refuses the kind web by name and reads
// the gateway's web source as an operation of shape http (refusedKind),
// which may name the record profile while a research source's web
// assignment names the generated one (runner #100, #114).
func validateProfileKeys(profiles []InputProfile) (map[string]ed25519.PublicKey, error) {
	if len(profiles) > 16 {
		return nil, errors.New("at most 16 input profiles")
	}
	ids := map[string]bool{}
	keys := map[string]ed25519.PublicKey{}
	for _, p := range profiles {
		if len(encode(p)) > 32<<10 {
			return nil, errors.New("input profile exceeds 32 KiB")
		}
		if !mappingName.MatchString(p.ID) || ids[p.ID] || (p.Class != "record" && p.Class != "generated") || p.Source == "" || len(p.Source) > 256 || p.Authority == "" || len(p.Authority) > 256 || p.Adapter.Name == "" || p.Adapter.Version == "" || !validDigest(p.Adapter.Digest) || !adapterShapes[p.Shape] && p.Shape != "command" {
			return nil, errors.New("invalid installation input profile")
		}
		key, e := ParsePublicKey(p.PublicKey)
		if e != nil {
			return nil, fmt.Errorf("invalid installation input profile public key: %w", e)
		}
		keys[p.ID] = key
		if p.Endpoint != nil && len(*p.Endpoint) > 2048 || len(p.Tools) > 64 {
			return nil, errors.New("input profile exceeds limit")
		}
		if c := p.Calculator; c != nil && (p.Class != "record" || p.Shape != "mcp" || c.Name == "" || len(c.Name) > 256 || c.Version == "" || len(c.Version) > 256) {
			return nil, errors.New("a calculator profile is a record MCP profile naming its calculator and version")
		}
		if e := researchDeclarationForm(p); e != nil {
			return nil, e
		}
		ids[p.ID] = true
		seen := map[string]bool{}
		for _, t := range p.Tools {
			if t == "" || len(t) > 256 || seen[t] {
				return nil, errors.New("invalid profile tool allowlist")
			}
			seen[t] = true
		}
		if p.Shape == "mcp" && len(p.Tools) == 0 {
			return nil, errors.New("MCP profiles require an operator-reviewed read tool allowlist")
		}
	}
	return keys, nil
}
func profileFor(s MappingSource, profiles []InputProfile) (InputProfile, error) {
	for _, p := range profiles {
		if p.ID == s.Profile {
			if profileHash(p) != s.ProfileDigest {
				return p, errors.New("trusted input profile changed; review a new release")
			}
			if s.Kind == "operation" && !adapterShapes[p.Shape] {
				return p, errors.New("an operation is read from an adapter source: its profile's shape must be airbyte, mcp or http")
			}
			if s.Provider == "google-drive" && (p.Source != "drive" || p.Class != "record") {
				return p, errors.New("selected Drive files require a record profile for drive")
			}
			return p, nil
		}
	}
	return InputProfile{}, errors.New("input profile is not configured by the installation")
}

// verifyAcquisition verifies a single retained v3 acquisition, not a sealed
// session/store. Unknown signed fields remain covered by the signature. It
// holds every receipt to the specification's §5.2 (spec/dialect.md) alike:
// an operation's, of dialect version 2 or 3, a selected Drive file's, and a
// research source's retained responses and its record's own (research.go).
// The receipt's adapter is the profile's adapter as a whole object, and its
// observation and serving instants are each within the window
// acquisitionTimes gives, with no rule between the two (runner #98).
// Its sessionId is a string of any content (§5.2 does not hold it to the
// gateway's token form), read and cited as given (runner #99).
func verifyAcquisition(raw, args []byte, p InputProfile, public ed25519.PublicKey, at time.Time, maxAge int) (json.RawMessage, *Citation, error) {
	fail := func(s string) (json.RawMessage, *Citation, error) { return nil, nil, errors.New(s) }
	var response struct {
		Result  json.RawMessage `json:"result"`
		Receipt json.RawMessage `json:"receipt"`
		Salts   struct {
			Args      string `json:"args"`
			Statement string `json:"statement,omitempty"`
		} `json:"salts"`
	}
	if e := strictJSON(raw, &response); e != nil {
		return fail("malformed acquisition response")
	}
	result, e := proofCanon(response.Result)
	if e != nil {
		return fail("result is outside the gateway canonical domain")
	}
	canonicalReceipt, e := proofCanon(response.Receipt)
	if e != nil {
		return fail("malformed receipt")
	}
	var r map[string]any
	v, _ := readInputJSON(canonicalReceipt)
	r, ok := v.(map[string]any)
	if !ok {
		return fail("receipt is not an object")
	}
	str := func(k string) string { s, _ := r[k].(string); return s }
	for _, k := range []string{"receiptVersion", "kind", "source", "servedAt", "authority", "keyId", "signature", "resultDigest", "argumentsCommitment"} {
		if _, ok := r[k].(string); !ok {
			return fail("receipt is missing a required string")
		}
	}
	// The session identifier is a string (§5.2), of any content: the
	// dialect does not hold it to the gateway's token form, so it is read,
	// and cited, as given. A member of another type is malformed.
	if _, ok := r["sessionId"].(string); !ok {
		return fail("invalid session id")
	}
	idx, ok := r["callIndex"].(json.Number)
	if !ok {
		return fail("invalid call index")
	}
	index, e := idx.Int64()
	if e != nil || index < 0 {
		return fail("invalid call index")
	}
	prev, present := r["prevSignature"]
	if !present || prev != nil && !validHex(fmt.Sprint(prev), 64) || index == 0 && prev != nil || index > 0 && prev == nil {
		return fail("invalid previous signature")
	}
	if str("receiptVersion") != "3" || str("kind") != "acquisition" || !validHex(str("signature"), 64) || !validDigest(str("resultDigest")) || !validDigest(str("argumentsCommitment")) {
		return fail("unsupported or malformed receipt")
	}
	if _, ok := r["argumentsDigest"]; ok {
		return fail("version 2 argument digests are not valid in v3")
	}
	if _, ok := r["action"]; ok {
		return fail("an action cannot supply verified input")
	}
	caller, ok := r["caller"]
	if !ok {
		return fail("missing caller")
	}
	if caller != nil {
		c, ok := caller.(map[string]any)
		if !ok {
			return fail("invalid caller")
		}
		for _, k := range []string{"issuer", "subject", "tokenDigest"} {
			if _, ok := c[k].(string); !ok {
				return fail("invalid caller")
			}
		}
		if !validDigest(c["tokenDigest"].(string)) {
			return fail("invalid caller digest")
		}
	}
	a, ok := r["acquisition"].(map[string]any)
	if !ok {
		return fail("missing acquisition")
	}
	for _, k := range []string{"endpoint", "statement", "snapshot", "peerIdentity", "schema", "upstreamToken"} {
		v, ok := a[k]
		if !ok {
			return fail("missing acquisition field")
		}
		if v != nil {
			s, ok := v.(string)
			if !ok || (k == "statement" || k == "schema") && !validDigest(s) {
				return fail("invalid acquisition field")
			}
		}
	}
	if _, ok := a["pageItems"]; ok {
		return fail("paged acquisitions are not supported")
	}
	adapter, ok := a["adapter"].(map[string]any)
	if !ok {
		return fail("missing adapter")
	}
	for _, k := range []string{"name", "version", "digest"} {
		if _, ok := adapter[k].(string); !ok {
			return fail("invalid adapter")
		}
	}
	if !validDigest(adapter["digest"].(string)) {
		return fail("invalid adapter digest")
	}
	shape, ok := a["shape"].(string)
	if !ok || shape != p.Shape {
		return fail("acquisition shape differs from the trusted profile")
	}
	if str("keyId") != keyID(p.PublicKey) || str("source") != p.Source || str("authority") != p.Authority {
		return fail("receipt identity differs from the trusted profile")
	}
	endpoint := any(nil)
	if p.Endpoint != nil {
		endpoint = *p.Endpoint
	}
	// The receipt's endpoint and adapter are, as JSON values, the profile's
	// (§5.2, check 3): the adapter as a whole object, so that a signed
	// receipt whose adapter carries a member the profile does not pin is
	// refused, as one that lacks a member is by its form above.
	if a["endpoint"] != endpoint || !sameJSON(encode(adapter), encode(p.Adapter)) {
		return fail("adapter or endpoint differs from the trusted profile")
	}
	sig, _ := hex.DecodeString(str("signature"))
	delete(r, "signature")
	unsigned, e := proofCanon(encode(r))
	if e != nil || !ed25519.Verify(public, append([]byte("judgment-pack-gateway/receipt/3:"), unsigned...), sig) {
		return fail("receipt signature failed")
	}
	if digest(result) != str("resultDigest") {
		return fail("result digest failed")
	}
	if !validHex(response.Salts.Args, 32) {
		return fail("missing argument commitment salt")
	}
	salt, _ := hex.DecodeString(response.Salts.Args)
	argBytes, e := proofCanon(args)
	if e != nil {
		return fail("request is outside the gateway canonical domain")
	}
	committed := append(append(salt, []byte("args:")...), argBytes...)
	if digest(committed) != str("argumentsCommitment") {
		return fail("response does not match the frozen request and this case's parameters")
	}
	observed, ok := a["observedAt"].(string)
	if !ok {
		return fail("missing observation time")
	}
	if e := acquisitionTimes(observed, str("servedAt"), at, maxAge); e != nil {
		return nil, nil, e
	}
	return result, &Citation{str("sessionId"), int(index), hex.EncodeToString(sig)}, nil
}

// errAcquisitionStale is verifyAcquisition's finding for an instant outside
// its window, which a research refusal names acquisition-stale.
var errAcquisitionStale = errors.New("acquisition is stale or from the future")

// acquisitionTimes holds a receipt's times to §5.2: the observation instant,
// acquisition.observedAt, and the serving instant, servedAt, each against
// at, the preparation's instant (verifiedAt): no more than maxAge seconds
// before it and no more than 30 seconds after it, the edges inside;
// errAcquisitionStale otherwise. No rule joins the two instants, so an
// observation long after its service reads when both are within the window.
// Both are read before either is judged, each to its instant
// (receiptInstant): a time with no instant is invalid, not stale, for an
// operation, a selected file and a research receipt alike.
func acquisitionTimes(observed, served string, at time.Time, maxAge int) error {
	observation, ok := receiptInstant(observed)
	if !ok {
		return errors.New("invalid observation time")
	}
	service, ok := receiptInstant(served)
	if !ok {
		return errors.New("invalid serving time")
	}
	earliest, latest := at.Add(-time.Duration(maxAge)*time.Second), at.Add(30*time.Second)
	for _, instant := range []time.Time{observation, service} {
		if instant.Before(earliest) || instant.After(latest) {
			return errAcquisitionStale
		}
	}
	return nil
}

// receiptInstant is a receipt time's instant (spec/dialect.md §1): the time
// is a timestamp, YYYY-MM-DDThh:mm:ssZ, whole seconds in UTC, that names a
// time. A fraction of a second, an offset in place of Z, or a date or time
// that does not exist gives none.
func receiptInstant(s string) (time.Time, bool) {
	if !paramValue(s, "timestamp") {
		return time.Time{}, false
	}
	t, _ := time.Parse("2006-01-02T15:04:05Z", s)
	return t, true
}

func ParseInputProfiles(raw []byte) ([]InputProfile, error) {
	var p []InputProfile
	if e := strictJSON(raw, &p); e != nil {
		return nil, e
	}
	return p, validateProfiles(p)
}
