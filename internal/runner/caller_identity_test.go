package runner

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

// Caller identity (caller_identity.go). Every key is generated at each run
// and every token is made here: no key, token or key set is a fixture.

const (
	testIssuer   = "https://issuer.example"
	testAudience = "runner-control"
)

// tokenSigner signs tokens with one generated key, named kid in the key set
// by jwk.
type tokenSigner struct {
	kid, alg string
	sign     func([]byte) []byte
	jwk      map[string]any
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// testRSAKey is generated once per test binary: a 2048-bit key takes a
// moment to make.
var testRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func edSigner(t *testing.T, kid string) tokenSigner {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	return tokenSigner{kid: kid, alg: "EdDSA", sign: func(b []byte) []byte { return ed25519.Sign(private, b) },
		jwk: map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": kid, "x": b64(public)}}
}

func ecSigner(t *testing.T, kid string) tokenSigner {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	point, err := private.PublicKey.ECDH()
	must(t, err)
	raw := point.Bytes()
	return tokenSigner{kid: kid, alg: "ES256", sign: func(b []byte) []byte {
		digest := sha256.Sum256(b)
		r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
		if err != nil {
			panic(err)
		}
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		s.FillBytes(out[32:])
		return out
	}, jwk: map[string]any{"kty": "EC", "crv": "P-256", "kid": kid, "x": b64(raw[1:33]), "y": b64(raw[33:])}}
}

func rsaSigner(t *testing.T, kid string) tokenSigner {
	t.Helper()
	k := testRSAKey()
	return tokenSigner{kid: kid, alg: "RS256", sign: func(b []byte) []byte {
		digest := sha256.Sum256(b)
		signature, err := rsa.SignPKCS1v15(nil, k, crypto.SHA256, digest[:])
		if err != nil {
			panic(err)
		}
		return signature
	}, jwk: map[string]any{"kty": "RSA", "kid": kid, "n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}}
}

// signedParts is a token of two parts as given, signed by s over exactly
// them.
func signedParts(s tokenSigner, header, payload string) string {
	signed := header + "." + payload
	return signed + "." + b64(s.sign([]byte(signed)))
}

// rawToken is a token of the header and claims given as JSON text.
func rawToken(s tokenSigner, header, claims string) string {
	return signedParts(s, b64([]byte(header)), b64([]byte(claims)))
}

func headerOf(s tokenSigner) string {
	return `{"alg":"` + s.alg + `","kid":"` + s.kid + `","typ":"JWT"}`
}

// claimsFor are the claims of a token for subject that is valid at now.
func claimsFor(subject string, now time.Time) map[string]any {
	return map[string]any{"iss": testIssuer, "aud": testAudience, "sub": subject, "iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix()}
}

func tokenOf(s tokenSigner, claims map[string]any) string {
	return rawToken(s, headerOf(s), string(encode(claims)))
}

// testIdentity is an identity of the test issuer and audience, for the
// subjects ops-service and build-bot, whose key set holds the signers' keys.
func testIdentity(t *testing.T, signers ...tokenSigner) *Identity {
	t.Helper()
	var jwks []any
	for _, s := range signers {
		jwks = append(jwks, s.jwk)
	}
	keys, err := parseKeySet(encode(map[string]any{"keys": jwks}))
	must(t, err)
	return &Identity{issuer: testIssuer, audience: testAudience, subjects: map[string]bool{"ops-service": true, "build-bot": true}, keys: keys}
}

// A token signed by a key of each kind the key set reads is taken, and names
// its issuer and subject as the requester.
func TestAnIdentityTokenOfEachKindOfKeyIsTaken(t *testing.T) {
	now := time.Now()
	ed, ec, rs := edSigner(t, "k-ed"), ecSigner(t, "k-ec"), rsaSigner(t, "k-rsa")
	id := testIdentity(t, ed, ec, rs)
	want := requester{issuer: testIssuer, subject: "ops-service"}
	for name, header := range map[string]string{
		"EdDSA":                          "Bearer " + tokenOf(ed, claimsFor("ops-service", now)),
		"ES256":                          "Bearer " + tokenOf(ec, claimsFor("ops-service", now)),
		"RS256":                          "Bearer " + tokenOf(rs, claimsFor("ops-service", now)),
		"the scheme in another case":     "bearer " + tokenOf(ed, claimsFor("ops-service", now)),
		"an audience among others":       "Bearer " + tokenOf(ed, withClaims(claimsFor("ops-service", now), "aud", []string{"elsewhere", testAudience})),
		"no nbf":                         "Bearer " + tokenOf(ed, withClaims(claimsFor("ops-service", now), "nbf", nil)),
		"an expiry passed within 30s":    "Bearer " + tokenOf(ed, withClaims(claimsFor("ops-service", now), "exp", now.Add(-20*time.Second).Unix())),
		"a start still to come in 30s":   "Bearer " + tokenOf(ed, withClaims(claimsFor("ops-service", now), "nbf", now.Add(20*time.Second).Unix())),
		"a header member Runner ignores": "Bearer " + rawToken(ed, `{"alg":"EdDSA","kid":"k-ed","typ":"at+jwt","cty":"x"}`, string(encode(claimsFor("ops-service", now)))),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := id.check([]string{header}, now)
			if err != nil || got != want {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	got, err := id.check([]string{"Bearer " + tokenOf(rs, claimsFor("build-bot", now))}, now)
	if err != nil || got != (requester{issuer: testIssuer, subject: "build-bot"}) {
		t.Fatalf("%+v %v", got, err)
	}
}

// withClaims is claims with name set to v, or removed when v is nil.
func withClaims(claims map[string]any, name string, v any) map[string]any {
	out := map[string]any{}
	for k, value := range claims {
		out[k] = value
	}
	if v == nil {
		delete(out, name)
	} else {
		out[name] = v
	}
	return out
}

// Each refusal of a token is its own reason, and none repeats any part of the
// token: not a segment, the key id or the subject.
func TestAnIdentityTokenIsRefusedForEachReason(t *testing.T) {
	now := time.Now()
	ed, ec := edSigner(t, "kid-marker-ed"), ecSigner(t, "kid-marker-ec")
	id := testIdentity(t, ed, ec)
	stranger := edSigner(t, "kid-marker-ed")
	unknown := edSigner(t, "kid-marker-unknown")
	good := func() map[string]any { return claimsFor("ops-service", now) }
	claims := func(name string, v any) string { return string(encode(withClaims(good(), name, v))) }
	valid := tokenOf(ed, good())
	parts := strings.Split(valid, ".")
	exp, nbf := now.Add(5*time.Minute).Unix(), now.Add(-time.Minute).Unix()
	rawClaims := func(members string) string {
		return fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":"ops-service",%s}`, testIssuer, testAudience, members)
	}
	edHeader := headerOf(ed)
	oversized := tokenOf(ed, withClaims(good(), "pad", strings.Repeat("p", maxTokenBytes)))
	for _, c := range []struct {
		name    string
		headers []string
		want    string
	}{
		{"no Authorization header", nil, "carries no bearer token"},
		{"two Authorization headers", []string{"Bearer " + valid, "Bearer " + valid}, "more than one Authorization header"},
		{"another scheme", []string{"Basic " + valid}, "carries no bearer token"},
		{"the scheme alone", []string{"Bearer "}, "carries no bearer token"},
		{"the owner bearer's shape", []string{"Bearer " + journalBearer}, "not three dot-separated parts"},
		{"a token longer than a token", []string{"Bearer " + oversized}, "longer than 8192 bytes"},
		{"two parts", []string{"Bearer " + parts[0] + "." + parts[1]}, "not three dot-separated parts"},
		{"four parts", []string{"Bearer " + valid + ".x"}, "not three dot-separated parts"},
		{"a padded header", []string{"Bearer " + signedParts(ed, parts[0]+"=", parts[1])}, "header is not canonical base64url"},
		{"a header in standard base64", []string{"Bearer " + signedParts(ed, base64.StdEncoding.EncodeToString([]byte(edHeader+"??")), parts[1])}, "header is not canonical base64url"},
		{"a header with a line break", []string{"Bearer " + signedParts(ed, parts[0][:8]+"\n"+parts[0][8:], parts[1])}, "header is not canonical base64url"},
		{"a payload with a line break, signed as given", []string{"Bearer " + signedParts(ed, parts[0], parts[1][:8]+"\r\n"+parts[1][8:])}, "payload is not canonical base64url"},
		{"a header that is not JSON", []string{"Bearer " + rawToken(ed, "not json", claims("x", nil))}, "header is not one JSON object"},
		{"a header that is an array", []string{"Bearer " + rawToken(ed, `["EdDSA"]`, claims("x", nil))}, "header is not a JSON object"},
		{"a header with alg twice", []string{"Bearer " + rawToken(ed, `{"alg":"none","alg":"EdDSA","kid":"kid-marker-ed"}`, claims("x", nil))}, "header is not one JSON object with each member once"},
		{"a critical extension", []string{"Bearer " + rawToken(ed, `{"alg":"EdDSA","kid":"kid-marker-ed","crit":["exp"],"exp":1}`, claims("x", nil))}, "critical extensions"},
		{"no key id", []string{"Bearer " + rawToken(ed, `{"alg":"EdDSA"}`, claims("x", nil))}, "names no key id"},
		{"a key id that is not a string", []string{"Bearer " + rawToken(ed, `{"alg":"EdDSA","kid":7}`, claims("x", nil))}, "kid is not a string"},
		{"a key id the key set does not hold", []string{"Bearer " + tokenOf(unknown, good())}, "not in the key set"},
		{"alg none", []string{"Bearer " + b64([]byte(`{"alg":"none","kid":"kid-marker-ed"}`)) + "." + parts[1] + "."}, "alg is not its key's, EdDSA"},
		{"alg of another kind of key", []string{"Bearer " + rawToken(ed, `{"alg":"ES256","kid":"kid-marker-ed"}`, claims("x", nil))}, "alg is not its key's, EdDSA"},
		{"alg HS256", []string{"Bearer " + rawToken(ec, `{"alg":"HS256","kid":"kid-marker-ec"}`, claims("x", nil))}, "alg is not its key's, ES256"},
		{"no alg", []string{"Bearer " + rawToken(ed, `{"kid":"kid-marker-ed"}`, claims("x", nil))}, "alg is not its key's"},
		{"an alg that is not a string", []string{"Bearer " + rawToken(ed, `{"alg":["EdDSA"],"kid":"kid-marker-ed"}`, claims("x", nil))}, "alg is not a string"},
		{"a signature changed", []string{"Bearer " + parts[0] + "." + parts[1] + "." + b64(flipped(ed.sign([]byte(parts[0]+"."+parts[1]))))}, "signature does not verify"},
		{"no signature", []string{"Bearer " + parts[0] + "." + parts[1] + "."}, "signature does not verify"},
		{"signed by another key under the same key id", []string{"Bearer " + tokenOf(stranger, good())}, "signature does not verify"},
		{"claims signed by one key, presented with another's signature", []string{"Bearer " + parts[0] + "." + strings.Split(tokenOf(ed, claimsFor("build-bot", now)), ".")[1] + "." + parts[2]}, "signature does not verify"},
		{"an ES256 signature of the wrong length", []string{"Bearer " + signedPartsWith(ec, func(b []byte) []byte { return append(b, 0) }, headerOf(ec), good())}, "signature does not verify"},
		// The same r and s, with s written in 33 bytes: a second spelling of
		// a good signature, which only the length tells apart.
		{"an ES256 signature with s widened by a zero byte", []string{"Bearer " + signedPartsWith(ec, func(b []byte) []byte { return append(append(b[:32:32], 0), b[32:]...) }, headerOf(ec), good())}, "signature does not verify"},
		{"a padded signature", []string{"Bearer " + valid + "=="}, "signature is not canonical base64url"},
		{"a padded payload, signed as given", []string{"Bearer " + signedParts(ed, parts[0], parts[1]+"=")}, "payload is not canonical base64url"},
		{"claims that are not JSON", []string{"Bearer " + rawToken(ed, edHeader, "{")}, "claims is not one JSON object"},
		{"claims that are a string", []string{"Bearer " + rawToken(ed, edHeader, `"ops-service"`)}, "claims is not a JSON object"},
		{"a subject twice", []string{"Bearer " + rawToken(ed, edHeader, rawClaims(fmt.Sprintf(`"sub":"build-bot","exp":%d`, exp)))}, "claims is not one JSON object with each member once"},
		{"claims that are not UTF-8", []string{"Bearer " + rawToken(ed, edHeader, rawClaims(fmt.Sprintf("\"note\":\"\xff\",\"exp\":%d", exp)))}, "in UTF-8"},
		{"an unpaired surrogate in the claims", []string{"Bearer " + rawToken(ed, edHeader, rawClaims(fmt.Sprintf(`"note":"\ud800","exp":%d`, exp)))}, "in UTF-8"},
		{"another issuer", []string{"Bearer " + tokenOf(ed, withClaims(good(), "iss", "https://elsewhere.example"))}, "not from the configured issuer"},
		{"the issuer in another case", []string{"Bearer " + tokenOf(ed, withClaims(good(), "iss", strings.ToUpper(testIssuer)))}, "not from the configured issuer"},
		{"no issuer", []string{"Bearer " + tokenOf(ed, withClaims(good(), "iss", nil))}, "not from the configured issuer"},
		{"an issuer that is not a string", []string{"Bearer " + tokenOf(ed, withClaims(good(), "iss", []string{testIssuer}))}, "iss is not a string"},
		{"another audience", []string{"Bearer " + tokenOf(ed, withClaims(good(), "aud", "elsewhere"))}, "does not name this Runner's audience"},
		{"audiences without this one", []string{"Bearer " + tokenOf(ed, withClaims(good(), "aud", []string{"elsewhere", "other"}))}, "does not name this Runner's audience"},
		{"audiences with one that is not a string", []string{"Bearer " + tokenOf(ed, withClaims(good(), "aud", []any{testAudience, 1}))}, "does not name this Runner's audience"},
		{"no audience", []string{"Bearer " + tokenOf(ed, withClaims(good(), "aud", nil))}, "does not name this Runner's audience"},
		{"an empty list of audiences", []string{"Bearer " + tokenOf(ed, withClaims(good(), "aud", []string{}))}, "does not name this Runner's audience"},
		{"no subject", []string{"Bearer " + tokenOf(ed, withClaims(good(), "sub", nil))}, "names no subject"},
		{"the subject's name in another case", []string{"Bearer " + rawToken(ed, edHeader, fmt.Sprintf(`{"iss":%q,"aud":%q,"SUB":"ops-service","exp":%d}`, testIssuer, testAudience, exp))}, "names no subject"},
		{"a subject that is not a string", []string{"Bearer " + tokenOf(ed, withClaims(good(), "sub", 7))}, "sub is not a string"},
		{"a subject not listed", []string{"Bearer " + tokenOf(ed, withClaims(good(), "sub", "someone-else"))}, "not one the configuration lists"},
		{"a listed subject in another case", []string{"Bearer " + tokenOf(ed, withClaims(good(), "sub", "OPS-SERVICE"))}, "not one the configuration lists"},
		{"no expiry", []string{"Bearer " + tokenOf(ed, withClaims(good(), "exp", nil))}, "has no expiry"},
		{"an expiry 31s past", []string{"Bearer " + tokenOf(ed, withClaims(good(), "exp", now.Add(-31*time.Second).Unix()))}, "has expired"},
		{"an expiry that is a string", []string{"Bearer " + tokenOf(ed, withClaims(good(), "exp", fmt.Sprint(exp)))}, "exp is not an integer"},
		{"an expiry with a fraction", []string{"Bearer " + rawToken(ed, edHeader, rawClaims(fmt.Sprintf(`"exp":%d.5`, exp)))}, "exp is not an integer"},
		{"an expiry with an exponent", []string{"Bearer " + rawToken(ed, edHeader, rawClaims(`"exp":2e9`))}, "exp is not an integer"},
		{"an expiry past what a JSON number holds", []string{"Bearer " + rawToken(ed, edHeader, rawClaims(`"exp":9007199254740992`))}, "exp is not an integer"},
		{"a start 31s to come", []string{"Bearer " + tokenOf(ed, withClaims(good(), "nbf", now.Add(31*time.Second).Unix()))}, "not yet valid"},
		{"a start that is a string", []string{"Bearer " + tokenOf(ed, withClaims(good(), "nbf", fmt.Sprint(nbf)))}, "nbf is not an integer"},
		{"a start far before what a JSON number holds", []string{"Bearer " + rawToken(ed, edHeader, rawClaims(fmt.Sprintf(`"exp":%d,"nbf":-9007199254740992`, exp)))}, "nbf is not an integer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := id.check(c.headers, now)
			if err == nil || !strings.Contains(err.Error(), c.want) || got != (requester{}) {
				t.Fatalf("%+v %v, want a refusal saying %q", got, err, c.want)
			}
			for _, h := range c.headers {
				for _, part := range strings.Split(bearerToken(h), ".") {
					if len(part) >= 8 && strings.Contains(err.Error(), part) {
						t.Fatalf("the refusal repeats the token: %v", err)
					}
				}
			}
			for _, named := range []string{"kid-marker", "ops-service", "build-bot", "someone-else", "elsewhere"} {
				if strings.Contains(err.Error(), named) {
					t.Fatalf("the refusal repeats %s: %v", named, err)
				}
			}
		})
	}
}

// flipped is b with its first bit inverted.
func flipped(b []byte) []byte {
	out := append([]byte{}, b...)
	out[0] ^= 1
	return out
}

// signedPartsWith is a token of header and claims whose signature is s's,
// passed through change.
func signedPartsWith(s tokenSigner, change func([]byte) []byte, header string, claims map[string]any) string {
	signed := b64([]byte(header)) + "." + b64(encode(claims))
	return signed + "." + b64(change(s.sign([]byte(signed))))
}

// A key set is read exactly as written, and every key it holds is one Runner
// can verify with as written: a key it would read only part of, or of a kind
// it does not read, refuses the file.
func TestAKeySetIsRefusedForEachReason(t *testing.T) {
	ed := edSigner(t, "a")
	x := ed.jwk["x"].(string)
	big256 := b64([]byte(strings.Repeat("\xff", 256)))
	jwk := func(members string) string { return `{"keys":[{` + members + `}]}` }
	for _, c := range []struct{ name, set, want string }{
		{"not JSON", `{`, "not one JSON object"},
		{"an array", `[]`, "not a JSON object"},
		{"no keys", `{}`, "has no keys array"},
		{"keys that are not an array", `{"keys":{}}`, "has no keys array"},
		{"no key", `{"keys":[]}`, "holds no key"},
		{"keys twice", `{"keys":[],"keys":[]}`, "each member once"},
		{"a key that is not an object", `{"keys":["a"]}`, "is not an object"},
		{"a key with no kid", jwk(`"kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "has no kid"},
		{"a kid that is not a string", jwk(`"kid":1,"kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "kid is not a string"},
		{"a kid twice", `{"keys":[{"kid":"a","kty":"OKP","crv":"Ed25519","x":"` + x + `"},{"kid":"a","kty":"OKP","crv":"Ed25519","x":"` + x + `"}]}`, "the kid of an earlier key"},
		{"a member twice in a key", jwk(`"kid":"a","kty":"OKP","crv":"Ed25519","x":"` + x + `","x":"` + x + `"`), "each member once"},
		{"a key for encryption", jwk(`"kid":"a","use":"enc","kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "not for signing"},
		{"key operations without verify", jwk(`"kid":"a","key_ops":["sign"],"kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "not for verifying"},
		{"key operations that are not strings", jwk(`"kid":"a","key_ops":["verify",1],"kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "not for verifying"},
		{"key operations that are not a list", jwk(`"kid":"a","key_ops":"verify","kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "not for verifying"},
		{"a symmetric key", jwk(`"kid":"a","kty":"oct","k":"` + x + `"`), "kty other than RSA, EC and OKP"},
		{"no kty", jwk(`"kid":"a","x":"` + x + `"`), "kty other than RSA, EC and OKP"},
		{"an RSA key of 1024 bits", jwk(`"kid":"a","kty":"RSA","n":"` + b64([]byte(strings.Repeat("\xff", 128))) + `","e":"AQAB"`), "under 2048 bits"},
		{"an RSA key with an even exponent", jwk(`"kid":"a","kty":"RSA","n":"` + big256 + `","e":"AQAA"`), "exponent that is even or outside"},
		{"an RSA key with exponent 1", jwk(`"kid":"a","kty":"RSA","n":"` + big256 + `","e":"AQ"`), "exponent that is even or outside"},
		{"an RSA key with an exponent past 2^31-1", jwk(`"kid":"a","kty":"RSA","n":"` + big256 + `","e":"` + b64([]byte{1, 0, 0, 0, 1}) + `"`), "exponent that is even or outside"},
		{"an RSA key with no exponent", jwk(`"kid":"a","kty":"RSA","n":"` + big256 + `"`), "has no e"},
		{"an RSA modulus padded", jwk(`"kid":"a","kty":"RSA","n":"` + big256 + `==","e":"AQAB"`), "its n is not canonical base64url"},
		{"an RSA modulus in standard base64", jwk(`"kid":"a","kty":"RSA","n":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xfb\xff", 128))) + `","e":"AQAB"`), "its n is not canonical base64url"},
		{"an RSA modulus with a line break", jwk(`"kid":"a","kty":"RSA","n":"` + big256[:8] + `\n` + big256[8:] + `","e":"AQAB"`), "its n is not canonical base64url"},
		{"an EC key on P-384", jwk(`"kid":"a","kty":"EC","crv":"P-384","x":"` + x + `","y":"` + x + `"`), "curve other than P-256"},
		{"an EC coordinate of 31 bytes", jwk(`"kid":"a","kty":"EC","crv":"P-256","x":"` + b64(make([]byte, 31)) + `","y":"` + x + `"`), "not 32 bytes"},
		{"an EC point off the curve", jwk(`"kid":"a","kty":"EC","crv":"P-256","x":"` + b64(append(make([]byte, 31), 1)) + `","y":"` + b64(append(make([]byte, 31), 1)) + `"`), "not a point on P-256"},
		{"an OKP key on X448", jwk(`"kid":"a","kty":"OKP","crv":"X448","x":"` + x + `"`), "curve other than Ed25519"},
		{"an OKP key of 31 bytes", jwk(`"kid":"a","kty":"OKP","crv":"Ed25519","x":"` + b64(make([]byte, 31)) + `"`), "not an Ed25519 public key"},
		{"an OKP key with x not a string", jwk(`"kid":"a","kty":"OKP","crv":"Ed25519","x":1`), "x is not a string"},
		{"an alg not the key's kind's", jwk(`"kid":"a","alg":"ES256","kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "names an alg that is not its kind's"},
		{"alg none", jwk(`"kid":"a","alg":"none","kty":"OKP","crv":"Ed25519","x":"` + x + `"`), "names an alg that is not its kind's"},
	} {
		t.Run(c.name, func(t *testing.T) {
			keys, err := parseKeySet([]byte(c.set))
			if err == nil || !strings.Contains(err.Error(), c.want) || keys != nil {
				t.Fatalf("%v %v, want a refusal saying %q", keys, err, c.want)
			}
		})
	}
	ec, rs := ecSigner(t, "b"), rsaSigner(t, "c")
	withMembers := func(s tokenSigner, extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range s.jwk {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	keys, err := parseKeySet(encode(map[string]any{"keys": []any{
		withMembers(ed, map[string]any{"alg": "EdDSA", "use": "sig"}),
		withMembers(ec, map[string]any{"alg": "ES256", "key_ops": []string{"verify"}}),
		withMembers(rs, map[string]any{"alg": "RS256", "x5t": "ignored"}),
	}}))
	if err != nil || len(keys) != 3 || keys["a"].alg != "EdDSA" || keys["b"].alg != "ES256" || keys["c"].alg != "RS256" {
		t.Fatalf("%v %v", keys, err)
	}
}

// An identity member is read exactly as written: issuer, audience, subjects
// and keys, each once, and nothing else. A refusal names the member, never
// what it holds.
func TestAnIdentityConfigurationIsRefusedForEachReason(t *testing.T) {
	member := func(name string, v any) string {
		c := map[string]any{"issuer": testIssuer, "audience": testAudience, "subjects": []string{"ops-service"}, "keys": "/etc/runner/keys.json"}
		if v == nil {
			delete(c, name)
		} else {
			c[name] = v
		}
		return string(encode(c))
	}
	many := make([]string, maxSubjects+1)
	for i := range many {
		many[i] = fmt.Sprint("subject-", i)
	}
	for _, c := range []struct{ name, raw, want string }{
		{"not an object", `["ops-service"]`, "identity is not a JSON object"},
		{"a member twice", `{"issuer":"a","issuer":"b","audience":"c","subjects":["d"],"keys":"/k"}`, "each member once"},
		{"a member Runner does not read", member("algorithms", []string{"EdDSA"}), "a member Runner does not read"},
		{"a member's name in another case", strings.Replace(member("issuer", testIssuer), `"issuer"`, `"Issuer"`, 1), "a member Runner does not read"},
		{"no issuer", member("issuer", nil), "identity.issuer must be"},
		{"an empty issuer", member("issuer", ""), "identity.issuer must be"},
		{"an issuer that is not a string", member("issuer", 1), "identity.issuer must be"},
		{"an issuer with a control character", member("issuer", "https://issuer.example\n"), "identity.issuer must be"},
		{"an issuer past 1024 bytes", member("issuer", strings.Repeat("i", 1025)), "identity.issuer must be"},
		{"no audience", member("audience", nil), "identity.audience must be"},
		{"an audience list", member("audience", []string{testAudience}), "identity.audience must be"},
		{"no subjects", member("subjects", nil), "identity.subjects must list"},
		{"no subject", member("subjects", []string{}), "identity.subjects must list"},
		{"subjects that are a string", member("subjects", "ops-service"), "identity.subjects must list"},
		{"257 subjects", member("subjects", many), "identity.subjects must list"},
		{"a subject that is not a string", member("subjects", []any{"ops-service", 1}), "identity.subjects[1] must be"},
		{"an empty subject", member("subjects", []string{""}), "identity.subjects[0] must be"},
		{"a subject past 256 bytes", member("subjects", []string{strings.Repeat("s", 257)}), "identity.subjects[0] must be"},
		{"a subject with a control character", member("subjects", []string{"ops\tservice"}), "identity.subjects[0] must be"},
		{"a subject twice", member("subjects", []string{"ops-service", "build-bot", "ops-service"}), "identity.subjects[2] lists a subject a second time"},
		{"no keys", member("keys", nil), "identity.keys must be"},
		{"keys at a relative path", member("keys", "keys.json"), "identity.keys must be"},
		{"keys at a path that is not clean", member("keys", "/etc/runner/../runner/keys.json"), "identity.keys must be"},
		{"keys that are not a string", member("keys", []string{"/k"}), "identity.keys must be"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseIdentityConfig([]byte(c.raw))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%+v %v, want a refusal saying %q", got, err, c.want)
			}
			if strings.Contains(err.Error(), "ops-service") || strings.Contains(err.Error(), "issuer.example") {
				t.Fatalf("the refusal repeats a value: %v", err)
			}
		})
	}
	got, err := ParseIdentityConfig([]byte(member("subjects", []string{"ops-service", "build-bot"})))
	if err != nil || got.Issuer != testIssuer || got.Audience != testAudience || got.Keys != "/etc/runner/keys.json" || strings.Join(got.Subjects, ",") != "ops-service,build-bot" {
		t.Fatalf("%+v %v", got, err)
	}
}

// A subject that names what Runner records for requests of its own is
// refused before the key set is read: a caller's runs and idempotency keys
// are never held in the owner's scope, a workflow's or a trigger's.
func TestAnIdentityMayNotListTheNamesRunnerRecordsItself(t *testing.T) {
	base := IdentityConfig{Issuer: testIssuer, Audience: testAudience, Keys: "/nonexistent-key-set-for-a-test.json"}
	for _, c := range []struct {
		name     string
		subjects []string
		want     string
	}{
		{"the owner", []string{"ops-service", "journal-owner"}, "identity.subjects[1] is the installation's owner"},
		{"workflow", []string{"workflow"}, "identity.subjects[0] is a name Runner records for runs it starts itself"},
		{"a trigger's", []string{"trigger:trg_1"}, "identity.subjects[0] is a name Runner records for runs it starts itself"},
		{"a subject twice", []string{"ops-service", "ops-service"}, "identity.subjects[1] must be a subject, listed once"},
		{"no subject", nil, "1 to 256 subjects"},
	} {
		t.Run(c.name, func(t *testing.T) {
			config := base
			config.Subjects = c.subjects
			id, err := LoadIdentity(config, "journal-owner", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), c.want) || id != nil {
				t.Fatalf("%v %v, want a refusal saying %q", id, err, c.want)
			}
		})
	}
}

// The requester a request carries is the installation unless an identity
// token named a caller; a caller's actor and requestedBy are its own.
func TestACallerIsNamedByItsIssuerAndSubject(t *testing.T) {
	s := &Service{cfg: Config{Owner: "journal-owner"}}
	if got := s.actor(requester{}); !sameActor(got, EventActor{Kind: "installation", Owner: "journal-owner"}) || s.requestedBy(requester{}) != "journal-owner" {
		t.Fatalf("%+v %s", got, s.requestedBy(requester{}))
	}
	caller := requester{issuer: testIssuer, subject: "ops-service"}
	if got := s.actor(caller); string(encode(got)) != `{"kind":"caller","issuer":"`+testIssuer+`","subject":"ops-service"}` || s.requestedBy(caller) != "ops-service" {
		t.Fatalf("%s %s", encode(got), s.requestedBy(caller))
	}
	if requesterFrom(t.Context()) != (requester{}) || requesterFrom(withRequester(t.Context(), caller)) != caller {
		t.Fatal("the requester is not carried")
	}
	var decoded map[string]any
	must(t, json.Unmarshal(encode(s.actor(caller)), &decoded))
	if len(decoded) != 3 {
		t.Fatalf("a caller carries %v", decoded)
	}
}
