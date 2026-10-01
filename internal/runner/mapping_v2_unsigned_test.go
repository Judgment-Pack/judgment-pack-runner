package runner

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Which parameters a rule reads unsigned is worked out from the frozen mapping:
// its request templates, calculation bindings and rule conditions, and which
// sources this run acquired and applied.
func TestUnsignedParametersAreWorkedOutFromTheFrozenMapping(t *testing.T) {
	// rule reads each parameter named in an equalsParam condition, and declares
	// those and the others given.
	rule := func(reads []string, others ...string) SourceRead {
		declared := map[string]string{}
		conditions := []string{}
		for _, n := range reads {
			declared[n] = "string"
			conditions = append(conditions, `{"op":"equalsParam","field":"/`+n+`","param":"`+n+`"}`)
		}
		for _, n := range others {
			declared[n] = "string"
		}
		when := `{"op":"always"}`
		if len(conditions) > 0 {
			when = `{"op":"all","of":[` + strings.Join(conditions, ",") + `]}`
		}
		return SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":` + string(encode(declared)) + `,"clauses":[{"when":` + when + `,"claim":{"facts":[{"pointer":"/vendor/id","from":"/id"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"matched"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"none"}]}`)}
	}
	reads := func(names ...string) []string { return names }
	operation := func(name, arguments string, parameters map[string]Parameter, read SourceRead) MappingSource {
		return MappingSource{Name: name, Kind: "operation", Profile: "vendors", Arguments: json.RawMessage(arguments), Parameters: parameters, Read: read}
	}
	fixed := `{"tool":"lookup","arguments":{}}`
	byID := `{"tool":"lookup","arguments":{"id":{"$param":"vendorId"}}}`
	ref := func(from string) map[string]Parameter {
		return map[string]Parameter{"ref": {From: from, Pointer: "/vendor/id", Type: "integer"}}
	}
	copied := SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/vendor/id", "/id"}}, Evidence: []EvidenceMapping{}}}
	file := MappingSource{Name: "lookup", Kind: "selected-file", Provider: "local-file", Read: copied}
	// run is a preparation in which each source has the targets given, and was
	// acquired or not; a calculator's status is given when it is one.
	type source struct {
		name        string
		acquired    bool
		targets     int
		calculation string
	}
	run := func(sources ...source) *Preparation {
		p := &Preparation{}
		for _, s := range sources {
			o := SourceOutcome{Name: s.name}
			var cite *Citation
			if s.acquired {
				o.Arguments = json.RawMessage(fixed)
				cite = &Citation{SessionID: "session", Signature: s.name}
			}
			var calc *CalculationLineage
			if s.calculation != "" {
				calc = &CalculationLineage{Status: s.calculation}
			}
			p.Outcomes = append(p.Outcomes, o)
			for i := 0; i < s.targets; i++ {
				p.Lineage = append(p.Lineage, TargetLineage{Source: s.name, Present: i == 0, Receipt: cite, Calculation: calc})
			}
		}
		return p
	}
	vendor := func(targets int) source { return source{"vendor", true, targets, ""} }
	lookup := func(acquired bool) source { return source{"lookup", acquired, 1, ""} }
	calculator := func(status string) source { return source{"vendor", true, 2, status} }
	calculated := func(read SourceRead) MappingSource {
		s := operation("vendor", fixed, nil, read)
		s.Calculation = &CalculationBinding{Inputs: map[string]string{"id": "vendorId"}, Tables: map[string]int{}}
		return s
	}
	listed := func(source string, targets int, parameters ...string) string {
		ps := []string{}
		for i := 0; i+1 < len(parameters); i += 2 {
			ps = append(ps, `{"name":"`+parameters[i]+`","kind":"`+parameters[i+1]+`"}`)
		}
		return `{"source":"` + source + `","parameters":[` + strings.Join(ps, ",") + `],"targets":` + string(encode(targets)) + `}`
	}
	for name, c := range map[string]struct {
		sources []MappingSource
		run     *Preparation
		want    []string
	}{
		"a case parameter only the rule reads": {
			[]MappingSource{operation("vendor", fixed, nil, rule(reads("vendorId")))}, run(vendor(2)),
			[]string{listed("vendor", 2, "vendorId", "case")}},
		"a case parameter the rule declares and does not read": {
			[]MappingSource{operation("vendor", fixed, nil, rule(nil, "vendorId"))}, run(vendor(2)), nil},
		"parameters named at any depth, and by freshWithin": {
			[]MappingSource{operation("vendor", fixed, nil, SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":{"vendorId":"integer","window":"integer","runAt":"timestamp","unread":"string"},"clauses":[{"when":{"op":"any","of":[{"op":"not","of":{"op":"freshWithin","field":"/t","asOf":"runAt","maxAge":"window"}},{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"}]}]},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"resolved"},"reason":"matched"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"none"}]}`)})}, run(vendor(1)),
			[]string{listed("vendor", 1, "runAt", "runAt", "vendorId", "case", "window", "case")}},
		"a case parameter the request commits as a whole value": {
			[]MappingSource{operation("vendor", byID, nil, rule(reads("vendorId")))}, run(vendor(2)), nil},
		"a case parameter the request commits in text that names it alone, twice": {
			[]MappingSource{operation("vendor", `{"tool":"execute_sql","arguments":{"sql":{"$text":"SELECT * FROM vendors WHERE id = {{vendorId}} OR parent = {{vendorId}}"}}}`, nil, rule(reads("vendorId")))}, run(vendor(2)), nil},
		"a case parameter in text that names another": {
			[]MappingSource{operation("vendor", `{"tool":"lookup","arguments":{"key":{"$text":"{{vendorId}}{{tail}}"}}}`, nil, rule(reads("vendorId")))}, run(vendor(2)),
			[]string{listed("vendor", 2, "vendorId", "ambiguous-text")}},
		"a case parameter in text that names another, and as a whole value": {
			[]MappingSource{operation("vendor", `{"tool":"lookup","arguments":{"key":{"$text":"{{vendorId}}{{tail}}"},"id":{"$param":"vendorId"}}}`, nil, rule(reads("vendorId")))}, run(vendor(2)), nil},
		"a case parameter the request commits in a list": {
			[]MappingSource{operation("vendor", `{"tool":"lookup","arguments":{"ids":[1,{"$param":"vendorId"}]}}`, nil, rule(reads("vendorId")))}, run(vendor(2)), nil},
		"a case parameter another acquired source's request commits": {
			[]MappingSource{operation("lookup", byID, nil, copied), operation("vendor", fixed, nil, rule(reads("vendorId")))}, run(lookup(true), vendor(2)),
			[]string{listed("vendor", 2, "vendorId", "case")}},
		"a case parameter an earlier rule's own request commits, read by a later rule": {
			[]MappingSource{operation("lookup", byID, nil, rule(reads("vendorId"))), operation("vendor", fixed, nil, rule(reads("vendorId")))}, run(lookup(true), vendor(2)),
			[]string{listed("vendor", 2, "vendorId", "case")}},
		"a commitment, then a skipped source that reuses the parameter": {
			[]MappingSource{operation("vendor", byID, nil, rule(reads("vendorId"))), operation("lookup", byID, nil, rule(reads("vendorId")))}, run(vendor(2), lookup(false)), nil},
		"runAt": {
			[]MappingSource{operation("vendor", byID, nil, rule(reads("vendorId", "runAt")))}, run(vendor(1)),
			[]string{listed("vendor", 1, "runAt", "runAt")}},
		"an acquired earlier source's fact, from a copy": {
			[]MappingSource{operation("lookup", byID, nil, copied), operation("vendor", fixed, ref("lookup"), rule(reads("ref")))}, run(lookup(true), vendor(2)), nil},
		"an earlier source's fact, from a rule that read a case parameter": {
			[]MappingSource{operation("lookup", fixed, nil, rule(reads("region"))), operation("vendor", fixed, ref("lookup"), rule(reads("ref")))}, run(lookup(true), vendor(2)),
			[]string{listed("lookup", 1, "region", "case"), listed("vendor", 2, "ref", "upstream")}},
		"an earlier source's fact, from a rule that read a case parameter, which the request commits": {
			[]MappingSource{operation("lookup", fixed, nil, rule(reads("region"))), operation("vendor", `{"tool":"lookup","arguments":{"id":{"$param":"ref"}}}`, ref("lookup"), rule(reads("ref")))}, run(lookup(true), vendor(2)),
			[]string{listed("lookup", 1, "region", "case")}},
		"an earlier source's fact, from a rule that read runAt": {
			[]MappingSource{operation("lookup", fixed, nil, rule(reads("runAt"))), operation("vendor", fixed, ref("lookup"), rule(reads("ref")))}, run(lookup(true), vendor(2)),
			[]string{listed("lookup", 1, "runAt", "runAt"), listed("vendor", 2, "ref", "upstream-runAt")}},
		"an earlier source's fact, two sources down": {
			[]MappingSource{operation("lookup", fixed, nil, rule(reads("region"))), operation("middle", fixed, ref("lookup"), rule(reads("ref"))), operation("vendor", fixed, map[string]Parameter{"ref": {From: "middle", Pointer: "/vendor/id", Type: "integer"}}, rule(reads("ref")))}, run(lookup(true), source{"middle", true, 1, ""}, vendor(2)),
			[]string{listed("lookup", 1, "region", "case"), listed("middle", 1, "ref", "upstream"), listed("vendor", 2, "ref", "upstream")}},
		"a local file's fact": {
			[]MappingSource{file, operation("vendor", fixed, ref("lookup"), rule(reads("ref")))}, run(lookup(false), vendor(2)),
			[]string{listed("vendor", 2, "ref", "local-file")}},
		"a local file's fact the request commits": {
			[]MappingSource{file, operation("vendor", `{"tool":"lookup","arguments":{"id":{"$param":"ref"}}}`, ref("lookup"), rule(reads("ref")))}, run(lookup(false), vendor(2)), nil},
		"a local file's fact in text that names another": {
			[]MappingSource{file, operation("vendor", `{"tool":"lookup","arguments":{"key":{"$text":"{{ref}}{{tail}}"}}}`, ref("lookup"), rule(reads("ref")))}, run(lookup(false), vendor(2)),
			[]string{listed("vendor", 2, "ref", "ambiguous-text")}},
		"a calculator's bound parameter": {
			[]MappingSource{calculated(rule(reads("vendorId")))}, run(calculator("computed")), nil},
		"a calculator's unbound parameter": {
			[]MappingSource{calculated(rule(reads("vendorId", "region")))}, run(calculator("computed")),
			[]string{listed("vendor", 2, "region", "case")}},
		"a calculator that did not compute": {
			[]MappingSource{calculated(rule(reads("region")))}, run(calculator("cannot-compute")), nil},
		"a skipped source": {
			[]MappingSource{operation("vendor", fixed, nil, rule(reads("vendorId")))}, run(source{"vendor", false, 2, ""}), nil},
		"an acquired source without targets": {
			[]MappingSource{operation("vendor", fixed, nil, rule(reads("vendorId")))}, run(vendor(0)), nil},
		"a copy": {
			[]MappingSource{operation("vendor", fixed, nil, copied)}, run(vendor(2)), nil},
		"several, by source and by name": {
			[]MappingSource{operation("vendor", fixed, nil, rule(reads("zeta", "runAt", "alpha"))), operation("detail", fixed, nil, rule(reads("vendorId")))}, run(vendor(2), source{"detail", true, 1, ""}),
			[]string{listed("vendor", 2, "alpha", "case", "runAt", "runAt", "zeta", "case"), listed("detail", 1, "vendorId", "case")}},
	} {
		t.Run(name, func(t *testing.T) {
			want := "null"
			if c.want != nil {
				want = "[" + strings.Join(c.want, ",") + "]"
			}
			if got := string(encode(unsignedParameters(InputMapping{Version: 2, Sources: c.sources}, c.run))); got != want {
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
//   - vendor's request commits vendorId, in text that names it alone, and
//     window, as a whole value; its rule reads both and runAt, in a freshness
//     condition, and maps the request fact and the intake form;
//   - registry's request carries vendorId only in text that also names suffix;
//     its rule reads vendorId, and region, which no request carries, declares
//     window without reading it, and maps the sponsor's endorsement.
func unsignedExport(t *testing.T) (VerificationBundle, []InputProfile) {
	t.Helper()
	cfg := testConfig(t)
	p, key := testProfile(t)
	cfg.InputProfiles = []InputProfile{p}
	at := time.Now().UTC().Truncate(time.Second)
	m := InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"},
		Case: &CaseMapping{Parameters: map[string]Parameter{"vendorId": {Pointer: "/id", Type: "integer"}, "window": {Pointer: "/window", Type: "integer"}, "suffix": {Pointer: "/suffix", Type: "integer"}, "region": {Pointer: "/region", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{
			{Name: "vendor", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"execute_sql","arguments":{"sql":{"$text":"SELECT * FROM vendors WHERE id = {{vendorId}}"},"window":{"$param":"window"}}}`),
				Read: packRule(`{"vendorId":"integer","window":"integer","runAt":"timestamp"}`, `{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"freshWithin","field":"/observedAt","asOf":"runAt","maxAge":"window"}]}`, true, "intake-form")},
			{Name: "registry", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"key":{"$text":"{{vendorId}}{{suffix}}"}}}`),
				Read: packRule(`{"vendorId":"integer","region":"string","window":"integer"}`, `{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"equalsParam","field":"/region","param":"region"}]}`, false, "sponsor-endorsement")},
		}}
	vendor := `{"id":7,"observedAt":"` + at.Format("2006-01-02T15:04:05Z") + `","facts":` + string(sample().Facts) + `}`
	input := Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{"id":7,"window":86400,"suffix":3,"region":"north"}`), Sources: map[string]SourceValue{
		"vendor":   {Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7","window":86400}}`), []byte(vendor), at, 0)},
		"registry": {Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{"key":"73"}}`), []byte(`{"id":7,"region":"north"}`), at, 1)},
	}}}
	return completeExport(t, cfg, input, true), cfg.InputProfiles
}

const unsignedReported = `[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":2},{"source":"registry","parameters":[{"name":"region","kind":"case"},{"name":"vendorId","kind":"ambiguous-text"}],"targets":1}]`

// A real run reports what its rules read unsigned, and every target was
// derived as the rules say. The export in testdata/mapping-v2-unsigned-parameters
// was written by this test with JPACK_V2_UNSIGNED_EXPORT_DIR set.
func TestVerifyInputsReportsUnsignedParameters(t *testing.T) {
	bundle, profiles := unsignedExport(t)
	raw := encode(bundle)
	if dir := os.Getenv("JPACK_V2_UNSIGNED_EXPORT_DIR"); dir != "" {
		for name, data := range map[string][]byte{"run.json": raw, "profiles.json": encode(profiles), "release-digest.txt": []byte(bundle.ReleaseDigest)} {
			if e := os.WriteFile(dir+"/"+name, data, 0644); e != nil {
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

// Through a real run, a later source reads an earlier source's fact: one
// copied from a signed response is backed by that receipt; one a rule derived
// by reading a case parameter is not, nor is a local file's.
func TestVerifyInputsReportsAnEarlierSourcesFactByItsSource(t *testing.T) {
	for name, c := range map[string]struct {
		upstream string
		want     string
	}{
		"an acquired operation's copy":                             {"copy", `null`},
		"an acquired operation's rule that reads a case parameter": {"rule", `[{"source":"lookup","parameters":[{"name":"region","kind":"case"}],"targets":1},{"source":"detail","parameters":[{"name":"ref","kind":"upstream"}],"targets":3}]`},
		"a local file": {"local", `[{"source":"detail","parameters":[{"name":"ref","kind":"local-file"}],"targets":3}]`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			p, key := testProfile(t)
			cfg.InputProfiles = []InputProfile{p}
			at := time.Now().UTC().Truncate(time.Second)
			facts := CopyMapping{Facts: []FactMapping{{"/vendor/id", "/id"}}, Evidence: []EvidenceMapping{}}
			upstream := MappingSource{Name: "lookup", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"execute_sql","arguments":{"sql":"SELECT id FROM vendors WHERE id = 7"}}`), Read: SourceRead{Copy: &facts}}
			value := SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT id FROM vendors WHERE id = 7"}}`), []byte(`{"id":7,"region":"north"}`), at, 0)}
			index := 1
			switch c.upstream {
			case "rule":
				upstream.Read = SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":{"region":"string"},"clauses":[{"when":{"op":"equalsParam","field":"/region","param":"region"},"claim":{"facts":[{"pointer":"/vendor/id","from":"/id"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"in-region"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"elsewhere"}]}`)}
			case "local":
				upstream = MappingSource{Name: "lookup", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &facts}}
				value = SourceValue{Snapshot: fileInput(`{"id":7}`, InputMapping{Version: 1, Provider: "local-file"}).Source.Snapshot}
				index = 0
			}
			detail := MappingSource{Name: "detail", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Parameters: map[string]Parameter{"ref": {From: "lookup", Pointer: "/vendor/id", Type: "integer"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{}}`),
				Read: SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":{"ref":"integer"},"clauses":[{"when":{"op":"equalsParam","field":"/id","param":"ref"},"claim":{"facts":[{"pointer":"/request","from":"/facts/request"}],"evidence":{"intake-form":"present","sponsor-endorsement":"present"},"acquisitionStatus":"resolved"},"reason":"matching-vendor"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"intake-form":"unknown","sponsor-endorsement":"unknown"},"acquisitionStatus":"unknown"},"reason":"no-match"}]}`)}}
			m := InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &CaseMapping{Parameters: map[string]Parameter{"region": {Pointer: "/region", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{upstream, detail}}
			result := `{"id":7,"facts":` + string(sample().Facts) + `}`
			input := Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{"region":"north"}`), Sources: map[string]SourceValue{"lookup": value, "detail": {Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{}}`), []byte(result), at, index)}}}}
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

// {{vendorId}}{{tail}} renders vendorId 1 and tail 23 as it renders 12 and 3.
// One receipt verifies both cases, a rule that reads vendorId derives other
// evidence from each, and both report vendorId as carried in ambiguous text.
func TestTextThatNamesTwoParametersCommitsNeither(t *testing.T) {
	p, key := testProfile(t)
	at := time.Now().UTC().Truncate(time.Second)
	m := InputMapping{Version: 2, Case: &CaseMapping{Parameters: map[string]Parameter{"vendorId": {Pointer: "/id", Type: "integer"}, "tail": {Pointer: "/tail", Type: "integer"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{{Name: "vendor", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"key":{"$text":"{{vendorId}}{{tail}}"}}}`),
			Read: SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":{"vendorId":"integer"},"clauses":[{"when":{"op":"equalsParam","field":"/id","param":"vendorId"},"claim":{"facts":[],"evidence":{"registry":"present"},"acquisitionStatus":"resolved"},"reason":"matching-vendor"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"registry":"unknown"},"acquisitionStatus":"unknown"},"reason":"wrong-subject"}]}`)}}}}
	response := signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{"key":"123"}}`), []byte(`{"id":12}`), at, 0)
	for kase, evidence := range map[string]string{`{"id":1,"tail":23}`: `{"registry":"unknown"}`, `{"id":12,"tail":3}`: `{"registry":"present"}`} {
		got, e := normalizeV2(Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(kase), Sources: map[string]SourceValue{"vendor": {Response: response}}}}, []InputProfile{p}, at)
		if e != nil || string(got.Evidence) != evidence {
			t.Fatal(kase, e, string(got.Evidence))
		}
		if reported := string(encode(unsignedParameters(m, got.Preparation))); reported != `[{"source":"vendor","parameters":[{"name":"vendorId","kind":"ambiguous-text"}],"targets":1}]` {
			t.Fatal(kase, reported)
		}
	}
}
