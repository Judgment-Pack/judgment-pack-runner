package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestWorkflowNamedDialectReference(t *testing.T) {
	for _, format := range []string{"2.0.0", "3.0.0"} {
		raw := bytes.Replace(workflowExample(), []byte(`"version":3`), []byte(`"formatVersion":"`+format+`","id":"urn:example:company","version":"1.2.3"`), 1)
		doc, canonical, err := readWorkflow(raw)
		if err != nil || doc.Stages[0].Dialect.FormatVersion != format {
			t.Fatal(err)
		}
		_, again, err := readWorkflow(canonical)
		if err != nil || !bytes.Equal(canonical, again) {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			old, replacement string
			want             error
		}{
			{`"formatVersion":"` + format + `"`, `"formatVersion":"4.0.0"`, errWorkflowVersion},
			{`"formatVersion":"` + format + `"`, `"formatVersion":null`, errWorkflowVersion},
			{`"version":"1.2.3"`, `"version":3`, errWorkflowForm},
			{`"version":"1.2.3"`, `"version":"01.2.3"`, errWorkflowForm},
			{`"id":"urn:example:company"`, `"id":"has space"`, errWorkflowForm},
			{`"id":"urn:example:company",`, ``, errWorkflowForm},
			{`"formatVersion":"` + format + `",`, ``, errWorkflowVersion},
		} {
			_, _, err := readWorkflow(bytes.Replace(raw, []byte(tc.old), []byte(tc.replacement), 1))
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s => %v, want %v", tc.replacement, err, tc.want)
			}
		}
	}
}

func workflowFreezeFixture(t *testing.T, published, binding []byte, profiles []InputProfile) ([]byte, workflowFreezeInput, string) {
	t.Helper()
	var mapping InputMapping
	if err := strictJSON(published, &mapping); err != nil {
		t.Fatal(err)
	}
	bound := mapping
	var imp *DialectImport
	if len(binding) != 0 {
		imp = &DialectImport{Dialect: published, Binding: binding}
		var err error
		bound, err = bindDialect(*imp, profiles)
		if err != nil {
			t.Fatal(err)
		}
	}
	pack := `{"specVersion":"0.2.0-draft","id":"urn:example:pack","version":"1.0.0","outcomes":[{"id":"yes"}]}`
	runtime := digest([]byte("test-runtime-identity"))
	release := Release{SchemaVersion: "1", ID: "release_test", Pack: pack, PackID: "urn:example:pack", PackVersion: "1.0.0", PackDigest: digest([]byte(pack)), RuntimeDigest: runtime, InputMapping: &bound, InputProfiles: profilesUsed(bound, profiles), Sample: Input{Source: &SourceInput{Mapping: bound, Import: imp}}}
	var err error
	release.Dialect, err = releaseDialectIdentity(release)
	if err != nil {
		t.Fatal(err)
	}
	doc := decoded(t, workflowExample()).(map[string]any)
	stage := doc["stages"].([]any)[0].(map[string]any)
	delete(stage, "selection")
	stage["dialect"] = map[string]any{"version": mapping.Version, "digest": digest(canonTest(t, published))}
	if mapping.FormatVersion != "" {
		stage["dialect"] = map[string]any{"formatVersion": mapping.FormatVersion, "id": mapping.ArtifactID, "version": mapping.ArtifactVersion, "digest": digest(canonTest(t, published))}
	}
	stage["pack"] = map[string]any{"id": release.PackID, "digest": release.PackDigest}
	doc["stages"], doc["result"] = []any{stage}, stage["id"]
	return encode(doc), workflowFreezeInput{published, binding, release}, runtime
}

func TestWorkflowFreezeImportedAndNamedIdentity(t *testing.T) {
	for _, named := range []bool{false, true} {
		f := newImportFixture(t)
		if named {
			f.published.FormatVersion, f.published.ArtifactID, f.published.ArtifactVersion = "3.0.0", "urn:example:vendor", "1.2.3"
		}
		published := encode(f.published)
		binding := bindingOf(t, published, map[string]InputProfile{"vendor": f.profile})
		profiles := []InputProfile{f.profile}
		raw, in, runtime := workflowFreezeFixture(t, published, binding, profiles)
		frozen, err := freezeWorkflowStages(raw, []workflowFreezeInput{in}, profiles, runtime)
		if err != nil || len(frozen) != 1 {
			t.Fatal(err)
		}
		got := frozen[0]
		if got.Frozen.PublishedDialectDigest == got.Frozen.BoundDialectDigest || got.Frozen.BindingDocumentDigest == nil || *got.Frozen.BindingDocumentDigest != digest(canonTest(t, binding)) {
			t.Fatal("published, binding and bound pins collapsed")
		}
		if got.Frozen.ReleaseDigest != releaseDigest(in.Release) || got.Frozen.PackDigest != digest([]byte(in.Release.Pack)) {
			t.Fatal("incorrect artifact digest")
		}
		var kept InputMapping
		if strictJSON(got.Bound, &kept) != nil || named && (kept.FormatVersion != "3.0.0" || kept.ArtifactID != f.published.ArtifactID) {
			t.Fatal("named identity lost")
		}
		published[0], binding[0] = 'x', 'x'
		if got.Published[0] != '{' || got.Binding[0] != '{' {
			t.Fatal("caller changed frozen bytes")
		}
	}
}

