package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// exactAcquisitionMembers holds each acquisition to the one member §5.2
// gives every source's, response: a selected file's included, whose kind is
// not read (§11's sixth phase).
func exactAcquisitionMembers(raw []byte) bool {
	var values map[string]map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return false
	}
	for _, v := range values {
		if v == nil {
			return false
		}
		for k := range v {
			if k != "response" {
				return false
			}
		}
	}
	return true
}
func exactDialectForm(raw []byte, m InputMapping) error {
	if m.Version != 2 || m.FormatVersion != "" {
		return asWritten(raw, m)
	}
	// Family 2 permits explicit empty optional members. Validate exact keys
	// without comparing against Go's omit-empty writer.
	if err := exactPreparationKeys(raw, reflect.TypeOf(m)); err != nil {
		return err
	}
	return exactKindMembers(raw)
}

// exactKindMembers holds each object of a version-2 dialect whose Go struct
// several kinds share to its own kind's members, as spec/dialect.md §3, §4,
// §4.1 and §4.2 list them and the specification's reader holds them: a case
// parameter exactly pointer and type, a source parameter exactly from,
// pointer and type, an operation §4's members and §4.1's, and a
// selected-file §4's, its provider and that provider's. exactPreparationKeys
// admits every kind's members, and once decoded a member of another kind
// written with its zero value, "from": "" or "provider": "", is not told
// from one not written.
func exactKindMembers(raw []byte) error {
	v, err := readInputJSONDepth(raw, maxDocumentDepth)
	if err != nil {
		return err
	}
	object := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	unknown := errors.New("a member its kind does not have, or without one it has")
	top := object(v)
	for _, p := range object(object(top["case"])["parameters"]) {
		if !closedMembers(object(p), []string{"pointer", "type"}, nil) {
			return unknown
		}
	}
	sources, _ := top["sources"].([]any)
	for _, value := range sources {
		s := object(value)
		for _, p := range object(s["parameters"]) {
			if !closedMembers(object(p), []string{"from", "pointer", "type"}, nil) {
				return unknown
			}
		}
		common := []string{"name", "kind", "read"}
		var required, optional []string
		switch {
		case s["kind"] == "operation":
			required, optional = []string{"profile", "profileDigest", "maxAge", "arguments"}, []string{"parameters", "calculation"}
		case s["kind"] == "selected-file" && s["provider"] == "local-file":
			required, optional = []string{"provider"}, []string{"parameters"}
		case s["kind"] == "selected-file" && s["provider"] == "google-drive":
			required, optional = []string{"provider", "profile", "profileDigest", "maxAge", "arguments"}, []string{"parameters"}
		default:
			// Another kind or provider is validateDialect's to refuse.
			continue
		}
		if !closedMembers(s, append(common, required...), optional) {
			return unknown
		}
	}
	return nil
}

