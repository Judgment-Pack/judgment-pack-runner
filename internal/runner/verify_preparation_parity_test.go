package runner

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func parityFixture(t *testing.T) PreparationFixture {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(preparationVectors + "/valid/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return PreparationFixture{Dialect: read("mapping.json"), Case: read("case.json"), Profiles: read("profiles.json"), Sources: read("sources.json"), Preparation: read("preparation.json")}
}
func mutateOfflineJSON(raw []byte, change func(map[string]any)) []byte {
	var value map[string]any
	json.Unmarshal(raw, &value)
	change(value)
	return encode(value)
}
func TestOfflinePreparationReviewedBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*PreparationFixture)
		code   string
	}{

		{"legacy facts array", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { v["facts"] = []any{} })
		}, codeMalformed},
		{"null optional profile research", func(f *PreparationFixture) {
			var p []map[string]any
			json.Unmarshal(f.Profiles, &p)
			p[0]["research"] = nil
			f.Profiles = encode(p)
		}, codeMalformed},
		{"null optional profile calculator", func(f *PreparationFixture) {
			var p []map[string]any
			json.Unmarshal(f.Profiles, &p)
			p[0]["calculator"] = nil
			f.Profiles = encode(p)
		}, codeMalformed},
		{"null case parameters", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { v["case"].(map[string]any)["parameters"] = nil })
		}, codeMalformed},
		{"null source parameters", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { v["sources"].([]any)[0].(map[string]any)["parameters"] = nil })
		}, codeMalformed},
		{"receipt signature", func(f *PreparationFixture) {
			f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["receipt"].(map[string]any)["signature"] = "bad"
			})
		}, codeInvalid},
		{"receipt timestamp", func(f *PreparationFixture) {
			f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["receipt"].(map[string]any)["servedAt"] = "bad"
			})
		}, codeInvalid},
		{"nested result alias", func(f *PreparationFixture) {
			f.Sources = []byte(strings.Replace(string(f.Sources), "\"result\":", "\"Result\":", 1))
		}, codeMalformed},
		{"profile alias", func(f *PreparationFixture) {
			f.Profiles = []byte(strings.Replace(string(f.Profiles), "\"class\":", "\"Class\":", 1))
		}, codeMalformed},
		{"extra statement salt", func(f *PreparationFixture) {
			f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["salts"].(map[string]any)["statement"] = "bad"
			})
		}, codeMalformed},
		{"salt form before profiles", func(f *PreparationFixture) {
			f.Profiles = []byte("[]")
			f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["salts"].(map[string]any)["args"] = "bad"
			})
		}, codeMalformed},
		{"preparation time alias", func(f *PreparationFixture) {
			f.Preparation = []byte(strings.Replace(string(f.Preparation), "\"verifiedAt\":", "\"VerifiedAt\":", 1))
		}, codeMalformed},
		{"undefined family", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { v["version"] = 4 })
		}, codeMalformed},
		{"missing profile endpoint", func(f *PreparationFixture) {
			var p []map[string]any
			json.Unmarshal(f.Profiles, &p)
			delete(p[0], "endpoint")
			f.Profiles = encode(p)
		}, codeMalformed},
		{"case nesting", func(f *PreparationFixture) {
			f.Case = []byte(strings.Repeat(`{"x":`, 33) + `0` + strings.Repeat("}", 33))
		}, codeOverBound},
		{"numeric exponent", func(f *PreparationFixture) { f.Case = []byte(`{"x":1e309}`) }, codeOverBound},
		{"huge exponent", func(f *PreparationFixture) { f.Case = []byte(`{"x":1e999999999999999999}`) }, codeOverBound},
		{"null profiles", func(f *PreparationFixture) { f.Profiles = []byte("null") }, codeMalformed},
		{"response case alias", func(f *PreparationFixture) {
			f.Sources = []byte(strings.Replace(string(f.Sources), `"response":`, `"Response":`, 1))
		}, codeMalformed},
		{"invalid salt", func(f *PreparationFixture) {
			f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["salts"].(map[string]any)["args"] = "bad"
			})
		}, codeMalformed},
		{"missing parameter", func(f *PreparationFixture) { f.Case = []byte(`{"currency":"EUR"}`) }, "parameter-invalid"},
		{"empty binding", func(f *PreparationFixture) { f.Binding = []byte("{}") }, codeMalformed},
		{"extra preparation member", func(f *PreparationFixture) {
			f.Preparation = mutateOfflineJSON(f.Preparation, func(v map[string]any) { v["extra"] = true })
		}, "preparation-differs"},
		{"admission", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) {
				v["admits"] = map[string]any{"facts": map[string]any{"/request": []string{"asserted"}}}
			})
		}, codeAdmission},
		{"acquisition before admission", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) {
				v["admits"] = map[string]any{"facts": map[string]any{"/request": []string{"asserted"}}}
			})
			f.Sources = []byte("{}")
		}, codeMissing},
		{"count before malformed form", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { v["sources"] = make([]any, 17); v["foreign"] = true })
		}, codeOverBound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := parityFixture(t)
			tt.mutate(&f)
			report := VerifyPreparation(f)
			if report.Code != tt.code {
				t.Fatalf("%+v", report)
			}
		})
	}
}
func TestOfflinePreparationValidLegacyOmissions(t *testing.T) {
	f := parityFixture(t)
	var m map[string]any
	json.Unmarshal(f.Dialect, &m)
	m["unmapped"] = []any{}
	m["sources"].([]any)[0].(map[string]any)["parameters"] = map[string]any{}
	delete(m, "provider")
	delete(m, "facts")
	delete(m, "evidence")
	f.Dialect = encode(m)
	// Omitting legacy members changes only the document identity, not the
	// preparation's outcomes or receipts.
	canonicalDialect, err := canonical(f.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	f.Preparation = mutateOfflineJSON(f.Preparation, func(v map[string]any) { v["mappingDigest"] = digest(canonicalDialect) })
	f.Preparation, err = canonical(f.Preparation)
	if err != nil {
		t.Fatal(err)
	}
	report := VerifyPreparation(f)
	if report.Result != "reading" {
		t.Fatalf("%+v", report)
	}
}

func TestOfflinePreparationRejectsNullContainers(t *testing.T) {
	for _, member := range []string{"sources", "unmapped", "unmappedEvidence", "admits", "case", "provider"} {
		f := parityFixture(t)
		f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { v[member] = nil })
		result := VerifyPreparation(f)
		if result.Result != ResultRefusal || result.Code != codeMalformed {
			t.Fatalf("%s: %+v", member, result)
		}
	}
}
