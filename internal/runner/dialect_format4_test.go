package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkdialect "github.com/Judgment-Pack/judgment-pack-runner/sdk/dialect"
)

// The Spec's format 4.0.0 vectors, copied byte for byte at its commit
// 561b2a7245cf6ee13cb5be78da9c7a89f9adcdef (vectors/dialect-format-4).
const format4Vectors = "../../vectors/dialect-format-4/spec"

// The SHA-256 of each top-level file at that commit, taken from the Spec's
// bytes (git show), not from anything this repository computes.
var format4Pins = map[string]string{
	"README.md":     "8db691c9c15c3aa18a2fd45f70af53f2c2c23173483c2def415d9c39d24cbf74",
	"manifest.json": "6155951b8f4bcfc239d7ee88cd5fb400b415b763c889128cc2cf2aabe3afda3a",
	"vectors.json":  "6be9f966999613a8bc2167d1142a4de2f5bf280f5e07ef79671ff223a668983e",
}

func format4File(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(format4Vectors, filepath.FromSlash(name)))
	must(t, err)
	return b
}

// readNumbers reads JSON keeping each number's literal, as the Spec's
// harness does (its Literal), so that -0 and 99.5 reach the reader as
// written.
func readNumbers(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	must(t, d.Decode(&v))
	return v
}

type format4Manifest struct {
	Cases []struct {
		Name              string                    `json:"name"`
		Files             map[string]string         `json:"files"`
		DialectDigest     string                    `json:"dialectDigest"`
		ReadDigest        string                    `json:"readDigest"`
		PreparationDigest string                    `json:"preparationDigest"`
		ExpectedReport    json.RawMessage           `json:"expectedReport"`
		ExpectedOutcomes  map[string][2]string      `json:"expectedOutcomes"`
		ExpectedDerived   map[string]map[string]any `json:"expectedDerivations"`
		Unread            map[string]string         `json:"expectedReportOfAReaderThatDoesNotReadTheFormat"`
	} `json:"cases"`
}

func TestFormat4SpecFilesArePinned(t *testing.T) {
	for name, hash := range format4Pins {
		if got := digest(format4File(t, name)); got != "sha256:"+hash {
			t.Fatalf("%s is not the Spec's bytes at the pinned commit: %s", name, got)
		}
	}
	var manifest format4Manifest
	must(t, json.Unmarshal(format4File(t, "manifest.json"), &manifest))
	files := 0
	for _, c := range manifest.Cases {
		for name, hash := range c.Files {
			if got := digest(format4File(t, c.Name+"/"+name)); got != "sha256:"+hash {
				t.Fatalf("%s/%s differs from the manifest: %s", c.Name, name, got)
			}
			files++
		}
	}
	if len(manifest.Cases) != 2 || files != 13 {
		t.Fatalf("expected the two end-to-end vectors and their 13 files, read %d and %d", len(manifest.Cases), files)
	}
}

// Both end-to-end vectors, through verify-preparation's reader: the
// report, the outcomes and the derivations are the manifest's, written by
// hand, and the recomputed preparation is the Spec's preparation.json.
func TestFormat4EndToEndVectors(t *testing.T) {
	var manifest format4Manifest
	must(t, json.Unmarshal(format4File(t, "manifest.json"), &manifest))
	for _, c := range manifest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			read := func(name string) []byte { return format4File(t, c.Name+"/"+name) }
			f := PreparationFixture{Dialect: read("dialect.json"), Case: read("case.json"), Sources: read("sources.json"), Profiles: read("profiles.json"), Preparation: read("preparation.json")}
			var recomputed *Preparation
			report := verifyPreparationUsing(f, verifyAcquisition, &recomputed)
			if got, want := canonTest(t, report.Line()), canonTest(t, c.ExpectedReport); !bytes.Equal(got, want) {
				t.Fatalf("report\n got %s (%s)\nwant %s", got, report.Reason, want)
			}
			if digest(read("preparation.json")) != c.PreparationDigest || report.Digest != c.PreparationDigest {
				t.Fatal("preparation digest differs from the manifest", report.Digest)
			}
			if recomputed.MappingDigest != c.DialectDigest || digest(read("dialect.canonical.json")) != c.DialectDigest {
				t.Fatal("dialect digest differs from the manifest", recomputed.MappingDigest)
			}
			if c.ReadDigest != "" {
				for _, l := range recomputed.Lineage {
					if l.Source == "case" && l.ReadDigest != c.ReadDigest {
						t.Fatal("the case's read digest is not the digest of its copy and its rule together", l.Target, l.ReadDigest)
					}
				}
			}
			for name, want := range c.ExpectedOutcomes {
				found := false
				for _, o := range recomputed.Outcomes {
					if o.Name == name {
						found = true
						if o.Status != want[0] || o.Reason != want[1] {
							t.Fatalf("outcome %s is %s %s, want %v", name, o.Status, o.Reason, want)
						}
					}
				}
				if !found {
					t.Fatal("no outcome", name)
				}
			}
			for source, want := range c.ExpectedDerived {
				got := map[string]any{}
				for _, l := range recomputed.Lineage {
					if l.Source == source && l.derivation != nil {
						got[l.Target] = readNumbers(t, encode(l.derivation))
					}
				}
				if !bytes.Equal(canonTest(t, encode(got)), canonTest(t, encode(want))) {
					t.Fatalf("derivations of %s\n got %s\nwant %s", source, encode(got), encode(want))
				}
			}
		})
	}
}

