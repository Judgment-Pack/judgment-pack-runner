package runner

import (
	"encoding/json"
	"testing"
	"time"
)

// The two examples of the specification's fixture plan (spec/dialect.md
// §11, "The two fixture lists to record"): gtm-company-person, a company's
// six fields and a person's, and work-item-triage, a work item's six and
// its follow-up record's. Every identifier and observation is synthetic.
// The tests prepare them from the stand-in's responses (research_test.go),
// and the recordings of the preparation vectors run them through Runtime
// (research_vectors_test.go).

// researchAsk is one retained response of a plan: the capability asked,
// the profile it is asked through, and, for a page, the item it names.
type researchAsk struct{ capability, profile, item string }

// researchSourcePlan is one research source of a plan: its recipe, its
// read, the profiles it is bound to, the responses it retains, in order,
// and its record's answers, citing them by their indexes.
type researchSourcePlan struct {
	name         string
	capabilities []string
	question     string
	constraints  string
	evidence     []string
	fields       []map[string]any
	read         string
	ai           string
	web          map[string]string
	asks         []researchAsk
	answers      map[string]any
	own          bool
	// parameters are the source's own, taken from earlier sources' facts.
	parameters map[string]Parameter
}

// researchPlan is one run of an example: its case, its research sources,
// the fact targets that admit generated values, and the installation's
// profiles.
type researchPlan struct {
	example    string
	parameters map[string]Parameter
	caseDoc    string
	sources    []researchSourcePlan
	admits     []string
	profiles   []InputProfile
}

func (p *researchPlan) source(name string) *researchSourcePlan {
	for i := range p.sources {
		if p.sources[i].name == name {
			return &p.sources[i]
		}
	}
	panic("no research source " + name)
}

// researchRequestCase is the triage request and its evidence, which the
// case supplies to the test pack as every fixture's does.
const researchRequestCase = `"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}`

func declare(name, kind string, required bool, description string) map[string]any {
	return map[string]any{"name": name, "type": kind, "required": required, "description": description}
}

func found(value any, cites ...int) map[string]any {
	return map[string]any{"status": "found", "value": value, "cites": cites}
}

func unfound(status string, cites ...int) map[string]any {
	return map[string]any{"status": status, "cites": append([]int{}, cites...)}
}

func conflicting(observations ...map[string]any) map[string]any {
	return map[string]any{"status": "conflicting", "cites": []int{}, "values": append([]map[string]any{}, observations...)}
}

func observed(value any, cites ...int) map[string]any {
	return map[string]any{"value": value, "cites": cites}
}

