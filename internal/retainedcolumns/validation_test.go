package retainedcolumns

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

func assertRefusal(t *testing.T, err error, want string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Code != want {
		t.Fatalf("expected refusal %s, got %v", want, err)
	}
}
func asserted(values map[string]any) map[string]any {
	return map[string]any{"contractVersion": "1", "origin": "asserted", "values": values, "research": nil}
}
func rawFrame(t *testing.T, values map[string]any) Frame {
	t.Helper()
	f, e := ParseFrame(encodeTest(t, asserted(values)), Base)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func plan(t *testing.T, raw string) Plan {
	t.Helper()
	p, e := ParsePlan([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestClosedPlanRefusals(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `[]`, `{}`, `{"contractVersion":"1","column":"A","kind":"json"}`,
		`{"contractVersion":"2","column":"A","kind":"text"}`, `{"contractVersion":1,"column":"A","kind":"text"}`,
		`{"contractVersion":"1","column":"","kind":"text"}`, `{"contractVersion":"1","column":"A","kind":"text","type":"text"}`,
		`{"contractVersion":"1","column":"A","kind":"text","trueValue":"Y","falseValue":"N"}`,
		`{"contractVersion":"1","column":"A","kind":"boolean"}`,
		`{"contractVersion":"1","column":"A","kind":"boolean","trueValue":"Y","falseValue":"Y"}`,
		`{"contractVersion":"1","column":"A","kind":"boolean","trueValue":"\u0085","falseValue":"N"}`,
		`{"contractVersion":"1","column":"A","kind":"evidence-availability"}`,
		`{"contractVersion":"1","column":"A","column":"B"}`,
		`{"contractVersion":"1","column":"\ud800","kind":"text"}`,
	} {
		t.Run(raw, func(t *testing.T) { _, e := ParsePlan([]byte(raw)); assertRefusal(t, e, "input-malformed") })
	}
	for _, rule := range []map[string]any{
		{"contractVersion": "1", "column": strings.Repeat("é", 81), "kind": "text"},
		{"contractVersion": "1", "column": "A", "kind": "boolean", "trueValue": strings.Repeat("é", 81), "falseValue": "N"},
	} {
		_, e := ParsePlan(encodeTest(t, rule))
		assertRefusal(t, e, "input-over-bound")
	}
	// Every allowed control-byte spelling remains usable; escaping expansion does
	// not turn the 160 UTF-8-byte limit into a serialized-JSON-byte limit.
	_, e := ParsePlan(encodeTest(t, map[string]any{"contractVersion": "1", "column": strings.Repeat("é", 80), "kind": "boolean", "trueValue": strings.Repeat("\x00", 160), "falseValue": strings.Repeat("\x01", 160)}))
	if e != nil {
		t.Fatal(e)
	}
}
func TestClosedFrameRefusals(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `[]`, `{}`, `{"contractVersion":"1","origin":"asserted","values":{},"research":null,"extra":0}`,
		`{"contractVersion":"2","origin":"asserted","values":{},"research":null}`,
		`{"contractVersion":"1","origin":"record","values":{},"research":null}`,
		`{"contractVersion":"1","origin":"asserted","values":[],"research":null}`,
		`{"contractVersion":"1","origin":"asserted","values":{"A":1},"research":null}`,
		`{"contractVersion":"1","origin":"asserted","values":{},"research":{}}`,
		`{"contractVersion":"1","origin":"generated","values":{},"research":null}`,
		`{"contractVersion":"1","origin":"asserted","values":{"A":"x","\u0041":"y"},"research":null}`,
		`{"contractVersion":"1","origin":"asserted","values":{"A":"\udfff"},"research":null}`,
	} {
		t.Run(raw, func(t *testing.T) { _, e := ParseFrame([]byte(raw), Base); assertRefusal(t, e, "input-malformed") })
	}
	_, e := ParseFrame(append([]byte(`{"contractVersion":"1","origin":"asserted","values":{"A":"`), append([]byte{0xff}, []byte(`"},"research":null}`)...)...), Base)
	assertRefusal(t, e, "input-malformed")
	_, e = ParseFrame(encodeTest(t, asserted(nil)), Base)
	assertRefusal(t, e, "input-malformed")
	_, e = ParseFrame(encodeTest(t, asserted(map[string]any{})), Profile("future"))
	assertRefusal(t, e, "input-malformed")
	_, e = Preview(Plan{}, Frame{})
	assertRefusal(t, e, "input-malformed")
}
func TestExplicitProfileByteBounds(t *testing.T) {
	raw := encodeTest(t, asserted(map[string]any{"A": "x"}))
	for _, c := range []struct {
		profile Profile
		max     int
	}{{Base, 2097152}, {Extended, 4194304}} {
		t.Run(string(c.profile), func(t *testing.T) {
			exact := append(bytes.Repeat([]byte(" "), c.max-len(raw)), raw...)
			f, e := ParseFrame(exact, c.profile)
			if e != nil {
				t.Fatalf("exact frame bound refused: %v", e)
			}
			if f.ProjectedFrameSHA256() != digest(exact) {
				t.Fatal("projected identity lost received whitespace")
			}
			_, e = ParseFrame(append(exact, ' '), c.profile)
			assertRefusal(t, e, "input-over-bound")
			if c.profile == Base {
				if _, e = ParseFrame(append(exact, ' '), Extended); e != nil {
					t.Fatal("base overflow within explicit extended profile refused", e)
				}
			}
		})
	}
}
func generated(t *testing.T, fieldJSON string, raw *string, status string) map[string]any {
	t.Helper()
	field := map[string]any{"status": status}
	if raw != nil {
		field["valueJson"] = *raw
	}
	return map[string]any{"contractVersion": "1", "origin": "generated", "values": map[string]any{"Column": "display"}, "research": map[string]any{"recordRecipeRevision": "recipe:7", "declaration": map[string]any{"recipeRevision": "recipe:7", "fields": []any{map[string]any{"name": "Column", "fieldJson": fieldJSON}}}, "fields": map[string]any{"Column": field}}}
}
func TestFixedFrameBudgets(t *testing.T) {
	for _, c := range []struct {
		name   string
		values map[string]any
		ok     bool
	}{
		{"column-string-at", map[string]any{"A": strings.Repeat("é", 8000)}, true},
		{"column-string-over", map[string]any{"A": strings.Repeat("é", 8000) + "a"}, false},
		{"column-name-at", map[string]any{strings.Repeat("é", 80): "x"}, true},
		{"column-name-over", map[string]any{strings.Repeat("é", 80) + "a": "x"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, e := ParseFrame(encodeTest(t, asserted(c.values)), Extended)
			if c.ok {
				if e != nil {
					t.Fatal(e)
				}
			} else {
				assertRefusal(t, e, "input-over-bound")
			}
		})
	}
	for _, n := range []int{1000, 1001} {
		values, fields := map[string]any{}, map[string]any{}
		declarations := []any{}
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("C%d", i)
			values[name] = "x"
			fields[name] = map[string]any{"status": "missing"}
			declarations = append(declarations, map[string]any{"name": name, "fieldJson": string(encodeTest(t, map[string]any{"name": name, "type": "text"}))})
		}
		for _, which := range []string{"values", "fields", "declarations"} {
			t.Run(fmt.Sprintf("%s-%d", which, n), func(t *testing.T) {
				g := generated(t, `{"name":"Column","type":"text"}`, nil, "missing")
				r := g["research"].(map[string]any)
				switch which {
				case "values":
					g["values"] = values
				case "fields":
					r["fields"] = fields
				case "declarations":
					r["declaration"].(map[string]any)["fields"] = declarations
				}
				_, e := ParseFrame(encodeTest(t, g), Extended)
				if n == 1000 {
					if e != nil {
						t.Fatal(e)
					}
				} else {
					assertRefusal(t, e, "input-over-bound")
				}
			})
		}
	}
	for _, n := range []int{16384, 16385} {
		prefix := `{"name":"Column","type":"text","opaque":"`
		suffix := `"}`
		field := prefix + strings.Repeat("a", n-len(prefix)-len(suffix)) + suffix
		_, e := ParseFrame(encodeTest(t, generated(t, field, nil, "missing")), Extended)
		if n == 16384 {
			if e != nil {
				t.Fatal(e)
			}
		} else {
			assertRefusal(t, e, "input-over-bound")
		}
	}
}
func TestOpaqueAndIndependentDepthBudgets(t *testing.T) {
	for _, n := range []int{31, 32} {
		field := `{"name":"Column","type":"text","opaque":` + strings.Repeat("[", n) + "0" + strings.Repeat("]", n) + "}"
		_, e := ParseFrame(encodeTest(t, generated(t, field, nil, "missing")), Extended)
		if n == 31 {
			if e != nil {
				t.Fatal("opaque depth32 refused", e)
			}
		} else {
			assertRefusal(t, e, "input-over-bound")
		}
	}
	for _, n := range []int{32, 33} {
		raw := strings.Repeat("[", n) + "0" + strings.Repeat("]", n)
		if got := ClaimAdmissible([]byte(raw)); got != (n == 32) {
			t.Fatalf("actual Runner depth32 boundary mismatch at %d", n)
		}
	}
	for _, raw := range []string{`{"name":"Column","type":"text","x":{"a":1,"a":2}}`, `{"name":"Column","type":"text","x":"\ud800"}`} {
		_, e := ParseFrame(encodeTest(t, generated(t, raw, nil, "missing")), Base)
		assertRefusal(t, e, "input-malformed")
	}
	// Original opaque members do not become executable policy or case-folded keys.
	if _, e := ParseFrame(encodeTest(t, generated(t, `{"name":"Column","type":"future","A":1,"a":2}`, nil, "missing")), Base); e != nil {
		t.Fatal(e)
	}
}
func TestUnicodeSimpleLowerAndScalarParsing(t *testing.T) {
	if unicode.Version != "15.0.0" {
		t.Fatal("Draft simple-lower table requires Unicode15.0")
	}
	for _, raw := range []string{`{"mİn":1,"min":2}`, `{"K":1,"k":2}`, `{"𐐀":1,"𐐨":2}`} {
		_, e := parseJSON([]byte(raw), 64, true, true)
		assertRefusal(t, e, "input-malformed")
	}
	for _, raw := range []string{`{"ß":1,"SS":2}`, `{"İ":1,"i̇":2}`, `"\ud83d\ude00"`, `"\\ud800"`} {
		if _, e := parseJSON([]byte(raw), 64, true, true); e != nil {
			t.Fatal("simple lowercase/scalar mismatch", raw, e)
		}
	}
	for _, raw := range []string{`"\ud800"`, `"\udfff"`, `"\ud800x"`, `"\ud800\u0041"`, `{"A":1,"\u0041":2}`} {
		_, e := parseJSON([]byte(raw), 64, false, true)
		assertRefusal(t, e, "input-malformed")
	}
}
func TestClaimDomainReusesRunnerCanon(t *testing.T) {
	for _, raw := range []string{`-0`, `0`, `9007199254740991`, `-9007199254740991`, `false`, `""`, `[]`, `{"min":-0,"max":0}`, `"\ud83d\ude00"`} {
		if !ClaimAdmissible([]byte(raw)) {
			t.Fatalf("existing canon value refused: %s", raw)
		}
	}
	for _, raw := range []string{`1.25`, `1.0`, `1e3`, `9007199254740992`, `9007199254740993`, `"\ud800"`, `{"a":1,"a":2}`, `[] true`} {
		if ClaimAdmissible([]byte(raw)) {
			t.Fatalf("existing canon refusal admitted: %s", raw)
		}
	}
}
func TestClaimDomainByteBound(t *testing.T) {
	exact := append(bytes.Repeat([]byte(" "), (2<<20)-1), '0')
	if !ClaimAdmissible(exact) {
		t.Fatal("actual Runner MaxBody boundary refused")
	}
	if ClaimAdmissible(append(exact, ' ')) {
		t.Fatal("actual Runner MaxBody overflow admitted")
	}
}
func TestOrdinaryJSONAndInputStateIdentity(t *testing.T) {
	p := plan(t, `{"contractVersion":"1","column":"A","kind":"text"}`)
	const exact = `"<>&/é \n\u0000"`
	f := rawFrame(t, map[string]any{"A": "<>&/é \n\x00"})
	r, e := Derive(p, f)
	if e != nil {
		t.Fatal(e)
	}
	resolved, e := ResolveInput(p, f, r)
	if e != nil || resolved == nil || *resolved != exact || r.Result.JSON == nil || *r.Result.JSON != exact {
		t.Fatalf("ordinary compact UTF8 spelling changed: %+v", r)
	}
	states := []struct {
		values map[string]any
		input  *string
		reason string
	}{{map[string]any{}, nil, "missing"}, {map[string]any{"A": nil}, ptr("null"), "legacy-null"}, {map[string]any{"A": ""}, ptr(`""`), "blank"}}
	for _, c := range states {
		frame := rawFrame(t, c.values)
		l, e := Derive(p, frame)
		if e != nil {
			t.Fatal(e)
		}
		resolved, e := ResolveInput(p, frame, l)
		if e != nil || !reflect.DeepEqual(resolved, c.input) || l.Result.Reason != c.reason {
			t.Fatalf("input state collapsed: %+v", l)
		}
	}
}
func ptr(s string) *string { return &s }
func TestOriginalRevisionRawSpellingAndFieldIdentity(t *testing.T) {
	p := plan(t, `{"contractVersion":"1","column":"Column","kind":"research-value"}`)
	const field = `{ "name": "Column", "type": "number", "description": "original" }`
	source := generated(t, field, ptr("  -0\n"), "interpretation")
	raw := encodeTest(t, source)
	f, e := ParseFrame(raw, Base)
	if e != nil {
		t.Fatal(e)
	}
	if f.Origin() != "generated" || f.ProjectedFrameSHA256() != digest(raw) {
		t.Fatal("projected source identity mislabeled")
	}
	l, e := Derive(p, f)
	if e != nil {
		t.Fatal(e)
	}
	resolved, e := ResolveInput(p, f, l)
	if e != nil || resolved == nil || *resolved != "  -0\n" || l.Result.JSON == nil || *l.Result.JSON != "  -0\n" {
		t.Fatal("original raw research bytes normalized")
	}
	if l.ResearchIdentity.RecipeRevision != "recipe:7" || l.ResearchIdentity.FieldSHA256 != "sha256:f3a6d7a9efa42fae90af0220ba0d11a233911cdcd0c3ff8409b5962a4b623cd3" {
		t.Fatalf("original field identity differs: %+v", l.ResearchIdentity)
	}
	r := source["research"].(map[string]any)
	r["declaration"].(map[string]any)["recipeRevision"] = "recipe:8"
	f, e = ParseFrame(encodeTest(t, source), Base)
	if e != nil {
		t.Fatal(e)
	}
	result, e := Preview(p, f)
	if e != nil || len(result.Issues) != 1 || result.Issues[0].Code != "research-declaration" {
		t.Fatalf("current declaration substituted: %+v %v", result, e)
	}
	l, e = Derive(p, f)
	if l != nil {
		t.Fatal("mismatched original revision fabricated success")
	}
	assertRefusal(t, e, "read-failed")
}
func TestResearchMalformedRawIsSelectedIssue(t *testing.T) {
	p := plan(t, `{"contractVersion":"1","column":"Column","kind":"research-value"}`)
	for _, raw := range []string{"", `null`, `{`, `"ok" true`, strings.Repeat(" ", 524289)} {
		f, e := ParseFrame(encodeTest(t, generated(t, `{"name":"Column","type":"text"}`, &raw, "supported")), Base)
		if e != nil {
			t.Fatal("inspectable raw frame refused", e)
		}
		r, e := Preview(p, f)
		if e != nil || len(r.Issues) != 1 || r.Issues[0].Code != "research-type" {
			t.Fatal("malformed raw did not give selected issue", r, e)
		}
	}
}
func TestGeneratedStatusAndDiagnosticPrecedence(t *testing.T) {
	p := plan(t, `{"contractVersion":"1","column":"Column","kind":"text"}`)
	researchPlan := plan(t, `{"contractVersion":"1","column":"Column","kind":"research-value"}`)
	for _, status := range []string{"supported", "interpretation", "missing", "conflicting", "inaccessible", "stale"} {
		g := generated(t, `{"name":"Column","type":"text"}`, ptr(`"typed"`), status)
		f, e := ParseFrame(encodeTest(t, g), Base)
		if e != nil {
			t.Fatal(e)
		}
		r, e := Preview(p, f)
		if e != nil {
			t.Fatal(e)
		}
		rr, e := Preview(researchPlan, f)
		if e != nil {
			t.Fatal(e)
		}
		available := status == "supported" || status == "interpretation"
		if available {
			if r.ValueJSON == nil || *r.ValueJSON != `"display"` || rr.ValueJSON == nil || *rr.ValueJSON != `"typed"` {
				t.Fatal("resolved generated values lost")
			}
		} else if r.Omitted != "unavailable" || rr.Omitted != "unavailable" {
			t.Fatal("unresolved generated values used")
		}
	}
	// Ordinary missing/null/blank precede unavailable; research declaration refusal
	// precedes unavailable status and never consults display fallback values.
	g := generated(t, `{"name":"Column","type":"text"}`, ptr(`"typed"`), "stale")
	g["values"] = map[string]any{"Column": " "}
	g["research"].(map[string]any)["declaration"] = nil
	f, e := ParseFrame(encodeTest(t, g), Base)
	if e != nil {
		t.Fatal(e)
	}
	r, _ := Preview(p, f)
	rr, _ := Preview(researchPlan, f)
	if r.Omitted != "blank" || len(rr.Issues) != 1 || rr.Issues[0].Code != "research-declaration" {
		t.Fatal("diagnostic/filter order changed")
	}
}

func TestExactLegacyWhitespaceSet(t *testing.T) {
	p := plan(t, `{"contractVersion":"1","column":"A","kind":"text"}`)
	for _, r := range "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000" {
		out, e := Preview(p, rawFrame(t, map[string]any{"A": string(r)}))
		if e != nil || out.Omitted != "blank" {
			t.Fatalf("declared whitespace U+%04X was not blank", r)
		}
	}
	for _, r := range "\x00\x1c\u200b\ufeff" {
		out, e := Preview(p, rawFrame(t, map[string]any{"A": string(r)}))
		if e != nil || out.ValueJSON == nil || out.Omitted != "" {
			t.Fatalf("additional host whitespace U+%04X was omitted", r)
		}
	}
}
func TestAllMetadataNamesAndExactRangeOrder(t *testing.T) {
	for _, name := range []string{"name", "url", "website", "company_id", "company_name", "company_url"} {
		p, e := ParsePlan(encodeTest(t, map[string]any{"contractVersion": "1", "column": name, "kind": "research-value"}))
		if e != nil {
			t.Fatal(e)
		}
		f, e := ParseFrame(encodeTest(t, generated(t, `{"name":"Column","type":"boolean"}`, ptr("false"), "supported")), Base)
		if e != nil {
			t.Fatal(e)
		}
		out, e := Preview(p, f)
		if e != nil || len(out.Issues) != 1 || out.Issues[0].Code != "research-metadata" {
			t.Fatal("metadata exclusion order changed", name)
		}
	}
	p := plan(t, `{"contractVersion":"1","column":"Column","kind":"research-value"}`)
	raw := `{"min":"1.000000000000000000000000000000000000001","max":"1.000000000000000000000000000000000000000"}`
	f, e := ParseFrame(encodeTest(t, generated(t, `{"name":"Column","type":"range"}`, &raw, "supported")), Base)
	if e != nil {
		t.Fatal(e)
	}
	out, e := Preview(p, f)
	if e != nil || len(out.Issues) != 1 || out.Issues[0].Code != "research-type" {
		t.Fatal("exact range order was rounded")
	}
}
