package dialect4

// RuleCounts measures a rule as written, before any form is checked
// (spec/dialect.md §11's third phase, §2.2.7): it reports whether the rule
// has more than 16 derived values, more than 64 clauses, more than 64
// conditions listed by one all or any, or a literal of more than 8192
// bytes, and adds the targets its claims name to facts and evidence, for
// §12's counts of targets. A member not of its form is not counted; the
// form is checked later.
func RuleCounts(rule any, facts, evidence map[string]bool) bool {
	r, ok := rule.(map[string]any)
	if !ok {
		return false
	}
	if d, ok := r["derived"].([]any); ok && len(d) > MaxDerived {
		return true
	}
	clauses, _ := r["clauses"].([]any)
	if len(clauses) > maxClauses {
		return true
	}
	for _, c := range clauses {
		clause, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if listedOver(clause["when"]) {
			return true
		}
		claim, _ := clause["claim"].(map[string]any)
		entries, _ := claim["facts"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if pointer, ok := entry["pointer"].(string); ok {
				facts[pointer] = true
			}
			if literal, ok := entry["value"].(string); ok && len(literal) > MaxLiteralBytes {
				return true
			}
		}
		if ev, ok := claim["evidence"].(map[string]any); ok {
			for requirement := range ev {
				evidence[requirement] = true
			}
		}
	}
	return false
}

// RuleCountsAdded is what RuleCounts measures that format 4.0.0 adds to a
// source's rule, which the Runner's count of format 3.0.0 measures the rest
// of: its derived values and its literals.
func RuleCountsAdded(rule any) bool {
	r, ok := rule.(map[string]any)
	if !ok {
		return false
	}
	if d, ok := r["derived"].([]any); ok && len(d) > MaxDerived {
		return true
	}
	clauses, _ := r["clauses"].([]any)
	for _, c := range clauses {
		clause, _ := c.(map[string]any)
		claim, _ := clause["claim"].(map[string]any)
		entries, _ := claim["facts"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if literal, ok := entry["value"].(string); ok && len(literal) > MaxLiteralBytes {
				return true
			}
		}
	}
	return false
}

func listedOver(v any) bool {
	c, _ := v.(map[string]any)
	switch c["op"] {
	case "all", "any":
		of, _ := c["of"].([]any)
		if len(of) > maxListed {
			return true
		}
		for _, x := range of {
			if listedOver(x) {
				return true
			}
		}
	case "not":
		return listedOver(c["of"])
	}
	return false
}
