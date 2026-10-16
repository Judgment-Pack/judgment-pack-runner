package runner

// Research sources (spec/dialect.md §4.3, §4.4, §5.1, §5.2, §6.4, §9 and
// §11; runner #88). A research source of dialect version 3 declares a
// recipe, a question, its constraints, the model it asks, the evidence to
// retain, the fields it asks for and the capabilities it needs, answer and
// optionally search and page, and is bound to an installation's AI profile
// and web profiles of class generated. Runner prepares it from the research
// record its caller assembles, R: the responses the run retained, each held
// by its digest under the receipt the gateway signed for it through the
// profile it names, and the typed answers, each citing the responses it
// rests on. Runner checks every retained response as it checks an
// operation's acquisition, holds the answers to the fields' declarations,
// and derives the pack's targets through the read, a copy or a rule over R,
// or a reconciliation of earlier research sources (§6.4).
//
// The recipe is recorded, under the dialect's digest, and never executed:
// Runner asks no model and reads no page, and neither does verify-run. What
// a preparation establishes of a research source is the least an
// acquisition of class generated establishes (§13): that each receipt holds
// its request and its bytes under its trusted profile, and that the model
// and capabilities the source declares match the profiles bound, not that
// the model answered or that an answer is right.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The research maxima of §12, strings counted in UTF-8 bytes after JSON
// decoding, but decimals in characters.
const (
	maxResearchFields       = 64
	maxResearchNameBytes    = 64
	maxResearchDescription  = 2048
	maxResearchModel        = 256
	maxResearchEvidence     = 4096
	maxResearchText         = 8192
	maxResearchListItems    = 64
	maxResearchItemBytes    = 1024
	maxResearchDecimal      = 128
	maxResearchObservations = 64
	maxResearchCites        = 64
	maxResearchCapabilities = 3
	maxResearchWeb          = 2
	maxReconciledSources    = 15
	maxRetainedResponses    = 64
)

// The codes the specification gives each refusal and finding (§11), which a
// research refusal names at its end.
const (
	codeMalformed  = "input-malformed"
	codeOverBound  = "input-over-bound"
	codeMismatch   = "profile-mismatch"
	codeMissing    = "acquisition-missing"
	codeUnexpected = "acquisition-unexpected"
	codeInvalid    = "acquisition-invalid"
	codeStale      = "acquisition-stale"
	codeSessions   = "acquisition-sessions"
	codeReadFailed = "read-failed"
	codeAdmission  = "admission-refused"
)

// classGenerated is the class of a research source, and of every profile
// it is bound to (spec/dialect.md §4.3, §5.1).
const classGenerated = "generated"

// researchCapabilities is the closed set of capabilities, in its order.
var researchCapabilities = []string{"answer", "search", "page"}

// webCapabilities are the capabilities a web profile serves.
var webCapabilities = []string{"search", "page"}

var researchTypes = map[string]bool{"integer": true, "decimal": true, "boolean": true, "text": true, "text-list": true, "range": true}

// decimalForm is Core §2.2's decimal grammar, matching the whole string.
var decimalForm = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// indexForm is a token that names an array's element (§1).
var indexForm = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// researchError is a research refusal: a sentence and the code the
// specification gives it, which it names at its end.
type researchError struct{ code, text string }

func (e researchError) Error() string { return e.text + " (" + e.code + ")" }

func researchFail(code, format string, a ...any) error {
	return researchError{code, fmt.Sprintf(format, a...)}
}

// profileRef names an input profile by its id and its digest.
type profileRef struct{ ID, Digest string }

// researchField is one requested field (§4.3).
type researchField struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// researchRecipe is a research source's recipe as read, and, bound, its
// assignments: the AI profile for answer, and a web profile for each web
// capability it asks for.
type researchRecipe struct {
	fields       []researchField
	types        map[string]string
	capabilities []string
	model        string
	bound        bool
	ai           profileRef
	web          map[string]profileRef
}

func isResearch(s MappingSource) bool { return s.Kind == "research" }

// unboundResearch says whether a research source names none of its
// binding members: it is then published unbound, and bound at import.
func unboundResearch(s MappingSource) bool {
	return isResearch(s) && s.Profile == "" && s.ProfileDigest == "" && len(s.WebProfiles) == 0
}

// utf8Len is a string's length in UTF-8 bytes; Go's strings are its bytes.
func utf8Len(s string) int { return len(s) }

// rawValue reads a member kept as written, which may be absent.
func rawValue(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	v, e := readInputJSONDepth(raw, 64)
	if e != nil {
		return nil, false
	}
	return v, true
}

// researchBounds holds a research source's recipe and its read to the
// maxima of §12, before its form: one over a maximum is input-over-bound.
func researchBounds(s MappingSource) error {
	over := func(format string, a ...any) error {
		return researchFail(codeOverBound, "source %s: "+format, append([]any{s.Name}, a...)...)
	}
	if fields, ok := rawValue(s.Fields); ok {
		if list, ok := fields.([]any); ok {
			if len(list) > maxResearchFields {
				return over("declares %d fields, and a research source declares at most %d", len(list), maxResearchFields)
			}
			for _, item := range list {
				d, _ := item.(map[string]any)
				if name, ok := d["name"].(string); ok && utf8Len(name) > maxResearchNameBytes {
					return over("declares a field whose name is longer than %d bytes", maxResearchNameBytes)
				}
				if text, ok := d["description"].(string); ok && utf8Len(text) > maxResearchDescription {
					return over("declares a field whose description is longer than %d bytes", maxResearchDescription)
				}
			}
		}
	}
	if model, ok := rawValue(s.Model); ok {
		if text, ok := model.(string); ok && utf8Len(text) > maxResearchModel {
			return over("names a model longer than %d bytes", maxResearchModel)
		}
	}
	if evidence, ok := rawValue(s.Evidence); ok {
		if list, ok := evidence.([]any); ok {
			for _, item := range list {
				if text, ok := item.(string); ok && utf8Len(text) > maxResearchEvidence {
					return over("gives an evidence instruction longer than %d bytes", maxResearchEvidence)
				}
			}
		}
	}
	if capabilities, ok := rawValue(s.Capabilities); ok {
		if list, ok := capabilities.([]any); ok && len(list) > maxResearchCapabilities {
			return over("asks for %d capabilities, and there are three", len(list))
		}
	}
	if web, ok := rawValue(s.WebProfiles); ok {
		if o, ok := web.(map[string]any); ok && len(o) > maxResearchWeb {
			return over("assigns %d web profiles, and a source has at most two", len(o))
		}
	}
	if names, ok := rawValue(s.Read.Sources); ok {
		if list, ok := names.([]any); ok && len(list) > maxReconciledSources {
			return over("reconciles %d earlier sources, and a reconciliation reads at most %d", len(list), maxReconciledSources)
		}
	}
	return nil
}

