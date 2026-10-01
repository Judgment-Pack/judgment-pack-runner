package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reportV3 is the report line verify-run writes of a version-3 export, whose
// audit record's original bytes have the digest given.
func reportV3(recordDigest, status, disposition, scope string, asserted, record, generated int) string {
	return fmt.Sprintf(`{"exactBytes":"matches-record","exportVersion":3,"recordDigest":%q,"retainedDisposition":%q,"scope":%q,"status":%q,"targetsByClass":{"asserted":%d,"record":%d,"generated":%d}}`+"\n", recordDigest, disposition, scope, status, asserted, record, generated)
}

// ampersand is the triage pack's identifier with an &, which the Runtime writes
// in its audit record as it is, and Runner's encoding of the record escapes.
const ampersand = "https://example.invalid/judgment-packs/data-request-intake-triage?edition=A&B"

// A version-3 export's report says that the audit record's original bytes
// were checked against the record, and gives their SHA-256: the digest of the
// line the Runtime wrote, which a gateway receipt names the record by, and not
// of Runner's encoding of the record. Both lines say so, with or without the
// release's Runtime.
func TestVerifyRunReportsTheRecordsDigest(t *testing.T) {
	bundle, digest, line := exportedRunAs(t, `{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`, ampersand, "?version=3")
	var run map[string]json.RawMessage
	if err := json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte("?edition=A&B")) || bytes.Equal(run["audit"], line) || string(bundle["version"]) != "3" {
		t.Fatal("the fixture's record is not one whose re-encoding has other bytes")
	}
	sum := sha256.Sum256(line)
	want := "sha256:" + hex.EncodeToString(sum[:])
	said := " The audit record's original bytes parse to the record, and their SHA-256, the digest a gateway receipt names it by, is " + want + ".\n"
	out, human, err := verify(t, bundle, digest)
	if err != nil || out != reportV3(want, "verified-inputs", "not-checked", inputsOnly, 3, 0, 0) {
		t.Fatal(err, out)
	}
	if human != "verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says. The run's inputs are the operator's own: nothing here checks them against a source."+said {
		t.Fatal("human output:", human)
	}
	t.Setenv("TMPDIR", t.TempDir())
	out, human, err = verify(t, bundle, digest, "--runtime="+os.Getenv("JPACK_TEST_BIN"))
	if err != nil || out != reportV3(want, "verified-disposition", "matches-re-execution", reExecuted, 3, 0, 0) || !strings.HasSuffix(human, " nothing here checks them against a source."+said) {
		t.Fatal(err, out, human)
	}
	// Bytes that parse to another record are refused, before any report.
	run["auditBytes"], _ = json.Marshal(base64.StdEncoding.EncodeToString(bytes.Replace(line, []byte(`"reviewed":true`), []byte(`"reviewed":false`), 1)))
	bundle["run"], _ = json.Marshal(run)
	out, human, err = verify(t, bundle, digest)
	if err == nil || err.Error() != "the audit record differs from its original bytes" || out != "" || human != "" {
		t.Fatal(err, out, human)
	}
}

// The export that Runner made at the commit before version 3 verifies as
// version 2 does: the report says that exact-byte checks were not possible.
func TestVerifyRunSaysVersion2CarriesNoBytes(t *testing.T) {
	dir := "../../internal/runner/testdata/mapping-v2-before-exact-bytes/"
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", strings.TrimSpace(string(trusted)))
	if err != nil || out != report("verified-inputs", "not-checked", inputsOnly, 3, 0, 0) || !strings.HasSuffix(human, notInExport+"\n") {
		t.Fatal(err, out, human)
	}
	// The same export claiming version 3 carries no bytes, and is refused.
	raw, err := os.ReadFile(dir + "run.json")
	if err != nil {
		t.Fatal(err)
	}
	claimed := bytes.Replace(raw, []byte(`{"version":2,`), []byte(`{"version":3,`), 1)
	if bytes.Equal(claimed, raw) {
		t.Fatal("the fixture's version moved")
	}
	file := filepath.Join(t.TempDir(), "run.json")
	if err = os.WriteFile(file, claimed, 0600); err != nil {
		t.Fatal(err)
	}
	out, human, err = verifyFiles(file, dir+"profiles.json", strings.TrimSpace(string(trusted)))
	if err == nil || !strings.Contains(err.Error(), "a version-3 export carries the original bytes") || out != "" || human != "" {
		t.Fatal(err, out, human)
	}
}
