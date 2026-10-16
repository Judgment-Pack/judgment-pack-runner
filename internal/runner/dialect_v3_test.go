package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Dialect version 3 (runner #82): a version-3 document is read, an unbound
// dialect is bound at import, and each kind of version 3 is prepared or
// refused by name. Version 2 is read as before.

// asVersion3 is a copy of the input with its mapping of version 3: the same
// sources, written as a version-3 document.
func asVersion3(i Input) Input {
	c := cloneInput(i)
	c.Source.Mapping.Version = 3
	return c
}

// refusedWith requires err to be the input problem want, exactly.
func refusedWith(t *testing.T, err error, want string) {
	t.Helper()
	var problem *apiError
	if err == nil || !errors.As(err, &problem) || problem.Message != want {
		t.Fatalf("not refused with %q: %v", want, err)
	}
}

// A version-3 document whose sources are all bound prepares as the version-2
// document with the same sources does, for an operation of shape mcp, an
// operation of shape http, and a calculator's: the same facts, evidence,
// citations, lineage and outcomes, in the same canonical form. The
// preparation's version is the document's, 3 (spec/dialect.md §9.1), and its
// mappingDigest is the digest of the document's canon bytes, written with no
// member Runner adds: version 3 carries no provider, facts or evidence. A
// version-2 document is encoded, digested and prepared as before.
func TestAVersion3DocumentPreparesAsItsVersion2Equivalent(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	mcp, mcpProfile, _, mcpAt := v2Fixture(t)
	calculated, calculator, _, calculatedAt := calculatorFixture(t)
	stand := newHTTPStandIn(t)
	for name, c := range map[string]struct {
		input   Input
		profile InputProfile
		at      time.Time
	}{
		"an operation of shape mcp":  {mcp, mcpProfile, mcpAt},
		"an operation of shape http": {stand.input(t, at), stand.profile, at},
		"a calculator's operation":   {calculated, calculator, calculatedAt},
	} {
		t.Run(name, func(t *testing.T) {
			two, err := normalizeV2(c.input, []InputProfile{c.profile}, c.at)
			if err != nil {
				t.Fatal(err)
			}
			three, err := normalizeV2(asVersion3(c.input), []InputProfile{c.profile}, c.at)
			if err != nil {
				t.Fatal("a bound version-3 document is not prepared:", err)
			}
			written := encode(asVersion3(c.input).Source.Mapping)
			for _, name := range []string{"provider", "facts", "evidence"} {
				if member(written, name) != nil {
					t.Fatalf("a version-3 document is written with %s: %s", name, written)
				}
			}
			if !bytes.Contains(encode(c.input.Source.Mapping), []byte(`"provider":"","facts":null,"evidence":null`)) {
				t.Fatal("a version-2 document is no longer written as before:", string(encode(c.input.Source.Mapping)))
			}
			if three.Preparation.Version != 3 || two.Preparation.Version != 2 {
				t.Fatalf("the preparations' versions are %d and %d, not the documents'", two.Preparation.Version, three.Preparation.Version)
			}
			if three.Preparation.MappingDigest != digest(canonTest(t, written)) || three.Preparation.MappingDigest == two.Preparation.MappingDigest {
				t.Fatalf("the version-3 preparation's mappingDigest %s is not the digest of the document as written", three.Preparation.MappingDigest)
			}
			if string(three.Facts) != string(two.Facts) || string(three.Evidence) != string(two.Evidence) {
				t.Fatalf("the facts or evidence differ: %s %s, %s %s", two.Facts, two.Evidence, three.Facts, three.Evidence)
			}
			// But for its version and its document's digest, the
			// preparation is the version-2 one, byte for byte.
			alike := *three.Preparation
			alike.Version, alike.MappingDigest = 2, two.Preparation.MappingDigest
			got, err := canonical(encode(alike))
			if err != nil {
				t.Fatal(err)
			}
			want, err := canonical(encode(two.Preparation))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the version-3 preparation is another than the version-2 one:\n%s\n%s", got, want)
			}
			again, err := normalizeV2(three, []InputProfile{c.profile}, c.at)
			if err != nil || !sameJSON(encode(again), encode(three)) {
				t.Fatal("the version-3 preparation is not repeatable:", err)
			}
		})
	}
}

// A version-3 document is kept as it is written, so that the digest Runner
// records is the document's own: one written otherwise than Runner keeps it,
// with an optional member empty, or with the version-1 members a version-2
// document carries empty, is refused by name rather than kept with another
// digest. A version-2 document is kept as before, those members written or
// not.
func TestAVersion3DocumentIsKeptAsWritten(t *testing.T) {
	input, p, _, _ := v2Fixture(t)
	s := &Service{cfg: Config{InputProfiles: []InputProfile{p}}}
	h := s.Handler("test")
	post := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/inputs/preview", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	three := asVersion3(input)
	written := encode(three.Source.Mapping)
	w := post(encode(three))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var answer struct {
		Input Input `json:"input"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || answer.Input.Preparation.MappingDigest != digest(canonTest(t, written)) {
		t.Fatalf("the digest recorded is not the document's own: %v %s", err, w.Body.String())
	}
	kept := "Mapping v3: the dialect is written otherwise than Runner keeps it, and would be kept with another digest than its own: write no optional member empty, and no provider, facts or evidence, which version 3 does not carry"
	for name, otherwise := range map[string]string{
		"provider written empty": strings.Replace(string(written), `"version":3`, `"version":3,"provider":""`, 1),
		"facts written null":     strings.Replace(string(written), `"version":3`, `"version":3,"facts":null`, 1),
		"no unmapped, written":   strings.Replace(string(written), `"version":3`, `"version":3,"unmapped":[]`, 1),
		"no parameters, written": strings.Replace(string(written), `"name":"vendor"`, `"name":"vendor","parameters":{}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			body := bytes.Replace(encode(three), written, []byte(otherwise), 1)
			if bytes.Equal(body, encode(three)) {
				t.Fatal("the document was not written otherwise")
			}
			w := post(body)
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), kept) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	two := cloneInput(input)
	body := bytes.Replace(encode(two), []byte(`,"provider":"","facts":null,"evidence":null`), nil, 1)
	if bytes.Equal(body, encode(two)) {
		t.Fatal("the version-2 document was not written without its version-1 members")
	}
	if w := post(body); w.Code != http.StatusOK {
		t.Fatal("a version-2 document written without its version-1 members is refused:", w.Code, w.Body.String())
	}
}

