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
func validateProfiles(profiles []InputProfile) error {
	if len(profiles) > 16 {
		return errors.New("at most 16 input profiles")
	}
	ids := map[string]bool{}
	identities := map[string]string{}
	for _, p := range profiles {
		if len(encode(p)) > 32<<10 {
			return errors.New("input profile exceeds 32 KiB")
		}
		if !mappingName.MatchString(p.ID) || ids[p.ID] || (p.Class != "record" && p.Class != "generated") || p.Source == "" || len(p.Source) > 256 || p.Authority == "" || len(p.Authority) > 256 || p.Adapter.Name == "" || p.Adapter.Version == "" || !validDigest(p.Adapter.Digest) || p.Shape != "mcp" && p.Shape != "command" && p.Shape != "http" {
			return errors.New("invalid installation input profile")
		}
		if _, e := ParsePublicKey(p.PublicKey); e != nil {
			return fmt.Errorf("invalid installation input profile public key: %w", e)
		}
		if p.Endpoint != nil && len(*p.Endpoint) > 2048 || len(p.Tools) > 64 {
			return errors.New("input profile exceeds limit")
		}
		if c := p.Calculator; c != nil && (p.Class != "record" || p.Shape != "mcp" || c.Name == "" || len(c.Name) > 256 || c.Version == "" || len(c.Version) > 256) {
			return errors.New("a calculator profile is a record MCP profile naming its calculator and version")
		}
		identity := p.PublicKey + "\n" + p.Source + "\n" + p.Authority
		if c, ok := identities[identity]; ok && c != p.Class {
			return errors.New("one trusted source cannot have conflicting output classes")
		}
		identities[identity] = p.Class
		ids[p.ID] = true
		seen := map[string]bool{}
		for _, t := range p.Tools {
			if t == "" || len(t) > 256 || seen[t] {
				return errors.New("invalid profile tool allowlist")
			}
			seen[t] = true
		}
		if p.Shape == "mcp" && len(p.Tools) == 0 {
			return errors.New("MCP profiles require an operator-reviewed read tool allowlist")
		}
	}
	return nil
}
func profileFor(s MappingSource, profiles []InputProfile) (InputProfile, error) {
	for _, p := range profiles {
		if p.ID == s.Profile {
			if profileHash(p) != s.ProfileDigest {
				return p, errors.New("trusted input profile changed; review a new release")
			}
			if s.Kind == "operation" && p.Shape != "mcp" {
				return p, errors.New("this slice supports MCP operations only")
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
// session/store. Unknown signed fields remain covered by the signature.
func verifyAcquisition(raw, args []byte, p InputProfile, at time.Time, maxAge int) (json.RawMessage, *Citation, error) {
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
	for _, k := range []string{"receiptVersion", "kind", "sessionId", "source", "servedAt", "authority", "keyId", "signature", "resultDigest", "argumentsCommitment"} {
		if _, ok := r[k].(string); !ok {
			return fail("receipt is missing a required string")
		}
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
	if str("receiptVersion") != "3" || str("kind") != "acquisition" || !flatToken(str("sessionId")) || !validHex(str("signature"), 64) || !validDigest(str("resultDigest")) || !validDigest(str("argumentsCommitment")) {
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
	if a["endpoint"] != endpoint || adapter["name"] != p.Adapter.Name || adapter["version"] != p.Adapter.Version || adapter["digest"] != p.Adapter.Digest {
		return fail("adapter or endpoint differs from the trusted profile")
	}
	sig, _ := hex.DecodeString(str("signature"))
	public, _ := hex.DecodeString(p.PublicKey)
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
	obs, e := time.Parse(time.RFC3339Nano, observed)
	if e != nil {
		return fail("invalid observation time")
	}
	served, e := time.Parse(time.RFC3339Nano, str("servedAt"))
	if e != nil || obs.After(served.Add(30*time.Second)) || served.After(at.Add(30*time.Second)) || obs.After(at.Add(30*time.Second)) || obs.Before(at.Add(-time.Duration(maxAge)*time.Second)) {
		return fail("acquisition is stale or from the future")
	}
	return result, &Citation{str("sessionId"), int(index), hex.EncodeToString(sig)}, nil
}

func ParseInputProfiles(raw []byte) ([]InputProfile, error) {
	var p []InputProfile
	if e := strictJSON(raw, &p); e != nil {
		return nil, e
	}
	return p, validateProfiles(p)
}
