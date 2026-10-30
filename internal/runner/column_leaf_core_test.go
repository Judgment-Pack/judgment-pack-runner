package runner

import (
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
}
