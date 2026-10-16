package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Research sources (runner #88, spec/dialect.md §§4.3, 4.4, 5.1, 5.2, 6.4,
// 9 and 11): bound at import to an AI profile and web profiles of class
// generated, and prepared from the responses their caller retains, each
// checked under the profile it came through. Nothing is asked again.

// researchDocs is a research input's documents, decoded, for a test to
// change: the published dialect, the binding document, the case, each
// source's acquisition, and the installation's profiles. input writes them
// again, the binding naming the dialect as changed unless keepDigest.
type researchDocs struct {
	h          *researchStandIn
	dialect    map[string]any
	binding    map[string]any
	caseDoc    json.RawMessage
	sources    map[string]any
	profiles   []InputProfile
	at         time.Time
	keepDigest bool
}

// decoded reads raw with its numbers as written.
func decoded(t *testing.T, raw []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// researchDocsOf is a plan's documents, its responses asked of a new
// stand-in, each plan change applied first.
func researchDocsOf(t *testing.T, plan func(*researchStandIn) researchPlan, changes ...func(*researchStandIn, *researchPlan)) *researchDocs {
	t.Helper()
	h := newResearchStandIn(t)
	p := plan(h)
	for _, change := range changes {
		change(h, &p)
	}
	at := time.Now().UTC().Truncate(time.Second)
	input := h.researchInput(t, p, at)
	d := &researchDocs{h: h, caseDoc: input.Source.Case, profiles: p.profiles, at: at, sources: map[string]any{}}
	d.dialect = decoded(t, input.Source.Import.Dialect).(map[string]any)
	d.binding = decoded(t, input.Source.Import.Binding).(map[string]any)
	for name, value := range input.Source.Sources {
		d.sources[name] = decoded(t, value.Response)
	}
	return d
}

func (d *researchDocs) source(name string) map[string]any {
	for _, s := range d.dialect["sources"].([]any) {
		if s.(map[string]any)["name"] == name {
			return s.(map[string]any)
		}
	}
	panic("no source " + name)
}
func (d *researchDocs) response(name string) map[string]any { return d.sources[name].(map[string]any) }
func (d *researchDocs) record(name string) map[string]any {
	return d.response(name)["result"].(map[string]any)
}
func (d *researchDocs) answers(name string) map[string]any {
	return d.record(name)["fields"].(map[string]any)
}
func (d *researchDocs) retained(name string, i int) map[string]any {
	return d.record(name)["responses"].([]any)[i].(map[string]any)
}
func (d *researchDocs) assignment(name string) map[string]any {
	return d.binding["sources"].(map[string]any)[name].(map[string]any)
}

// resign signs a changed receipt again under the stand-in's key, as its
// gateway would have signed it.
func (d *researchDocs) resign(t *testing.T, receipt map[string]any) {
	t.Helper()
	signReceipt(t, receipt, d.h.key)
}

func (d *researchDocs) input(t *testing.T) Input {
	t.Helper()
	published := encode(d.dialect)
	if canon, err := proofCanon(published); err == nil && !d.keepDigest {
		d.binding["dialect"] = digest(canon)
	}
	sources := map[string]SourceValue{}
	for name, v := range d.sources {
		sources[name] = SourceValue{Response: encode(v)}
	}
	return Input{Source: &SourceInput{Import: &DialectImport{Dialect: published, Binding: encode(d.binding)}, Case: d.caseDoc, Sources: sources}}
}

func (d *researchDocs) prepare(t *testing.T) (Input, error) {
	t.Helper()
	return prepareV2(d.input(t), d.profiles, d.at, false, nil)
}

// refusedFor requires err to be a refusal naming code at its end; with no
// code, a refusal by a sentence of Runner's that names none.
func refusedFor(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || code != "" && !strings.Contains(err.Error(), "("+code+")") {
		t.Fatalf("not refused as %s: %v", code, err)
	}
}

// A research source is prepared from the research record its caller
// retains, each response checked under the profile it came through, its
// request the recipe, recorded and never sent: the stand-in is closed
// before preparation, and preparation asks nothing of it. Its targets are
// of class generated; its lineage names every citation it adds, in the
// order its record retains them, and the record's own receipt when it has
// one; and the preparation's cites follow the sources (spec/dialect.md
// §9.1, §9.3).
func TestAResearchSourceIsPreparedFromItsRetainedResponses(t *testing.T) {
	for name, plan := range map[string]func(*researchStandIn) researchPlan{"gtm-company-person": gtmPlan, "work-item-triage": workItemPlan} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, plan)
			d.h.server.Close()
			prepared, err := d.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			p := prepared.Preparation
			if p.Version != 3 || p.MappingDigest != mappingV2Hash(prepared.Source.Mapping) {
				t.Fatal("the preparation is not of the bound dialect:", p.Version, p.MappingDigest)
			}
			cites := 0
			for _, o := range p.Outcomes[1:] {
				var request map[string]json.RawMessage
				if err = json.Unmarshal(o.Arguments, &request); err != nil || len(request) != 6 || request["model"] == nil || request["question"] == nil || bytes.Contains(o.Arguments, []byte("{{")) {
					t.Fatalf("%s's request is not its recipe, the question instantiated: %s", o.Name, o.Arguments)
				}
				cites += len(d.record(o.Name)["responses"].([]any))
				if d.response(o.Name)["receipt"] != nil {
					cites++
				}
			}
			if len(p.Cites) != cites {
				t.Fatalf("the preparation cites %d acquisitions, not every retained response and record receipt, %d", len(p.Cites), cites)
			}
			for i, c := range p.Cites {
				if c.CallIndex != i {
					t.Fatalf("cite %d is not in the order of sources and responses: %+v", i, c)
				}
			}
			for _, l := range p.Lineage {
				if l.Source == "case" {
					continue
				}
				own, _ := d.response(l.Source)["receipt"].(map[string]any)
				if l.Class != "generated" || !l.GeneratedInfluence || l.Receipts == nil || len(*l.Receipts) == 0 || (own != nil) != (l.Receipt != nil) {
					t.Fatalf("%s's target %s is not generated, citing its responses: %s", l.Source, l.Target, encode(l))
				}
				if own != nil && (*l.Receipts)[len(*l.Receipts)-1] != *l.Receipt {
					t.Fatal("the record's own citation is not the last of its receipts")
				}
			}
		})
	}
}

// The facts a research read writes are the record's answers as recorded:
// a copy writes the whole fields object, statuses and citations with it,
// and a rule writes found values only, so that a field not found, stale
// or conflicting resolves nothing; zero, "0", false and "" are values.
func TestAResearchReadWritesTheAnswersAsRecorded(t *testing.T) {
	cases := map[string]struct {
		plan    func(*researchStandIn) researchPlan
		variant func(*researchStandIn, *researchPlan)
		facts   string
	}{
		"a copy of every type":        {gtmPlan, nil, `"company":{"active":{"cites":[0],"status":"found","value":true},"company-id":{"cites":[0],"status":"found","value":"synthetic-company-0007"},"employee-count":{"cites":[0],"status":"found","value":240},"employee-range":{"cites":[0],"status":"found","value":{"max":250,"min":200}},"revenue":{"cites":[0],"status":"found","value":"18250000.00"},"roles":{"cites":[0],"status":"found","value":["data-engineer","analyst"]}},"person":{"id":"synthetic-person-0012"},"qualification":{"contactable":true}`},
		"false is a value":            {gtmPlan, func(h *researchStandIn, p *researchPlan) { presence(p, "boolean-false") }, `"qualification":{"contactable":false}`},
		"the empty text is a value":   {gtmPlan, func(h *researchStandIn, p *researchPlan) { presence(p, "text-empty") }, `"person":{"id":""}`},
		"zero is a value":             {workItemPlan, func(h *researchStandIn, p *researchPlan) { presence(p, "integer-zero") }, `"work-item":{"id":"WI-0042","points":0}`},
		"not-found resolves nothing":  {gtmPlan, func(h *researchStandIn, p *researchPlan) { presence(p, "required-not-found") }, `"request":`},
		"a conflict resolves nothing": {workItemPlan, func(h *researchStandIn, p *researchPlan) { staleAndConflicting(p) }, `"request":`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var changes []func(*researchStandIn, *researchPlan)
			if c.variant != nil {
				changes = append(changes, c.variant)
			}
			prepared, err := researchDocsOf(t, c.plan, changes...).prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(prepared.Facts, []byte(c.facts)) {
				t.Fatalf("the facts do not hold %s: %s", c.facts, prepared.Facts)
			}
			if name == "not-found resolves nothing" && bytes.Contains(prepared.Facts, []byte(`"qualification"`)) || name == "a conflict resolves nothing" && bytes.Contains(prepared.Facts, []byte(`"triage"`)) {
				t.Fatalf("an answer that is not found resolved a target: %s", prepared.Facts)
			}
		})
	}
}

