package runner

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowCopiedCaseKeepsParentInfluence(t *testing.T) {
	m := InputMapping{Version: 3, Case: &CaseMapping{Facts: []FactMapping{{Source: "/company/name", Target: "/selected/name"}, {Source: "/elsewhere", Target: "/other"}}, Evidence: []EvidenceMapping{{Source: "/company/evidence", Requirement: "source"}}}, Admits: &Admission{Facts: map[string][]string{"/selected/name": {"asserted", "generated"}}, Evidence: map[string][]string{"source": {"asserted", "generated"}}}}
	bindings := []workflowBinding{{"/company", "/company", "object", true}}
	parent := workflowInfluence{[]string{"generated", "record"}, true}
	inherited, err := inheritedWorkflowInfluence(m, bindings, []byte(`{"company":{"name":"A","evidence":"present"}}`), parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(inherited) != 2 || !reflect.DeepEqual(inherited[workflowTarget{"fact", "/selected/name"}], parent) || !reflect.DeepEqual(inherited[workflowTarget{"evidence", "source"}], parent) {
		t.Fatalf("%+v", inherited)
	}
	if _, found := inherited[workflowTarget{"fact", "/other"}]; found {
		t.Fatal("unrelated case value contaminated")
	}
	delete(m.Admits.Evidence, "source")
	if _, err := inheritedWorkflowInfluence(m, bindings, []byte(`{"company":{"name":"A"}}`), parent); !errors.Is(err, errWorkflowInfluenceAdmission) {
		t.Fatal("missing nested value erased container influence", err)
	}
}
func TestWorkflowUnusedCaseParameterRequiresGeneratedAdmission(t *testing.T) {
	input, _, _, _ := v2Fixture(t)
	m := input.Source.Mapping
	// The declared parameter still has scope even when no request mentions it.
	m.Sources[0].Arguments = json.RawMessage(`{"tool":"execute_sql","arguments":{"sql":"SELECT 1"}}`)
	bindings := []workflowBinding{{"/companyId", "/id", "integer", true}}
	parent := workflowInfluence{[]string{"generated"}, true}
	if _, err := inheritedWorkflowInfluence(m, bindings, []byte(`{"companyId":7}`), parent); !errors.Is(err, errWorkflowInfluenceAdmission) {
		t.Fatal("unused parameter bypass", err)
	}
	facts, evidence, err := readTargets(m.Sources[0].Read)
	if err != nil {
		t.Fatal(err)
	}
	m.Admits = &Admission{Facts: map[string][]string{}, Evidence: map[string][]string{}}
	for _, f := range facts {
		m.Admits.Facts[f.Target] = []string{"record", "generated"}
	}
	for _, e := range evidence {
		m.Admits.Evidence[e.Requirement] = []string{"record", "generated"}
	}
	inherited, err := inheritedWorkflowInfluence(m, bindings, []byte(`{"companyId":7}`), parent)
	if err != nil || len(inherited) != len(facts)+len(evidence) {
		t.Fatal(inherited, err)
	}
	// An optional missing copy does not influence an unrelated preexisting case.
	bindings[0].Required = false
	inherited, err = inheritedWorkflowInfluence(m, bindings, []byte(`{}`), parent)
	if err != nil || len(inherited) != 0 {
		t.Fatal(inherited, err)
	}
}
func TestWorkflowOutputInfluenceIncludesOverlappingContainers(t *testing.T) {
	lineage := []TargetLineage{{Kind: "fact", Target: "/people", Class: "record"}, {Kind: "fact", Target: "/other", Class: "generated", GeneratedInfluence: true}, {Kind: "evidence", Target: "proof", Class: "generated", GeneratedInfluence: true}}
	inherited := map[workflowTarget]workflowInfluence{{"fact", "/people"}: {[]string{"generated"}, true}}
	got, err := outputWorkflowInfluence("/people/0", lineage, inherited)
	want := workflowInfluence{[]string{"generated", "record"}, true}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	got, err = outputWorkflowInfluence("/people/0/name", lineage, inherited)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("narrow copy lost container influence", got, err)
	}
	got, err = outputWorkflowInfluence("/unrelated", lineage, inherited)
	if err != nil || len(got.Classes) != 0 || got.GeneratedInfluence {
		t.Fatal(got, err)
	}
	// Whole-token comparison: /people is not a prefix of /people-other.
	got, err = outputWorkflowInfluence("/people-other", lineage, inherited)
	if err != nil || len(got.Classes) != 0 || got.GeneratedInfluence {
		t.Fatal(got, err)
	}
}
func TestWorkflowInfluenceForms(t *testing.T) {
	for _, influence := range []workflowInfluence{{nil, false}, {[]string{"record", "generated"}, true}, {[]string{"generated", "generated"}, true}, {[]string{"human"}, false}} {
		if checkedWorkflowInfluence(influence) {
			t.Fatal("invalid influence", influence)
		}
	}
}

func TestWorkflowManyOutputInfluenceAtPointerBounds(t *testing.T) {
	for _, pointer := range []string{"/" + strings.Repeat("x", 1023), strings.Repeat("/x", 32)} {
		t.Run(pointer, func(t *testing.T) {
			tokens, err := workflowPointer(pointer, false)
			if err != nil {
				t.Fatal(err)
			}
			var facts any = []any{map[string]any{"name": "A"}}
			for i := len(tokens) - 1; i >= 0; i-- {
				facts = map[string]any{tokens[i]: facts}
			}
			outputs, err := outputsWorkflow(encode(facts), workflowOutputs{pointer, "many"})
			if err != nil || len(outputs) != 1 {
				t.Fatal(outputs, err)
			}
			lineage := []TargetLineage{{Kind: "fact", Target: pointer, Class: "record"}}
			inherited := map[workflowTarget]workflowInfluence{{"fact", pointer}: {[]string{"generated"}, true}}
			got, err := outputWorkflowInfluence(pointer+"/0", lineage, inherited)
			want := workflowInfluence{[]string{"generated", "record"}, true}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatal(got, err)
			}
			for _, suffix := range []string{"/4096", "/01", "/-1", "/0/0", "/x"} {
				if _, err := outputWorkflowInfluence(pointer+suffix, lineage, inherited); err == nil {
					t.Fatal("accepted invalid derived pointer", suffix)
				}
			}
		})
	}
}

func TestWorkflowDeclaredPointerCountBoundPrecedesSyntax(t *testing.T) {
	if _, err := workflowPointer(strings.Repeat("/x", 32)+"/~2", false); !errors.Is(err, errWorkflowBound) {
		t.Fatal("token bound must precede malformed escape", err)
	}
}
