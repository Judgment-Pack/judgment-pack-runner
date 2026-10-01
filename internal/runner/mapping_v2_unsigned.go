package runner

import "encoding/json"

// UnsignedParameters names, for one acquired source, the parameters its rule
// reads that its own receipt does not commit, and how many fact and evidence
// targets the source maps, with a value or without. Changing such a parameter
// in an export can change the source's targets without failing verification.
type UnsignedParameters struct {
	Source     string              `json:"source"`
	Parameters []UnsignedParameter `json:"parameters"`
	Targets    int                 `json:"targets"`
}

// UnsignedParameter is one such parameter, and why it is not signed:
//
//   - "case": a case parameter, as the operator supplied it;
//   - "local-file": a fact of an earlier local-file source, which is asserted;
//   - "upstream": a fact of an earlier acquired source whose own rule read a
//     parameter of one of these kinds;
//   - "ambiguous-text": one of those, which the request carries only in text
//     that names another parameter;
//   - "runAt": the export's own verification time;
//   - "upstream-runAt": a fact of an earlier acquired source whose own rule
//     read runAt, and no parameter the operator supplied.
type UnsignedParameter struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Operator reports whether the parameter rests on what the operator supplied,
// rather than only on the export's verification time.
func (p UnsignedParameter) Operator() bool { return p.Kind != "runAt" && p.Kind != "upstream-runAt" }

// unsignedParameters lists, in the mapping's order of sources, each acquired
// source with targets whose rule read a parameter that its own receipt does not
// commit. It is worked out from the frozen mapping and this run's outcomes, and
// the stored lineage is unchanged.
//
// A source's receipt commits a parameter its request carries unambiguously: as
// a whole value ($param), or in $text that names no other parameter. Text that
// names one parameter, however often, renders different values differently;
// text that names two need not: {{a}}{{b}} renders 1 and 23 as 12 and 3. A
// calculator's receipt also commits each parameter its calculation binds, whose
// echo in the signed answer must equal it. Another source's receipt commits
// nothing for this one.
//
// A rule reads the parameters its conditions name. A source was acquired when
// its outcome retains the request it sent. A skipped source, and a calculator
// that did not compute, whose rule was not applied, read nothing; a source
// without targets has no facts, and influences nothing.
func unsignedParameters(m InputMapping, p *Preparation) []UnsignedParameters {
	acquired := map[string]bool{}
	for _, o := range p.Outcomes {
		acquired[o.Name] = len(o.Arguments) > 0
	}
	targets := map[string]int{}
	applied := map[string]bool{}
	for _, l := range p.Lineage {
		targets[l.Source]++
		applied[l.Source] = l.Calculation == nil || l.Calculation.Status == calculationComputed
	}
	// A source without targets is not in the lineage, and is not applied here.
	local := map[string]bool{}
	// taint is "operator" or "clock" for a source whose rule read an unsigned
	// parameter: its facts carry that parameter's influence to later sources.
	taint := map[string]string{}
	var out []UnsignedParameters
	for _, s := range m.Sources {
		local[s.Name] = s.Kind != "operation" && s.Provider != "google-drive"
		var rule ruleDocument
		if local[s.Name] || !acquired[s.Name] || !applied[s.Name] || len(s.Read.Rule) == 0 || json.Unmarshal(s.Read.Rule, &rule) != nil {
			continue
		}
		reads := map[string]bool{}
		for _, c := range rule.Clauses {
			conditionParameters(c.When, reads)
		}
		committed, ambiguous := map[string]bool{}, map[string]bool{}
		template, _ := readInputJSON(s.Arguments)
		requestParameters(template, committed, ambiguous)
		if s.Calculation != nil {
			for _, n := range s.Calculation.Inputs {
				committed[n] = true
			}
		}
		var ps []UnsignedParameter
		byOperator := false
		for _, n := range sortedKeys(reads) {
			if committed[n] {
				continue
			}
			kind, operator := "case", true
			if own, ok := s.Parameters[n]; ok {
				switch {
				case local[own.From]:
					kind = "local-file"
				case taint[own.From] == "operator":
					kind = "upstream"
				case taint[own.From] == "clock":
					kind, operator = "upstream-runAt", false
				default:
					continue
				}
			} else if n == "runAt" {
				kind, operator = "runAt", false
			}
			if operator && ambiguous[n] {
				kind = "ambiguous-text"
			}
			byOperator = byOperator || operator
			ps = append(ps, UnsignedParameter{n, kind})
		}
		if len(ps) == 0 {
			continue
		}
		taint[s.Name] = "clock"
		if byOperator {
			taint[s.Name] = "operator"
		}
		out = append(out, UnsignedParameters{s.Name, ps, targets[s.Name]})
	}
	return out
}

// conditionParameters adds to into each parameter a rule condition names:
// equalsParam's param, and freshWithin's asOf and maxAge, at any depth of not,
// all and any.
func conditionParameters(raw json.RawMessage, into map[string]bool) {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil {
		return
	}
	var op string
	_ = json.Unmarshal(o["op"], &op)
	name := func(member string) {
		var n string
		if json.Unmarshal(o[member], &n) == nil {
			into[n] = true
		}
	}
	switch op {
	case "equalsParam":
		name("param")
	case "freshWithin":
		name("asOf")
		name("maxAge")
	case "not":
		conditionParameters(o["of"], into)
	case "all", "any":
		var of []json.RawMessage
		_ = json.Unmarshal(o["of"], &of)
		for _, c := range of {
			conditionParameters(c, into)
		}
	}
}

// requestParameters sorts the parameters a request template refers to, as
// instantiate reads them: into committed, those it carries unambiguously, as a
// whole value or in text that names no other; into ambiguous, those it carries
// only in text that names another.
func requestParameters(v any, committed, ambiguous map[string]bool) {
	switch o := v.(type) {
	case map[string]any:
		if n, ok := o["$param"].(string); ok && len(o) == 1 {
			committed[n] = true
			return
		}
		if s, ok := o["$text"].(string); ok && len(o) == 1 {
			names := map[string]bool{}
			for _, m := range textParameter.FindAllStringSubmatch(s, -1) {
				names[m[1]] = true
			}
			for n := range names {
				if len(names) == 1 {
					committed[n] = true
				} else {
					ambiguous[n] = true
				}
			}
			return
		}
		for _, x := range o {
			requestParameters(x, committed, ambiguous)
		}
	case []any:
		for _, x := range o {
			requestParameters(x, committed, ambiguous)
		}
	}
}
