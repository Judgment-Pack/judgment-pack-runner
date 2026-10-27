package runner

import "encoding/json"

// Only Base is executable in this intermediate Draft integration. It reuses
// the genuine complete-artifact limits; retained strings are not canon claims.
func columnProfileBytes(raw []byte) string {
	m, e := exactMembers(raw)
	if e != nil {
		return ""
	} // ordinary form reading reports this
	v, exists := m["profile"]
	if !exists {
		return ""
	}
	var p string
	if json.Unmarshal(v, &p) != nil {
		return codeMalformed
	}
	switch p {
	case "base":
		return ""
	case "extended":
		return "profile-unsupported"
	}
	return codeMalformed
}
func columnInputBounds(c *SourceInput) error {
	if !hasColumnMapping(c.Mapping) {
		return nil
	}
	if e := c.ConversionContract.validate("preparation"); e != nil {
		return e
	}
	limit := MaxBody
	if c.ConversionContract != nil {
		limit = columnCombinedLimit
		if len(encode(c.Mapping)) > columnDocumentLimit || c.Import != nil && (len(c.Import.Dialect) > columnDocumentLimit || len(c.Import.Binding) > maxBindingDocument) {
			return codedProblem(codeOverBound, "retained-column document exceeds selected limit")
		}
	}
	if c.ConversionContract != nil {
		counted := *c
		if hasColumnCase(c.Mapping) && counted.RetainedCaseJSON == "" {
			raw := counted.Case
			if len(raw) == 0 {
				raw = json.RawMessage(`{}`)
			}
			retainColumnCase(&counted, raw)
		}
		if len(encode(counted)) > limit {
			return codedProblem(codeOverBound, "retained-column complete source exceeds selected combined limit")
		}
		return nil
	}
	sources := encode(c.Sources)
	if c.Sources == nil {
		sources = []byte(`{}`)
	}
	documents := len(encode(c.Mapping))
	if c.Import != nil {
		documents += len(c.Import.Dialect) + len(c.Import.Binding)
	}
	if documents+columnCaseSlotBytes(c)+len(sources) > limit {
		return codedProblem(codeOverBound, "retained-column complete dialect, case and acquisitions exceed base limit")
	}
	return nil
}