// postTo posts body to route, as the owner.
func postTo(h http.Handler, route string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", route, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A version-3 document is read only where each member of its path, source
// and mapping (input, source and mapping in a preview request), is named
// exactly so and none beside it is named so but for case: Go's decoder
// would read such a member as the field, and the check that the document
// is kept as written must run over what it read. The document written
// otherwise under Mapping, refused under mapping, is refused under Mapping
// too, as is one under Source, beside source, or beside mapping, before or
// after it, and under Input; the document written as Runner keeps it is
// kept with its own digest under mapping. A version-2 document under
// Mapping is read as before.
func TestAVersion3DocumentIsReadOnlyUnderItsExactNames(t *testing.T) {
	input, p, _, _ := v2Fixture(t)
	h := (&Service{cfg: Config{InputProfiles: []InputProfile{p}}}).Handler("test")
	three := asVersion3(input)
	body := encode(three)
	written := encode(three.Source.Mapping)
	otherwise := []byte(strings.Replace(string(written), `"version":3`, `"version":3,"unmapped":[]`, 1))
	at := []byte(`"mapping":` + string(written))
	if !bytes.Contains(body, at) || bytes.Equal(otherwise, written) {
		t.Fatal("the request does not name the document at source.mapping:", string(body))
	}
	renamed := func(name string, document []byte) []byte {
		return bytes.Replace(body, at, []byte(`"`+name+`":`+string(document)), 1)
	}
	kept := "Mapping v3: the dialect is written otherwise than Runner keeps it, and would be kept with another digest than its own: write no optional member empty, and no provider, facts or evidence, which version 3 does not carry"
	cased := func(member, name, path string) string {
		return "Mapping v3: the request names the member " + member + ", which is " + name + " but for case: a dialect of version 3 is read only at " + path + ", each member of the path named exactly so and none beside it named so but for case, so that the document Runner keeps is the one written"
	}

	w := postTo(h, "/v1/inputs/preview", body)
	var answer struct {
		Input Input `json:"input"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Input.Preparation.MappingDigest != digest(canonTest(t, written)) {
		t.Fatal("the document under mapping is not kept with its own digest:", w.Code, w.Body.String())
	}
	for name, c := range map[string]struct {
		route, body, want string
	}{
		"written otherwise, under mapping":  {"/v1/inputs/preview", string(renamed("mapping", otherwise)), kept},
		"written otherwise, under Mapping":  {"/v1/inputs/preview", string(renamed("Mapping", otherwise)), cased("Mapping", "mapping", "source.mapping")},
		"under MAPPING, to the next step":   {"/v1/inputs/next", string(renamed("MAPPING", written)), cased("MAPPING", "mapping", "source.mapping")},
		"under Source":                      {"/v1/inputs/preview", strings.Replace(string(body), `{"source":`, `{"Source":`, 1), cased("Source", "source", "source.mapping")},
		"a Source beside source":            {"/v1/inputs/preview", strings.TrimSuffix(string(body), "}") + `,"Source":{"mapping":` + string(otherwise) + `}}`, cased("Source", "source", "source.mapping")},
		"a Mapping after mapping":           {"/v1/inputs/preview", string(renamed("mapping", []byte(string(written)+`,"Mapping":`+string(otherwise)))), cased("Mapping", "mapping", "source.mapping")},
		"a Mapping before mapping":          {"/v1/inputs/preview", string(renamed("Mapping", []byte(string(otherwise)+`,"mapping":`+string(written)))), cased("Mapping", "mapping", "source.mapping")},
		"under Input, in a preview request": {"/v1/previews", `{"pack":"","Input":` + string(body) + `}`, cased("Input", "input", "input.source.mapping")},
	} {
		t.Run(name, func(t *testing.T) {
			w := postTo(h, c.route, []byte(c.body))
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"code":"invalid_source_input"`) || !strings.Contains(w.Body.String(), c.want) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	two := encode(input)
	underCase := bytes.Replace(two, []byte(`"mapping":`), []byte(`"Mapping":`), 1)
	if bytes.Equal(underCase, two) {
		t.Fatal("the version-2 request does not name its mapping")
	}
	if w := postTo(h, "/v1/inputs/preview", underCase); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Input.Preparation.MappingDigest != mappingV2Hash(input.Source.Mapping) {
		t.Fatal("a version-2 document under Mapping is not read as before:", w.Code, w.Body.String())
	}
}

// importFixture is v2Fixture's mapping published as a version-3 dialect
// whose one source is unbound, the binding document that binds it to the
// fixture's profile, and the same dialect written bound, directly.
type importFixture struct {
	input     Input
	profile   InputProfile
	at        time.Time
	published InputMapping
	direct    InputMapping
}

func newImportFixture(t *testing.T) importFixture {
	t.Helper()
	input, p, _, at := v2Fixture(t)
	direct := input.Source.Mapping
	direct.Version = 3
	return importFixture{input, p, at, unboundOf(direct, "vendor", "postgres", "vendors.read", "mcp", "dbhub"), direct}
}

// unboundOf is m with its source name unbound: its profile replaced by the
// platform, operation, shape and record an installation binds.
func unboundOf(m InputMapping, name, platform, operation, shape, record string) InputMapping {
	m.Sources = append([]MappingSource(nil), m.Sources...)
	for i, s := range m.Sources {
		if s.Name == name {
			s.Profile, s.ProfileDigest = "", ""
			s.Platform, s.Operation, s.Shape, s.Record = encode(platform), encode(operation), encode(shape), encode(record)
			m.Sources[i] = s
		}
	}
	return m
}

// bindingOf is the binding document that binds each source named to its
// profile, for the dialect published.
func bindingOf(t *testing.T, published []byte, profiles map[string]InputProfile) []byte {
	t.Helper()
	sources := map[string]any{}
	for name, p := range profiles {
		sources[name] = map[string]string{"profile": p.ID, "profileDigest": profileHash(p)}
	}
	return encode(map[string]any{"dialect": digest(canonTest(t, published)), "sources": sources})
}

// imported is the fixture's input with its mapping named by an import alone.
func (f importFixture) imported(t *testing.T) Input {
	t.Helper()
	i := cloneInput(f.input)
	published := encode(f.published)
	i.Source.Mapping = InputMapping{}
	i.Source.Import = &DialectImport{Dialect: published, Binding: bindingOf(t, published, map[string]InputProfile{"vendor": f.profile})}
	return i
}

// The import step binds an unbound dialect to this installation's profiles
// through a binding document, and makes the bound dialect: the dialect
// written bound directly, with the same digest. A run of it keeps the bound
// dialect as its mapping and the documents beside it, as given; its
// preparation is the directly bound dialect's, byte for byte, and holds
// neither document's digest nor the catalog names the binding removed. The
// stored input prepares again, its import bound again, as a dispatcher or
// verify-run prepares it; a mapping that is not the bound dialect its import
// makes is refused. The API takes the import alone.
func TestTheImportStepBindsAnUnboundDialect(t *testing.T) {
	f := newImportFixture(t)
	imported := f.imported(t)
	bound, err := bindDialect(*imported.Source.Import, []InputProfile{f.profile})
	if err != nil {
		t.Fatal(err)
	}
	if mappingV2Hash(bound) != mappingV2Hash(f.direct) || !sameJSON(encode(bound), encode(f.direct)) {
		t.Fatalf("the bound dialect is not the one written bound directly:\n%s\n%s", encode(bound), encode(f.direct))
	}
	prepared, err := normalizeV2(imported, []InputProfile{f.profile}, f.at)
	if err != nil {
		t.Fatal(err)
	}
	direct := cloneInput(f.input)
	direct.Source.Mapping = f.direct
	want, err := normalizeV2(direct, []InputProfile{f.profile}, f.at)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := canonical(encode(prepared.Preparation))
	held, _ := canonical(encode(want.Preparation))
	if !bytes.Equal(got, held) || prepared.Preparation.MappingDigest != mappingV2Hash(f.direct) {
		t.Fatalf("the imported dialect's preparation is not the directly bound dialect's:\n%s\n%s", got, held)
	}
	if !sameJSON(encode(prepared.Source.Mapping), encode(f.direct)) || prepared.Source.Import == nil ||
		!bytes.Equal(prepared.Source.Import.Dialect, imported.Source.Import.Dialect) || !bytes.Equal(prepared.Source.Import.Binding, imported.Source.Import.Binding) {
		t.Fatal("the run does not keep the bound dialect, and the documents beside it as given:", string(encode(prepared.Source)))
	}
	published, binding := digest(canonTest(t, prepared.Source.Import.Dialect)), digest(canonTest(t, prepared.Source.Import.Binding))
	for _, kept := range []string{published, binding, strings.TrimPrefix(published, "sha256:"), strings.TrimPrefix(binding, "sha256:"), `"platform":`, `"operation":`, `"shape":`, `"record":`, "vendors.read", "postgres", "dbhub"} {
		if bytes.Contains(got, []byte(kept)) {
			t.Fatalf("the preparation holds %s, which the import keeps beside it: %s", kept, got)
		}
	}
	if published == mappingV2Hash(f.direct) {
		t.Fatal("the published dialect's digest is the bound dialect's")
	}
	again, err := normalizeV2(prepared, []InputProfile{f.profile}, f.at)
	if err != nil || !sameJSON(encode(again), encode(prepared)) {
		t.Fatal("the stored input does not prepare again:", err)
	}
	other := cloneInput(prepared)
	other.Source.Mapping.Sources[0].MaxAge = 299
	_, err = normalizeV2(other, []InputProfile{f.profile}, f.at)
	refusedWith(t, err, "Mapping v3: the mapping is not the bound dialect its import makes: name the import alone, or the import and the mapping it binds")

	s := &Service{cfg: Config{InputProfiles: []InputProfile{f.profile}}}
	w := postTo(s.Handler("test"), "/v1/inputs/preview", importRequest(t, imported, "mapping", nil))
	var answer struct {
		Input Input `json:"input"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Input.Preparation.MappingDigest != mappingV2Hash(f.direct) || answer.Input.Source.Import == nil {
		t.Fatal("the API does not take the import alone:", w.Code, w.Body.String())
	}
}

// importRequest is the request body of the input imported, its source's
// mapping member as given: absent when mapping is nil.
func importRequest(t *testing.T, imported Input, name string, mapping []byte) []byte {
	t.Helper()
	var request map[string]map[string]json.RawMessage
	if err := json.Unmarshal(encode(imported), &request); err != nil || request["source"] == nil {
		t.Fatal("the imported input is not a request with a source:", err)
	}
	delete(request["source"], "mapping")
	if mapping != nil {
		request["source"][name] = mapping
	}
	return encode(request)
}

// Beside an import, the bound dialect becomes the mapping only when the
// request names no mapping member: a mapping given, whatever its value, is
// held to the bound dialect, byte for byte in canonical form. The empty
// object, null and an empty document of version 0, under mapping or under
// a name that is mapping but for case, are refused, naming the inequality,
// at preview and at the next step; the bound dialect given is taken, with
// the digest of the import alone.
func TestAMappingGivenBesideAnImportIsTheBoundDialect(t *testing.T) {
	f := newImportFixture(t)
	imported := f.imported(t)
	h := (&Service{cfg: Config{InputProfiles: []InputProfile{f.profile}}}).Handler("test")
	digestOf := func(body []byte) string {
		t.Helper()
		w := postTo(h, "/v1/inputs/preview", body)
		var answer struct {
			Input Input `json:"input"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Input.Preparation == nil {
			t.Fatal("the import is not taken:", w.Code, w.Body.String())
		}
		return answer.Input.Preparation.MappingDigest
	}
	alone := digestOf(importRequest(t, imported, "mapping", nil))
	if alone != mappingV2Hash(f.direct) {
		t.Fatalf("the import alone is prepared with the digest %s, not the bound dialect's %s", alone, mappingV2Hash(f.direct))
	}
	if given := digestOf(importRequest(t, imported, "mapping", encode(f.direct))); given != alone {
		t.Fatalf("the bound dialect given beside its import is prepared with the digest %s, not the import alone's %s", given, alone)
	}
	refused := `"code":"invalid_source_input","message":"Mapping v3: the mapping is not the bound dialect its import makes: name the import alone, or the import and the mapping it binds"`
	for name, c := range map[string]struct {
		member, mapping string
	}{
		"the empty object":             {"mapping", `{}`},
		"null":                         {"mapping", `null`},
		"a document of version 0":      {"mapping", `{"version":0}`},
		"the empty object, as Mapping": {"Mapping", `{}`},
	} {
		for _, route := range []string{"/v1/inputs/preview", "/v1/inputs/next"} {
			t.Run(name+" "+route, func(t *testing.T) {
				w := postTo(h, route, importRequest(t, imported, c.member, []byte(c.mapping)))
				if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), refused) {
					t.Fatal(w.Code, w.Body.String())
				}
			})
		}
	}
}