// gtmPlan is gtm-company-person's valid-types run: a company source asking
// for answer alone, whose read copies its whole record's fields, and a
// person source asking for answer, search and page, whose rule writes the
// person's id and a qualification target from found values. All six types,
// both integer and decimal ranges, an empty list and a list with values,
// signed decimals of different scales.
func gtmPlan(h *researchStandIn) researchPlan {
	return researchPlan{
		example:    "gtm-company-person",
		parameters: map[string]Parameter{"companyNumber": {Pointer: "/companyNumber", Type: "integer"}, "personNumber": {Pointer: "/personNumber", Type: "integer"}},
		caseDoc:    `{"companyNumber":7,"personNumber":12,` + researchRequestCase + `}`,
		sources: []researchSourcePlan{{
			name:         "company",
			capabilities: []string{"answer"},
			question:     `{"$text":"Describe the synthetic company numbered {{companyNumber}}: its identifier, its headcount, its revenue, whether it is active, the roles it hires for and its headcount band."}`,
			constraints:  `{"language":"en","synthetic":true}`,
			evidence:     []string{"the answer that states each figure"},
			fields: []map[string]any{
				declare("company-id", "text", true, "The company's synthetic identifier."),
				declare("employee-count", "integer", true, "The company's headcount, as cited."),
				declare("revenue", "decimal", true, "The company's annual revenue, a decimal string."),
				declare("active", "boolean", true, "Whether the company trades."),
				declare("roles", "text-list", false, "The roles the company hires for."),
				declare("employee-range", "range", false, "The company's headcount band."),
			},
			read: `{"copy":{"facts":[{"target":"/company","source":"/fields"}],"evidence":[]}}`,
			ai:   "assistant", web: map[string]string{},
			asks: []researchAsk{{"answer", "assistant", ""}},
			answers: map[string]any{
				"company-id":     found("synthetic-company-0007", 0),
				"employee-count": found(json.Number("240"), 0),
				"revenue":        found("18250000.00", 0),
				"active":         found(true, 0),
				"roles":          found([]string{"data-engineer", "analyst"}, 0),
				"employee-range": found(map[string]any{"min": json.Number("200"), "max": json.Number("250")}, 0),
			},
		}, {
			name:         "person",
			capabilities: []string{"answer", "search", "page"},
			question:     `{"$text":"Describe the synthetic person numbered {{personNumber}}: their identifier, years of experience, allocation, whether they may be contacted, their roles and their experience band."}`,
			constraints:  `{"language":"en","synthetic":true}`,
			evidence:     []string{"the page that states whether the person may be contacted"},
			fields: []map[string]any{
				declare("person-id", "text", true, "The person's synthetic identifier."),
				declare("experience-years", "integer", true, "The person's years of experience."),
				declare("allocation", "decimal", true, "The share of the person's time that is allocated, a signed decimal."),
				declare("contactable", "boolean", true, "Whether the person may be contacted."),
				declare("roles", "text-list", false, "The person's roles."),
				declare("experience-range", "range", false, "The person's experience band, in years."),
			},
			read: `{"rule":{"ruleVersion":"1","parameters":{},"clauses":[` +
				`{"when":{"op":"all","of":[{"op":"equals","field":"/fields/person-id/status","to":"found"},{"op":"equals","field":"/fields/contactable/status","to":"found"}]},"claim":{"facts":[{"pointer":"/person/id","from":"/fields/person-id/value"},{"pointer":"/qualification/contactable","from":"/fields/contactable/value"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"person-and-contactability-found"},` +
				`{"when":{"op":"exists","field":"/fields/contactable/values/0/value"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"contactability-conflicting"},` +
				`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"person-or-contactability-not-found"}]}}`,
			ai: "assistant", web: map[string]string{"search": "search", "page": "page"},
			asks: []researchAsk{{"answer", "assistant", ""}, {"search", "search", ""}, {"page", "page", "person-0012"}},
			answers: map[string]any{
				"person-id":        found("synthetic-person-0012", 0, 2),
				"experience-years": found(json.Number("6"), 0),
				"allocation":       found("-0.25", 0),
				"contactable":      found(true, 2),
				"roles":            found([]string{}, 0),
				"experience-range": found(map[string]any{"min": "2.50", "max": "4.0"}, 0, 1),
			},
		}},
		admits:   []string{"/company", "/person/id", "/qualification/contactable"},
		profiles: []InputProfile{h.aiProfile("assistant", "/answers"), h.searchProfile("search"), h.pageProfile("page", "/page")},
	}
}

