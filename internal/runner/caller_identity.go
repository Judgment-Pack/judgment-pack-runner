package runner

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Caller identity on the control API (runner #236, docs/design/service-mode.md).
//
// In service mode a Runner can hold every request on its control API to a
// signed identity token, in place of the owner bearer: a JSON Web Token (RFC
// 7519) in compact form, from the one issuer its configuration names, naming
// the configured audience, for a subject the configuration lists, signed by a
// key of a key set file the operator holds, within its validity window. Runner
// checks it with the standard library and fetches nothing on the request
// path: a token signed by a key that is not in the file is refused, and a
// changed file is read at the next start. The rules are the gateway's (its
// ADR-0025 and go/identity.go), written again here rather than shared; the two
// are meant to stay aligned, and docs/design/service-mode.md lists where they
// differ.
//
// What the check does not establish: who holds a token. A subject is the
// issuer's name for a caller, not a person Runner knows. A token is a bearer
// credential: whoever holds it can present it until it expires, Runner keeps
// no record of the tokens it has seen, and over plain HTTP it can be read on
// the network between unless TLS terminates in front of Runner. Runner keeps
// no token: neither a record nor the journal nor an answer holds one.

// requester is who made a request on the control API. The zero value is the
// installation: a request that carried the owner bearer. Otherwise it is the
// issuer and subject of the identity token the request carried, as checked.
type requester struct {
	issuer, subject string
}

// requesterContext is the key of a request's requester in its context.
type requesterContext struct{}

// withRequester is ctx carrying by as its request's requester.
func withRequester(ctx context.Context, by requester) context.Context {
	return context.WithValue(ctx, requesterContext{}, by)
}

// requesterFrom is the requester ctx carries: the installation when it
// carries none, as every request did before service mode.
func requesterFrom(ctx context.Context) requester {
	by, _ := ctx.Value(requesterContext{}).(requester)
	return by
}

// actor is the journal's name for a requester: the installation, or a caller
// by its issuer and subject.
func (s *Service) actor(by requester) EventActor {
	if by.subject == "" {
		return s.installation()
	}
	return EventActor{Kind: "caller", Issuer: by.issuer, Subject: by.subject}
}

// requestedBy is a run's requestedBy, and the scope its idempotency key is
// held in: the owner for the installation, as it always was, and the subject
// for a caller.
func (s *Service) requestedBy(by requester) string {
	if by.subject == "" {
		return s.cfg.Owner
	}
	return by.subject
}

// Bounds of an identity.
const (
	// maxIdentityText bounds the issuer and the audience.
	maxIdentityText = 1024
	// maxSubject and maxSubjects bound a subject and the list of them.
	maxSubject  = 256
	maxSubjects = 256
	// maxKeySetBytes bounds the key set file, as the gateway bounds its own.
	maxKeySetBytes = 1 << 20
	// maxTokenBytes bounds a token: a JWT is a few hundred bytes to a few
	// kilobytes; anything more is not one.
	maxTokenBytes = 8192
	// tokenLeeway is how far a token's validity window is stretched, for
	// clocks that disagree by a little.
	tokenLeeway = 30 * time.Second
	// maxTokenTime bounds exp and nbf to the integers a JSON number holds
	// exactly, so that no time made of one overflows.
	maxTokenTime = 1<<53 - 1
	// maxRSAExponent bounds an RSA public exponent to one that fits an int
	// on every build.
	maxRSAExponent = 1<<31 - 1
)

// IdentityConfig is a service configuration's identity member, as written:
// the token issuer, the audience this Runner is named as, the subjects whose
// requests it takes, and the absolute path of the issuer's key set file.
type IdentityConfig struct {
	Issuer   string
	Audience string
	Subjects []string
	Keys     string
}

// Identity is the identity Runner holds each control request to: the
// configured issuer, audience and subjects, and the keys read from the key
// set file at start.
type Identity struct {
	issuer, audience string
	subjects         map[string]bool
	keys             map[string]identityKey
}

// identityKey is one verification key of the key set, with the algorithm it
// is for.
type identityKey struct {
	alg string
	key crypto.PublicKey
}

// identityJSON reads a JSON text an identity reads (the identity member, the
// key set, a token's header and its claims) as one object, by the reader every
// input is read by (readInputJSONDepth): member names matched exactly, a
// duplicate member, invalid UTF-8 and an unpaired surrogate escape refused.
// encoding/json alone would match "SUB" to sub and keep the last of two
// subjects; none of that may decide who a caller is.
func identityJSON(data []byte, what string) (map[string]any, error) {
	v, err := readInputJSONDepth(data, 16)
	if err != nil {
		return nil, fmt.Errorf("%s is not one JSON object with each member once, in UTF-8", what)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a JSON object", what)
	}
	return obj, nil
}

// stringMember is a member that must be a string: "" and false when absent.
func stringMember(obj map[string]any, name, what string) (string, bool, error) {
	v, present := obj[name]
	if !present {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("%s %s is not a string", what, name)
	}
	return s, true, nil
}

// integerMember is a member that must be an integer within maxTokenTime of
// zero, written with no fraction and no exponent.
func integerMember(obj map[string]any, name, what string) (int64, bool, error) {
	v, present := obj[name]
	if !present {
		return 0, false, nil
	}
	// A fraction or an exponent is refused by ParseInt, as is a number past
	// int64; the bound keeps every time made of one exact.
	n, ok := v.(json.Number)
	if !ok {
		return 0, true, fmt.Errorf("%s %s is not an integer", what, name)
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || i > maxTokenTime || i < -maxTokenTime {
		return 0, true, fmt.Errorf("%s %s is not an integer", what, name)
	}
	return i, true, nil
}

// identityText says whether s is an issuer, an audience or a subject as
// configured: 1 to limit bytes, with no control character.
func identityText(s string, limit int) bool {
	return s != "" && len(s) <= limit && strings.IndexFunc(s, unicode.IsControl) < 0
}

// ParseIdentityConfig reads a service configuration's identity member: an
// object with exactly issuer, audience, subjects and keys. The issuer and the
// audience are 1 to 1024 bytes, each subject 1 to 256, none with a control
// character; subjects lists 1 to 256 of them, each once; keys is an absolute,
// clean path. A refusal names the member, never a value.
func ParseIdentityConfig(raw []byte) (IdentityConfig, error) {
	obj, err := identityJSON(raw, "identity")
	if err != nil {
		return IdentityConfig{}, err
	}
	for name := range obj {
		switch name {
		case "issuer", "audience", "subjects", "keys":
		default:
			return IdentityConfig{}, fmt.Errorf("identity has a member Runner does not read, %q: it takes issuer, audience, subjects and keys, each spelled so", name)
		}
	}
	var c IdentityConfig
	if c.Issuer, _, err = stringMember(obj, "issuer", "identity"); err != nil || !identityText(c.Issuer, maxIdentityText) {
		return IdentityConfig{}, errors.New("identity.issuer must be the token issuer, as its tokens name it: 1 to 1024 bytes, with no control character")
	}
	if c.Audience, _, err = stringMember(obj, "audience", "identity"); err != nil || !identityText(c.Audience, maxIdentityText) {
		return IdentityConfig{}, errors.New("identity.audience must be the audience this Runner is named as in a token: 1 to 1024 bytes, with no control character")
	}
	subjects, ok := obj["subjects"].([]any)
	if !ok || len(subjects) == 0 || len(subjects) > maxSubjects {
		return IdentityConfig{}, errors.New("identity.subjects must list the subjects whose requests Runner takes: 1 to 256 of them")
	}
	for i, v := range subjects {
		subject, ok := v.(string)
		switch {
		case !ok || !identityText(subject, maxSubject):
			return IdentityConfig{}, fmt.Errorf("identity.subjects[%d] must be a subject as its tokens name it: 1 to 256 bytes, with no control character", i)
		case slices.Contains(c.Subjects, subject):
			return IdentityConfig{}, fmt.Errorf("identity.subjects[%d] lists a subject a second time", i)
		}
		c.Subjects = append(c.Subjects, subject)
	}
	if c.Keys, _, err = stringMember(obj, "keys", "identity"); err != nil || !filepath.IsAbs(c.Keys) || filepath.Clean(c.Keys) != c.Keys {
		return IdentityConfig{}, errors.New("identity.keys must be the absolute, clean path of the issuer's key set file")
	}
	return c, nil
}

// LoadIdentity holds an identity's subjects to the names Runner records for
// requests of its own, then reads the issuer's key set from its file, held to
// its placement (readKeySet), before anything is opened. A subject is
// refused when it is the installation's owner, which is the requestedBy and
// the idempotency scope of every request that carries the owner bearer;
// "workflow", which a workflow's runs are requested by; or one that begins
// "trigger:", which a trigger's runs are. A refusal never names a path or a
// value.
func LoadIdentity(c IdentityConfig, owner, stateDir string) (*Identity, error) {
	if !identityText(c.Issuer, maxIdentityText) || !identityText(c.Audience, maxIdentityText) || len(c.Subjects) == 0 || len(c.Subjects) > maxSubjects {
		return nil, errors.New("identity needs an issuer, an audience and 1 to 256 subjects")
	}
	id := &Identity{issuer: c.Issuer, audience: c.Audience, subjects: map[string]bool{}}
	for i, subject := range c.Subjects {
		switch {
		case !identityText(subject, maxSubject) || id.subjects[subject]:
			return nil, fmt.Errorf("identity.subjects[%d] must be a subject, listed once: 1 to 256 bytes, with no control character", i)
		case subject == owner:
			return nil, fmt.Errorf("identity.subjects[%d] is the installation's owner, which every run the owner bearer requests is recorded as requested by: a caller's runs must be told apart from those", i)
		case subject == "workflow" || strings.HasPrefix(subject, "trigger:"):
			return nil, fmt.Errorf("identity.subjects[%d] is a name Runner records for runs it starts itself: \"workflow\" and every name that begins \"trigger:\" are", i)
		}
		id.subjects[subject] = true
	}
	data, err := readKeySet(c.Keys, stateDir)
	if err != nil {
		return nil, fmt.Errorf("identity.keys is refused: %w", err)
	}
	if id.keys, err = parseKeySet(data); err != nil {
		return nil, fmt.Errorf("identity.keys: %w", err)
	}
	return id, nil
}

// parseKeySet reads a JSON Web Key Set (RFC 7517) holding the issuer's public
// keys: RSA of 2048 bits or more (RS256), EC P-256 (ES256) and Ed25519
// (EdDSA). Every key carries a key id, once in the set. A use other than
// signing, or key operations that do not include verifying, refuse the key;
// so does an alg that is not its kind's. A key of another kind is refused
// rather than skipped, since a file Runner would read only part of is a file
// the operator misreads. Each encoded member is the unpadded base64url of its
// bytes, and nothing else.
func parseKeySet(data []byte) (map[string]identityKey, error) {
	set, err := identityJSON(data, "the key set")
	if err != nil {
		return nil, err
	}
	list, ok := set["keys"].([]any)
	if !ok {
		return nil, errors.New("the key set has no keys array")
	}
	if len(list) == 0 {
		return nil, errors.New("the key set holds no key")
	}
	keys := map[string]identityKey{}
	for i, item := range list {
		jwk, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("key %d of the key set is not an object", i)
		}
		what := fmt.Sprintf("key %d of the key set", i)
		kid, _, err := stringMember(jwk, "kid", what)
		if err != nil {
			return nil, err
		}
		if kid == "" {
			return nil, fmt.Errorf("%s has no kid", what)
		}
		if _, dup := keys[kid]; dup {
			return nil, fmt.Errorf("%s has the kid of an earlier key", what)
		}
		use, _, err := stringMember(jwk, "use", what)
		if err != nil {
			return nil, err
		}
		if use != "" && use != "sig" {
			return nil, fmt.Errorf("%s is not for signing", what)
		}
		if raw, present := jwk["key_ops"]; present {
			ops, ok := raw.([]any)
			if !ok || !slices.ContainsFunc(ops, func(op any) bool { return op == "verify" }) || slices.ContainsFunc(ops, func(op any) bool { _, s := op.(string); return !s }) {
				return nil, fmt.Errorf("%s is not for verifying: its key_ops do not include verify", what)
			}
		}
		decode := func(name string) ([]byte, error) {
			s, _, err := stringMember(jwk, name, what)
			if err != nil {
				return nil, err
			}
			if s == "" {
				return nil, fmt.Errorf("%s has no %s", what, name)
			}
			b, err := base64.RawURLEncoding.Strict().DecodeString(s)
			if err != nil || base64.RawURLEncoding.EncodeToString(b) != s {
				return nil, fmt.Errorf("%s: its %s is not canonical base64url", what, name)
			}
			return b, nil
		}
		kty, _, err := stringMember(jwk, "kty", what)
		if err != nil {
			return nil, err
		}
		crv, _, err := stringMember(jwk, "crv", what)
		if err != nil {
			return nil, err
		}
		var k identityKey
		switch kty {
		case "RSA":
			n, err := decode("n")
			if err != nil {
				return nil, err
			}
			e, err := decode("e")
			if err != nil {
				return nil, err
			}
			modulus, exponent := new(big.Int).SetBytes(n), new(big.Int).SetBytes(e)
			if modulus.BitLen() < 2048 {
				return nil, fmt.Errorf("%s is an RSA key of under 2048 bits", what)
			}
			if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > maxRSAExponent || exponent.Bit(0) == 0 {
				return nil, fmt.Errorf("%s has an RSA exponent that is even or outside 3 to 2^31-1", what)
			}
			k = identityKey{alg: "RS256", key: &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}}
		case "EC":
			if crv != "P-256" {
				return nil, fmt.Errorf("%s is on a curve other than P-256, the one EC curve read", what)
			}
			x, err := decode("x")
			if err != nil {
				return nil, err
			}
			y, err := decode("y")
			if err != nil {
				return nil, err
			}
			if len(x) != 32 || len(y) != 32 {
				return nil, fmt.Errorf("%s has a coordinate that is not 32 bytes", what)
			}
			point := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if !point.Curve.IsOnCurve(point.X, point.Y) {
				return nil, fmt.Errorf("%s is not a point on P-256", what)
			}
			k = identityKey{alg: "ES256", key: point}
		case "OKP":
			if crv != "Ed25519" {
				return nil, fmt.Errorf("%s is on a curve other than Ed25519, the one OKP curve read", what)
			}
			x, err := decode("x")
			if err != nil {
				return nil, err
			}
			if len(x) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("%s is not an Ed25519 public key", what)
			}
			k = identityKey{alg: "EdDSA", key: ed25519.PublicKey(x)}
		default:
			return nil, fmt.Errorf("%s has a kty other than RSA, EC and OKP, the kinds read", what)
		}
		alg, _, err := stringMember(jwk, "alg", what)
		if err != nil {
			return nil, err
		}
		if alg != "" && alg != k.alg {
			return nil, fmt.Errorf("%s names an alg that is not its kind's, %s", what, k.alg)
		}
		keys[kid] = k
	}
	return keys, nil
}

