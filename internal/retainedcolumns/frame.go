package retainedcolumns

// Profile is the explicitly negotiated Draft retained-frame budget. These are
// frame bounds only; the caller must also count the complete combined carrier.
type Profile string

const (
	Base     Profile = "base"
	Extended Profile = "extended"
)

type retainedField struct {
	status, raw string
	hasRaw      bool
}
type declaration struct {
	revision string
	fields   map[string]originalField
}
type originalField struct{ raw, kind string }
type research struct {
	revision    string
	declaration *declaration
	fields      map[string]retainedField
}

// Frame owns a validated projection. No mutable decoded fields are exposed.
// Its zero value is invalid. Its identity is never an original source identity.
type Frame struct {
	origin           string
	values           map[string]any
	research         *research
	projectionDigest string
	valid            bool
}

func (f Frame) ProjectedFrameSHA256() string { return f.projectionDigest }
func (f Frame) Origin() string               { return f.origin }

// ParseFrame retains exact received projection identity and embedded raw strings.
// The selected profile cannot enlarge fixed compatibility limits. It establishes
// neither original-source authenticity nor generated-input review authorization.
func ParseFrame(raw []byte, profile Profile) (Frame, error) {
	max, depth := 0, 0
	switch profile {
	case Base:
		max, depth = 2097152, 64
	case Extended:
		max, depth = 4194304, 128
	default:
		return Frame{}, refuse("input-malformed")
	}
	if len(raw) > max {
		return Frame{}, refuse("input-over-bound")
	}
	v, err := parseJSON(raw, depth, false, true)
	if err != nil {
		return Frame{}, err
	}
	m, ok := v.(map[string]any)
	if !ok || !closed(m, "contractVersion", "origin", "values", "research") || m["contractVersion"] != "1" {
		return Frame{}, refuse("input-malformed")
	}
	origin, ok := m["origin"].(string)
	if !ok || origin != "asserted" && origin != "generated" {
		return Frame{}, refuse("input-malformed")
	}
	values, ok := m["values"].(map[string]any)
	if !ok {
		return Frame{}, refuse("input-malformed")
	}
	if len(values) > 1000 {
		return Frame{}, refuse("input-over-bound")
	}
	// Generated values are exact original record strings, bounded by the
	// containing original snapshot. Asserted/CSV cells keep their old limit.
	valueLimit := 16000
	if origin == "generated" {
		valueLimit = 524288
	}
	for name, value := range values {
		if _, err = boundedString(name, 160, true); err != nil {
			return Frame{}, err
		}
		if value != nil {
			if _, err = boundedString(value, valueLimit, false); err != nil {
				return Frame{}, err
			}
		}
	}
	f := Frame{origin: origin, values: values, projectionDigest: digest(raw)}
	if origin == "asserted" {
		if m["research"] != nil {
			return Frame{}, refuse("input-malformed")
		}
		f.valid = true
		return f, nil
	}
	rm, ok := m["research"].(map[string]any)
	if !ok || !closed(rm, "recordRecipeRevision", "declaration", "fields") {
		return Frame{}, refuse("input-malformed")
	}
	r := &research{}
	if r.revision, err = boundedString(rm["recordRecipeRevision"], 160, true); err != nil {
		return Frame{}, err
	}
	if rm["declaration"] != nil {
		dm, ok := rm["declaration"].(map[string]any)
		if !ok || !closed(dm, "recipeRevision", "fields") {
			return Frame{}, refuse("input-malformed")
		}
		d := &declaration{fields: map[string]originalField{}}
		if d.revision, err = boundedString(dm["recipeRevision"], 160, true); err != nil {
			return Frame{}, err
		}
		entries, ok := dm["fields"].([]any)
		if !ok {
			return Frame{}, refuse("input-malformed")
		}
		if len(entries) > 1000 {
			return Frame{}, refuse("input-over-bound")
		}
		for _, entry := range entries {
			e, ok := entry.(map[string]any)
			if !ok || !closed(e, "name", "fieldJson") {
				return Frame{}, refuse("input-malformed")
			}
			name, e1 := boundedString(e["name"], 160, true)
			if e1 != nil {
				return Frame{}, e1
			}
			rawField, e1 := boundedString(e["fieldJson"], 16384, false)
			if e1 != nil {
				return Frame{}, e1
			}
			if _, exists := d.fields[name]; exists {
				return Frame{}, refuse("input-malformed")
			}
			original, e1 := parseJSON([]byte(rawField), 32, false, true)
			if e1 != nil {
				return Frame{}, e1
			}
			originalMap, ok := original.(map[string]any)
			if !ok || originalMap["name"] != name {
				return Frame{}, refuse("input-malformed")
			}
			kind, ok := originalMap["type"].(string)
			if !ok {
				return Frame{}, refuse("input-malformed")
			}
			d.fields[name] = originalField{raw: rawField, kind: kind}
		}
		r.declaration = d
	}
	fields, ok := rm["fields"].(map[string]any)
	if !ok {
		return Frame{}, refuse("input-malformed")
	}
	if len(fields) > 1000 {
		return Frame{}, refuse("input-over-bound")
	}
	r.fields = map[string]retainedField{}
	for name, value := range fields {
		if _, err = boundedString(name, 160, true); err != nil {
			return Frame{}, err
		}
		entry, ok := value.(map[string]any)
		if !ok {
			return Frame{}, refuse("input-malformed")
		}
		rawValue, present := entry["valueJson"]
		if present {
			if !closed(entry, "status", "valueJson") {
				return Frame{}, refuse("input-malformed")
			}
		} else if !closed(entry, "status") {
			return Frame{}, refuse("input-malformed")
		}
		status, ok := entry["status"].(string)
		if !ok {
			return Frame{}, refuse("input-malformed")
		}
		switch status {
		case "supported", "interpretation", "missing", "conflicting", "inaccessible", "stale":
		default:
			return Frame{}, refuse("input-malformed")
		}
		field := retainedField{status: status, hasRaw: present}
		// Raw typed JSON is opaque here. Its own structure/type limit is a selected
		// per-record issue, even if this frame is only an inspectable proposal.
		if present {
			if field.raw, err = boundedString(rawValue, 4194304, false); err != nil {
				return Frame{}, err
			}
		}
		r.fields[name] = field
	}
	f.research = r
	f.valid = true
	return f, nil
}
