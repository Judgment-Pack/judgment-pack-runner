package runner

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Which parameters a rule reads unsigned is worked out from the frozen mapping:
// its request templates and rules, and which sources this run acquired.
func TestUnsignedParametersAreWorkedOutFromTheFrozenMapping(t *testing.T) {
	rule := func(parameters string) SourceRead {
		return SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":` + parameters + `,"clauses":[{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"registry":"unknown"},"acquisitionStatus":"unknown"},"reason":"none"}]}`)}
	}
	operation := func(name, arguments string, parameters map[string]Parameter, read SourceRead) MappingSource {
		return MappingSource{Name: name, Kind: "operation", Profile: "vendors", Arguments: json.RawMessage(arguments), Parameters: parameters, Read: read}
	}
	fixed := `{"tool":"lookup","arguments":{}}`
	byID := `{"tool":"lookup","arguments":{"id":{"$param":"vendorId"}}}`
	ref := func(from string) map[string]Parameter {
		return map[string]Parameter{"ref": {From: from, Pointer: "/vendor/id", Type: "integer"}}
	}
	file := MappingSource{Name: "file", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/vendor/id", "/id"}}, Evidence: []EvidenceMapping{}}}}
	lookup := operation("lookup", byID, nil, SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/vendor/id", "/id"}}, Evidence: []EvidenceMapping{}}})
	// run is a preparation in which the sources named were acquired, each with
	// the number of targets given, and the others were not.
	type source struct {
		name     string
		acquired bool
		targets  int
	}
	run := func(sources ...source) *Preparation {
		p := &Preparation{}
		for _, s := range sources {
			o := SourceOutcome{Name: s.name}
			var cite *Citation
			if s.acquired {
				o.Arguments = json.RawMessage(`{"tool":"lookup","arguments":{}}`)
				cite = &Citation{SessionID: "session", Signature: s.name}
			}
			p.Outcomes = append(p.Outcomes, o)
			for i := 0; i < s.targets; i++ {
				p.Lineage = append(p.Lineage, TargetLineage{Source: s.name, Present: i == 0, Receipt: cite})
			}
		}
		return p
	}
	vendor := func(targets int) source { return source{"vendor", true, targets} }
	for name, c := range map[string]struct {
		sources []MappingSource
		run     *Preparation
		want    string
	}{
		"a case parameter only the rule reads": {
			[]MappingSource{operation("vendor", fixed, nil, rule(`{"vendorId":"integer"}`))}, run(vendor(2)),
			`[{"source":"vendor","parameters":[{"name":"vendorId","kind":"case"}],"targets":2}]`},
		"a case parameter the request commits as a whole value": {
			[]MappingSource{operation("vendor", byID, nil, rule(`{"vendorId":"integer"}`))}, run(vendor(2)), `null`},
		"a case parameter the request commits in text": {
			[]MappingSource{operation("vendor", `{"tool":"execute_sql","arguments":{"sql":{"$text":"SELECT * FROM vendors WHERE id = {{vendorId}}"}}}`, nil, rule(`{"vendorId":"integer"}`))}, run(vendor(2)), `null`},
		"a case parameter the request commits in a list": {
			[]MappingSource{operation("vendor", `{"tool":"lookup","arguments":{"ids":[1,{"$param":"vendorId"}]}}`, nil, rule(`{"vendorId":"integer"}`))}, run(vendor(2)), `null`},
		"a case parameter another acquired source's request commits": {
			[]MappingSource{lookup, operation("vendor", fixed, nil, rule(`{"vendorId":"integer"}`))}, run(source{"lookup", true, 1}, vendor(2)), `null`},
		"a case parameter only a skipped source's request refers to": {
			[]MappingSource{lookup, operation("vendor", fixed, nil, rule(`{"vendorId":"integer"}`))}, run(source{"lookup", false, 1}, vendor(2)),
			`[{"source":"vendor","parameters":[{"name":"vendorId","kind":"case"}],"targets":2}]`},
		"runAt": {
			[]MappingSource{operation("vendor", byID, nil, rule(`{"vendorId":"integer","runAt":"timestamp"}`))}, run(vendor(1)),
			`[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":1}]`},
		"an acquired earlier source's fact": {
			[]MappingSource{lookup, operation("vendor", fixed, ref("lookup"), rule(`{"ref":"integer"}`))}, run(source{"lookup", true, 1}, vendor(2)), `null`},
		"a local file's fact": {
			[]MappingSource{file, operation("vendor", fixed, ref("file"), rule(`{"ref":"integer"}`))}, run(source{"file", false, 1}, vendor(2)),
			`[{"source":"vendor","parameters":[{"name":"ref","kind":"local-file"}],"targets":2}]`},
		"a local file's fact the request commits": {
			[]MappingSource{file, operation("vendor", `{"tool":"lookup","arguments":{"id":{"$param":"ref"}}}`, ref("file"), rule(`{"ref":"integer"}`))}, run(source{"file", false, 1}, vendor(2)), `null`},
		"a local file's fact another source's request refers to by that name": {
			[]MappingSource{file, operation("lookup", `{"tool":"lookup","arguments":{"id":{"$param":"ref"}}}`, ref("file"), SourceRead{Copy: &CopyMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}}), operation("vendor", fixed, ref("file"), rule(`{"ref":"integer"}`))}, run(source{"file", false, 1}, source{"lookup", true, 0}, vendor(2)),
			`[{"source":"vendor","parameters":[{"name":"ref","kind":"local-file"}],"targets":2}]`},
		"a skipped source": {
			[]MappingSource{operation("vendor", fixed, nil, rule(`{"vendorId":"integer"}`))}, run(source{"vendor", false, 2}), `null`},
		"an acquired source without targets": {
			[]MappingSource{operation("vendor", fixed, nil, rule(`{"vendorId":"integer"}`))}, run(vendor(0)), `null`},
		"a copy": {
			[]MappingSource{operation("vendor", fixed, nil, SourceRead{Copy: &CopyMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}})}, run(vendor(2)), `null`},
		"several, by source and by name": {
			[]MappingSource{operation("vendor", fixed, nil, rule(`{"zeta":"string","runAt":"timestamp","alpha":"string"}`)), operation("detail", fixed, nil, rule(`{"vendorId":"integer"}`))}, run(vendor(2), source{"detail", true, 1}),
			`[{"source":"vendor","parameters":[{"name":"alpha","kind":"case"},{"name":"runAt","kind":"runAt"},{"name":"zeta","kind":"case"}],"targets":2},{"source":"detail","parameters":[{"name":"vendorId","kind":"case"}],"targets":1}]`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := string(encode(unsignedParameters(InputMapping{Version: 2, Sources: c.sources}, c.run))); got != c.want {
				t.Fatal(got)
			}
		})
	}
}

