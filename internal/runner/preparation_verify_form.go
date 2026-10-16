package runner

import (
	"encoding/json"
	"errors"
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