// A preparation with a research source keeps its cites in the order of
// sources, each research source's in the order its record retains them,
// whatever their call indexes (spec/dialect.md §9.1); one without keeps
// them by call index, as before.
func TestResearchCitesFollowTheSourcesNotTheCallIndex(t *testing.T) {
	d := researchDocsOf(t, gtmPlan)
	// The company's one response, asked first at call index 0, is signed
	// again at call index 20, after the person's three.
	r := d.retained("company", 0)["receipt"].(map[string]any)
	r["callIndex"], r["prevSignature"] = json.Number("20"), strings.Repeat("ab", 64)
	d.resign(t, r)
	prepared, err := d.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	var indexes []int
	for _, c := range prepared.Preparation.Cites {
		indexes = append(indexes, c.CallIndex)
	}
	if encode(indexes) == nil || string(encode(indexes)) != "[20,1,2,3]" {
		t.Fatalf("the cites are not in the order of sources: %v", indexes)
	}
	// Without a research source, cites are by call index, as before.
	two, p, key, at := v2Fixture(t)
	two.Source.Mapping.Sources = append(two.Source.Mapping.Sources, two.Source.Mapping.Sources[0])
	two.Source.Mapping.Sources[1].Name = "second"
	two.Source.Mapping.Sources[1].Read = SourceRead{Rule: json.RawMessage(strings.NewReplacer("/vendor/description", "/second/description", `"registry"`, `"second-registry"`).Replace(string(vendorRule())))}
	two.Source.Sources["second"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":7,"description":"a verified vendor"}`), at, 0)}
	first := decoded(t, two.Source.Sources["vendor"].Response).(map[string]any)
	first["receipt"].(map[string]any)["callIndex"], first["receipt"].(map[string]any)["prevSignature"] = json.Number("1"), strings.Repeat("ab", 64)
	signReceipt(t, first["receipt"].(map[string]any), key)
	two.Source.Sources["vendor"] = SourceValue{Response: encode(first)}
	prepared, err = normalizeV2(two, []InputProfile{p}, at)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Preparation.Cites[0].CallIndex != 0 || prepared.Preparation.Cites[1].CallIndex != 1 {
		t.Fatalf("an operation's cites are not by call index: %+v", prepared.Preparation.Cites)
	}
}

// The import step binds a published research source with its binding
// document: the bound dialect is the published one with its profile, its
// digest and its web profiles added, every other member as written, and
// the preparation commits the bound dialect alone, by its digest.
func TestAResearchSourceIsBoundAtImport(t *testing.T) {
	d := researchDocsOf(t, gtmPlan)
	input := d.input(t)
	bound, err := bindDialect(*input.Source.Import, d.profiles)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range bound.Sources {
		published := d.dialect["sources"].([]any)[i].(map[string]any)
		written := decoded(t, encode(s)).(map[string]any)
		entry := d.assignment(s.Name)
		for _, member := range []string{"profile", "profileDigest", "webProfiles"} {
			if !sameJSON(encode(written[member]), encode(entry[member])) {
				t.Fatalf("%s's %s is not the binding's", s.Name, member)
			}
			delete(written, member)
		}
		if !sameJSON(encode(written), encode(published)) {
			t.Fatalf("binding changed %s otherwise than adding its assignments:\n%s\n%s", s.Name, encode(written), encode(published))
		}
	}
	prepared, err := d.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := canonical(encode(prepared.Preparation))
	for _, document := range [][]byte{input.Source.Import.Dialect, input.Source.Import.Binding} {
		if bytes.Contains(canon, []byte(strings.TrimPrefix(digest(canonTest(t, document)), "sha256:"))) {
			t.Fatal("the preparation holds the digest of a document the import keeps beside it")
		}
	}
	if prepared.Preparation.MappingDigest != mappingV2Hash(bound) {
		t.Fatal("the preparation does not commit the bound dialect")
	}
	// A research source given unbound as the mapping is refused by name.
	unbound := cloneInput(input)
	if err = strictJSON(input.Source.Import.Dialect, &unbound.Source.Mapping); err != nil {
		t.Fatal(err)
	}
	unbound.Source.Import = nil
	if _, err = prepareV2(unbound, d.profiles, d.at, false, nil); err == nil || !strings.Contains(err.Error(), "source company is unbound: a research source is published with no AI or web profile") {
		t.Fatal("an unbound research source was prepared:", err)
	}
	// So is a dialect bound by another installation's binding: rebinding one
	// assignment changes the bound dialect and its digest.
	other := researchDocsOf(t, gtmPlan, secondInstallation)
	rebound, err := other.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Preparation.MappingDigest == prepared.Preparation.MappingDigest {
		t.Fatal("a dialect bound to another profile has the same digest")
	}
}

// A profile's research declaration is held to its form (spec/dialect.md
// §5.1): one to three capabilities, distinct, of answer, search and page in
// that order, and a model, a non-empty string of at most 256 bytes,
// exactly when it serves answer. An installation with one otherwise does
// not start.
func TestAProfilesResearchDeclarationIsHeldToItsForm(t *testing.T) {
	h := newResearchStandIn(t)
	model, empty, long := researchModel, "", strings.Repeat("m", 257)
	for name, c := range map[string]struct {
		declaration ResearchDeclaration
		code        string
	}{
		"no capability":          {ResearchDeclaration{Capabilities: []string{}}, codeMalformed},
		"an unknown capability":  {ResearchDeclaration{Capabilities: []string{"browse"}}, codeMalformed},
		"capabilities reordered": {ResearchDeclaration{Capabilities: []string{"page", "search"}}, codeMalformed},
		"a capability twice":     {ResearchDeclaration{Capabilities: []string{"search", "search"}}, codeMalformed},
		"four capabilities":      {ResearchDeclaration{Capabilities: []string{"answer", "search", "page", "page"}, Model: &model}, codeOverBound},
		"answer without a model": {ResearchDeclaration{Capabilities: []string{"answer"}}, codeMalformed},
		"a model without answer": {ResearchDeclaration{Capabilities: []string{"search"}, Model: &model}, codeMalformed},
		"an empty model":         {ResearchDeclaration{Capabilities: []string{"answer"}, Model: &empty}, codeMalformed},
		"a model of 257 bytes":   {ResearchDeclaration{Capabilities: []string{"answer"}, Model: &long}, codeOverBound},
	} {
		t.Run(name, func(t *testing.T) {
			p := h.aiProfile("assistant", "/answers")
			p.Research = &c.declaration
			_, err := validateProfileKeys([]InputProfile{p})
			refusedFor(t, err, c.code)
			if _, err = ParseInputProfiles(encode([]InputProfile{p})); err == nil {
				t.Fatal("the profile was admitted")
			}
		})
	}
	if _, err := validateProfileKeys([]InputProfile{h.aiProfile("assistant", "/answers"), h.searchProfile("search"), h.pageProfile("page", "/page")}); err != nil {
		t.Fatal(err)
	}
	// A profile without one encodes as before.
	plain, _ := testProfile(t)
	if bytes.Contains(encode(plain), []byte("research")) {
		t.Fatal("a profile without a research declaration writes one")
	}
}

// A research source bound to a profile that does not fit it is
// profile-mismatch, by name, at import and given directly as a bound
// dialect (spec/dialect.md §4.4, §5.1): a profile this installation does
// not hold by that id and digest, of another class than generated,
// declaring no research capabilities, lacking the capability it is bound
// for, or naming another model. The work item's search is bound to a
// profile, searcher, through which no response came, so that only the
// binding misfits. So is a retained response through a profile the source
// is not bound to; and a profile that does not fit precedes every other
// finding.
func TestAResearchSourceBoundToAnUnsuitableProfileIsAProfileMismatch(t *testing.T) {
	other := researchModel + "-other"
	cases := map[string]func(*InputProfile){
		"of class record":              func(p *InputProfile) { p.Class, p.Source = "record", "web-records" },
		"with no research declaration": func(p *InputProfile) { p.Research = nil },
		"lacking the capability":       func(p *InputProfile) { p.Research = &ResearchDeclaration{Capabilities: []string{"page"}} },
		"naming another model": func(p *InputProfile) {
			p.Research = &ResearchDeclaration{Capabilities: []string{"answer", "search"}, Model: &other}
		},
		"this installation does not hold": nil,
		"by an old digest":                nil,
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, workItemPlan)
			searcher := d.h.searchProfile("searcher")
			pinned := profileHash(searcher)
			if change != nil {
				change(&searcher)
				pinned = profileHash(searcher)
			}
			if name != "this installation does not hold" {
				d.profiles = append(d.profiles, searcher)
			}
			if name == "by an old digest" {
				pinned = digest([]byte("an old profile"))
			}
			d.assignment("item")["webProfiles"].(map[string]any)["search"] = map[string]any{"profile": "searcher", "profileDigest": pinned}
			input := d.input(t)
			_, err := bindDialect(*input.Source.Import, d.profiles)
			refusedFor(t, err, codeMismatch)
			_, err = d.prepare(t)
			refusedFor(t, err, codeMismatch)
			direct := Input{Source: &SourceInput{Mapping: boundOf(t, d), Case: input.Source.Case, Sources: input.Source.Sources}}
			_, err = prepareV2(direct, d.profiles, d.at, false, nil)
			refusedFor(t, err, codeMismatch)
		})
	}
	t.Run("a retained response through a profile it is not bound to", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		search := d.profiles[1]
		d.retained("company", 0)["profile"] = map[string]any{"id": search.ID, "digest": profileHash(search)}
		_, err := d.prepare(t)
		refusedFor(t, err, codeMismatch)
	})
	t.Run("a retained response naming another digest", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		d.retained("person", 2)["profile"].(map[string]any)["digest"] = digest([]byte("another"))
		_, err := d.prepare(t)
		refusedFor(t, err, codeMismatch)
	})
	// The person's search is bound to searcher, of class record, through
	// which its search response names it came; the company's response before
	// it is changed. The profile that does not fit is found first.
	t.Run("before every other finding", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		searcher := d.h.searchProfile("searcher")
		searcher.Class, searcher.Source = "record", "web-records"
		d.profiles = append(d.profiles, searcher)
		ref := map[string]any{"profile": "searcher", "profileDigest": profileHash(searcher)}
		d.assignment("person")["webProfiles"].(map[string]any)["search"] = ref
		d.retained("person", 1)["profile"] = map[string]any{"id": "searcher", "digest": profileHash(searcher)}
		d.retained("company", 0)["result"].(map[string]any)["status"] = json.Number("203")
		input := d.input(t)
		direct := Input{Source: &SourceInput{Mapping: boundOf(t, d), Case: input.Source.Case, Sources: input.Source.Sources}}
		_, err := prepareV2(direct, d.profiles, d.at, false, nil)
		refusedFor(t, err, codeMismatch)
	})
}