// An unbound dialect given as the mapping is refused by name: Runner
// prepares only a bound dialect, and binds one only at import.
func TestAnUnboundSourceIsNotPreparedUnbound(t *testing.T) {
	f := newImportFixture(t)
	i := cloneInput(f.input)
	i.Source.Mapping = f.published
	_, err := normalizeV2(i, []InputProfile{f.profile}, f.at)
	refusedWith(t, err, "Mapping v3: source vendor is unbound: it names the platform, operation, shape and record an installation binds, and no input profile. Import the dialect with a binding document (source.import), which binds it to an input profile of this installation, before it is prepared")
}

// The import step refuses by name whatever it does not bind: a source left
// unbound, a binding of a source the dialect does not have or of one that
// is bound already, a profile this installation does not hold by that id and
// digest, a profile of another shape or record than the source names, a
// binding that would change anything but the profile assignments, a binding
// for another dialect, a dialect with no unbound source, of version 2, or
// written otherwise than Runner keeps it, an unbound source not of its form,
// and an import without its binding.
func TestTheImportStepRefusesByName(t *testing.T) {
	f := newImportFixture(t)
	profiles := []InputProfile{f.profile}
	published := encode(f.published)
	entry := map[string]string{"profile": f.profile.ID, "profileDigest": profileHash(f.profile)}
	withBinding := func(b map[string]any) DialectImport {
		if _, ok := b["dialect"]; !ok {
			b["dialect"] = digest(canonTest(t, published))
		}
		return DialectImport{Dialect: published, Binding: encode(b)}
	}
	withDialect := func(m InputMapping, sources map[string]any) DialectImport {
		d := encode(m)
		return DialectImport{Dialect: d, Binding: encode(map[string]any{"dialect": digest(canonTest(t, d)), "sources": sources})}
	}
	shaped := f.profile
	shaped.Shape, shaped.Tools = "http", nil
	recorded := f.profile
	recorded.Adapter.Name = "another-adapter"
	twoSources := f.published
	twoSources.Sources = append(append([]MappingSource(nil), f.published.Sources...), MappingSource{Name: "file", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &CopyMapping{[]FactMapping{{"/file", "/file"}}, []EvidenceMapping{}}}})
	partial := f.published
	partial.Sources = append([]MappingSource(nil), f.published.Sources...)
	partial.Sources[0].Record = nil
	profiled := f.published
	profiled.Sources = append([]MappingSource(nil), f.published.Sources...)
	profiled.Sources[0].Profile = f.profile.ID
	shapeless := unboundOf(f.direct, "vendor", "postgres", "vendors.read", "ftp", "dbhub")
	two := f.direct
	two.Version = 2
	for name, c := range map[string]struct {
		imp      DialectImport
		profiles []InputProfile
		want     string
	}{
		"a source left unbound":                 {withBinding(map[string]any{"sources": map[string]any{}}), profiles, "the binding document leaves source vendor unbound: it binds every unbound source of the dialect"},
		"a source the dialect does not have":    {withBinding(map[string]any{"sources": map[string]any{"vendor": entry, "ghost": entry}}), profiles, "the binding document binds ghost, which is not a source of the dialect"},
		"a source that is not unbound":          {withDialect(twoSources, map[string]any{"vendor": entry, "file": entry}), profiles, "the binding document binds file, which is not an unbound source: a binding binds unbound sources only"},
		"a profile this installation lacks":     {withBinding(map[string]any{"sources": map[string]any{"vendor": map[string]string{"profile": "elsewhere", "profileDigest": profileHash(f.profile)}}}), profiles, "the binding document binds source vendor to the profile elsewhere, which this installation does not hold"},
		"a profile by another digest":           {withBinding(map[string]any{"sources": map[string]any{"vendor": map[string]string{"profile": f.profile.ID, "profileDigest": digest([]byte("another"))}}}), profiles, "the binding document binds source vendor to the profile vendors by a digest that is not the digest of this installation's profile vendors"},
		"a profile of another shape":            {withBinding(map[string]any{"sources": map[string]any{"vendor": map[string]string{"profile": shaped.ID, "profileDigest": profileHash(shaped)}}}), []InputProfile{shaped}, "the binding document binds source vendor, served by shape mcp, to the profile vendors, of shape http"},
		"a profile of another record":           {withBinding(map[string]any{"sources": map[string]any{"vendor": map[string]string{"profile": recorded.ID, "profileDigest": profileHash(recorded)}}}), []InputProfile{recorded}, "the binding document binds source vendor, written against the record dbhub, to the profile vendors, whose adapter is another-adapter"},
		"a binding that changes a read":         {withBinding(map[string]any{"sources": map[string]any{"vendor": map[string]any{"profile": f.profile.ID, "profileDigest": profileHash(f.profile), "read": map[string]any{"copy": map[string]any{"facts": []any{}, "evidence": []any{}}}}}}), profiles, "the binding document gives source vendor the member read: it binds a source to a profile, by its id and digest, and changes nothing else in the dialect"},
		"a binding that changes a maxAge":       {withBinding(map[string]any{"sources": map[string]any{"vendor": map[string]any{"profile": f.profile.ID, "profileDigest": profileHash(f.profile), "maxAge": 86400}}}), profiles, "the binding document gives source vendor the member maxAge: it binds a source to a profile, by its id and digest, and changes nothing else in the dialect"},
		"a binding that changes the case":       {withBinding(map[string]any{"sources": map[string]any{"vendor": entry}, "case": map[string]any{}}), profiles, "the binding document has the member case: it is an object of exactly dialect and sources, which binds each unbound source to a profile and changes nothing else in the dialect"},
		"a binding for another dialect":         {withBinding(map[string]any{"dialect": mappingV2Hash(f.direct), "sources": map[string]any{"vendor": entry}}), profiles, "the binding document binds the dialect " + mappingV2Hash(f.direct) + ", and this one's digest is " + digest(canonTest(t, published))},
		"a binding with no profile":             {withBinding(map[string]any{"sources": map[string]any{"vendor": map[string]string{"profileDigest": profileHash(f.profile)}}}), profiles, "the binding document gives source vendor no profile: an id and a digest"},
		"a dialect with no unbound source":      {withDialect(f.direct, map[string]any{}), profiles, "the dialect has no unbound source, and a binding document beside it binds nothing: give the dialect as the mapping"},
		"a dialect of version 2":                {withDialect(two, map[string]any{}), profiles, "the published dialect is of version 2: only a dialect of version 3 has unbound sources to bind, and one of version 2 is given as the mapping"},
		"a dialect written otherwise":           {DialectImport{Dialect: []byte(strings.Replace(string(published), `"version":3`, `"version":3,"provider":""`, 1)), Binding: encode(map[string]any{"dialect": digest(canonTest(t, []byte(strings.Replace(string(published), `"version":3`, `"version":3,"provider":""`, 1)))), "sources": map[string]any{"vendor": entry}})}, profiles, "the dialect is written otherwise than Runner keeps it, and would be kept with another digest than its own: write no optional member empty, and no provider, facts or evidence, which version 3 does not carry"},
		"an unbound source with no record":      {withDialect(partial, map[string]any{"vendor": entry}), profiles, "source vendor is an unbound operation, which names its platform, operation, shape and record, all four, and no profile or profile digest"},
		"an unbound source with a profile":      {withDialect(profiled, map[string]any{"vendor": entry}), profiles, "source vendor is an unbound operation, which names its platform, operation, shape and record, all four, and no profile or profile digest"},
		"an unbound source of no adapter shape": {withDialect(shapeless, map[string]any{"vendor": entry}), profiles, "source vendor is an unbound operation, whose platform, operation and record are non-empty strings and whose shape is airbyte, mcp or http"},
		"an import without its binding":         {DialectImport{Dialect: published}, profiles, "an import names the published dialect and its binding document"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := bindDialect(c.imp, c.profiles)
			refusedWith(t, err, "Mapping v3: "+c.want)
			i := cloneInput(f.input)
			i.Source.Mapping = InputMapping{}
			imp := c.imp
			i.Source.Import = &imp
			_, err = normalizeV2(i, c.profiles, f.at)
			refusedWith(t, err, "Mapping v3: "+c.want)
		})
	}
}

