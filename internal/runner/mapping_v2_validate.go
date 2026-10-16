package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
)

var mappingName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
var textParameter = regexp.MustCompile(`\{\{([a-zA-Z][a-zA-Z0-9_-]{0,63})\}\}`)

func strictJSON(raw []byte, dest any) error {
	if _, err := readInputJSONDepth(raw, 64); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(dest); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("one JSON value required")
	}
	if foreignReadSources(reflect.ValueOf(dest)) {
		return errors.New(`json: unknown field "sources"`)
	}
	return nil
}

// foreignReadSources says whether a value strictJSON decoded holds a source
// of a kind other than research whose read names sources. A read's sources
// are a research source's reconciliation (spec/dialect.md §6.4), and the
// read of a source of another kind has no such member: one that names it is
// refused at decoding, with the decoder's own sentence for a member it does
// not know, as it always was. Every other refusal of the decoder, and its
// sentence, is as it was.
func foreignReadSources(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && foreignReadSources(v.Elem())
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(MappingSource{}) {
			return v.FieldByName("Kind").String() != "research" && v.FieldByName("Read").FieldByName("Sources").Len() > 0
		}
		for i := 0; i < v.NumField(); i++ {
			if foreignReadSources(v.Field(i)) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if foreignReadSources(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if foreignReadSources(it.Value()) {
				return true
			}
		}
	}
	return false
}
func proofCanon(raw []byte) ([]byte, error) {
	if len(raw) > MaxBody {
		return nil, errors.New("value exceeds input limit")
	}
	if _, err := readInputJSON(raw); err != nil {
		return nil, err
	}
	return derivation.Canon(raw)
}
func profileHash(p InputProfile) string   { b, _ := proofCanon(encode(p)); return digest(b) }
func mappingV2Hash(m InputMapping) string { b, _ := proofCanon(encode(m)); return digest(b) }
func classValid(c string) bool            { return c == "asserted" || c == "record" || c == "generated" }
func paramValue(v any, typ string) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, e := proofCanon([]byte(n))
		return e == nil
	case "timestamp":
		s, ok := v.(string)
		if !ok || len(s) != 20 {
			return false
		}
		t, e := time.Parse("2006-01-02T15:04:05Z", s)
		return e == nil && t.Format("2006-01-02T15:04:05Z") == s
	}
	return false
}
func instantiate(v any, params map[string]any, types map[string]string, validate bool) (any, error) {
	switch o := v.(type) {
	case map[string]any:
		if p, ok := o["$param"]; ok {
			n, ok := p.(string)
			if !ok || len(o) != 1 || types[n] == "" || n == "runAt" {
				return nil, errors.New("invalid template parameter; runAt is derivation-only")
			}
			return params[n], nil
		}
		if p, ok := o["$text"]; ok {
			s, ok := p.(string)
			if !ok || len(o) != 1 {
				return nil, errors.New("invalid text template")
			}
			var err error
			out := textParameter.ReplaceAllStringFunc(s, func(token string) string {
				n := textParameter.FindStringSubmatch(token)[1]
				if types[n] != "integer" {
					err = errors.New("text interpolation requires an integer parameter")
					return ""
				}
				if validate {
					return "0"
				}
				b, e := proofCanon(encode(params[n]))
				if e != nil {
					err = e
				}
				return string(b)
			})
			if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
				err = errors.New("invalid text template placeholder")
			}
			return out, err
		}
		result := map[string]any{}
		for k, x := range o {
			if strings.HasPrefix(k, "$") {
				return nil, errors.New("reserved template member")
			}
			n, e := instantiate(x, params, types, validate)
			if e != nil {
				return nil, e
			}
			result[k] = n
		}
		return result, nil
	case []any:
		a := make([]any, len(o))
		for i, x := range o {
			n, e := instantiate(x, params, types, validate)
			if e != nil {
				return nil, e
			}
			a[i] = n
		}
		return a, nil
	}
	return v, nil
}
func sortedKeys[V any](m map[string]V) []string {
	a := make([]string, 0, len(m))
	for k := range m {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}
func readTargets(r SourceRead) ([]FactMapping, []EvidenceMapping, error) {
	if r.Copy != nil && len(r.Rule) > 0 || r.Copy == nil && len(r.Rule) == 0 {
		return nil, nil, errors.New("choose exactly one copy or rule")
	}
	if len(r.Unwrap) > 4 {
		return nil, nil, withCode(codeOverBound, errors.New("at most four unwrap steps"))
	}
	for _, p := range r.Unwrap {
		if _, e := pointerTokens(p, true); e != nil {
			return nil, nil, e
		}
	}
	if r.Copy != nil {
		return r.Copy.Facts, r.Copy.Evidence, nil
	}
	if err := derivation.Validate(r.Rule); err != nil {
		return nil, nil, err
	}
	var doc ruleDocument
	if e := strictJSON(r.Rule, &doc); e != nil {
		return nil, nil, e
	}
	if len(doc.Clauses) > 64 {
		return nil, nil, withCode(codeOverBound, errors.New("rule requires parameters and at most 64 clauses"))
	}
	if doc.Parameters == nil {
		return nil, nil, errors.New("rule requires parameters and at most 64 clauses")
	}
	facts := map[string]string{}
	evidence := map[string]bool{}
	for _, cl := range doc.Clauses {
		if err := validateConditionV2(cl.When, doc.Parameters); err != nil {
			return nil, nil, err
		}
		f := []FactMapping{}
		ev := []EvidenceMapping{}
		for _, x := range cl.Claim.Facts {
			f = append(f, FactMapping{x.Pointer, x.From})
			facts[x.Pointer] = x.From
		}
		for k := range cl.Claim.Evidence {
			ev = append(ev, EvidenceMapping{k, ""})
			evidence[k] = true
		}
		if err := (InputMapping{Version: 1, Provider: "local-file", Facts: f, Evidence: ev}).validate(); err != nil {
			return nil, nil, err
		}
	}
	f := []FactMapping{}
	ev := []EvidenceMapping{}
	for _, k := range sortedKeys(facts) {
		f = append(f, FactMapping{k, facts[k]})
	}
	for _, k := range sortedKeys(evidence) {
		ev = append(ev, EvidenceMapping{k, ""})
	}
	return f, ev, nil
}
func validateConditionV2(raw []byte, params map[string]string) error {
	var o map[string]json.RawMessage
	if e := strictJSON(raw, &o); e != nil {
		return e
	}
	var op string
	_ = json.Unmarshal(o["op"], &op)
	allowed := map[string][]string{"always": {"op"}, "exists": {"op", "field"}, "isTrue": {"op", "field"}, "isDecimalString": {"op", "field"}, "equals": {"op", "field", "to"}, "equalsParam": {"op", "field", "param"}, "freshWithin": {"op", "field", "asOf", "maxAge"}, "not": {"op", "of"}, "all": {"op", "of"}, "any": {"op", "of"}}[op]
	if len(o) != len(allowed) || allowed == nil {
		return errors.New("invalid condition fields")
	}
	for _, k := range allowed {
		if _, ok := o[k]; !ok {
			return errors.New("missing condition field")
		}
	}
	if p, ok := o["field"]; ok {
		var s string
		if json.Unmarshal(p, &s) != nil {
			return errors.New("invalid field")
		}
		if _, e := pointerTokens(s, true); e != nil {
			return e
		}
	}
	for k, typ := range map[string]string{"param": "", "asOf": "timestamp", "maxAge": "integer"} {
		if p, ok := o[k]; ok {
			var n string
			_ = json.Unmarshal(p, &n)
			if params[n] == "" || typ != "" && params[n] != typ {
				return errors.New("condition parameter is undeclared or mistyped")
			}
		}
	}
	switch op {
	case "not":
		return validateConditionV2(o["of"], params)
	case "all", "any":
		var a []json.RawMessage
		if json.Unmarshal(o["of"], &a) != nil {
			return errors.New("invalid condition list")
		}
		if len(a) > 64 {
			return withCode(codeOverBound, errors.New("invalid condition list"))
		}
		for _, x := range a {
			if e := validateConditionV2(x, params); e != nil {
				return e
			}
		}
	}
	return nil
}
func validateMappingV2(m InputMapping) error { return validateDialect(m, false) }

// validateDialect holds a dialect document of version 2 or 3 to its form.
// With open, an unbound operation of version 3 is read as the import step
// reads a published dialect (dialect_v3.go); without it, as every dialect
// Runner prepares is read, it is refused by name.
func validateDialect(m InputMapping, open bool) error {
	if e := m.validateArtifactIdentity(); e != nil {
		return e
	}
	prefix := "Mapping v2: "
	if m.Version == 3 {
		prefix = "Mapping v3: "
	}
	fail := func(s string) error { return inputProblem(prefix + s) }
	// failOn is fail with e's sentence, keeping e and the code it names.
	failOn := func(e error) error { return inputProblemOf(prefix+e.Error(), e) }
	if len(encode(m)) > 128<<10 {
		return codedProblem(codeOverBound, prefix+"mapping exceeds 128 KiB")
	}
	if _, e := proofCanon(encode(m)); e != nil {
		return fail(e.Error())
	}
	if m.dialect() && len(m.Sources) > 16 {
		return codedProblem(codeOverBound, prefix+"use case and/or at most 16 named sources")
	}
	if !m.dialect() || m.Provider != "" || len(m.Facts) > 0 || len(m.Evidence) > 0 || m.Case == nil && len(m.Sources) == 0 {
		return fail("use case and/or at most 16 named sources")
	}
	allFacts := []FactMapping{}
	allEvidence := []EvidenceMapping{}
	types := map[string]string{"runAt": "timestamp"}
	prior := map[string]bool{}
	// Each earlier research source's recipe, by its name, which a
	// reconciliation read names (spec/dialect.md §6.4).
	recipes := map[string]*researchRecipe{}
	// Each earlier source's fact targets, by its name: a source parameter's
	// pointer is exactly one of them (spec/dialect.md §3).
	factTargets := map[string]map[string]bool{}
	checkParams := func(ps map[string]Parameter, caseOnly bool, local map[string]string) error {
		if len(ps) > 64 {
			return withCode(codeOverBound, errors.New("too many parameters"))
		}
		for n, p := range ps {
			if !mappingName.MatchString(n) || types[n] != "" || local[n] != "" {
				return errors.New("parameter name is invalid, reserved or shadowed")
			}
			if p.Type != "string" && p.Type != "integer" && p.Type != "timestamp" {
				return errors.New("unsupported parameter type")
			}
			if _, e := pointerTokens(p.Pointer, true); e != nil {
				return e
			}
			if caseOnly && p.From != "" || !caseOnly && !prior[p.From] {
				return errors.New("dependencies must name an earlier source's derived facts")
			}
			if !caseOnly && !factTargets[p.From][p.Pointer] {
				return fmt.Errorf("source parameter %s reads no fact target of %s: pointer %q is not exactly a fact target the source declares", n, p.From, p.Pointer)
			}
			local[n] = p.Type
		}
		return nil
	}
	if m.Case != nil {
		extra := map[string]string{}
		if e := checkParams(m.Case.Parameters, true, extra); e != nil {
			return failOn(e)
		}
		for n, t := range extra {
			types[n] = t
		}
		if m.Case.Facts == nil || m.Case.Evidence == nil {
			return fail("case requires facts and evidence arrays")
		}
		allFacts = append(allFacts, m.Case.Facts...)
		allEvidence = append(allEvidence, m.Case.Evidence...)
	}
	for _, s := range m.Sources {
		if !mappingName.MatchString(s.Name) || s.Name == "case" || prior[s.Name] {
			return fail("invalid or duplicate source name")
		}
		if e := sourceForm(m.Version, s, open); e != nil {
			return failOn(e)
		}
		if len(s.Read.Sources) > 0 && !(m.Version == 3 && isResearch(s)) {
			return fail(researchFail(codeMalformed, "source %s: a read names sources only as a research source's reconciliation of earlier research sources, in dialect version 3", s.Name).Error())
		}
		research := m.Version == 3 && isResearch(s)
		verified := s.Kind == "operation" || s.Kind == "selected-file" && s.Provider == "google-drive"
		if !research && (s.Kind != "operation" && s.Kind != "selected-file" || s.Kind == "operation" && s.Provider != "" || s.Kind == "selected-file" && s.Provider != "google-drive" && s.Provider != "local-file") {
			return fail("unsupported source kind/provider")
		}
		// A research source's form, its recipe's and its assignments', is
		// readResearchRecipe's, once its parameters are read (below).
		switch {
		case research:
		case verified && unboundOperation(s):
			// Its form, and that it names no profile, are sourceForm's.
			if s.MaxAge < 1 || s.MaxAge > 86400 || len(s.Arguments) == 0 {
				return fail("an unbound operation requires arguments and maxAge 1..86400")
			}
		case verified:
			if s.Profile == "" || !validDigest(s.ProfileDigest) || s.MaxAge < 1 || s.MaxAge > 86400 || len(s.Arguments) == 0 {
				return fail("verified sources require a pinned profile, arguments and maxAge 1..86400")
			}
		case s.Profile != "" || s.ProfileDigest != "" || s.MaxAge != 0 || len(s.Arguments) > 0 || len(s.Parameters) > 0:
			return fail("local files cannot declare verification metadata or dependencies")
		}
		local := map[string]string{}
		if e := checkParams(s.Parameters, false, local); e != nil {
			return failOn(e)
		}
		for n, t := range types {
			local[n] = t
		}
		var recipe *researchRecipe
		if research {
			r, e := readResearchRecipe(s, local, open)
			if e != nil {
				return failOn(e)
			}
			recipe = r
		}
		if verified {
			v, e := readInputJSON(s.Arguments)
			if e != nil {
				return fail(e.Error())
			}
			// An operation's request is held to the form of its source's
			// shape where its profile is known (mcpToolCall); here, to the
			// template rules every request is held to.
			if _, ok := v.(map[string]any); !ok {
				return fail("arguments must be an object")
			}
			if _, e = instantiate(v, nil, local, true); e != nil {
				return fail(e.Error())
			}
		}
		if s.Calculation != nil {
			if e := validateCalculationBinding(*s.Calculation, s.Kind, local); e != nil {
				return failOn(e)
			}
		}
		f, ev, e := readTargets(s.Read)
		if e != nil {
			return failOn(e)
		}
		factTargets[s.Name] = map[string]bool{}
		for _, x := range f {
			factTargets[s.Name][x.Target] = true
		}
		if recipe != nil {
			if e := researchReadForm(s, recipe, recipes); e != nil {
				return failOn(e)
			}
			recipes[s.Name] = recipe
		}
		if s.Read.Copy != nil && (f == nil || ev == nil) {
			return fail("copy requires facts and evidence arrays")
		}
		if len(s.Read.Rule) > 0 {
			var d ruleDocument
			_ = json.Unmarshal(s.Read.Rule, &d)
			for n, t := range d.Parameters {
				if local[n] != t {
					return fail("rule parameter not supplied by this source")
				}
			}
		}
		allFacts = append(allFacts, f...)
		allEvidence = append(allEvidence, ev...)
		prior[s.Name] = true
	}
	if e := (InputMapping{Version: 1, Provider: "local-file", Facts: allFacts, Evidence: allEvidence}).validate(); e != nil {
		return e
	}
	targets := map[string]bool{}
	evs := map[string]bool{}
	for _, f := range allFacts {
		targets[f.Target] = true
	}
	for _, e := range allEvidence {
		evs[e.Requirement] = true
	}
	if m.Admits != nil {
		for kind, ad := range map[string]map[string][]string{"facts": m.Admits.Facts, "evidence": m.Admits.Evidence} {
			for target, classes := range ad {
				if kind == "facts" && !targets[target] || kind == "evidence" && !evs[target] || len(classes) == 0 || len(classes) > 3 {
					return fail("admission must name a mapped target and 1..3 classes")
				}
				seen := map[string]bool{}
				for _, c := range classes {
					if !classValid(c) || seen[c] {
						return fail("invalid or duplicate admitted class")
					}
					seen[c] = true
				}
			}
		}
	}
	seen := map[string]bool{}
	if len(m.Unmapped) > 256 || len(m.UnmappedEvidence) > 128 {
		return fail("too many omissions")
	}
	for _, p := range m.Unmapped {
		if _, e := pointerTokens(p, true); e != nil {
			return e
		}
		if seen[p] {
			return fail("duplicate omitted pointer")
		}
		seen[p] = true
		for t := range targets {
			if pointerCovers(t, p) || pointerCovers(p, t) {
				return fail("mapped and intentionally unmapped pointers overlap")
			}
		}
	}
	seen = map[string]bool{}
	for _, p := range m.UnmappedEvidence {
		if p == "" || len(p) > 256 || evs[p] || seen[p] {
			return fail("invalid evidence omission")
		}
		seen[p] = true
	}
	return nil
}
func pointerCovers(parent, child string) bool {
	return parent == "" || parent == child || strings.HasPrefix(child, parent+"/")
}
func admitted(m InputMapping, kind, target, class string, generated bool) bool {
	classes := []string{"asserted", "record"}
	if m.Admits != nil {
		a := m.Admits.Facts
		if kind == "evidence" {
			a = m.Admits.Evidence
		}
		if c, ok := a[target]; ok {
			classes = c
		}
	}
	has := func(c string) bool {
		for _, x := range classes {
			if x == c {
				return true
			}
		}
		return false
	}
	return has(class) && (!generated || has("generated"))
}

// mcpToolCall holds the request template of an operation whose profile is
// of shape mcp to the form of a tool call: exactly a fixed, literal tool and
// an object of arguments, the tool one the profile allows. DBHub's
// execute_sql takes its SQL as fixed text, or as text that interpolates
// integers only. The request of an operation of another shape is not a tool
// call, and is not held to this (adapterShapes).
func mcpToolCall(source MappingSource, profile InputProfile) error {
	fail := func(s string) error { return errors.New("Mapping v2: " + s) }
	v, e := readInputJSON(source.Arguments)
	if e != nil {
		return fail(e.Error())
	}
	request, _ := v.(map[string]any)
	tool, literal := request["tool"].(string)
	arguments, object := request["arguments"].(map[string]any)
	if !literal || tool == "" || !object || len(request) != 2 {
		return fail("MCP tool identity must be a fixed literal with an arguments object")
	}
	if tool == "execute_sql" {
		switch sql := arguments["sql"].(type) {
		case string:
		case map[string]any:
			if _, ok := sql["$text"].(string); !ok || len(sql) != 1 {
				return fail("execute_sql requires fixed SQL text or integer-only text interpolation; use bind arguments for strings")
			}
		default:
			return fail("execute_sql requires fixed SQL text")
		}
	}
	for _, allowed := range profile.Tools {
		if allowed == tool {
			return nil
		}
	}
	return fmt.Errorf("source %s: tool is not allowed by its trusted profile", source.Name)
}

func sourceFailure(name string, err error) error {
	return causedError{bad("input_preparation_failed", fmt.Sprintf("Source %s: %s. No decision was requested.", name, err)).(*apiError), err}
}

// Bound arbitrary-precision comparison work before the reference interpreter.
// This admission limit is Runner-specific; it does not change the rule contract.
func boundedArtifact(v any) error {
	switch x := v.(type) {
	case json.Number:
		text := string(x)
		if len(text) > 128 {
			return errors.New("numeric token exceeds 128 characters")
		}
		if at := strings.IndexAny(text, "eE"); at >= 0 {
			e, err := strconv.Atoi(text[at+1:])
			if err != nil || e > 308 || e < -308 {
				return errors.New("numeric exponent exceeds mapping profile limits")
			}
		}
	case map[string]any:
		for _, child := range x {
			if e := boundedArtifact(child); e != nil {
				return e
			}
		}
	case []any:
		for _, child := range x {
			if e := boundedArtifact(child); e != nil {
				return e
			}
		}
	}
	return nil
}