// boundOf is the docs' dialect bound by their binding document, its
// assignments written in, as a bound dialect given directly is: no import
// step checks the profiles it names.
func boundOf(t *testing.T, d *researchDocs) InputMapping {
	t.Helper()
	dialect := decoded(t, encode(d.dialect)).(map[string]any)
	for _, s := range dialect["sources"].([]any) {
		o := s.(map[string]any)
		for k, v := range d.assignment(o["name"].(string)) {
			o[k] = v
		}
	}
	var m InputMapping
	if err := strictJSON(encode(dialect), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A binding document binds a research source by exactly its AI profile, its
// digest and its web profiles, the web profiles exactly the web
// capabilities the source asks for (spec/dialect.md §4.4).
func TestTheBindingOfAResearchSourceIsHeldToItsForm(t *testing.T) {
	assignment := map[string]any{"profile": "page", "profileDigest": digest([]byte("x"))}
	for name, c := range map[string]struct {
		change func(*researchDocs)
		code   string
	}{
		"without webProfiles":            {func(d *researchDocs) { delete(d.assignment("person"), "webProfiles") }, codeMalformed},
		"a member more":                  {func(d *researchDocs) { d.assignment("person")["model"] = researchModel }, codeMalformed},
		"a requested capability missing": {func(d *researchDocs) { delete(d.assignment("person")["webProfiles"].(map[string]any), "page") }, codeMalformed},
		"an unrequested capability":      {func(d *researchDocs) { d.assignment("company")["webProfiles"].(map[string]any)["page"] = assignment }, codeMalformed},
		"a web assignment of another form": {func(d *researchDocs) {
			delete(d.assignment("person")["webProfiles"].(map[string]any)["page"].(map[string]any), "profileDigest")
		}, codeMalformed},
		"three web assignments": {func(d *researchDocs) { d.assignment("person")["webProfiles"].(map[string]any)["answer"] = assignment }, codeOverBound},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, gtmPlan)
			c.change(d)
			_, err := d.prepare(t)
			refusedFor(t, err, c.code)
		})
	}
	t.Run("three web assignments in a bound dialect given directly", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		input := d.input(t)
		bound, err := bindDialect(*input.Source.Import, d.profiles)
		if err != nil {
			t.Fatal(err)
		}
		bound.Sources[1].WebProfiles = encode(map[string]any{"search": assignment, "page": assignment, "answer": assignment})
		_, err = prepareV2(Input{Source: &SourceInput{Mapping: bound, Case: input.Source.Case, Sources: input.Source.Sources}}, d.profiles, d.at, false, nil)
		refusedFor(t, err, codeOverBound)
	})
	t.Run("a binding for another published digest", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		d.binding["dialect"], d.keepDigest = digest([]byte("another dialect")), true
		if _, err := d.prepare(t); err == nil || !strings.Contains(err.Error(), "the binding document binds the dialect") {
			t.Fatal("a binding of another dialect bound:", err)
		}
	})
}

// A research source's recipe is held to its form (spec/dialect.md §4.3,
// §4.4, §12): every member it names, each of its form, the maxima first.
func TestAResearchRecipeIsHeldToItsForm(t *testing.T) {
	field := func(d *researchDocs, i int) map[string]any {
		return d.source("company")["fields"].([]any)[i].(map[string]any)
	}
	set := func(member string, value any) func(*researchDocs) {
		return func(d *researchDocs) { d.source("company")[member] = value }
	}
	for name, c := range map[string]struct {
		change func(*researchDocs)
		code   string
	}{
		"no fields":       {func(d *researchDocs) { delete(d.source("company"), "fields") }, codeMalformed},
		"no capabilities": {func(d *researchDocs) { delete(d.source("company"), "capabilities") }, codeMalformed},
		"an empty declaration list": {func(d *researchDocs) {
			d.source("company")["fields"] = []any{}
			d.record("company")["fields"] = map[string]any{}
		}, codeMalformed},
		"a declaration of another type":    {func(d *researchDocs) { field(d, 4)["type"] = "number"; delete(d.answers("company"), "roles") }, codeMalformed},
		"required that is not a boolean":   {func(d *researchDocs) { field(d, 4)["required"] = "no" }, codeMalformed},
		"a field named case":               {func(d *researchDocs) { field(d, 4)["name"] = "case"; delete(d.answers("company"), "roles") }, codeMalformed},
		"an empty description":             {func(d *researchDocs) { field(d, 4)["description"] = "" }, codeMalformed},
		"a declaration with a member more": {func(d *researchDocs) { field(d, 4)["unit"] = "EUR" }, codeMalformed},
		"a field declared twice": {func(d *researchDocs) {
			d.source("company")["fields"] = append(d.source("company")["fields"].([]any), declare("active", "boolean", false, "Whether the company trades, again."))
		}, codeMalformed},
		"evidence that is not an array": {set("evidence", "a page"), codeMalformed},
		"an empty evidence instruction": {set("evidence", []any{""}), codeMalformed},
		"capabilities without answer": {func(d *researchDocs) {
			d.source("company")["capabilities"] = []any{"search"}
			d.assignment("company")["webProfiles"] = map[string]any{"search": map[string]any{"profile": "search", "profileDigest": profileHash(d.profiles[1])}}
		}, codeMalformed},
		"answer named twice":             {set("capabilities", []any{"answer", "answer"}), codeMalformed},
		"an unknown capability":          {set("capabilities", []any{"answer", "browse"}), codeMalformed},
		"a question naming no parameter": {set("question", map[string]any{"$param": "elsewhere"}), codeMalformed},
		"a question naming an integer":   {set("question", map[string]any{"$param": "companyNumber"}), codeMalformed},
		"a question naming runAt":        {set("question", map[string]any{"$param": "runAt"}), codeMalformed},
		"a question that is a number":    {set("question", 7), codeMalformed},
		"a question that is an object":   {set("question", map[string]any{"text": "Which?"}), codeMalformed},
		"constraints that are an array":  {set("constraints", []any{"EU"}), codeMalformed},
		"maxAge over a day":              {set("maxAge", json.Number("86401")), codeMalformed},
		"constraints with a fraction":    {set("constraints", map[string]any{"share": json.Number("0.5")}), ""},
		"an empty model":                 {set("model", ""), codeMalformed},
		"an operation's catalog member":  {set("platform", "research"), codeMalformed},
		"arguments":                      {set("arguments", map[string]any{}), codeMalformed},
		"65 fields declared": {func(d *researchDocs) {
			for i := 0; len(d.source("company")["fields"].([]any)) < 65; i++ {
				d.source("company")["fields"] = append(d.source("company")["fields"].([]any), declare("extra-"+strings.Repeat("x", i), "integer", false, "An extra."))
			}
		}, codeOverBound},
		"a field name of 65 bytes":        {func(d *researchDocs) { field(d, 4)["name"] = strings.Repeat("n", 65) }, codeOverBound},
		"a description of 2049 bytes":     {func(d *researchDocs) { field(d, 4)["description"] = strings.Repeat("d", 2049) }, codeOverBound},
		"a model of 257 bytes":            {set("model", strings.Repeat("m", 257)), codeOverBound},
		"an evidence instruction of 4097": {set("evidence", []any{strings.Repeat("e", 4097)}), codeOverBound},
		"four capabilities":               {set("capabilities", []any{"answer", "search", "page", "page"}), codeOverBound},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, gtmPlan)
			c.change(d)
			_, err := d.prepare(t)
			refusedFor(t, err, c.code)
		})
	}
	t.Run("a bound source without its profile", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		input := d.input(t)
		bound := boundOf(t, d)
		bound.Sources[0].Profile = ""
		_, err := prepareV2(Input{Source: &SourceInput{Mapping: bound, Case: input.Source.Case, Sources: input.Source.Sources}}, d.profiles, d.at, false, nil)
		refusedFor(t, err, codeMalformed)
	})
	t.Run("a research source in a version-2 dialect", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		input := d.input(t)
		bound, err := bindDialect(*input.Source.Import, d.profiles)
		if err != nil {
			t.Fatal(err)
		}
		bound.Version = 2
		if _, err = prepareV2(Input{Source: &SourceInput{Mapping: bound, Case: input.Source.Case, Sources: input.Source.Sources}}, d.profiles, d.at, false, nil); err == nil || !strings.Contains(err.Error(), "only a source of dialect version 3 has") {
			t.Fatal("a version-2 research source was read:", err)
		}
	})
}

