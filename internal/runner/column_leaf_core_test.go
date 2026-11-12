package runner

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectedLeafEvaluatorContractIsExactAndScoped(t *testing.T) {
	selected := selectedContract()
	if !supportedEvaluatorSpec("0.2.0-draft", nil) || !supportedEvaluatorSpec("0.2.0-draft", selected) || !supportedEvaluatorSpec("0.3.0", selected) {
		t.Fatal("supported ordinary or selected evaluator removed")
	}
	// Ordinary runs admit Core 0.3.0 since #188 (core_selection_test.go).
	if supportedEvaluatorSpec("0.4.0", selected) || supportedEvaluatorSpec("0.3.0-rc.2", selected) {
		t.Fatal("selected evaluator admission enlarged to unknown Core")
	}
	bad := *selected
	bad.Spec.Revision = strings.Repeat("0", 40)
	if supportedEvaluatorSpec("0.3.0", &bad) {
		t.Fatal("uninstalled selector admitted Core0.3")
	}
	r := Release{Pack: `{"specVersion":"0.3.0"}`, PackID: "pack", PackVersion: "1", Sample: Input{Source: &SourceInput{ConversionContract: selected}}}
	if expectedEvaluatorSpec(r) != "0.3.0" {
		t.Fatal("release pack evaluator not retained")
	}
	if !evaluatorSpecMatches(r, "0.3.0") || evaluatorSpecMatches(r, "0.2.0-draft") || evaluatorSpecMatches(r, "0.4.0") {
		t.Fatal("Runtime evaluation evaluator binding lost")
	}
	matrix := `{"cases":[{"id":"one"}]}`
	report := `{"outputVersion":"2","command":"packs test","status":"passed","experimental":true,"evaluatorSpecVersion":"0.3.0","summary":{"total":1,"passed":1,"mismatched":0},"packs":[{"id":"target","packId":"pack","packVersion":"1","status":"passed","summary":{"total":1,"passed":1,"mismatched":0},"rows":[{"id":"one","status":"passed"}]}]}`
	if status, e := checkedReport(r, matrix, []byte(report)); e != nil || status != "passed" {
		t.Fatalf("matching selected report refused: %s %v", status, e)
	}
	for _, version := range []string{"0.2.0-draft", "0.4.0"} {
		if _, e := checkedReport(r, matrix, []byte(strings.Replace(report, `"evaluatorSpecVersion":"0.3.0"`, `"evaluatorSpecVersion":"`+version+`"`, 1))); e == nil {
			t.Fatal("mismatched evaluator report accepted", version)
		}
	}
	r.Sample.Source.ConversionContract = &bad
	if expectedEvaluatorSpec(r) != "" {
		t.Fatal("release tuple mismatch supplied expected evaluator")
	}
	// Core 0.5.0 is admitted on ordinary runs only (#232): the selected path
	// refuses it by name, and keeps the refusal it had for every other
	// version it does not evaluate.
	if !supportedEvaluatorSpec("0.5.0", nil) || supportedEvaluatorSpec("0.5.0", selected) || supportedEvaluatorSpec("0.5.0", &bad) {
		t.Fatal("selected evaluator admission enlarged to Core 0.5.0")
	}
	for declared, want := range map[string]string{
		"0.5.0":                 `The selected retained-column path evaluates packs that declare specVersion 0.2.0-draft or 0.3.0, the Core its companion (Spec v0.3.0) is written against; this pack declares "0.5.0", which this runner evaluates on ordinary runs only.`,
		"0.4.0":                 `This runner evaluates packs that declare specVersion 0.2.0-draft or 0.3.0; this pack declares "0.4.0".`,
		"0.4.0-draft":           `This runner evaluates packs that declare specVersion 0.2.0-draft or 0.3.0; this pack declares "0.4.0-draft".`,
		"0.5.0-draft":           `This runner evaluates packs that declare specVersion 0.2.0-draft or 0.3.0; this pack declares "0.5.0-draft".`,
		"":                      `This runner evaluates packs that declare specVersion 0.2.0-draft or 0.3.0; this pack declares none.`,
		strings.Repeat("9", 65): `This runner evaluates packs that declare specVersion 0.2.0-draft or 0.3.0; this pack declares another version.`,
	} {
		if got := unsupportedEvaluatorSpec(declared, selected); got != want {
			t.Fatalf("selected refusal of %q: %q", declared, got)
		}
	}
	released := Release{Pack: `{"id":"pack","version":"1","specVersion":"0.5.0"}`, PackID: "pack", PackVersion: "1", Sample: Input{Source: &SourceInput{ConversionContract: selected}}}
	if expectedEvaluatorSpec(released) != "" || evaluatorSpecMatches(released, "0.5.0") {
		t.Fatal("a selected Core 0.5.0 release selected an evaluator")
	}
	if _, e := checkedReport(released, matrix, []byte(strings.Replace(report, `"evaluatorSpecVersion":"0.3.0"`, `"evaluatorSpecVersion":"0.5.0"`, 1))); e == nil {
		t.Fatal("a selected Core 0.5.0 report accepted")
	}
	calls := 0
	invoker := func(context.Context, string, string, string, []string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("not invoked")
	}
	bin := filepath.Join(t.TempDir(), "runtime-is-never-started")
	if _, _, _, _, e := evaluateWithInvoker(context.Background(), bin, released, sample(), filepath.Join(t.TempDir(), "evaluation"), true, signingInputs{}, invoker); e == nil || e.Error() != "unsupported retained release evaluator contract" || calls != 0 {
		t.Fatal("a selected Core 0.5.0 release reached a Runtime", e, calls)
	}
	if e := checkEvaluatorSpec(VerificationBundle{Release: released, Run: Run{State: "completed", Result: json.RawMessage(`{"evaluatorSpecVersion":"0.5.0"}`)}}); !errors.Is(e, ErrEvaluatorSpec) {
		t.Fatal("verify-run accepted a selected Core 0.5.0 record", e)
	}
}
