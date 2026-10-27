package retainedcolumns

import (
	"strconv"
	"strings"
	"testing"
)

func TestExactNumberGuardOrderAndSpelling(t *testing.T) {
	p := plan(t, `{"contractVersion":"1","column":"A","kind":"number"}`)
	for _, c := range []struct {
		raw, code string
		claim     bool
	}{
		{"1.2500", "", false}, {"1E+003", "", false}, {"-0", "", true}, {"-0.000", "", false},
		{"0.00e-308", "", false}, {"9007199254740991.0", "", false},
		{"1424953923781206.2", "", false},
		{"1424953923781206.3", "number-precision", false},
		{"9007199254740991.1", "number-precision", false},
		{"9007199254740991.5", "number-range", false},
		{"1e999", "number-range", false}, {"0e-999", "number-exponent", false},
		{"0e+0000000000000000000000000000000000000309", "number-exponent", false},
		{"0.00000000000000001e-308", "number-precision", false},
		{"+1", "number-encoding", false}, {"1,000", "number-encoding", false}, {"1.", "number-encoding", false},
	} {
		t.Run(c.raw, func(t *testing.T) {
			got, e := Preview(p, rawFrame(t, map[string]any{"A": c.raw}))
			if e != nil {
				t.Fatal(e)
			}
			if c.code != "" {
				if len(got.Issues) != 1 || got.Issues[0].Code != c.code {
					t.Fatalf("diagnostic precedence: got=%+v want=%s", got, c.code)
				}
				return
			}
			if got.ValueJSON == nil || *got.ValueJSON != c.raw || len(got.Issues) != 0 || got.ClaimAdmissible != c.claim {
				t.Fatalf("exact number spelling/domain mismatch: %+v", got)
			}
		})
	}
}
func TestResearchLegacyAndModernRawBounds(t *testing.T) {
	p := plan(t, `{"contractVersion":"1","column":"Column","kind":"research-value"}`)
	for _, c := range []struct {
		kind  string
		raw   string
		valid bool
	}{
		{"string", strings.Repeat(" ", 8190) + `""`, true},
		{"string", strings.Repeat(" ", 8191) + `""`, false},
		{"text", strings.Repeat(" ", 8191) + `""`, true},
		{"text", strings.Repeat(" ", 524286) + `""`, true},
		{"text", strings.Repeat(" ", 524287) + `""`, false},
		{"number", strings.Repeat(" ", 8191) + "0", true},
		{"number", strings.Repeat(" ", 8192) + "0", false},
		{"integer", `9007199254740993`, false},
		{"number", `9007199254740993`, true},
		{"number", `1e309`, false},
	} {
		t.Run(c.kind+"-"+strconv.Itoa(len(c.raw)), func(t *testing.T) {
			field := `{"name":"Column","type":"` + c.kind + `"}`
			f, e := ParseFrame(encodeTest(t, generated(t, field, &c.raw, "supported")), Base)
			if e != nil {
				t.Fatal(e)
			}
			got, e := Preview(p, f)
			if e != nil {
				t.Fatal(e)
			}
			if c.valid {
				if got.ValueJSON == nil || *got.ValueJSON != c.raw || len(got.Issues) != 0 {
					t.Fatal("raw typed bytes changed/refused")
				}
			} else if len(got.Issues) != 1 || got.Issues[0].Code != "research-type" {
				t.Fatal("invalid typed value was used")
			}
		})
	}
}