// A research record is held to its form and its answers to the source's
// declarations (spec/dialect.md §4.3, "Answers"; §12): a missing entry for a
// required field, an undeclared one, null, a value of another type or
// encoding, a misplaced value or values, a citation that is not a distinct
// index into the record's responses, and every maximum exceeded.
func TestAResearchRecordIsHeldToItsForm(t *testing.T) {
	entry := func(field string) func(*researchDocs) map[string]any {
		return func(d *researchDocs) map[string]any { return d.answers("company")[field].(map[string]any) }
	}
	value := func(field string, v any) func(*researchDocs) {
		return func(d *researchDocs) { entry(field)(d)["value"] = v }
	}
	cites := func(field string, v any) func(*researchDocs) {
		return func(d *researchDocs) { entry(field)(d)["cites"] = v }
	}
	answer := func(field string, v any) func(*researchDocs) {
		return func(d *researchDocs) { d.answers("company")[field] = v }
	}
	n := func(s string) json.Number { return json.Number(s) }
	for name, c := range map[string]struct {
		change func(*researchDocs)
		code   string
	}{
		"a required entry omitted":               {func(d *researchDocs) { delete(d.answers("company"), "employee-count") }, codeMalformed},
		"an undeclared field":                    {answer("headcount", map[string]any{"status": "not-found", "cites": []any{}}), codeMalformed},
		"a null entry":                           {answer("roles", nil), codeMalformed},
		"a null value":                           {value("revenue", nil), codeMalformed},
		"an integer as a string":                 {value("employee-count", "240"), codeMalformed},
		"an integer with a fraction":             {value("employee-count", n("240.0")), codeMalformed},
		"an integer beyond 2^53-1":               {value("employee-count", n("9007199254740992")), codeMalformed},
		"a numeric decimal":                      {value("revenue", n("12")), codeMalformed},
		"a decimal with a leading zero":          {value("revenue", "012"), codeMalformed},
		"a decimal with a plus":                  {value("revenue", "+1"), codeMalformed},
		"a decimal with an exponent":             {value("revenue", "1e2"), codeMalformed},
		"an empty decimal":                       {value("revenue", ""), codeMalformed},
		"a decimal ending in a point":            {value("revenue", "1."), codeMalformed},
		"a boolean as a string":                  {value("active", "false"), codeMalformed},
		"a text that is a number":                {value("company-id", n("1")), codeMalformed},
		"a null list item":                       {value("roles", []any{"buyer", nil}), codeMalformed},
		"a null range endpoint":                  {value("employee-range", map[string]any{"min": nil, "max": "2"}), codeMalformed},
		"mixed range endpoints":                  {value("employee-range", map[string]any{"min": n("1"), "max": "2"}), codeMalformed},
		"a reversed decimal range":               {value("employee-range", map[string]any{"min": "2.5", "max": "2.25"}), codeMalformed},
		"a reversed integer range":               {value("employee-range", map[string]any{"min": n("3"), "max": n("-3")}), codeMalformed},
		"a range with a unit":                    {value("employee-range", map[string]any{"min": n("1"), "max": n("2"), "unit": "people"}), codeMalformed},
		"a found entry without cites":            {func(d *researchDocs) { delete(entry("revenue")(d), "cites") }, codeMalformed},
		"a found entry citing nothing":           {cites("revenue", []any{}), codeMalformed},
		"a value on a not-found entry":           {func(d *researchDocs) { entry("revenue")(d)["status"] = "not-found" }, codeMalformed},
		"values on a found entry":                {func(d *researchDocs) { entry("revenue")(d)["values"] = []any{} }, codeMalformed},
		"a conflict of one observation":          {answer("revenue", conflicting(observed("1", 0))), codeMalformed},
		"a conflict whose values agree":          {answer("employee-count", conflicting(observed(n("7"), 0), observed(n("7"), 0))), codeMalformed},
		"a conflict with cites of its own":       {answer("revenue", map[string]any{"status": "conflicting", "cites": []any{n("0")}, "values": []any{observed("1", 0), observed("2", 0)}}), codeMalformed},
		"an observation citing nothing":          {answer("revenue", conflicting(observed("1"), observed("2", 0))), codeMalformed},
		"an observation with a URL":              {answer("revenue", conflicting(map[string]any{"value": "1", "cites": []any{n("0")}, "url": "https://example.invalid/"}, observed("2", 0))), codeMalformed},
		"an unknown status":                      {answer("revenue", map[string]any{"status": "guessed", "cites": []any{}}), codeMalformed},
		"an index -0":                            {cites("revenue", []any{n("-0")}), codeMalformed},
		"an index at the responses' length":      {cites("revenue", []any{n("1")}), codeMalformed},
		"a negative index":                       {cites("revenue", []any{n("-1")}), codeMalformed},
		"an index with a fraction":               {cites("revenue", []any{n("0.0")}), codeMalformed},
		"a URL in place of an index":             {cites("revenue", []any{"https://example.invalid/"}), codeMalformed},
		"a record without fields":                {func(d *researchDocs) { delete(d.record("company"), "fields") }, codeMalformed},
		"a record with a member more":            {func(d *researchDocs) { d.record("company")["model"] = researchModel }, codeMalformed},
		"a retained response with a member more": {func(d *researchDocs) { d.retained("company", 0)["url"] = "https://example.invalid/" }, codeMalformed},
		"salts without a receipt": {func(d *researchDocs) {
			d.response("company")["salts"] = map[string]any{"args": strings.Repeat("ab", 32)}
		}, codeMalformed},
		"a response with a member more":  {func(d *researchDocs) { d.response("company")["model"] = researchModel }, codeMalformed},
		"a retained response's salts":    {func(d *researchDocs) { d.retained("company", 0)["salts"] = map[string]any{"args": "ab"} }, codeMalformed},
		"a range endpoint spelled 1e2":   {value("employee-range", map[string]any{"min": "1e2", "max": "2e2"}), codeMalformed},
		"a conflict with no observation": {answer("revenue", conflicting()), codeMalformed},
		"a text of 8193 bytes":           {value("company-id", strings.Repeat("é", 4096)+"x"), codeOverBound},
		"a text-list of 65 items":        {value("roles", make([]any, 65)), codeOverBound},
		"a text-list item of 1025 bytes": {value("roles", []any{strings.Repeat("r", 1025)}), codeOverBound},
		"a decimal of 129 characters":    {value("revenue", strings.Repeat("1", 129)), codeOverBound},
		"a range endpoint of 129":        {value("employee-range", map[string]any{"min": "1", "max": strings.Repeat("1", 129)}), codeOverBound},
		"65 indices in a citation":       {cites("revenue", make([]any, 65)), codeOverBound},
		"65 answer entries": {func(d *researchDocs) {
			for i := 0; len(d.answers("company")) < 65; i++ {
				d.answers("company")[strings.Repeat("u", i+1)] = map[string]any{"status": "not-found", "cites": []any{}}
			}
		}, codeOverBound},
		"an answer named by 65 bytes": {answer(strings.Repeat("u", 65), map[string]any{"status": "not-found", "cites": []any{}}), codeOverBound},
		"65 conflicting observations": {func(d *researchDocs) {
			o := []any{}
			for i := 0; i < 65; i++ {
				o = append(o, observed(n(strings.Repeat("1", i+1)), 0))
			}
			d.answers("company")["employee-count"] = map[string]any{"status": "conflicting", "cites": []any{}, "values": o}
		}, codeOverBound},
		"65 retained responses": {func(d *researchDocs) {
			r := d.record("company")
			for len(r["responses"].([]any)) < 65 {
				r["responses"] = append(r["responses"].([]any), d.retained("company", 0))
			}
		}, codeOverBound},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, gtmPlan)
			c.change(d)
			_, err := d.prepare(t)
			refusedFor(t, err, c.code)
		})
	}
	t.Run("the record's own salts", func(t *testing.T) {
		d := researchDocsOf(t, workItemPlan)
		d.response("follow-up")["salts"] = map[string]any{"args": "ab"}
		_, err := d.prepare(t)
		refusedFor(t, err, codeMalformed)
	})
	t.Run("an index cited twice", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		d.answers("person")["person-id"].(map[string]any)["cites"] = []any{json.Number("2"), json.Number("2")}
		_, err := d.prepare(t)
		refusedFor(t, err, codeMalformed)
	})
	// Every maximum reached is read: the record's answers are the caller's,
	// which no receipt commits when the record has none of its own.
	t.Run("every maximum reached", func(t *testing.T) {
		d := researchDocsOf(t, gtmPlan)
		d.source("company")["evidence"] = []any{strings.Repeat("e", 4096)}
		fields := d.source("company")["fields"].([]any)
		fields[4].(map[string]any)["description"] = strings.Repeat("d", 2048)
		long := strings.Repeat("n", 64)
		d.source("company")["fields"] = append(fields, declare(long, "integer", false, "A field whose name is 64 bytes."))
		answers := d.answers("company")
		answers["company-id"].(map[string]any)["value"] = strings.Repeat("é", 4096)
		items := []any{}
		for i := 0; i < 64; i++ {
			items = append(items, strings.Repeat("é", 512))
		}
		answers["roles"].(map[string]any)["value"] = items
		answers["revenue"].(map[string]any)["value"] = "-" + strings.Repeat("1", 125) + ".5"
		answers["employee-range"].(map[string]any)["value"] = map[string]any{"min": strings.Repeat("1", 128), "max": "2" + strings.Repeat("0", 127)}
		answers[long] = map[string]any{"status": "not-found", "cites": []any{json.Number("0")}}
		if _, err := d.prepare(t); err != nil {
			t.Fatal(err)
		}
	})
}