// format4Frame is the rows' frame (spec/README.md, "The rows"): one case
// with no source, its copy empty, its rule the row's rule or the clauses
// given, declaring every case parameter and runAt.
func format4Frame(row map[string]any, clauses []any) []byte {
	if d, ok := row["dialect"]; ok {
		return encode(d)
	}
	params, _ := row["parameters"].(map[string]any)
	rule := row["rule"]
	if clauses != nil {
		declared := map[string]any{"runAt": "timestamp"}
		for name, d := range params {
			declared[name] = d.(map[string]any)["type"]
		}
		r := map[string]any{"ruleVersion": "2.0.0", "parameters": declared, "clauses": clauses}
		if d, ok := row["derived"]; ok {
			r["derived"] = d
		}
		rule = r
	}
	c := map[string]any{"facts": []any{}, "evidence": []any{}, "rule": rule}
	if len(params) > 0 {
		c["parameters"] = params
	}
	return encode(map[string]any{"formatVersion": "4.0.0", "id": "urn:judgmentpack:dialect:format-4-vectors", "version": "0.1.0", "case": c})
}

func resolvedClause(when any, facts []any, reason string) any {
	return map[string]any{"when": when, "claim": map[string]any{"facts": facts, "evidence": map[string]any{}, "acquisitionStatus": "resolved"}, "reason": reason}
}

// format4Answer is what a reader of a row gives: the code it refuses or
// finds with, and otherwise the case outcome's rule.
type format4Answer struct {
	result, code, reason string
	rule                 map[string]any
}

// format4Verifier reads a row through verify-preparation's reader, given a
// preparation of verifiedAt alone: every reading ends preparation-differs,
// and the recomputed preparation is kept.
func format4Verifier(t *testing.T, dialect, caseDocument []byte, verifiedAt string) format4Answer {
	t.Helper()
	var recomputed *Preparation
	report := verifyPreparationUsing(PreparationFixture{Dialect: dialect, Case: caseDocument, Sources: []byte(`{}`), Profiles: []byte(`[]`), Preparation: []byte(`{"verifiedAt":"` + verifiedAt + `"}`)}, verifyAcquisition, &recomputed)
	if report.Code != codeDiffers {
		return format4Answer{result: report.Result, code: report.Code, reason: report.Reason}
	}
	rule, _ := readNumbers(t, recomputed.Outcomes[0].rule).(map[string]any)
	return format4Answer{result: ResultReading, rule: rule}
}

// format4Package reads a row through the sdk/dialect package, which refuses
// a dialect with sources sources-unread.
func format4Package(t *testing.T, dialect, caseDocument []byte, at time.Time) format4Answer {
	t.Helper()
	code := func(err error) format4Answer {
		var e *sdkdialect.Error
		if !errors.As(err, &e) {
			t.Fatal("the package returned an error with no code", err)
		}
		return format4Answer{code: e.Code, reason: e.Reason}
	}
	d, err := sdkdialect.Read(dialect)
	if err != nil {
		return code(err)
	}
	p, err := d.Prepare(caseDocument, at)
	if err != nil {
		return code(err)
	}
	prep := readNumbers(t, p.Bytes()).(map[string]any)
	rule, _ := prep["outcomes"].([]any)[0].(map[string]any)["rule"].(map[string]any)
	return format4Answer{result: ResultReading, rule: rule}
}