// closedMembers holds an object to exactly the members required, and
// those optional.
func closedMembers(o map[string]any, required, optional []string) bool {
	allowed := map[string]bool{}
	for _, n := range required {
		if _, ok := o[n]; !ok {
			return false
		}
		allowed[n] = true
	}
	for _, n := range optional {
		allowed[n] = true
	}
	for n := range o {
		if !allowed[n] {
			return false
		}
	}
	return true
}

// checkCapabilities holds a list of capabilities to the closed set, each
// once, in the set's order, one at least.
func checkCapabilities(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := []string{}
	last := -1
	for _, item := range list {
		name, ok := item.(string)
		rank := -1
		for i, c := range researchCapabilities {
			if c == name {
				rank = i
			}
		}
		if !ok || rank <= last {
			return nil, false
		}
		last = rank
		out = append(out, name)
	}
	return out, true
}

func asks(capabilities []string, capability string) bool {
	for _, c := range capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// checkAssignment reads an assignment: exactly a profile's id, a non-empty
// string, and its digest.
func checkAssignment(v any) (profileRef, bool) {
	o, ok := v.(map[string]any)
	if !ok || !closedMembers(o, []string{"profile", "profileDigest"}, nil) {
		return profileRef{}, false
	}
	id, ok1 := o["profile"].(string)
	pinned, ok2 := o["profileDigest"].(string)
	if !ok1 || id == "" || !ok2 || !validDigest(pinned) {
		return profileRef{}, false
	}
	return profileRef{id, pinned}, true
}

// checkWebProfiles reads webProfiles: exactly the web capabilities the
// source asks for, each assigned a profile.
func checkWebProfiles(v any, capabilities []string) (map[string]profileRef, bool) {
	o, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	web := map[string]profileRef{}
	for _, c := range webCapabilities {
		_, given := o[c]
		if given != asks(capabilities, c) {
			return nil, false
		}
		if given {
			ref, ok := checkAssignment(o[c])
			if !ok {
				return nil, false
			}
			web[c] = ref
		}
	}
	if len(o) != len(web) {
		return nil, false
	}
	return web, true
}

// readResearchRecipe holds a research source to the form of §4.3 and §4.4:
// its recipe, its maximum age and, when bound, its three assignment members.
// An unbound source is read only with open, as the import step reads a
// published dialect; otherwise it is refused by name. scope is the source's
// parameters by their types, which its question is held to; nil reads a
// source whose form was held so before, and does not hold the question.
func readResearchRecipe(s MappingSource, scope map[string]string, open bool) (*researchRecipe, error) {
	if e := researchBounds(s); e != nil {
		return nil, e
	}
	malformed := func(format string, a ...any) (*researchRecipe, error) {
		return nil, researchFail(codeMalformed, "source %s: "+format, append([]any{s.Name}, a...)...)
	}
	if s.Provider != "" || len(s.Arguments) > 0 || s.Calculation != nil {
		return malformed("a research source names no provider, arguments or calculation: its request is its recipe")
	}
	for _, m := range []struct {
		name string
		raw  json.RawMessage
	}{{"question", s.Question}, {"constraints", s.Constraints}, {"model", s.Model}, {"evidence", s.Evidence}, {"fields", s.Fields}, {"capabilities", s.Capabilities}} {
		if len(m.raw) == 0 {
			return malformed("a research source names its question, constraints, model, evidence, fields and capabilities, and %s is missing", m.name)
		}
	}
	if s.MaxAge < 1 || s.MaxAge > 86400 {
		return malformed("maxAge is an integer from 1 to 86400")
	}
	question, _ := rawValue(s.Question)
	if scope != nil && !stringTemplate(question, scope) {
		return malformed("the question is a string template: a string, a $text whose placeholders name integer parameters, or a $param naming a parameter of type string or timestamp, never runAt")
	}
	constraints, _ := rawValue(s.Constraints)
	// That they are in the gateway canonical domain is the dialect's: a
	// dialect outside it is refused with its digest (validateDialect).
	if _, ok := constraints.(map[string]any); !ok {
		return malformed("constraints is an object")
	}
	r := &researchRecipe{types: map[string]string{}}
	model, _ := rawValue(s.Model)
	text, ok := model.(string)
	if !ok || text == "" {
		return malformed("the model is a non-empty string")
	}
	r.model = text
	evidence, _ := rawValue(s.Evidence)
	list, ok := evidence.([]any)
	if !ok {
		return malformed("evidence is an array of non-empty strings")
	}
	for _, item := range list {
		if text, ok := item.(string); !ok || text == "" {
			return malformed("evidence is an array of non-empty strings")
		}
	}
	fields, _ := rawValue(s.Fields)
	declared, ok := fields.([]any)
	if !ok || len(declared) == 0 {
		return malformed("fields is an array of one declaration at least")
	}
	for i, item := range declared {
		d, ok := item.(map[string]any)
		if !ok || !closedMembers(d, []string{"name", "type", "required", "description"}, nil) {
			return malformed("field %d is not an object of exactly name, type, required and description", i)
		}
		name, ok := d["name"].(string)
		if !ok || name == "" || name == "case" {
			return malformed("field %d's name is a non-empty string other than case", i)
		}
		if _, twice := r.types[name]; twice {
			return malformed("declares the field %s twice", name)
		}
		kind, ok := d["type"].(string)
		if !ok || !researchTypes[kind] {
			return malformed("the field %s is of a type that is not integer, decimal, boolean, text, text-list or range", name)
		}
		required, ok := d["required"].(bool)
		if !ok {
			return malformed("the field %s's required is not a boolean", name)
		}
		description, ok := d["description"].(string)
		if !ok || description == "" {
			return malformed("the field %s's description is not a non-empty string", name)
		}
		r.types[name] = kind
		r.fields = append(r.fields, researchField{name, kind, required, description})
	}
	capabilities, _ := rawValue(s.Capabilities)
	if r.capabilities, ok = checkCapabilities(capabilities); !ok || r.capabilities[0] != "answer" {
		return malformed("capabilities are distinct names of answer, search and page, in that order, answer among them")
	}
	given := 0
	for _, member := range []bool{s.Profile != "", s.ProfileDigest != "", len(s.WebProfiles) > 0} {
		if member {
			given++
		}
	}
	if given == 0 && !open {
		return nil, fmt.Errorf("source %s is unbound: a research source is published with no AI or web profile, and an installation binds it to its own. Import the dialect with a binding document (source.import), which assigns the source its AI profile and a web profile for each web capability it asks for, before it is prepared", s.Name)
	}
	if given == 0 {
		return r, nil
	}
	if given != 3 {
		return malformed("a research source names profile, profileDigest and webProfiles, all three, or none of them")
	}
	if !validDigest(s.ProfileDigest) {
		return malformed("profileDigest is not a digest")
	}
	r.bound = true
	r.ai = profileRef{s.Profile, s.ProfileDigest}
	web, _ := rawValue(s.WebProfiles)
	if r.web, ok = checkWebProfiles(web, r.capabilities); !ok {
		return malformed("webProfiles assigns exactly the web capabilities the source asks for, each a profile's id and digest")
	}
	return r, nil
}

// stringTemplate says whether v is a string template (§4.1): a string, a
// $text whose placeholders name integer parameters of the scope, or a
// $param naming a parameter of the scope of type string or timestamp. No
// template names runAt.
func stringTemplate(v any, scope map[string]string) bool {
	switch o := v.(type) {
	case string:
		return true
	case map[string]any:
		if len(o) != 1 {
			return false
		}
		if n, ok := o["$param"].(string); ok {
			return n != "runAt" && (scope[n] == "string" || scope[n] == "timestamp")
		}
		if _, ok := o["$text"].(string); ok {
			_, e := instantiate(o, nil, scope, true)
			return e == nil
		}
	}
	return false
}

// researchReadForm holds a research source's read to §4.3's closed list of
// pointers, or, when it names sources, to §6.4's reconciliation. earlier is
// each earlier research source's recipe by its name. The rule's own form is
// readTargets'.
func researchReadForm(s MappingSource, r *researchRecipe, earlier map[string]*researchRecipe) error {
	malformed := func(format string, a ...any) error {
		return researchFail(codeMalformed, "source %s: "+format, append([]any{s.Name}, a...)...)
	}
	if len(s.Read.Sources) > 0 {
		return reconciliationForm(s, r, earlier)
	}
	// A plain research read: each pointer it reads in R is of the closed list,
	// and none it takes a value from is under values.
	readable := func(pointer string, taken bool) error {
		tokens, e := pointerTokens(pointer, true)
		if e != nil {
			return malformed("%s is not a pointer", pointer)
		}
		if len(tokens) == 1 && tokens[0] == "fields" {
			return nil
		}
		if len(tokens) >= 2 && tokens[0] == "fields" && r.types[tokens[1]] != "" {
			rest := tokens[2:]
			if len(rest) == 0 || rest[0] == "value" || len(rest) == 1 && (rest[0] == "status" || rest[0] == "cites") {
				return nil
			}
			if len(rest) == 3 && rest[0] == "values" && indexForm.MatchString(rest[1]) && rest[2] == "value" {
				if taken {
					return malformed("the read takes %s, a value from under values, which a plain research read never takes: a conflicting entry's observations are read by its conditions, and reconciled by an explicit rule", pointer)
				}
				return nil
			}
		}
		return malformed("the read reads %s, which is not among the pointers a research read reads: /fields, a declared field's entry, its status, its cites, its value and what is below it, and in a condition an observation's value", pointer)
	}
	for _, p := range s.Read.Unwrap {
		if e := readable(p, true); e != nil {
			return e
		}
	}
	if s.Read.Copy != nil {
		for _, f := range s.Read.Copy.Facts {
			if e := readable(f.Source, true); e != nil {
				return e
			}
		}
		for _, f := range s.Read.Copy.Evidence {
			if e := readable(f.Source, true); e != nil {
				return e
			}
		}
		return nil
	}
	rule, e := readInputJSON(s.Read.Rule)
	if e != nil {
		return malformed("the rule is not one JSON document")
	}
	for _, leaf := range ruleLeaves(rule) {
		field, _ := leaf["field"].(string)
		if e := readable(field, false); e != nil {
			return e
		}
	}
	for _, from := range ruleFroms(rule) {
		if e := readable(from, true); e != nil {
			return e
		}
	}
	return nil
}

// ruleConditions is every condition of every clause of a rule, at any
// depth, each clause's in order.
func ruleConditions(rule any) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(c any) {
		o, ok := c.(map[string]any)
		if !ok {
			return
		}
		out = append(out, o)
		switch o["op"] {
		case "all", "any":
			list, _ := o["of"].([]any)
			for _, x := range list {
				walk(x)
			}
		case "not":
			walk(o["of"])
		}
	}
	for _, clause := range ruleClauses(rule) {
		walk(clause["when"])
	}
	return out
}