// packRule is a rule over the triage pack's targets: when the condition holds,
// the request fact if asked for and the evidence named are present; otherwise
// the evidence is unknown.
func packRule(parameters, when string, fact bool, evidence string) SourceRead {
	facts := `[]`
	if fact {
		facts = `[{"pointer":"/request","from":"/facts/request"}]`
	}
	return SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":` + parameters + `,"clauses":[{"when":` + when + `,"claim":{"facts":` + facts + `,"evidence":{"` + evidence + `":"present"},"acquisitionStatus":"resolved"},"reason":"matching-vendor"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"` + evidence + `":"unknown"},"acquisitionStatus":"unknown"},"reason":"no-match"}]}`)}
}

// unsignedExport completes one run with the real Runtime whose two operations
// read parameters by rules:
//
//   - vendor's request commits vendorId and window; its rule reads both and
//     runAt, in a freshness condition, and maps the request fact and the
//     intake form;
//   - registry's request commits nothing; its rule reads vendorId, committed by
//     vendor's request, and region, which no request commits, and maps the
//     sponsor's endorsement.
func unsignedExport(t *testing.T) (VerificationBundle, []InputProfile) {
	t.Helper()
	cfg := testConfig(t)
	p, key := testProfile(t)
	cfg.InputProfiles = []InputProfile{p}
	at := time.Now().UTC().Truncate(time.Second)
	m := InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"},
		Case: &CaseMapping{Parameters: map[string]Parameter{"vendorId": {Pointer: "/id", Type: "integer"}, "window": {Pointer: "/window", Type: "integer"}, "region": {Pointer: "/region", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{
			{Name: "vendor", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"execute_sql","arguments":{"sql":{"$text":"SELECT *, {{window}} AS window FROM vendors WHERE id = {{vendorId}}"}}}`),
				Read: packRule(`{"vendorId":"integer","window":"integer","runAt":"timestamp"}`, `{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"freshWithin","field":"/observedAt","asOf":"runAt","maxAge":"window"}]}`, true, "intake-form")},
			{Name: "registry", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{}}`),
				Read: packRule(`{"vendorId":"integer","region":"string"}`, `{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"equalsParam","field":"/region","param":"region"}]}`, false, "sponsor-endorsement")},
		}}
	vendor := `{"id":7,"observedAt":"` + at.Format("2006-01-02T15:04:05Z") + `","facts":` + string(sample().Facts) + `}`
	input := Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{"id":7,"window":86400,"region":"north"}`), Sources: map[string]SourceValue{
		"vendor":   {Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT *, 86400 AS window FROM vendors WHERE id = 7"}}`), []byte(vendor), at, 0)},
		"registry": {Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{}}`), []byte(`{"id":7,"region":"north"}`), at, 1)},
	}}}
	return completeExport(t, cfg, input, true), cfg.InputProfiles
}

const unsignedReported = `[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":2},{"source":"registry","parameters":[{"name":"region","kind":"case"}],"targets":1}]`

// A real run reports what its rules read unsigned, and every target was
// derived as the rules say. The export in testdata/mapping-v2-unsigned-parameters
// was written by this test with JPACK_V2_UNSIGNED_EXPORT_DIR set.
func TestVerifyInputsReportsUnsignedParameters(t *testing.T) {
	bundle, profiles := unsignedExport(t)
	raw := encode(bundle)
	if dir := os.Getenv("JPACK_V2_UNSIGNED_EXPORT_DIR"); dir != "" {
		for name, data := range map[string][]byte{"run.json": raw, "profiles.json": encode(profiles), "release-digest.txt": []byte(bundle.ReleaseDigest)} {
			if e := os.WriteFile(dir+"/"+name, data, 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	v, e := VerifyInputs(raw, profiles, bundle.ReleaseDigest)
	if e != nil || string(encode(v.Unsigned)) != unsignedReported || v.Classes != (InputClasses{Record: 3}) {
		t.Fatal(e, string(encode(v.Unsigned)), v.Classes)
	}
	if string(bundle.Run.Input.Evidence) != `{"intake-form":"present","sponsor-endorsement":"present"}` || !strings.Contains(string(bundle.Run.Input.Facts), `"request"`) {
		t.Fatal("a rule did not match:", string(bundle.Run.Input.Facts), string(bundle.Run.Input.Evidence))
	}
}

// Through a real run: a fact of an acquired earlier source is backed by its
// receipt, and a local file's fact is not.
func TestVerifyInputsReportsAnEarlierSourcesFactByItsSource(t *testing.T) {
	for name, c := range map[string]struct {
		local bool
		want  string
	}{"an acquired operation": {false, `null`}, "a local file": {true, `[{"source":"detail","parameters":[{"name":"ref","kind":"local-file"}],"targets":3}]`}} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			p, key := testProfile(t)
			cfg.InputProfiles = []InputProfile{p}
			at := time.Now().UTC().Truncate(time.Second)
			facts := CopyMapping{Facts: []FactMapping{{"/vendor/id", "/id"}}, Evidence: []EvidenceMapping{}}
			upstream := MappingSource{Name: "lookup", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"execute_sql","arguments":{"sql":"SELECT id FROM vendors WHERE id = 7"}}`), Read: SourceRead{Copy: &facts}}
			value := SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT id FROM vendors WHERE id = 7"}}`), []byte(`{"id":7}`), at, 0)}
			index := 1
			if c.local {
				upstream = MappingSource{Name: "lookup", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &facts}}
				value = SourceValue{Snapshot: fileInput(`{"id":7}`, InputMapping{Version: 1, Provider: "local-file"}).Source.Snapshot}
				index = 0
			}
			detail := MappingSource{Name: "detail", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Parameters: map[string]Parameter{"ref": {From: "lookup", Pointer: "/vendor/id", Type: "integer"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{}}`),
				Read: SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":{"ref":"integer"},"clauses":[{"when":{"op":"equalsParam","field":"/id","param":"ref"},"claim":{"facts":[{"pointer":"/request","from":"/facts/request"}],"evidence":{"intake-form":"present","sponsor-endorsement":"present"},"acquisitionStatus":"resolved"},"reason":"matching-vendor"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"intake-form":"unknown","sponsor-endorsement":"unknown"},"acquisitionStatus":"unknown"},"reason":"no-match"}]}`)}}
			m := InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &CaseMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{upstream, detail}}
			result := `{"id":7,"facts":` + string(sample().Facts) + `}`
			input := Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{}`), Sources: map[string]SourceValue{"lookup": value, "detail": {Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{}}`), []byte(result), at, index)}}}}
			bundle := completeExport(t, cfg, input, true)
			v, e := VerifyInputs(encode(bundle), cfg.InputProfiles, bundle.ReleaseDigest)
			if e != nil || string(encode(v.Unsigned)) != c.want || string(bundle.Run.Input.Evidence) != `{"intake-form":"present","sponsor-endorsement":"present"}` {
				t.Fatal(e, string(encode(v.Unsigned)), string(bundle.Run.Input.Evidence))
			}
		})
	}
}

// The export in testdata/mapping-v2-unsigned-parameters verifies, offline and
// without a Runtime, and reports what its rules read unsigned.
func TestARunWithUnsignedParametersReportsThem(t *testing.T) {
	dir := "testdata/mapping-v2-unsigned-parameters/"
	raw, e := os.ReadFile(dir + "run.json")
	if e != nil {
		t.Fatal(e)
	}
	rawProfiles, e := os.ReadFile(dir + "profiles.json")
	if e != nil {
		t.Fatal(e)
	}
	trusted, e := os.ReadFile(dir + "release-digest.txt")
	if e != nil {
		t.Fatal(e)
	}
	profiles, e := ParseInputProfiles(rawProfiles)
	if e != nil {
		t.Fatal(e)
	}
	v, e := VerifyInputs(raw, profiles, strings.TrimSpace(string(trusted)))
	if e != nil || string(encode(v.Unsigned)) != unsignedReported || v.Classes != (InputClasses{Record: 3}) {
		t.Fatal(e, string(encode(v.Unsigned)), v.Classes)
	}
}
