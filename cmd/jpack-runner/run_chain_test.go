package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// chainedRun completes one run, exports it as version 4, and returns the
// export, its trusted release digest, the record's line, the installation's
// chain, and the entry's checkpoint as the export carries it.
func chainedRun(t *testing.T) (exported, runner.Checkpoint) {
	t.Helper()
	e := exportedRunFull(t, caseInput(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`), nil, ampersand, "?version=4")
	var chain struct {
		Entry      []byte            `json:"entry"`
		Checkpoint runner.Checkpoint `json:"checkpoint"`
	}
	if err := json.Unmarshal(e.bundle["chain"], &chain); err != nil || string(e.bundle["version"]) != "4" {
		t.Fatal("the export is not version 4 with an entry:", err)
	}
	if !bytes.Equal(e.chain, append(bytes.Clone(chain.Entry), '\n')) || chain.Checkpoint.Sequence != 1 {
		t.Fatalf("the chain is not the run's one entry: %s", e.chain)
	}
	return e, chain.Checkpoint
}

// sha is the SHA-256 of b, as the report writes a digest.
func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// reportV4 is the report line verify-run writes of a version-4 export of the
// chained run, all of whose inputs are asserted, with the runChain member.
func reportV4(recordDigest, status, disposition, scope, runChain string) string {
	return fmt.Sprintf(`{"exactBytes":"matches-record","exportVersion":4,"recordDigest":%q,"retainedDisposition":%q,"runChain":%s,"scope":%q,"status":%q,"targetsByClass":{"asserted":3,"record":0,"generated":0}}`+"\n", recordDigest, disposition, runChain, scope, status)
}

// file writes b to a new file and returns its path.
func file(t *testing.T, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// verify-run checks a version-4 export's entry as far as it is given: alone,
// along the chain, and against held checkpoints, and says what each
// establishes. Without a held checkpoint it says it checked one supplied entry
// or chain only, which establishes nothing against the operator.
func TestVerifyRunChecksTheRunsEntry(t *testing.T) {
	e, checkpoint := chainedRun(t)
	record := sha(e.line)
	cp, _ := json.Marshal(checkpoint)
	bytesSaid := " The export's bytes of the audit record parse to the record. Their SHA-256 is " + record + ": a digest held independently, such as a gateway receipt's, shows whether they are the bytes the Runtime wrote for this run."
	entrySaid := " The export's entry in the installation's chain of runs, at sequence 1, binds this run to those bytes"
	inputs := "verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says. The run's inputs are the operator's own: nothing here checks them against a source." + bytesSaid + entrySaid
	held := file(t, "held.jsonl", append(cp, '\n'))
	chain := file(t, "chain.jsonl", e.chain)
	head := `{"head":` + string(cp) + `,"lines":1}`
	for name, c := range map[string]struct {
		extra    []string
		runChain string
		human    string
	}{
		"the entry alone": {nil, `{"checkpoint":` + string(cp) + `,"findings":[],"findingsTotal":0,"scope":"one-supplied-entry","status":"valid","witnessed":false}`,
			inputs + ". Only that one supplied entry was checked, which establishes nothing against the operator, who keeps the chain and could have written the entry with the export: a checkpoint held independently, supplied with --expect, would. The report gives the entry's checkpoint, for a holder to keep from now on.\n"},
		"along the chain": {[]string{"--chain", chain}, `{"chain":` + head + `,"checkpoint":` + string(cp) + `,"findings":[],"findingsTotal":0,"scope":"one-supplied-chain","status":"valid","witnessed":false}`,
			inputs + ", and the supplied chain of 1 line is consistent with it. That is the integrity of one supplied chain, which establishes nothing against the operator, who keeps the chain and could have rewritten it with its links recomputed: a checkpoint held independently, supplied with --expect, would.\n"},
		"held": {[]string{"--expect", held}, `{"checkpoint":` + string(cp) + `,"findings":[],"findingsTotal":0,"held":{"matched":1,"supplied":1,"through":1},"scope":"checkpoint","status":"valid","witnessed":true}`,
			inputs + ", and the held checkpoints, through sequence 1, cover it: it is the entry that existed when they were made, if they were held independently of the operator. Nothing here shows when that was, or that the chain is complete after sequence 1.\n"},
		"held, along the chain, required": {[]string{"--chain", chain, "--expect", held, "--require-witnessed"}, `{"chain":` + head + `,"checkpoint":` + string(cp) + `,"findings":[],"findingsTotal":0,"held":{"matched":1,"supplied":1,"through":1},"scope":"checkpoint","status":"valid","witnessed":true}`,
			inputs + ", and the held checkpoints, through sequence 1, cover it: it is the entry that existed when they were made, if they were held independently of the operator. Nothing here shows when that was, or that the chain is complete after sequence 1.\n"},
	} {
		t.Run(name, func(t *testing.T) {
			out, human, err := verify(t, e.bundle, e.digest, c.extra...)
			if err != nil || out != reportV4(record, "verified-inputs", "not-checked", inputsOnly, c.runChain) {
				t.Fatal(err, out)
			}
			if human != c.human {
				t.Fatal("human output:", human)
			}
		})
	}
	// With the release's Runtime, the entry is reported the same way.
	t.Setenv("TMPDIR", t.TempDir())
	out, human, err := verify(t, e.bundle, e.digest, "--expect", held, "--runtime="+os.Getenv("JPACK_TEST_BIN"))
	if err != nil || !strings.HasPrefix(out, `{"exactBytes":"matches-record","exportVersion":4,`) || !strings.Contains(out, `"retainedDisposition":"matches-re-execution","runChain":{"checkpoint":`+string(cp)) || !strings.Contains(out, `"witnessed":true}`) || !strings.HasSuffix(human, "or that the chain is complete after sequence 1.\n") {
		t.Fatal(err, out, human)
	}
}

// --require-witnessed refuses an entry no held checkpoint covers, and a check
// that fails refuses the run: each with a report and an error, before any
// re-execution. A held checkpoint at another sequence is checked only along
// the chain.
func TestVerifyRunRefusesAnEntryItCannotHold(t *testing.T) {
	e, checkpoint := chainedRun(t)
	record := sha(e.line)
	cp, _ := json.Marshal(checkpoint)
	absent := "--runtime=" + filepath.Join(t.TempDir(), "absent")
	out, human, err := verify(t, e.bundle, e.digest, "--require-witnessed", absent)
	if !errors.Is(err, errUnwitnessed) || err.Error() != "unwitnessed: no held checkpoint covers the run's entry, at sequence 1, and --require-witnessed refuses it: nothing here establishes the entry against the operator, who keeps the chain" || human != "" ||
		out != reportV4(record, "unwitnessed", "not-checked", inputsOnly, `{"checkpoint":`+string(cp)+`,"findings":[],"findingsTotal":0,"scope":"one-supplied-entry","status":"valid","witnessed":false}`) {
		t.Fatal(err, out, human)
	}
	// A chain whose one line is another entry, and a checkpoint of another one.
	other := bytes.Replace(e.chain, []byte(`"sequence":1`), []byte(`"sequence":2`), 1)
	changed := checkpoint
	changed.RecordDigest = sha(nil)
	ch, _ := json.Marshal(changed)
	for name, c := range map[string]struct {
		extra    []string
		refusal  string
		runChain string
	}{
		"along another chain": {[]string{"--chain", file(t, "chain.jsonl", other), absent},
			"chain-invalid: sequence-mismatch at line 1: the entry's sequence is 2 (and 1 more)",
			`{"chain":{"head":{"checkpointVersion":"1","recordDigest":"` + sha(bytes.TrimSuffix(other, []byte("\n"))) + `","sequence":1,"trail":"` + checkpoint.Trail + `"},"lines":1},"checkpoint":` + string(cp) + `,"findings":[{"name":"sequence-mismatch","line":1,"detail":"the entry's sequence is 2"},{"name":"entry-mismatch","line":1,"detail":"the line at the run's sequence is not the export's entry"}],"findingsTotal":2,"scope":"one-supplied-chain","status":"invalid","witnessed":false}`},
		"held to another entry": {[]string{"--expect", file(t, "held.jsonl", ch), absent},
			"chain-invalid: checkpoint-record-mismatch at line 1: the entry at the checkpoint's sequence is not the one the checkpoint names",
			`{"checkpoint":` + string(cp) + `,"findings":[{"name":"checkpoint-record-mismatch","line":1,"detail":"the entry at the checkpoint's sequence is not the one the checkpoint names"}],"findingsTotal":1,"held":{"matched":0,"supplied":1},"scope":"checkpoint","status":"invalid","witnessed":false}`},
	} {
		t.Run(name, func(t *testing.T) {
			out, human, err := verify(t, e.bundle, e.digest, c.extra...)
			if !errors.Is(err, errChainInvalid) || err.Error() != c.refusal || human != "" || out != reportV4(record, "chain-invalid", "not-checked", inputsOnly, c.runChain) {
				t.Fatal(err, "\n", out, human)
			}
		})
	}
	later := checkpoint
	later.Sequence = 2
	lt, _ := json.Marshal(later)
	out, human, err = verify(t, e.bundle, e.digest, "--expect", file(t, "held.jsonl", lt))
	if err == nil || err.Error() != "a held checkpoint at sequence 2 does not name this run's entry, at sequence 1: it is checked along the chain, supplied with --chain" || out != "" || human != "" {
		t.Fatal(err, out, human)
	}
	// A held document that is not one of checkpoints is refused.
	if _, _, err = verify(t, e.bundle, e.digest, "--expect", file(t, "held.jsonl", []byte("{}\n"))); err == nil || !strings.Contains(err.Error(), "held.jsonl: line 1:") {
		t.Fatal(err)
	}
}

// The export Runner made before the chain existed is version 3 and carries no
// entry: verify-run reports it as it did, and refuses to check an entry it does
// not have, saying why.
func TestVerifyRunSaysAnUnchainedRunHasNoEntry(t *testing.T) {
	dir := "../../internal/runner/testdata/mapping-v2-before-run-chain/"
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	trail, err := os.ReadFile(dir + "audit-record.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	record := sha(bytes.TrimSuffix(trail, []byte("\n")))
	out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", strings.TrimSpace(string(trusted)))
	if err != nil || out != reportV3(record, "verified-inputs", "not-checked", inputsOnly, 3, 0, 0) || !strings.HasSuffix(human, " shows whether they are the bytes the Runtime wrote for this run.\n") {
		t.Fatal(err, out, human)
	}
	held := file(t, "held.jsonl", []byte(`{"checkpointVersion":"1","recordDigest":"`+record+`","sequence":1,"trail":"`+strings.Repeat("a", 32)+`"}`+"\n"))
	for _, extra := range [][]string{{"--chain", held}, {"--expect", held}, {"--require-witnessed"}} {
		out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", strings.TrimSpace(string(trusted)), extra...)
		if !errors.Is(err, runner.ErrNoChainEntry) || !strings.Contains(err.Error(), "it is version 3. A run recorded before Runner chained its runs has no entry and is unchained") || out != "" || human != "" {
			t.Fatal(extra, err, out, human)
		}
	}
}

// What the line on standard error says of the entry follows from what was
// checked: one supplied entry, one supplied chain, held checkpoints that do not
// reach the entry, and held checkpoints that cover it.
func TestTheChainSentenceSaysWhatEachCheckEstablishes(t *testing.T) {
	cp := runner.Checkpoint{CheckpointVersion: "1", RecordDigest: sha(nil), Sequence: 3, Trail: strings.Repeat("a", 32)}
	at := " The export's entry in the installation's chain of runs, at sequence 3, binds this run to those bytes"
	for _, c := range []struct {
		report *runner.RunChainReport
		want   string
	}{
		{nil, ""},
		{&runner.RunChainReport{Checkpoint: cp, Scope: runner.ScopeOneSuppliedEntry, Status: "valid"},
			at + ". Only that one supplied entry was checked, which establishes nothing against the operator, who keeps the chain and could have written the entry with the export: a checkpoint held independently, supplied with --expect, would. The report gives the entry's checkpoint, for a holder to keep from now on."},
		{&runner.RunChainReport{Checkpoint: cp, Scope: runner.ScopeOneSuppliedChain, Status: "valid", Chain: &runner.ChainRead{Lines: 5}},
			at + ", and the supplied chain of 5 lines is consistent with it. That is the integrity of one supplied chain, which establishes nothing against the operator, who keeps the chain and could have rewritten it with its links recomputed: a checkpoint held independently, supplied with --expect, would."},
		{&runner.RunChainReport{Checkpoint: cp, Scope: runner.ScopeCheckpoint, Status: "valid", Chain: &runner.ChainRead{Lines: 5}, Held: &runner.ChainHeld{Supplied: 1, Matched: 1, Through: 2}},
			at + ", and the supplied chain agrees with the held checkpoints, through sequence 2. None of them covers this run's entry: it is unwitnessed, and nothing here establishes it against the operator, who keeps the chain."},
		{&runner.RunChainReport{Checkpoint: cp, Scope: runner.ScopeCheckpoint, Status: "valid", Chain: &runner.ChainRead{Lines: 5}, Held: &runner.ChainHeld{Supplied: 2, Matched: 2, Through: 5}, Witnessed: true},
			at + ", and the held checkpoints, through sequence 5, cover it: it is the entry that existed when they were made, if they were held independently of the operator. Nothing here shows when that was, or that the chain is complete after sequence 5."},
	} {
		if got := chainSentence(c.report); got != c.want {
			t.Errorf("%+v:\n%s", c.report, got)
		}
	}
}
