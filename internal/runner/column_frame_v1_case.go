package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

func hasColumnCase(m InputMapping) bool {
	return m.Case != nil && hasColumnCopy(CopyMapping{Facts: m.Case.Facts, Evidence: m.Case.Evidence})
}

// columnCaseBytes materializes exact custody text transiently. Callers retain
// either the historical object or the sole exact string, never both copies.
func columnCaseBytes(c *SourceInput) (json.RawMessage, error) {
	if c == nil {
		return nil, nil
	}
	raw := c.Case
	if c.RetainedCaseJSON != "" {
		if len(c.Case) > 0 {
			return nil, withCode(codeMalformed, errors.New("case and retainedCaseJson are mutually exclusive"))
		}
		raw = json.RawMessage(c.RetainedCaseJSON)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > artifactLimit(c.ConversionContract, MaxBody, 2*MaxBody+256) {
		return nil, withCode(codeOverBound, errors.New("case custody exceeds selected case limit"))
	}
	if c.ConversionContract != nil {
		if e := measureBytes(raw, maxValueDepth); e != nil {
			return nil, e
		}
	}
	v, e := readInputJSON(raw)
	if e != nil {
		return nil, withCode(codeMalformed, errors.New("case custody is not strict UTF8 object JSON"))
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, withCode(codeMalformed, errors.New("case custody requires one object"))
	}
	return raw, nil
}
func columnCaseSlotBytes(c *SourceInput) int {
	if c.RetainedCaseJSON != "" {
		return len(encode(map[string]string{"retainedCaseJson": c.RetainedCaseJSON}))
	}
	if hasColumnCase(c.Mapping) {
		raw := c.Case
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		return len(encode(map[string]string{"retainedCaseJson": string(raw)}))
	}
	return len(c.Case)
}
func retainColumnCase(c *SourceInput, raw []byte) { c.Case = nil; c.RetainedCaseJSON = string(raw) }
func setColumnCase(c *SourceInput, raw []byte) {
	c.RetainedCaseJSON = ""
	c.Case = append(json.RawMessage(nil), raw...)
}
func (c *SourceInput) UnmarshalJSON(raw []byte) error {
	type plain SourceInput
	var value plain
	if e := strictJSON(raw, &value); e != nil {
		return e
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*c = SourceInput(value)
		return nil
	}
	m, e := exactMembers(raw)
	if e != nil {
		return e
	}
	draftCase := hasColumnCase(value.Mapping)
	if value.Import != nil {
		var published InputMapping
		if strictJSON(value.Import.Dialect, &published) == nil {
			draftCase = draftCase || hasColumnCase(published)
		}
	}
	for k, v := range m {
		if strings.EqualFold(k, "conversionContract") && (k != "conversionContract" || bytes.Equal(bytes.TrimSpace(v), []byte("null"))) {
			return errors.New("invalid conversionContract member")
		}
		// Preserve historical ordinary decoding; close the new Draft custody slot.
		if draftCase && strings.EqualFold(k, "case") && k != "case" {
			return errors.New("invalid Draft case custody member spelling")
		}
		if strings.EqualFold(k, "retainedCaseJson") {
			if k != "retainedCaseJson" || m["case"] != nil || value.RetainedCaseJSON == "" || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return errors.New("invalid retainedCaseJson custody variant")
			}
		}
	}
	*c = SourceInput(value)
	if c.RetainedCaseJSON != "" {
		if _, e := columnCaseBytes(c); e != nil {
			return e
		}
	}
	return nil
}
