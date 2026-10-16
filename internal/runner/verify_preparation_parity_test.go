package runner

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func parityFixture(t *testing.T) PreparationFixture {
	t.Helper()
	return parityVectorFixture(t, "valid")
}

// parityVectorFixture is a vector's bound dialect and the files beside it.
func parityVectorFixture(t *testing.T, vector string) PreparationFixture {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(preparationVectors + "/" + vector + "/" + name)
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
	// The cross-vendor review of #119 (runner #123), each agreeing with the
	// specification's reader: the research half of the counted bounds, each
	// count beside a malformed sibling it precedes; a research value
	// measured as a live research record is, and typed only by a declaration
	// whose name is a string; each object held to its kind's members, its
	// zero value included; a selected file's acquisition held to §5.2's
	// envelope, its kind refused after every form; and a member of the
	// acquisitions that names no source reported after the last source.
	const research = "gtm-company-person-valid-types-all-six"
	const skipped = "skipped-source-dependency-unavailable"
	dialectSource := func(f *PreparationFixture, index int, change func(map[string]any)) {
		f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) { change(v["sources"].([]any)[index].(map[string]any)) })
	}
	answer := func(f *PreparationFixture, field string, value any) {
		f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) {
			v["company"].(map[string]any)["response"].(map[string]any)["result"].(map[string]any)["fields"].(map[string]any)[field].(map[string]any)["value"] = value
		})
	}
	foreign := func(f *PreparationFixture) { dialectSource(f, 0, func(s map[string]any) { s["foreign"] = 1 }) }
	caseParameter := func(name string) func(*PreparationFixture) {
		return func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) {
				v["case"].(map[string]any)["parameters"].(map[string]any)[name].(map[string]any)["from"] = ""
			})
		}
	}
	operationProvider := func(f *PreparationFixture) { dialectSource(f, 0, func(s map[string]any) { s["provider"] = "" }) }
	// redigested pins the preparation to the digest of the dialect as given.
	redigested := func(change func(*PreparationFixture)) func(*PreparationFixture) {
		return func(f *PreparationFixture) {
			change(f)
			c, err := canonical(f.Dialect)
			if err != nil {
				panic(err)
			}
			f.Preparation = mutateOfflineJSON(f.Preparation, func(v map[string]any) { v["mappingDigest"] = digest(c) })
		}
	}
	drive := func(f *PreparationFixture) {
		dialectSource(f, 0, func(s map[string]any) {
			delete(s, "calculation")
			s["kind"], s["provider"], s["arguments"] = "selected-file", "google-drive", map[string]any{"fileId": "f", "grant": "g"}
		})
	}
	for _, tt := range []struct {
		name, vector string
		mutate       func(*PreparationFixture)
		code         string
	}{
		{"research description over beside an unknown member", research, func(f *PreparationFixture) {
			dialectSource(f, 0, func(s map[string]any) {
				s["fields"].([]any)[0].(map[string]any)["description"] = strings.Repeat("d", 2049)
			})
			foreign(f)
		}, codeOverBound},
		{"research model over beside a malformed kind", research, func(f *PreparationFixture) {
			dialectSource(f, 0, func(s map[string]any) { s["model"] = strings.Repeat("m", 257) })
			dialectSource(f, 1, func(s map[string]any) { s["kind"] = 5 })
		}, codeOverBound},
		{"research text answer over beside an unknown member", research, func(f *PreparationFixture) {
			answer(f, "company-id", strings.Repeat("x", 8193))
			foreign(f)
		}, codeOverBound},
		{"research decimal of 129 characters beside an unknown member", research, func(f *PreparationFixture) {
			answer(f, "revenue", strings.Repeat("1", 129))
			foreign(f)
		}, codeOverBound},
		{"research decimal of 128 characters in 256 bytes beside an unknown member", research, func(f *PreparationFixture) {
			answer(f, "revenue", strings.Repeat("é", 128))
			foreign(f)
		}, codeMalformed},
		{"research decimal of 100 characters in 200 bytes", research, func(f *PreparationFixture) {
			answer(f, "revenue", strings.Repeat("é", 100))
		}, codeMalformed},
		{"research range endpoint of 70 characters in 140 bytes", research, func(f *PreparationFixture) {
			answer(f, "employee-range", map[string]any{"min": strings.Repeat("é", 70), "max": "1"})
		}, codeMalformed},
		{"research field whose name is not a string", research, func(f *PreparationFixture) {
			dialectSource(f, 0, func(s map[string]any) {
				s["fields"] = append([]any{map[string]any{"description": "d", "name": 7, "required": false, "type": "text"}}, s["fields"].([]any)...)
			})
			f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) {
				v["company"].(map[string]any)["response"].(map[string]any)["result"].(map[string]any)["fields"].(map[string]any)[""] = map[string]any{"cites": []any{0}, "status": "found", "value": strings.Repeat("x", 9000)}
			})
		}, codeMalformed},
		{"case parameter from empty, the dialect's own digest", "valid", redigested(caseParameter("invoiceAmount")), codeMalformed},
		{"operation provider empty, the dialect's own digest", "valid", redigested(operationProvider), codeMalformed},
		{"case parameter from empty, the original digest", "valid", caseParameter("invoiceAmount"), codeMalformed},
		{"operation provider empty, the original digest", "valid", operationProvider, codeMalformed},
		{"case parameter from empty, rule vector", skipped, redigested(caseParameter("requestId")), codeMalformed},
		{"case parameter without its pointer", "valid", redigested(func(f *PreparationFixture) {
			f.Dialect = mutateOfflineJSON(f.Dialect, func(v map[string]any) {
				delete(v["case"].(map[string]any)["parameters"].(map[string]any)["invoiceAmount"].(map[string]any), "pointer")
			})
		}), codeMalformed},
		{"selected-file drive with a snapshot acquisition", "valid", func(f *PreparationFixture) {
			drive(f)
			f.Sources = []byte(`{"fx":{"snapshot":{"version":1}}}`)
		}, codeMalformed},
		{"selected-file drive with a response acquisition", "valid", func(f *PreparationFixture) {
			drive(f)
			f.Sources = []byte(`{"fx":{"response":{"result":{},"receipt":{},"salts":{"args":"` + strings.Repeat("00", 32) + `"}}}}`)
		}, codeKindUnread},
		{"selected-file drive with a snapshot beside its response", "valid", func(f *PreparationFixture) {
			drive(f)
			f.Sources = []byte(`{"fx":{"response":{"result":{},"receipt":{},"salts":{"args":"` + strings.Repeat("00", 32) + `"}},"snapshot":{"version":1}}}`)
		}, codeMalformed},
		{"local-file selected-file with a response acquisition", "valid", func(f *PreparationFixture) {
			dialectSource(f, 0, func(s map[string]any) {
				for _, member := range []string{"calculation", "profile", "profileDigest", "maxAge", "arguments"} {
					delete(s, member)
				}
				s["kind"], s["provider"] = "selected-file", "local-file"
			})
		}, codeKindUnread},
		{"unknown acquisition member beside a stale receipt", "valid", func(f *PreparationFixture) {
			f.Sources = mutateOfflineJSON(f.Sources, func(v map[string]any) { v["zz"] = v["fx"] })
			f.Preparation = mutateOfflineJSON(f.Preparation, func(v map[string]any) { v["verifiedAt"] = "2026-10-20T00:00:00Z" })
		}, codeStale},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := parityVectorFixture(t, tt.vector)
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