// v3KindSource is a version-3 source of kind, with the members the
// specification gives that kind (spec/dialect.md §4.3), pinned to p.
func v3KindSource(kind string, p InputProfile) MappingSource {
	s := MappingSource{Name: "asked", Kind: kind, Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Read: SourceRead{Copy: &CopyMapping{[]FactMapping{{"/asked", "/answer"}}, []EvidenceMapping{}}}}
	switch kind {
	case "http":
		s.Method, s.Path, s.Query, s.Headers = json.RawMessage(`"GET"`), json.RawMessage(`"/vendors"`), json.RawMessage(`{"id":{"$param":"vendorId"}}`), json.RawMessage(`["accept"]`)
	case "sql":
		s.Statement, s.Bindings = json.RawMessage(`"SELECT * FROM vendors WHERE id = :id"`), json.RawMessage(`[{"name":"id","type":"integer","value":{"$param":"vendorId"}}]`)
	case "web":
		s.URL = json.RawMessage(`"https://example.org/vendors/7"`)
	case "research":
		s.Question, s.Constraints, s.Model, s.Evidence = json.RawMessage(`{"$text":"Who supplies vendor {{vendorId}}?"}`), json.RawMessage(`{}`), json.RawMessage(`"a-model"`), json.RawMessage(`["supplier"]`)
	}
	return s
}