// workItemPlan is work-item-triage's valid-types run: a work item source
// asking for answer, search and page, its search served by its AI profile,
// whose rule writes the item's id, its points and whether it is blocked;
// and its follow-up record's source, asking for answer alone, whose record
// came back through the AI connection with a receipt of its own, and whose
// read copies the whole record's fields. Its question is a $param.
func workItemPlan(h *researchStandIn) researchPlan {
	return researchPlan{
		example:    "work-item-triage",
		parameters: map[string]Parameter{"itemNumber": {Pointer: "/itemNumber", Type: "integer"}, "followUpQuestion": {Pointer: "/followUpQuestion", Type: "string"}},
		caseDoc:    `{"itemNumber":42,"followUpQuestion":"Describe the synthetic follow-up record FU-0043 of the synthetic work item WI-0042.",` + researchRequestCase + `}`,
		sources: []researchSourcePlan{{
			name:         "item",
			capabilities: []string{"answer", "search", "page"},
			question:     `{"$text":"Describe the synthetic work item numbered {{itemNumber}}: its identifier, its points, its estimate in days, whether it is blocked, its labels and its effort band."}`,
			constraints:  `{"synthetic":true,"tracker":"stand-in"}`,
			evidence:     []string{"the page that states whether the item is blocked"},
			fields: []map[string]any{
				declare("item-id", "text", true, "The work item's synthetic identifier."),
				declare("points", "integer", true, "The work item's points."),
				declare("estimate", "decimal", true, "The work item's estimate in days."),
				declare("blocked", "boolean", true, "Whether the work item is blocked."),
				declare("labels", "text-list", false, "The work item's labels, as written."),
				declare("effort-range", "range", false, "The work item's effort band, in days."),
			},
			read: `{"rule":{"ruleVersion":"1","parameters":{},"clauses":[` +
				`{"when":{"op":"all","of":[{"op":"equals","field":"/fields/item-id/status","to":"found"},{"op":"equals","field":"/fields/points/status","to":"found"},{"op":"equals","field":"/fields/blocked/status","to":"found"}]},"claim":{"facts":[{"pointer":"/work-item/id","from":"/fields/item-id/value"},{"pointer":"/work-item/points","from":"/fields/points/value"},{"pointer":"/triage/blocked","from":"/fields/blocked/value"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"item-found"},` +
				`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"item-not-established"}]}}`,
			ai: "assistant", web: map[string]string{"search": "assistant", "page": "page"},
			asks: []researchAsk{{"answer", "assistant", ""}, {"search", "assistant", ""}, {"page", "page", "work-item-0042"}},
			answers: map[string]any{
				"item-id":      found("WI-0042", 0),
				"points":       found(json.Number("5"), 0, 1),
				"estimate":     found("3.5", 0),
				"blocked":      found(true, 2),
				"labels":       found([]string{"backend", "backend", ""}, 0),
				"effort-range": found(map[string]any{"min": "-0.5", "max": "2.25"}, 0),
			},
		}, {
			name:         "follow-up",
			capabilities: []string{"answer"},
			question:     `{"$param":"followUpQuestion"}`,
			constraints:  `{"synthetic":true,"tracker":"stand-in"}`,
			evidence:     []string{"the answer that states the follow-up record"},
			fields: []map[string]any{
				declare("follow-up-id", "text", true, "The follow-up record's synthetic identifier."),
				declare("points", "integer", false, "The follow-up record's points."),
				declare("estimate", "decimal", false, "The follow-up record's estimate in days."),
				declare("blocked", "boolean", false, "Whether the follow-up record is blocked."),
				declare("labels", "text-list", false, "The follow-up record's labels."),
				declare("effort-range", "range", false, "The follow-up record's effort band, in whole days."),
			},
			read: `{"copy":{"facts":[{"target":"/follow-up","source":"/fields"}],"evidence":[]}}`,
			ai:   "assistant", web: map[string]string{},
			asks: []researchAsk{{"answer", "assistant", ""}},
			answers: map[string]any{
				"follow-up-id": found("FU-0043", 0),
				"points":       found(json.Number("2"), 0),
				"estimate":     found("1.0", 0),
				"blocked":      found(false, 0),
				"labels":       found([]string{}, 0),
				"effort-range": found(map[string]any{"min": json.Number("1"), "max": json.Number("3")}, 0),
			},
			own: true,
		}},
		admits:   []string{"/work-item/id", "/work-item/points", "/triage/blocked", "/follow-up"},
		profiles: []InputProfile{h.aiProfile("assistant", "/answers"), h.pageProfile("page", "/page")},
	}
}

