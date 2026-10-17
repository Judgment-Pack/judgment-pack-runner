package runner

// Draft workflow writer admission. The caller supplies releases already checked
// by Runner's normal Runtime validation/preview path; this does not replace Core
// validation or act as the offline workflow verifier. It performs no acquisition.
import (
	"bytes"
	"encoding/json"
	"errors"
)

var errWorkflowBinding = errors.New("workflow stage artifacts do not match their frozen pins or admission")

type workflowFrozenStage struct {
	Stage                  string  `json:"stage"`
	PublishedDialectDigest string  `json:"publishedDialectDigest"`
	BindingDocumentDigest  *string `json:"bindingDocumentDigest"`
	BoundDialectDigest     string  `json:"boundDialectDigest"`
	PackDigest             string  `json:"packDigest"`
	ReleaseDigest          string  `json:"releaseDigest"`
	RuntimeDigest          string  `json:"runtimeDigest"`
}
type workflowFreezeInput struct {
	Published, Binding []byte
	Release            Release
}
type workflowStageSnapshot struct {
	Frozen                          workflowFrozenStage
	Published, Binding, Bound, Pack []byte
	Release                         []byte
}

// freezeWorkflowStages checks all stages before returning any snapshot. Profile
// trust comes only from the independent installation input, never the release.
// Retained bytes are owned copies so caller mutation cannot change admitted work.
func freezeWorkflowStages(raw []byte, inputs []workflowFreezeInput, profiles []InputProfile, runtimeDigest string) ([]workflowStageSnapshot, error) {
	d, _, err := readWorkflow(raw)
	if err != nil {
		return nil, err
	}
	if !workflowDigest.MatchString(runtimeDigest) || len(inputs) != len(d.Stages) {
		return nil, errWorkflowBinding
	}
	if err := validateProfiles(profiles); err != nil {
		return nil, err
	}
	out := make([]workflowStageSnapshot, 0, len(inputs))
	for i, stage := range d.Stages {
		snapshot, err := freezeWorkflowStage(stage, inputs[i], profiles, runtimeDigest)
		if err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, nil
}

func freezeWorkflowStage(stage workflowStage, in workflowFreezeInput, profiles []InputProfile, runtimeDigest string) (workflowStageSnapshot, error) {
	var out workflowStageSnapshot
	if len(in.Published) > maxPublishedDialect || len(in.Binding) > maxBindingDocument {
		return out, errWorkflowBound
	}
	var published InputMapping
	if err := strictJSON(in.Published, &published); err != nil {
		return out, err
	}
	if err := exactDialectForm(in.Published, published); err != nil {
		return out, err
	}
	if err := validateDialect(published, true); err != nil {
		return out, err
	}
	canon, err := proofCanon(in.Published)
	if err != nil {
		return out, err
	}
	if stage.Dialect.Digest != digest(canon) || !workflowDialectMatches(stage.Dialect, published) {
		return out, errWorkflowBinding
	}

	bound := published
	var bindingDigest *string
	unbound := false
	for _, source := range published.Sources {
		unbound = unbound || unboundOperation(source) || unboundResearch(source)
	}
	if unbound {
		bound, err = bindDialect(DialectImport{Dialect: in.Published, Binding: in.Binding}, profiles)
		if err != nil {
			return out, err
		}
		binding, err := proofCanon(in.Binding)
		if err != nil {
			return out, err
		}
		pin := digest(binding)
		bindingDigest = &pin
	} else if len(in.Binding) != 0 {
		return out, errWorkflowBinding
	}
	if !unbound && mappingV2Hash(bound) != stage.Dialect.Digest {
		return out, errWorkflowBinding
	}
	if err := validateMappingV2(bound); err != nil {
		return out, err
	}
	if err := workflowSourceAdmission(bound, profiles); err != nil {
		return out, err
	}

	// Match the existing release's typed encoding exactly, including named
	// identity. A matching version label is never sufficient.
	r := in.Release
	if r.SchemaVersion != "1" || r.RuntimeDigest != runtimeDigest || r.InputMapping == nil || mappingV2Hash(*r.InputMapping) != mappingV2Hash(bound) || r.PackDigest != stage.Pack.Digest || digest([]byte(r.Pack)) != stage.Pack.Digest || r.PackID != stage.Pack.ID {
		return out, errWorkflowBinding
	}
	if r.Sample.Source == nil || mappingV2Hash(r.Sample.Source.Mapping) != mappingV2Hash(bound) {
		return out, errWorkflowBinding
	}
	if unbound {
		imp := r.Sample.Source.Import
		if imp == nil || !sameWorkflowCanonical(imp.Dialect, in.Published) || !sameWorkflowCanonical(imp.Binding, in.Binding) {
			return out, errWorkflowBinding
		}
	} else if r.Sample.Source.Import != nil {
		return out, errWorkflowBinding
	}
	identity, err := releaseDialectIdentity(r)
	if err != nil {
		return out, err
	}
	if (identity == nil) != (r.Dialect == nil) || identity != nil && *identity != *r.Dialect {
		return out, errWorkflowBinding
	}
	for _, source := range bound.Sources {
		for _, ref := range sourceProfileRefs(source) {
			trusted, err := profileFor(MappingSource{Profile: ref.ID, ProfileDigest: ref.Digest}, profiles)
			if err != nil {
				return out, err
			}
			retained, err := profileFor(MappingSource{Profile: ref.ID, ProfileDigest: ref.Digest}, r.InputProfiles)
			if err != nil || profileHash(trusted) != profileHash(retained) {
				return out, errWorkflowBinding
			}
		}
	}
	// Core is validated by the existing release path. Here only the id and
	// declared outcome used by disposition selection are compared.
	var pack struct {
		ID       string `json:"id"`
		Outcomes []struct {
			ID string `json:"id"`
		} `json:"outcomes"`
	}
	if _, err := readInputJSONDepth([]byte(r.Pack), 64); err != nil {
		return out, err
	}
	if json.Unmarshal([]byte(r.Pack), &pack) != nil || pack.ID != stage.Pack.ID {
		return out, errWorkflowBinding
	}
	if stage.Selection != nil && stage.Selection.Rule != nil && stage.Selection.Rule.Kind == "outcome" {
		found := false
		for _, outcome := range pack.Outcomes {
			found = found || outcome.ID == stage.Selection.Rule.OutcomeID
		}
		if !found {
			return out, errWorkflowBinding
		}
	}
	boundBytes, err := json.Marshal(bound)
	if err != nil {
		return out, err
	}
	releaseBytes, err := json.Marshal(r)
	if err != nil {
		return out, err
	}
	out = workflowStageSnapshot{
		Frozen:    workflowFrozenStage{stage.ID, stage.Dialect.Digest, bindingDigest, mappingV2Hash(bound), stage.Pack.Digest, releaseDigest(r), runtimeDigest},
		Published: bytes.Clone(in.Published), Binding: bytes.Clone(in.Binding), Bound: boundBytes, Pack: []byte(r.Pack), Release: releaseBytes,
	}
	return out, nil
}

func workflowDialectMatches(ref workflowDialectRef, mapping InputMapping) bool {
	if ref.FormatVersion != mapping.FormatVersion {
		return false
	}
	if ref.FormatVersion == "" {
		return string(ref.Version) == string(encode(mapping.Version)) && ref.ID == ""
	}
	var version string
	return json.Unmarshal(ref.Version, &version) == nil && ref.ID == mapping.ArtifactID && version == mapping.ArtifactVersion
}
func sameWorkflowCanonical(a, b []byte) bool {
	ac, ae := proofCanon(a)
	bc, be := proofCanon(b)
	return ae == nil && be == nil && bytes.Equal(ac, bc)
}
func workflowSourceAdmission(m InputMapping, profiles []InputProfile) error {
	for _, source := range m.Sources {
		class := "asserted"
		if isResearch(source) {
			recipe, err := readResearchRecipe(source, nil, true)
			if err != nil {
				return err
			}
			if _, err := researchProfiles(source, recipe, profiles); err != nil {
				return err
			}
			class = "generated"
		} else if source.Profile != "" {
			profile, err := profileFor(source, profiles)
			if err != nil {
				return err
			}
			class = profile.Class
		}
		facts, evidence, err := readTargets(source.Read)
		if err != nil {
			return err
		}
		check := func(kind, target string) bool {
			if !admitted(m, kind, target, class, class == "generated") {
				return false
			}
			if class != "generated" {
				return true
			}
			if m.Admits == nil {
				return false
			}
			classes := m.Admits.Facts[target]
			if kind == "evidence" {
				classes = m.Admits.Evidence[target]
			}
			return len(classes) == 1 && classes[0] == "generated"
		}
		for _, fact := range facts {
			if !check("fact", fact.Target) {
				return errWorkflowBinding
			}
		}
		for _, entry := range evidence {
			if !check("evidence", entry.Requirement) {
				return errWorkflowBinding
			}
		}
	}
	return nil
}