func sameJSONValue(t *testing.T, a, b any) bool {
	t.Helper()
	return bytes.Equal(canonTest(t, encode(a)), canonTest(t, encode(b)))
}

// Every one of the Spec's 167 rows, through verify-preparation's reader and
// through the sdk/dialect package, each held to the row's hand-written
// answer, as the Spec's own test reads them (test_every_row).
func TestFormat4Rows(t *testing.T) {
	document := readNumbers(t, format4File(t, "vectors.json")).(map[string]any)
	verifiedAt := document["verifiedAt"].(string)
	at, err := time.Parse("2006-01-02T15:04:05Z", verifiedAt)
	must(t, err)
	rows := document["cases"].([]any)
	if len(rows) != 167 {
		t.Fatalf("expected the Spec's 167 rows, read %d", len(rows))
	}
	seen := map[string]bool{}
	ran, packaged := 0, 0
	for _, value := range rows {
		row := value.(map[string]any)
		id := row["id"].(string)
		if seen[id] {
			t.Fatal("two rows named", id)
		}
		seen[id] = true
		caseDocument := []byte(`{}`)
		if c, ok := row["case"]; ok {
			caseDocument = encode(c)
		}
		t.Run(id, func(t *testing.T) {
			// check holds one reader's answer to the row. The package does
			// not read a dialect that names a source, refused
			// sources-unread, or one of format 3.0.0, refused version-unread
			// (unread): the verifier reads both.
			given, _ := row["dialect"].(map[string]any)
			_, named := given["sources"]
			earlier := given["formatVersion"] == "3.0.0" || given["formatVersion"] == "2.0.0"
			unread := false
			notRead := func(reader string, a format4Answer) bool {
				if reader == "package" && (a.code == "sources-unread" && named || a.code == "version-unread" && earlier) {
					unread = true
				}
				return unread
			}
			check := func(reader string, answer func(clauses []any) format4Answer) {
				t.Helper()
				hold := func(a format4Answer) bool {
					if notRead(reader, a) {
						return false
					}
					if want, ok := row["expectedRefusal"].(string); ok {
						if a.code != want || reader == "verifier" && a.result != ResultRefusal {
							t.Fatalf("%s: %s %s, want the refusal %s", reader, a.result, a.code, want)
						}
						return false
					}
					if want, ok := row["expectedFinding"].(string); ok {
						if a.code != want || reader == "verifier" && a.result != ResultFinding {
							t.Fatalf("%s: %s %s, want the finding %s", reader, a.result, a.code, want)
						}
						return false
					}
					if a.rule == nil {
						t.Fatalf("%s: %s %s (%s), want a reading", reader, a.result, a.code, a.reason)
					}
					return true
				}
				switch {
				case row["expectedDerived"] != nil:
					for name, want := range row["expectedDerived"].(map[string]any) {
						a := answer([]any{
							resolvedClause(map[string]any{"op": "exists", "derived": name}, []any{map[string]any{"pointer": "/value", "derived": name}}, "some"),
							resolvedClause(map[string]any{"op": "always"}, []any{}, "none"),
						})
						if notRead(reader, a) {
							return
						}
						if a.rule == nil {
							t.Fatalf("%s: %s: %s %s, want a reading", reader, name, a.result, a.code)
						}
						facts := a.rule["claim"].(map[string]any)["facts"]
						if want == nil {
							if a.rule["reason"] != "none" || !sameJSONValue(t, facts, map[string]any{}) {
								t.Fatalf("%s: %s has a value, %v %v", reader, name, a.rule["reason"], facts)
							}
						} else if a.rule["reason"] != "some" || !sameJSONValue(t, facts, map[string]any{"value": want}) {
							t.Fatalf("%s: %s is %v %v, want %v", reader, name, a.rule["reason"], facts, want)
						}
					}
				case row["when"] != nil:
					a := answer([]any{
						resolvedClause(row["when"], []any{map[string]any{"pointer": "/matched", "value": true}}, "true"),
						resolvedClause(map[string]any{"op": "always"}, []any{map[string]any{"pointer": "/matched", "value": false}}, "false"),
					})
					if !hold(a) {
						return
					}
					expected := row["expected"].(bool)
					claim := a.rule["claim"].(map[string]any)
					reason := "false"
					if expected {
						reason = "true"
					}
					if !sameJSONValue(t, claim["facts"], map[string]any{"matched": expected}) || a.rule["reason"] != reason {
						t.Fatalf("%s: matched %v reason %v, want %v", reader, claim["facts"], a.rule["reason"], expected)
					}
					if basis, ok := row["expectedBasis"]; ok && !sameJSONValue(t, claim["basis"], basis) {
						t.Fatalf("%s: basis %v, want %v", reader, claim["basis"], basis)
					}
				default:
					a := answer(nil)
					if !hold(a) {
						return
					}
					claim := a.rule["claim"].(map[string]any)
					for name, want := range row["expectedClaim"].(map[string]any) {
						if !sameJSONValue(t, claim[name], want) {
							t.Fatalf("%s: claim %s is %v, want %v", reader, name, claim[name], want)
						}
					}
				}
			}
			check("verifier", func(clauses []any) format4Answer {
				return format4Verifier(t, format4Frame(row, clauses), caseDocument, verifiedAt)
			})
			check("package", func(clauses []any) format4Answer {
				return format4Package(t, format4Frame(row, clauses), caseDocument, at)
			})
			if !unread {
				packaged++
			}
			ran++
		})
	}
	if ran != 167 {
		t.Fatalf("ran %d of the 167 rows", ran)
	}
	t.Logf("format 4.0.0 rows: %d run through the verifier, %d also read by the package (the rest name a source, or are of format 3.0.0)", ran, packaged)
}

