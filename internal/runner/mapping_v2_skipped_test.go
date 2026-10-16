package runner

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// skippedSourceProfile is the one profile the skipped-source run's three
// operations are read through: a record source of shape mcp, its key made
// for the run, standing in for a gateway. No live service is asked, and the
// profile names the adapter adapter-mcp, version stand-in.
func skippedSourceProfile(t *testing.T) (InputProfile, ed25519.PrivateKey) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "stdio:intake-records"
	return InputProfile{ID: "records", PublicKey: hex.EncodeToString(public), Class: "record", Source: "intake/records", Authority: "test", Shape: "mcp",
		Adapter: AdapterPin{Name: "adapter-mcp", Version: "stand-in", Digest: digest([]byte("adapter-mcp stand-in"))}, Endpoint: &endpoint, Tools: []string{"lookup"}}, key
}

// skippedSourceInput is a mapping of three operations, asked for the case's
// request, and the records of the two that are acquired. intake reads the
// request and its type and resolves them. sponsor reads the request's
// sponsor; its record names none, so its claim is unknown. endorsement
// declares two source parameters, each a fact target of an earlier source,
// approver from sponsor's /sponsor/id and requestType from intake's
// /intake/type, and is skipped: sponsor's claim is not resolved
// (docs/MAPPING-V2.md, "Missing/unknown/absent upstream outputs skip the
// dependent source and record `dependency-unavailable`"), so it has no
// record and no request.
func skippedSourceInput(t *testing.T, p InputProfile, key ed25519.PrivateKey, at time.Time) Input {
	t.Helper()
	intake := `{"ruleVersion":"1","parameters":{"requestId":"string"},"clauses":[` +
		`{"when":{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"requestId"},{"op":"exists","field":"/request"}]},` +
		`"claim":{"facts":[{"pointer":"/request","from":"/request"},{"pointer":"/intake/type","from":"/request/type"}],"evidence":{"intake-form":"present"},"acquisitionStatus":"resolved"},"reason":"matching-request"},` +
		`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"intake-form":"unknown"},"acquisitionStatus":"unknown"},"reason":"no-matching-request"}]}`
	sponsor := `{"ruleVersion":"1","parameters":{"requestId":"string"},"clauses":[` +
		`{"when":{"op":"all","of":[{"op":"equalsParam","field":"/request","param":"requestId"},{"op":"exists","field":"/sponsor/id"}]},` +
		`"claim":{"facts":[{"pointer":"/sponsor/id","from":"/sponsor/id"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"sponsor-on-record"},` +
		`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"unknown"},"reason":"no-sponsor-on-record"}]}`
	endorsement := `{"ruleVersion":"1","parameters":{"approver":"string","requestType":"string"},"clauses":[` +
		`{"when":{"op":"all","of":[{"op":"equalsParam","field":"/approver","param":"approver"},{"op":"equalsParam","field":"/requestType","param":"requestType"},{"op":"exists","field":"/endorsement"}]},` +
		`"claim":{"facts":[{"pointer":"/endorsement","from":"/endorsement"}],"evidence":{"sponsor-endorsement":"present"},"acquisitionStatus":"resolved"},"reason":"endorsed"},` +
		`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"sponsor-endorsement":"unknown"},"acquisitionStatus":"unknown"},"reason":"not-endorsed"}]}`
	operation := func(name, arguments, rule string, parameters map[string]Parameter) MappingSource {
		return MappingSource{Name: name, Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Parameters: parameters,
			Arguments: json.RawMessage(arguments), Read: SourceRead{Rule: json.RawMessage(rule)}}
	}
	m := InputMapping{Version: 2,
		Case: &CaseMapping{Parameters: map[string]Parameter{"requestId": {Pointer: "/id", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{
			operation("intake", `{"tool":"lookup","arguments":{"table":"requests","id":{"$param":"requestId"}}}`, intake, nil),
			operation("sponsor", `{"tool":"lookup","arguments":{"table":"sponsors","request":{"$param":"requestId"}}}`, sponsor, nil),
			operation("endorsement", `{"tool":"lookup","arguments":{"table":"endorsements","approver":{"$param":"approver"},"requestType":{"$param":"requestType"}}}`, endorsement,
				map[string]Parameter{"approver": {From: "sponsor", Pointer: "/sponsor/id", Type: "string"}, "requestType": {From: "intake", Pointer: "/intake/type", Type: "string"}}),
		},
		UnmappedEvidence: []string{"sensitive-data-approvals"},
	}
	return Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{"id":"REQ-7"}`), Sources: map[string]SourceValue{
		"intake":  {Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{"table":"requests","id":"REQ-7"}}`), []byte(`{"id":"REQ-7","request":`+standInRequest+`}`), at, 0)},
		"sponsor": {Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{"table":"sponsors","request":"REQ-7"}}`), []byte(`{"request":"REQ-7","sponsor":null}`), at, 1)},
	}}}
}

// requireSkippedSource holds a preparation of skippedSourceInput to the form
// Runner writes for a skipped source, of every kind: its outcome carries its
// claim, of no facts and no availability, with status unknown and reason
// dependency-unavailable, the parameters that have a value and no request;
// each of its lineage entries carries the status unknown, not the reason, and
// the reason dependency-unavailable, no value, no basis and no receipt, and
// dependencies, the source parameters it waited on, in the order of their
// names: approver, from sponsor, before requestType, from intake, though
// intake comes first among the sources.
func requireSkippedSource(t *testing.T, p *Preparation) {
	t.Helper()
	if p == nil || len(p.Outcomes) != 4 || len(p.Cites) != 2 {
		t.Fatalf("not the preparation of the three sources, two acquired: %s", encode(p))
	}
	sponsor, skipped := p.Outcomes[2], p.Outcomes[3]
	if sponsor.Name != "sponsor" || sponsor.Status != "unknown" || sponsor.Reason != "no-sponsor-on-record" {
		t.Fatalf("the source endorsement waits on is not unknown: %+v", sponsor)
	}
	if skipped.Name != "endorsement" || skipped.Status != "unknown" || skipped.Reason != "dependency-unavailable" || len(skipped.Arguments) != 0 {
		t.Fatalf("the skipped source's outcome is %s", encode(skipped))
	}
	if claim, _ := canonical(skipped.Claim); string(claim) != `{"acquisitionStatus":"unknown","basis":[],"evidenceAvailability":{},"facts":{},"reason":"dependency-unavailable"}` {
		t.Fatalf("the skipped source's claim is %s", skipped.Claim)
	}
	var parameters map[string]any
	if err := json.Unmarshal(skipped.Parameters, &parameters); err != nil || len(parameters) != 3 || parameters["requestId"] != "REQ-7" || parameters["requestType"] != "data-access" || parameters["runAt"] != p.VerifiedAt {
		t.Fatalf("the skipped source's parameters are %s, not those that have a value", skipped.Parameters)
	}
	want := `[{"parameter":"approver","source":"sponsor","pointer":"/sponsor/id"},{"parameter":"requestType","source":"intake","pointer":"/intake/type"}]`
	entries := 0
	for _, l := range p.Lineage {
		if l.Source != "endorsement" {
			if len(l.Dependencies) != 0 {
				t.Fatalf("%s's entry names dependencies: %s", l.Source, encode(l))
			}
			continue
		}
		entries++
		if l.Status != "unknown" {
			t.Fatalf("the skipped source's entry for %s has the status %q, not unknown", l.Target, l.Status)
		}
		if l.Reason != "dependency-unavailable" || l.Present || len(l.Basis) != 0 || l.Receipt != nil || l.Class != "record" || string(encode(l.Dependencies)) != want {
			t.Fatalf("the skipped source's entry is %s", encode(l))
		}
	}
	if entries != 2 {
		t.Fatalf("the skipped source has %d lineage entries, not one for /endorsement and one for sponsor-endorsement", entries)
	}
}

// A source whose source parameter has no value is skipped, and Runner writes
// its outcome and lineage in one form, the one the vector
// skipped-source-dependency-unavailable holds (judgment-pack-spec#189):
// computed here, and as the recorded export retains it.
func TestASkippedSourceKeepsItsClaimAndNamesWhatItWaitedOn(t *testing.T) {
	p, key := skippedSourceProfile(t)
	at := time.Now().UTC().Truncate(time.Second)
	got, err := normalizeV2(skippedSourceInput(t, p, key, at), []InputProfile{p}, at)
	if err != nil {
		t.Fatal(err)
	}
	requireSkippedSource(t, got.Preparation)
	if string(got.Facts) != `{"intake":{"type":"data-access"},"request":`+standInRequest+`}` || string(got.Evidence) != `{"intake-form":"present"}` {
		t.Fatalf("the run's facts are %s and its evidence %s", got.Facts, got.Evidence)
	}

	raw, err := os.ReadFile("testdata/mapping-v2-skipped-source/run.json")
	if err != nil {
		t.Fatal(err)
	}
	var recorded VerificationBundle
	if err = strictJSON(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	requireSkippedSource(t, recorded.Run.Input.Preparation)
}

// A run with a skipped source is evaluated, signed, witnessed and bound, and
// verified offline: the skipped source has no record and no receipt, and its
// targets are counted with their class. The export in
// testdata/mapping-v2-skipped-source was written by this test, under the
// Runtime CI pins, with JPACK_V2_SKIPPED_EXPORT_DIR set, and the vector
// skipped-source-dependency-unavailable was generated from it.
func TestASkippedSourceRunIsSignedWitnessedAndBound(t *testing.T) {
	cfg := testConfig(t)
	keyPath := signingKey(t)
	cfg.SigningKey = keyPath
	cfg.Runtime = signingRuntimeWithKey(t, cfg.Runtime, filepath.Join(t.TempDir(), "signing.log"), keyPath)
	p, key := skippedSourceProfile(t)
	cfg.InputProfiles = []InputProfile{p}
	input := skippedSourceInput(t, p, key, time.Now().UTC().Truncate(time.Second))
	bundle := signedAndWitnessedExport(t, cfg, input, p, "A skipped source, signed and witnessed", "skipped-source")
	v, err := VerifyInputs(encode(bundle), cfg.InputProfiles, bundle.ReleaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if v.Classes.Record != 6 || v.Classes.Asserted != 0 || len(v.Unsigned) != 0 {
		t.Fatalf("the skipped source's targets are not counted with their class: %+v %+v", v.Classes, v.Unsigned)
	}
	requireSkippedSource(t, bundle.Run.Input.Preparation)
	if runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN")) && (v.Binding == nil || v.Binding.Status != BindingMatches) {
		t.Fatalf("the record does not bind the preparation: %+v", v.Binding)
	}
	writeBoundFixture(t, os.Getenv("JPACK_V2_SKIPPED_EXPORT_DIR"), bundle, p)
}

// acquiredDependentInput is skippedSourceInput with sponsor's record naming
// the sponsor S-1, so that sponsor resolves and endorsement is acquired with
// both its source parameters valued, approver S-1 and requestType
// data-access, carried into its request. Its record, signed as the others
// are, names the endorsement when endorsed, and its claim is resolved;
// otherwise it names none, and its claim is unknown, not-endorsed.
func acquiredDependentInput(t *testing.T, p InputProfile, key ed25519.PrivateKey, at time.Time, endorsed bool) Input {
	t.Helper()
	i := skippedSourceInput(t, p, key, at)
	result := `{"approver":"S-1","requestType":"data-access"}`
	if endorsed {
		result = `{"approver":"S-1","endorsement":{"approved":true},"requestType":"data-access"}`
	}
	i.Source.Sources["sponsor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{"table":"sponsors","request":"REQ-7"}}`), []byte(`{"request":"REQ-7","sponsor":{"id":"S-1"}}`), at, 1)}
	i.Source.Sources["endorsement"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{"table":"endorsements","approver":"S-1","requestType":"data-access"}}`), []byte(result), at, 2)}
	return i
}

// requireAcquiredDependent holds a preparation of acquiredDependentInput to
// the form Runner writes for an acquired source that declares source
// parameters: its outcome carries every parameter's value and the request
// they were carried into, and each of its lineage entries carries its
// receipt, the outcome's status and dependencies, every source parameter it
// declares, in the order of their names, approver from sponsor before
// requestType from intake, as for a skipped source (spec/dialect.md §9.3).
// No other source's entry names dependencies.
func requireAcquiredDependent(t *testing.T, p *Preparation, endorsed bool) {
	t.Helper()
	if p == nil || len(p.Outcomes) != 4 || len(p.Cites) != 3 {
		t.Fatalf("not the preparation of the three sources, each acquired: %s", encode(p))
	}
	status, reason := "unknown", "not-endorsed"
	if endorsed {
		status, reason = "resolved", "endorsed"
	}
	sponsor, acquired := p.Outcomes[2], p.Outcomes[3]
	if sponsor.Name != "sponsor" || sponsor.Status != "resolved" || sponsor.Reason != "sponsor-on-record" {
		t.Fatalf("the source endorsement waits on is not resolved: %+v", sponsor)
	}
	if acquired.Name != "endorsement" || acquired.Status != status || acquired.Reason != reason {
		t.Fatalf("the dependent source's outcome is %s", encode(acquired))
	}
	if arguments, _ := canonical(acquired.Arguments); string(arguments) != `{"arguments":{"approver":"S-1","requestType":"data-access","table":"endorsements"},"tool":"lookup"}` {
		t.Fatalf("the dependent source's request is %s, not its parameters' values carried into it", acquired.Arguments)
	}
	var parameters map[string]any
	if err := json.Unmarshal(acquired.Parameters, &parameters); err != nil || len(parameters) != 4 || parameters["approver"] != "S-1" || parameters["requestId"] != "REQ-7" || parameters["requestType"] != "data-access" || parameters["runAt"] != p.VerifiedAt {
		t.Fatalf("the dependent source's parameters are %s, not every one valued", acquired.Parameters)
	}
	want := `[{"parameter":"approver","source":"sponsor","pointer":"/sponsor/id"},{"parameter":"requestType","source":"intake","pointer":"/intake/type"}]`
	var targets []string
	for _, l := range p.Lineage {
		if l.Source != "endorsement" {
			if len(l.Dependencies) != 0 {
				t.Fatalf("%s's entry names dependencies: %s", l.Source, encode(l))
			}
			continue
		}
		targets = append(targets, l.Target)
		if string(encode(l.Dependencies)) != want {
			t.Fatalf("the acquired dependent source's entry for %s names the dependencies %s, not every declared source parameter in the order of their names", l.Target, encode(l.Dependencies))
		}
		if l.Status != status || l.Reason != reason || l.Receipt == nil || *l.Receipt != p.Cites[2] || l.Class != "record" {
			t.Fatalf("the acquired dependent source's entry is %s", encode(l))
		}
	}
	if string(encode(targets)) != `["/endorsement","sponsor-endorsement"]` {
		t.Fatalf("the acquired dependent source has the lineage entries %v, not one for /endorsement and one for sponsor-endorsement", targets)
	}
}

// acquiredDependentFixtures are the exports an acquired dependent source's
// runs were recorded into, by whether endorsement's record names the
// endorsement: the vectors acquired-dependent-source and
// acquired-dependent-source-unknown were generated from them
// (judgment-pack-runner#107).
var acquiredDependentFixtures = map[bool]string{true: "mapping-v2-acquired-dependent", false: "mapping-v2-acquired-dependent-unknown"}

// An acquired source that declares source parameters carries dependencies on
// each of its lineage entries, as a skipped one does: every parameter it
// declares, valued, in the order of their names, whether its claim is
// resolved or unknown. Computed here, and as the recorded exports retain it.
func TestAnAcquiredDependentSourceNamesEveryParameterItDeclares(t *testing.T) {
	for _, endorsed := range []bool{true, false} {
		t.Run(acquiredDependentFixtures[endorsed], func(t *testing.T) {
			p, key := skippedSourceProfile(t)
			at := time.Now().UTC().Truncate(time.Second)
			got, err := normalizeV2(acquiredDependentInput(t, p, key, at, endorsed), []InputProfile{p}, at)
			if err != nil {
				t.Fatal(err)
			}
			requireAcquiredDependent(t, got.Preparation, endorsed)
			facts, evidence := `{"intake":{"type":"data-access"},"request":`+standInRequest+`,"sponsor":{"id":"S-1"}}`, `{"intake-form":"present","sponsor-endorsement":"unknown"}`
			if endorsed {
				facts, evidence = `{"endorsement":{"approved":true},"intake":{"type":"data-access"},"request":`+standInRequest+`,"sponsor":{"id":"S-1"}}`, `{"intake-form":"present","sponsor-endorsement":"present"}`
			}
			if string(got.Facts) != facts || string(got.Evidence) != evidence {
				t.Fatalf("the run's facts are %s and its evidence %s", got.Facts, got.Evidence)
			}

			raw, err := os.ReadFile(filepath.Join("testdata", acquiredDependentFixtures[endorsed], "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			var recorded VerificationBundle
			if err = strictJSON(raw, &recorded); err != nil {
				t.Fatal(err)
			}
			requireAcquiredDependent(t, recorded.Run.Input.Preparation, endorsed)
		})
	}
}

// A run with an acquired dependent source is evaluated, signed, witnessed and
// bound, and verified offline. The exports in
// testdata/mapping-v2-acquired-dependent and
// testdata/mapping-v2-acquired-dependent-unknown were written by this test,
// under the Runtime CI pins, with JPACK_V2_ACQUIRED_EXPORT_DIR set to the
// directory that holds them.
func TestAnAcquiredDependentSourceRunIsSignedWitnessedAndBound(t *testing.T) {
	for _, endorsed := range []bool{true, false} {
		name := acquiredDependentFixtures[endorsed]
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			keyPath := signingKey(t)
			cfg.SigningKey = keyPath
			cfg.Runtime = signingRuntimeWithKey(t, cfg.Runtime, filepath.Join(t.TempDir(), "signing.log"), keyPath)
			p, key := skippedSourceProfile(t)
			cfg.InputProfiles = []InputProfile{p}
			input := acquiredDependentInput(t, p, key, time.Now().UTC().Truncate(time.Second), endorsed)
			bundle := signedAndWitnessedExport(t, cfg, input, p, "An acquired dependent source, signed and witnessed", name)
			v, err := VerifyInputs(encode(bundle), cfg.InputProfiles, bundle.ReleaseDigest)
			if err != nil {
				t.Fatal(err)
			}
			if v.Classes.Record != 6 || v.Classes.Asserted != 0 || len(v.Unsigned) != 0 {
				t.Fatalf("the dependent source's targets are not counted with their class: %+v %+v", v.Classes, v.Unsigned)
			}
			requireAcquiredDependent(t, bundle.Run.Input.Preparation, endorsed)
			if runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN")) && (v.Binding == nil || v.Binding.Status != BindingMatches) {
				t.Fatalf("the record does not bind the preparation: %+v", v.Binding)
			}
			dir := os.Getenv("JPACK_V2_ACQUIRED_EXPORT_DIR")
			if dir != "" {
				dir = filepath.Join(dir, name)
				if err = os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeBoundFixture(t, dir, bundle, p)
		})
	}
}
