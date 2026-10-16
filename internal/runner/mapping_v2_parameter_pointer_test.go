package runner

import (
	"errors"
	"testing"
	"time"
)

// A source parameter's pointer is exactly a fact target of the earlier source
// it comes from (judgment-pack-spec, spec/dialect.md §3): not a path below
// one, not one that names no target, not an evidence requirement. The
// specification's reader refuses the document, input-malformed, before any
// acquisition; Runner refuses it as a malformed mapping in the same place,
// the form check every path that reads a mapping goes through.
func TestASourceParameterPointerIsExactlyAFactTarget(t *testing.T) {
	p, key := skippedSourceProfile(t)
	mapping := func(version int, pointer string) InputMapping {
		m := skippedSourceInput(t, p, key, time.Now()).Source.Mapping
		m.Version = version
		endorsement := &m.Sources[2]
		approver := endorsement.Parameters["approver"]
		approver.Pointer = pointer
		endorsement.Parameters = map[string]Parameter{"approver": approver, "requestType": endorsement.Parameters["requestType"]}
		return m
	}
	for _, version := range []int{2, 3} {
		if err := mapping(version, "/sponsor/id").validate(); err != nil {
			t.Fatalf("version %d: an exact fact target is refused: %v", version, err)
		}
		for name, c := range map[string]struct{ pointer, message string }{
			"a path below a fact target": {"/sponsor/id/value", `source parameter approver reads no fact target of sponsor: pointer "/sponsor/id/value" is not exactly a fact target the source declares`},
			"a prefix of a fact target":  {"/sponsor", `source parameter approver reads no fact target of sponsor: pointer "/sponsor" is not exactly a fact target the source declares`},
			"no target at all":           {"/sponsor/name", `source parameter approver reads no fact target of sponsor: pointer "/sponsor/name" is not exactly a fact target the source declares`},
			"another source's target":    {"/intake/type", `source parameter approver reads no fact target of sponsor: pointer "/intake/type" is not exactly a fact target the source declares`},
			"an evidence requirement":    {"/intake-form", `source parameter approver reads no fact target of sponsor: pointer "/intake-form" is not exactly a fact target the source declares`},
		} {
			err := mapping(version, c.pointer).validate()
			var api *apiError
			if !errors.As(err, &api) || api.Code != "invalid_source_input" {
				t.Fatalf("version %d, %s: not refused as a malformed mapping: %v", version, name, err)
			}
			prefix := "Mapping v2: "
			if version == 3 {
				prefix = "Mapping v3: "
			}
			if api.Message != prefix+c.message {
				t.Fatalf("version %d, %s: the message is %q", version, name, api.Message)
			}
		}
	}
	// The case is not a source of `sources`: a source parameter cannot come from it.
	m := mapping(2, "/sponsor/id")
	approver := m.Sources[2].Parameters["approver"]
	approver.From = "case"
	m.Sources[2].Parameters["approver"] = approver
	if err := m.validate(); err == nil {
		t.Fatal("a source parameter from the case is accepted")
	}
	// A case parameter's pointer is into the case document, not a fact target.
	m = mapping(2, "/sponsor/id")
	m.Case.Parameters["requestId"] = Parameter{Pointer: "/anything/at/all", Type: "string"}
	if err := m.validate(); err != nil {
		t.Fatalf("a case parameter's pointer is refused: %v", err)
	}
}
