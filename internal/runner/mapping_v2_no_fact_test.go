package runner

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// The warning a release gets when its sample derived no fact, byte for byte.
const wantNoFactWarning = "The sample derived no fact: the release's sample shows nothing of how the mapping turns a record into facts."

// noFactRule claims /request from a record of the case's id, and nothing,
// unknown, from any other record. Every evidence requirement of the triage
// pack is acknowledged in unmappedEvidence, so the rule claims none.
const noFactRule = `{"ruleVersion":"1","parameters":{"vendorId":"integer"},"clauses":[` +
	`{"when":{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"exists","field":"/facts/request"}]},` +
	`"claim":{"facts":[{"pointer":"/request","from":"/facts/request"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"matching-request"},` +
	`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"wrong-subject"}]}`

// A preparation that has fact targets and derived none of them is named;
// one that derived any fact, or has no fact target, is not (#212).
func TestASampleThatDerivesNoFactIsNamed(t *testing.T) {
	if sampleDerivedNoFact != wantNoFactWarning {
		t.Fatalf("the warning reads %q", sampleDerivedNoFact)
	}
	input, p, key, at := v2Fixture(t)
	present, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	if derivedNoFact(present.Preparation) {
		t.Fatal("a sample that derived its fact was named as deriving none")
	}

	// The record is of another vendor: the rule's last clause claims no fact.
	none := cloneInput(input)
	none.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":8,"description":"another vendor"}`), at, 0)}
	derived, e := normalizeV2(none, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	if string(derived.Facts) != `{}` || !derivedNoFact(derived.Preparation) {
		t.Fatalf("a sample that derived no fact (%s) was not named", derived.Facts)
	}

	// A rule with only an evidence target has no fact to derive, by design.
	evidenceOnly := cloneInput(none)
	evidenceOnly.Source.Mapping.Sources[0].Read.Rule = json.RawMessage(`{"ruleVersion":"1","parameters":{"vendorId":"integer"},"clauses":[{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"registry":"unknown"},"acquisitionStatus":"unknown"},"reason":"evidence-only"}]}`)
	derived, e = normalizeV2(evidenceOnly, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	if len(derived.Preparation.Lineage) != 1 || derived.Preparation.Lineage[0].Kind != "evidence" || derivedNoFact(derived.Preparation) {
		t.Fatal("a mapping with only evidence targets was named as deriving no fact", string(encode(derived.Preparation.Lineage)))
	}

	// A skipped source and a source claimed unknown beside one that derived
	// its facts are a legitimate release, and are not named.
	sp, skey := skippedSourceProfile(t)
	skipped, e := normalizeV2(skippedSourceInput(t, sp, skey, at), []InputProfile{sp}, at)
	if e != nil {
		t.Fatal(e)
	}
	if derivedNoFact(skipped.Preparation) {
		t.Fatal("a sample with a skipped source and derived facts was named as deriving none")
	}
	if derivedNoFact(nil) {
		t.Fatal("a missing preparation was named; the preview refuses it")
	}
}

// A release whose sample derived no fact is created and ready, and says so
// as its last mapping review warning; the same release over a sample that
// derives its fact does not (#212).
func TestAReleaseWhoseSampleDerivesNoFactIsWarned(t *testing.T) {
	cfg := testConfig(t)
	input, p, key, at := v2Fixture(t)
	cfg.InputProfiles = []InputProfile{p}
	input.Source.Mapping.UnmappedEvidence = []string{"intake-form", "sponsor-endorsement", "sensitive-data-approvals"}
	input.Source.Mapping.Sources[0].Read = SourceRead{Rule: json.RawMessage(noFactRule)}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	pack, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	request := func(id int) Input {
		i := cloneInput(input)
		record := encode(map[string]any{"id": id, "facts": json.RawMessage(sample().Facts)})
		i.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), record, at, 0)}
		return i
	}

	none, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: request(8)})
	if e != nil {
		t.Fatal(e)
	}
	if string(none.Sample.Facts) != `{}` || !releaseReady(none) {
		t.Fatalf("the release over a sample deriving no fact has facts %s and is ready %v", none.Sample.Facts, releaseReady(none))
	}
	n := len(none.MappingWarnings)
	if n < 2 || none.MappingWarnings[n-1] != wantNoFactWarning {
		t.Fatalf("the release's warnings are %q", none.MappingWarnings)
	}
	for _, w := range none.MappingWarnings[:n-1] {
		if w == wantNoFactWarning {
			t.Fatalf("the warning is given twice: %q", none.MappingWarnings)
		}
	}
	retained, e := s.release(none.ID)
	if e != nil || !sameJSON(encode(retained.MappingWarnings), encode(none.MappingWarnings)) || releaseDigest(retained) != releaseDigest(none) {
		t.Fatal("the stored release does not carry the warning as it was made", e)
	}

	derived, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: request(7)})
	if e != nil {
		t.Fatal(e)
	}
	if string(derived.Sample.Facts) == `{}` || !releaseReady(derived) {
		t.Fatalf("the release over a sample deriving its fact has facts %s", derived.Sample.Facts)
	}
	for _, w := range derived.MappingWarnings {
		if w == wantNoFactWarning {
			t.Fatalf("a sample that derived its fact was warned: %q", derived.MappingWarnings)
		}
	}
	if len(derived.MappingWarnings) != n-1 {
		t.Fatalf("the warnings beside it changed: %q and %q", derived.MappingWarnings, none.MappingWarnings)
	}
}