// fallbackPlan is an example's failed-fallback run (§6.4, §11's fifth
// item): a primary, a secondary and a third source, each asking for the
// example's count field, the first two copying their entry's status, and
// the third reconciling the two before it and itself by a rule that tests
// each status in a declared order: the primary's found value, then the
// secondary's, a conflict anywhere unknown, then its own found value, and
// unknown when no candidate is left. Each absence, not-found, stale and
// conflicting status is named.
func fallbackPlan(h *researchStandIn, example string) researchPlan {
	base, field, target := "company", "employee-count", "/company/employee-count"
	plan := researchPlan{example: example, parameters: map[string]Parameter{"companyNumber": {Pointer: "/companyNumber", Type: "integer"}}, caseDoc: `{"companyNumber":7,` + researchRequestCase + `}`}
	if example == "work-item-triage" {
		base, field, target = "item", "points", "/work-item/points"
		plan.parameters = map[string]Parameter{"itemNumber": {Pointer: "/itemNumber", Type: "integer"}}
		plan.caseDoc = `{"itemNumber":42,` + researchRequestCase + `}`
	}
	primary, secondary, reconciled := base+"-primary", base+"-secondary", base+"-reconciled"
	entry := func(source string) string { return "/sources/" + source + "/fields/" + field }
	current := "/current/fields/" + field
	guard := func(entry, status string) string {
		return `{"op":"equals","field":"` + entry + `/status","to":"` + status + `"}`
	}
	absent := func(entry string) string { return `{"op":"not","of":{"op":"exists","field":"` + entry + `"}}` }
	unavailable := func(entry string) string {
		return `{"op":"any","of":[` + absent(entry) + `,` + guard(entry, "not-found") + `,` + guard(entry, "stale") + `]}`
	}
	clause := func(when, reason, from, status string) string {
		facts := `[]`
		if from != "" {
			facts = `[{"pointer":"` + target + `","from":"` + from + `"}]`
		}
		return `{"when":` + when + `,"claim":{"facts":` + facts + `,"evidence":{},"acquisitionStatus":"` + status + `"},"reason":"` + reason + `"}`
	}
	rule := `{"sources":["` + primary + `","` + secondary + `"],"rule":{"ruleVersion":"1","parameters":{},"clauses":[` +
		clause(guard(entry(primary), "found"), "the primary found it", entry(primary)+"/value", "resolved") + `,` +
		clause(`{"op":"all","of":[`+unavailable(entry(primary))+`,`+guard(entry(secondary), "found")+`]}`, "the secondary found it", entry(secondary)+"/value", "resolved") + `,` +
		clause(`{"op":"any","of":[`+guard(entry(primary), "conflicting")+`,`+guard(entry(secondary), "conflicting")+`,`+guard(current, "conflicting")+`]}`, "a conflict is not chosen", "", "unknown") + `,` +
		clause(`{"op":"all","of":[`+unavailable(entry(secondary))+`,`+guard(current, "found")+`]}`, "this source found it", current+"/value", "resolved") + `,` +
		clause(unavailable(current), "no candidate is left", "", "unknown") + `,` +
		clause(`{"op":"always"}`, "otherwise", "", "unknown") + `]}}`
	question := func(who string) string {
		if example == "work-item-triage" {
			return `{"$text":"How many points does the synthetic work item numbered {{itemNumber}} carry? Ask the ` + who + ` tracker."}`
		}
		return `{"$text":"How many people does the synthetic company numbered {{companyNumber}} employ? Ask the ` + who + ` register."}`
	}
	count := []map[string]any{declare(field, "integer", false, "The count, as cited.")}
	observe := func(name string) string {
		return `{"copy":{"facts":[{"target":"/observed/` + name + `","source":"/fields/` + field + `/status"}],"evidence":[]}}`
	}
	for _, s := range []struct{ name, who, read string }{{primary, "primary", observe("primary")}, {secondary, "secondary", observe("secondary")}, {reconciled, "third", rule}} {
		plan.sources = append(plan.sources, researchSourcePlan{
			name: s.name, capabilities: []string{"answer"}, question: question(s.who), constraints: `{"synthetic":true}`,
			evidence: []string{"the answer that states the count"}, fields: count, read: s.read,
			ai: "assistant", web: map[string]string{}, asks: []researchAsk{{"answer", "assistant", ""}}, answers: map[string]any{},
		})
	}
	plan.admits = []string{"/observed/primary", "/observed/secondary", target}
	plan.profiles = []InputProfile{h.aiProfile("assistant", "/answers")}
	return plan
}

// researchInput is a plan's input as an installation imports it: the
// published dialect, whose research sources name no profile, the binding
// document that binds each to the plan's profiles, the case, and each
// source's research record, its responses asked of the stand-in at, in the
// order of the sources and of each source's asks.
func (h *researchStandIn) researchInput(t *testing.T, plan researchPlan, at time.Time) Input {
	t.Helper()
	published := plan.published(t)
	raw := encode(published)
	profiles := map[string]InputProfile{}
	for _, p := range plan.profiles {
		profiles[p.ID] = p
	}
	assign := func(id string) map[string]string {
		return map[string]string{"profile": id, "profileDigest": profileHash(profiles[id])}
	}
	bindings := map[string]any{}
	for _, s := range plan.sources {
		web := map[string]any{}
		for capability, id := range s.web {
			web[capability] = assign(id)
		}
		bindings[s.name] = map[string]any{"profile": s.ai, "profileDigest": profileHash(profiles[s.ai]), "webProfiles": web}
	}
	binding := encode(map[string]any{"dialect": digest(canonTest(t, raw)), "sources": bindings})
	bound, err := bindDialect(DialectImport{Dialect: raw, Binding: binding}, plan.profiles)
	if err != nil {
		t.Fatal("the plan does not bind:", err)
	}
	caseValue, err := readInputJSON([]byte(plan.caseDoc))
	if err != nil {
		t.Fatal(err)
	}
	params, types := map[string]any{}, map[string]string{"runAt": "timestamp"}
	for n, p := range plan.parameters {
		tokens, _ := pointerTokens(p.Pointer, true)
		params[n], _ = pointerRead(caseValue, tokens)
		types[n] = p.Type
	}
	sources := map[string]SourceValue{}
	for i, s := range plan.sources {
		// A source whose question takes an earlier source's fact asks
		// nothing here: its record retains no response.
		if len(s.parameters) > 0 {
			sources[s.name] = SourceValue{Response: encode(map[string]any{"result": map[string]any{"fields": s.answers, "responses": []any{}}})}
			continue
		}
		source := bound.Sources[i]
		request, err := researchRequest(source, params, types)
		if err != nil {
			t.Fatal(err)
		}
		var asked struct {
			Question string `json:"question"`
		}
		if err = json.Unmarshal(request, &asked); err != nil {
			t.Fatal(err)
		}
		h.answers[asked.Question] = map[string]any{"text": "A synthetic answer. Every figure in it is invented for a test.", "fields": s.answers}
		responses := []any{}
		for _, ask := range s.asks {
			responses = append(responses, h.retain(t, profiles[ask.profile], ask.capability, asked.Question, ask.item, at))
		}
		record := encode(map[string]any{"fields": s.answers, "responses": responses})
		response := map[string]any{"result": json.RawMessage(record)}
		if s.own {
			response["receipt"], response["salts"] = h.own(t, profiles[s.ai], request, record, at)
		}
		sources[s.name] = SourceValue{Response: encode(response)}
	}
	return Input{Source: &SourceInput{Import: &DialectImport{Dialect: raw, Binding: binding}, Case: json.RawMessage(plan.caseDoc), Sources: sources}}
}

