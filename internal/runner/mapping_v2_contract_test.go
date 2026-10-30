package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Dialect §4.1 gives adapter-http one additional request rule: under a
// profile whose adapter is adapter-http, a request's query, when present, is
// an object, not a template form, each of whose members is a string template.
// It is checked before acquisition, and again when retained inputs are
// prepared (#200).
func TestAdapterHTTPQueryIsAnObjectOfStringTemplates(t *testing.T) {
	base, profile, _, at := v2Fixture(t)
	profile.Shape, profile.Tools = "http", nil
	profile.Adapter.Name = "adapter-http"
	base.Source.Mapping.Case.Parameters["vendorId"] = Parameter{Pointer: "/vendorId", Type: "integer"}
	base.Source.Mapping.Case.Parameters["search"] = Parameter{Pointer: "/search", Type: "string"}
	base.Source.Mapping.Case.Parameters["when"] = Parameter{Pointer: "/when", Type: "timestamp"}
	base.Source.Case = json.RawMessage(`{"vendorId":7,"search":"synthetic query","when":"2026-01-01T00:00:00Z"}`)
	base.Source.Mapping.Sources[0].ProfileDigest = profileHash(profile)
	base.Source.Sources = map[string]SourceValue{}
	mismatch := func(t *testing.T, err error) {
		t.Helper()
		if _, code := verificationOutcome(err); err == nil || code != codeMismatch {
			t.Fatalf("not refused as profile-mismatch: %v (code %s)", err, code)
		}
	}
	for _, c := range []struct {
		name, arguments string
		valid           bool
	}{
		{"absent", `{"path":"/records"}`, true},
		{"empty", `{"query":{}}`, true},
		{"strings", `{"query":{"literal":"seven","text":{"$text":"id={{vendorId}}"},"string":{"$param":"search"},"timestamp":{"$param":"when"}}}`, true},
		{"whole parameter", `{"query":{"$param":"search"}}`, false},
		{"whole text", `{"query":{"$text":"id={{vendorId}}"}}`, false},
		{"string query", `{"query":"query"}`, false},
		{"null query", `{"query":null}`, false},
		{"array query", `{"query":[]}`, false},
		{"integer member", `{"query":{"id":7}}`, false},
		{"boolean member", `{"query":{"id":true}}`, false},
		{"null member", `{"query":{"id":null}}`, false},
		{"object member", `{"query":{"id":{"nested":"seven"}}}`, false},
		{"array member", `{"query":{"id":["seven"]}}`, false},
		{"integer parameter", `{"query":{"id":{"$param":"vendorId"}}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			input := cloneInput(base)
			input.Source.Mapping.Sources[0].Arguments = json.RawMessage(c.arguments)
			plan, err := nextInput(input, []InputProfile{profile}, at)
			if c.valid {
				if err != nil || plan.Next == nil {
					t.Fatal("valid query did not reach acquisition planning", err)
				}
			} else {
				mismatch(t, err)
				_, err = prepareV2Mode(input, []InputProfile{profile}, at, false, nil, true)
				mismatch(t, err)
			}
		})
	}
	// Another HTTP-shaped adapter still owns its provider request form.
	other := profile
	other.Adapter.Name = "another-http-adapter"
	input := cloneInput(base)
	input.Source.Mapping.Sources[0].ProfileDigest = profileHash(other)
	input.Source.Mapping.Sources[0].Arguments = json.RawMessage(`{"query":{"$param":"search"}}`)
	if plan, err := nextInput(input, []InputProfile{other}, at); err != nil || plan.Next == nil {
		t.Fatal("adapter-http's rule narrowed another adapter", err)
	}
}

// A preparation's evidence-availability document is an object, {} when no
// target names a requirement (spec/dialect.md §8.1), and it is the evidence
// an evaluation takes (#200).
func TestDialectPreparationRetainsAnEmptyEvidenceObject(t *testing.T) {
	for _, version := range []int{2, 3} {
		input := Input{Source: &SourceInput{Mapping: InputMapping{Version: version, Case: &CaseMapping{Facts: []FactMapping{{Target: "/request", Source: "/request"}}, Evidence: []EvidenceMapping{}}}, Case: json.RawMessage(`{"request":{"id":7}}`)}}
		prepared, err := normalizeV2(input, nil, time.Now().UTC().Truncate(time.Second))
		if err != nil || string(prepared.Evidence) != `{}` || string(member(encode(prepared), "evidence")) != `{}` {
			t.Fatalf("dialect %d lost its derived empty evidence object: %s %v", version, encode(prepared), err)
		}
	}
	// A v1 file mapping is not a dialect preparation; its optional evidence
	// input and not-mapped export keep their own semantics.
	legacy := fileInput(`{"request":{"id":7}}`, InputMapping{Version: 1, Provider: "local-file", Facts: []FactMapping{{Target: "/request", Source: "/request"}}, Evidence: []EvidenceMapping{}})
	prepared, err := normalizeInput(legacy)
	if err != nil || len(prepared.Evidence) != 0 || prepared.Preparation != nil {
		t.Fatal("v1 optional evidence semantics changed", err, string(encode(prepared)))
	}
}

// Local TLS research responses and the real Runtime exercise the shared
// preparation, evaluation and export seam. The source content and signer are
// synthetic stand-ins, with no live model or provider call.
func TestResearchRuntimeRetainsTheDerivedEmptyEvidenceObject(t *testing.T) {
	cfg := testConfig(t)
	d := researchDocsOf(t, workItemPlan)
	d.dialect["case"].(map[string]any)["evidence"] = []any{}
	d.dialect["unmappedEvidence"] = []any{"intake-form", "sponsor-endorsement", "sensitive-data-approvals"}
	input := d.input(t)
	d.h.server.Close()
	cfg.InputProfiles = d.profiles
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.createJob("Explicit empty research evidence", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.submit(job.ID, "empty-evidence", input)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "completed" || string(done.Input.Evidence) != `{}` || string(member(done.Audit, "inputs", "evidence")) != `{}` || string(member(done.Audit, "inputs", "evidenceSupplied")) != `true` {
		t.Fatalf("the evaluation did not retain the prepared evidence: state=%s evidence=%s audit inputs=%s problem=%s", done.State, done.Input.Evidence, member(done.Audit, "inputs"), done.Problem)
	}
	bundle := verificationExport(release, done, 2)
	if err = VerifyRun(encode(bundle), d.profiles, bundle.ReleaseDigest); err != nil {
		t.Fatal("the export does not verify", err)
	}
	for _, omitted := range []string{"evidence", "facts"} {
		bad := bundle
		if omitted == "evidence" {
			bad.Run.Input.Evidence = nil
		} else {
			bad.Run.Input.Facts = nil
		}
		if _, err = verifyPreparation(bad, d.profiles, nil); err == nil {
			t.Fatalf("omitted retained %s accepted against the derived Runtime inputs", omitted)
		}
	}
	// An earlier record of omitted (null) evidence is refused: verification
	// does not repair it into the object the dialect derives.
	bad := bundle
	audit := decoded(t, done.Audit).(map[string]any)
	audit["inputs"].(map[string]any)["evidence"] = nil
	audit["inputs"].(map[string]any)["evidenceSupplied"] = false
	bad.Run.Audit = encode(audit)
	if _, err = verifyPreparation(bad, d.profiles, nil); err == nil {
		t.Fatal("an earlier null evidence record was silently repaired")
	}
}

// A run's retained facts and evidence are the inputs its export says were
// evaluated. Verification recomputes them from the retained acquisitions and
// refuses an export whose retained run inputs are not that derivation,
// omitted ones included (#200). The export is the checked-in http-shaped run.
func TestVerificationRefusesRetainedInputsThatAreNotTheDerivation(t *testing.T) {
	dir := "testdata/mapping-v2-http-shaped/"
	raw, err := os.ReadFile(dir + "run.json")
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	rawProfiles, err := os.ReadFile(dir + "profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := ParseInputProfiles(rawProfiles)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(string(trusted))
	if _, err = VerifyInputs(raw, profiles, digest); err != nil {
		t.Fatal("the recorded export does not verify", err)
	}
	tamper := func(t *testing.T, change func(input map[string]any)) ([]byte, VerificationBundle) {
		t.Helper()
		export := decoded(t, raw).(map[string]any)
		change(export["run"].(map[string]any)["input"].(map[string]any))
		var bundle VerificationBundle
		if err := strictJSON(encode(export), &bundle); err != nil {
			t.Fatal(err)
		}
		return encode(export), bundle
	}
	// Omitted retained inputs were not compared with the derivation at all.
	for _, omitted := range []string{"facts", "evidence"} {
		t.Run(omitted+" omitted", func(t *testing.T) {
			export, bundle := tamper(t, func(input map[string]any) { delete(input, omitted) })
			if _, err := verifyPreparation(bundle, profiles, nil); err == nil || err.Error() != "retained facts or evidence do not match verified derivation" {
				t.Error("omitted retained inputs were not refused as differing from the derivation:", err)
			}
			if _, err := VerifyInputs(export, profiles, digest); err == nil {
				t.Error("the export was verified with omitted retained inputs")
			}
		})
	}
	// Retained inputs that are present and differ were refused before, and
	// still are, by the preparation's own comparison.
	for _, changed := range []string{"facts", "evidence"} {
		t.Run(changed+" null", func(t *testing.T) {
			export, bundle := tamper(t, func(input map[string]any) { input[changed] = nil })
			if _, err := verifyPreparation(bundle, profiles, nil); err == nil {
				t.Error("null retained inputs were accepted")
			}
			if _, err := VerifyInputs(export, profiles, digest); err == nil {
				t.Error("the export was verified with null retained inputs")
			}
		})
	}
}
