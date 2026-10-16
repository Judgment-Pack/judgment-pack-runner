package runner

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The preparer refuses as it always did (runner #110's review). The codes
// verify-preparation names are attached to its refusals and replace none of
// their sentences: a receipt that is missing or no object, and a value a read
// unwraps that is over a bound or no JSON, are refused in the words they were
// refused in before the codes. These tests use nothing the codes added, so
// they read the same against the Runner before them.

// validWithReceipt is the vector valid's inputs, its one source's receipt
// replaced by receipt, or removed when receipt is empty, and its profiles and
// instant.
func validWithReceipt(t *testing.T, receipt string) (Input, []InputProfile, time.Time) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("../../vectors/preparation/valid/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var sources map[string]map[string]map[string]json.RawMessage
	if err := json.Unmarshal(read("sources.json"), &sources); err != nil {
		t.Fatal(err)
	}
	if receipt == "" {
		delete(sources["fx"]["response"], "receipt")
	} else {
		sources["fx"]["response"]["receipt"] = json.RawMessage(receipt)
	}
	source := &SourceInput{Case: read("case.json")}
	if err := strictJSON(read("mapping.json"), &source.Mapping); err != nil {
		t.Fatal(err)
	}
	if err := strictJSON(encode(sources), &source.Sources); err != nil {
		t.Fatal(err)
	}
	profiles, err := ParseInputProfiles(read("profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Input{Source: source}, profiles, time.Date(2026, 10, 14, 14, 58, 26, 0, time.UTC)
}

// receiptSentences are the sentences a receipt that is no object, or none,
// is refused with.
var receiptSentences = map[string]string{
	`[]`:   "Source fx: receipt is not an object. No decision was requested.",
	`null`: "Source fx: receipt is not an object. No decision was requested.",
	`true`: "Source fx: receipt is not an object. No decision was requested.",
	``:     "Source fx: malformed receipt. No decision was requested.",
}

func TestAReceiptThatIsNoObjectIsRefusedInItsOwnWords(t *testing.T) {
	for receipt, want := range receiptSentences {
		input, profiles, at := validWithReceipt(t, receipt)
		if _, err := prepareV2(input, profiles, at, false, nil); err == nil || err.Error() != want {
			t.Fatalf("a receipt %q is refused with %v, not %q", receipt, err, want)
		}
	}
}

// unwrapped is a run of one operation whose read unwraps the string its
// record holds at /body, held: a record signed now for a profile made for
// the run.
func unwrapped(t *testing.T, held string) (Input, []InputProfile, time.Time) {
	t.Helper()
	p, key := testProfile(t)
	at := time.Now().UTC().Truncate(time.Second)
	args := `{"tool":"lookup","arguments":{"id":"7"}}`
	m := InputMapping{Version: 2, Case: &CaseMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{{
		Name: "vendor", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(args),
		Read: SourceRead{Unwrap: []string{"/body"}, Copy: &CopyMapping{Facts: []FactMapping{{Target: "/vendor", Source: "/vendor"}}, Evidence: []EvidenceMapping{}}},
	}}}
	result := encode(map[string]string{"body": held})
	return Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{}`), Sources: map[string]SourceValue{
		"vendor": {Response: signResponse(t, p, key, []byte(args), result, at, 0)},
	}}}, []InputProfile{p}, at
}

// unwrapSentences are the sentences a value a read unwraps is refused with,
// by what the string holds; one that is read has none.
var unwrapSentences = map[string]string{
	`{"vendor":1}`:     "",
	`{"vendor":1e309}`: "Source vendor: numeric exponent exceeds mapping profile limits. No decision was requested.",
	`{"vendor":` + strings.Repeat("1", 129) + `}`:           "Source vendor: numeric token exceeds 128 characters. No decision was requested.",
	strings.Repeat("[", 40) + "1" + strings.Repeat("]", 40): "Source vendor: unwrap contains invalid JSON. No decision was requested.",
	`{"vendor":`: "Source vendor: unwrap contains invalid JSON. No decision was requested.",
}

func TestAnUnwrappedValueIsRefusedInItsOwnWords(t *testing.T) {
	for held, want := range unwrapSentences {
		input, profiles, at := unwrapped(t, held)
		_, err := prepareV2(input, profiles, at, false, nil)
		if want == "" && err != nil || want != "" && (err == nil || err.Error() != want) {
			t.Fatalf("an unwrapped %q is refused with %v, not %q", held, err, want)
		}
	}
}