func TestWorkflowFreezeRefusesChangedPinsAndProfiles(t *testing.T) {
	for _, mutate := range []struct {
		name   string
		change func(*workflowFreezeInput, *[]InputProfile, *string)
	}{
		{"runtime", func(_ *workflowFreezeInput, _ *[]InputProfile, runtime *string) {
			*runtime = digest([]byte("different"))
		}},
		{"pack-bytes", func(in *workflowFreezeInput, _ *[]InputProfile, _ *string) { in.Release.Pack += "\n" }},
		{"bound", func(in *workflowFreezeInput, _ *[]InputProfile, _ *string) { in.Release.InputMapping.Version = 2 }},
		{"missing-independent-profile", func(_ *workflowFreezeInput, p *[]InputProfile, _ *string) { *p = nil }},
		{"changed-independent-profile", func(_ *workflowFreezeInput, p *[]InputProfile, _ *string) { (*p)[0].Authority += "-changed" }},
		{"release-profile", func(in *workflowFreezeInput, _ *[]InputProfile, _ *string) { in.Release.InputProfiles = nil }},
		{"release-import", func(in *workflowFreezeInput, _ *[]InputProfile, _ *string) { in.Release.Sample.Source.Import = nil }},
		{"binding", func(in *workflowFreezeInput, _ *[]InputProfile, _ *string) { in.Binding = []byte(`{}`) }},
		{"pack-id", func(in *workflowFreezeInput, _ *[]InputProfile, _ *string) { in.Release.PackID = "urn:elsewhere" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			f := newImportFixture(t)
			published := encode(f.published)
			profiles := []InputProfile{f.profile}
			raw, in, runtime := workflowFreezeFixture(t, published, bindingOf(t, published, map[string]InputProfile{"vendor": f.profile}), profiles)
			mutate.change(&in, &profiles, &runtime)
			if got, err := freezeWorkflowStages(raw, []workflowFreezeInput{in}, profiles, runtime); err == nil || got != nil {
				t.Fatal("admitted mismatched stage", got, err)
			}
		})
	}
}

func TestWorkflowFreezeBoundDialectHasNoBinding(t *testing.T) {
	for _, family := range []int{2, 3} {
		for _, named := range []bool{false, true} {
			input, profile, _, _ := v2Fixture(t)
			mapping := input.Source.Mapping
			mapping.Version = family
			if named {
				mapping.FormatVersion, mapping.ArtifactID, mapping.ArtifactVersion = string(encode(family))+".0.0", "urn:example:dialect", "0.1.0"
			}
			raw, in, runtime := workflowFreezeFixture(t, encode(mapping), nil, []InputProfile{profile})
			got, err := freezeWorkflowStages(raw, []workflowFreezeInput{in}, []InputProfile{profile}, runtime)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Frozen.BindingDocumentDigest != nil || got[0].Frozen.PublishedDialectDigest != got[0].Frozen.BoundDialectDigest {
				t.Fatal("binding without import")
			}
			in.Binding = []byte(`null`)
			if _, err := freezeWorkflowStages(raw, []workflowFreezeInput{in}, []InputProfile{profile}, runtime); err == nil {
				t.Fatal("accepted superfluous binding")
			}
		}
	}
}

func TestWorkflowFreezeResearchChecksAllCapabilitiesBeforeAcquisition(t *testing.T) {
	d := researchDocsOf(t, gtmPlan)
	d.h.server.Close() // freeze must use declarations only; no live source exists
	input := d.input(t)
	raw, in, runtime := workflowFreezeFixture(t, input.Source.Import.Dialect, input.Source.Import.Binding, d.profiles)
	if _, err := freezeWorkflowStages(raw, []workflowFreezeInput{in}, d.profiles, runtime); err != nil {
		t.Fatal(err)
	}
	// Change both pins consistently so the declared capability, rather than
	// an incidental stale digest, is what rejects the changed assignment.
	for _, profile := range d.profiles {
		if profile.Research == nil || !asks(profile.Research.Capabilities, "answer") {
			continue
		}
		changed := append([]InputProfile(nil), d.profiles...)
		for i := range changed {
			if changed[i].ID == profile.ID {
				research := *changed[i].Research
				model := "different-model"
				research.Model = &model
				changed[i].Research = &research
			}
		}
		binding := bytes.ReplaceAll(in.Binding, []byte(profileHash(profile)), []byte(profileHash(profileNamed(t, changed, profile.ID))))
		candidate := in
		candidate.Binding = binding
		if _, err := freezeWorkflowStages(raw, []workflowFreezeInput{candidate}, changed, runtime); err == nil || !strings.Contains(err.Error(), "model") {
			t.Fatal("research model mismatch admitted", err)
		}
		break
	}
}