func format4Simple(facts []any, derived []any, parameters map[string]any) map[string]any {
	if parameters == nil {
		parameters = map[string]any{}
	}
	rule := map[string]any{"ruleVersion": "2.0.0", "parameters": parameters, "clauses": []any{resolvedClause(map[string]any{"op": "always"}, facts, "r")}}
	if derived != nil {
		rule["derived"] = derived
	}
	return rule
}

func format4Document(rule any, parameters map[string]any) map[string]any {
	c := map[string]any{"facts": []any{}, "evidence": []any{}, "rule": rule}
	if parameters != nil {
		c["parameters"] = parameters
	}
	return map[string]any{"formatVersion": "4.0.0", "id": "urn:judgmentpack:dialect:format-4-vectors", "version": "0.1.0", "case": c}
}

func format4Durations(n int) []any {
	d := make([]any, n)
	for i := range d {
		d[i] = map[string]any{"name": "d" + strconv.Itoa(i), "builtin": "duration", "from": map[string]any{"field": "/a"}, "to": map[string]any{"field": "/b"}}
	}
	return d
}

func repeated(v any, n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = v
	}
	return a
}

// The bounds of §2.2.7 and §12 on the case's rule and its parameters, one
// at and one over each maximum, written by hand as the Spec's reader tests
// write them (FormatFourBoundTests): through verify-preparation's reader,
// whose reading of a placeholder preparation is preparation-differs, and
// through the sdk/dialect package, which reads the same documents.
func TestFormat4BoundsAtAndOverEachMaximum(t *testing.T) {
	const verifiedAt = "2026-11-12T18:00:00Z"
	at, _ := time.Parse("2006-01-02T15:04:05Z", verifiedAt)
	one := []any{map[string]any{"pointer": "/x", "value": 1}}
	literal := func(s string) []any { return []any{map[string]any{"pointer": "/x", "value": s}} }
	allowed := map[string]any{"allowed": map[string]any{"pointer": "/allowed", "type": "string-list"}}
	ranges := map[string]any{"ranges": map[string]any{"pointer": "/ranges", "type": "address-range-list"}}
	decimal := map[string]any{"t": map[string]any{"pointer": "/t", "type": "decimal"}}
	listDocument := format4Document(format4Simple(one, nil, map[string]any{"allowed": "string-list"}), allowed)
	rangeDocument := format4Document(format4Simple(one, nil, map[string]any{"ranges": "address-range-list"}), ranges)
	decimalDocument := format4Document(format4Simple(one, nil, map[string]any{"t": "decimal"}), decimal)
	sixtyFive := []any{}
	for i := 0; i < 64; i++ {
		sixtyFive = append(sixtyFive, resolvedClause(map[string]any{"op": "exists", "field": "/x"}, []any{map[string]any{"pointer": "/x" + strconv.Itoa(i), "value": 1}}, "r"))
	}
	sixtyFive = append(sixtyFive, resolvedClause(map[string]any{"op": "always"}, []any{}, "r"))
	listed := map[string]any{"op": "all", "of": repeated(map[string]any{"op": "exists", "field": "/x"}, 65)}
	overClauses := format4Document(map[string]any{"ruleVersion": "2.0.0", "parameters": map[string]any{}, "clauses": sixtyFive}, nil)
	overListed := format4Document(map[string]any{"ruleVersion": "2.0.0", "parameters": map[string]any{}, "clauses": []any{resolvedClause(listed, []any{}, "r"), resolvedClause(map[string]any{"op": "always"}, []any{}, "r")}}, nil)
	overLiteral := format4Document(format4Simple(literal(strings.Repeat("é", 4096)+"a"), nil, nil), nil)
	overDerived := format4Document(format4Simple(one, format4Durations(17), nil), nil)
	// A member no form admits: a document over a bound is over-bound before
	// its form is read (§11's third phase).
	withUnknown := func(document map[string]any) map[string]any {
		out := map[string]any{"unknown": 1}
		for k, v := range document {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		name     string
		document any
		caseDoc  any
		want     string
	}{
		{"16 derived values", format4Document(format4Simple(one, format4Durations(16), nil), nil), nil, ""},
		{"17 derived values", overDerived, nil, codeOverBound},
		{"17 derived values, counted before any form", withUnknown(overDerived), nil, codeOverBound},
		{"a literal of 8192 bytes", format4Document(format4Simple(literal(strings.Repeat("é", 4096)), nil, nil), nil), nil, ""},
		{"a literal of 8193 bytes", overLiteral, nil, codeOverBound},
		{"a literal of 8193 bytes, counted before any form", withUnknown(overLiteral), nil, codeOverBound},
		{"1024 list elements", listDocument, map[string]any{"allowed": repeated("a", 1024)}, ""},
		{"1025 list elements", listDocument, map[string]any{"allowed": repeated("a", 1025)}, codeOverBound},
		{"a list element of 1024 bytes", listDocument, map[string]any{"allowed": []any{strings.Repeat("é", 512)}}, ""},
		{"a list element of 1025 bytes", listDocument, map[string]any{"allowed": []any{strings.Repeat("é", 512) + "a"}}, codeOverBound},
		{"1025 elements, not all strings, measured before the type", listDocument, map[string]any{"allowed": append(repeated("a", 1024), 1)}, codeOverBound},
		{"1024 elements, not all strings", listDocument, map[string]any{"allowed": append(repeated("a", 1023), 1)}, codeParameter},
		{"1024 address ranges", rangeDocument, map[string]any{"ranges": repeated("10.0.0.0/8", 1024)}, ""},
		{"1025 address ranges", rangeDocument, map[string]any{"ranges": repeated("10.0.0.0/8", 1025)}, codeOverBound},
		{"a decimal of 128 characters", decimalDocument, map[string]any{"t": "1." + strings.Repeat("0", 126)}, ""},
		{"a decimal of 129 characters", decimalDocument, map[string]any{"t": "1." + strings.Repeat("0", 127)}, codeParameter},
		{"65 clauses", overClauses, nil, codeOverBound},
		{"65 clauses, counted before any form", withUnknown(overClauses), nil, codeOverBound},
		{"65 conditions listed by all", overListed, nil, codeOverBound},
		{"65 conditions listed by all, counted before any form", withUnknown(overListed), nil, codeOverBound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caseDocument := []byte(`{}`)
			if c.caseDoc != nil {
				caseDocument = encode(c.caseDoc)
			}
			verifier := format4Verifier(t, encode(c.document), caseDocument, verifiedAt)
			packaged := format4Package(t, encode(c.document), caseDocument, at)
			for reader, a := range map[string]format4Answer{"verifier": verifier, "package": packaged} {
				if c.want == "" && a.rule == nil || c.want != "" && a.code != c.want {
					t.Fatalf("%s: %s %s (%s), want %q", reader, a.result, a.code, a.reason, c.want)
				}
			}
		})
	}
}