func ruleClauses(rule any) []map[string]any {
	o, _ := rule.(map[string]any)
	list, _ := o["clauses"].([]any)
	out := []map[string]any{}
	for _, c := range list {
		if clause, ok := c.(map[string]any); ok {
			out = append(out, clause)
		}
	}
	return out
}

// ruleLeaves are the conditions of a rule that read a field.
func ruleLeaves(rule any) []map[string]any {
	out := []map[string]any{}
	for _, c := range ruleConditions(rule) {
		switch c["op"] {
		case "always", "all", "any", "not":
		default:
			out = append(out, c)
		}
	}
	return out
}

// claimFacts are the facts a clause's claim writes, as {pointer, from}.
func claimFacts(clause map[string]any) []map[string]any {
	claim, _ := clause["claim"].(map[string]any)
	list, _ := claim["facts"].([]any)
	out := []map[string]any{}
	for _, f := range list {
		if o, ok := f.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// ruleFroms is every from of every clause's claim.
func ruleFroms(rule any) []string {
	out := []string{}
	for _, clause := range ruleClauses(rule) {
		for _, f := range claimFacts(clause) {
			from, _ := f["from"].(string)
			out = append(out, from)
		}
	}
	return out
}

// statusGuard says whether a condition is exactly the guard of an entry's
// status (§6.4).
func statusGuard(c any, entry []string, status string) bool {
	o, ok := c.(map[string]any)
	if !ok || len(o) != 3 || o["op"] != "equals" || o["to"] != status {
		return false
	}
	field, _ := o["field"].(string)
	tokens, e := pointerTokens(field, true)
	return e == nil && sameTokens(tokens, append(append([]string{}, entry...), "status"))
}

// absenceGuard says whether a condition is exactly the guard of an entry's
// absence (§6.4).
func absenceGuard(c any, entry []string) bool {
	o, ok := c.(map[string]any)
	if !ok || len(o) != 2 || o["op"] != "not" {
		return false
	}
	inner, ok := o["of"].(map[string]any)
	if !ok || len(inner) != 2 || inner["op"] != "exists" {
		return false
	}
	field, _ := inner["field"].(string)
	tokens, e := pointerTokens(field, true)
	return e == nil && sameTokens(tokens, entry)
}

func sameTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reconciliationNames reads a reconciliation's earlier sources.
func reconciliationNames(raw json.RawMessage) ([]string, bool) {
	v, ok := rawValue(raw)
	list, isList := v.([]any)
	if !ok || !isList || len(list) == 0 {
		return nil, false
	}
	names := []string{}
	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			return nil, false
		}
		names = append(names, name)
	}
	return names, true
}

