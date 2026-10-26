package retainedcolumns

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestReferenceExactFrameAndClosedInput(t *testing.T) {
	raw := []byte(`{ "contractVersion":"1", "origin":"asserted", "values":{"A":"\t","N":null}, "research":null }`)
	p, e := ParsePlan([]byte(`{"contractVersion":"1","column":"A","kind":"text"}`))
	if e != nil {
		t.Fatal(e)
	}
	f, e := ParseFrame(raw, Base)
	if e != nil {
		t.Fatal(e)
	}
	tr, e := Derive(p, f)
	if e != nil {
		t.Fatal(e)
	}
	if tr.Input.Ref == nil || tr.Input.Ref.FrameSHA256 != digest(raw) || tr.Input.Ref.Location != "values" || tr.Result.Reason != "blank" {
		t.Fatal("exact frame reference not derived")
	}
	input, e := ResolveInput(p, f, tr)
	if e != nil || input == nil || *input != `"\t"` {
		t.Fatalf("raw input resolution: %v %v", input, e)
	}
	for _, change := range []string{"hash", "location", "column", "unresolved"} {
		t.Run(change, func(t *testing.T) {
			changed := *tr
			ref := *tr.Input.Ref
			changed.Input.Ref = &ref
			switch change {
			case "hash":
				ref.FrameSHA256 = "sha256:" + strings.Repeat("0", 64)
			case "location":
				ref.Location = "research-value"
			case "column":
				changed.Column = "N"
			case "unresolved":
				changed.Input = InputState{State: "missing"}
			}
			if _, e := ResolveInput(p, f, &changed); e == nil {
				t.Fatal("mismatched reference resolved")
			}
		})
	}
	f2, e := ParseFrame([]byte(strings.Replace(string(raw), `{ "contractVersion"`, `{"contractVersion"`, 1)), Base)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := ResolveInput(p, f2, tr); e == nil {
		t.Fatal("different raw frame resolved same reference")
	}
	for _, raw := range []string{`{"state":"present","json":"x"}`, `{"state":"missing","ref":null}`, `{"state":"present","ref":{"frameSha256":"sha256:` + strings.Repeat("0", 64) + `","location":"values","extra":0}}`, `{"state":"present","ref":{"frameSha256":"sha256:` + strings.Repeat("A", 64) + `","location":"values"}}`, `{"state":"present","ref":{"frameSha256":"sha256:` + strings.Repeat("0", 64) + `","location":"values","locat\u0069on":"values"}}`} {
		var state InputState
		if json.Unmarshal([]byte(raw), &state) == nil {
			t.Fatalf("open or malformed reference accepted: %s", raw)
		}
	}
}

// These are the new reference rows only. The historical foundation corpus keeps
// its original pin; its prior execution is not attributed to this amendment.
func TestReferencePinnedDraftCases(t *testing.T) {
	raw, e := os.ReadFile("testdata/reference-cases.json")
	if e != nil {
		t.Fatal(e)
	}
	if digest(raw) != "sha256:f5de0701bb989f6a5b606e15d3409e4d5df43fe9227cfdd672e2410ac249b571" {
		t.Fatal("experimental Draft reference corpus changed")
	}
	schema, e := os.ReadFile("testdata/reference-contract.schema.json")
	if e != nil || digest(schema) != "sha256:b5c6d5fe2c4819e76de70070c4e35222e8f5fe19819166d054c0c2fc2359369b" {
		t.Fatal("experimental Draft reference schema changed")
	}
	var corpus struct {
		References []struct {
			ID       string
			Request  json.RawMessage
			Expected json.RawMessage
			WireJSON string
		} `json:"referenceCases"`
	}
	if e = json.Unmarshal(raw, &corpus); e != nil {
		t.Fatal(e)
	}
	count := 0
	for _, row := range corpus.References {
		var req map[string]json.RawMessage
		wire := row.Request
		if row.WireJSON == "" {
			var compact bytes.Buffer
			if err := json.Compact(&compact, wire); err != nil {
				t.Fatal(err)
			}
			wire = compact.Bytes()
		}
		if row.WireJSON != "" {
			wire = []byte(row.WireJSON)
		}
		if e = json.Unmarshal(wire, &req); e != nil {
			t.Fatal(e)
		}
		action := req["lineage"]
		verify := false
		if action == nil {
			action = req["verifyLineage"]
			verify = true
		}
		if action == nil {
			continue
		}
		count++
		t.Run(row.ID, func(t *testing.T) {
			var r struct{ Rule, Frame, Transformation json.RawMessage }
			if e := json.Unmarshal(action, &r); e != nil {
				t.Fatal(e)
			}
			var expected struct{ Refusal string }
			json.Unmarshal(row.Expected, &expected)
			p, e := ParsePlan(r.Rule)
			if e != nil {
				if expected.Refusal == "input-malformed" {
					return
				}
				t.Fatal(e)
			}
			f, e := ParseFrame(r.Frame, Base)
			if e != nil {
				if expected.Refusal == "input-malformed" {
					return
				}
				t.Fatal(e)
			}
			got, e := Derive(p, f)
			if e != nil {
				t.Fatal(e)
			}
			if !verify {
				var want Transformation
				if e = json.Unmarshal(row.Expected, &want); e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(got, &want) {
					t.Fatalf("Draft derivation differs: got=%s expected=%s", encodeTest(t, got), row.Expected)
				}
				return
			}
			var supplied Transformation
			d := json.NewDecoder(bytes.NewReader(r.Transformation))
			d.DisallowUnknownFields()
			valid := d.Decode(&supplied) == nil && reflect.DeepEqual(got, &supplied)
			if valid {
				_, e = ResolveInput(p, f, &supplied)
				valid = e == nil
			}
			if valid != (expected.Refusal == "") {
				t.Fatalf("Draft occurrence consistency differs: valid=%t expected=%s", valid, row.Expected)
			}
		})
	}
	if count != 17 {
		t.Fatalf("unexpected new reference row count: %d", count)
	}
}