// refusesKind requires a version-3 document with a source of kind, as the
// specification writes one, to be refused with want: as the mapping, at
// import, and through the API; and a version-2 document naming the kind to
// be refused as it was.
func refusesKind(t *testing.T, kind, want string) {
	t.Helper()
	input, p, _, at := v2Fixture(t)
	i := asVersion3(input)
	i.Source.Mapping.Sources = []MappingSource{v3KindSource(kind, p)}
	i.Source.Sources = map[string]SourceValue{"asked": input.Source.Sources["vendor"]}
	_, err := normalizeV2(i, []InputProfile{p}, at)
	refusedWith(t, err, want)
	unbound := unboundOf(i.Source.Mapping, "vendor", "postgres", "vendors.read", "mcp", "dbhub")
	unbound.Sources = append(unbound.Sources, unboundOf(input.Source.Mapping, "vendor", "postgres", "vendors.read", "mcp", "dbhub").Sources...)
	unbound.Version = 3
	published := encode(unbound)
	_, err = bindDialect(DialectImport{Dialect: published, Binding: bindingOf(t, published, map[string]InputProfile{"vendor": p})}, []InputProfile{p})
	refusedWith(t, err, want)
	s := &Service{cfg: Config{InputProfiles: []InputProfile{p}}}
	req := httptest.NewRequest("POST", "/v1/inputs/preview", bytes.NewReader(encode(i)))
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	s.Handler("test").ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), want) {
		t.Fatal("the API does not refuse the kind by name:", w.Code, w.Body.String())
	}
	two := cloneInput(i)
	two.Source.Mapping.Version = 2
	two.Source.Mapping.Sources[0].Method, two.Source.Mapping.Sources[0].Path, two.Source.Mapping.Sources[0].Query, two.Source.Mapping.Sources[0].Headers = nil, nil, nil, nil
	two.Source.Mapping.Sources[0].Statement, two.Source.Mapping.Sources[0].Bindings, two.Source.Mapping.Sources[0].URL = nil, nil, nil
	two.Source.Mapping.Sources[0].Question, two.Source.Mapping.Sources[0].Constraints, two.Source.Mapping.Sources[0].Model, two.Source.Mapping.Sources[0].Evidence = nil, nil, nil, nil
	_, err = normalizeV2(two, []InputProfile{p}, at)
	refusedWith(t, err, "Mapping v2: unsupported source kind/provider")
}