// bearerToken is the token of an Authorization header, or "" when the header
// carries none: the Bearer scheme, in any case, then the token.
func bearerToken(authorization string) string {
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// tokenSegment decodes one part of a compact token, which must be exactly the
// unpadded base64url of its bytes (RFC 7515 §5.2): no padding, no line break,
// no bit the encoding does not read, so that no second spelling of a token is
// taken.
func tokenSegment(what, s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, fmt.Errorf("the token's %s is not canonical base64url", what)
	}
	return b, nil
}

// check holds a request's Authorization headers to the identity at now, and
// answers the requester the token names: exactly one header, the Bearer
// scheme, a JWT in compact form of at most 8192 bytes, whose header names no
// critical extension and a key id the key set holds, whose alg is that key's
// (so never "none"), whose signature that key verifies over its first two
// parts, and whose claims, read only then, name the configured issuer as iss,
// the configured audience in aud (a string, or an array of strings), a
// subject the configuration lists as sub, an integer exp that has not passed
// and, when present, an integer nbf that has: each time with 30 seconds'
// leeway. Every refusal is one reason, and none of them repeats any part of
// the token.
func (id *Identity) check(authorization []string, now time.Time) (requester, error) {
	if len(authorization) > 1 {
		return requester{}, errors.New("the request carries more than one Authorization header")
	}
	token := ""
	if len(authorization) == 1 {
		token = bearerToken(authorization[0])
	}
	if token == "" {
		return requester{}, errors.New("the request carries no bearer token")
	}
	if len(token) > maxTokenBytes {
		return requester{}, errors.New("the token is longer than 8192 bytes, longer than a token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return requester{}, errors.New("the token is not three dot-separated parts")
	}
	headerBytes, err := tokenSegment("header", parts[0])
	if err != nil {
		return requester{}, err
	}
	header, err := identityJSON(headerBytes, "the token's header")
	if err != nil {
		return requester{}, err
	}
	if _, present := header["crit"]; present {
		return requester{}, errors.New("the token's header names critical extensions Runner does not implement")
	}
	alg, _, err := stringMember(header, "alg", "the token's header:")
	if err != nil {
		return requester{}, err
	}
	kid, _, err := stringMember(header, "kid", "the token's header:")
	if err != nil {
		return requester{}, err
	}
	if kid == "" {
		return requester{}, errors.New("the token names no key id")
	}
	key, ok := id.keys[kid]
	if !ok {
		return requester{}, errors.New("the token is signed by a key that is not in the key set")
	}
	// The algorithm is the key's, never the token's word: a token that says
	// otherwise is refused, and "none" with it.
	if alg != key.alg {
		return requester{}, fmt.Errorf("the token's alg is not its key's, %s", key.alg)
	}
	signature, err := tokenSegment("signature", parts[2])
	if err != nil {
		return requester{}, err
	}
	if !verifyTokenSignature(key, []byte(parts[0]+"."+parts[1]), signature) {
		return requester{}, errors.New("the token's signature does not verify under its key")
	}
	payload, err := tokenSegment("payload", parts[1])
	if err != nil {
		return requester{}, err
	}
	claims, err := identityJSON(payload, "the token's claims")
	if err != nil {
		return requester{}, err
	}
	issuer, _, err := stringMember(claims, "iss", "the token's")
	if err != nil {
		return requester{}, err
	}
	if issuer != id.issuer {
		return requester{}, errors.New("the token is not from the configured issuer")
	}
	if !namesAudience(claims["aud"], id.audience) {
		return requester{}, errors.New("the token does not name this Runner's audience")
	}
	subject, _, err := stringMember(claims, "sub", "the token's")
	if err != nil {
		return requester{}, err
	}
	if subject == "" {
		return requester{}, errors.New("the token names no subject")
	}
	expires, present, err := integerMember(claims, "exp", "the token's")
	if err != nil {
		return requester{}, err
	}
	if !present {
		return requester{}, errors.New("the token has no expiry")
	}
	if !now.Before(time.Unix(expires, 0).Add(tokenLeeway)) {
		return requester{}, errors.New("the token has expired")
	}
	notBefore, present, err := integerMember(claims, "nbf", "the token's")
	if err != nil {
		return requester{}, err
	}
	if present && now.Add(tokenLeeway).Before(time.Unix(notBefore, 0)) {
		return requester{}, errors.New("the token is not yet valid")
	}
	if !id.subjects[subject] {
		return requester{}, errors.New("the token's subject is not one the configuration lists")
	}
	return requester{issuer: issuer, subject: subject}, nil
}

// namesAudience says whether a token's aud, a string or an array of strings,
// names audience. Any other shape names none.
func namesAudience(aud any, audience string) bool {
	switch v := aud.(type) {
	case string:
		return v == audience
	case []any:
		named := false
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return false
			}
			named = named || s == audience
		}
		return named
	}
	return false
}

// verifyTokenSignature checks a JWS signature over signed with the key's
// algorithm: RSASSA-PKCS1-v1_5 or ECDSA (as r || s, 32 bytes each) over
// SHA-256, or Ed25519.
func verifyTokenSignature(key identityKey, signed, signature []byte) bool {
	digest := sha256.Sum256(signed)
	switch k := key.key.(type) {
	case *rsa.PublicKey:
		return key.alg == "RS256" && rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], signature) == nil
	case *ecdsa.PublicKey:
		if key.alg != "ES256" || len(signature) != 64 {
			return false
		}
		return ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
	case ed25519.PublicKey:
		return key.alg == "EdDSA" && ed25519.Verify(k, signed, signature)
	}
	return false
}
