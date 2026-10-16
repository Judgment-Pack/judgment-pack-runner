package runner

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

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
			if k != "response" && k != "snapshot" {
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
	return exactPreparationKeys(raw, reflect.TypeOf(m))
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
		if known && source.Kind == "selected-file" {
			if len(value.Response) > 0 || len(value.Snapshot) == 0 {
				return researchFail(codeMalformed, "invalid selected file")
			}
			continue
		}
		if len(value.Snapshot) > 0 {
			return researchFail(codeMalformed, "unexpected snapshot")
		}
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
