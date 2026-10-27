package runner

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const columnGeneratedFrame = `{ "contractVersion":"1", "origin":"generated", "values":{"X":"original"}, "research":{"recordRecipeRevision":"original:1","declaration":{"recipeRevision":"original:1","fields":[{"name":"X","fieldJson":"{\"name\":\"X\",\"type\":\"number\"}"}]},"fields":{"X":{"status":"supported","valueJson":" -0 "}}}}`

func carrierBytes(snapshot string) []byte {
	// Handwritten wrapper preserves deliberately noncanonical frame whitespace.
	return []byte(`{ "contractVersion":"1", "originalSnapshotJson":` + string(encode(snapshot)) + `, "originalSnapshotSha256":` + string(encode(digest([]byte(snapshot)))) + `, "retained":` + columnGeneratedFrame + ` }`)
}
func TestColumnCarrierExactRestorationAndDomains(t *testing.T) {
	snapshot := "{ \"number\":-0, \"historical\":\"\\ud800\", \"slash\":\"\\/\", \"inventory\":[{\"name\":\"X\",\"type\":\"number\"}] }\n"
	raw := carrierBytes(snapshot)
	c, e := readColumnCarrier(raw)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(c.OriginalSnapshot, []byte(snapshot)) || string(c.RawFrame) != columnGeneratedFrame {
		t.Fatal("original snapshot or received frame bytes rewritten")
	}
	if c.OriginalSnapshotSHA256 != digest([]byte(snapshot)) || c.CarrierSHA256 != digest(raw) || c.Frame.ProjectedFrameSHA256() != digest([]byte(columnGeneratedFrame)) {
		t.Fatal("identity domains conflated")
	}
	canonical, e := proofCanon(raw)
	if e != nil {
		t.Fatal(e)
	}
	if digest(canonical) == c.CarrierSHA256 {
		t.Fatal("raw carrier digest mislabeled canonical")
	}
}
func TestColumnCarrierClosedAndDuplicateSafe(t *testing.T) {
	raw := carrierBytes(`{"x":1}`)
	cases := map[string][]byte{
		"digest":             bytes.Replace(raw, []byte(digest([]byte(`{"x":1}`))), []byte("sha256:"+strings.Repeat("0", 64)), 1),
		"unknown":            bytes.Replace(raw, []byte(`{ "contractVersion"`), []byte(`{ "extra":false,"contractVersion"`), 1),
		"duplicate":          bytes.Replace(raw, []byte(`{ "contractVersion"`), []byte(`{ "contractVersion":"1","contract\u0056ersion"`), 1),
		"version":            bytes.Replace(raw, []byte(`"contractVersion":"1"`), []byte(`"contractVersion":"2"`), 1),
		"nongenerated":       bytes.ReplaceAll(raw, []byte(`"origin":"generated"`), []byte(`"origin":"asserted"`)),
		"duplicate-snapshot": carrierBytes(`{"x":1,"\u0078":2}`),
		"nonobject":          carrierBytes(`[]`),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := readColumnCarrier(b); e == nil {
				t.Fatalf("malformed generated carrier admitted: %s", name)
			}
		})
	}
	if !snapshotObject([]byte(`{"\ud800":1,"\ud801":2}`)) {
		t.Fatal("distinct legacy member names collapsed")
	}
	if snapshotObject([]byte(`{"\ud800":1,"\ud800":2}`)) {
		t.Fatal("duplicate legacy member names admitted")
	}
	if !snapshotObject([]byte(`{"😀":1,"other":2}`)) || snapshotObject([]byte(`{"😀":1,"\ud83d\ude00":2}`)) {
		t.Fatal("decoded supplementary member identity differs")
	}
}
func TestColumnVerifiedResultPreservesRawAndCanonicalSignatureDomain(t *testing.T) {
	p, key := testProfile(t)
	p.Class = "generated"
	at := time.Unix(1700000000, 0).UTC()
	args := []byte(`{"tool":"lookup","arguments":{}}`)
	result := carrierBytes(`{"number":-0,"historical":"\ud800"}`)
	response := signResponse(t, p, key, args, result, at, 0)
	// encoding/json compacts RawMessage when it writes the response. Restore the
	// actual received serialization; the canonical signed result is identical.
	var parts map[string]json.RawMessage
	json.Unmarshal(response, &parts)
	raw := []byte(`{"result":` + string(result) + `,"receipt":` + string(parts["receipt"]) + `,"salts":` + string(parts["salts"]) + `}`)
	public, _ := hex.DecodeString(p.PublicKey)
	verified, cite, e := verifyRetainedAcquisition(raw, args, p, public, at, 300)
	if e != nil || cite == nil {
		t.Fatalf("signed actual acquisition: %v", e)
	}
	if !bytes.Equal(verified.Raw, result) || !bytes.Equal(verified.Canonical, canonTest(t, result)) {
		t.Fatal("verified result lost raw frame or changed canonical signature domain")
	}
	c, e := readColumnCarrier(verified.Raw)
	if e != nil || string(c.RawFrame) != columnGeneratedFrame {
		t.Fatalf("signed raw carrier not restored: %v", e)
	}
	changed := bytes.Replace(raw, []byte(`"values":{"X":"original"}`), []byte(`"values":{"X":"changed"}`), 1)
	if _, _, e = verifyRetainedAcquisition(changed, args, p, public, at, 300); e == nil {
		t.Fatal("changed signed wrapper accepted")
	}
}
