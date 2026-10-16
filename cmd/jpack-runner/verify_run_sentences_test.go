package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// verify-run refuses a receipt that is no object, or none, in the words it
// always did (runner #110's review): the code verify-preparation names is
// attached to the refusal and replaces none of its sentence. The export is
// the vector valid's, its one source's receipt replaced or removed.
func TestVerifyRunRefusesAReceiptThatIsNoObjectInItsOwnWords(t *testing.T) {
	dir := "../../vectors/preparation/valid/"
	raw, err := os.ReadFile(dir + "run.json")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	for receipt, want := range map[string]string{
		`[]`:   "Source fx: receipt is not an object. No decision was requested.",
		`null`: "Source fx: receipt is not an object. No decision was requested.",
		`true`: "Source fx: receipt is not an object. No decision was requested.",
		``:     "Source fx: malformed receipt. No decision was requested.",
	} {
		var export, run, input, source, sources, fx map[string]json.RawMessage
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &export) != nil || json.Unmarshal(export["run"], &run) != nil || json.Unmarshal(run["input"], &input) != nil || json.Unmarshal(input["source"], &source) != nil || json.Unmarshal(source["sources"], &sources) != nil || json.Unmarshal(sources["fx"], &fx) != nil || json.Unmarshal(fx["response"], &response) != nil {
			t.Fatal("the export holds no response for fx")
		}
		if receipt == "" {
			delete(response, "receipt")
		} else {
			response["receipt"] = json.RawMessage(receipt)
		}
		fx["response"], _ = json.Marshal(response)
		sources["fx"], _ = json.Marshal(fx)
		source["sources"], _ = json.Marshal(sources)
		input["source"], _ = json.Marshal(source)
		run["input"], _ = json.Marshal(input)
		export["run"], _ = json.Marshal(run)
		changed, _ := json.Marshal(export)
		file := filepath.Join(t.TempDir(), "run.json")
		if err := os.WriteFile(file, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		err := verifyRun([]string{"--file", file, "--profiles", dir + "profiles.json", "--release-digest", string(digest)}, &stdout, &stderr)
		if err == nil || err.Error() != want || stdout.Len() != 0 {
			t.Fatalf("a receipt %q: verify-run says %v and writes %q, not %q", receipt, err, stdout.String(), want)
		}
	}
}