// reconciliationForm holds a reconciliation read to §6.4: exactly sources
// and rule, the sources distinct earlier research sources, the rule's
// pointers into {"current": C, "sources": U} of its closed list, each value
// it selects guarded as found by its clause and named, somewhere in the
// rule, for every other status and for its absence, no evidence written,
// and a last clause that writes no fact and resolves nothing.
func reconciliationForm(s MappingSource, r *researchRecipe, earlier map[string]*researchRecipe) error {
	malformed := func(format string, a ...any) error {
		return researchFail(codeMalformed, "source %s: "+format, append([]any{s.Name}, a...)...)
	}
	if s.Read.Copy != nil || len(s.Read.Unwrap) > 0 || len(s.Read.Rule) == 0 {
		return malformed("a reconciliation read is exactly sources and a rule, with no copy and no unwrap")
	}
	names, ok := reconciliationNames(s.Read.Sources)
	if !ok {
		return malformed("a reconciliation's sources are the names of one earlier research source at least")
	}
	listed := map[string]map[string]string{}
	for _, name := range names {
		recipe, ok := earlier[name]
		if !ok {
			return malformed("a reconciliation names %s, which is not an earlier research source of the dialect", name)
		}
		if _, twice := listed[name]; twice {
			return malformed("a reconciliation names %s twice", name)
		}
		listed[name] = recipe.types
	}
	rule, e := readInputJSON(s.Read.Rule)
	if e != nil {
		return malformed("the rule is not one JSON document")
	}
	// entryOf splits a pointer into the field entry it names, and what
	// follows it.
	entryOf := func(pointer string) ([]string, []string, bool) {
		tokens, e := pointerTokens(pointer, true)
		if e != nil {
			return nil, nil, false
		}
		if len(tokens) >= 3 && tokens[0] == "current" && tokens[1] == "fields" && r.types[tokens[2]] != "" {
			return tokens[:3], tokens[3:], true
		}
		if len(tokens) >= 4 && tokens[0] == "sources" && listed[tokens[1]] != nil && tokens[2] == "fields" && listed[tokens[1]][tokens[3]] != "" {
			return tokens[:4], tokens[4:], true
		}
		return nil, nil, false
	}
	for _, leaf := range ruleLeaves(rule) {
		field, _ := leaf["field"].(string)
		_, rest, ok := entryOf(field)
		if !ok || len(rest) > 0 && !(len(rest) == 1 && rest[0] == "status") && rest[0] != "value" && rest[0] != "values" {
			return malformed("a condition reads %s, which is not a declared field's entry, its status, or its value or values, of this source or of a source the reconciliation names", field)
		}
	}
	var selected [][]string
	clauses := ruleClauses(rule)
	for _, clause := range clauses {
		claim, _ := clause["claim"].(map[string]any)
		if evidence, _ := claim["evidence"].(map[string]any); len(evidence) > 0 {
			return malformed("a reconciliation's claim writes evidence, and in this version it writes fact targets only")
		}
		for _, f := range claimFacts(clause) {
			from, _ := f["from"].(string)
			entry, rest, ok := entryOf(from)
			if !ok || len(rest) == 0 || rest[0] != "value" {
				return malformed("a reconciliation takes %s, and takes only a declared field's value, or what is below it", from)
			}
			when, _ := clause["when"].(map[string]any)
			guarded := statusGuard(when, entry, "found")
			if !guarded && when["op"] == "all" {
				list, _ := when["of"].([]any)
				for _, c := range list {
					guarded = guarded || statusGuard(c, entry, "found")
				}
			}
			if !guarded {
				return malformed("a clause takes %s, and its condition does not guard the entry as found: the guard, or an all with the guard as a direct member", from)
			}
			known := false
			for _, s := range selected {
				known = known || sameTokens(s, entry)
			}
			if !known {
				selected = append(selected, entry)
			}
		}
	}
	every := ruleConditions(rule)
	for _, entry := range selected {
		for _, status := range []string{"not-found", "stale", "conflicting"} {
			found := false
			for _, c := range every {
				found = found || statusGuard(c, entry, status)
			}
			if !found {
				return malformed("the rule selects /%s, and names nowhere what it does when the entry is %s", strings.Join(escapeTokens(entry), "/"), status)
			}
		}
		found := false
		for _, c := range every {
			found = found || absenceGuard(c, entry)
		}
		if !found {
			return malformed("the rule selects /%s, and names nowhere what it does when the entry is absent", strings.Join(escapeTokens(entry), "/"))
		}
	}
	if len(clauses) > 0 {
		last := clauses[len(clauses)-1]
		claim, _ := last["claim"].(map[string]any)
		if len(claimFacts(last)) > 0 || claim["acquisitionStatus"] != "unknown" && claim["acquisitionStatus"] != "absent" {
			return malformed("a reconciliation's last clause writes no fact, and its status is unknown or absent")
		}
	}
	return nil
}