// A plain research read reads the record through a closed list of
// pointers, and never takes a value from under values (spec/dialect.md
// §4.3, "Reading fields").
func TestAPlainResearchReadReadsItsClosedListOfPointers(t *testing.T) {
	rule := func(d *researchDocs) map[string]any {
		return d.source("person")["read"].(map[string]any)["rule"].(map[string]any)
	}
	clause := func(d *researchDocs, i int) map[string]any { return rule(d)["clauses"].([]any)[i].(map[string]any) }
	copyFrom := func(from string) func(*researchDocs) {
		return func(d *researchDocs) {
			d.source("company")["read"].(map[string]any)["copy"].(map[string]any)["facts"].([]any)[0].(map[string]any)["source"] = from
		}
	}
	for name, change := range map[string]func(*researchDocs){
		"a rule's from under values": func(d *researchDocs) {
			clause(d, 0)["claim"].(map[string]any)["facts"].([]any)[0].(map[string]any)["from"] = "/fields/person-id/values/0/value"
		},
		"a copy from under values":      copyFrom("/fields/roles/values/0/value"),
		"a copy of a retained response": copyFrom("/responses/0/result"),
		"a copy of the whole record":    copyFrom(""),
		"a copy of an undeclared field": copyFrom("/fields/headcount"),
		"a condition on an undeclared field": func(d *researchDocs) {
			clause(d, 1)["when"] = map[string]any{"op": "exists", "field": "/fields/headcount/status"}
		},
		"a pointer below a status": func(d *researchDocs) {
			clause(d, 1)["when"] = map[string]any{"op": "exists", "field": "/fields/contactable/status/0"}
		},
		"an observation index of another form": func(d *researchDocs) {
			clause(d, 1)["when"] = map[string]any{"op": "exists", "field": "/fields/contactable/values/00/value"}
		},
		"an unwrap outside the closed list": func(d *researchDocs) {
			d.source("company")["read"].(map[string]any)["unwrap"] = []any{"/responses/0/result/body"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, gtmPlan)
			change(d)
			_, err := d.prepare(t)
			if err == nil {
				t.Fatal("the read was taken")
			}
			if name != "a copy of the whole record" {
				refusedFor(t, err, codeMalformed)
			}
		})
	}
	// A read names sources only as a research source's reconciliation: an
	// operation's that names them is refused.
	t.Run("sources on an operation's read", func(t *testing.T) {
		input, p, _, at := v2Fixture(t)
		three := asVersion3(input)
		three.Source.Mapping.Sources[0].Read.Sources = json.RawMessage(`["vendor"]`)
		_, err := normalizeV2(three, []InputProfile{p}, at)
		refusedWith(t, err, "Mapping v3: source vendor: a read names sources only as a research source's reconciliation of earlier research sources, in dialect version 3 (input-malformed)")
	})
}

// A reconciliation reads earlier research sources' records beside the
// current one (spec/dialect.md §6.4): the rule's declared order of statuses
// decides, a failed fallback deriving unknown with no resolved target.
func TestAReconciliationReadsEarlierResearchSources(t *testing.T) {
	for _, example := range []string{"gtm-company-person", "work-item-triage"} {
		target, field := "company", "employee-count"
		if example == "work-item-triage" {
			target, field = "work-item", "points"
		}
		for variant, want := range map[string]string{"all-unavailable": "a conflict is not chosen", "secondary-found": "the secondary found it"} {
			t.Run(example+"/"+variant, func(t *testing.T) {
				d := researchDocsOf(t, func(h *researchStandIn) researchPlan { return fallbackPlan(h, example) }, func(h *researchStandIn, p *researchPlan) { fallback(p, variant) })
				prepared, err := d.prepare(t)
				if err != nil {
					t.Fatal(err)
				}
				last := prepared.Preparation.Outcomes[len(prepared.Preparation.Outcomes)-1]
				resolved := bytes.Contains(prepared.Facts, []byte(`"`+target+`":{"`+field+`":240}`))
				if last.Reason != want || resolved != (variant == "secondary-found") {
					t.Fatalf("the reconciliation derived %s: %s", last.Reason, prepared.Facts)
				}
				for _, l := range prepared.Preparation.Lineage {
					if l.Source == last.Name && len(*l.Receipts) != 1 {
						t.Fatal("the reconciliation's lineage names the earlier sources' citations")
					}
				}
			})
		}
	}
}

// A reconciliation is held to its form (spec/dialect.md §6.4).
func TestAReconciliationIsHeldToItsForm(t *testing.T) {
	read := func(d *researchDocs) map[string]any { return d.source("company-reconciled")["read"].(map[string]any) }
	clauses := func(d *researchDocs) []any { return read(d)["rule"].(map[string]any)["clauses"].([]any) }
	clause := func(d *researchDocs, i int) map[string]any { return clauses(d)[i].(map[string]any) }
	from := func(d *researchDocs, i int) map[string]any {
		return clause(d, i)["claim"].(map[string]any)["facts"].([]any)[0].(map[string]any)
	}
	primary := "/sources/company-primary/fields/employee-count"
	without := func(guard map[string]any) func(*researchDocs) {
		return func(d *researchDocs) {
			var prune func(any) any
			prune = func(c any) any {
				o := c.(map[string]any)
				if o["op"] == "all" || o["op"] == "any" {
					kept := []any{}
					for _, x := range o["of"].([]any) {
						if !sameJSON(encode(x), encode(guard)) {
							kept = append(kept, prune(x))
						}
					}
					o["of"] = kept
				}
				return o
			}
			for _, c := range clauses(d) {
				prune(c.(map[string]any)["when"])
			}
		}
	}
	guard := func(status string) map[string]any {
		return map[string]any{"op": "equals", "field": primary + "/status", "to": status}
	}
	for name, c := range map[string]struct {
		change func(*researchDocs)
		code   string
	}{
		"no earlier source named": {func(d *researchDocs) {
			read(d)["sources"] = []any{}
			current := "/current/fields/employee-count"
			read(d)["rule"] = decoded(t, []byte(`{"ruleVersion":"1","parameters":{},"clauses":[`+
				`{"when":{"op":"equals","field":"`+current+`/status","to":"found"},"claim":{"facts":[{"pointer":"/company/employee-count","from":"`+current+`/value"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"found"},`+
				`{"when":{"op":"any","of":[{"op":"not","of":{"op":"exists","field":"`+current+`"}},{"op":"equals","field":"`+current+`/status","to":"not-found"},{"op":"equals","field":"`+current+`/status","to":"stale"},{"op":"equals","field":"`+current+`/status","to":"conflicting"}]},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"not found"},`+
				`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"otherwise"}]}`))
		}, codeMalformed},
		"an earlier source named twice": {func(d *researchDocs) {
			read(d)["sources"] = []any{"company-primary", "company-secondary", "company-primary"}
		}, codeMalformed},
		"the current source named":     {func(d *researchDocs) { read(d)["sources"] = []any{"company-primary", "company-reconciled"} }, codeMalformed},
		"a source that does not exist": {func(d *researchDocs) { read(d)["sources"] = []any{"company-primary", "elsewhere"} }, codeMalformed},
		"a later source named": {func(d *researchDocs) {
			s := d.dialect["sources"].([]any)
			s[1], s[2] = s[2], s[1]
		}, codeMalformed},
		"an unwrap":                     {func(d *researchDocs) { read(d)["unwrap"] = []any{"/current"} }, codeMalformed},
		"a from under values":           {func(d *researchDocs) { from(d, 0)["from"] = primary + "/values/0/value" }, codeMalformed},
		"a from of a status":            {func(d *researchDocs) { from(d, 0)["from"] = primary + "/status" }, codeMalformed},
		"a from of an undeclared field": {func(d *researchDocs) { from(d, 0)["from"] = "/sources/company-primary/fields/revenue/value" }, codeMalformed},
		"a from of a source not named":  {func(d *researchDocs) { read(d)["sources"] = []any{"company-secondary"} }, codeMalformed},
		"a selecting clause unguarded": {func(d *researchDocs) {
			clause(d, 0)["when"] = map[string]any{"op": "exists", "field": primary + "/value"}
		}, codeMalformed},
		"a found guard below an any": {func(d *researchDocs) {
			w := clause(d, 1)["when"].(map[string]any)
			w["of"].([]any)[1] = map[string]any{"op": "any", "of": []any{w["of"].([]any)[1]}}
		}, codeMalformed},
		"no not-found guard":   {without(guard("not-found")), codeMalformed},
		"no stale guard":       {without(guard("stale")), codeMalformed},
		"no conflicting guard": {without(guard("conflicting")), codeMalformed},
		"no absence guard":     {without(map[string]any{"op": "not", "of": map[string]any{"op": "exists", "field": primary}}), codeMalformed},
		"a claim that writes evidence": {func(d *researchDocs) {
			clause(d, 2)["claim"].(map[string]any)["evidence"] = map[string]any{"intake-form": "unknown"}
		}, codeMalformed},
		"a last clause that resolves": {func(d *researchDocs) { clause(d, 5)["claim"].(map[string]any)["acquisitionStatus"] = "resolved" }, codeMalformed},
		"a condition on a response": {func(d *researchDocs) {
			clause(d, 4)["when"] = map[string]any{"op": "any", "of": []any{clause(d, 4)["when"], map[string]any{"op": "exists", "field": "/sources/company-primary/responses"}}}
		}, codeMalformed},
		"16 earlier sources named": {func(d *researchDocs) {
			names := []any{}
			for i := 0; i < 16; i++ {
				names = append(names, "company-primary")
			}
			read(d)["sources"] = names
		}, codeOverBound},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, func(h *researchStandIn) researchPlan { return fallbackPlan(h, "gtm-company-person") }, func(h *researchStandIn, p *researchPlan) { fallback(p, "all-unavailable") })
			c.change(d)
			_, err := d.prepare(t)
			refusedFor(t, err, c.code)
		})
	}
	t.Run("the combined value nested beyond 32 levels", func(t *testing.T) {
		d := researchDocsOf(t, func(h *researchStandIn) researchPlan { return fallbackPlan(h, "gtm-company-person") }, func(h *researchStandIn, p *researchPlan) { fallback(p, "all-unavailable") })
		// 29 arrays in the primary's retained result: its record is nested
		// 32 levels, and the combined value 34. The result's receipt is
		// signed again over it, as a gateway would have signed it.
		var nested any = []any{}
		for i := 0; i < 28; i++ {
			nested = []any{nested}
		}
		retained := d.retained("company-primary", 0)
		retained["result"] = nested
		receipt := retained["receipt"].(map[string]any)
		receipt["resultDigest"] = digest(canonTest(t, encode(nested)))
		d.resign(t, receipt)
		_, err := d.prepare(t)
		refusedFor(t, err, codeOverBound)
	})
}

