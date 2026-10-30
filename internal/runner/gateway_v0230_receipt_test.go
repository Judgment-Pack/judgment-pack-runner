package runner

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// descriptorProvenance is a gateway v0.23.0 acquisition's optional reference
// to its retained descriptor snapshot and signed statement (gateway SPEC.md
// section 9): three strings, two of them sha256 digests and the binding a
// name@sha256 reference. The values are written by hand; Runner holds neither
// artifact, so nothing here names a real snapshot.
func descriptorProvenance() map[string]any {
	return map[string]any{
		"binding":   "customer@sha256:" + strings.Repeat("1", 64),
		"snapshot":  "sha256:" + strings.Repeat("2", 64),
		"statement": "sha256:" + strings.Repeat("3", 64),
	}
}

// withProvenance returns the fixture's response with descriptorProvenance in
// its acquisition, signed again by the profile's key, as a v0.23.0 gateway
// signs an MCP acquisition whose operation names a descriptor statement.
func withProvenance(t *testing.T, i Input, key ed25519.PrivateKey) (Input, map[string]any) {
	t.Helper()
	i = cloneInput(i)
	v, e := readInputJSON(i.Source.Sources["vendor"].Response)
	if e != nil {
		t.Fatal(e)
	}
	r := v.(map[string]any)
	receipt := r["receipt"].(map[string]any)
	receipt["acquisition"].(map[string]any)["descriptorProvenance"] = descriptorProvenance()
	signReceipt(t, receipt, key)
	i.Source.Sources["vendor"] = SourceValue{Response: encode(r)}
	return i, r
}

// A receipt that carries the gateway v0.23.0 acquisition member
// descriptorProvenance is verified as one without it is: the same facts,
// evidence and citation, the citation naming the receipt's own signature
// (runner #192). The member is signed, so altering or removing it after
// signing fails the signature. Runner verifies a single retained receipt
// and holds neither the snapshot nor the statement, so it accepts the member
// without establishing section 9's property and keeps the response as signed.
func TestAGatewayV0230ReceiptWithDescriptorProvenanceIsVerified(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	earlier, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	current, r := withProvenance(t, input, key)
	got, e := normalizeV2(current, []InputProfile{p}, at)
	if e != nil {
		t.Fatal("a receipt carrying descriptorProvenance was refused:", e)
	}
	if string(got.Facts) != string(earlier.Facts) || string(got.Evidence) != string(earlier.Evidence) {
		t.Fatal("the member changed what was read", string(got.Facts), string(got.Evidence))
	}
	signature := r["receipt"].(map[string]any)["signature"].(string)
	if len(got.Preparation.Cites) != 1 || got.Preparation.Cites[0] != (Citation{"mapping.session", 0, signature}) {
		t.Fatal("citation", got.Preparation.Cites)
	}
	if !bytes.Contains(got.Source.Sources["vendor"].Response, []byte(`"descriptorProvenance"`)) {
		t.Fatal("the retained response lost the signed member")
	}
	again, e := normalizeV2(got, []InputProfile{p}, at)
	if e != nil || !sameJSON(encode(again), encode(got)) {
		t.Fatal("not repeatable", e)
	}
	unsigned := map[string]func(map[string]any){
		"binding-altered": func(a map[string]any) {
			a["descriptorProvenance"].(map[string]any)["binding"] = "customer@sha256:" + strings.Repeat("4", 64)
		},
		"statement-altered": func(a map[string]any) {
			a["descriptorProvenance"].(map[string]any)["statement"] = "sha256:" + strings.Repeat("4", 64)
		},
		"removed": func(a map[string]any) { delete(a, "descriptorProvenance") },
	}
	for name, change := range unsigned {
		t.Run(name, func(t *testing.T) {
			i, r := withProvenance(t, input, key)
			change(r["receipt"].(map[string]any)["acquisition"].(map[string]any))
			i.Source.Sources["vendor"] = SourceValue{Response: encode(r)}
			_, e := normalizeV2(i, []InputProfile{p}, at)
			if e == nil || !strings.Contains(e.Error(), "receipt signature failed") {
				t.Fatal("an unsigned change to descriptorProvenance was not a signature failure:", e)
			}
		})
	}
}