func escapeTokens(tokens []string) []string {
	out := []string{}
	for _, t := range tokens {
		out = append(out, strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1"))
	}
	return out
}

// researchDeclarationForm holds a profile's research declaration to §5.1:
// one to three distinct capabilities of answer, search and page, in that
// order, and a model, a non-empty string of at most 256 bytes, exactly
// when they include answer. A profile without one encodes as before.
func researchDeclarationForm(p InputProfile) error {
	r := p.Research
	if r == nil {
		return nil
	}
	if len(r.Capabilities) > maxResearchCapabilities {
		return researchFail(codeOverBound, "the input profile %s declares %d research capabilities, and there are three", p.ID, len(r.Capabilities))
	}
	if r.Model != nil && utf8Len(*r.Model) > maxResearchModel {
		return researchFail(codeOverBound, "the input profile %s declares a model longer than %d bytes", p.ID, maxResearchModel)
	}
	names := []any{}
	for _, c := range r.Capabilities {
		names = append(names, c)
	}
	if _, ok := checkCapabilities(names); !ok {
		return researchFail(codeMalformed, "the input profile %s declares research capabilities that are not distinct names of answer, search and page, in that order, one at least", p.ID)
	}
	if asks(r.Capabilities, "answer") != (r.Model != nil) || r.Model != nil && *r.Model == "" {
		return researchFail(codeMalformed, "the input profile %s declares a model exactly when it serves answer, and the model is a non-empty string", p.ID)
	}
	return nil
}

// bindResearch binds an unbound research source with its entry in a
// binding document (§4.4): exactly profile, profileDigest and webProfiles,
// the web profiles exactly the web capabilities the source asks for. The
// three are added to the source, and every other member is kept as
// written: a research source has no catalog names to remove. Each profile
// is then held to the installation's (researchProfiles).
func bindResearch(s MappingSource, entry map[string]json.RawMessage, profiles []InputProfile) (MappingSource, error) {
	malformed := func(format string, a ...any) (MappingSource, error) {
		return s, researchFail(codeMalformed, "the binding document "+format, a...)
	}
	if web, ok := rawValue(entry["webProfiles"]); ok {
		if o, ok := web.(map[string]any); ok && len(o) > maxResearchWeb {
			return s, researchFail(codeOverBound, "the binding document assigns the research source %s %d web profiles, and a source has at most two", s.Name, len(o))
		}
	}
	for _, member := range sortedKeys(entry) {
		if member != "profile" && member != "profileDigest" && member != "webProfiles" {
			return malformed("gives the research source %s the member %s: it binds a research source to its AI profile and its web profiles, and changes nothing else in the dialect", s.Name, member)
		}
	}
	// Each of the three is required: profile and profileDigest here, and
	// webProfiles, written even when empty, below.
	var id, pinned string
	if json.Unmarshal(entry["profile"], &id) != nil || id == "" || json.Unmarshal(entry["profileDigest"], &pinned) != nil || !validDigest(pinned) {
		return malformed("gives the research source %s no AI profile: an id and a digest", s.Name)
	}
	recipe, e := readResearchRecipe(s, nil, true)
	if e != nil {
		return s, e
	}
	web, _ := rawValue(entry["webProfiles"])
	assigned, ok := checkWebProfiles(web, recipe.capabilities)
	if !ok {
		asked := "none"
		if len(recipe.capabilities) > 1 {
			asked = strings.Join(recipe.capabilities[1:], " and ")
		}
		return malformed("assigns the research source %s web profiles other than exactly the web capabilities it asks for, %s, each a profile's id and digest", s.Name, asked)
	}
	s.Profile, s.ProfileDigest, s.WebProfiles = id, pinned, entry["webProfiles"]
	recipe.bound, recipe.ai, recipe.web = true, profileRef{id, pinned}, assigned
	if _, e = researchProfiles(s, recipe, profiles); e != nil {
		return s, e
	}
	return s, nil
}

// researchProfiles holds a bound research source's assignments to the
// trusted profiles (§4.4, §5.1): the AI profile, then the web profiles in
// search, page order. Each is the installation's profile of its id and
// digest, of class generated, declaring the capability it serves, and,
// when it declares a model, the source's; the AI profile declares the
// source's model. Anything else is profile-mismatch, by name.
func researchProfiles(s MappingSource, r *researchRecipe, profiles []InputProfile) (map[string]InputProfile, error) {
	mismatch := func(format string, a ...any) (map[string]InputProfile, error) {
		return nil, researchFail(codeMismatch, "source %s: "+format, append([]any{s.Name}, a...)...)
	}
	served := map[string]InputProfile{}
	for _, capability := range r.capabilities {
		ref := r.ai
		if capability != "answer" {
			ref = r.web[capability]
		}
		var profile *InputProfile
		for i := range profiles {
			if profiles[i].ID == ref.ID {
				profile = &profiles[i]
			}
		}
		switch {
		case profile == nil:
			return mismatch("it is bound for %s to the profile %s, which this installation does not hold", capability, ref.ID)
		case profileHash(*profile) != ref.Digest:
			return mismatch("it is bound for %s to the profile %s by a digest that is not the digest of this installation's profile %s", capability, ref.ID, ref.ID)
		case profile.Class != classGenerated:
			return mismatch("it is bound for %s to the profile %s, of class %s: a research source's profiles are of class generated", capability, ref.ID, profile.Class)
		case profile.Research == nil:
			return mismatch("it is bound for %s to the profile %s, which declares no research capabilities", capability, ref.ID)
		case !asks(profile.Research.Capabilities, capability):
			return mismatch("it asks for %s, and the profile %s it is bound to for %s does not declare it", capability, ref.ID, capability)
		case profile.Research.Model != nil && *profile.Research.Model != r.model:
			return mismatch("it asks the model %s, and the profile %s it is bound to for %s declares the model %s", r.model, ref.ID, capability, *profile.Research.Model)
		}
		served[capability] = *profile
	}
	return served, nil
}

// assignments is each profile a bound research source is assigned, the AI
// profile first, then the web profiles in search, page order.
func (r *researchRecipe) assignments() []profileRef {
	out := []profileRef{r.ai}
	for _, c := range webCapabilities {
		if ref, ok := r.web[c]; ok {
			out = append(out, ref)
		}
	}
	return out
}

// sourceProfileRefs is every profile a source names: an operation's or a
// Drive file's one, a bound research source's AI and web profiles.
func sourceProfileRefs(s MappingSource) []profileRef {
	if isResearch(s) {
		r, e := readResearchRecipe(s, nil, true)
		if e != nil || !r.bound {
			return nil
		}
		return r.assignments()
	}
	if s.Profile == "" {
		return nil
	}
	return []profileRef{{s.Profile, s.ProfileDigest}}
}

// researchRequest is the source's request (§4.3): exactly its recipe's
// members, the question instantiated.
func researchRequest(s MappingSource, params map[string]any, types map[string]string) (json.RawMessage, error) {
	question, _ := rawValue(s.Question)
	q, e := instantiate(question, params, types, false)
	if e != nil {
		return nil, e
	}
	request := map[string]any{"question": q, "constraints": s.Constraints, "model": s.Model, "evidence": s.Evidence, "fields": s.Fields, "capabilities": s.Capabilities}
	return proofCanon(encode(request))
}

// retainedResponse is one response a research record retains.
type retainedResponse struct {
	profile profileRef
	request json.RawMessage
	result  json.RawMessage
	receipt json.RawMessage
	salts   json.RawMessage
}

// researchAcquisition is a research source's acquisition as read: its
// research record, R, and the receipt and salts of the record's own, when
// it came back through a gateway source.
type researchAcquisition struct {
	raw       json.RawMessage
	record    any
	responses []retainedResponse
	receipt   json.RawMessage
	salts     json.RawMessage
}

// readResearchAcquisition holds a research source's acquisition to §5.2's
// form and its record to §4.3's: exactly result, and receipt and salts both
// or neither; the record exactly fields and responses, at most 64 retained
// responses, each exactly profile, request, receipt and salts, and its
// result; and the answers held to the source's declarations. The maxima
// of §12 come first, input-over-bound; then the forms, input-malformed. A
// retained response without its result is no refusal of form: preparation
// finds it acquisition-missing.
func readResearchAcquisition(value SourceValue, r *researchRecipe) (*researchAcquisition, error) {
	malformed := func(format string, a ...any) (*researchAcquisition, error) {
		return nil, researchFail(codeMalformed, format, a...)
	}
	if len(value.Snapshot) > 0 || len(value.Response) == 0 {
		return malformed("a research source's acquisition is its response, {result}, or {result, receipt, salts} when the record came back through a gateway source")
	}
	var response map[string]json.RawMessage
	if _, e := readInputJSONDepth(value.Response, 64); e != nil || json.Unmarshal(value.Response, &response) != nil || response == nil {
		return malformed("the response is not one JSON object")
	}
	for member := range response {
		if member != "result" && member != "receipt" && member != "salts" {
			return malformed("the response has the member %s: it is exactly result, and receipt and salts both or neither", member)
		}
	}
	raw, ok := response["result"]
	if !ok {
		return malformed("the response has no result, the research record")
	}
	_, hasReceipt := response["receipt"]
	_, hasSalts := response["salts"]
	if hasReceipt != hasSalts {
		return malformed("the record's receipt and salts are both present or both absent")
	}
	record, e := readInputJSON(raw)
	if e != nil {
		if strings.Contains(e.Error(), "nesting") {
			return nil, researchFail(codeOverBound, "the research record is nested more than 32 levels")
		}
		return malformed("the research record is not one JSON value")
	}
	if e := researchRecordBounds(record, r); e != nil {
		return nil, e
	}
	o, ok := record.(map[string]any)
	if !ok || !closedMembers(o, []string{"fields", "responses"}, nil) {
		return malformed("the research record is an object of exactly fields and responses")
	}
	list, ok := o["responses"].([]any)
	if !ok {
		return malformed("the research record's responses are an array")
	}
	var members struct {
		Responses []map[string]json.RawMessage `json:"responses"`
	}
	if json.Unmarshal(raw, &members) != nil || len(members.Responses) != len(list) {
		return malformed("the research record's responses are objects")
	}
	a := &researchAcquisition{raw: raw, record: record, receipt: response["receipt"], salts: response["salts"]}
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok || !closedMembers(entry, []string{"profile", "request", "receipt", "salts"}, []string{"result"}) {
			return malformed("retained response %d is not an object of exactly profile, request, result, receipt and salts", i)
		}
		ref, ok := entry["profile"].(map[string]any)
		if !ok || !closedMembers(ref, []string{"id", "digest"}, nil) {
			return malformed("retained response %d's profile is not exactly an id and a digest", i)
		}
		id, ok1 := ref["id"].(string)
		pinned, ok2 := ref["digest"].(string)
		if !ok1 || id == "" || !ok2 || !validDigest(pinned) {
			return malformed("retained response %d's profile is not exactly an id and a digest", i)
		}
		if _, ok := entry["request"].(map[string]any); !ok {
			return malformed("retained response %d's request is not an object", i)
		}
		if _, e := proofCanon(members.Responses[i]["request"]); e != nil {
			return malformed("retained response %d's request is outside the gateway canonical domain", i)
		}
		if _, ok := entry["receipt"].(map[string]any); !ok {
			return malformed("retained response %d's receipt is not an object", i)
		}
		if !saltsForm(entry["salts"]) {
			return malformed("retained response %d's salts are not exactly args and, optionally, statement, each 64 hexadecimal characters", i)
		}
		a.responses = append(a.responses, retainedResponse{profileRef{id, pinned}, members.Responses[i]["request"], members.Responses[i]["result"], members.Responses[i]["receipt"], members.Responses[i]["salts"]})
	}
	if hasReceipt {
		receipt, _ := readInputJSONDepth(a.receipt, 64)
		salts, _ := readInputJSONDepth(a.salts, 64)
		if _, ok := receipt.(map[string]any); !ok {
			return malformed("the record's receipt is not an object")
		}
		if !saltsForm(salts) {
			return malformed("the record's salts are not exactly args and, optionally, statement, each 64 hexadecimal characters")
		}
	}
	if e := checkAnswers(o["fields"], r, len(list)); e != nil {
		return nil, e
	}
	return a, nil
}

