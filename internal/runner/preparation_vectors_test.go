package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// preparationVectors is where the preparation vectors are published
// (runner #75): normative data for a second reader of the mapping and its
// preparation, which the specification takes up as the dialect (spec #141).
const preparationVectors = "../../vectors/preparation/"

// preparationVectorsManifest is the SHA-256 of the vectors' manifest, which
// holds the SHA-256 of every file of every vector. The vectors were generated
// once and are not regenerated: changing one is a change of the contract a
// second reader is held to, and changes this digest with it.
const preparationVectorsManifest = "sha256:62dd8998b30ca89688e78fcc83ecde4a2535df1a84e30d7f9feb43f00babd179"

// preparationVector is a vector as the manifest describes it
// (vectors/preparation/README.md).
type preparationVector struct {
	Name        string            `json:"name"`
	Note        string            `json:"note"`
	From        string            `json:"from"`
	Altered     *string           `json:"altered"`
	VerifiedAt  string            `json:"verifiedAt"`
	Files       map[string]string `json:"files"`
	Preparation struct {
		SHA256   string `json:"sha256"`
		Binding  string `json:"binding"`
		Retained string `json:"retained"`
	} `json:"preparation"`
	// Import names, for a vector of a dialect bound at import, the
	// published dialect, the binding document and the bound dialect.
	Import *struct {
		Published string `json:"published"`
		Binding   string `json:"binding"`
		Bound     string `json:"bound"`
	} `json:"import"`
	RecordBinding *string `json:"recordBinding"`
	VerifyRun     struct {
		Arguments []string `json:"arguments"`
		Exit      int      `json:"exit"`
		Status    *string  `json:"status"`
		Report    *string  `json:"report"`
		Error     *string  `json:"error"`
	} `json:"verifyRun"`
}

// readPreparationVectors reads the manifest, held to its digest.
func readPreparationVectors(t *testing.T) []preparationVector {
	t.Helper()
	raw, err := os.ReadFile(preparationVectors + "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := digest(raw); got != preparationVectorsManifest {
		t.Fatalf("the vectors' manifest is %s, not %s: the vectors are not regenerated, and changing one is a contract change", got, preparationVectorsManifest)
	}
	var m struct {
		Family        string              `json:"family"`
		BindingKind   string              `json:"bindingKind"`
		CanonicalForm string              `json:"canonicalForm"`
		Generator     string              `json:"generator"`
		Vectors       []preparationVector `json:"vectors"`
	}
	if err = strictJSON(raw, &m); err != nil || m.Family != "preparation" || m.BindingKind != PreparationBindingKind || len(m.Vectors) == 0 {
		t.Fatal("the manifest is not the preparation vectors' manifest:", err)
	}
	return m.Vectors
}

