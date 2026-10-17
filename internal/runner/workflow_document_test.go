package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func workflowExample() []byte {
	return []byte(`{"workflowVersion":1,"stages":[
{"id":"companies","dialect":{"version":3,"digest":"sha256:` + strings.Repeat("1", 64) + `"},"pack":{"id":"https://example.invalid/company","digest":"sha256:` + strings.Repeat("2", 64) + `"},"input":{"root":true},"execution":"once","caseBindings":[],"outputs":{"pointer":"/companies","cardinality":"many"},"relation":{"label":"discovered","direction":"output-to-parent"},"selection":{"mode":"reviewer"}},
{"id":"people","dialect":{"version":3,"digest":"sha256:` + strings.Repeat("3", 64) + `"},"pack":{"id":"urn:example:people","digest":"sha256:` + strings.Repeat("4", 64) + `"},"input":{"fromStage":"companies","selected":true},"execution":"each-selected-record","caseBindings":[{"from":"/name","to":"/company/name","type":"string","required":true}],"outputs":{"pointer":"/people","cardinality":"many"},"relation":{"label":"at-company","direction":"output-to-parent"}}],"result":"people","limits":{"maxStages":2,"maxRecords":5,"maxInstances":3,"maxAttempts":1,"maxBytes":1048576}}`)
}
func TestWorkflowDocumentAndLiteralDomains(t *testing.T) {
	d, canonical, err := readWorkflow(workflowExample())
	if err != nil || len(d.Stages) != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	_, again, err := readWorkflow(canonical)
	if err != nil || !bytes.Equal(canonical, again) {
		t.Fatalf("canonical roundtrip: %s %v", again, err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, canonical, "", "  "); err != nil {
		t.Fatal(err)
	}
	_, again, err = readWorkflow(indented.Bytes())
	if err != nil || !bytes.Equal(canonical, again) {
		t.Fatal("whitespace changed identity", err)
	}
	_, normalized, err := workflowValue([]byte(`{"z":-0,"text":"<>&\u2028\u2029"}`), 1<<20, 32)
	if err != nil || !bytes.Equal(normalized, []byte("{\"text\":\"<>&\u2028\u2029\",\"z\":0}")) {
		t.Fatalf("canon: %s %v", normalized, err)
	}
	_, literal, err := workflowCase([]byte(`{"z":-0,"text":"<>&\u2028\u2029"}`))
	if err != nil || string(literal) != `{"text":"\u003c\u003e\u0026\u2028\u2029","z":-0}` {
		t.Fatalf("literal: %s %v", literal, err)
	}
	if digest(literal) == digest(normalized) {
		t.Fatal("collapsed payload and envelope domains")
	}
}
func TestWorkflowDocumentRefusals(t *testing.T) {
	tests := []struct{ name, old, replacement string }{
		{"duplicate-member", `"workflowVersion":1`, `"workflowVersion":1,"workflowVersion":1`},
		{"case-sibling", `"workflowVersion":1`, `"WorkflowVersion":1`},
		{"unknown", `"workflowVersion":1`, `"workflowVersion":1,"script":"execute"`},
		{"version", `"workflowVersion":1`, `"workflowVersion":2`},
		{"dialect-version", `"version":3`, `"version":4`},
		{"implicit-named", `"version":3`, `"formatVersion":"2.0.0"`},
		{"duplicate-stage", `"id":"people"`, `"id":"companies"`},
		{"second-root", `"fromStage":"companies","selected":true`, `"root":true`},
		{"forward-edge", `"fromStage":"companies"`, `"fromStage":"people"`},
		{"unselected", `"selected":true`, `"selected":false`},
		{"root-binding", `"caseBindings":[]`, `"caseBindings":[{"from":"","to":"/x","type":"object","required":true}]`},
		{"null-bindings", `"caseBindings":[]`, `"caseBindings":null`},
		{"missing-binding-bool", `"type":"string","required":true`, `"type":"string"`},
		{"null-from", `"from":"/name"`, `"from":null`},
		{"bad-escape", `"from":"/name"`, `"from":"/na~2me"`},
		{"empty-target", `"to":"/company/name"`, `"to":""`},
		{"invalid-type", `"type":"string"`, `"type":"number"`},
		{"selection-null", `"selection":{"mode":"reviewer"}`, `"selection":null`},
		{"selection-extra", `"selection":{"mode":"reviewer"}`, `"selection":{"mode":"reviewer","rule":null}`},
		{"batch-disposition", `"selection":{"mode":"reviewer"}`, `"selection":{"mode":"disposition","rule":{"kind":"unresolved"}}`},
		{"nonterminal-result", `"result":"people"`, `"result":"companies"`},
		{"relative-pack", `https://example.invalid/company`, `example.invalid/company`},
		{"relation", `"direction":"output-to-parent"`, `"direction":"forward"`},
		{"stage-bound", `"maxStages":2`, `"maxStages":1`},
		{"ceiling", `"maxRecords":5`, `"maxRecords":4097`},
		{"float", `"maxStages":2`, `"maxStages":2.0`},
		{"unsafe-integer", `"maxBytes":1048576`, `"maxBytes":9007199254740992`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(string(workflowExample()), tc.old, tc.replacement, 1)
			if raw == string(workflowExample()) {
				t.Fatal("test replacement did not apply")
			}
			if _, _, err := readWorkflow([]byte(raw)); err == nil {
				t.Fatal("accepted malformed workflow")
			}
		})
	}
	for _, raw := range [][]byte{nil, []byte(`null`), []byte(`[]`), append(workflowExample(), []byte(`{}`)...), bytes.Repeat([]byte(" "), 262145)} {
		if _, _, err := readWorkflow(raw); err == nil {
			t.Fatal("accepted malformed top level")
		}
	}
}
func TestWorkflowProjectionPresenceAndTypes(t *testing.T) {
	payload := []byte(`{"name":"A","zero":-0,"flag":false,"nothing":null,"empty":"","when":"2026-10-16T20:00:00Z","array":["a",false],"object":{"nested":"<&>"},"unselected":"private"}`)
	bindings := []workflowBinding{{"/name", "/company/name", "string", true}, {"/zero", "/size", "integer", true}, {"/flag", "/flag", "boolean", true}, {"/nothing", "/nil", "null", true}, {"/empty", "/empty", "string", true}, {"/when", "/when", "timestamp", true}, {"/array", "/arr", "array", true}, {"/object", "/obj", "object", true}, {"/absent", "/not-created", "string", false}}
	got, err := projectWorkflowCase(payload, bindings)
	want := `{"arr":["a",false],"company":{"name":"A"},"empty":"","flag":false,"nil":null,"obj":{"nested":"\u003c\u0026\u003e"},"size":-0,"when":"2026-10-16T20:00:00Z"}`
	if err != nil || string(got) != want {
		t.Fatalf("%s %v", got, err)
	}
	for _, b := range []workflowBinding{{"/absent", "/x", "string", true}, {"/nothing", "/x", "string", false}, {"/flag", "/x", "integer", false}, {"/name", "/x", "boolean", true}, {"/when", "/x", "integer", true}} {
		if _, err := projectWorkflowCase(payload, []workflowBinding{b}); !errors.Is(err, errWorkflowProjection) {
			t.Fatalf("%+v: %v", b, err)
		}
	}
	for _, value := range []string{`1.0`, `1e0`, `9007199254740992`, `"2026-02-30T00:00:00Z"`} {
		typ := "integer"
		if strings.HasPrefix(value, `"`) {
			typ = "timestamp"
		}
		if _, err := projectWorkflowCase([]byte(`{"v":`+value+`}`), []workflowBinding{{"/v", "/x", typ, true}}); err == nil {
			t.Fatal("accepted", value)
		}
	}
}
func TestWorkflowProjectionPointersAndOverlap(t *testing.T) {
	got, err := projectWorkflowCase([]byte(`{"a/b":{"~key":[{"v":-0}]},"__proto__":"ordinary"}`), []workflowBinding{{"/a~1b/~0key/0/v", "/numeric/0", "integer", true}, {"/__proto__", "/constructor/prototype", "string", true}})
	if err != nil || string(got) != `{"constructor":{"prototype":"ordinary"},"numeric":{"0":-0}}` {
		t.Fatalf("%s %v", got, err)
	}
	for _, targets := range [][]string{{"/a", "/a"}, {"/a", "/a/b"}, {"/a/b", "/a"}, {"/a~1b", "/a~1b/c"}} {
		bs := []workflowBinding{{"/absent", targets[0], "string", false}, {"/absent", targets[1], "string", false}}
		if _, err := projectWorkflowCase([]byte(`{}`), bs); err == nil {
			t.Fatal("overlap bypassed through absence", targets)
		}
	}
	got, err = projectWorkflowCase([]byte(`{"v":false}`), []workflowBinding{{"", "/all", "object", true}, {"/v", "/also", "boolean", true}})
	if err != nil || string(got) != `{"all":{"v":false},"also":false}` {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := projectWorkflowCase([]byte(`{"a":[0]}`), []workflowBinding{{"/a/00", "/v", "integer", true}}); !errors.Is(err, errWorkflowProjection) {
		t.Fatal("array index accepted", err)
	}
}
func TestWorkflowOutputsDistinguishEmptyAndFailure(t *testing.T) {
	for _, tc := range []struct {
		raw, card string
		count     int
		valid     bool
	}{
		{`{"people":[]}`, "many", 0, true},
		{`{"people":[{"n":"A"},{"n":"A"}]}`, "many", 2, true},
		{`{"people":{"n":-0},"unrelated":1.25}`, "one", 1, true},
		{`{}`, "many", 0, false}, {`{"people":null}`, "many", 0, false},
		{`{"people":[{} ,null]}`, "many", 0, false}, {`{"people":{}}`, "many", 0, false},
		{`{"people":[]}`, "one", 0, false}, {`{"people":{"n":1.25}}`, "one", 0, false},
	} {
		got, err := outputsWorkflow([]byte(tc.raw), workflowOutputs{"/people", tc.card})
		if (err == nil) != tc.valid || tc.valid && len(got) != tc.count {
			t.Fatalf("%s %s => %s %v", tc.raw, tc.card, got, err)
		}
	}
}
func TestWorkflowBoundsApplyBeforeAndAfterProjection(t *testing.T) {
	for _, raw := range []string{`{"s":"` + strings.Repeat("a", 65537) + `"}`, `{"s":"` + strings.Repeat("é", 32769) + `"}`, strings.Repeat(`{"a":`, 33) + `0` + strings.Repeat(`}`, 33), `{"a":[` + strings.Repeat(`0,`, 4096) + `0]}`} {
		if _, _, err := workflowCase([]byte(raw)); err == nil {
			t.Fatal("accepted over-bound payload")
		}
	}
	payload := []byte(`{"s":"` + strings.Repeat("x", 65536) + `"}`)
	var bs []workflowBinding
	for _, name := range strings.Split("a b c d e f g h i j k l m n o p q", " ") {
		bs = append(bs, workflowBinding{"/s", "/" + name, "string", true})
	}
	if _, err := projectWorkflowCase(payload, bs); !errors.Is(err, errWorkflowBound) {
		t.Fatal("projected bytes not bounded", err)
	}
	// Input is within the limit, but the required literal escaping is larger.
	raw := `{`
	for i, name := range strings.Split("a b c d", " ") {
		if i > 0 {
			raw += ","
		}
		raw += `"` + name + `":"` + strings.Repeat("<", 65536) + `"`
	}
	raw += `}`
	if _, _, err := workflowCase([]byte(raw)); !errors.Is(err, errWorkflowBound) {
		t.Fatal("literal byte bound", err)
	}
}

func TestWorkflowVersionBeforeClosedForms(t *testing.T) {
	raw := strings.Replace(string(workflowExample()), `"workflowVersion":1`, `"workflowVersion":1,"unknown":true`, 1)
	raw = strings.Replace(raw, `"version":3`, `"version":4`, 1)
	if _, _, err := readWorkflow([]byte(raw)); !errors.Is(err, errWorkflowVersion) {
		t.Fatal(err)
	}
}
func TestWorkflowPackURIGrammar(t *testing.T) {
	for _, uri := range []string{"https://example.invalid/a%20b?x=%C3%A9#f", "urn:example:a:b", "https://[::1]:443/a", "https://[vF.a:b]/", "urn:"} {
		raw := strings.Replace(string(workflowExample()), "https://example.invalid/company", uri, 1)
		if _, _, err := readWorkflow([]byte(raw)); err != nil {
			t.Fatal("valid URI", uri, err)
		}
	}
	for _, uri := range []string{"https://example.invalid/é", "https://example.invalid/a\\b", "urn:bad%XX", "https://example.invalid/a[0]", "https://example.invalid/?x=[a]", "https://[bad]/", "https://[127.0.0.1]/", "https://host:port/"} {
		raw := strings.Replace(string(workflowExample()), `"https://example.invalid/company"`, string(encode(uri)), 1)
		if _, _, err := readWorkflow([]byte(raw)); err == nil {
			t.Fatal("invalid URI", uri)
		}
	}
}
func TestWorkflowOutputBoundClassification(t *testing.T) {
	for _, depth := range []int{33, 35, 80} {
		raw := strings.Repeat(`{"a":`, depth) + `0` + strings.Repeat(`}`, depth)
		if _, _, err := workflowCase([]byte(raw)); !errors.Is(err, errWorkflowBound) {
			t.Fatalf("depth %d: %v", depth, err)
		}
	}
	raw := []byte(`{"people":{"s":"` + strings.Repeat("x", 65537) + `"}}`)
	if _, err := outputsWorkflow(raw, workflowOutputs{"/people", "one"}); !errors.Is(err, errWorkflowBound) {
		t.Fatal(err)
	}
}
