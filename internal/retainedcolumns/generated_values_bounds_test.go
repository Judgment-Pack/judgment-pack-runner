package retainedcolumns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func boundedOriginFrame(t *testing.T, origin, value string) []byte {
	t.Helper()
	var research any
	if origin == "generated" {
		research = map[string]any{"recordRecipeRevision": "original:1", "declaration": nil, "fields": map[string]any{}}
	}
	raw, e := json.Marshal(map[string]any{"contractVersion": "1", "origin": origin, "values": map[string]any{"X": value}, "research": research})
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

// These are frame-only ceilings, independent of whether a complete original
// snapshot can contain the same cell together with required source metadata.
func TestGeneratedRawValueOriginAndUTF8Boundaries(t *testing.T) {
	for _, profile := range []Profile{Base, Extended} {
		for _, origin := range []string{"asserted", "generated"} {
			limit := 16000
			if origin == "generated" {
				limit = 524288
			}
			for _, unit := range []string{"a", "é", "😀"} {
				for _, delta := range []int{0, 1} {
					t.Run(fmt.Sprintf("%s/%s/%d-byte-unit/%d", profile, origin, len(unit), delta), func(t *testing.T) {
						value := strings.Repeat(unit, limit/len(unit)) + strings.Repeat("a", delta)
						if len(value) != limit+delta {
							t.Fatal("handwritten UTF8 byte threshold")
						}
						frame, e := ParseFrame(boundedOriginFrame(t, origin, value), profile)
						if delta == 1 {
							assertRefusal(t, e, "input-over-bound")
							return
						}
						if e != nil {
							t.Fatalf("exact %s raw-value byte boundary rejected: %v", origin, e)
						}
						if !frame.valid || frame.values["X"] != value {
							t.Fatal("exact raw value changed")
						}
					})
				}
			}
		}
	}
	// JSON escape spelling does not change decoded UTF8 cell charging.
	raw := boundedOriginFrame(t, "asserted", strings.Repeat("é", 8000))
	raw = bytes.ReplaceAll(raw, []byte("é"), []byte(`\u00e9`))
	if _, e := ParseFrame(raw, Base); e != nil {
		t.Fatal("escaped exact asserted value", e)
	}
	raw = bytes.Replace(raw, []byte(`\u00e9"`), []byte(`\u00e9a"`), 1)
	if _, e := ParseFrame(raw, Base); e == nil {
		t.Fatal("escaped one-over asserted cell accepted")
	}
	// The old generated counterexample is valid without selecting its large cell.
	list := `["` + strings.Repeat("x", 1000) + `"` + strings.Repeat(`,"`+strings.Repeat("x", 1000)+`"`, 15) + `]`
	if len(list) != 16049 {
		t.Fatal("handwritten sixteen-item list size", len(list))
	}
	if _, e := ParseFrame(boundedOriginFrame(t, "generated", list), Base); e != nil {
		t.Fatal("old-domain generated16049B raw value rejected", e)
	}
	if _, e := ParseFrame(boundedOriginFrame(t, "asserted", list), Base); e == nil {
		t.Fatal("generated amendment enlarged asserted raw cells")
	}
}

func TestGeneratedRawValueIndependentAggregateAndCountBounds(t *testing.T) {
	for _, profile := range []Profile{Base, Extended} {
		limit := 2097152
		if profile == Extended {
			limit = 4194304
		}
		raw := boundedOriginFrame(t, "generated", strings.Repeat("x", 16049))
		exact := append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), limit-len(raw))...)
		if _, e := ParseFrame(exact, profile); e != nil {
			t.Fatal("exact whole frame refused", profile, e)
		}
		if _, e := ParseFrame(append(exact, ' '), profile); e == nil {
			t.Fatal("whole frame ceiling bypassed by generated amendment", profile)
		}
	}
	// Valid individual cells cannot bypass the independent1000-entry work bound.
	frame := map[string]any{"contractVersion": "1", "origin": "generated", "research": map[string]any{"recordRecipeRevision": "r", "declaration": nil, "fields": map[string]any{}}}
	values := map[string]any{}
	for i := 0; i < 1000; i++ {
		values[fmt.Sprint(i)] = "x"
	}
	frame["values"] = values
	if _, e := ParseFrame(encodeTest(t, frame), Base); e != nil {
		t.Fatal("exact generated count refused", e)
	}
	values["one-more"] = "x"
	if _, e := ParseFrame(encodeTest(t, frame), Base); e == nil {
		t.Fatal("generated raw allowance enlarged entry count")
	}
}