func saltsForm(v any) bool {
	o, ok := v.(map[string]any)
	if !ok || !closedMembers(o, []string{"args"}, []string{"statement"}) {
		return false
	}
	for _, salt := range o {
		s, ok := salt.(string)
		if !ok || !validHex(s, 32) {
			return false
		}
	}
	return true
}

// researchRecordBounds holds a research record to the maxima of §12, each
// value measured by its field's declared type: the retained responses, the
// answers, their names, their citations, and each text, list, decimal and
// observation.
func researchRecordBounds(record any, r *researchRecipe) error {
	over := func(format string, a ...any) error {
		return researchFail(codeOverBound, format, a...)
	}
	o, _ := record.(map[string]any)
	if list, ok := o["responses"].([]any); ok && len(list) > maxRetainedResponses {
		return over("the research record retains %d responses, and one retains at most %d", len(list), maxRetainedResponses)
	}
	answers, ok := o["fields"].(map[string]any)
	if !ok {
		return nil
	}
	if len(answers) > maxResearchFields {
		return over("the research record answers %d fields, and one answers at most %d", len(answers), maxResearchFields)
	}
	for _, field := range sortedKeys(answers) {
		if utf8Len(field) > maxResearchNameBytes {
			return over("the research record answers a field whose name is longer than %d bytes", maxResearchNameBytes)
		}
	}
	citesOver := func(v any) bool { list, ok := v.([]any); return ok && len(list) > maxResearchCites }
	for _, field := range sortedKeys(answers) {
		entry, ok := answers[field].(map[string]any)
		if !ok {
			continue
		}
		kind := r.types[field]
		if citesOver(entry["cites"]) || valueOver(entry["value"], kind) {
			return over("the answer for %s is over a maximum", field)
		}
		observations, ok := entry["values"].([]any)
		if !ok {
			continue
		}
		if len(observations) > maxResearchObservations {
			return over("the answer for %s records %d conflicting observations, and one records at most %d", field, len(observations), maxResearchObservations)
		}
		for _, item := range observations {
			if observation, ok := item.(map[string]any); ok && (citesOver(observation["cites"]) || valueOver(observation["value"], kind)) {
				return over("an observation for %s is over a maximum", field)
			}
		}
	}
	return nil
}