// A research acquisition is replayed, never asked again (spec/dialect.md
// §5.2, §11): each retained response's bytes are the ones its receipt
// names under the profile it came through, over its own request, within
// the source's maxAge, in one session form; the record's own receipt over
// the source's request and the whole record. Any change is refused, by
// the finding the specification gives it.
func TestAResearchAcquisitionIsReplayedNotAskedAgain(t *testing.T) {
	expired := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	receipt := func(d *researchDocs, source string, i int) map[string]any {
		return d.retained(source, i)["receipt"].(map[string]any)
	}
	for name, c := range map[string]struct {
		plan   func(*researchStandIn) researchPlan
		change func(*testing.T, *researchDocs)
		code   string
	}{
		"the research record not retained":        {gtmPlan, func(t *testing.T, d *researchDocs) { delete(d.sources, "person") }, codeMissing},
		"a response whose bytes are not retained": {gtmPlan, func(t *testing.T, d *researchDocs) { delete(d.retained("person", 2), "result") }, codeMissing},
		"a retained result changed": {gtmPlan, func(t *testing.T, d *researchDocs) {
			d.retained("person", 2)["result"].(map[string]any)["body"] = "Another page."
		}, codeInvalid},
		"a retained request changed": {gtmPlan, func(t *testing.T, d *researchDocs) {
			d.retained("person", 0)["request"].(map[string]any)["query"] = "Which?"
		}, codeInvalid},
		"the fields changed under the record's own receipt": {workItemPlan, func(t *testing.T, d *researchDocs) {
			d.answers("follow-up")["points"].(map[string]any)["value"] = json.Number("3")
		}, codeInvalid},
		"the record's own receipt over another request": {workItemPlan, func(t *testing.T, d *researchDocs) {
			d.source("follow-up")["constraints"] = map[string]any{"synthetic": true}
		}, codeInvalid},
		"a receipt of another source": {gtmPlan, func(t *testing.T, d *researchDocs) {
			r := receipt(d, "person", 1)
			r["source"] = "elsewhere"
			d.resign(t, r)
		}, codeInvalid},
		"a receipt under another key": {gtmPlan, func(t *testing.T, d *researchDocs) {
			receipt(d, "person", 1)["signature"] = strings.Repeat("0", 128)
		}, codeInvalid},
		"an observation time with a fraction": {gtmPlan, func(t *testing.T, d *researchDocs) {
			r := receipt(d, "person", 2)
			r["acquisition"].(map[string]any)["observedAt"] = d.at.Format("2006-01-02T15:04:05") + ".5Z"
			d.resign(t, r)
		}, codeInvalid},
		"the record's own receipt time with a fraction": {workItemPlan, func(t *testing.T, d *researchDocs) {
			r := d.response("follow-up")["receipt"].(map[string]any)
			r["servedAt"] = d.at.Format("2006-01-02T15:04:05") + ".5Z"
			d.resign(t, r)
		}, codeInvalid},
		"a serving time with an offset in place of Z": {gtmPlan, func(t *testing.T, d *researchDocs) {
			r := receipt(d, "person", 2)
			r["servedAt"] = d.at.Format("2006-01-02T15:04:05") + "+00:00"
			d.resign(t, r)
		}, codeInvalid},
		"the record's own observation time with an offset in place of Z": {workItemPlan, func(t *testing.T, d *researchDocs) {
			r := d.response("follow-up")["receipt"].(map[string]any)
			r["acquisition"].(map[string]any)["observedAt"] = d.at.Format("2006-01-02T15:04:05") + "+00:00"
			d.resign(t, r)
		}, codeInvalid},
		"an expired retained receipt": {gtmPlan, func(t *testing.T, d *researchDocs) {
			r := receipt(d, "person", 1)
			r["acquisition"].(map[string]any)["observedAt"] = expired
			d.resign(t, r)
		}, codeStale},
		"the record's own receipt expired": {workItemPlan, func(t *testing.T, d *researchDocs) {
			r := d.response("follow-up")["receipt"].(map[string]any)
			r["servedAt"], r["acquisition"].(map[string]any)["observedAt"] = expired, expired
			d.resign(t, r)
		}, codeStale},
		"two retained responses at one call index": {gtmPlan, func(t *testing.T, d *researchDocs) {
			r := receipt(d, "person", 1)
			r["callIndex"] = json.Number("1")
			d.resign(t, r)
		}, codeSessions},
		"the record's own receipt at a retained response's index": {workItemPlan, func(t *testing.T, d *researchDocs) {
			r := d.response("follow-up")["receipt"].(map[string]any)
			r["callIndex"] = json.Number("3")
			d.resign(t, r)
		}, codeSessions},
		"a rule's matched from that does not resolve": {gtmPlan, func(t *testing.T, d *researchDocs) {
			delete(d.answers("person"), "roles")
			rule := d.source("person")["read"].(map[string]any)["rule"].(map[string]any)
			first := rule["clauses"].([]any)[0].(map[string]any)["claim"].(map[string]any)
			first["facts"] = append(first["facts"].([]any), map[string]any{"pointer": "/person/roles", "from": "/fields/roles/value"})
			d.dialect["admits"].(map[string]any)["facts"].(map[string]any)["/person/roles"] = []any{"generated"}
		}, codeReadFailed},
		"a target that does not admit generated": {gtmPlan, func(t *testing.T, d *researchDocs) {
			d.dialect["admits"].(map[string]any)["facts"].(map[string]any)["/company"] = []any{"record"}
		}, codeAdmission},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, c.plan)
			c.change(t, d)
			_, err := d.prepare(t)
			refusedFor(t, err, c.code)
		})
	}
}

