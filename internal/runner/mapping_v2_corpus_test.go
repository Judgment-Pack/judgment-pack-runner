package runner

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestGatewayCanonicalAndSignedReceiptCorpus(t *testing.T) {
	b, e := os.ReadFile("testdata/gateway/canon.json")
	if e != nil {
		t.Fatal(e)
	}
	var corpus struct {
		Vectors []struct {
			Note     string `json:"note"`
			Input    string `json:"inputJson"`
			Expected string `json:"expectedHex"`
			Reject   bool   `json:"reject"`
		} `json:"vectors"`
	}
	if e = json.Unmarshal(b, &corpus); e != nil {
		t.Fatal(e)
	}
	for _, v := range corpus.Vectors {
		t.Run(v.Note, func(t *testing.T) {
			got, e := proofCanon([]byte(v.Input))
			if v.Reject {
				if e == nil {
					t.Fatal("invalid domain accepted")
				}
				return
			}
			if e != nil || hex.EncodeToString(got) != v.Expected {
				t.Fatal(e, string(got), v.Expected)
			}
		})
	}
	b, e = os.ReadFile("testdata/gateway/v3-valid-sealed.json")
	if e != nil {
		t.Fatal(e)
	}
	var store struct {
		Files map[string]string `json:"files"`
	}
	_ = json.Unmarshal(b, &store)
	pubRaw, e := os.ReadFile("testdata/gateway/TEST-PUBLIC-KEY")
	if e != nil {
		t.Fatal(e)
	}
	pub, e := hex.DecodeString(strings.TrimSpace(string(pubRaw)))
	if e != nil {
		t.Fatal(e)
	}
	for path, raw := range store.Files {
		if !strings.HasPrefix(path, "receipts/") {
			continue
		}
		v, e := readInputJSON([]byte(raw))
		if e != nil {
			t.Fatal(e)
		}
		r := v.(map[string]any)
		sig, _ := hex.DecodeString(r["signature"].(string))
		delete(r, "signature")
		unsigned := canonTest(t, encode(r))
		if !ed25519.Verify(pub, append([]byte("judgment-pack-gateway/receipt/3:"), unsigned...), sig) {
			t.Fatal("upstream signed receipt did not verify", path)
		}
		artifact := store.Files["artifacts/"+strings.TrimPrefix(r["resultDigest"].(string), "sha256:")]
		if digest(canonTest(t, []byte(artifact))) != r["resultDigest"] {
			t.Fatal("upstream artifact mismatch")
		}
	}
}