// Every preparation vector is byte for byte what the Runner computes: its
// files are the ones the manifest names, with their digests; the preparation
// the Runner's own code computes from the vector's mapping, case, source
// records and profiles, at the vector's verifiedAt, is preparation.json,
// byte for byte, and its digest preparation.sha256; the binding Runner passes
// for that digest is the manifest's; the vector's mapping, case and sources
// are the export's own bytes, and its retained preparation is that one, or,
// where the manifest says so, another; the record's binding is the
// manifest's; and the verifier's findings on the export are the ones verify-
// run reports in the manifest. No Runtime is run: a preparation precedes
// evaluation, and no vector asks for a re-execution.
func TestPreparationVectorsAreByteExact(t *testing.T) {
	vectors := readPreparationVectors(t)
	want := []string{"README.md", "manifest.json"}
	for _, v := range vectors {
		want = append(want, v.Name)
	}
	if got := dirNames(t, preparationVectors); strings.Join(got, " ") != strings.Join(sortedStrings(want), " ") {
		t.Fatalf("the vectors' directory holds %v, not %v", got, sortedStrings(want))
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			dir := preparationVectors + v.Name + "/"
			names := []string{}
			for name := range v.Files {
				names = append(names, name)
			}
			if got := dirNames(t, dir); strings.Join(got, " ") != strings.Join(sortedStrings(names), " ") {
				t.Fatalf("the vector holds %v, not the files its manifest names", got)
			}
			file := map[string][]byte{}
			for name, sum := range v.Files {
				b, err := os.ReadFile(dir + name)
				if err != nil {
					t.Fatal(err)
				}
				if digest(b) != sum {
					t.Fatalf("%s is %s, not %s", name, digest(b), sum)
				}
				file[name] = b
			}
			if v.Note == "" || v.Altered != nil && *v.Altered == "" || !strings.HasPrefix(v.From, "internal/runner/testdata/") {
				t.Fatal("the vector does not say what it holds and where it is from")
			}
			if v.Altered != nil && v.VerifyRun.Exit != 1 {
				t.Fatal("an altered vector is not refused")
			}

			// The preparation the Runner computes, through its own code.
			profiles, err := ParseInputProfiles(file["profiles.json"])
			if err != nil {
				t.Fatal(err)
			}
			source := &SourceInput{Case: file["case.json"]}
			if err = strictJSON(file["mapping.json"], &source.Mapping); err != nil {
				t.Fatal(err)
			}
			// A dialect bound at import: the published dialect bound by the
			// binding document, under the vector's profiles, is the bound
			// dialect, byte for byte in the gateway's canonical form, and
			// the preparation is computed from the import as a run's is.
			if v.Import != nil {
				if v.Import.Published != "unbound.json" || v.Import.Binding != "binding.json" || v.Import.Bound != "mapping.json" {
					t.Fatalf("the manifest names the import's files otherwise: %+v", *v.Import)
				}
				source.Import = &DialectImport{Dialect: file["unbound.json"], Binding: file["binding.json"]}
				bound, err := bindDialect(*source.Import, profiles)
				if err != nil {
					t.Fatal("the vector's import does not bind:", err)
				}
				if !bytes.Equal(canonTest(t, encode(bound)), canonTest(t, file["mapping.json"])) {
					t.Fatalf("the bound dialect is not mapping.json:\n%s\n%s", encode(bound), file["mapping.json"])
				}
			} else if _, ok := file["unbound.json"]; ok {
				t.Fatal("the vector holds an import the manifest does not name")
			}
			if sources, ok := file["sources.json"]; ok {
				if err = strictJSON(sources, &source.Sources); err != nil {
					t.Fatal(err)
				}
			}
			at, err := time.Parse("2006-01-02T15:04:05Z", v.VerifiedAt)
			if err != nil {
				t.Fatal(err)
			}
			// A vector whose inputs were changed after the run, retained
			// "recorded", is one the Runner computes no preparation from: it
			// refuses the inputs as verify-run does, and preparation.json is
			// the preparation the run recorded, held fixed.
			prepared, err := prepareV2(Input{Source: source}, profiles, at, false, nil)
			if v.Preparation.Retained == "recorded" {
				if v.Altered == nil || err == nil || v.VerifyRun.Error == nil || err.Error() != *v.VerifyRun.Error {
					t.Fatalf("the Runner does not refuse the changed inputs as verify-run does: %v", err)
				}
				var recorded VerificationBundle
				if err = strictJSON(file["run.json"], &recorded); err != nil {
					t.Fatal(err)
				}
				prepared = Input{Preparation: recorded.Run.Input.Preparation}
			} else if err != nil {
				t.Fatal("the Runner does not prepare the vector's inputs:", err)
			}
			computed, err := canonical(encode(prepared.Preparation))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(computed, file["preparation.json"]) {
				t.Fatalf("preparation.json is not what the Runner computes:\n%s\n%s", file["preparation.json"], computed)
			}
			sum := preparationDigest(prepared.Preparation)
			if sum != string(file["preparation.sha256"]) || sum != digest(file["preparation.json"]) || sum != v.Preparation.SHA256 {
				t.Fatalf("the preparation's digest is %s, not preparation.sha256 %s", sum, file["preparation.sha256"])
			}
			if string(bindingDocument(sum)) != v.Preparation.Binding {
				t.Fatalf("the binding Runner passes is %s, not the manifest's", bindingDocument(sum))
			}

			// The export holds the vector's inputs, and retains its
			// preparation, or another where the manifest says so.
			export := json.RawMessage(file["run.json"])
			for name, path := range map[string][]string{"mapping.json": {"mapping"}, "case.json": {"case"}, "sources.json": {"sources"}, "unbound.json": {"import", "dialect"}, "binding.json": {"import", "binding"}} {
				held := member(export, append([]string{"run", "input", "source"}, path...)...)
				if b, ok := file[name]; ok != (len(held) > 0) || !bytes.Equal(b, held) {
					t.Fatalf("%s is not the export's own bytes", name)
				}
			}
			retained, err := canonical(member(export, "run", "input", "preparation"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(retained, computed) != (v.Preparation.Retained != "altered") || v.Preparation.Retained != "equal" && v.Preparation.Retained != "altered" && v.Preparation.Retained != "recorded" {
				t.Fatalf("the export's preparation is not %s to the one the Runner computes", v.Preparation.Retained)
			}
			var bundle VerificationBundle
			if err = strictJSON(file["run.json"], &bundle); err != nil {
				t.Fatal(err)
			}
			record := []byte(bundle.Run.Audit)
			if bundle.Version >= 3 {
				record = bundle.Run.AuditBytes
			}
			binding, err := recordBinding(record)
			if err != nil {
				t.Fatal(err)
			}
			if (binding == nil) != (v.RecordBinding == nil) || binding != nil && string(binding) != *v.RecordBinding {
				t.Fatalf("the record's binding is %s, not the manifest's", binding)
			}

			// The verifier's findings.
			args := v.VerifyRun.Arguments
			if len(args) < 6 || args[0] != "--file" || args[1] != "run.json" || args[2] != "--profiles" || args[3] != "profiles.json" || args[4] != "--release-digest" || args[5] != string(file["release-digest.txt"]) {
				t.Fatal("verify-run is not given the vector's export, profiles and release digest:", args)
			}
			verified, err := VerifyInputs(file["run.json"], profiles, args[5])
			if err != nil {
				if v.VerifyRun.Error == nil || err.Error() != *v.VerifyRun.Error {
					t.Fatalf("the verifier finds %q, not the manifest's %v", err, v.VerifyRun.Error)
				}
				return
			}
			if v.VerifyRun.Report == nil {
				t.Fatal("the verifier refuses nothing, and the manifest has no report")
			}
			// A binding matches exactly when the record carries the binding of
			// the preparation the Runner computes from the vector's inputs.
			if (verified.Binding != nil && verified.Binding.Status == BindingMatches) != bytes.Equal(binding, bindingDocument(sum)) {
				t.Fatalf("the record's binding %s is reported %+v, against the computed preparation's %s", binding, verified.Binding, bindingDocument(sum))
			}
			var report map[string]json.RawMessage
			if err = json.Unmarshal([]byte(*v.VerifyRun.Report), &report); err != nil {
				t.Fatal(err)
			}
			found := map[string]any{"binding": verified.Binding, "targetsByClass": verified.Classes, "unsignedFreshness": verified.Freshness, "unsignedParameters": verified.Unsigned}
			if verified.Binding == nil {
				found["binding"] = nil
			}
			if verified.Freshness == nil {
				found["unsignedFreshness"] = nil
			}
			if len(verified.Unsigned) == 0 {
				found["unsignedParameters"] = nil
			}
			for name, value := range found {
				if value == nil {
					if _, ok := report[name]; ok {
						t.Fatalf("the report has %s, which the verifier does not find", name)
					}
					continue
				}
				if !sameJSON(encode(value), report[name]) {
					t.Fatalf("the verifier finds %s %s, not the report's %s", name, encode(value), report[name])
				}
			}
		})
	}
}

// dirNames is the sorted names a directory holds.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, filepath.Base(e.Name()))
	}
	return sortedStrings(names)
}

func sortedStrings(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