// The gateway's own v0.23.0 corpus vector for section 9 (copied unchanged,
// testdata/gateway/NOTICE.md) is a signed receipt-v3 acquisition carrying
// descriptorProvenance. Its signature verifies under Runner's canonical form
// and signing domain with the corpus key, and the receipt passes Runner's
// form checks through to its arguments commitment: the vector's commitment is
// a stand-in with no salt published, so that check, which follows the
// signature, is the first one it fails.
func TestTheGatewayV0230DescriptorProvenanceVectorVerifies(t *testing.T) {
	b, e := os.ReadFile("testdata/gateway/v3-descriptor-valid.json")
	if e != nil {
		t.Fatal(e)
	}
	var store struct {
		Authority string            `json:"authority"`
		Files     map[string]string `json:"files"`
		Expected  struct {
			OK bool `json:"ok"`
		} `json:"expected"`
	}
	if e = json.Unmarshal(b, &store); e != nil || !store.Expected.OK {
		t.Fatal("not the valid section 9 vector", e)
	}
	pubRaw, e := os.ReadFile("testdata/gateway/TEST-PUBLIC-KEY")
	if e != nil {
		t.Fatal(e)
	}
	public := strings.TrimSpace(string(pubRaw))
	pub, e := hex.DecodeString(public)
	if e != nil {
		t.Fatal(e)
	}
	raw := store.Files["receipts/s1/0.json"]
	v, e := readInputJSON([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	r := v.(map[string]any)
	a := r["acquisition"].(map[string]any)
	if _, ok := a["descriptorProvenance"].(map[string]any); !ok {
		t.Fatal("the vector carries no descriptorProvenance")
	}
	sig, _ := hex.DecodeString(r["signature"].(string))
	delete(r, "signature")
	if !ed25519.Verify(pub, append([]byte("judgment-pack-gateway/receipt/3:"), canonTest(t, encode(r))...), sig) {
		t.Fatal("the upstream section 9 receipt did not verify")
	}
	result := store.Files["artifacts/"+strings.TrimPrefix(r["resultDigest"].(string), "sha256:")]
	adapter := a["adapter"].(map[string]any)
	profile := InputProfile{ID: "tickets", PublicKey: public, Class: "record", Source: r["source"].(string), Authority: store.Authority, Shape: "mcp",
		Adapter: AdapterPin{adapter["name"].(string), adapter["version"].(string), adapter["digest"].(string)}, Tools: []string{"query"}}
	response := encode(map[string]any{"receipt": json.RawMessage(raw), "result": json.RawMessage(result), "salts": map[string]string{"args": strings.Repeat("0", 64)}})
	at, _ := receiptInstant(a["observedAt"].(string))
	_, _, e = verifyAcquisition(response, []byte(`{"tool":"query","arguments":{}}`), profile, ed25519.PublicKey(pub), at, 300)
	if e == nil || !strings.HasPrefix(e.Error(), "response does not match the frozen request") {
		t.Fatal("the vector did not pass every check before its stand-in arguments commitment:", e)
	}
}

// A receipt as gateway v0.9.1 wrote it, with no descriptorProvenance, keeps
// its verdict under the v0.23.0 pin: the fixture's facts, evidence and
// citation, as before (runner #192). The earlier signed corpus is unchanged
// upstream and still verifies (TestGatewayCanonicalAndSignedReceiptCorpus).
func TestAnEarlierGatewayReceiptVerifiesUnchanged(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	v, e := readInputJSON(input.Source.Sources["vendor"].Response)
	if e != nil {
		t.Fatal(e)
	}
	receipt := v.(map[string]any)["receipt"].(map[string]any)
	if _, ok := receipt["acquisition"].(map[string]any)["descriptorProvenance"]; ok {
		t.Fatal("the earlier fixture carries the v0.23.0 member")
	}
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	if string(got.Facts) != `{"vendor":{"description":"a verified vendor"}}` || string(got.Evidence) != `{"registry":"present"}` {
		t.Fatal(string(got.Facts), string(got.Evidence))
	}
	if len(got.Preparation.Cites) != 1 || got.Preparation.Cites[0] != (Citation{"mapping.session", 0, receipt["signature"].(string)}) {
		t.Fatal("citation", got.Preparation.Cites)
	}
}

// descriptorProvenance is a member of an MCP acquisition only: on an
// acquisition of another shape gateway SPEC.md section 9 calls it malformed,
// and Runner refuses the receipt as acquisition-invalid, as it refuses a
// paged acquisition, whoever prepares it, though the gateway's key signed it
// (runner #194, from #195). The same member on an MCP acquisition reads.
func TestDescriptorProvenanceIsRefusedOnANonMCPAcquisition(t *testing.T) {
	verificationCodeOf := func(err error) string { _, code := verificationOutcome(err); return code }
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	carry := func(receipt map[string]any) {
		receipt["acquisition"].(map[string]any)["descriptorProvenance"] = descriptorProvenance()
	}
	for _, shape := range []string{"http", "airbyte"} {
		t.Run(shape, func(t *testing.T) {
			input, p := shapedFixture(t, shape, public, key, at)
			if err := preparedByEachCaller(t, input, p, at); err != nil {
				t.Fatal("the receipt without the member does not read:", err)
			}
			resign(t, &input, key, carry)
			err := preparedByEachCaller(t, input, p, at)
			var problem *apiError
			if err == nil || !errors.As(err, &problem) || problem.Message != "Source vendor: descriptor provenance is valid on an MCP acquisition only. No decision was requested." || verificationCodeOf(err) != codeInvalid {
				t.Fatalf("a %s acquisition carrying descriptorProvenance was not refused as malformed: %v", shape, err)
			}
		})
	}
	t.Run("mcp", func(t *testing.T) {
		input, p := shapedFixture(t, "mcp", public, key, at)
		resign(t, &input, key, carry)
		if err := preparedByEachCaller(t, input, p, at); err != nil {
			t.Fatal("an MCP acquisition carrying descriptorProvenance was refused:", err)
		}
	})
}
