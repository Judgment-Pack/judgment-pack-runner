package derivation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVendoredAgreementCorpus(t *testing.T) {
	files, e := filepath.Glob("testdata/corpus/*.json")
	if e != nil || len(files) != 21 {
		t.Fatalf("corpus: %d %v", len(files), e)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, e := os.ReadFile(f)
			if e != nil {
				t.Fatal(e)
			}
			var c struct {
				Rule     json.RawMessage `json:"rule"`
				Artifact json.RawMessage `json:"artifact"`
				Params   json.RawMessage `json:"params"`
				Expected json.RawMessage `json:"expected"`
				Reject   bool            `json:"reject"`
			}
			if e = json.Unmarshal(raw, &c); e != nil {
				t.Fatal(e)
			}
			var ruleFile string
			if json.Unmarshal(c.Rule, &ruleFile) == nil {
				c.Rule, e = os.ReadFile(filepath.Join("testdata", ruleFile))
				if e != nil {
					t.Fatal(e)
				}
			}
			if len(c.Params) == 0 {
				c.Params = []byte(`{}`)
			}
			actual, e := Derive(c.Rule, c.Artifact, c.Params)
			// Corpus rejection cases explicitly use expected: null and reject: true.
			if c.Reject || bytes.Equal(c.Expected, []byte(`null`)) {
				if e == nil {
					t.Fatal("expected rejection")
				}
				return
			}
			expected, ce := Canon(c.Expected)
			if ce != nil {
				t.Fatal(ce)
			}
			if e != nil || !bytes.Equal(actual, expected) {
				t.Fatalf("%v\n%s\nwant %s", e, actual, expected)
			}
		})
	}
}
