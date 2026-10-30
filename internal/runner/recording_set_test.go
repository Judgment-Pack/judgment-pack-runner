package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// recordingCore names the Core the recorded runs' pack declares: the runs
// whose exports the preparation vectors are made from, which write them when
// their variables name a directory (tools/preparation-vectors, recorders).
// Unset, they declare the triage pack's own, 0.2.0-draft, which
// vectors/preparation holds; scripts/recording-set.sh sets it to 0.3.0 to
// record the Core 0.3.0 recording set (runner #174).
const recordingCore = "JPACK_RECORDING_CORE"

// recordingPack is the pack the recorded runs are previewed with: the
// handwritten triage pack as written, or, when JPACK_RECORDING_CORE names
// 0.3.0, the same pack declaring Core 0.3.0 as its own artifact version,
// 0.1.1, as TestOrdinaryCoreVersionsRealRuntimeSideBySide makes it. Any other
// value fails the test: no run is recorded under a Core no one asked for.
func recordingPack(t *testing.T) string {
	t.Helper()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	switch core := os.Getenv(recordingCore); core {
	case "", "0.2.0-draft":
		return string(pack)
	case "0.3.0":
		declares, version := []byte(`"specVersion": "0.2.0-draft"`), []byte(`"version": "0.1.0"`)
		if bytes.Count(pack, declares) != 1 || bytes.Count(pack, version) != 1 {
			t.Fatal("the handwritten pack no longer declares 0.2.0-draft and its version once")
		}
		pack = bytes.Replace(pack, declares, []byte(`"specVersion": "0.3.0"`), 1)
		return string(bytes.Replace(pack, version, []byte(`"version": "0.1.1"`), 1))
	default:
		t.Fatalf("%s=%q: the recorded runs declare Core 0.2.0-draft or 0.3.0", recordingCore, core)
		return ""
	}
}

// recordingSample are the vectors of the Core 0.3.0 recording set that
// TestCore030RecordingSetSampleRegenerates makes again: the valid case and
// the four made of its run by an alteration (recomputed, rewritten and
// signed again, a changed retained lineage, a changed retained result), and
// a research run bound at import with the vector made of it by a changed
// retained result, which keeps the preparation the run recorded.
var recordingSample = []string{"valid", "recomputed-one-second-later", "foreign-binding-kind", "changed-table-lineage", "disposition-differs-from-audit", "gtm-company-person-valid-types-all-six", "gtm-company-person-changed-response-retained-result"}

