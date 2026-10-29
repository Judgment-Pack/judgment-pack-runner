package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The Runner's dialect reader and the Runtime's offline dialect document tools
// are held to the same documents (runner #86; docs/design/dialect-document-checks.md).
// The shared fixtures are the preparation vectors (vectors/preparation), which
// the specification publishes byte for byte as its dialect vectors. Each
// document goes through one Runtime invocation, here, in the tests; the
// Runner calls no Runtime document tool when it prepares or runs anything.

// dialectToolsSpecRevision is the Spec commit whose spec/dialect.md the
// Runtime's dialect document tools are pinned to: Runtime v0.39.0, the
// retained-column companion (scripts/checks/build-pinned.sh), reports it as
// specRevision. The comparison below is made at that revision. A Runtime that
// moves its pin fails these tests until this constant, and the review of the
// comparison it stands for, move with it.
const dialectToolsSpecRevision = "b7cc8906c23d92c63f4e42c4706cc1e21b9d324f"

// runtimeDialectReport is what the Runtime's dialect document tools print
// with --format json: the members these tests read.
type runtimeDialectReport struct {
	Command         string `json:"command"`
	SpecRevision    string `json:"specRevision"`
	Status          string `json:"status"`
	ValidationScope string `json:"validationScope"`
	DialectVersion  int    `json:"dialectVersion"`
	Document        string `json:"document"`
	Digest          string `json:"digest"`
	Diagnostics     []struct {
		Code string `json:"code"`
	} `json:"diagnostics"`
	Capabilities *struct {
		EncodingVersions        []int    `json:"encodingVersions"`
		ValidationVersions      []int    `json:"validationVersions"`
		ValidationSourceKinds   []string `json:"validationSourceKinds"`
		BindingVerification     bool     `json:"bindingVerification"`
		ProfileCompatibility    bool     `json:"profileCompatibility"`
		PreparationVerification bool     `json:"preparationVerification"`
	} `json:"capabilities"`
}

// codes is the report's diagnostic codes, in order.
func (r runtimeDialectReport) codes() []string {
	out := []string{}
	for _, d := range r.Diagnostics {
		out = append(out, d.Code)
	}
	return out
}

// dialectToolsRuntime is the pinned Runtime whose document tools are compared.
// CI builds it (scripts/checks/build-pinned.sh), and chain-tests fails when a
// test that needs it does not run and pass.
func dialectToolsRuntime(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("JPACK_RETAINED_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_RETAINED_TEST_BIN to the pinned Runtime (scripts/checks/build-pinned.sh)")
	}
	return bin
}

// runtimeDialect runs one Runtime dialect command on document, or on none, and
// reads its JSON report. A refusal exits non-zero and still reports.
func runtimeDialect(t *testing.T, bin, command string, document []byte) runtimeDialectReport {
	t.Helper()
	dir := t.TempDir()
	args := []string{"dialect", command}
	if document != nil {
		path := filepath.Join(dir, "dialect.json")
		if err := os.WriteFile(path, document, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, path)
	}
	cmd := exec.Command(bin, append(args, "--format", "json")...)
	cmd.Dir = dir
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	out, _ := cmd.Output()
	var r runtimeDialectReport
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("the Runtime's dialect %s printed no report: %v\n%.500s", command, err, out)
	}
	if r.Command != "dialect "+command || r.SpecRevision != dialectToolsSpecRevision {
		t.Fatalf("the Runtime's dialect %s answered %q at Spec %s, not at the pinned %s", command, r.Command, r.SpecRevision, dialectToolsSpecRevision)
	}
	return r
}

