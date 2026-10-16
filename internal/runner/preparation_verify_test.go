package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func offlineFixture(t *testing.T, name string) PreparationFixture {
	t.Helper()
	root := filepath.Join("..", "..", "vectors", "preparation", name)
	read := func(n string) []byte {
		b, e := os.ReadFile(filepath.Join(root, n))
		if os.IsNotExist(e) && n == "sources.json" {
			return []byte(`{}`)
		}
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	f := PreparationFixture{Dialect: read("mapping.json"), Case: read("case.json"), Acquisitions: read("sources.json"), Profiles: read("profiles.json"), Preparation: read("preparation.json")}
	if b, e := os.ReadFile(filepath.Join(root, "binding.json")); e == nil {
		f.Binding = b
		f.Dialect = read("unbound.json")
	}
	f.ExpectedDigest = digest(f.Preparation)
	return f
}
func TestOfflinePreparationReplaysEveryRecordedVector(t *testing.T) {
	raw, e := os.ReadFile("../../vectors/preparation/manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	var manifest struct {
		Vectors []struct {
			Name        string
			Preparation struct{ Retained string }
		}
	}
	if e = json.Unmarshal(raw, &manifest); e != nil {
		t.Fatal(e)
	}
	for _, v := range manifest.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			f := offlineFixture(t, v.Name)
			if v.Preparation.Retained != "equal" {
				raw, err := os.ReadFile(filepath.Join("..", "..", "vectors", "preparation", v.Name, "run.json"))
				if err != nil {
					t.Fatal(err)
				}
				var run struct {
					Run struct {
						Input struct{ Preparation json.RawMessage }
					}
				}
				if err = json.Unmarshal(raw, &run); err != nil {
					t.Fatal(err)
				}
				f.Preparation, err = canonical(run.Run.Input.Preparation)
				if err != nil {
					t.Fatal(err)
				}
				f.ExpectedDigest = digest(f.Preparation)
			}
			report := VerifyPreparation(f)
			switch v.Preparation.Retained {
			case "equal":
				if report.Result != "reading" || report.Binding != "not-checked" || report.Digest != digest(f.Preparation) {
					t.Fatalf("result=%s refusal=%s finding=%s digest=%s", report.Result, report.Refusal, report.Finding, report.Digest)
				}
			case "altered":
				if report.Finding != "preparation-differs" {
					t.Fatalf("result=%s refusal=%s finding=%s digest=%s", report.Result, report.Refusal, report.Finding, report.Digest)
				}
			case "recorded":
				if report.Finding != codeInvalid {
					t.Fatalf("result=%s refusal=%s finding=%s digest=%s", report.Result, report.Refusal, report.Finding, report.Digest)
				}
			default:
				t.Fatal("unclassified fixture")
			}
		})
	}
}
func TestOfflinePreparationNeverFallsBackToTodayOrUntrustedDigest(t *testing.T) {
	base := offlineFixture(t, "gtm-company-person-valid-types-all-six")
	for _, tt := range []struct {
		name         string
		mutate       func(*PreparationFixture)
		result, code string
	}{
		{"wrong digest", func(f *PreparationFixture) { f.ExpectedDigest = "sha256:" + strings.Repeat("0", 64) }, "finding", "preparation-differs"},
		{"missing digest", func(f *PreparationFixture) { f.ExpectedDigest = "" }, "refusal", codeMalformed},
		{"absent time", func(f *PreparationFixture) {
			var p map[string]any
			json.Unmarshal(f.Preparation, &p)
			delete(p, "verifiedAt")
			f.Preparation = encode(p)
			f.ExpectedDigest = digest(f.Preparation)
		}, "refusal", codeMalformed},
		{"invalid time", func(f *PreparationFixture) {
			var p map[string]any
			json.Unmarshal(f.Preparation, &p)
			p["verifiedAt"] = "not-a-time"
			f.Preparation = encode(p)
			f.ExpectedDigest = digest(f.Preparation)
		}, "refusal", codeMalformed},
		{"wrong profile", func(f *PreparationFixture) { f.Profiles = []byte(`[]`) }, "finding", codeMismatch},
		{"oversize profiles", func(f *PreparationFixture) { f.Profiles = []byte(strings.Repeat(" ", 65537)) }, "refusal", codeOverBound},
		{"duplicate document field", func(f *PreparationFixture) { f.Case = []byte(`{"x":1,"x":2}`) }, "refusal", codeMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := base
			tt.mutate(&f)
			r := VerifyPreparation(f)
			if r.Result != tt.result || r.Refusal+r.Finding != tt.code {
				t.Fatalf("%+v", r)
			}
		})
	}
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
		{"receipt signature", func(f *PreparationFixture) {
			f.Acquisitions = mutateOfflineJSON(f.Acquisitions, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["receipt"].(map[string]any)["signature"] = "bad"
			})
		}, codeInvalid},
		{"receipt timestamp", func(f *PreparationFixture) {
			f.Acquisitions = mutateOfflineJSON(f.Acquisitions, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["receipt"].(map[string]any)["servedAt"] = "bad"
			})
		}, codeInvalid},
		{"nested result alias", func(f *PreparationFixture) {
			f.Acquisitions = []byte(strings.Replace(string(f.Acquisitions), "\"result\":", "\"Result\":", 1))
		}, codeMalformed},
		{"profile alias", func(f *PreparationFixture) {
			f.Profiles = []byte(strings.Replace(string(f.Profiles), "\"class\":", "\"Class\":", 1))
		}, codeMalformed},
		{"extra statement salt", func(f *PreparationFixture) {
			f.Acquisitions = mutateOfflineJSON(f.Acquisitions, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["salts"].(map[string]any)["statement"] = "bad"
			})
		}, codeMalformed},
		{"salt form before profiles", func(f *PreparationFixture) {
			f.Profiles = []byte("[]")
			f.Acquisitions = mutateOfflineJSON(f.Acquisitions, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["salts"].(map[string]any)["args"] = "bad"
			})
		}, codeMalformed},
		{"preparation time alias", func(f *PreparationFixture) {
			f.Preparation = []byte(strings.Replace(string(f.Preparation), "\"verifiedAt\":", "\"VerifiedAt\":", 1))
			f.ExpectedDigest = digest(f.Preparation)
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
			f.Acquisitions = []byte(strings.Replace(string(f.Acquisitions), `"response":`, `"Response":`, 1))
		}, codeMalformed},
		{"invalid salt", func(f *PreparationFixture) {
			f.Acquisitions = mutateOfflineJSON(f.Acquisitions, func(v map[string]any) {
				v["fx"].(map[string]any)["response"].(map[string]any)["salts"].(map[string]any)["args"] = "bad"
			})
		}, codeMalformed},
		{"missing parameter", func(f *PreparationFixture) { f.Case = []byte(`{"currency":"EUR"}`) }, "parameter-invalid"},
		{"empty binding", func(f *PreparationFixture) { f.Binding = []byte("{}") }, codeMalformed},
		{"extra preparation member", func(f *PreparationFixture) {
			f.Preparation = mutateOfflineJSON(f.Preparation, func(v map[string]any) { v["extra"] = true })
			f.ExpectedDigest = digest(f.Preparation)
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
			f.Acquisitions = []byte("{}")
		}, codeMissing},
		{"count before malformed form", func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { v["sources"] = make([]any, 17); v["foreign"] = true })
		}, codeOverBound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := offlineFixture(t, "valid")
			tt.mutate(&f)
			report := VerifyPreparation(f)
			if report.Finding+report.Refusal != tt.code {
				t.Fatalf("%+v", report)
			}
		})
	}
}
func TestOfflinePreparationValidLegacyOmissions(t *testing.T) {
	f := offlineFixture(t, "valid")
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
	f.ExpectedDigest = digest(f.Preparation)
	report := VerifyPreparation(f)
	if report.Result != "reading" {
		t.Fatalf("%+v", report)
	}
}
func TestOfflinePreparationCanonicalReportIncludesEmptyEvidence(t *testing.T) {
	report := PreparationReport{Result: "reading", Evidence: json.RawMessage("{}"), Facts: json.RawMessage("{}")}
	raw, err := report.CanonicalBytes()
	if err != nil || !strings.Contains(string(raw), `"evidence":{}`) {
		t.Fatalf("%s %v", raw, err)
	}
	canonicalRaw, _ := canonical(raw)
	if string(raw) != string(canonicalRaw) {
		t.Fatal("noncanonical report")
	}
}
