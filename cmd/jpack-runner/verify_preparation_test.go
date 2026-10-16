package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/buildinfo"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// runVerifyPreparation runs the command in this process, as main does, and
// returns its exit status and what it wrote.
func runVerifyPreparation(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	status := verifyPreparation(args, &stdout, &stderr)
	return status, stdout.String(), stderr.String()
}

// reportOf is the report a run wrote: one line, one object.
func reportOf(t *testing.T, stdout string) map[string]json.RawMessage {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("the report is not one line: %q", stdout)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

// verify-preparation reads each preparation vector (vectors/preparation) as
// its manifest says a preparation verifier reads it, given as a fixture is:
// preparation.json, with preparation.sha256 as the expected digest. A vector
// whose inputs were changed after the run (retained "recorded") is the
// finding verify-run names, exit 2; every other is a reading of
// preparation.sha256, exit 0, its binding not-checked. A vector bound at
// import is read alike, and reported byte for byte alike, given bound, given
// published with --binding, as the specification's verifier is called, and
// given with --unbound.
func TestVerifyPreparationReportsEachVector(t *testing.T) {
	dir := "../../vectors/preparation/"
	raw, err := os.ReadFile(dir + "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Vectors []struct {
			Name        string            `json:"name"`
			Files       map[string]string `json:"files"`
			Preparation struct {
				SHA256   string `json:"sha256"`
				Retained string `json:"retained"`
			} `json:"preparation"`
			Import    *json.RawMessage `json:"import"`
			VerifyRun struct {
				Error *string `json:"error"`
			} `json:"verifyRun"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &m); err != nil || len(m.Vectors) != 41 {
		t.Fatal("not the forty-one vectors:", err, len(m.Vectors))
	}
	for _, v := range m.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			at := func(name string) string { return filepath.Join(dir, v.Name, name) }
			sum, err := os.ReadFile(at("preparation.sha256"))
			if err != nil || string(sum) != v.Preparation.SHA256 {
				t.Fatal("the vector's digest is not its manifest's:", err)
			}
			given := []string{"--case", at("case.json"), "--profiles", at("profiles.json"), "--preparation", at("preparation.json"), "--preparation-digest", string(sum)}
			if _, ok := v.Files["sources.json"]; ok {
				given = append(given, "--sources", at("sources.json"))
			}
			forms := [][]string{{"--dialect", at("mapping.json")}}
			if v.Import != nil {
				forms = append(forms, []string{"--dialect", at("unbound.json"), "--binding", at("binding.json")}, []string{"--unbound", at("unbound.json"), "--binding", at("binding.json")})
			}
			var first string
			for i, form := range forms {
				status, stdout, stderr := runVerifyPreparation(append(form, given...)...)
				report := reportOf(t, stdout)
				if v.Preparation.Retained == "recorded" {
					if status != 2 || stdout != `{"finding":"acquisition-invalid","result":"finding"}`+"\n" || stderr != "verify-preparation: acquisition-invalid: "+*v.VerifyRun.Error+"\n" {
						t.Fatalf("%v: %d %s %s", form, status, stdout, stderr)
					}
				} else if status != 0 || string(report["result"]) != `"reading"` || string(report["digest"]) != `"`+v.Preparation.SHA256+`"` || string(report["binding"]) != `"not-checked"` || !strings.HasPrefix(stderr, "verify-preparation: reading: ") || strings.Count(stderr, "\n") != 1 {
					t.Fatalf("%v: %d %s %s", form, status, stdout, stderr)
				}
				if i == 0 {
					first = stdout
				} else if stdout != first {
					t.Fatalf("%v reports otherwise than the bound dialect:\n%s\n%s", form, stdout, first)
				}
			}
		})
	}
}

// exchangeFixtures is where the Exchange's example fixtures are copied
// (testdata/exchange/README.md).
const exchangeFixtures = "testdata/exchange"

// verify-preparation reads the Exchange's example fixtures as the catalog's
// tests call the specification's preparation verifier on them: the entry's
// dialect.json, and the fixture's sources.json and binding.json when it has
// them. Each is what the catalog requires of that verifier: a reading whose
// digest is the fixture's preparationDigest, or the refusal kind-unread for a
// kind the verifier does not read, which the catalog skips naming the kind.
func TestVerifyPreparationReadsTheExchangeFixtures(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(exchangeFixtures, "*", "entries", "example", "*", "entry.json"))
	if err != nil {
		t.Fatal(err)
	}
	readings, unread := 0, 0
	for _, path := range entries {
		directory := filepath.Dir(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var entry struct {
			SourceKinds []string `json:"sourceKinds"`
			Fixtures    []struct {
				Name              string `json:"name"`
				Path              string `json:"path"`
				PreparationDigest string `json:"preparationDigest"`
			} `json:"fixtures"`
		}
		if err = json.Unmarshal(raw, &entry); err != nil || len(entry.Fixtures) == 0 {
			t.Fatal("an example entry without fixtures:", path, err)
		}
		read := true
		for _, kind := range entry.SourceKinds {
			read = read && (kind == "operation" || kind == "research")
		}
		for _, fixture := range entry.Fixtures {
			t.Run(strings.TrimPrefix(directory, exchangeFixtures+string(filepath.Separator))+"/"+fixture.Name, func(t *testing.T) {
				at := func(name string) string { return filepath.Join(directory, fixture.Path, name) }
				args := []string{"--dialect", filepath.Join(directory, "dialect.json"), "--case", at("case.json"), "--profiles", at("profiles.json"), "--preparation", at("preparation.json"), "--preparation-digest", fixture.PreparationDigest}
				for flag, name := range map[string]string{"--sources": "sources.json", "--binding": "binding.json"} {
					if _, err := os.Stat(at(name)); err == nil {
						args = append(args, flag, at(name))
					}
				}
				status, stdout, stderr := runVerifyPreparation(args...)
				report := reportOf(t, stdout)
				if !read {
					if status != 1 || stdout != `{"refusal":"kind-unread","result":"refusal"}`+"\n" || !strings.Contains(stderr, "of kind "+strings.Join(entry.SourceKinds, ", ")) {
						t.Fatalf("a fixture of the kinds %v: %d %s %s", entry.SourceKinds, status, stdout, stderr)
					}
					unread++
					return
				}
				if status != 0 || string(report["digest"]) != `"`+fixture.PreparationDigest+`"` || string(report["binding"]) != `"not-checked"` {
					t.Fatalf("%d %s %s", status, stdout, stderr)
				}
				readings++
			})
		}
	}
	if readings != 10 || unread != 1 {
		t.Fatalf("%d readings and %d unread kinds, not the examples' 10 and 1", readings, unread)
	}
}

// --capabilities prints what verify-preparation reads, as one object, the
// release it was built from among it, and nothing else.
func TestVerifyPreparationCapabilities(t *testing.T) {
	status, stdout, stderr := runVerifyPreparation("--capabilities")
	if status != 0 || stdout != string(runner.PreparationVerifierCapabilities(buildinfo.Version()))+"\n" || stderr != "" {
		t.Fatalf("%d %s %s", status, stdout, stderr)
	}
	report := reportOf(t, stdout)
	if string(report["command"]) != `"verify-preparation"` || string(report["version"]) != "1" || string(report["dialectVersions"]) != "[2,3]" || string(report["sourceKinds"]) != `["operation","research"]` {
		t.Fatal(stdout)
	}
}

// The expected preparation's digest is a precondition, as a file the command
// can open is: a preparation the digest supplied with it does not name is not
// read, and the command could not run, exit 3, with no report and the reason
// on standard error. A preparation over its bound is not read whole, nor
// digested: the reading refuses it, input-over-bound.
func TestVerifyPreparationHoldsThePreparationToItsDigest(t *testing.T) {
	dir := "../../vectors/preparation/valid/"
	given := []string{"--dialect", dir + "mapping.json", "--case", dir + "case.json", "--profiles", dir + "profiles.json", "--sources", dir + "sources.json"}
	other := "sha256:" + strings.Repeat("0", 64)
	status, stdout, stderr := runVerifyPreparation(append(given, "--preparation", dir+"preparation.json", "--preparation-digest", other)...)
	if status != couldNotRun || stdout != "" || !strings.HasPrefix(stderr, "verify-preparation: could not run: the preparation's bytes are ") || !strings.Contains(stderr, other) || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("%d %q %q", status, stdout, stderr)
	}
	large := filepath.Join(t.TempDir(), "preparation.json")
	if err := os.WriteFile(large, bytes.Repeat([]byte(" "), 8<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	status, stdout, _ = runVerifyPreparation(append(given, "--preparation", large, "--preparation-digest", other)...)
	if status != 1 || stdout != `{"refusal":"input-over-bound","result":"refusal"}`+"\n" {
		t.Fatalf("%d %s", status, stdout)
	}
}

// What the command does not take, and a file it cannot read, run no reading:
// exit 3, no report, and the reason on standard error.
func TestVerifyPreparationCouldNotRun(t *testing.T) {
	dir := "../../vectors/preparation/unbound-bound-at-import/"
	sum, err := os.ReadFile(dir + "preparation.sha256")
	if err != nil {
		t.Fatal(err)
	}
	rest := []string{"--case", dir + "case.json", "--profiles", dir + "profiles.json", "--sources", dir + "sources.json", "--preparation", dir + "preparation.json", "--preparation-digest", string(sum)}
	for name, args := range map[string][]string{
		"nothing":                          nil,
		"help":                             {"-h"},
		"a flag it does not take":          append([]string{"--dialect", dir + "mapping.json", "--record", dir + "run.json"}, rest...),
		"an argument":                      append([]string{"--dialect", dir + "mapping.json", "extra"}, rest...),
		"both --dialect and --unbound":     append([]string{"--dialect", dir + "mapping.json", "--unbound", dir + "unbound.json", "--binding", dir + "binding.json"}, rest...),
		"--unbound without --binding":      append([]string{"--unbound", dir + "unbound.json"}, rest...),
		"no dialect":                       rest,
		"no expected digest":               append([]string{"--dialect", dir + "mapping.json"}, rest[:len(rest)-2]...),
		"a digest of another form":         append(append([]string{"--dialect", dir + "mapping.json"}, rest[:len(rest)-1]...), strings.ToUpper(string(sum))),
		"a digest that does not name it":   append(append([]string{"--dialect", dir + "mapping.json"}, rest[:len(rest)-1]...), "sha256:"+strings.Repeat("0", 64)),
		"a file that is not there":         append([]string{"--dialect", dir + "absent.json"}, rest...),
		"a directory":                      append([]string{"--dialect", dir}, rest...),
		"a flag naming no file":            append([]string{"--dialect", dir + "mapping.json", "--binding", ""}, rest...),
		"--capabilities with another flag": {"--capabilities", "--dialect", dir + "mapping.json"},
	} {
		t.Run(name, func(t *testing.T) {
			status, stdout, stderr := runVerifyPreparation(args...)
			if status != couldNotRun || stdout != "" || stderr == "" {
				t.Fatalf("%d %q %q", status, stdout, stderr)
			}
		})
	}
	// Given as it should be, the same files read.
	if status, _, stderr := runVerifyPreparation(append([]string{"--dialect", dir + "mapping.json"}, rest...)...); status != 0 {
		t.Fatal(stderr)
	}
}

// readAtMost reads no more of a file than its limit, so a document over its
// bound is refused by its length and never taken in whole.
func TestReadAtMostReadsNoMoreThanItsLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.json")
	content := bytes.Repeat([]byte("0123456789"), 100)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{1, 10, 999} {
		got, err := readAtMost(path, limit)
		if err != nil || !bytes.Equal(got, content[:limit]) {
			t.Fatalf("readAtMost(%d) read %d bytes: %v", limit, len(got), err)
		}
	}
	if got, err := readAtMost(path, 5000); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("readAtMost read %d bytes of a shorter file: %v", len(got), err)
	}
}