func TestWorkflowFreezeEveryStageBeforeReturningAny(t *testing.T) {
	f := newImportFixture(t)
	raw, in, runtime := workflowFreezeFixture(t, encode(f.direct), nil, []InputProfile{f.profile})
	doc := decoded(t, raw).(map[string]any)
	first := doc["stages"].([]any)[0].(map[string]any)
	first["selection"] = map[string]any{"mode": "reviewer"}
	second := decoded(t, encode(first)).(map[string]any)
	second["id"], second["execution"], second["input"] = "people", "each-selected-record", map[string]any{"fromStage": "companies", "selected": true}
	second["caseBindings"] = []any{map[string]any{"from": "/name", "to": "/name", "type": "string", "required": true}}
	delete(second, "selection")
	doc["stages"], doc["result"] = []any{first, second}, "people"
	bad := in
	bad.Release.RuntimeDigest = digest([]byte("other-runtime"))
	if got, err := freezeWorkflowStages(encode(doc), []workflowFreezeInput{in, bad}, []InputProfile{f.profile}, runtime); err == nil || got != nil {
		t.Fatal("partial freeze admitted", got, err)
	}
}

func TestWorkflowFreezeDispositionRequiresDeclaredOutcome(t *testing.T) {
	f := newImportFixture(t)
	raw, in, runtime := workflowFreezeFixture(t, encode(f.direct), nil, []InputProfile{f.profile})
	doc, _, err := readWorkflow(raw)
	if err != nil {
		t.Fatal(err)
	}
	stage := doc.Stages[0]
	stage.Selection = &workflowSelectionPolicy{Mode: "disposition", Rule: &workflowRule{Kind: "outcome", OutcomeID: "undeclared"}}
	if _, err := freezeWorkflowStage(stage, in, []InputProfile{f.profile}, runtime); !errors.Is(err, errWorkflowBinding) {
		t.Fatal(err)
	}
	stage.Selection.Rule.OutcomeID = "yes"
	if _, err := freezeWorkflowStage(stage, in, []InputProfile{f.profile}, runtime); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowFreezeGeneratedOperationRequiresExclusiveAdmission(t *testing.T) {
	f := newImportFixture(t)
	profile := f.profile
	profile.Class = "generated"
	m := f.direct
	m.Sources[0].ProfileDigest = profileHash(profile)
	m.Admits = &Admission{Facts: map[string][]string{}, Evidence: map[string][]string{}}
	facts, evidence, err := readTargets(m.Sources[0].Read)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		m.Admits.Facts[fact.Target] = []string{"record", "generated"}
	}
	for _, entry := range evidence {
		m.Admits.Evidence[entry.Requirement] = []string{"record", "generated"}
	}
	raw, in, runtime := workflowFreezeFixture(t, encode(m), nil, []InputProfile{profile})
	if _, err := freezeWorkflowStages(raw, []workflowFreezeInput{in}, []InputProfile{profile}, runtime); !errors.Is(err, errWorkflowBinding) {
		t.Fatal("mixed generated operation admission", err)
	}
	for _, fact := range facts {
		m.Admits.Facts[fact.Target] = []string{"generated"}
	}
	for _, entry := range evidence {
		m.Admits.Evidence[entry.Requirement] = []string{"generated"}
	}
	raw, in, runtime = workflowFreezeFixture(t, encode(m), nil, []InputProfile{profile})
	if _, err := freezeWorkflowStages(raw, []workflowFreezeInput{in}, []InputProfile{profile}, runtime); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowFreezeWithRealRuntimeRelease(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	mapping := InputMapping{Version: 3, FormatVersion: "3.0.0", ArtifactID: "urn:example:intake", ArtifactVersion: "1.0.0", UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &CaseMapping{Facts: []FactMapping{{Source: "/facts/request", Target: "/request"}}, Evidence: []EvidenceMapping{{Source: "/evidence/intake-form", Requirement: "intake-form"}, {Source: "/evidence/sponsor-endorsement", Requirement: "sponsor-endorsement"}}}}
	input := Input{Source: &SourceInput{Mapping: mapping, Case: encode(map[string]any{"facts": sample().Facts, "evidence": sample().Evidence})}}
	release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	raw, in, _ := workflowFreezeFixture(t, encode(mapping), nil, nil)
	in.Release = release
	doc := decoded(t, raw).(map[string]any)
	stage := doc["stages"].([]any)[0].(map[string]any)
	stage["pack"] = map[string]any{"id": release.PackID, "digest": release.PackDigest}
	stage["outputs"] = map[string]any{"pointer": "/request", "cardinality": "one"}
	frozen, err := freezeWorkflowStages(encode(doc), []workflowFreezeInput{in}, nil, s.runtimeDigest)
	if err != nil || len(frozen) != 1 {
		t.Fatal(err)
	}
	if frozen[0].Frozen.ReleaseDigest != releaseDigest(release) || !bytes.Equal(frozen[0].Pack, pack) {
		t.Fatal("real release identity changed")
	}
}