// format4Mapping is the end-to-end vector case-rule-no-sources' dialect,
// read as Runner reads a dialect document.
func format4Mapping(t *testing.T) InputMapping {
	t.Helper()
	var m InputMapping
	must(t, strictJSON(format4File(t, "case-rule-no-sources/dialect.json"), &m))
	if m.FormatVersion != format4 || m.caseRule == nil {
		t.Fatal("the case's rule was not read", m.FormatVersion)
	}
	return m
}

func notAdmitted(t *testing.T, err error, code string) {
	t.Helper()
	var api *apiError
	if !errors.As(err, &api) || api.Code != code || api.Status != 422 {
		t.Fatalf("want a 422 %s refusal, got %v", code, err)
	}
	if !strings.Contains(api.Message, "spec issue #324") || !strings.Contains(api.Message, "verify-preparation") {
		t.Fatal("the refusal does not say what it waits for", api.Message)
	}
}

// No release, job, preview, delivery or workflow is on a dialect of format
// 4.0.0 (dialect_format4.go): each path refuses it by name, and only
// verify-preparation reads it.
func TestFormat4IsNotAdmittedOutsideTheVerifier(t *testing.T) {
	m := format4Mapping(t)
	notAdmitted(t, validateMappingV2(m), codeFormatNotAdmitted)
	notAdmitted(t, validateDialect(m, true), codeFormatNotAdmitted)
	_, err := releaseDialectIdentity(Release{InputMapping: &m})
	notAdmitted(t, err, codeFormatNotAdmitted)
	at, _ := time.Parse("2006-01-02T15:04:05Z", "2026-11-12T18:00:00Z")
	_, err = normalizeV2(Input{Source: &SourceInput{Mapping: m, Case: format4File(t, "case-rule-no-sources/case.json")}}, nil, at)
	notAdmitted(t, err, codeFormatNotAdmitted)

	// A release saved before this check, on such a dialect, is refused a job.
	cfg := testConfig(t)
	cfg.disableDispatcher = true
	cfg.disableAutomation = true
	s := openJournal(t, cfg)
	defer s.Close()
	r := journalRelease(t, s, s.runtimeDigest)
	r.ID = id("release_")
	r.InputMapping = &m
	r.Dialect = &DialectIdentity{m.ArtifactID, m.ArtifactVersion, mappingV2Hash(m)}
	must(t, s.saveRelease(r))
	_, err = s.createJob("Format four", r.ID)
	notAdmitted(t, err, codeFormatNotAdmitted)
}

