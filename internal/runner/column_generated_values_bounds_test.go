package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Source-derived old text-list domain:16 strings of1000 ASCII bytes. The full
// original map and typed values are retained even though Integer is selected.
func generatedListValueFixture(t *testing.T) producerFixture {
	t.Helper()
	f := selectedProducer(t)
	list := `["` + strings.Repeat("x", 1000) + `"` + strings.Repeat(`,"`+strings.Repeat("x", 1000)+`"`, 15) + `]`
	if len(list) != 16049 {
		t.Fatal("handwritten original list size", len(list))
	}
	f.snapshot = strings.Replace(f.snapshot, `"List":"A"`, `"List":`+string(encode(list)), 1)
	f.snapshot = strings.Replace(f.snapshot, `"List":{"status":"supported","value":["A","B"]}`, `"List":{"status":"supported","value":`+list+`}`, 1)
	f.frame = strings.Replace(f.frame, `"List":"A"`, `"List":`+string(encode(list)), 1)
	f.frame = strings.Replace(f.frame, `"List":{"status":"supported","valueJson":"[\"A\",\"B\"]"}`, `"List":{"status":"supported","valueJson":`+string(encode(list))+`}`, 1)
	f.input.Source.Mapping.Sources[0].Read.Copy.Facts[0].Conversion = json.RawMessage(`{"contractVersion":"1","column":"Integer","kind":"research-value"}`)
	f.sign(t)
	return f
}

func TestColumnGeneratedRawValuesCompleteProjectionAndPreparation(t *testing.T) {
	f := generatedListValueFixture(t)
	code, plan, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil)
	if code != 200 || plan.Input == nil {
		t.Fatalf("complete original16049B generated cell rejected: %d %s", code, body)
	}
	if string(plan.Input.Facts) != `{"value":7}` {
		t.Fatal("handwritten selected integer facts changed", string(plan.Input.Facts))
	}
	raw := plan.Input.Source.Sources["desk"].RetainedResponseJSON
	if raw != string(f.input.Source.Sources["desk"].Response) || !bytes.Contains([]byte(raw), f.carrier()) {
		t.Fatal("unselected original cell or complete carrier changed")
	}
	fixture := producerPreparationFixture(*plan.Input, f.profile)
	fixture.ConversionContract = selectedContract()
	if report := VerifyPreparation(fixture); report.Result != ResultReading {
		t.Fatal("offline original projection/preparation differs", report)
	}
	parsed, e := readColumnCarrier(f.carrier(), selectedContract())
	if e != nil {
		t.Fatal(e)
	}
	if e = validateColumnProducerRelation(parsed); e != nil {
		t.Fatal("original snapshot projection refused", e)
	}
	// A fully signed envelope cannot substitute an altered original raw cell.
	bad := f
	listValue, _ := columnSnapshotMember([]byte(bad.snapshot), "record", "values", "List")
	bad.snapshot = strings.Replace(bad.snapshot, string(listValue), `"A"`, 1)
	bad.sign(t)
	if code, _, _ := scopedEntry(t, bad.input, []InputProfile{bad.profile}, nil); code < 400 {
		t.Fatal("altered original raw projection accepted")
	}
	t.Logf("actual complete fixture snapshot=%d frame=%d carrier=%d; selected fact7, unselected raw List16049B preserved", len(f.snapshot), len(f.frame), len(f.carrier()))
}

func TestColumnGeneratedRawValuesSnapshotAggregateUnchanged(t *testing.T) {
	f := generatedListValueFixture(t)
	// Whitespace is a complete-reader boundary probe, not a maximum-family or
	// original-history claim. A524288B cell itself cannot fit this same snapshot.
	f.snapshot += strings.Repeat(" ", 524288-len(f.snapshot))
	f.sign(t)
	parsed, e := readColumnCarrier(f.carrier(), selectedContract())
	if e != nil || len(parsed.OriginalSnapshot) != 524288 {
		t.Fatal("exact original snapshot refused", e)
	}
	if e = validateColumnProducerRelation(parsed); e != nil {
		t.Fatal("exact snapshot projection changed", e)
	}
	f.snapshot += " "
	f.sign(t)
	if _, e = readColumnCarrier(f.carrier(), selectedContract()); e == nil {
		t.Fatal("generated cell amendment enlarged complete snapshot")
	} else if _, code := verificationOutcome(e); code != codeOverBound {
		t.Fatal("wrong snapshot bound refusal", code, e)
	}
}
