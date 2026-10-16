package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNamedDialectSpecVectors(t *testing.T) {
	var manifest struct {
		Cases []struct {
			Name, DialectDigest, PreparationDigest string
			Files                                  map[string]string
		}
	}
	raw, e := os.ReadFile("testdata/artifact-identity/manifest.json")
	must(t, e)
	must(t, json.Unmarshal(raw, &manifest))
	for _, entry := range manifest.Cases {
		t.Run(entry.Name, func(t *testing.T) {
			dir := filepath.Join("testdata/artifact-identity", entry.Name)
			read := func(name string) []byte { b, e := os.ReadFile(filepath.Join(dir, name)); must(t, e); return b }
			for name, hash := range entry.Files {
				if digest(read(name)) != "sha256:"+hash {
					t.Fatalf("fixture file changed: %s", name)
				}
			}
			var m InputMapping
			must(t, strictJSON(read("dialect.json"), &m))
			if mappingV2Hash(m) != entry.DialectDigest {
				t.Fatal("published digest differs")
			}
			if !bytes.Equal(canonTest(t, encode(m)), read("dialect.canonical.json")) {
				t.Fatal("canonical identity differs")
			}
			var profiles []InputProfile
			must(t, strictJSON(read("profiles.json"), &profiles))
			var sources map[string]SourceValue
			must(t, strictJSON(read("sources.json"), &sources))
			input := Input{Source: &SourceInput{Mapping: m, Case: read("case.json"), Sources: sources}}
			if binding, e := os.ReadFile(filepath.Join(dir, "binding.json")); e == nil {
				input.Source.Mapping = InputMapping{}
				input.Source.Import = &DialectImport{Dialect: read("dialect.json"), Binding: binding}
				input, e = importedInput(input, profiles)
				must(t, e)
			}
			var prep Preparation
			must(t, strictJSON(read("preparation.json"), &prep))
			at, e := time.Parse("2006-01-02T15:04:05Z", prep.VerifiedAt)
			must(t, e)
			prepared, e := normalizeV2(input, profiles, at)
			must(t, e)
			computed, e := canonical(encode(prepared.Preparation))
			must(t, e)
			if digest(computed) != entry.PreparationDigest {
				t.Fatal("preparation digest differs from manifest")
			}
			if !bytes.Equal(computed, read("preparation.json")) {
				t.Fatal("preparation differs from Spec readers")
			}
		})
	}
}