// What this release does not read of format 4.0.0 is refused by name: its
// extended profile, until the Spec decides its literal bound (spec issue
// #324), and a binding document.
func TestFormat4ExtendedProfileAndBindingAreRefused(t *testing.T) {
	read := func(name string) []byte { return format4File(t, "case-rule-no-sources/"+name) }
	f := PreparationFixture{Dialect: read("dialect.json"), Case: read("case.json"), Sources: read("sources.json"), Profiles: read("profiles.json"), Preparation: read("preparation.json")}
	if r := VerifyPreparation(f); r.Result != ResultReading {
		t.Fatal("the vector is not read", r.Code, r.Reason)
	}
	extended := f
	extended.Dialect = bytes.Replace(f.Dialect, []byte(`"formatVersion"`), []byte(`"profile": "extended", "formatVersion"`), 1)
	if r := VerifyPreparation(extended); r.Result != ResultRefusal || r.Code != "profile-unsupported" || !strings.Contains(r.Reason, "spec issue #324") {
		t.Fatal("the extended profile is read, or refused for another reason", r.Result, r.Code, r.Reason)
	}
	base := f
	base.Dialect = bytes.Replace(f.Dialect, []byte(`"formatVersion"`), []byte(`"profile": "base", "formatVersion"`), 1)
	if r := VerifyPreparation(base); r.Result != ResultFinding || r.Code != codeDiffers {
		t.Fatal("the base profile named is not read", r.Result, r.Code, r.Reason)
	}
	bound := f
	bound.Binding = []byte(`{}`)
	if r := VerifyPreparation(bound); r.Result != ResultRefusal || r.Code != "capability-unsupported" {
		t.Fatal("a binding document is read with format 4.0.0", r.Result, r.Code, r.Reason)
	}
}