// vectorFile reads one file of a preparation vector.
func vectorFile(t *testing.T, vector, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(preparationVectors + vector + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every dialect document of the shared fixtures, version 2, version 3 bound,
// and version 3 published unbound with the dialect it is bound to at import,
// is valid to the Runtime's document tools and read by the Runner, and both
// give it the same canonical bytes and the same digest. That digest is the
// identity each fixture already records: the bound dialect's is its
// preparation's mappingDigest, and the published dialect's is the one its
// binding document names. The same holds for a named (formatVersion) dialect,
// whose digest is the published identity a release records. The Runtime
// claims no binding, profile or preparation verification: those stay the
// Runner's.
func TestDialectDocumentsAgreeWithTheRuntimesDocumentTools(t *testing.T) {
	bin := dialectToolsRuntime(t)
	contract := runtimeDialect(t, bin, "contract", nil)
	c := contract.Capabilities
	if contract.Status != "available" || c == nil ||
		!reflect.DeepEqual(c.EncodingVersions, []int{2, 3}) || !reflect.DeepEqual(c.ValidationVersions, []int{2, 3}) ||
		!reflect.DeepEqual(c.ValidationSourceKinds, []string{"operation", "selected-file", "research"}) {
		t.Fatalf("the Runtime's dialect contract is not the one compared here: %+v", c)
	}
	if c.BindingVerification || c.ProfileCompatibility || c.PreparationVerification {
		t.Fatal("the Runtime's document tools claim a binding, profile or preparation check the Runner keeps")
	}

	documents := 0
	for _, v := range readPreparationVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			names := []string{"mapping.json"}
			if v.Import != nil {
				names = append(names, v.Import.Published)
			}
			for _, name := range names {
				raw := vectorFile(t, v.Name, name)
				r := runtimeDialect(t, bin, "validate", raw)
				var m InputMapping
				if err := strictJSON(raw, &m); err != nil {
					t.Fatalf("%s: the Runner does not read a document the Runtime finds %s: %v", name, r.Status, err)
				}
				canon := canonTest(t, raw)
				if r.Status != "document-valid" || r.DialectVersion != m.Version || len(r.Diagnostics) != 0 {
					t.Fatalf("%s: the Runtime finds the Runner's version-%d document %s %v", name, m.Version, r.Status, r.codes())
				}
				if r.Document != string(canon) {
					t.Fatalf("%s: the canonical bytes differ:\nRuntime %s\nRunner  %s", name, r.Document, canon)
				}
				if r.Digest != digest(canon) || r.Digest != mappingV2Hash(m) {
					t.Fatalf("%s: the Runtime's digest %s is not the Runner's %s", name, r.Digest, mappingV2Hash(m))
				}
				switch {
				case name == "mapping.json" && v.Preparation.Retained == "equal":
					var p struct {
						MappingDigest string `json:"mappingDigest"`
					}
					if err := json.Unmarshal(vectorFile(t, v.Name, "preparation.json"), &p); err != nil || p.MappingDigest != r.Digest {
						t.Fatalf("the bound dialect's digest %s is not its preparation's mappingDigest %s", r.Digest, p.MappingDigest)
					}
				case name != "mapping.json":
					var b struct {
						Dialect string `json:"dialect"`
					}
					if err := json.Unmarshal(vectorFile(t, v.Name, v.Import.Binding), &b); err != nil || b.Dialect != r.Digest {
						t.Fatalf("the published dialect's digest %s is not the one its binding names, %s", r.Digest, b.Dialect)
					}
				}
				documents++
			}
		})
	}
	if documents < 47 {
		t.Fatalf("only %d shared documents were compared", documents)
	}

	for _, vector := range []string{"valid", "version-3-bound"} {
		var m InputMapping
		must(t, strictJSON(vectorFile(t, vector, "mapping.json"), &m))
		named := namedMapping(m, "1.0.0")
		raw := encode(named)
		r := runtimeDialect(t, bin, "validate", raw)
		identity, err := releaseDialectIdentity(Release{InputMapping: &named})
		must(t, err)
		if r.Status != "document-valid" || identity == nil || r.Digest != identity.Digest || r.Digest == mappingV2Hash(m) || r.Document != string(canonTest(t, raw)) {
			t.Fatalf("%s named: the Runtime finds %s %v with digest %s; the release records %+v", vector, r.Status, r.codes(), r.Digest, identity)
		}
	}
}