// published is the plan's dialect as its publisher writes it, of version
// 3, its research sources unbound.
func (p researchPlan) published(t *testing.T) InputMapping {
	t.Helper()
	admits := map[string][]string{}
	for _, target := range p.admits {
		admits[target] = []string{"generated"}
	}
	m := InputMapping{Version: 3,
		Case: &CaseMapping{Parameters: p.parameters, Facts: []FactMapping{{Target: "/request", Source: "/request"}},
			Evidence: []EvidenceMapping{{Requirement: "intake-form", Source: "/evidence/intake-form"}, {Requirement: "sponsor-endorsement", Source: "/evidence/sponsor-endorsement"}}},
		Admits:           &Admission{Facts: admits},
		UnmappedEvidence: []string{"sensitive-data-approvals"},
	}
	for _, s := range p.sources {
		source := MappingSource{Name: s.name, Kind: "research", MaxAge: 3600, Parameters: s.parameters,
			Question: json.RawMessage(s.question), Constraints: json.RawMessage(s.constraints), Model: encode(researchModel),
			Evidence: encode(s.evidence), Fields: encode(s.fields), Capabilities: encode(s.capabilities)}
		if err := strictJSON([]byte(s.read), &source.Read); err != nil {
			t.Fatal(s.name, err)
		}
		m.Sources = append(m.Sources, source)
	}
	return m
}

// Variants of a plan (spec/dialect.md §11): presence, stale and conflicting
// observations, the fallbacks, and the second installation's binding.

