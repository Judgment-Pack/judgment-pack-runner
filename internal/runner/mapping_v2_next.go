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
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Provider  string          `json:"provider,omitempty"`
	Profile   string          `json:"profile,omitempty"`
	Source    string          `json:"source,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
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
	generated := map[string]bool{}
	check := func(read SourceRead, class string, gen bool) error {
		facts, evidence, err := readTargets(read)
		if err != nil {
			return err
		}
		for _, f := range facts {
			if !admitted(m, "facts", f.Target, class, gen) {
				return errors.New("fact target does not admit source class or generated dependency")
			}
		}
		for _, e := range evidence {
			if !admitted(m, "evidence", e.Requirement, class, gen) {
				return errors.New("evidence target does not admit source class or generated dependency")
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
		if source.Kind == "operation" || source.Provider == "google-drive" {
			profile, err := profileFor(source, profiles)
			if err != nil {
				return err
			}
			class = profile.Class
			if source.Kind == "operation" {
				var request struct {
					Tool string `json:"tool"`
				}
				if json.Unmarshal(source.Arguments, &request) != nil {
					return errors.New("MCP request must name a fixed tool")
				}
				allowed := false
				for _, tool := range profile.Tools {
					allowed = allowed || tool == request.Tool
				}
				if !allowed {
					return fmt.Errorf("source %s: tool is not allowed by its trusted profile", source.Name)
				}
			}
		}
		gen := class == "generated"
		for _, p := range source.Parameters {
			gen = gen || generated[p.From]
		}
		generated[source.Name] = gen
		if err := check(source.Read, class, gen); err != nil {
			return fmt.Errorf("source %s: %w", source.Name, err)
		}
	}
	return nil
}
