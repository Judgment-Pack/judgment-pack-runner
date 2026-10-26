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
			// An MCP operation's request is a tool call its profile allows.
			// A request to a source of another shape is held to the
			// template rules alone (adapterShapes).
			if source.Kind == "operation" && profile.Shape == "mcp" {
				if err := mcpToolCall(source, profile); err != nil {
					return withCode(codeMismatch, err)
				}
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