// Every receipt of a research source, retained responses and the record's
// own, is read whatever string its sessionId is (spec/dialect.md §5.2, runner
// #99): the citations carry it as given, and the session rule reads it so.
func TestAResearchReceiptsSessionIdIsAnyString(t *testing.T) {
	// inSession puts every receipt of the documents in one session. The
	// record's own receipt commits the record, retained receipts included,
	// so it is signed again over the changed record.
	inSession := func(t *testing.T, d *researchDocs, session string) int {
		receipts := 0
		for source := range d.sources {
			for i := range d.record(source)["responses"].([]any) {
				r := d.retained(source, i)["receipt"].(map[string]any)
				r["sessionId"] = session
				d.resign(t, r)
				receipts++
			}
			if r, ok := d.response(source)["receipt"].(map[string]any); ok {
				r["sessionId"] = session
				r["resultDigest"] = digest(canonTest(t, encode(d.record(source))))
				d.resign(t, r)
				receipts++
			}
		}
		return receipts
	}
	for name, session := range map[string]string{"spaces": "a session of mine", "a colon": "job:2026-10-16", "non-ASCII": "séance-日本", "empty": ""} {
		for plan, p := range map[string]func(*researchStandIn) researchPlan{"gtm-company-person": gtmPlan, "work-item-triage": workItemPlan} {
			t.Run(plan+" "+name, func(t *testing.T) {
				d := researchDocsOf(t, p)
				receipts := inSession(t, d, session)
				prepared, err := d.prepare(t)
				if err != nil {
					t.Fatal(err)
				}
				cites := prepared.Preparation.Cites
				if len(cites) != receipts || receipts < 2 {
					t.Fatalf("%d citations of %d receipts", len(cites), receipts)
				}
				for _, c := range cites {
					if c.SessionID != session {
						t.Fatalf("citation %+v", c)
					}
				}
				// The session rule still holds in such a session: two
				// receipts at one call index are one receipt twice.
				d = researchDocsOf(t, p)
				inSession(t, d, session)
				if plan == "gtm-company-person" {
					r := d.retained("person", 1)["receipt"].(map[string]any)
					r["callIndex"] = json.Number("1")
					d.resign(t, r)
				} else {
					r := d.response("follow-up")["receipt"].(map[string]any)
					r["callIndex"] = json.Number("3")
					d.resign(t, r)
				}
				_, err = d.prepare(t)
				refusedFor(t, err, codeSessions)
			})
		}
	}
}

// A research receipt whose sessionId is not a string is malformed
// (acquisition-invalid), a retained response's and the record's own alike.
func TestAResearchReceiptsSessionIdThatIsNoStringIsInvalid(t *testing.T) {
	for name, c := range map[string]struct {
		plan   func(*researchStandIn) researchPlan
		target func(d *researchDocs) map[string]any
	}{
		"a retained response's": {gtmPlan, func(d *researchDocs) map[string]any { return d.retained("person", 1)["receipt"].(map[string]any) }},
		"the record's own":      {workItemPlan, func(d *researchDocs) map[string]any { return d.response("follow-up")["receipt"].(map[string]any) }},
	} {
		for kind, value := range map[string]any{"a number": json.Number("7"), "null": nil} {
			t.Run(name+" "+kind, func(t *testing.T) {
				d := researchDocsOf(t, c.plan)
				r := c.target(d)
				r["sessionId"] = value
				d.resign(t, r)
				_, err := d.prepare(t)
				refusedFor(t, err, codeInvalid)
				if !strings.Contains(err.Error(), "invalid session id") {
					t.Fatal(err)
				}
			})
		}
	}
}

// A research source whose question takes an earlier source's fact, which
// that source does not resolve, is skipped: it is not acquired, and a record
// supplied for it is acquisition-unexpected (spec/dialect.md §3, §5.2).
func TestASkippedResearchSourceIsNotAcquired(t *testing.T) {
	follow := func(h *researchStandIn, p *researchPlan) {
		presence(p, "required-not-found")
		p.sources = append(p.sources, researchSourcePlan{
			name: "follow", capabilities: []string{"answer"}, question: `{"$param":"personId"}`, constraints: `{"synthetic":true}`,
			evidence: []string{"the answer"}, fields: []map[string]any{declare("note", "text", false, "A note.")},
			read: `{"copy":{"facts":[{"target":"/follow","source":"/fields"}],"evidence":[]}}`, ai: "assistant", web: map[string]string{},
			answers: map[string]any{"note": unfound("not-found")}, parameters: map[string]Parameter{"personId": {From: "person", Pointer: "/person/id", Type: "string"}},
		})
		p.admits = append(p.admits, "/follow")
	}
	d := researchDocsOf(t, gtmPlan, follow)
	_, err := d.prepare(t)
	refusedFor(t, err, codeUnexpected)
	delete(d.sources, "follow")
	prepared, err := d.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	last := prepared.Preparation.Outcomes[len(prepared.Preparation.Outcomes)-1]
	if last.Name != "follow" || last.Reason != "dependency-unavailable" || len(last.Arguments) != 0 {
		t.Fatalf("the skipped research source was acquired: %+v", last)
	}
}

// A research source is not acquired: Runner asks no model and reads no
// page, in the background or when a caller plans its acquisitions. Planning
// names the source and its request, the recipe, for the caller to run.
func TestAResearchSourceIsNotAcquiredByRunner(t *testing.T) {
	d := researchDocsOf(t, gtmPlan)
	input := d.input(t)
	bound, err := bindDialect(*input.Source.Import, d.profiles)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: Config{InputProfiles: d.profiles}}
	if err = s.validateGatewayMapping(Release{InputMapping: &bound}); err == nil || !strings.Contains(err.Error(), "Runner asks no model and reads no page") {
		t.Fatal("a research source was taken for background acquisition:", err)
	}
	planned := cloneInput(input)
	planned.Source.Sources = map[string]SourceValue{}
	plan, err := nextInput(planned, d.profiles, d.at)
	if err != nil || plan.Next == nil || plan.Next.Name != "company" || plan.Next.Kind != "research" || plan.Next.Profile != "assistant" {
		t.Fatalf("planning does not name the research source to run: %+v %v", plan.Next, err)
	}
	var request map[string]any
	if json.Unmarshal(plan.Next.Arguments, &request) != nil || request["model"] != researchModel {
		t.Fatalf("the planned request is not the recipe: %s", plan.Next.Arguments)
	}
}

// A release freezes every profile a research source is bound to, its AI
// profile and its web profiles; a run's trust configuration must still hold
// each.
func TestAReleaseFreezesEveryProfileOfAResearchSource(t *testing.T) {
	d := researchDocsOf(t, gtmPlan)
	input := d.input(t)
	bound, err := bindDialect(*input.Source.Import, d.profiles)
	if err != nil {
		t.Fatal(err)
	}
	used := profilesUsed(bound, d.profiles)
	if len(used) != 3 {
		t.Fatalf("the release freezes %d profiles, not the three the research sources are bound to", len(used))
	}
	s := &Service{cfg: Config{InputProfiles: d.profiles}}
	if err = s.checkReleaseProfiles(Release{InputMapping: &bound, InputProfiles: used}); err != nil {
		t.Fatal(err)
	}
	changed := append([]InputProfile(nil), d.profiles...)
	changed[2].Authority = "another"
	s.cfg.InputProfiles = changed
	if err = s.checkReleaseProfiles(Release{InputMapping: &bound, InputProfiles: used}); err == nil {
		t.Fatal("a changed web profile was taken as the one the release froze")
	}
	s.cfg.InputProfiles = d.profiles
	if err = s.checkReleaseProfiles(Release{InputMapping: &bound, InputProfiles: used[:2]}); err == nil {
		t.Fatal("a web profile the release did not freeze was taken as one it froze")
	}
}

// A research source's own receipt commits the parameters its question
// carries; its retained responses' receipts commit their own requests, not
// the question. A rule that reads a parameter the question carries is
// unsigned unless the record has its own receipt.
func TestAResearchRulesParametersAreSignedOnlyByTheRecordsOwnReceipt(t *testing.T) {
	read := func(d *researchDocs, source string) {
		rule := d.source(source)["read"].(map[string]any)["rule"].(map[string]any)
		rule["parameters"] = map[string]any{"itemNumber": "integer"}
		first := rule["clauses"].([]any)[0].(map[string]any)
		first["when"] = map[string]any{"op": "all", "of": []any{first["when"], map[string]any{"op": "equalsParam", "field": "/fields/points/value", "param": "itemNumber"}}}
	}
	d := researchDocsOf(t, workItemPlan)
	read(d, "item")
	prepared, err := d.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := unsignedParameters(prepared.Source.Mapping, prepared.Preparation)
	if len(unsigned) != 1 || unsigned[0].Source != "item" || unsigned[0].Parameters[0] != (UnsignedParameter{"itemNumber", "case"}) {
		t.Fatalf("the parameter a research rule reads is not named unsigned: %+v", unsigned)
	}
	// The follow-up's record has its own receipt, which commits its
	// question's $param.
	own := researchDocsOf(t, workItemPlan)
	followUp := own.source("follow-up")
	followUp["read"] = map[string]any{"rule": map[string]any{"ruleVersion": "1", "parameters": map[string]any{"followUpQuestion": "string"}, "clauses": []any{
		map[string]any{"when": map[string]any{"op": "all", "of": []any{map[string]any{"op": "equals", "field": "/fields/follow-up-id/status", "to": "found"}, map[string]any{"op": "equalsParam", "field": "/fields/follow-up-id/value", "param": "followUpQuestion"}}}, "claim": map[string]any{"facts": []any{map[string]any{"pointer": "/follow-up", "from": "/fields/follow-up-id/value"}}, "evidence": map[string]any{}, "acquisitionStatus": "resolved"}, "reason": "found"},
		map[string]any{"when": map[string]any{"op": "always"}, "claim": map[string]any{"facts": []any{}, "evidence": map[string]any{}, "acquisitionStatus": "unknown"}, "reason": "otherwise"},
	}}}
	// The record's own receipt is over the source's request, which the read
	// is not part of: it still verifies.
	prepared, err = own.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range unsignedParameters(prepared.Source.Mapping, prepared.Preparation) {
		if u.Source == "follow-up" {
			t.Fatalf("a parameter the record's own receipt commits is named unsigned: %+v", u)
		}
	}
}