// §11's first phase scans a dialect for its profile declaration only up to
// twice the dialect bound, 262144 bytes, as the Spec's reader does
// (admit_envelope): a larger dialect of format 4.0.0 is input-over-bound
// through both readers, whatever profile it declares. A dialect of format
// 3.0.0 is scanned at any size, as before.
func TestFormat4ProfileScanStopsAtTwiceTheDialectBound(t *testing.T) {
	const verifiedAt = "2026-11-12T18:00:00Z"
	at, _ := time.Parse("2006-01-02T15:04:05Z", verifiedAt)
	base := format4File(t, "case-rule-no-sources/dialect.json")
	caseDocument := format4File(t, "case-rule-no-sources/case.json")
	// declaring is the dialect declaring profile, padded with white space to
	// size bytes, of format version.
	declaring := func(profile string, size int, version string) []byte {
		d := bytes.Replace(base, []byte(`"formatVersion": "4.0.0"`), []byte(`"formatVersion": "`+version+`"`), 1)
		head := []byte(`"profile": "` + profile + `", `)
		pad := size - len(d) - len(head)
		if pad < 0 {
			t.Fatal("the dialect is larger than the size asked for")
		}
		d = bytes.Replace(d, []byte(`"formatVersion"`), append(append(head, bytes.Repeat([]byte(" "), pad)...), []byte(`"formatVersion"`)...), 1)
		if len(d) != size || !bytes.Contains(d, []byte(`"formatVersion": "`+version+`"`)) {
			t.Fatal("the padded dialect is not of the size or format asked for", len(d))
		}
		return d
	}
	for _, c := range []struct {
		name     string
		dialect  []byte
		verifier string
		packaged string
	}{
		{"extended, at 262144 bytes", declaring("extended", maxProfileScan, "4.0.0"), "profile-unsupported", "profile-unsupported"},
		{"extended, at 262145 bytes", declaring("extended", maxProfileScan+1, "4.0.0"), codeOverBound, codeOverBound},
		{"extended, at 300 KiB", declaring("extended", 300<<10, "4.0.0"), codeOverBound, codeOverBound},
		{"an unknown profile, at 262144 bytes", declaring("other", maxProfileScan, "4.0.0"), codeMalformed, codeMalformed},
		{"an unknown profile, at 300 KiB", declaring("other", 300<<10, "4.0.0"), codeOverBound, codeOverBound},
		// Format 3.0.0 keeps its answer; the package does not read it.
		{"format 3.0.0, extended, at 300 KiB", declaring("extended", 300<<10, "3.0.0"), "profile-unsupported", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := format4Verifier(t, c.dialect, caseDocument, verifiedAt)
			if v.result != ResultRefusal || v.code != c.verifier {
				t.Fatalf("verifier: %s %s (%s), want the refusal %s", v.result, v.code, v.reason, c.verifier)
			}
			if c.packaged == "" {
				return
			}
			if p := format4Package(t, c.dialect, caseDocument, at); p.code != c.packaged {
				t.Fatalf("package: %s (%s), want %s", p.code, p.reason, c.packaged)
			}
		})
	}
}

// Format 3.0.0 reads none of what format 4.0.0 adds: a case's rule is not a
// member of its case, as before.
func TestFormat3StillRefusesACaseRule(t *testing.T) {
	m := format4Mapping(t)
	raw := bytes.Replace(encode(m), []byte(`"formatVersion":"4.0.0"`), []byte(`"formatVersion":"3.0.0"`), 1)
	var three InputMapping
	if err := strictJSON(raw, &three); err == nil {
		t.Fatal("a case's rule was read in format 3.0.0")
	}
	if !bytes.Contains(encode(m), []byte(`"rule"`)) {
		t.Fatal("format 4.0.0's case lost its rule")
	}
}

// A research source's read takes a rule of version 1.0.0 alone in format
// 4.0.0 too (spec/dialect.md §2.2.4): the hand-written research vector's
// dialect, of format 4.0.0, is read, and the same rule written as version
// 2.0.0 is refused.
func TestFormat4ResearchReadsTakeRule1Alone(t *testing.T) {
	raw, err := os.ReadFile("../../vectors/preparation/gtm-company-person-stale-and-conflicting-unknown/mapping.json")
	must(t, err)
	document := readNumbers(t, raw).(map[string]any)
	delete(document, "version")
	document["formatVersion"], document["id"], document["version"] = format4, "urn:judgmentpack:dialect:format-4-research", "0.1.0"
	four := encode(document)
	if bytes.Count(four, []byte(`"ruleVersion":"1"`)) != 1 {
		t.Fatal("the research vector's dialect is not the one this test expects")
	}
	var m InputMapping
	must(t, strictJSON(four, &m))
	if err := validateDialectMode(m, false, true); err != nil {
		t.Fatal("the research dialect of format 4.0.0 is not read", err)
	}
	var two InputMapping
	must(t, strictJSON(bytes.Replace(four, []byte(`"ruleVersion":"1"`), []byte(`"ruleVersion":"2.0.0"`), 1), &two))
	if err = validateDialectMode(two, false, true); err == nil || !strings.Contains(err.Error(), `ruleVersion is not "1"`) {
		t.Fatal("a research read's rule of version 2.0.0 was read", err)
	}
}