// valueOver measures a value by its declared type against §12's maxima.
func valueOver(v any, kind string) bool {
	switch kind {
	case "text":
		s, ok := v.(string)
		return ok && utf8Len(s) > maxResearchText
	case "text-list":
		list, ok := v.([]any)
		if !ok {
			return false
		}
		if len(list) > maxResearchListItems {
			return true
		}
		for _, item := range list {
			if s, ok := item.(string); ok && utf8Len(s) > maxResearchItemBytes {
				return true
			}
		}
	case "decimal":
		s, ok := v.(string)
		return ok && utf8.RuneCountInString(s) > maxResearchDecimal
	case "range":
		o, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for _, end := range []string{"min", "max"} {
			if s, ok := o[end].(string); ok && utf8.RuneCountInString(s) > maxResearchDecimal {
				return true
			}
		}
	}
	return false
}

// integerValue is an integer's value (§1), when v is one: a JSON number
// whose literal has no fraction or exponent, within ±(2^53-1). -0 is one.
func integerValue(v any) (*big.Int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, false
	}
	x, ok := new(big.Int).SetString(string(n), 10)
	limit := big.NewInt(1<<53 - 1)
	if !ok || x.CmpAbs(limit) > 0 {
		return nil, false
	}
	return x, true
}

// typedValue says whether v is a value of the declared type, in its exact
// encoding (§4.3); null is never one.
func typedValue(v any, kind string) bool {
	switch kind {
	case "integer":
		_, ok := integerValue(v)
		return ok
	case "decimal":
		s, ok := v.(string)
		return ok && decimalForm.MatchString(s)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "text":
		_, ok := v.(string)
		return ok
	case "text-list":
		list, ok := v.([]any)
		if !ok {
			return false
		}
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	case "range":
		o, ok := v.(map[string]any)
		if !ok || !closedMembers(o, []string{"min", "max"}, nil) {
			return false
		}
		if low, ok := integerValue(o["min"]); ok {
			high, ok := integerValue(o["max"])
			return ok && low.Cmp(high) <= 0
		}
		low, ok1 := o["min"].(string)
		high, ok2 := o["max"].(string)
		if !ok1 || !ok2 || !decimalForm.MatchString(low) || !decimalForm.MatchString(high) {
			return false
		}
		l, _ := new(big.Rat).SetString(low)
		h, _ := new(big.Rat).SetString(high)
		return l.Cmp(h) <= 0
	}
	return false
}

// checkCites holds a citation array (§4.3): least indices at least, each
// an integer from 0 below the number of responses, written without -0, and
// none twice.
func checkCites(v any, responses, least int) bool {
	list, ok := v.([]any)
	if !ok || len(list) < least {
		return false
	}
	seen := map[string]bool{}
	for _, item := range list {
		n, ok := integerValue(item)
		if !ok || string(item.(json.Number)) == "-0" || n.Sign() < 0 || n.Cmp(big.NewInt(int64(responses))) >= 0 || seen[n.String()] {
			return false
		}
		seen[n.String()] = true
	}
	return true
}

// checkAnswers holds R.fields to the source's declarations (§4.3,
// "Answers"): an entry for each declared field it answers, every required
// one among them, each of its status's form.
func checkAnswers(v any, r *researchRecipe, responses int) error {
	malformed := func(format string, a ...any) error {
		return researchFail(codeMalformed, format, a...)
	}
	answers, ok := v.(map[string]any)
	if !ok {
		return malformed("the research record's fields are an object")
	}
	for _, field := range sortedKeys(answers) {
		if r.types[field] == "" {
			return malformed("the research record answers %s, which the source does not declare", field)
		}
	}
	for _, d := range r.fields {
		if _, ok := answers[d.Name]; d.Required && !ok {
			return malformed("the research record has no entry for the required field %s", d.Name)
		}
	}
	for _, field := range sortedKeys(answers) {
		kind := r.types[field]
		entry, ok := answers[field].(map[string]any)
		status, _ := entry["status"].(string)
		switch {
		case !ok:
			return malformed("the answer for %s is not an object", field)
		case status == "found":
			if !closedMembers(entry, []string{"status", "cites", "value"}, nil) {
				return malformed("the answer for %s, found, is exactly status, value and cites", field)
			}
			if !checkCites(entry["cites"], responses, 1) {
				return malformed("the answer for %s, found, cites one retained response at least, each by a distinct index into the record's responses", field)
			}
			if !typedValue(entry["value"], kind) {
				return malformed("the answer for %s has a value that is not of its declared type, %s", field, kind)
			}
		case status == "conflicting":
			if !closedMembers(entry, []string{"status", "cites", "values"}, nil) {
				return malformed("the answer for %s, conflicting, is exactly status, values and cites", field)
			}
			if cites, ok := entry["cites"].([]any); !ok || len(cites) != 0 {
				return malformed("the answer for %s, conflicting, cites [] of its own: its observations carry their citations", field)
			}
			observations, ok := entry["values"].([]any)
			if !ok || len(observations) < 2 {
				return malformed("the answer for %s, conflicting, records two observations at least", field)
			}
			for i, item := range observations {
				observation, ok := item.(map[string]any)
				if !ok || !closedMembers(observation, []string{"value", "cites"}, nil) {
					return malformed("observation %d of %s is exactly a value and its cites", i, field)
				}
				if !typedValue(observation["value"], kind) {
					return malformed("observation %d of %s has a value that is not of its declared type, %s", i, field, kind)
				}
				if !checkCites(observation["cites"], responses, 1) {
					return malformed("observation %d of %s cites one retained response at least, each by a distinct index into the record's responses", i, field)
				}
			}
			differ := false
			first := observations[0].(map[string]any)["value"]
			for _, item := range observations[1:] {
				differ = differ || !jsonEqual(first, item.(map[string]any)["value"])
			}
			if !differ {
				return malformed("the answer for %s, conflicting, records no two observations that differ", field)
			}
		case status == "not-found" || status == "stale":
			if !closedMembers(entry, []string{"status", "cites"}, nil) {
				return malformed("the answer for %s, %s, is exactly status and cites, with no value", field, status)
			}
			if !checkCites(entry["cites"], responses, 0) {
				return malformed("the answer for %s cites retained responses each by a distinct index into the record's responses", field)
			}
		default:
			return malformed("the answer for %s is not of the status found, not-found, stale or conflicting", field)
		}
	}
	return nil
}

// jsonEqual is §6.3's equality: type and value, numbers by their
// mathematical value, objects as sets of members.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		p, ok1 := new(big.Rat).SetString(string(x))
		q, ok2 := new(big.Rat).SetString(string(y))
		return ok1 && ok2 && p.Cmp(q) == 0
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	case nil:
		return b == nil
	}
	return a == b
}

