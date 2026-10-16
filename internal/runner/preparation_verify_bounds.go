package runner

// Count limits before checking forms: malformed siblings must not mask a
// measurable bound. These are the portable Dialect §12 limits.
func preparationCountBounds(dialect, acquisitions, profiles, binding any) bool {
	object := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	list := func(v any) []any { a, _ := v.([]any); return a }
	text := func(v any) string { s, _ := v.(string); return s }
	long := func(v any, n int) bool { s, ok := v.(string); return ok && len(s) > n }
	d, acq := object(dialect), object(acquisitions)
	facts, evidence := map[string]bool{}, map[string]bool{}
	copyTargets := func(v any) {
		m := object(v)
		for _, f := range list(m["facts"]) {
			if s, ok := object(f)["target"].(string); ok {
				facts[s] = true
			}
		}
		for _, f := range list(m["evidence"]) {
			if s, ok := object(f)["requirement"].(string); ok {
				evidence[s] = true
			}
		}
	}
	var listed func(any) bool
	listed = func(v any) bool {
		c := object(v)
		switch c["op"] {
		case "all", "any":
			a := list(c["of"])
			if len(a) > 64 {
				return true
			}
			for _, x := range a {
				if listed(x) {
					return true
				}
			}
		case "not":
			return listed(c["of"])
		}
		return false
	}
	caseDoc := object(d["case"])
	countCase := len(object(caseDoc["parameters"]))
	if countCase+1 > 64 {
		return true
	}
	copyTargets(caseDoc)
	sources := list(d["sources"])
	if len(sources) > 16 {
		return true
	}
	declared := map[string]map[string]string{}
	for _, value := range sources {
		s := object(value)
		if countCase+len(object(s["parameters"]))+1 > 64 {
			return true
		}
		read := object(s["read"])
		if len(list(read["unwrap"])) > 4 {
			return true
		}
		copyTargets(read["copy"])
		clauses := list(object(read["rule"])["clauses"])
		if len(clauses) > 64 {
			return true
		}
		for _, value := range clauses {
			c := object(value)
			if listed(c["when"]) {
				return true
			}
			claim := object(c["claim"])
			for _, f := range list(claim["facts"]) {
				if key, ok := object(f)["pointer"].(string); ok {
					facts[key] = true
				}
			}
			for key := range object(claim["evidence"]) {
				evidence[key] = true
			}
		}
		if s["kind"] != "research" {
			continue
		}
		fields := list(s["fields"])
		if len(fields) > 64 || long(s["model"], 256) || len(list(s["capabilities"])) > 3 || len(object(s["webProfiles"])) > 2 || len(list(read["sources"])) > 15 {
			return true
		}
		for _, e := range list(s["evidence"]) {
			if long(e, 4096) {
				return true
			}
		}
		types := map[string]string{}
		for _, value := range fields {
			f := object(value)
			if long(f["name"], 64) || long(f["description"], 2048) {
				return true
			}
			// Only a declaration whose name is a string types its answers:
			// the reader skips any other, and a run's recipe, which
			// researchRecordBounds measures a record by, refuses it.
			name, named := f["name"].(string)
			if _, ok := types[name]; named && !ok {
				switch kind := text(f["type"]); kind {
				case "text", "decimal", "boolean", "integer", "range", "text-list":
					types[name] = kind
				}
			}
		}
		if name, ok := s["name"].(string); ok {
			if _, seen := declared[name]; !seen {
				declared[name] = types
			}
		}
	}
	if len(facts) > 256 || len(evidence) > 128 {
		return true
	}
	for _, value := range list(profiles) {
		r := object(object(value)["research"])
		if len(list(r["capabilities"])) > 3 || long(r["model"], 256) {
			return true
		}
	}
	for name, value := range object(object(binding)["sources"]) {
		if _, ok := declared[name]; ok && len(object(object(value)["webProfiles"])) > 2 {
			return true
		}
	}
	var depth func(any) int
	depth = func(v any) int {
		n := 0
		switch x := v.(type) {
		case map[string]any:
			for _, y := range x {
				if k := depth(y); k > n {
					n = k
				}
			}
			return n + 1
		case []any:
			for _, y := range x {
				if k := depth(y); k > n {
					n = k
				}
			}
			return n + 1
		}
		return 0
	}
	for name, value := range acq {
		result := object(object(value)["response"])["result"]
		if depth(result) > 32 {
			return true
		}
		types, research := declared[name]
		if !research {
			continue
		}
		record := object(result)
		if len(list(record["responses"])) > 64 {
			return true
		}
		answers := object(record["fields"])
		if len(answers) > 64 {
			return true
		}
		for field, value := range answers {
			if len(field) > 64 {
				return true
			}
			a := object(value)
			// Each value by its field's declared type, as a live research
			// record is measured (valueOver): a decimal and a range endpoint
			// in characters, a text and a list item in bytes.
			if len(list(a["cites"])) > 64 || valueOver(a["value"], types[field]) || len(list(a["values"])) > 64 {
				return true
			}
			for _, value := range list(a["values"]) {
				o := object(value)
				if len(list(o["cites"])) > 64 || valueOver(o["value"], types[field]) {
					return true
				}
			}
		}
	}
	return false
}
