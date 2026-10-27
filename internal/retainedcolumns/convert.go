package retainedcolumns

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
)

// Issue is a pinned deterministic diagnostic, never a claim or preparation.
type Issue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var messages = map[string]string{
	"decimal-encoding":      "Use a plain decimal without separators, units, exponents, or leading zeros.",
	"number-encoding":       "Use an unambiguous number without separators, units, or leading zeros.",
	"number-range":          "Number is outside the supported precise range.",
	"number-exponent":       "Number exponent is outside the supported range.",
	"number-precision":      "This number would lose precision. Keep it as text or revise it.",
	"boolean-spelling":      "Does not match either declared Yes or No value.",
	"research-context":      "Research value requires a typed research field.",
	"research-metadata":     "Record metadata cannot use Research value. Choose another conversion.",
	"research-declaration":  "The original research field declaration is unavailable.",
	"research-field":        "Choose a declared research field for Research value.",
	"research-type":         "The research value does not match its original field type.",
	"evidence-availability": "Evidence availability must be present, absent, or unknown.",
}

// PreviewResult contains exact JSON text, never a float-coerced value. Omission
// has no candidate; Issues are record diagnostics. ClaimAdmissible only tests
// the existing claim boundary and does not establish preparation validity.
type PreviewResult struct {
	ValueJSON       *string `json:"valueJson,omitempty"`
	Omitted         string  `json:"omitted,omitempty"`
	Issues          []Issue `json:"issues"`
	ClaimAdmissible bool    `json:"claimAdmissible"`
}

func issue(code string) PreviewResult {
	return PreviewResult{Issues: []Issue{{Code: code, Message: messages[code]}}}
}
func omitted(reason string) PreviewResult { return PreviewResult{Omitted: reason, Issues: []Issue{}} }
func candidate(raw string) PreviewResult {
	return PreviewResult{ValueJSON: &raw, Issues: []Issue{}, ClaimAdmissible: ClaimAdmissible([]byte(raw))}
}
func resolved(status string) bool { return status == "supported" || status == "interpretation" }

// ClaimAdmissible uses Runner's existing canonicalizer, preceded by the same
// scalar/duplicate/exact-number and depth32 input domain used by proofCanon.
// This is a domain check; no emitted spelling is replaced by canonical bytes.
func ClaimAdmissible(raw []byte) bool {
	// proofCanon at the frozen Runner base also enforces MaxBody = 2 << 20.
	// Every selected typed candidate is already <= 524288 bytes; this guard
	// keeps direct calls to this domain helper consistent with that entry point.
	if len(raw) > 2<<20 {
		return false
	}
	if _, err := parseJSON(raw, 32, false, true); err != nil {
		return false
	}
	_, err := derivation.Canon(raw)
	return err == nil
}

// Preview interprets one validated leaf and raw frame. It has no acquisition,
// review, authorization or evaluation effects. Zero-value inputs refuse.
func Preview(p Plan, f Frame) (PreviewResult, error) {
	if !p.valid || !f.valid {
		return PreviewResult{}, refuse("input-malformed")
	}
	if p.kind == "research-value" {
		if f.origin != "generated" {
			return issue("research-context"), nil
		}
		switch p.column {
		case "name", "url", "website", "company_id", "company_name", "company_url":
			return issue("research-metadata"), nil
		}
		r := f.research
		if r.declaration == nil || r.declaration.revision != r.revision {
			return issue("research-declaration"), nil
		}
		original, ok := r.declaration.fields[p.column]
		if !ok {
			return issue("research-field"), nil
		}
		field, ok := r.fields[p.column]
		if !ok || !resolved(field.status) {
			return omitted("unavailable"), nil
		}
		if !typedValue(field.raw, original.kind) {
			return issue("research-type"), nil
		}
		return candidate(field.raw), nil
	}
	value, ok := f.values[p.column]
	if !ok {
		return omitted("missing"), nil
	}
	if value == nil {
		return omitted("legacy-null"), nil
	}
	text := value.(string)
	if blank(text) {
		return omitted("blank"), nil
	}
	if f.origin == "generated" {
		if field, ok := f.research.fields[p.column]; ok && !resolved(field.status) {
			return omitted("unavailable"), nil
		}
	}
	switch p.kind {
	case "text":
		return candidate(compactString(text)), nil
	case "decimal":
		if decimalValue(text) {
			return candidate(compactString(text)), nil
		}
		return issue("decimal-encoding"), nil
	case "number":
		if code := numberIssue(text); code != "" {
			return issue(code), nil
		}
		return candidate(text), nil
	case "boolean":
		if text == p.yes {
			return candidate("true"), nil
		}
		if text == p.no {
			return candidate("false"), nil
		}
		return issue("boolean-spelling"), nil
	case "evidence-availability":
		if text == "present" || text == "absent" || text == "unknown" {
			return candidate(compactString(text)), nil
		}
		return issue("evidence-availability"), nil
	}
	return PreviewResult{}, refuse("input-malformed")
}

func typedValue(raw, kind string) bool {
	if len(raw) == 0 || len(raw) > 524288 || (kind == "string" || kind == "number") && len(raw) > 8192 {
		return false
	}
	legacy := kind == "string" || kind == "number"
	v, err := parseJSON([]byte(raw), 64, true, !legacy)
	if err != nil {
		return false
	}
	switch kind {
	case "string":
		_, ok := v.(string)
		return ok
	case "text":
		s, ok := v.(string)
		return ok && len(s) <= 8192
	case "number":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, e := strconv.ParseFloat(string(n), 64)
		return e == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	case "integer":
		return integerValue(v)
	case "decimal":
		return decimalValue(v)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "text-list":
		a, ok := v.([]any)
		if !ok || len(a) > 64 {
			return false
		}
		for _, x := range a {
			s, ok := x.(string)
			if !ok || len(s) > 1024 {
				return false
			}
		}
		return true
	case "range":
		m, ok := v.(map[string]any)
		if !ok || !closed(m, "min", "max") {
			return false
		}
		a, b := m["min"], m["max"]
		if integerValue(a) && integerValue(b) {
			return rat(string(a.(json.Number))).Cmp(rat(string(b.(json.Number)))) <= 0
		}
		return decimalValue(a) && decimalValue(b) && rat(a.(string)).Cmp(rat(b.(string))) <= 0
	}
	return false
}
