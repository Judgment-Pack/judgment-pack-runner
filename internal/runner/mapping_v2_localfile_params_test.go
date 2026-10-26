package runner

import (
	"encoding/json"
	"testing"
)

// A local file may declare source parameters as any source may (the dialect
// companion's §3 and §4; §4.2 excludes a profile, maxAge and a request, and
// nothing else): the validator admits them on every path, a local file has no
// request, so the values reach the outcome's parameters and the lineage's
// dependencies, and a parameter with no value skips the source (§9.2).
func TestALocalFileMayDeclareSourceParameters(t *testing.T) {
	input, p, key, at := calculatorFixture(t)
	local := MappingSource{Name: "lookup", Kind: "selected-file", Provider: "local-file",
		Parameters: map[string]Parameter{"converted": {From: "fx", Pointer: "/invoice/converted", Type: "string"}},
		Read:       SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/vendor/id", Source: "/id"}}, Evidence: []EvidenceMapping{}}}}
	input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, local)
	snapshot := fileInput(`{"id":7}`, InputMapping{Version: 1, Provider: "local-file"}).Source.Snapshot

	t.Run("a valued parameter is carried and the file read", func(t *testing.T) {
		i := cloneInput(input)
		answer(t, &i, p, key, at, calculation(at))
		i.Source.Sources["lookup"] = SourceValue{Snapshot: snapshot}
		got, err := normalizeV2(i, []InputProfile{p}, at)
		if err != nil {
			t.Fatal(err)
		}
		o := got.Preparation.Outcomes[len(got.Preparation.Outcomes)-1]
		var parameters map[string]any
		if json.Unmarshal(o.Parameters, &parameters) != nil || o.Name != "lookup" || o.Status != "resolved" || parameters["converted"] != "1302.36" || len(o.Arguments) != 0 {
			t.Fatal(string(encode(o)))
		}
		seen := 0
		for _, l := range got.Preparation.Lineage {
			if l.Source != "lookup" {
				continue
			}
			seen++
			if !l.Present || l.Class != "asserted" || string(encode(l.Dependencies)) != `[{"parameter":"converted","source":"fx","pointer":"/invoice/converted"}]` {
				t.Fatal(string(encode(l)))
			}
		}
		if seen != 1 || string(got.Facts) != `{"invoice":{"converted":"1302.36"},"vendor":{"id":7}}` {
			t.Fatal(seen, string(got.Facts))
		}
	})
	t.Run("a parameter with no value skips the file", func(t *testing.T) {
		i := cloneInput(input)
		c := calculation(at)
		c["status"] = "cannot-compute"
		answer(t, &i, p, key, at, c)
		got, err := normalizeV2(i, []InputProfile{p}, at)
		if err != nil {
			t.Fatal(err)
		}
		o := got.Preparation.Outcomes[len(got.Preparation.Outcomes)-1]
		if o.Name != "lookup" || o.Status != "unknown" || o.Reason != "dependency-unavailable" || len(o.Arguments) != 0 {
			t.Fatal(string(encode(o)))
		}
		if claim, _ := canonical(o.Claim); string(claim) != `{"acquisitionStatus":"unknown","basis":[],"evidenceAvailability":{},"facts":{},"reason":"dependency-unavailable"}` {
			t.Fatal(string(o.Claim))
		}
		i.Source.Sources["lookup"] = SourceValue{Snapshot: snapshot}
		if _, err = normalizeV2(i, []InputProfile{p}, at); err == nil {
			t.Fatal("a response for a skipped local file was accepted")
		}
	})
	t.Run("the live validator and the reader's form check agree", func(t *testing.T) {
		// The reader (VerifyPreparation) answers kind-unread for a local file,
		// as verify_preparation_parity_test.go holds; its form check is
		// validateDialectMode offline, and the live path's is validateDialect.
		m := cloneInput(input).Source.Mapping
		if err := validateDialect(m, false); err != nil {
			t.Fatal("live:", err)
		}
		if err := validateDialectMode(m, false, true); err != nil {
			t.Fatal("offline:", err)
		}
	})
	t.Run("verification metadata stays refused", func(t *testing.T) {
		for name, change := range map[string]func(*MappingSource){
			"profile":   func(s *MappingSource) { s.Profile = "fx" },
			"maxAge":    func(s *MappingSource) { s.MaxAge = 300 },
			"arguments": func(s *MappingSource) { s.Arguments = json.RawMessage(`{}`) },
		} {
			i := cloneInput(input)
			change(&i.Source.Mapping.Sources[len(i.Source.Mapping.Sources)-1])
			if _, err := normalizeV2(i, []InputProfile{p}, at); err == nil {
				t.Fatal(name, "accepted on a local file")
			}
		}
	})
}