// A sample of the Core 0.3.0 recording set is made again as
// scripts/recording-set.sh makes the whole: its runs recorded through the
// Runner's own tests, with the triage pack declaring Core 0.3.0 and the
// pinned Runtime that evaluates it, and the vectors made of them by
// tools/preparation-vectors -core 0.3.0, which runs verify-run, built from
// this commit, on each and requires the manifest's finding. Each vector then
// verifies through the Runner's own preparation verifier as
// vectors/preparation's do (checkPreparationVector), and every recording
// names Core 0.3.0: the release's pack, the run's result, the record as the
// export parses it and the record's own bytes, and the record's pack. The
// manifest names the Runtime that recorded the runs and the signing
// identity. The test's commits are placeholders; the published set's are
// the script's.
func TestCore030RecordingSetSampleRegenerates(t *testing.T) {
	bin := os.Getenv("JPACK_RETAINED_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_RETAINED_TEST_BIN to the pinned Runtime that evaluates Core 0.3.0 (scripts/checks/build-pinned.sh)")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("JPACK_TEST_BIN", bin)
	t.Setenv(recordingCore, "0.3.0")
	fixtures := t.TempDir()
	valid := filepath.Join(fixtures, "mapping-v2-calculated-bound")
	if err = os.MkdirAll(valid, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, profile := calculatedSignedExport(t, "computed")
	writeBoundFixture(t, valid, bundle, profile)
	for _, r := range researchRecordings() {
		if r.name == "gtm-company-person-valid-types-all-six" {
			recordResearchRun(t, newResearchStandIn(t), r, filepath.Join(fixtures, "research"))
		}
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(t.TempDir(), "jpack-runner")
	out := filepath.Join(t.TempDir(), "set")
	zero := strings.Repeat("0", 40)
	for _, args := range [][]string{
		{"build", "-o", runner, "./cmd/jpack-runner"},
		{"run", "./tools/preparation-vectors", "-core", "0.3.0", "-fixtures", fixtures, "-only", strings.Join(recordingSample, ","), "-runner", runner,
			"-runner-commit", zero, "-runtime-commit", zero, "-spec-version", "v0.3.0", "-spec-commit", zero, "-out", out},
	} {
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", args[0], err, b)
		}
	}

	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Family        string `json:"family"`
		BindingKind   string `json:"bindingKind"`
		CanonicalForm string `json:"canonicalForm"`
		Generator     string `json:"generator"`
		Core          string `json:"core"`
		Producer      struct {
			RunnerCommit   string `json:"runnerCommit"`
			RuntimeVersion string `json:"runtimeVersion"`
			RuntimeCommit  string `json:"runtimeCommit"`
			SpecVersion    string `json:"specVersion"`
			SpecCommit     string `json:"specCommit"`
		} `json:"producer"`
		Signing struct {
			Identity  string `json:"identity"`
			Seed      string `json:"seed"`
			PublicKey string `json:"publicKey"`
			KeyID     string `json:"keyId"`
			Note      string `json:"note"`
		} `json:"signing"`
		NotFixed   []string          `json:"notFixed"`
		Recordings []json.RawMessage `json:"recordings"`
		Vectors    []struct {
			preparationVector
			Recorded string `json:"recorded"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	version, err := runtimeVersionOf(bin)
	if err != nil {
		t.Fatal(err)
	}
	if m.Family != "preparation" || m.BindingKind != PreparationBindingKind || m.Core != "0.3.0" || m.Producer.RuntimeVersion != version || m.Producer.SpecVersion != "v0.3.0" || len(m.NotFixed) == 0 || len(m.Recordings) == 0 {
		t.Fatalf("the set's manifest does not say what recorded it: %s", raw)
	}
	if m.Signing.Seed != vectorSeed1 || m.Signing.PublicKey != vectorKey1 || m.Signing.KeyID != vectorKeyID1 {
		t.Fatalf("the set is not signed under the published test identity: %+v", m.Signing)
	}
	names := []string{"manifest.json"}
	for _, v := range m.Vectors {
		names = append(names, v.Name)
	}
	if strings.Join(sortedStrings(names), " ") != strings.Join(sortedStrings(append([]string{"manifest.json"}, recordingSample...)), " ") || strings.Join(dirNames(t, out), " ") != strings.Join(sortedStrings(names), " ") {
		t.Fatalf("the set holds %v, not the sample", dirNames(t, out))
	}
	for _, v := range m.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			dir := filepath.Join(out, v.Name) + "/"
			checkPreparationVector(t, dir, v.preparationVector)
			run, err := os.ReadFile(dir + "run.json")
			if err != nil {
				t.Fatal(err)
			}
			if v.Recorded == "" || (v.Altered == nil) != (v.Recorded == digest(run)) {
				t.Fatalf("the manifest's recorded digest %s is not the export's as the Runner wrote it", v.Recorded)
			}
			var b VerificationBundle
			if err = strictJSON(run, &b); err != nil {
				t.Fatal(err)
			}
			named := `"0.3.0"`
			for what, got := range map[string][]byte{
				"the release's pack declares":       member(json.RawMessage(b.Release.Pack), "specVersion"),
				"the run's result names":            member(b.Run.Result, "evaluatorSpecVersion"),
				"the record names":                  member(b.Run.Audit, "evaluatorSpecVersion"),
				"the record's pack declares":        member(b.Run.Audit, "pack", "specVersion"),
				"the record's bytes name":           member(json.RawMessage(b.Run.AuditBytes), "evaluatorSpecVersion"),
				"the record's bytes' pack declares": member(json.RawMessage(b.Run.AuditBytes), "pack", "specVersion"),
			} {
				if string(got) != named {
					t.Fatalf("%s Core %s, not 0.3.0", what, got)
				}
			}
			if string(member(json.RawMessage(b.Run.AuditBytes), "tool", "version")) != `"`+version+`"` {
				t.Fatalf("the record was not made by the Runtime %s", version)
			}
		})
	}
}

// runtimeVersionOf is the version a Runtime names itself by.
func runtimeVersionOf(bin string) (string, error) {
	out, err := exec.Command(bin, "version", "--format", "json").Output()
	if err != nil {
		return "", err
	}
	var answer struct {
		Tool struct {
			Version string `json:"version"`
		} `json:"tool"`
	}
	if err = json.Unmarshal(out, &answer); err != nil || answer.Tool.Version == "" {
		return "", fmt.Errorf("the Runtime names no version: %v", err)
	}
	return answer.Tool.Version, nil
}
