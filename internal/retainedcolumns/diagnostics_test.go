package retainedcolumns

import "testing"

func TestPinnedDiagnosticMessages(t *testing.T) {
	for _, c := range []struct{ kind, raw, want string }{
		{"decimal", "01", "Use a plain decimal without separators, units, exponents, or leading zeros."},
		{"number", "01", "Use an unambiguous number without separators, units, or leading zeros."},
		{"number", "1e309", "Number is outside the supported precise range."},
		{"number", "0e309", "Number exponent is outside the supported range."},
		{"number", "1.0000000000000001", "This number would lose precision. Keep it as text or revise it."},
		{"boolean", "maybe", "Does not match either declared Yes or No value."},
		{"evidence-availability", "citation", "Evidence availability must be present, absent, or unknown."},
	} {
		rule := map[string]any{"contractVersion": "1", "column": "A"}
		if c.kind != "evidence-availability" {
			rule["kind"] = c.kind
		}
		if c.kind == "boolean" {
			rule["trueValue"] = "YES"
			rule["falseValue"] = "NO"
		}
		p, e := ParsePlan(encodeTest(t, rule))
		if e != nil {
			t.Fatal(e)
		}
		got, e := Preview(p, rawFrame(t, map[string]any{"A": c.raw}))
		if e != nil || len(got.Issues) != 1 || got.Issues[0].Message != c.want {
			t.Fatalf("pinned message mismatch: %+v %v", got, e)
		}
	}
	for _, c := range []struct {
		column               string
		context, declaration bool
		want                 string
	}{
		{"Column", false, false, "Research value requires a typed research field."},
		{"name", true, false, "Record metadata cannot use Research value. Choose another conversion."},
		{"Column", true, false, "The original research field declaration is unavailable."},
		{"Other", true, true, "Choose a declared research field for Research value."},
		{"Column", true, true, "The research value does not match its original field type."},
	} {
		p, e := ParsePlan(encodeTest(t, map[string]any{"contractVersion": "1", "column": c.column, "kind": "research-value"}))
		if e != nil {
			t.Fatal(e)
		}
		g := generated(t, `{"name":"Column","type":"boolean"}`, ptr("null"), "supported")
		if !c.context {
			g = asserted(map[string]any{})
		} else if !c.declaration {
			g["research"].(map[string]any)["declaration"] = nil
		}
		f, e := ParseFrame(encodeTest(t, g), Base)
		if e != nil {
			t.Fatal(e)
		}
		got, e := Preview(p, f)
		if e != nil || len(got.Issues) != 1 || got.Issues[0].Message != c.want {
			t.Fatalf("pinned research message mismatch: %+v %v", got, e)
		}
	}
}
