package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// freshnessOf prepares input at at, as verification recomputes it, and
// returns its report of what was judged against the instant.
func freshnessOf(t *testing.T, input Input, profiles []InputProfile, at time.Time) (*UnsignedFreshness, Input) {
	t.Helper()
	var judged []judgedFreshness
	got, err := prepareV2(input, profiles, at, false, &judged)
	if err != nil {
		t.Fatal(err)
	}
	return unsignedFreshness(got.Preparation, judged), got
}

// The report names the instant and each source whose freshness was judged
// against it, whatever reads the source: a rule, a copy, or nothing at all.
// The unsigned-parameter report says nothing of a calculator on a copy
// mapping, which is the audit's case (#55): this one names it. A calculator
// that did not compute had its acquisition judged and not its tables, and a
// source skipped behind it nothing. A local file, which no receipt verifies,
// had nothing judged either, and a case-only run judged nothing.
func TestFreshnessJudgedAgainstTheInstantIsNamedSourceBySource(t *testing.T) {
	input, p, key, at := calculatorFixture(t)
	instant := at.Format("2006-01-02T15:04:05Z")
	want := func(sources string) string {
		return `{"against":"preparation.verifiedAt","verifiedAt":"` + instant + `","sources":` + sources + `}`
	}
	t.Run("a calculator read by a rule", func(t *testing.T) {
		got, _ := freshnessOf(t, input, []InputProfile{p}, at)
		if string(encode(got)) != want(`[{"source":"fx","checks":["acquisition","calculation"],"targets":2}]`) {
			t.Fatal(string(encode(got)))
		}
	})
	t.Run("a calculator read by a copy", func(t *testing.T) {
		i := cloneInput(input)
		i.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/invoice/converted", Source: "/converted"}}, Evidence: []EvidenceMapping{}}}
		got, prepared := freshnessOf(t, i, []InputProfile{p}, at)
		if string(encode(got)) != want(`[{"source":"fx","checks":["acquisition","calculation"],"targets":1}]`) {
			t.Fatal(string(encode(got)))
		}
		if u := unsignedParameters(i.Source.Mapping, prepared.Preparation); u != nil {
			t.Fatal("the fixture is not the audit's case: a parameter is reported unsigned", u)
		}
	})
	t.Run("a calculator whose answer maps no target", func(t *testing.T) {
		i := cloneInput(input)
		i.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}}
		got, _ := freshnessOf(t, i, []InputProfile{p}, at)
		if string(encode(got)) != want(`[{"source":"fx","checks":["acquisition","calculation"],"targets":0}]`) {
			t.Fatal(string(encode(got)))
		}
	})
	t.Run("a calculator that could not compute, and a source skipped behind it", func(t *testing.T) {
		i := cloneInput(input)
		i.Source.Mapping.Sources = append(i.Source.Mapping.Sources, MappingSource{Name: "ledger", Kind: "operation", Profile: "vendors", MaxAge: 300, Parameters: map[string]Parameter{"converted": {From: "fx", Pointer: "/invoice/converted", Type: "string"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"converted":{"$param":"converted"}}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/ledger", Source: "/ledger"}}, Evidence: []EvidenceMapping{}}}})
		vendors, _ := testProfile(t)
		i.Source.Mapping.Sources[1].ProfileDigest = profileHash(vendors)
		c := calculation(at)
		c["status"] = "cannot-compute"
		c["asOf"].(map[string]any)["ecb-rates"] = at.Add(-400 * 24 * time.Hour).Format(time.RFC3339)
		answer(t, &i, p, key, at, c)
		got, _ := freshnessOf(t, i, []InputProfile{p, vendors}, at)
		if string(encode(got)) != want(`[{"source":"fx","checks":["acquisition"],"targets":2}]`) {
			t.Fatal(string(encode(got)))
		}
	})
	t.Run("a local file beside an acquired calculator", func(t *testing.T) {
		i := cloneInput(input)
		local := MappingSource{Name: "lookup", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/vendor/id", Source: "/id"}}, Evidence: []EvidenceMapping{}}}}
		i.Source.Mapping.Sources = append([]MappingSource{local}, i.Source.Mapping.Sources...)
		i.Source.Sources["lookup"] = SourceValue{Snapshot: fileInput(`{"id":7}`, InputMapping{Version: 1, Provider: "local-file"}).Source.Snapshot}
		got, prepared := freshnessOf(t, i, []InputProfile{p}, at)
		if string(encode(got)) != want(`[{"source":"fx","checks":["acquisition","calculation"],"targets":2}]`) {
			t.Fatal(string(encode(got)))
		}
		if l := prepared.Preparation.Lineage[0]; l.Source != "lookup" || l.Class != "asserted" || !l.Present {
			t.Fatal("the local file was not read:", string(encode(l)))
		}
	})
	t.Run("a case-only run", func(t *testing.T) {
		i := cloneInput(input)
		i.Source.Mapping.Sources = nil
		i.Source.Mapping.Case.Facts = []FactMapping{{Target: "/amount", Source: "/amount"}}
		i.Source.Sources = map[string]SourceValue{}
		if got, _ := freshnessOf(t, i, nil, at); got != nil {
			t.Fatal(string(encode(got)))
		}
	})
}

// The audit's reproduction (#55): a calculated run on a copy mapping, signed
// and witnessed, whose preparation is recomputed one second later and put in
// its export in place of the original. The audit record's bytes, their
// signature and the chain entry with its checkpoint are unchanged, and the
// export's inputs verify as the original's did: freshness was judged against
// the instant the export supplies. The report names that instant, the moved
// one, and the calculator whose acquisition and tables were judged against
// it; the unsigned-parameter report has nothing to say of it. What tells the
// two apart is the record's binding, when the Runtime under test recorded one
// (0.31.0 and later): it matches the original's preparation and not the moved
// one, which verify-run refuses. A record without one is absent in both.
func TestAPreparationMovedBySecondVerifiesAndItsInstantIsNamed(t *testing.T) {
	bundle, profile := calculatedSignedExport(t, "computed")
	at, err := time.Parse("2006-01-02T15:04:05Z", bundle.Run.Input.Preparation.VerifiedAt)
	if err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Second)
	moved, err := normalizeV2(bundle.Run.Input, []InputProfile{profile}, later)
	if err != nil {
		t.Fatal(err)
	}
	var changed VerificationBundle
	if err = json.Unmarshal(encode(bundle), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Run.Input.Preparation = moved.Preparation
	if sameJSON(encode(changed.Run.Input.Preparation), encode(bundle.Run.Input.Preparation)) || changed.Run.Input.Preparation.VerifiedAt != later.Format("2006-01-02T15:04:05Z") {
		t.Fatal("the preparation did not move")
	}
	if !bytes.Equal(changed.Run.AuditBytes, bundle.Run.AuditBytes) || !bytes.Equal(changed.Run.AuditSignatures, bundle.Run.AuditSignatures) || !sameJSON(encode(changed.Chain), encode(bundle.Chain)) {
		t.Fatal("the record, its signature or the chain entry changed")
	}
	bound := runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN"))
	binding := map[string]string{"original": BindingAbsent, "moved": BindingAbsent}
	if bound {
		binding = map[string]string{"original": BindingMatches, "moved": BindingMismatch}
	}
	for name, b := range map[string]VerificationBundle{"original": bundle, "moved": changed} {
		v, err := VerifyInputs(encode(b), []InputProfile{profile}, b.ReleaseDigest)
		if err != nil {
			t.Fatal(name, err)
		}
		if s := v.CheckSignature(mustKey(t, vectorKey1)); s.Status != SignatureSigned {
			t.Fatalf("%s: the signature does not hold: %+v", name, s)
		}
		check, err := v.CheckRunChain(bytes.NewReader(append(bytes.Clone(b.Chain.Entry), '\n')), []Checkpoint{bundle.Chain.Checkpoint})
		if err != nil || check.Status != "valid" || !check.Witnessed {
			t.Fatalf("%s: the held checkpoint does not witness the run: %+v %v", name, check, err)
		}
		want := `{"against":"preparation.verifiedAt","verifiedAt":"` + b.Run.Input.Preparation.VerifiedAt + `","sources":[{"source":"fx","checks":["acquisition","calculation"],"targets":3}]}`
		if string(encode(v.Freshness)) != want || v.Unsigned != nil {
			t.Fatal(name, string(encode(v.Freshness)), v.Unsigned)
		}
		if v.Binding == nil || v.Binding.Status != binding[name] {
			t.Fatal(name, v.Binding)
		}
	}
}