func exactPreparationKeys(raw []byte, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return errors.New("member cannot be null")
	}
	switch typ.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return errors.New("object required")
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key != "" && key != "-" {
				fields[key] = f.Type
			}
		}
		for key, v := range obj {
			t, ok := fields[key]
			if !ok {
				return errors.New("unknown member")
			}
			// Legacy root facts/evidence are absent or null, never arrays. The
			// profile endpoint is the only nullable typed metadata member.
			if typ == reflect.TypeOf(InputMapping{}) && (key == "facts" || key == "evidence") {
				if strings.TrimSpace(string(v)) != "null" {
					return errors.New("legacy member must be null")
				}
				continue
			}
			if typ == reflect.TypeOf(InputProfile{}) && key == "endpoint" && strings.TrimSpace(string(v)) == "null" {
				continue
			}
			if err := exactPreparationKeys(v, t); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return errors.New("array required")
		}
		for _, v := range values {
			if err := exactPreparationKeys(v, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return errors.New("object required")
		}
		for _, v := range values {
			if err := exactPreparationKeys(v, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func preparationAcquisitionForms(m InputMapping, acquisitions map[string]SourceValue) error {
	sources := map[string]MappingSource{}
	for _, s := range m.Sources {
		sources[s.Name] = s
	}
	for name, value := range acquisitions {
		source, known := sources[name]
		if known && isResearch(source) {
			recipe, err := readResearchRecipe(source, nil, true)
			if err != nil {
				return err
			}
			if _, err := readResearchAcquisition(value, recipe); err != nil {
				return err
			}
			continue
		}
		// Every other acquisition, a selected file's among them, has §5.2's
		// envelope; a kind the verifier does not read is refused after every
		// form (§11's sixth phase).
		if !operationEnvelopeForm(value.Response) {
			return researchFail(codeMalformed, "invalid acquisition envelope")
		}
	}
	return nil
}
func operationEnvelopeForm(raw []byte) bool {
	v, err := readInputJSONDepth(raw, 64)
	if err != nil {
		return false
	}
	obj, ok := v.(map[string]any)
	if !ok || !closedMembers(obj, []string{"result", "receipt", "salts"}, nil) {
		return false
	}
	_, receipt := obj["receipt"].(map[string]any)
	return receipt && saltsForm(obj["salts"])
}

// unreadKindForm holds a source of a kind of version 3 that Runner does not
// prepare, http, sql or web, to its form as spec/dialect.md §4.3 gives it and
// the specification's reader holds it: §4's members, profile, profileDigest
// and maxAge as in §4.1, and its kind's, each of its form, its templates
// held to scope. Offline, the kind is refused after every form (§11's sixth
// phase); a run refuses it by name (refusedKind).
func unreadKindForm(s MappingSource, scope map[string]string) error {
	malformed := func(format string, a ...any) error {
		return fmt.Errorf("source %s: "+format, append([]any{s.Name}, a...)...)
	}
	kinds := map[string][2][]string{
		"http": {{"method", "path"}, {"query", "body", "headers"}},
		"sql":  {{"statement", "bindings"}, nil},
		"web":  {{"url"}, nil},
	}
	required, optional := kinds[s.Kind][0], kinds[s.Kind][1]
	present := map[string]json.RawMessage{}
	for _, m := range version3Members(s) {
		if len(m.value) > 0 {
			present[m.name] = m.value
		}
	}
	if s.Provider != "" || len(s.Arguments) > 0 || s.Calculation != nil {
		return malformed("a member a source of kind %s does not have", s.Kind)
	}
	allowed := map[string]bool{}
	for _, name := range required {
		if _, ok := present[name]; !ok {
			return malformed("a source of kind %s has %s", s.Kind, strings.Join(required, " and "))
		}
		allowed[name] = true
	}
	for _, name := range optional {
		allowed[name] = true
	}
	for name := range present {
		if !allowed[name] {
			return malformed("the member %s, which a source of kind %s does not have", name, s.Kind)
		}
	}
	if s.Profile == "" || !validDigest(s.ProfileDigest) || s.MaxAge < 1 || s.MaxAge > 86400 {
		return malformed("a source of kind %s requires a pinned profile and maxAge 1..86400", s.Kind)
	}
	value := func(name string) any { v, _ := rawValue(present[name]); return v }
	template := func(v any) bool { _, e := instantiate(v, nil, scope, true); return e == nil }
	// A string template (§4.1): a string, a $text, or a $param naming a
	// string or timestamp parameter.
	stringTemplate := func(v any) bool {
		if _, ok := v.(string); ok {
			return true
		}
		o, ok := v.(map[string]any)
		if !ok || len(o) != 1 || !template(v) {
			return false
		}
		if _, ok := o["$text"]; ok {
			return true
		}
		name, _ := o["$param"].(string)
		return scope[name] == "string" || scope[name] == "timestamp"
	}
	names := func(v any) bool {
		list, ok := v.([]any)
		for _, item := range list {
			if s, isString := item.(string); !isString || s == "" {
				return false
			}
		}
		return ok
	}
	paramForm := func(v any) bool {
		o, ok := v.(map[string]any)
		_, param := o["$param"]
		return ok && len(o) == 1 && param && template(v)
	}
	switch s.Kind {
	case "http":
		method, _ := value("method").(string)
		if method != "GET" && method != "POST" {
			return malformed("a method other than GET and POST")
		}
		if !stringTemplate(value("path")) {
			return malformed("path is not a string template")
		}
		if _, ok := present["query"]; ok {
			query, ok := value("query").(map[string]any)
			if !ok {
				return malformed("query is not an object of string templates")
			}
			for name, member := range query {
				if strings.HasPrefix(name, "$") || !stringTemplate(member) {
					return malformed("query is not an object of string templates")
				}
			}
		}
		if _, ok := present["body"]; ok && (method == "GET" || !template(value("body"))) {
			return malformed("a body beside GET, or not a template")
		}
		if _, ok := present["headers"]; ok && !names(value("headers")) {
			return malformed("headers is not an array of non-empty strings")
		}
	case "sql":
		if _, ok := value("statement").(string); !ok {
			return malformed("statement is not a string, and is never a template")
		}
		bindings, ok := value("bindings").([]any)
		if !ok {
			return malformed("bindings is not an array")
		}
		for _, item := range bindings {
			b, ok := item.(map[string]any)
			if !ok || !closedMembers(b, []string{"name", "type", "value"}, nil) || !names([]any{b["name"], b["type"]}) || !paramForm(b["value"]) {
				return malformed("a binding that is not a name, a type and a $param form")
			}
		}
	case "web":
		url := value("url")
		if _, ok := url.(string); !ok {
			form, _ := url.(map[string]any)
			name, _ := form["$param"].(string)
			if !paramForm(url) || scope[name] != "string" {
				return malformed("url is not a string or a $param form naming a string parameter")
			}
		}
	}
	return nil
}

// bindingDocumentForm holds a binding document to its form (spec/dialect.md
// §4.4) as the specification's reader does, before any kind is refused
// (§11's fifth and sixth phases): an object of exactly dialect, the digest
// of the dialect it binds, and sources, which names exactly that dialect's
// unbound sources, one at least, each assigned exactly a profile's id and
// digest, a research source with its webProfiles too, exactly the web
// capabilities it asks for. Each sentence is the import step's
// (bindDialect, bindResearch), which reads the document again when the
// dialect is prepared and holds each assignment to the profiles.
func bindingDocumentForm(binding any, published []byte, m InputMapping) error {
	fail := func(format string, a ...any) error {
		return inputProblem("Mapping v3: " + fmt.Sprintf(format, a...))
	}
	document, ok := binding.(map[string]any)
	if !ok {
		return fail("the binding document is not one JSON object")
	}
	for _, name := range sortedKeys(document) {
		if name != "dialect" && name != "sources" {
			return fail("the binding document has the member %s: it is an object of exactly dialect and sources, which binds each unbound source to a profile and changes nothing else in the dialect", name)
		}
	}
	named, _ := document["dialect"].(string)
	if !validDigest(named) {
		return fail("the binding document's dialect is not the digest of the dialect it binds")
	}
	entries, ok := document["sources"].(map[string]any)
	if !ok {
		return fail("the binding document's sources are not an object giving each unbound source its profile")
	}
	for _, name := range sortedKeys(entries) {
		entry, _ := entries[name].(map[string]any)
		for _, member := range sortedKeys(entry) {
			if member != "profile" && member != "profileDigest" && member != "webProfiles" {
				return fail("the binding document gives source %s the member %s: it binds a source to a profile, by its id and digest, and changes nothing else in the dialect", name, member)
			}
		}
		if _, ok := checkAssignment(map[string]any{"profile": entry["profile"], "profileDigest": entry["profileDigest"]}); !ok {
			return fail("the binding document gives source %s no profile: an id and a digest", name)
		}
	}
	unbound := map[string]MappingSource{}
	sources := map[string]bool{}
	for _, s := range m.Sources {
		sources[s.Name] = true
		if unboundOperation(s) || unboundResearch(s) {
			unbound[s.Name] = s
		}
	}
	if len(unbound) == 0 {
		if m.Version != 3 {
			return fail("the published dialect is of version %d: only a dialect of version 3 has unbound sources to bind, and one of version 2 is given as the mapping", m.Version)
		}
		return fail("the dialect has no unbound source, and a binding document beside it binds nothing: give the dialect as the mapping")
	}
	if given, err := proofCanon(published); err != nil || named != digest(given) {
		return fail("the binding document binds the dialect %s, and this one's digest is %s", named, digest(given))
	}
	for _, name := range sortedKeys(entries) {
		if !sources[name] {
			return fail("the binding document binds %s, which is not a source of the dialect", name)
		}
		if _, ok := unbound[name]; !ok {
			return fail("the binding document binds %s, which is not an unbound source: a binding binds unbound sources only", name)
		}
	}
	for _, s := range m.Sources {
		if _, ok := unbound[s.Name]; !ok {
			continue
		}
		entry, ok := entries[s.Name].(map[string]any)
		if !ok {
			return fail("the binding document leaves source %s unbound: it binds every unbound source of the dialect", s.Name)
		}
		web, assigned := entry["webProfiles"]
		if !isResearch(s) {
			if assigned {
				return fail("the binding document gives source %s the member webProfiles: it binds a source to a profile, by its id and digest, and changes nothing else in the dialect", s.Name)
			}
			continue
		}
		var capabilities []string
		_ = json.Unmarshal(s.Capabilities, &capabilities)
		if _, ok := checkWebProfiles(web, capabilities); !ok {
			return researchFail(codeMalformed, "the binding document assigns the research source %s web profiles other than exactly the web capabilities it asks for, each a profile's id and digest", s.Name)
		}
	}
	return nil
}