// presence applies a presence variant to a valid-types plan: an omitted
// optional entry, a required entry not-found, integer zero, decimal "0",
// false and the empty text, each a value of its type, none a default.
func presence(p *researchPlan, variant string) {
	type change struct {
		source, field string
		entry         map[string]any
	}
	var changes []change
	switch p.example + "/" + variant {
	case "gtm-company-person/optional-omitted":
		changes = []change{{"company", "roles", nil}, {"company", "employee-range", nil}, {"person", "roles", nil}, {"person", "experience-range", nil}}
	case "gtm-company-person/required-not-found":
		changes = []change{{"company", "employee-count", unfound("not-found", 0)}, {"person", "contactable", unfound("not-found", 2)}}
	case "gtm-company-person/integer-zero":
		changes = []change{{"company", "employee-count", found(json.Number("0"), 0)}, {"person", "experience-years", found(json.Number("0"), 0)}}
	case "gtm-company-person/decimal-zero":
		changes = []change{{"company", "revenue", found("0", 0)}, {"person", "allocation", found("0", 0)}}
	case "gtm-company-person/boolean-false":
		changes = []change{{"company", "active", found(false, 0)}, {"person", "contactable", found(false, 2)}}
	case "gtm-company-person/text-empty":
		changes = []change{{"company", "company-id", found("", 0)}, {"person", "person-id", found("", 0, 2)}}
	case "work-item-triage/optional-omitted":
		changes = []change{{"item", "labels", nil}, {"item", "effort-range", nil}, {"follow-up", "points", nil}, {"follow-up", "estimate", nil}, {"follow-up", "blocked", nil}, {"follow-up", "labels", nil}, {"follow-up", "effort-range", nil}}
	case "work-item-triage/required-not-found":
		changes = []change{{"item", "blocked", unfound("not-found", 2)}, {"follow-up", "follow-up-id", unfound("not-found")}}
	case "work-item-triage/integer-zero":
		changes = []change{{"item", "points", found(json.Number("0"), 0, 1)}, {"follow-up", "points", found(json.Number("0"), 0)}}
	case "work-item-triage/decimal-zero":
		changes = []change{{"item", "estimate", found("0", 0)}, {"follow-up", "estimate", found("0", 0)}}
	case "work-item-triage/boolean-false":
		changes = []change{{"item", "blocked", found(false, 2)}, {"follow-up", "blocked", found(false, 0)}}
	case "work-item-triage/text-empty":
		changes = []change{{"item", "item-id", found("", 0)}, {"follow-up", "follow-up-id", found("", 0)}}
	default:
		panic("no presence variant " + variant)
	}
	for _, c := range changes {
		answers := p.source(c.source).answers
		if c.entry == nil {
			delete(answers, c.field)
		} else {
			answers[c.field] = c.entry
		}
	}
}

// staleAndConflicting gives a valid-types plan fresh receipts carrying an
// explicitly stale observation, one with no cited observation, and
// conflicting typed values, each with its own citations: the copied
// entries keep them as recorded, and the rule's declared unknown claim is
// derived.
func staleAndConflicting(p *researchPlan) {
	if p.example == "gtm-company-person" {
		company, person := p.source("company").answers, p.source("person").answers
		company["employee-count"] = unfound("stale")
		company["revenue"] = conflicting(observed("18250000.00", 0), observed("18300000.00", 0))
		person["experience-years"] = unfound("stale", 1)
		person["contactable"] = conflicting(observed(true, 0), observed(false, 2))
		return
	}
	item, followUp := p.source("item").answers, p.source("follow-up").answers
	item["points"] = unfound("stale", 1)
	item["blocked"] = conflicting(observed(true, 0), observed(false, 2))
	followUp["estimate"] = conflicting(observed("1.0", 0), observed("1.00", 0))
	followUp["labels"] = unfound("stale")
}

// fallback sets a failed-fallback plan's answers: all-unavailable, the
// primary not-found, the secondary stale and the third conflicting, so that
// the reconciliation derives unknown with no resolved target; or
// secondary-found, the primary not-found and the secondary's value taken.
func fallback(p *researchPlan, variant string) {
	field := "employee-count"
	if p.example == "work-item-triage" {
		field = "points"
	}
	primary, secondary, reconciled := p.sources[0].answers, p.sources[1].answers, p.sources[2].answers
	switch variant {
	case "all-unavailable":
		primary[field] = unfound("not-found", 0)
		secondary[field] = unfound("stale", 0)
		reconciled[field] = conflicting(observed(json.Number("7"), 0), observed(json.Number("9"), 0))
	case "secondary-found":
		primary[field] = unfound("not-found", 0)
		secondary[field] = found(json.Number("240"), 0)
		reconciled[field] = unfound("not-found")
	default:
		panic("no fallback variant " + variant)
	}
}

// secondInstallation rebinds a valid-types plan as a second installation
// does: one assignment, and its trusted profile, changed. gtm-company-person's
// person reads its page through the profile reader, at another endpoint of
// the same gateway; work-item-triage's follow-up asks its answer through the
// AI profile assistant-2. Every response through the changed assignment is
// asked through the new profile, and its receipt names it.
func secondInstallation(h *researchStandIn, p *researchPlan) {
	if p.example == "gtm-company-person" {
		person := p.source("person")
		person.web["page"] = "reader"
		person.asks[2].profile = "reader"
		p.profiles = []InputProfile{h.aiProfile("assistant", "/answers"), h.searchProfile("search"), h.pageProfile("reader", "/reader")}
		return
	}
	followUp := p.source("follow-up")
	followUp.ai = "assistant-2"
	followUp.asks[0].profile = "assistant-2"
	p.profiles = []InputProfile{h.aiProfile("assistant", "/answers"), h.pageProfile("page", "/page"), h.aiProfile("assistant-2", "/answers-2")}
}
