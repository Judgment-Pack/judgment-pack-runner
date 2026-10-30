package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PlannedSource is an instruction to acquire, not permission to evaluate.
// The endpoint is read-only: the caller fetches and submits receipts for checking.
type PlannedSource struct {
	RetainedColumn     bool           `json:"retainedColumn,omitempty"`
	ConversionLimits   map[string]int `json:"conversionLimits,omitempty"`
	retainResponse     bool
	ConversionContract *ConversionContract `json:"conversionContract,omitempty"`
	Name               string              `json:"name"`
	Kind               string              `json:"kind"`
	Provider           string              `json:"provider,omitempty"`
	Profile            string              `json:"profile,omitempty"`
	Source             string              `json:"source,omitempty"`
	Arguments          json.RawMessage     `json:"arguments,omitempty"`
}
type InputPlan struct {
	Next         *PlannedSource `json:"next,omitempty"`
	Input        *Input         `json:"input,omitempty"`
	FactsText    string         `json:"factsText,omitempty"`
	EvidenceText string         `json:"evidenceText,omitempty"`
}
type acquisitionNeeded struct{ PlannedSource }

func (e *acquisitionNeeded) Error() string { return "source acquisition required" }
func nextInput(input Input, profiles []InputProfile, at time.Time) (InputPlan, error) {
	// Planning accepts source-only input. Derived values must be recomputed.
	if len(input.Facts) > 0 || len(input.Evidence) > 0 || input.Preparation != nil {
		return InputPlan{}, inputProblem("planning accepts source-only input")
	}
	mapped, err := normalizeV2Mode(input, profiles, at, true)
	var needed *acquisitionNeeded
	if errors.As(err, &needed) {
		needed.PlannedSource.ConversionContract = inputConversion(input)
		needed.RetainedColumn = needed.retainResponse && inputConversion(input) != nil
		if needed.RetainedColumn {
			for _, p := range profiles {
				if p.ID == needed.Profile {
					needed.ConversionLimits, err = conversionCapabilityLimits(p.ConversionContracts, "producer-carrier", true)
					if err != nil {
						return InputPlan{}, err
					}
				}
			}
		}
		return InputPlan{Next: &needed.PlannedSource}, nil
	}
	if err != nil {
		return InputPlan{}, err
	}
	return InputPlan{Input: &mapped, FactsText: inputJSONText(mapped.Facts), EvidenceText: inputJSONText(mapped.Evidence)}, nil
}

// Check every source before the first acquisition, including downstream model
// influence and fixed tool permissions. A late invalid source must not trigger
// needless external calls to earlier sources.
func preflightV2(m InputMapping, profiles []InputProfile) error {
	return preflightPreparation(m, profiles, true)
}

// Offline verification defers admission until after retained acquisitions; live
// planning still rejects it before any source can be called.
func preflightPreparation(m InputMapping, profiles []InputProfile, admission bool) error {
	generated := map[string]bool{}
	check := func(read SourceRead, class string, gen bool) error {
		facts, evidence, err := readTargets(read)
		if err != nil {
			return err
		}
		for _, f := range facts {
			if admission && !admitted(m, "facts", f.Target, class, gen) {
				return withCode(codeAdmission, errors.New("fact target does not admit source class or generated dependency"))
			}
		}
		for _, e := range evidence {
			if admission && !admitted(m, "evidence", e.Requirement, class, gen) {
				return withCode(codeAdmission, errors.New("evidence target does not admit source class or generated dependency"))
			}
		}
		return nil
	}
	if m.Case != nil {
		if err := check(SourceRead{Copy: &CopyMapping{m.Case.Facts, m.Case.Evidence}}, "asserted", false); err != nil {
			return err
		}
	}
	for _, source := range m.Sources {
		class := "asserted"
		if isResearch(source) {
			// A research source's AI profile and web profiles, each of
			// class generated, declaring the capability it serves and the
			// source's model (research.go): profile-mismatch otherwise.
			recipe, err := readResearchRecipe(source, nil, false)
			if err != nil {
				return err
			}
			if _, err = researchProfiles(source, recipe, profiles); err != nil {
				return err
			}
			class = classGenerated
		}
		if source.Kind == "operation" || source.Provider == "google-drive" {
			profile, err := profileFor(source, profiles)
			if err != nil {
				return err
			}
			class = profile.Class
			// A calculator's answer is held to a binding, and a binding is
			// held only to a calculator's answer: neither stands alone.
			if (profile.Calculator != nil) != (source.Calculation != nil) {
				return withCode(codeMismatch, fmt.Errorf("source %s: a calculator's profile and a calculation binding go together", source.Name))
			}
			if err := profileRequestForm(source, profile, m); err != nil {
				return err
			}
		}
		gen := class == "generated"
		for _, p := range source.Parameters {
			gen = gen || generated[p.From]
		}
		generated[source.Name] = gen
		if err := check(source.Read, class, gen); err != nil {
			if isResearch(source) {
				return researchFail(codeAdmission, "source %s: %v", source.Name, err)
			}
			return fmt.Errorf("source %s: %w", source.Name, err)
		}
	}
	return nil
}

// profileRequestForm holds an operation's request to what its profile allows
// (spec/dialect.md §4.1): under a profile of shape mcp, a tool call the
// profile allows; under one whose adapter is adapter-http, a query, when
// present, of string templates. A request to a source of another shape or
// adapter is held to the template rules alone (adapterShapes). The same check
// holds before acquisition, at preparation, and when a saved release is
// frozen into a workflow, an unused stage included.
func profileRequestForm(source MappingSource, profile InputProfile, m InputMapping) error {
	if source.Kind != "operation" {
		return nil
	}
	if profile.Shape == "mcp" {
		if err := mcpToolCall(source, profile); err != nil {
			return withCode(codeMismatch, err)
		}
	}
	if profile.Adapter.Name == "adapter-http" {
		if err := adapterHTTPQuery(source, m); err != nil {
			return withCode(codeMismatch, err)
		}
	}
	return nil
}

// adapterHTTPQuery holds an adapter-http request's query to §4.1: when
// present, an object, not a template form, each of whose members is a string
// template. The rule is adapter-http's, by the profile's adapter.name, and not
// every http-shaped adapter's. The dialect's validation has already held the
// request to the template rules; here the parameters' types decide.
func adapterHTTPQuery(source MappingSource, m InputMapping) error {
	v, err := readInputJSON(source.Arguments)
	if err != nil {
		return err
	}
	request, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("source %s: operation arguments must be an object", source.Name)
	}
	v, present := request["query"]
	if !present {
		return nil
	}
	query, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("source %s: adapter-http query must be an object of string templates", source.Name)
	}
	if _, parameter := query["$param"]; parameter {
		return fmt.Errorf("source %s: adapter-http query must not be a template form", source.Name)
	}
	if _, text := query["$text"]; text {
		return fmt.Errorf("source %s: adapter-http query must not be a template form", source.Name)
	}
	scope := map[string]string{}
	if m.Case != nil {
		for name, parameter := range m.Case.Parameters {
			scope[name] = parameter.Type
		}
	}
	for name, parameter := range source.Parameters {
		scope[name] = parameter.Type
	}
	for name, value := range query {
		if !stringTemplate(value, scope) {
			return fmt.Errorf("source %s: adapter-http query member %s must be a string template", source.Name, name)
		}
	}
	return nil
}