// A copy of format 4.0.0 writes its basis as a set, in code point order
// (spec/dialect.md §6.2: "every entry's Q, of facts and of evidence,
// distinct, ordered by code point"), and its lineage in §9.3's order, the
// targets in code point order (§2.2.6), whatever the order of its entries:
// through the verifier's reader and through the package.
func TestFormat4CopyBasisIsASetAndItsTargetsAreOrdered(t *testing.T) {
	dialect := []byte(`{"formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:format-4-copy","version":"0.1.0","case":{"facts":[{"target":"/b","source":"/x"},{"target":"/a","source":"/x"}],"evidence":[]}}`)
	caseDocument := []byte(`{"x":1}`)
	const verifiedAt = "2026-11-12T18:00:00Z"
	var recomputed *Preparation
	report := verifyPreparationUsing(PreparationFixture{Dialect: dialect, Case: caseDocument, Sources: []byte(`{}`), Profiles: []byte(`[]`), Preparation: []byte(`{"verifiedAt":"` + verifiedAt + `"}`)}, verifyAcquisition, &recomputed)
	if report.Code != codeDiffers || recomputed == nil {
		t.Fatal("the dialect is not read", report.Result, report.Code, report.Reason)
	}
	at, _ := time.Parse("2006-01-02T15:04:05Z", verifiedAt)
	d, err := sdkdialect.Read(dialect)
	must(t, err)
	packaged, err := d.Prepare(caseDocument, at)
	must(t, err)
	for reader, document := range map[string][]byte{"verifier": encode(recomputed), "package": packaged.Bytes()} {
		var p struct {
			Lineage []struct {
				Target string   `json:"target"`
				Basis  []string `json:"basis"`
			} `json:"lineage"`
			Outcomes []struct {
				Claim struct {
					Basis []string `json:"basis"`
				} `json:"claim"`
			} `json:"outcomes"`
		}
		must(t, json.Unmarshal(document, &p))
		if len(p.Lineage) != 2 || p.Lineage[0].Target != "/a" || p.Lineage[1].Target != "/b" {
			t.Fatalf("%s: lineage %+v, want /a then /b", reader, p.Lineage)
		}
		for _, basis := range [][]string{p.Lineage[0].Basis, p.Lineage[1].Basis, p.Outcomes[0].Claim.Basis} {
			if len(basis) != 1 || basis[0] != "/x" {
				t.Fatalf("%s: basis %q, want [/x]", reader, basis)
			}
		}
	}
}

// A retained-column leaf is format 3.0.0's: a copy of format 4.0.0 names a
// target and a source, or a requirement and a source, alone. The verifier
// refuses one by name, and the package finds the member outside the copy's
// form.
func TestFormat4RefusesARetainedColumnLeaf(t *testing.T) {
	dialect := []byte(`{"profile":"base","formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:format-4-column","version":"0.1.0","case":{"facts":[{"target":"/value","source":"/retained","conversion":{"contractVersion":"1","column":"X","kind":"string"}}],"evidence":[]}}`)
	caseDocument := []byte(`{"origin":{"originalValues":{"X":"a"}},"retained":{"contractVersion":"1","origin":"asserted","values":{"X":"a"},"research":null}}`)
	report := VerifyPreparation(PreparationFixture{Dialect: dialect, Case: caseDocument, Sources: []byte(`{}`), Profiles: []byte(`[]`), Preparation: []byte(`{"verifiedAt":"2026-11-12T18:00:00Z"}`)})
	if report.Code != codeMalformed || !strings.Contains(report.Reason, "retained-column leaf is read in format 3.0.0 alone") {
		t.Fatal("a retained-column leaf was read in format 4.0.0", report.Result, report.Code, report.Reason)
	}
	if _, err := sdkdialect.Read(dialect); err == nil || err.(*sdkdialect.Error).Code != sdkdialect.CodeMalformed {
		t.Fatal("the package read a retained-column leaf", err)
	}
}