// A document the Runtime's document tools refuse is one the Runner does not
// prepare from either: each change below is made to a shared fixture the
// Runner otherwise prepares, and both refuse it.
func TestDialectDocumentRefusalsAgreeWithTheRuntimesDocumentTools(t *testing.T) {
	bin := dialectToolsRuntime(t)
	for _, vector := range []string{"valid", "version-3-bound"} {
		raw := string(vectorFile(t, vector, "mapping.json"))
		profiles, err := ParseInputProfiles(vectorFile(t, vector, "profiles.json"))
		must(t, err)
		var sources map[string]SourceValue
		must(t, strictJSON(vectorFile(t, vector, "sources.json"), &sources))
		var at time.Time
		for _, v := range readPreparationVectors(t) {
			if v.Name == vector {
				at, err = time.Parse("2006-01-02T15:04:05Z", v.VerifiedAt)
				must(t, err)
			}
		}
		prepare := func(document string) error {
			source := &SourceInput{Case: vectorFile(t, vector, "case.json"), Sources: sources}
			if err := strictJSON([]byte(document), &source.Mapping); err != nil {
				return err
			}
			_, err := prepareV2(Input{Source: source}, profiles, at, false, nil)
			return err
		}
		if err := prepare(raw); err != nil {
			t.Fatalf("%s: the Runner does not prepare the unchanged fixture: %v", vector, err)
		}
		version := `"version":2`
		if strings.Contains(raw, `"version":3`) {
			version = `"version":3`
		}
		for _, c := range []struct{ name, from, to, code string }{
			{"a duplicated member", `{"case":`, `{` + version + `,"case":`, "JPS-CARRIER-DUPLICATE-MEMBER"},
			{"an unknown version", version, `"version":4`, "JPS-DIALECT-VERSION"},
			{"a fractional number", `"maxAge":300`, `"maxAge":300.0`, "JPS-DIALECT-CANONICAL-NUMBER"},
			{"an unknown source member", `"maxAge":300`, `"maxAge":300,"extra":1`, "JPS-DIALECT-FORM"},
			{"an unknown source kind", `"kind":"operation"`, `"kind":"spreadsheet"`, "JPS-DIALECT-FORM"},
		} {
			t.Run(vector+"/"+c.name, func(t *testing.T) {
				changed := strings.Replace(raw, c.from, c.to, 1)
				if changed == raw {
					t.Fatal("the change does not apply")
				}
				r := runtimeDialect(t, bin, "validate", []byte(changed))
				if r.Status != "refused" || len(r.Diagnostics) == 0 || r.Diagnostics[0].Code != c.code {
					t.Fatalf("the Runtime finds %s %v, not refused with %s", r.Status, r.codes(), c.code)
				}
				if err := prepare(changed); err == nil {
					t.Fatalf("the Runner prepares a document the Runtime refuses with %s", c.code)
				}
			})
		}
	}
}

