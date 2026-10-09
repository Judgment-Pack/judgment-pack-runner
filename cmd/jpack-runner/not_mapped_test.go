package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// notMappedVector is an export of a run without lineage that the runner's
// tests keep (internal/runner/testdata), and what verify-run is given with it.
type notMappedVector struct {
	dir, digest string
	line        []byte // the record's line the Runtime wrote
	chain       string // the installation's chain, as GET /v1/run-chain served it
	held        string // the run's entry's checkpoint, as a holder keeps it
	checkpoint  string // the same, as the report encodes it
	sequence    int64
	lines       int
}

func readNotMappedVector(t *testing.T, name string) notMappedVector {
	t.Helper()
	dir := "../../internal/runner/testdata/" + name + "/"
	read := func(file string) []byte {
		t.Helper()
		b, err := os.ReadFile(dir + file)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	held := read("checkpoints.jsonl")
	var checkpoint runner.Checkpoint
	if err := json.Unmarshal(held, &checkpoint); err != nil {
		t.Fatal(err)
	}
	cp, _ := json.Marshal(checkpoint)
	return notMappedVector{dir: dir, digest: strings.TrimSpace(string(read("release-digest.txt"))), line: bytes.TrimSuffix(read("audit-record.jsonl"), []byte("\n")),
		chain: dir + "chain.jsonl", held: dir + "checkpoints.jsonl", checkpoint: string(cp), sequence: checkpoint.Sequence, lines: bytes.Count(read("chain.jsonl"), []byte("\n"))}
}

// verifyVector runs the command on the vector's export and profiles.
func (v notMappedVector) verify(extra ...string) (string, string, error) {
	return verifyFiles(v.dir+"run.json", v.dir+"profiles.json", v.digest, extra...)
}

// notMappedReport is the report line of an export without lineage: no
// targetsByClass, which no lineage counts, and the inputs member, which sorts
// after exportVersion.
func notMappedReport(version int, recordDigest, disposition, runChain, scope, signature, status string) string {
	if signature != "" {
		signature = `"signature":` + signature + `,`
	}
	return fmt.Sprintf(`{"exactBytes":"matches-record","exportVersion":%d,"inputs":"not-mapped","recordDigest":%q,"retainedDisposition":%q,"runChain":%s,"scope":%q,%s"status":%q}`+"\n", version, recordDigest, disposition, runChain, scope, signature, status)
}

// notMappedSaid is what the line on standard error says of an export without
// lineage, after its first sentences.
const notMappedSaid = " The run's job has no mapping v2, so its export carries no lineage: its inputs are the operator's own, and nothing here derives them from a source or checks them against one."

const (
	notMappedInputs     = "audit binding of the retained inputs, which no lineage derives; not input derivation, sealed-session completeness or policy truth"
	notMappedReExecuted = "audit binding of the retained inputs, which no lineage derives, and the retained disposition against a re-execution by the release's Runtime; not input derivation, sealed-session completeness or policy truth"
)

// verify-run checks an export of a run whose job has no input mapping, or a
// v1 file mapping, as it checks any export: its record's bytes, its entry
// along the chain and against a held checkpoint, and its record's signature;
// and reports that its inputs are not mapped, counting no targets.
func TestVerifyRunChecksAnExportWithoutLineage(t *testing.T) {
	signature := func(status string, readable int) string {
		return fmt.Sprintf(`{"findings":[],"findingsTotal":0,"keyId":%q,"publicKey":%q,"readableLines":%d,"status":%q,"unreadableLines":0}`, keyID1, key1, readable, status)
	}
	for name, c := range map[string]struct {
		version   int
		signature string
		said      string
		extra     []string
	}{
		"no-mapping-run": {4, signature("unsigned", 0),
			" A version-4 export carries no record signatures, so as far as it shows no signature under key " + keyID1 + " covers the record: ask for version 5 to check one. An unsigned record is refused only with --require-signed.", nil},
		"v1-file-mapping-run": {5, signature("signed", 1),
			" The record's signature verifies under key " + keyID1 + ": whoever held that key signed these exact bytes as the attempt's record. That establishes nothing against the operator, who holds the key, and nothing once the key is copied.", []string{"--require-signed"}},
	} {
		t.Run(name, func(t *testing.T) {
			v := readNotMappedVector(t, name)
			record := sha(v.line)
			runChain := fmt.Sprintf(`{"chain":{"head":%s,"lines":%d},"checkpoint":%s,"findings":[],"findingsTotal":0,"held":{"matched":1,"supplied":1,"through":%d},"scope":"checkpoint","status":"valid","witnessed":true}`, v.checkpoint, v.lines, v.checkpoint, v.sequence)
			out, said, err := v.verify(append([]string{"--chain", v.chain, "--expect", v.held, "--require-witnessed", "--public-key", key1}, c.extra...)...)
			if err != nil || out != notMappedReport(c.version, record, "matches-audit", runChain, notMappedInputs, c.signature, "verified-inputs") {
				t.Fatal(err, out)
			}
			want := "verified-inputs: the run's inputs match its record. Its retained disposition matches its audit record; the evaluator was not replayed." + notMappedSaid +
				" The export's bytes of the audit record parse to the record. Their SHA-256 is " + record + ": a digest held independently, such as a gateway receipt's, shows whether they are the bytes the Runtime wrote for this run." +
				fmt.Sprintf(" The export's entry in the installation's chain of runs, at sequence %d, binds this run to those bytes, and the held checkpoints, through sequence %d, cover it: it is the entry that existed when they were made, if they were held independently of the operator. Nothing here shows when that was, or that the chain is complete after sequence %d.", v.sequence, v.sequence, v.sequence) +
				c.said + "\n"
			if said != want {
				t.Fatal("said:", said)
			}
		})
	}
}

// What verify-run refuses of an export without lineage, each with its report
// and before any re-execution: --require-sourced, since nothing derived its
// inputs from a source; a chain whose line at the run's sequence is not its
// entry, and a held checkpoint of another entry; a signature under another key;
// and an unsigned record, with --require-signed.
func TestVerifyRunRefusesWhatAnExportWithoutLineageCannotShow(t *testing.T) {
	noMapping, v1 := readNotMappedVector(t, "no-mapping-run"), readNotMappedVector(t, "v1-file-mapping-run")
	absent := "--runtime=" + filepath.Join(t.TempDir(), "absent")
	chain, err := os.ReadFile(noMapping.chain)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(chain, []byte("\n"))
	// The run's entry, at sequence 2, replaced by the entry at sequence 1.
	swapped := file(t, "chain.jsonl", append(append([]byte{}, lines[0]...), lines[0]...))
	// The run's checkpoint, naming other bytes.
	var changed runner.Checkpoint
	if err = json.Unmarshal([]byte(noMapping.checkpoint), &changed); err != nil {
		t.Fatal(err)
	}
	changed.RecordDigest = sha(nil)
	ch, _ := json.Marshal(changed)
	other := file(t, "held.jsonl", append(ch, '\n'))
	for name, c := range map[string]struct {
		v       notMappedVector
		extra   []string
		status  string
		refusal string
		is      error
	}{
		"not sourced": {noMapping, []string{"--require-sourced", absent}, "inputs-not-mapped",
			"inputs-not-mapped: the run's job has no mapping v2, so its export carries no lineage, and --require-sourced refuses it: its inputs are the operator's own, and nothing here checks them against a source", errInputsNotMapped},
		"not sourced, signed": {v1, []string{"--require-sourced", "--public-key", key1, absent}, "inputs-not-mapped",
			"inputs-not-mapped: the run's job has no mapping v2, so its export carries no lineage, and --require-sourced refuses it: its inputs are the operator's own, and nothing here checks them against a source", errInputsNotMapped},
		"another entry along the chain": {noMapping, []string{"--chain", swapped, absent}, "chain-invalid",
			"chain-invalid: sequence-mismatch at line 2: the entry's sequence is 1 (and 3 more)", errChainInvalid},
		"held to another entry": {noMapping, []string{"--expect", other, absent}, "chain-invalid",
			"chain-invalid: checkpoint-record-mismatch at line 2: the entry at the checkpoint's sequence is not the one the checkpoint names", errChainInvalid},
		"under another key": {v1, []string{"--public-key", key2, absent}, "signature-invalid",
			"signature-invalid: signature-invalid at line 1: sidecar line 1: the signature does not verify under the key in force", errSignatureInvalid},
		"unsigned, required": {noMapping, []string{"--public-key", key1, "--require-signed", absent}, "unsigned",
			"unsigned: no valid signature under key " + keyID1 + " covers the run's audit record, and --require-signed refuses it", errUnsigned},
	} {
		t.Run(name, func(t *testing.T) {
			out, said, err := c.v.verify(c.extra...)
			if !errors.Is(err, c.is) || err.Error() != c.refusal || said != "" {
				t.Fatal(err, said)
			}
			if !strings.HasPrefix(out, `{"exactBytes":"matches-record","exportVersion":`) || !strings.Contains(out, `,"inputs":"not-mapped","recordDigest":"`+sha(c.v.line)+`","retainedDisposition":"matches-audit",`) || !strings.Contains(out, `,"scope":"`+notMappedInputs+`",`) || !strings.HasSuffix(out, `,"status":"`+c.status+`"}`+"\n") || strings.Contains(out, "targetsByClass") {
				t.Fatal(out)
			}
		})
	}
}

// With the release's Runtime, a run without lineage is evaluated again on the
// inputs it retains, and a disposition changed after the run is a failure,
// with a report of its own: as for a run with lineage, with the scope of one
// without.
func TestVerifyRunReExecutesARunWithoutLineage(t *testing.T) {
	input := runner.Input{Facts: json.RawMessage(`{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}}`), Evidence: json.RawMessage(`{"intake-form":"present","sponsor-endorsement":"present"}`)}
	e := exportedRunFull(t, input, nil, "", "?version=4")
	if string(e.bundle["inputs"]) != `"not-mapped"` || string(e.bundle["version"]) != "4" {
		t.Fatal("not an export without lineage:", string(e.bundle["inputs"]), string(e.bundle["version"]))
	}
	t.Setenv("TMPDIR", t.TempDir())
	runtime := "--runtime=" + os.Getenv("JPACK_TEST_BIN")
	out, said, err := verify(t, e.bundle, e.digest, runtime)
	if err != nil || !strings.HasPrefix(out, `{"exactBytes":"matches-record","exportVersion":4,"inputs":"not-mapped",`) || !strings.Contains(out, `"retainedDisposition":"matches-re-execution",`) || !strings.HasSuffix(out, `"scope":"`+notMappedReExecuted+`","status":"verified-disposition"}`+"\n") {
		t.Fatal(err, out)
	}
	if !strings.HasPrefix(said, "verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true."+notMappedSaid+" The export's bytes") {
		t.Fatal("said:", said)
	}
	var run map[string]json.RawMessage
	if err = json.Unmarshal(e.bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	run["result"] = bytes.Replace(run["result"], []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`), 1)
	e.bundle["run"], _ = json.Marshal(run)
	out, said, err = verify(t, e.bundle, e.digest, runtime)
	if !errors.Is(err, runner.ErrAuditDispositionMismatch) || out != auditDispositionMismatchReport || said != "" {
		t.Fatal(err, out, said)
	}
}
