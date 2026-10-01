package runner

import "encoding/json"

// UnsignedParameters names, for one acquired source, the parameters its rule
// reads that no signed request commits, and how many fact and evidence targets
// the source maps, with a value or without. A receipt commits to the request
// the source sent, so a parameter a request template refers to is signed; one
// that only a rule reads is not, and changing it in an export can change the
// source's targets without failing verification.
type UnsignedParameters struct {
	Source     string              `json:"source"`
	Parameters []UnsignedParameter `json:"parameters"`
	Targets    int                 `json:"targets"`
}

// UnsignedParameter is one such parameter and where its value comes from:
// "case", the case as the operator supplied it; "runAt", the export's own
// verification time; or "local-file", a fact of an earlier local-file source.
// A fact of an earlier acquired source is derived from that source's signed
// response, and is not listed: what that source's own rule reads is listed for
// that source.
type UnsignedParameter struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Operator reports whether the parameter's value is the operator's own: from
// the case or a local file. runAt is the export's verification time.
func (p UnsignedParameter) Operator() bool { return p.Kind != "runAt" }

// unsignedParameters lists, in the mapping's order of sources, each acquired
// source with targets whose rule reads a parameter that no signed request
// commits. The parameters are worked out from the frozen mapping's request
// templates and rules. A source was acquired in this run when its outcome
// retains the request it sent; a skipped source's rule read nothing.
//
// A case parameter is committed when the request of any acquired source refers
// to it: changing it changes that request, and its receipt then fails. A
// source's own parameter is committed only by its own request. runAt never is.
func unsignedParameters(m InputMapping, p *Preparation) []UnsignedParameters {
	acquired := map[string]bool{}
	for _, o := range p.Outcomes {
		acquired[o.Name] = len(o.Arguments) > 0
	}
	local := map[string]bool{}
	requests := map[string]map[string]bool{}
	signed := map[string]bool{}
	for _, s := range m.Sources {
		local[s.Name] = s.Kind != "operation" && s.Provider != "google-drive"
		template, _ := readInputJSON(s.Arguments)
		requests[s.Name] = map[string]bool{}
		templateParameters(template, requests[s.Name])
		// A source's own parameter cannot share a case parameter's name, so
		// only case parameters are looked up here.
		for n := range requests[s.Name] {
			signed[n] = signed[n] || acquired[s.Name]
		}
	}
	var out []UnsignedParameters
	for _, s := range m.Sources {
		var rule ruleDocument
		if !acquired[s.Name] || len(s.Read.Rule) == 0 || json.Unmarshal(s.Read.Rule, &rule) != nil {
			continue
		}
		var ps []UnsignedParameter
		for _, n := range sortedKeys(rule.Parameters) {
			kind := "case"
			if own, ok := s.Parameters[n]; ok {
				if !local[own.From] || requests[s.Name][n] {
					continue
				}
				kind = "local-file"
			} else if n == "runAt" {
				kind = "runAt"
			} else if signed[n] {
				continue
			}
			ps = append(ps, UnsignedParameter{n, kind})
		}
		targets := 0
		for _, l := range p.Lineage {
			if l.Source == s.Name {
				targets++
			}
		}
		if len(ps) > 0 && targets > 0 {
			out = append(out, UnsignedParameters{s.Name, ps, targets})
		}
	}
	return out
}

// templateParameters adds to into each parameter a request template refers to,
// as a whole value ($param) or in text ($text), as instantiate reads them.
func templateParameters(v any, into map[string]bool) {
	switch o := v.(type) {
	case map[string]any:
		if n, ok := o["$param"].(string); ok && len(o) == 1 {
			into[n] = true
			return
		}
		if s, ok := o["$text"].(string); ok && len(o) == 1 {
			for _, m := range textParameter.FindAllStringSubmatch(s, -1) {
				into[m[1]] = true
			}
			return
		}
		for _, x := range o {
			templateParameters(x, into)
		}
	case []any:
		for _, x := range o {
			templateParameters(x, into)
		}
	}
}