// A research receipt's observation and serving instants are each held to
// the window §5.2 gives, against the preparation's instant: no more than the
// source's maxAge before it and no more than 30 seconds after it, the edges
// inside; no rule joins the two instants (the review of #97, finding 1).
func TestAResearchReceiptsTimesAreEachWithinTheWindow(t *testing.T) {
	for _, member := range []string{"servedAt", "observedAt"} {
		for name, c := range map[string]struct {
			offset time.Duration
			code   string
		}{
			"at the lower edge":          {-3600 * time.Second, ""},
			"a second before the window": {-3601 * time.Second, codeStale},
			"at the upper edge":          {30 * time.Second, ""},
			"a second after the window":  {31 * time.Second, codeStale},
		} {
			t.Run(member+" "+name, func(t *testing.T) {
				d := researchDocsOf(t, gtmPlan)
				r := d.retained("person", 1)["receipt"].(map[string]any)
				instant := d.at.Add(c.offset).Format(time.RFC3339)
				if member == "servedAt" {
					r["servedAt"] = instant
				} else {
					r["acquisition"].(map[string]any)["observedAt"] = instant
				}
				d.resign(t, r)
				_, err := d.prepare(t)
				if c.code == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				refusedFor(t, err, c.code)
			})
		}
	}
	// Observed 3630 seconds after it was served, each within the window: read.
	d := researchDocsOf(t, gtmPlan)
	r := d.retained("person", 1)["receipt"].(map[string]any)
	r["servedAt"], r["acquisition"].(map[string]any)["observedAt"] = d.at.Add(-3600*time.Second).Format(time.RFC3339), d.at.Add(30*time.Second).Format(time.RFC3339)
	d.resign(t, r)
	if _, err := d.prepare(t); err != nil {
		t.Fatal("an observation after its service, both within the window, was refused:", err)
	}
}

// A research receipt's acquisition.adapter is its profile's adapter, as a
// whole object (§5.2, check 3; the review of #97, finding 2): a member more
// or a member fewer is acquisition-invalid, though the receipt is signed.
func TestAResearchReceiptsAdapterIsItsProfilesWhole(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"a member more":  func(a map[string]any) { a["extra"] = "not pinned" },
		"a member fewer": func(a map[string]any) { delete(a, "version") },
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, gtmPlan)
			r := d.retained("person", 2)["receipt"].(map[string]any)
			change(r["acquisition"].(map[string]any)["adapter"].(map[string]any))
			d.resign(t, r)
			_, err := d.prepare(t)
			refusedFor(t, err, codeInvalid)
		})
	}
}

// The research record's maxima come first (§11, §12; the review of #97,
// finding 3), after the dialect's own checks: every measurable member of the
// record and of its retained results is measured before its form, a
// profile that does not fit or any acquisition finding.
func TestAResearchRecordsMaximaComeFirst(t *testing.T) {
	long := strings.Repeat("1", 129)
	for name, c := range map[string]struct {
		change func(*researchDocs)
		code   string
	}{
		"65 responses beside a member more in the envelope": {func(d *researchDocs) {
			r := d.record("company")
			for len(r["responses"].([]any)) < 65 {
				r["responses"] = append(r["responses"].([]any), d.retained("company", 0))
			}
			d.response("company")["extra"] = true
		}, codeOverBound},
		"a number of 129 characters in a retained result": {func(d *researchDocs) {
			d.retained("person", 1)["result"].(map[string]any)["status"] = json.Number(long)
		}, codeOverBound},
		"an exponent over 308 in a retained result": {func(d *researchDocs) {
			d.retained("person", 1)["result"].(map[string]any)["status"] = json.Number("1e309")
		}, codeOverBound},
		"a record nested 33 levels": {func(d *researchDocs) {
			var nested any = []any{}
			for i := 0; i < 29; i++ {
				nested = []any{nested}
			}
			d.retained("person", 1)["result"] = nested
		}, codeOverBound},
		"a response through an unbound profile beside 65 responses": {func(d *researchDocs) {
			r := d.record("company")
			for len(r["responses"].([]any)) < 65 {
				r["responses"] = append(r["responses"].([]any), d.retained("company", 0))
			}
			search := d.profiles[1]
			d.retained("person", 0)["profile"] = map[string]any{"id": search.ID, "digest": profileHash(search)}
		}, codeOverBound},
		// The binding is the dialect's: its profiles are held at import and in
		// preflight, before any record is read. A binding by a wrong digest
		// beside an over-bound record is profile-mismatch, as
		// docs/MAPPING-V2.md says ("Research sources", the replay).
		"a wrong binding digest beside 65 responses": {func(d *researchDocs) {
			r := d.record("company")
			for len(r["responses"].([]any)) < 65 {
				r["responses"] = append(r["responses"].([]any), d.retained("company", 0))
			}
			d.assignment("company")["profileDigest"] = digest([]byte("an old profile"))
		}, codeMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, gtmPlan)
			c.change(d)
			_, err := d.prepare(t)
			refusedFor(t, err, c.code)
		})
	}
}

// An unwrapped research value is measured as it is unwrapped (§11, §12; the
// review of #97, finding 4): nested beyond 32 levels it is input-over-bound,
// kept apart from a pointer that does not resolve and a string that holds no
// JSON, which are read-failed.
func TestAnUnwrappedResearchValueIsMeasured(t *testing.T) {
	nested := func(levels int) string { return strings.Repeat("[", levels) + strings.Repeat("]", levels) }
	for name, c := range map[string]struct {
		value, unwrap, code string
	}{
		"nested 33 levels":             {nested(33), "/fields/company-id/value", codeOverBound},
		"nested 32 levels":             {nested(32), "/fields/company-id/value", ""},
		"a string that holds no JSON":  {"not JSON", "/fields/company-id/value", codeReadFailed},
		"a value that is not a string": {nested(1), "/fields/employee-count/value", codeReadFailed},
	} {
		t.Run(name, func(t *testing.T) {
			d := researchDocsOf(t, gtmPlan)
			d.answers("company")["company-id"].(map[string]any)["value"] = c.value
			d.source("company")["read"].(map[string]any)["unwrap"] = []any{c.unwrap}
			_, err := d.prepare(t)
			if c.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			refusedFor(t, err, c.code)
		})
	}
}

// A source of a kind other than research has no read.sources: one that
// names it is refused at decoding, by the decoder's own sentence, in
// version 2 and in version 3, as before research was read (the review of
// #97, finding 5). Every other refusal of the decoder keeps its sentence.
func TestAnOperationsReadNamingSourcesIsRefusedAtDecoding(t *testing.T) {
	operation := `{"name":"vendor","kind":"operation","profile":"vendors","profileDigest":"sha256:` + strings.Repeat("0", 64) + `","maxAge":300,"arguments":{"tool":"execute_sql","arguments":{}},"read":%s}`
	source := func(read string) string { return strings.Replace(operation, "%s", read, 1) }
	for name, c := range map[string]struct{ mapping, want string }{
		"version 2":                      {`{"version":2,"provider":"","facts":null,"evidence":null,"case":{"facts":[],"evidence":[]},"sources":[` + source(`{"sources":["vendor"],"copy":{"facts":[],"evidence":[]}}`) + `]}`, `json: unknown field "sources"`},
		"version 3":                      {`{"version":3,"case":{"facts":[],"evidence":[]},"sources":[` + source(`{"copy":{"facts":[],"evidence":[]},"sources":["vendor"]}`) + `]}`, `json: unknown field "sources"`},
		"a read's type error, as before": {`{"version":2,"case":{"facts":[],"evidence":[]},"sources":[` + source(`{"unwrap":"x","copy":{"facts":[],"evidence":[]}}`) + `]}`, "json: cannot unmarshal string into Go struct field SourceRead.sources.read.unwrap of type []string"},
	} {
		t.Run(name, func(t *testing.T) {
			var m InputMapping
			if err := strictJSON([]byte(c.mapping), &m); err == nil || err.Error() != c.want {
				t.Fatalf("decoding found %v, not %s", err, c.want)
			}
			h := (&Service{}).Handler("test")
			w := postTo(h, "/v1/inputs/preview", []byte(`{"source":{"mapping":`+c.mapping+`,"case":{}}}`))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_request"`) {
				t.Fatal("the request was not refused at decoding:", w.Code, w.Body.String())
			}
		})
	}
	// A research source's read names them, and is decoded.
	d := researchDocsOf(t, func(h *researchStandIn) researchPlan { return fallbackPlan(h, "gtm-company-person") }, func(h *researchStandIn, p *researchPlan) { fallback(p, "all-unavailable") })
	var m InputMapping
	if err := strictJSON(encode(d.dialect), &m); err != nil || len(m.Sources[2].Read.Sources) == 0 {
		t.Fatal("a research source's reconciliation was not decoded:", err)
	}
}
