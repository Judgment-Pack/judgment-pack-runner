package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// researchRecording is one run the research vectors are generated from:
// its name, the vector's (spec/dialect.md §11: <example>-<item>-<variant>),
// and its plan.
type researchRecording struct {
	name string
	plan func(*researchStandIn) researchPlan
}

// researchRecordings are the runs of both examples of the specification's
// fixture plan that a preparation records (§11, "The two fixture lists to
// record"): valid-types; the six presence readings; stale and conflicting
// observations; the two failed-fallback readings through a reconciliation;
// and the second installation's binding of valid-types, recorded right
// after it, from which the vector changed-binding-old-preparation is made.
// The refusals of those items are reader tests, not fixtures (research_test.go
// holds them for Runner). Parent and child is the workflow's (RFC 0020), and
// is not recorded here.
func researchRecordings() []researchRecording {
	var out []researchRecording
	for _, example := range []struct {
		name string
		plan func(*researchStandIn) researchPlan
	}{{"gtm-company-person", gtmPlan}, {"work-item-triage", workItemPlan}} {
		plan := example.plan
		with := func(change func(*researchStandIn, *researchPlan)) func(*researchStandIn) researchPlan {
			return func(h *researchStandIn) researchPlan { p := plan(h); change(h, &p); return p }
		}
		out = append(out, researchRecording{example.name + "-valid-types-all-six", plan})
		out = append(out, researchRecording{example.name + "-changed-binding-second-installation", with(secondInstallation)})
		for _, variant := range []string{"optional-omitted", "required-not-found", "integer-zero", "decimal-zero", "boolean-false", "text-empty"} {
			out = append(out, researchRecording{example.name + "-presence-" + variant, with(func(h *researchStandIn, p *researchPlan) { presence(p, variant) })})
		}
		out = append(out, researchRecording{example.name + "-stale-and-conflicting-unknown", with(func(h *researchStandIn, p *researchPlan) { staleAndConflicting(p) })})
		name := example.name
		for _, variant := range []string{"all-unavailable", "secondary-found"} {
			out = append(out, researchRecording{example.name + "-failed-fallback-" + variant, func(h *researchStandIn) researchPlan {
				p := fallbackPlan(h, name)
				fallback(&p, variant)
				return p
			}})
		}
	}
	return out
}

// Each research run is evaluated, signed, witnessed and bound, and verified
// offline, its research sources bound at import and prepared from the
// responses the stand-in served, as a run of an operation is: verify-run
// binds the import again and replays every retained response, asking
// nothing. The exports in testdata/research were written by this test,
// under Runtime 0.31.0 or later, with JPACK_RESEARCH_EXPORT_DIR set, and the
// research vectors were generated from them. A second installation's run
// is recorded right after the first's, by the same gateway in a session of
// its own, so that the first's preparation can be held to its inputs.
func TestResearchRunsAreSignedWitnessedAndBound(t *testing.T) {
	testConfig(t)
	dir := os.Getenv("JPACK_RESEARCH_EXPORT_DIR")
	var h *researchStandIn
	for _, r := range researchRecordings() {
		// The second installation's run is of the same gateway as the run
		// before it, in a session of its own.
		if h == nil || !strings.HasSuffix(r.name, "-changed-binding-second-installation") {
			h = newResearchStandIn(t)
		} else {
			h.session, h.next, h.previous = researchSession+"-2", 0, nil
		}
		t.Run(r.name, func(t *testing.T) { recordResearchRun(t, h, r, dir) })
	}
}

// recordResearchRun completes the research run r, from the stand-in h,
// signed, witnessed and bound, verifies it offline, and writes its export
// into dir/<name> when dir is set.
func recordResearchRun(t *testing.T, h *researchStandIn, r researchRecording, dir string) {
	t.Helper()
	plan := r.plan(h)
	run := testConfig(t)
	keyPath := signingKey(t)
	run.SigningKey = keyPath
	run.Runtime = signingRuntimeWithKey(t, run.Runtime, filepath.Join(t.TempDir(), "signing.log"), keyPath)
	run.InputProfiles = plan.profiles
	input := h.researchInput(t, plan, time.Now().UTC().Truncate(time.Second))
	bundle := signedAndWitnessedExportOf(t, run, input, plan.profiles, "Research: "+r.name, r.name)
	v, err := VerifyInputs(encode(bundle), plan.profiles, bundle.ReleaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Run.Input.Source.Import == nil || bundle.Run.Input.Preparation.Version != 3 || v.Classes.Generated == 0 || v.Freshness == nil {
		t.Fatalf("the run is not of a research dialect bound at import: %+v", v.Classes)
	}
	if runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN")) && (v.Binding == nil || v.Binding.Status != BindingMatches) {
		t.Fatalf("the record does not bind the preparation: %+v", v.Binding)
	}
	if dir != "" {
		out := filepath.Join(dir, r.name)
		if err = os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		writeBoundFixtureOf(t, out, bundle, plan.profiles)
	}
}