// The kind operation of version 3 is version 2's, of every adapter shape,
// and prepared as version 2 prepares it
// (TestAVersion3DocumentPreparesAsItsVersion2Equivalent): bound, or unbound
// and bound at import (TestTheImportStepBindsAnUnboundDialect). A member of
// another kind beside it is refused, not read as part of its request.
func TestTheVersion3KindOperationIsPrepared(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	three := asVersion3(input)
	if _, err := normalizeV2(three, []InputProfile{p}, at); err != nil {
		t.Fatal(err)
	}
	three.Source.Mapping.Sources[0].Method = json.RawMessage(`"GET"`)
	_, err := normalizeV2(three, []InputProfile{p}, at)
	refusedWith(t, err, "Mapping v3: source vendor has the member method, which a source of kind operation does not have")
}

// The kind selected-file of version 3 is version 2's, read from the
// Runner's selected snapshot, which the specification does not restate:
// a local file, asserted, and a Drive file, verified under its profile, are
// prepared as version 2 prepares them.
func TestTheVersion3KindSelectedFileIsPrepared(t *testing.T) {
	local := fileInput(string(encode(sample())), InputMapping{Version: 1, Provider: "local-file"})
	copy := CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}}}
	at := time.Now().UTC().Truncate(time.Second)
	for _, version := range []int{2, 3} {
		input := Input{Source: &SourceInput{Mapping: InputMapping{Version: version, Sources: []MappingSource{{Name: "file", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &copy}}}}, Sources: map[string]SourceValue{"file": {Snapshot: local.Source.Snapshot}}}}
		got, err := normalizeV2(input, nil, at)
		if err != nil || string(got.Facts) != `{"request":`+string(valueAt(t, sample().Facts, "/request"))+`}` || string(got.Evidence) != `{"intake-form":"present"}` || got.Preparation.Version != version {
			t.Fatalf("version %d: a local file is not prepared: %v %s", version, err, got.Facts)
		}
	}
	drive, p, at := driveInput(t)
	for _, version := range []int{2, 3} {
		input := cloneInput(drive)
		input.Source.Mapping.Version = version
		got, err := normalizeV2(input, []InputProfile{p}, at)
		if err != nil || string(got.Facts) != `{"approved":false}` || got.Preparation.Version != version || len(got.Preparation.Cites) != 1 {
			t.Fatalf("version %d: a Drive file is not prepared: %v %s", version, err, got.Facts)
		}
	}
	three := asVersion3(drive)
	three.Source.Mapping.Sources[0].URL = json.RawMessage(`"https://example.org"`)
	_, err := normalizeV2(three, []InputProfile{p}, at)
	refusedWith(t, err, "Mapping v3: source file has the member url, which a source of kind selected-file does not have")
}

// valueAt is the value at pointer in raw.
func valueAt(t *testing.T, raw []byte, pointer string) []byte {
	t.Helper()
	v, err := readInputJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	tokens, _ := pointerTokens(pointer, true)
	at, ok := pointerRead(v, tokens)
	if !ok {
		t.Fatal("no value at", pointer)
	}
	return encode(at)
}