// Document validity is not installation readiness. Encoding a document (its
// canonical bytes and digest) says nothing of its sources, and the Runtime
// encodes a version-3 document whose source is of kind http, sql or web while
// its validator refuses that kind as unsupported; the Runner refuses each by
// name. A document the Runtime finds valid, operation or research, bound or
// published unbound, is still not prepared by the Runner without its binding
// at import, its installed profiles and, for research, its retained responses.
func TestADocumentTheRuntimeAcceptsIsNotExecutableOnThatAlone(t *testing.T) {
	bin := dialectToolsRuntime(t)
	raw := vectorFile(t, "version-3-bound", "mapping.json")
	profiles, err := ParseInputProfiles(vectorFile(t, "version-3-bound", "profiles.json"))
	must(t, err)
	var document map[string]any
	must(t, json.Unmarshal(raw, &document))
	bound := document["sources"].([]any)[0].(map[string]any)
	members := map[string]map[string]any{
		"http": {"method": "GET", "path": "/vendors", "query": map[string]any{"id": map[string]any{"$param": "invoiceAmount"}}, "headers": []any{"accept"}},
		"sql":  {"statement": "SELECT * FROM vendors WHERE id = :id", "bindings": []any{map[string]any{"name": "id", "type": "integer", "value": map[string]any{"$param": "invoiceAmount"}}}},
		"web":  {"url": "https://example.org/vendors/7"},
	}
	at := time.Now().UTC().Truncate(time.Second)
	for kind, extra := range members {
		t.Run(kind, func(t *testing.T) {
			source := map[string]any{"kind": kind}
			for _, k := range []string{"name", "profile", "profileDigest", "maxAge", "read"} {
				source[k] = bound[k]
			}
			for k, v := range extra {
				source[k] = v
			}
			changed := map[string]any{}
			for k, v := range document {
				changed[k] = v
			}
			changed["sources"] = []any{source}
			doc := canonTest(t, encode(changed))
			if r := runtimeDialect(t, bin, "digest", doc); r.Status != "encoded" || r.Digest != digest(doc) {
				t.Fatalf("the Runtime does not encode the document: %s %v", r.Status, r.codes())
			}
			if r := runtimeDialect(t, bin, "validate", doc); r.Status != "refused" || len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "JPS-DIALECT-SOURCE-UNSUPPORTED" {
				t.Fatalf("the Runtime's validator does not refuse the kind as unsupported: %s %v", r.Status, r.codes())
			}
			var m InputMapping
			must(t, strictJSON(doc, &m))
			_, err := prepareV2(Input{Source: &SourceInput{Mapping: m, Case: vectorFile(t, "version-3-bound", "case.json")}}, profiles, at, false, nil)
			if err == nil || !strings.Contains(err.Error(), "is of kind "+kind+", which this Runner does not prepare") {
				t.Fatalf("the Runner does not refuse the kind %s by name: %v", kind, err)
			}
		})
	}

	for _, vector := range []string{"version-3-bound", "unbound-bound-at-import", "gtm-company-person-valid-types-all-six", "work-item-triage-valid-types-all-six"} {
		t.Run(vector, func(t *testing.T) {
			var v preparationVector
			for _, each := range readPreparationVectors(t) {
				if each.Name == vector {
					v = each
				}
			}
			at, err := time.Parse("2006-01-02T15:04:05Z", v.VerifiedAt)
			must(t, err)
			profiles, err := ParseInputProfiles(vectorFile(t, vector, "profiles.json"))
			must(t, err)
			var sources map[string]SourceValue
			if _, err := os.Stat(preparationVectors + vector + "/sources.json"); err == nil {
				must(t, strictJSON(vectorFile(t, vector, "sources.json"), &sources))
			}
			input := func(mapping []byte, imported bool) Input {
				source := &SourceInput{Case: vectorFile(t, vector, "case.json"), Sources: sources}
				if imported {
					source.Import = &DialectImport{Dialect: vectorFile(t, vector, "unbound.json"), Binding: vectorFile(t, vector, "binding.json")}
				} else {
					must(t, strictJSON(mapping, &source.Mapping))
				}
				return Input{Source: source}
			}
			mapping := vectorFile(t, vector, "mapping.json")
			imported := v.Import != nil
			if r := runtimeDialect(t, bin, "validate", mapping); r.Status != "document-valid" {
				t.Fatalf("the Runtime does not find the bound dialect valid: %s %v", r.Status, r.codes())
			}
			if _, err := prepareV2(input(mapping, imported), profiles, at, false, nil); err != nil {
				t.Fatalf("the Runner does not prepare the fixture as it was run: %v", err)
			}
			if _, err := prepareV2(input(mapping, imported), nil, at, false, nil); err == nil {
				t.Fatal("a document the Runtime finds valid was prepared with no installed profile")
			}
			if len(sources) > 0 {
				withheld := input(mapping, imported)
				withheld.Source.Sources = nil
				if _, err := prepareV2(withheld, profiles, at, false, nil); err == nil {
					t.Fatal("a document the Runtime finds valid was prepared without its source records")
				}
			}
			if imported {
				published := vectorFile(t, vector, "unbound.json")
				if r := runtimeDialect(t, bin, "validate", published); r.Status != "document-valid" {
					t.Fatalf("the Runtime does not find the published dialect valid: %s %v", r.Status, r.codes())
				}
				if _, err := prepareV2(input(published, false), profiles, at, false, nil); err == nil {
					t.Fatal("a published unbound dialect the Runtime finds valid was prepared without its binding at import")
				}
			}
		})
	}
}
