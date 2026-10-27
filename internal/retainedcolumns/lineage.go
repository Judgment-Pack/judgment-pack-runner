package retainedcolumns

import "encoding/json"

// InputState is the Draft preparation representation. Diagnostic raw input is
// resolved from the exact retained occurrence; it is never repeated per target.
type InputState struct {
	State string          `json:"state"`
	Ref   *InputReference `json:"ref,omitempty"`
}
type InputReference struct {
	FrameSHA256 string `json:"frameSha256"`
	Location    string `json:"location"`
}

func (s *InputState) UnmarshalJSON(raw []byte) error {
	v, e := parseJSON(raw, 4, false, true)
	if e != nil {
		return e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return refuse("input-malformed")
	}
	switch m["state"] {
	case "missing":
		if !closed(m, "state") {
			return refuse("input-malformed")
		}
		*s = InputState{State: "missing"}
		return nil
	case "present":
		if !closed(m, "state", "ref") {
			return refuse("input-malformed")
		}
		ref, ok := m["ref"].(map[string]any)
		if !ok || !closed(ref, "frameSha256", "location") {
			return refuse("input-malformed")
		}
		pin, ok := ref["frameSha256"].(string)
		if !ok || len(pin) != 71 || pin[:7] != "sha256:" {
			return refuse("input-malformed")
		}
		for _, c := range pin[7:] {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return refuse("input-malformed")
			}
		}
		location, _ := ref["location"].(string)
		if location != "values" && location != "research-value" {
			return refuse("input-malformed")
		}
		*s = InputState{State: "present", Ref: &InputReference{FrameSHA256: pin, Location: location}}
		return nil
	}
	return refuse("input-malformed")
}

type ResultState struct {
	State  string  `json:"state"`
	JSON   *string `json:"json,omitempty"`
	Reason string  `json:"reason,omitempty"`
}
type ResearchIdentity struct {
	RecipeRevision string `json:"recipeRevision"`
	FieldSHA256    string `json:"fieldSha256"`
}

// Transformation retains each result and original identity. Its input reference
// resolves only within the caller-selected source/read/receipt occurrence.
type Transformation struct {
	ContractVersion  string            `json:"contractVersion"`
	Column           string            `json:"column"`
	Input            InputState        `json:"input"`
	Result           ResultState       `json:"result"`
	ResearchIdentity *ResearchIdentity `json:"researchIdentity,omitempty"`
}

func frameInput(p Plan, f Frame) (*string, string) {
	if p.kind == "research-value" {
		if f.research != nil {
			if field, ok := f.research.fields[p.column]; ok && field.hasRaw {
				raw := field.raw
				return &raw, "research-value"
			}
		}
		return nil, "research-value"
	}
	if value, ok := f.values[p.column]; ok {
		raw := "null"
		if value != nil {
			raw = compactString(value.(string))
		}
		return &raw, "values"
	}
	return nil, "values"
}

// ResolveInput is a diagnostic read of the already-bound exact occurrence. The
// caller must supply that occurrence, never search or substitute by hash. The
// parent plan selects column and location; the digest grants no authority.
func ResolveInput(p Plan, f Frame, t *Transformation) (*string, error) {
	if !p.valid || !f.valid {
		return nil, refuse("input-malformed")
	}
	if t == nil || t.ContractVersion != "1" || t.Column != p.column {
		return nil, refuse("read-failed")
	}
	preview, e := Preview(p, f)
	if e != nil {
		return nil, e
	}
	if len(preview.Issues) > 0 || preview.ValueJSON != nil && !preview.ClaimAdmissible {
		return nil, refuse("read-failed")
	}
	input := t.Input
	raw, location := frameInput(p, f)
	if raw == nil {
		if input.State != "missing" || input.Ref != nil {
			return nil, refuse("read-failed")
		}
		return nil, nil
	}
	if input.State != "present" || input.Ref == nil || input.Ref.FrameSHA256 != f.projectionDigest || input.Ref.Location != location {
		return nil, refuse("read-failed")
	}
	return raw, nil
}

// Derive recomputes reference and result from the exact selected frame. It reads
// no retained result or reference supplied by a caller and emits no preparation.
func Derive(p Plan, f Frame) (*Transformation, error) {
	preview, e := Preview(p, f)
	if e != nil {
		return nil, e
	}
	if len(preview.Issues) > 0 || preview.ValueJSON != nil && !preview.ClaimAdmissible {
		return nil, refuse("read-failed")
	}
	t := &Transformation{ContractVersion: "1", Column: p.column, Input: InputState{State: "missing"}, Result: ResultState{State: "omitted", Reason: preview.Omitted}}
	if preview.ValueJSON != nil {
		t.Result = ResultState{State: "value", JSON: preview.ValueJSON}
	}
	if raw, location := frameInput(p, f); raw != nil {
		t.Input = InputState{State: "present", Ref: &InputReference{FrameSHA256: f.projectionDigest, Location: location}}
	}
	if p.kind == "research-value" {
		original := f.research.declaration.fields[p.column]
		t.ResearchIdentity = &ResearchIdentity{RecipeRevision: f.research.revision, FieldSHA256: digest([]byte(original.raw))}
	}
	return t, nil
}

// Input JSON is a separate preview diagnostic, not a preparation member.
func DiagnosticInputJSON(p Plan, f Frame) (json.RawMessage, error) {
	t, e := Derive(p, f)
	if e != nil {
		return nil, e
	}
	raw, e := ResolveInput(p, f, t)
	if e != nil || raw == nil {
		return nil, e
	}
	return json.RawMessage(*raw), nil
}
