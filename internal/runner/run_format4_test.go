package runner

import (
	"encoding/json"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
)

// triageFour is a dialect of format 4.0.0 for the triage pack: its case
// copies three members of the request and the two evidence requirements,
// and its rule of version 2.0.0 writes the fourth member.
const triageFour = `{"formatVersion":"4.0.0","id":"urn:judgmentpack:dialect:triage-four?a&b","version":"0.1.0",
 "unmappedEvidence":["sensitive-data-approvals"],
 "case":{
  "facts":[{"target":"/request/type","source":"/facts/request/type"},{"target":"/request/completeness","source":"/facts/request/completeness"},{"target":"/request/appropriateness","source":"/facts/request/appropriateness"}],
  "evidence":[{"requirement":"intake-form","source":"/evidence/intake-form"},{"requirement":"sponsor-endorsement","source":"/evidence/sponsor-endorsement"}],
  "rule":{"ruleVersion":"2.0.0","parameters":{},"clauses":[{"when":{"op":"always"},"claim":{"facts":[{"pointer":"/request/embargoedInformationToUnauthorizedRecipients","value":false}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"no-embargo"}]}}}`

const triageFourCase = `{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass"}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`

// A run of a dialect of format 4.0.0 (docs/design/dialect-format-4.md,
// section 2): the preparation it records is the one verify-preparation reads
// from the same dialect and case, valid against the Spec's dialect-recording
// schema of version 2, and read back from the store as it was written; the
// release's digest is RFC 0020's for the format, over the canon bytes of the
// whole release, and the export verifies against it.
func TestARunOnAFormat4DialectRecordsWhatVerifyPreparationReads(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	must(t, err)
	defer func() { s.Close() }()
	var m InputMapping
	must(t, strictJSON([]byte(triageFour), &m))
	release, run := boundRun(t, s, Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(triageFourCase)}})
	if run.State != "completed" || run.Input.Preparation == nil {
		t.Fatal("the run did not complete with a preparation:", run.State, run.Reason)
	}
	if run.Input.Preparation.formatVersion != format4 {
		t.Fatal("the run's preparation is not of format 4.0.0")
	}
	recorded := encode(run.Input.Preparation)
	if !readSchema2(t).Valid(recorded) {
		t.Fatalf("the run's preparation is not valid against the recording schema of version 2:\n%s", recorded)
	}
	report := VerifyPreparation(PreparationFixture{Dialect: []byte(triageFour), Case: []byte(triageFourCase), Sources: []byte(`{}`), Profiles: []byte(`[]`), Preparation: recorded})
	if report.Result != ResultReading {
		t.Fatalf("verify-preparation does not read the run's preparation: %s %s %s", report.Result, report.Code, report.Reason)
	}
	// The store gives the preparation back as it was written.
	held, err := s.store.run(run.ID)
	must(t, err)
	if string(encode(held.Input.Preparation)) != string(recorded) {
		t.Fatal("the preparation read back from the store differs")
	}
	// RFC 0020 for format 4.0.0: the SHA-256 of the canon bytes of the whole
	// release as retained, not the pinned encoding of earlier formats.
	canon, err := derivation.Canon(encode(release))
	must(t, err)
	if got := releaseDigest(release); got != digest(canon) {
		t.Fatal("the release digest is not over the canon bytes of the release", got)
	}
	pinned, _ := canonical(encode(release))
	if digest(pinned) == digest(canon) {
		t.Fatal("the release's two encodings agree: the test shows nothing")
	}
	exported := get(t, s.Handler("test"), "/v1/runs/"+run.ID+"/verification", 200)
	if _, err = VerifyInputs(exported, nil, releaseDigest(release)); err != nil {
		t.Fatal("the run's export does not verify against the release digest:", err)
	}
}

// releaseDigest is releaseDigestOf for a release the test knows has a
// digest.
func releaseDigest(r Release) string {
	d, err := releaseDigestOf(r)
	if err != nil {
		panic(err)
	}
	return d
}

// verificationExport is verificationExportOf for a release the test knows
// has a digest.
func verificationExport(release Release, run Run, asked int) VerificationBundle {
	b, err := verificationExportOf(release, run, asked)
	if err != nil {
		panic(err)
	}
	return b
}

// A release on a dialect of format 4.0.0 outside the canon domain (a
// fraction in what Runner adds to it) has no release digest: an error, never
// a value that two such releases would share, and no export.
func TestAFormat4ReleaseOutsideTheCanonDomainHasNoDigest(t *testing.T) {
	var m InputMapping
	must(t, strictJSON([]byte(triageFour), &m))
	r := Release{ID: "release_fraction", InputMapping: &m, Validation: json.RawMessage(`{"score":0.5}`)}
	if d, err := releaseDigestOf(r); err == nil || d != "" {
		t.Fatalf("a release outside the canon domain has a digest: %q %v", d, err)
	}
	if releaseDigestIs(r, "") {
		t.Fatal("a release with no digest matches the empty one")
	}
	if _, err := verificationExportOf(r, Run{}, 2); err == nil {
		t.Fatal("a release with no digest was exported")
	}
	if fourReleaseDigestible(r) == nil {
		t.Fatal("a release outside the canon domain can be saved")
	}
}
