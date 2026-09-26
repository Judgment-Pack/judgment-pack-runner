package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

func profilesUsed(m InputMapping, profiles []InputProfile) []InputProfile {
	used := map[string]bool{}
	for _, s := range m.Sources {
		used[s.Profile] = true
	}
	out := []InputProfile{}
	for _, p := range profiles {
		if used[p.ID] {
			out = append(out, p)
		}
	}
	return out
}
func (s *Service) checkReleaseProfiles(r Release) error {
	if r.InputMapping == nil || r.InputMapping.Version != 2 {
		return nil
	}
	for _, source := range r.InputMapping.Sources {
		if source.Profile != "" {
			p, e := profileFor(source, s.cfg.InputProfiles)
			if e != nil {
				return inputProblem(e.Error())
			}
			old, e := profileFor(source, r.InputProfiles)
			if e != nil || profileHash(p) != profileHash(old) {
				return inputProblem("release trust configuration changed")
			}
		}
	}
	return nil
}
func (s *Service) checkMappingCoverage(ctx context.Context, r Release, dir string) ([]string, error) {
	b, e := invoke(ctx, s.runtime, dir, "mapping-coverage", "packs", "list", "--config", "jpack.json", "--format", "json")
	if e != nil {
		return nil, e
	}
	var inv struct {
		Version string `json:"outputVersion"`
		Packs   []struct {
			ID       string   `json:"id"`
			PackID   string   `json:"packId"`
			Paths    []string `json:"consultedFactPaths"`
			Evidence []string `json:"evidenceRequirements"`
			Detail   string   `json:"detail"`
		} `json:"packs"`
	}
	if json.Unmarshal(b, &inv) != nil || inv.Version != "2" || len(inv.Packs) != 1 || inv.Packs[0].ID != "target" || inv.Packs[0].PackID != r.PackID || inv.Packs[0].Paths == nil || inv.Packs[0].Evidence == nil || inv.Packs[0].Detail != "" {
		return nil, errors.New("unsupported mapping coverage inventory")
	}
	return mappingCoverage(*r.InputMapping, inv.Packs[0].Paths, inv.Packs[0].Evidence)
}
func mappingCoverage(m InputMapping, paths, requirements []string) ([]string, error) {
	targets := []string{}
	mappedEvidence := map[string]bool{}
	if m.Case != nil {
		for _, f := range m.Case.Facts {
			targets = append(targets, f.Target)
		}
		for _, e := range m.Case.Evidence {
			mappedEvidence[e.Requirement] = true
		}
	}
	for _, s := range m.Sources {
		facts, ev, e := readTargets(s.Read)
		if e != nil {
			return nil, e
		}
		for _, f := range facts {
			targets = append(targets, f.Target)
		}
		for _, x := range ev {
			mappedEvidence[x.Requirement] = true
		}
	}
	warnings := []string{"Fact coverage is a static over-approximation, not proof of runtime reads or of an unknown result."}
	for _, p := range paths {
		if _, e := pointerTokens(p, true); e != nil {
			return nil, inputProblem("inventory contains an invalid fact pointer")
		}
		covered := false
		partial := false
		for _, t := range targets {
			covered = covered || pointerCovers(t, p)
			partial = partial || pointerCovers(p, t)
		}
		for _, u := range m.Unmapped {
			covered = covered || pointerCovers(u, p)
		}
		if p == "" || !covered && partial {
			warnings = append(warnings, fmt.Sprintf("Aggregate fact pointer %q includes the projected object; review partial coverage.", p))
			continue
		}
		if !covered {
			return nil, inputProblem(fmt.Sprintf("Map or explicitly acknowledge fact pointer %q before release.", p))
		}
	}
	for _, t := range targets {
		found := false
		for _, p := range paths {
			found = found || pointerCovers(t, p) || pointerCovers(p, t)
		}
		if !found {
			warnings = append(warnings, "Mapped fact is not listed in the pack inventory: "+t)
		}
	}
	required := map[string]bool{}
	for _, r := range requirements {
		required[r] = true
	}
	for r := range mappedEvidence {
		if !required[r] {
			return nil, inputProblem("Mapped evidence is not declared by the pack: " + r)
		}
	}
	for _, r := range m.UnmappedEvidence {
		if !required[r] {
			return nil, inputProblem("Evidence omission is not declared by the pack: " + r)
		}
		delete(required, r)
	}
	for r := range required {
		if !mappedEvidence[r] {
			return nil, inputProblem("Map or explicitly acknowledge evidence requirement: " + r)
		}
	}
	for _, s := range m.Sources {
		if s.Kind == "operation" {
			warnings = append(warnings, "Review every successful branch for subject binding and expected row count: "+s.Name)
		}
	}
	return warnings, nil
}