// driveInput is a mapping of one selected Drive file, verified under its
// drive profile, as TestV2DriveVerifiesSelectedBytesAndRequest makes one.
func driveInput(t *testing.T) (Input, InputProfile, time.Time) {
	t.Helper()
	p, key := testProfile(t)
	p.ID, p.Source, p.Shape, p.Tools = "drive", "drive", "http", nil
	at := time.Now().UTC().Truncate(time.Second)
	selected := fileInput(`{"approved":false}`, InputMapping{Version: 1, Provider: "local-file"})
	var snap map[string]any
	_ = json.Unmarshal(selected.Source.Snapshot, &snap)
	delete(snap, "selectedAt")
	original := snap["original"].(map[string]any)
	grant := strings.Repeat("b", 64)
	args := encode(map[string]string{"fileId": "file-A", "grant": grant})
	result := encode(map[string]any{"document": map[string]any{"id": original["sha256"], "name": original["name"], "mediaType": original["mediaType"], "size": len(`{"approved":false}`)}, "original": map[string]any{"bytes": original["bytes"]}, "provenance": map[string]any{"observedAt": at.Format(time.RFC3339), "source": map[string]string{"kind": "google-drive", "fileId": "file-A", "version": "7"}}})
	response := signResponse(t, p, key, args, result, at, 0)
	snap["proof"] = map[string]any{"session": "mapping.session", "source": "drive", "authority": p.Authority, "publicKey": p.PublicKey, "response": string(response), "registry": "", "drive": map[string]string{"fileId": "file-A", "grant": grant}}
	mapping := InputMapping{Version: 2,
		Case: &CaseMapping{Parameters: map[string]Parameter{"fileId": {Pointer: "/fileId", Type: "string"}, "grant": {Pointer: "/grant", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{{Name: "file", Kind: "selected-file", Provider: "google-drive", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300,
			Arguments: json.RawMessage(`{"fileId":{"$param":"fileId"},"grant":{"$param":"grant"}}`),
			Read:      SourceRead{Copy: &CopyMapping{[]FactMapping{{"/approved", "/approved"}}, []EvidenceMapping{}}},
		}},
	}
	return Input{Source: &SourceInput{Mapping: mapping, Case: args, Sources: map[string]SourceValue{"file": {Snapshot: encode(snap)}}}}, p, at
}

// The kind http of version 3 is refused by name: a gateway source of shape
// http is read as an operation.
func TestTheVersion3KindHTTPIsRefusedByName(t *testing.T) {
	refusesKind(t, "http", "Mapping v3: source asked is of kind http, which this Runner does not prepare: a gateway source of shape http is read as an operation")
}

// The kind sql of version 3 is refused by name.
func TestTheVersion3KindSQLIsRefusedByName(t *testing.T) {
	refusesKind(t, "sql", "Mapping v3: source asked is of kind sql, which this Runner does not prepare: a connection's statement is read as an operation of the shape its gateway source serves")
}

// The kind web of version 3 is refused by name.
func TestTheVersion3KindWebIsRefusedByName(t *testing.T) {
	refusesKind(t, "web", "Mapping v3: source asked is of kind web, which this Runner does not prepare: the gateway's web source is read as an operation of shape http")
}

// The kind research of version 3 is prepared as research, and never read as
// an operation: a research source with an operation's arguments is refused,
// and so is one without its recipe (research_test.go holds the kind's
// preparation).
func TestTheVersion3KindResearchIsNotReadAsAnOperation(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	i := asVersion3(input)
	asked := v3KindSource("research", p)
	asked.Arguments = input.Source.Mapping.Sources[0].Arguments
	i.Source.Mapping.Sources = []MappingSource{asked}
	i.Source.Sources = map[string]SourceValue{"asked": input.Source.Sources["vendor"]}
	_, err := normalizeV2(i, []InputProfile{p}, at)
	refusedWith(t, err, "Mapping v3: source asked: a research source names no provider, arguments or calculation: its request is its recipe (input-malformed)")
	asked.Arguments = nil
	i.Source.Mapping.Sources = []MappingSource{asked}
	_, err = normalizeV2(i, []InputProfile{p}, at)
	refusedWith(t, err, "Mapping v3: source asked: a research source names its question, constraints, model, evidence, fields and capabilities, and fields is missing (input-malformed)")
}

// A version-2 document has no member of version 3: one that names one is
// refused, as it was, now by name.
func TestAVersion2SourceHasNoMemberOfVersion3(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	for name, set := range map[string]func(*MappingSource){
		"platform": func(s *MappingSource) { s.Platform = json.RawMessage(`"postgres"`) },
		"record":   func(s *MappingSource) { s.Record = json.RawMessage(`null`) },
		"method":   func(s *MappingSource) { s.Method = json.RawMessage(`"GET"`) },
		"model":    func(s *MappingSource) { s.Model = json.RawMessage(`""`) },
	} {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			set(&i.Source.Mapping.Sources[0])
			_, err := normalizeV2(i, []InputProfile{p}, at)
			refusedWith(t, err, "Mapping v2: source vendor has the member "+name+", which only a source of dialect version 3 has")
		})
	}
}

// A run of a version-3 document whose sources are all bound is evaluated,
// signed, witnessed and bound, and verified offline, as a version-2 run is.
// The export in testdata/mapping-v3-bound was written by this test, under
// Runtime 0.31.0, with JPACK_V3_BOUND_EXPORT_DIR set, and the vector
// version-3-bound was generated from it.
func TestAVersion3RunIsSignedWitnessedAndBound(t *testing.T) {
	bundle, profile := calculatedSignedExport(t, "computed", func(i *Input) { i.Source.Mapping.Version = 3 })
	v, err := VerifyInputs(encode(bundle), []InputProfile{profile}, bundle.ReleaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Run.Input.Preparation.Version != 3 || bundle.Release.InputMapping.Version != 3 || member(encode(bundle.Run.Input.Source.Mapping), "provider") != nil {
		t.Fatalf("the run is not of a version-3 document: %s", encode(bundle.Run.Input.Source.Mapping))
	}
	if runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN")) && (v.Binding == nil || v.Binding.Status != BindingMatches) {
		t.Fatalf("the record does not bind the preparation: %+v", v.Binding)
	}
	writeBoundFixture(t, os.Getenv("JPACK_V3_BOUND_EXPORT_DIR"), bundle, profile)
}

// A run of a dialect bound at import is evaluated, signed, witnessed and
// bound, and verified offline: verify-run binds the import again, under the
// profiles it trusts, and holds the run's mapping to the bound dialect. The
// export keeps the published dialect and the binding document beside the
// run, and its preparation holds neither digest. The export in
// testdata/mapping-v3-imported was written by this test, under Runtime
// 0.31.0, with JPACK_V3_IMPORT_EXPORT_DIR set, and the vector
// unbound-bound-at-import was generated from it.
func TestAnImportedRunIsSignedWitnessedAndBound(t *testing.T) {
	cfg := testConfig(t)
	keyPath := signingKey(t)
	cfg.SigningKey = keyPath
	cfg.Runtime = signingRuntimeWithKey(t, cfg.Runtime, t.TempDir()+"/signing.log", keyPath)
	stand := newHTTPStandIn(t)
	cfg.InputProfiles = []InputProfile{stand.profile}
	input := stand.input(t, time.Now().UTC().Truncate(time.Second))
	direct := input.Source.Mapping
	direct.Version = 3
	published := encode(unboundOf(direct, "intake", "intake-api", "requests.read", "http", "adapter-http"))
	input.Source.Mapping = InputMapping{}
	input.Source.Import = &DialectImport{Dialect: published, Binding: bindingOf(t, published, map[string]InputProfile{"intake": stand.profile})}
	bundle := signedAndWitnessedExport(t, cfg, input, stand.profile, "An imported dialect, signed and witnessed", "imported")
	v, err := VerifyInputs(encode(bundle), cfg.InputProfiles, bundle.ReleaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(encode(bundle.Run.Input.Source.Mapping), encode(direct)) || bundle.Run.Input.Source.Import == nil || bundle.Run.Input.Preparation.MappingDigest != mappingV2Hash(direct) {
		t.Fatal("the run does not keep the bound dialect, and its import beside it")
	}
	prepared, _ := canonical(encode(bundle.Run.Input.Preparation))
	for _, document := range []json.RawMessage{bundle.Run.Input.Source.Import.Dialect, bundle.Run.Input.Source.Import.Binding} {
		if bytes.Contains(prepared, []byte(strings.TrimPrefix(digest(canonTest(t, document)), "sha256:"))) {
			t.Fatal("the preparation holds the digest of a document the import keeps beside it")
		}
	}
	if runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN")) && (v.Binding == nil || v.Binding.Status != BindingMatches) {
		t.Fatalf("the record does not bind the preparation: %+v", v.Binding)
	}
	// A run whose mapping is not the dialect its import binds is refused.
	var other VerificationBundle
	if err = json.Unmarshal(encode(bundle), &other); err != nil {
		t.Fatal(err)
	}
	other.Run.Input.Source.Import.Binding = bindingOf(t, published, map[string]InputProfile{})
	if _, err = VerifyInputs(encode(other), cfg.InputProfiles, other.ReleaseDigest); err == nil || !strings.Contains(err.Error(), "leaves source intake unbound") {
		t.Fatalf("an export whose import does not bind its mapping verified: %v", err)
	}
	writeBoundFixture(t, os.Getenv("JPACK_V3_IMPORT_EXPORT_DIR"), bundle, stand.profile)
}

// The event route reads a version-3 document by the same rule as the other
// routes: one under a member that is mapping but for case is refused
// invalid_source_input with the message the other routes give, and the
// document under mapping is read as before.
func TestAVersion3DocumentOnTheEventRouteIsReadOnlyUnderItsExactNames(t *testing.T) {
	input, p, _, _ := v2Fixture(t)
	three := asVersion3(input)
	written := encode(three.Source.Mapping)
	at := []byte(`"mapping":` + string(written))
	delivery := encode(EventDelivery{ID: "evt-1", OccurredAt: now(), Input: three})
	if !bytes.Contains(delivery, at) {
		t.Fatal("the delivery does not name the document at input.source.mapping:", string(delivery))
	}
	cased := bytes.Replace(delivery, at, []byte(`"Mapping":`+string(written)), 1)

	w := newCoverWorld(t)
	tr, key := w.eventTrigger("queue")
	header := map[string]string{"X-Trigger-Token": key}
	message := func(w *httptest.ResponseRecorder) (int, string, string) {
		var body struct {
			Error struct{ Code, Message string }
		}
		if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
			t.Fatal("not an error body:", w.Code, w.Body.String())
		}
		return w.Code, body.Error.Code, body.Error.Message
	}
	got := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/events", string(cased), header)
	want := "Mapping v3: the request names the member Mapping, which is mapping but for case: a dialect of version 3 is read only at input.source.mapping, each member of the path named exactly so and none beside it named so but for case, so that the document Runner keeps is the one written"
	if status, code, text := message(got); status != http.StatusUnprocessableEntity || code != "invalid_source_input" || text != want {
		t.Fatal(got.Code, got.Body.String())
	}
	// The same document, as a preview request, is refused with the same message.
	h := (&Service{cfg: Config{InputProfiles: []InputProfile{p}}}).Handler("test")
	preview := postTo(h, "/v1/previews", []byte(`{"pack":"","input":`+string(bytes.Replace(encode(three), at, []byte(`"Mapping":`+string(written)), 1))+`}`))
	_, _, previewText := message(preview)
	if _, _, text := message(got); text != previewText {
		t.Fatal("the preview route's refusal differs:", preview.Body.String())
	}
	// The control: the document under mapping passes the encoding rule and
	// reaches the frozen-mapping check, which this fixture's release fails.
	control := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/events", string(delivery), header)
	if status, code, text := message(control); status != http.StatusUnprocessableEntity || code != "mapping_mismatch" || text != "Use the job's reviewed input mapping." {
		t.Fatal("the lowercase document does not reach the frozen-mapping check:", control.Code, control.Body.String())
	}
}
