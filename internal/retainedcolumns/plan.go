package retainedcolumns

import "fmt"

// Refusal distinguishes malformed/bounded input from a per-record conversion issue.
// Details intentionally contain no retained values or declaration contents.
type Refusal struct{ Code string }

func (e *Refusal) Error() string { return e.Code }
func refuse(code string) error   { return &Refusal{Code: code} }

// Plan is a closed validated leaf. Its zero value cannot be interpreted.
type Plan struct {
	column, kind, yes, no string
	valid                 bool
}

// ParsePlan reads a conversion (including kind) or an availability (without kind).
// It does not validate the surrounding copy, source pointer, target or document.
func ParsePlan(raw []byte) (Plan, error) {
	v, err := parseJSON(raw, 4, false, true)
	if err != nil {
		return Plan{}, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Plan{}, refuse("input-malformed")
	}
	kind, hasKind := m["kind"]
	keys := []string{"contractVersion", "column"}
	p := Plan{kind: "evidence-availability"}
	if hasKind {
		keys = append(keys, "kind")
		p.kind, ok = kind.(string)
		if !ok {
			return Plan{}, refuse("input-malformed")
		}
		switch p.kind {
		case "text", "decimal", "number", "research-value":
		case "boolean":
			keys = append(keys, "trueValue", "falseValue")
		default:
			return Plan{}, refuse("input-malformed")
		}
	}
	if !closed(m, keys...) || m["contractVersion"] != "1" {
		return Plan{}, refuse("input-malformed")
	}
	if p.column, err = boundedString(m["column"], 160, true); err != nil {
		return Plan{}, err
	}
	if p.kind == "boolean" {
		if p.yes, err = boundedString(m["trueValue"], 160, true); err != nil {
			return Plan{}, err
		}
		if p.no, err = boundedString(m["falseValue"], 160, true); err != nil {
			return Plan{}, err
		}
		if blank(p.yes) || blank(p.no) || p.yes == p.no {
			return Plan{}, refuse("input-malformed")
		}
	}
	p.valid = true
	return p, nil
}

func closed(m map[string]any, names ...string) bool {
	if len(m) != len(names) {
		return false
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			return false
		}
	}
	return true
}
func boundedString(v any, max int, nonempty bool) (string, error) {
	s, ok := v.(string)
	if !ok || nonempty && s == "" {
		return "", refuse("input-malformed")
	}
	if len(s) > max {
		return "", refuse("input-over-bound")
	}
	return s, nil
}
func blank(s string) bool {
	for _, r := range s {
		switch {
		case r >= 9 && r <= 13, r == 32, r == 0x85, r == 0xa0, r == 0x1680,
			r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		default:
			return false
		}
	}
	return true
}
func digest(raw []byte) string { return fmt.Sprintf("sha256:%x", sha256Bytes(raw)) }