// checkResponseProfiles holds each retained response's profile to the
// source's assignments (§5.1): the trusted profile of its id and digest,
// one the source is bound to, of class generated. It is profile-mismatch
// otherwise.
func checkResponseProfiles(s MappingSource, r *researchRecipe, a *researchAcquisition, profiles []InputProfile) error {
	assigned := map[profileRef]bool{}
	for _, ref := range r.assignments() {
		assigned[ref] = true
	}
	// Each assignment was held to the trusted profiles first
	// (researchProfiles): one of this installation's, by its id and digest,
	// of class generated. So a response through one is through such a
	// profile, and any other is not.
	for i, response := range a.responses {
		if !assigned[response.profile] {
			return researchFail(codeMismatch, "retained response %d names the profile %s, by its id and digest, which is not one the source is bound to", i, response.profile.ID)
		}
	}
	return nil
}

// errAcquisitionStale is verifyAcquisition's finding for a time outside its
// window, which a research refusal names acquisition-stale.
var errAcquisitionStale = errors.New("acquisition is stale or from the future")

// acquisitionCode is the finding a failed acquisition check names.
func acquisitionCode(e error) string {
	if errors.Is(e, errAcquisitionStale) {
		return codeStale
	}
	return codeInvalid
}

// retainedAsAcquired is a retained response as verifyAcquisition reads an
// acquisition: {result, receipt, salts}.
func retainedAsAcquired(result, receipt, salts json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"result":`)
	b.Write(result)
	b.WriteString(`,"receipt":`)
	b.Write(receipt)
	b.WriteString(`,"salts":`)
	b.Write(salts)
	b.WriteString(`}`)
	return b.Bytes()
}

// receiptTimes says whether a receipt's servedAt and acquisition.observedAt
// are each a timestamp (spec/dialect.md §1): whole seconds in UTC, a time
// that exists. A research receipt is held to the form, before its checks.
func receiptTimes(receipt json.RawMessage) bool {
	var r struct {
		ServedAt    any `json:"servedAt"`
		Acquisition struct {
			ObservedAt any `json:"observedAt"`
		} `json:"acquisition"`
	}
	if json.Unmarshal(receipt, &r) != nil {
		return false
	}
	return paramValue(r.ServedAt, "timestamp") && paramValue(r.Acquisition.ObservedAt, "timestamp")
}

// valueDepth is a value's nesting, each object and array one level, as
// §12 counts it.
func valueDepth(v any) int {
	deepest := 0
	switch x := v.(type) {
	case map[string]any:
		for _, c := range x {
			if d := valueDepth(c); d > deepest {
				deepest = d
			}
		}
		return deepest + 1
	case []any:
		for _, c := range x {
			if d := valueDepth(c); d > deepest {
				deepest = d
			}
		}
		return deepest + 1
	}
	return 0
}

// replayResearch checks a research source's acquisition (spec/dialect.md
// §5.2, §11): each retained response in the order the record retains it,
// its result present, then checks 1 to 5 under its own profile, over its
// own request and result, and its times against at within the source's
// maxAge, and its receipt admitted to the preparation's sessions; then the
// record in the gateway's canonical domain; then the record's own receipt,
// when it has one, under the AI profile, over the source's request and the
// whole record. Nothing is asked again: the bytes are the ones retained. It
// returns the record's own citation, when it has one, and every citation in
// order, which the source adds to the preparation's cites.
func replayResearch(a *researchAcquisition, request json.RawMessage, ai InputProfile, profiles []InputProfile, keys map[string]ed25519.PublicKey, at time.Time, maxAge int, admit func(Citation) error) (*Citation, *[]Citation, error) {
	citations := []Citation{}
	for n, response := range a.responses {
		if len(response.result) == 0 {
			return nil, nil, researchFail(codeMissing, "retained response %d has no result: the bytes it is held by are not retained", n)
		}
		var profile InputProfile
		for _, p := range profiles {
			if p.ID == response.profile.ID {
				profile = p
			}
		}
		if !receiptTimes(response.receipt) {
			return nil, nil, researchFail(codeInvalid, "retained response %d, through the profile %s: its receipt's servedAt and acquisition.observedAt are not each a timestamp of whole seconds in UTC that names a time", n, profile.ID)
		}
		_, citation, e := verifyAcquisition(retainedAsAcquired(response.result, response.receipt, response.salts), response.request, profile, keys[profile.ID], at, maxAge)
		if e != nil {
			return nil, nil, researchFail(acquisitionCode(e), "retained response %d, through the profile %s: %v", n, profile.ID, e)
		}
		if e = admit(*citation); e != nil {
			return nil, nil, researchFail(codeSessions, "retained response %d: %v", n, e)
		}
		citations = append(citations, *citation)
	}
	// The whole record is in the gateway canonical domain now (§5.2): its
	// answers are typed values and indexes, each retained result and receipt
	// passed the domain's check above, and each request did at its form.
	var own *Citation
	if len(a.receipt) > 0 {
		if !receiptTimes(a.receipt) {
			return nil, nil, researchFail(codeInvalid, "the record's own receipt, through the profile %s: its servedAt and acquisition.observedAt are not each a timestamp of whole seconds in UTC that names a time", ai.ID)
		}
		_, citation, e := verifyAcquisition(retainedAsAcquired(a.raw, a.receipt, a.salts), request, ai, keys[ai.ID], at, maxAge)
		if e != nil {
			return nil, nil, researchFail(acquisitionCode(e), "the record's own receipt, through the profile %s: %v", ai.ID, e)
		}
		if e = admit(*citation); e != nil {
			return nil, nil, researchFail(codeSessions, "the record's own receipt: %v", e)
		}
		own = citation
		citations = append(citations, *citation)
	}
	return own, &citations, nil
}

// readResearch applies a research source's read to its record (§4.3), or,
// for a reconciliation, to {"current": R, "sources": U}, U holding the
// record of each earlier source it names that was acquired, a skipped one
// absent (§6.4), the whole held to the nesting bound before the rule runs.
func readResearch(s MappingSource, record any, records map[string]any, params map[string]any) (derivedClaim, error) {
	var value any
	var e error
	if len(s.Read.Sources) > 0 {
		names, _ := reconciliationNames(s.Read.Sources)
		earlier := map[string]any{}
		for _, name := range names {
			if r, ok := records[name]; ok {
				earlier[name] = r
			}
		}
		value = map[string]any{"current": record, "sources": earlier}
		if valueDepth(value) > 32 {
			return derivedClaim{}, researchFail(codeOverBound, "the value the reconciliation reads is nested more than 32 levels")
		}
		if e = boundedArtifact(value); e != nil {
			return derivedClaim{}, researchFail(codeOverBound, "%v", e)
		}
	} else if value, e = unwrapRead(s.Read, record); e != nil {
		return derivedClaim{}, researchFail(codeReadFailed, "%v", e)
	}
	claim, e := deriveRead(s.Read, value, params)
	if e != nil {
		return derivedClaim{}, researchFail(codeReadFailed, "%v", e)
	}
	return claim, nil
}
