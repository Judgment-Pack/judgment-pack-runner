package runner

import (
	"encoding/json"
	"testing"
)

func TestV2PlanningVerifiesPrefixBeforeChaining(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: "detail", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Parameters: map[string]Parameter{"description": {From: "vendor", Pointer: "/vendor/description", Type: "string"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"description":{"$param":"description"}}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/detail", "/detail"}}, Evidence: []EvidenceMapping{}}}})
	prefix := cloneInput(input)
	prefix.Source.Sources = nil
	plan, err := nextInput(prefix, []InputProfile{p}, at)
	if err != nil || plan.Next == nil || plan.Next.Name != "vendor" || string(plan.Next.Arguments) != `{"arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"},"tool":"execute_sql"}` {
		t.Fatal(plan, err)
	}
	plan, err = nextInput(input, []InputProfile{p}, at)
	if err != nil || plan.Next == nil || plan.Next.Name != "detail" || string(plan.Next.Arguments) != `{"arguments":{"description":"a verified vendor"},"tool":"lookup"}` {
		t.Fatal(plan, err)
	}
	input.Source.Sources["detail"] = SourceValue{Response: signResponse(t, p, key, plan.Next.Arguments, []byte(`{"detail":"ok"}`), at, 1)}
	plan, err = nextInput(input, []InputProfile{p}, at)
	full, fullErr := normalizeV2(input, []InputProfile{p}, at)
	if err != nil || fullErr != nil || plan.Next != nil || plan.Input == nil || !sameJSON(encode(full), encode(plan.Input)) {
		t.Fatal(plan, err, fullErr)
	}
	delete(input.Source.Sources, "detail")
	input.Source.Case = []byte(`{"id":8}`)
	if _, err = nextInput(input, []InputProfile{p}, at); err == nil {
		t.Fatal("planned from tampered prefix")
	}
	input.Source.Case = []byte(`{"id":7}`)
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":8,"description":"wrong subject"}`), at, 0)}
	plan, err = nextInput(input, []InputProfile{p}, at)
	if err != nil || plan.Next != nil || plan.Input == nil || plan.Input.Preparation.Outcomes[2].Reason != "dependency-unavailable" {
		t.Fatal(plan, err)
	}
}
func TestV2PlanningPreflightsTrustAndAdmission(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	input.Source.Sources = nil
	p.Class = "generated"
	input.Source.Mapping.Sources[0].ProfileDigest = profileHash(p)
	if _, err := nextInput(input, []InputProfile{p}, at); err == nil {
		t.Fatal("planned disallowed generated input")
	}
	p.Class = "record"
	input.Source.Mapping.Sources[0].ProfileDigest = profileHash(p)
	input.Source.Mapping.Sources[0].Arguments = []byte(`{"tool":"destroy","arguments":{}}`)
	if _, err := nextInput(input, []InputProfile{p}, at); err == nil {
		t.Fatal("planned disallowed tool")
	}
}
