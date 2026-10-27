package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestColumnPreparationSelectionMustMatchExplicitContext(t *testing.T) {
	input := columnCase(t, "text", `"A"`)
	input.Source.ConversionContract = selectedContract()
	prepared, e := normalizeV2(input, nil, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	fixture := columnFixture(prepared)
	fixture.ConversionContract = selectedContract()
	if report := VerifyPreparation(fixture); report.Result != ResultReading {
		t.Fatal(report)
	}
	for _, name := range []string{"missing-explicit-selection", "missing-retained-selection", "different-retained-pin", "aliased-retained-selection", "null-retained-selection"} {
		t.Run(name, func(t *testing.T) {
			changed := fixture
			var preparation map[string]json.RawMessage
			if e := json.Unmarshal(fixture.Preparation, &preparation); e != nil {
				t.Fatal(e)
			}
			switch name {
			case "missing-explicit-selection":
				changed.ConversionContract = nil
			case "missing-retained-selection":
				delete(preparation, "conversionContract")
			case "different-retained-pin":
				other := *selectedContract()
				other.Spec.Revision = strings.Repeat("0", 40)
				preparation["conversionContract"] = encode(other)
			case "aliased-retained-selection":
				preparation["ConversionContract"] = preparation["conversionContract"]
				delete(preparation, "conversionContract")
			case "null-retained-selection":
				preparation["conversionContract"] = json.RawMessage(`null`)
			}
			changed.Preparation = encode(preparation)
			if report := VerifyPreparation(changed); report.Result != ResultRefusal || report.Code != codeMalformed || report.Reason != "preparation conversion selection differs" {
				t.Fatalf("explicit/retained selection guard lost: %+v", report)
			}
		})
	}
}
