package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The endpoint-scope vectors' runs (runner #141; spec/dialect.md §5.1, RFC
// 0018 round 15): one research source reads two pages under one page profile
// of class generated that carries an endpoint scope in place of one
// endpoint. The stand-in of research_standin_test.go serves them, at hosts of
// example.com that the test's client reaches at the stand-in's loopback
// address; every identifier and page is synthetic.

// scopeRecording is one run the endpoint-scope vectors are generated from:
// its name, the vector's; the page profile's scope; the two pages the source
// reads, as requested; and the pages the stand-in serves next in the run's
// session, which the run does not retain, by name.
type scopeRecording struct {
	name       string
	scope      []string
	pages      [2]string
	unretained map[string]string
}

// scopeRecordings are the two runs: two paths of one origin under a prefix,
// and two origins under the wildcard. Beside the first, the stand-in serves
// two pages the run does not retain, from which the refusals are made: one
// whose path is outside the prefix, which it does not continue at a slash,
// and one requested inside the scope whose redirect ends outside it.
func scopeRecordings() []scopeRecording {
	return []scopeRecording{{
		name:  "endpoint-scope-two-paths-one-origin",
		scope: []string{"https://docs.example.com/guides"},
		pages: [2]string{"https://docs.example.com/guides/alpha?item=company-0007-a", "https://docs.example.com/guides/beta?item=company-0007-b"},
		unretained: map[string]string{
			"outside":    "https://docs.example.com/guides-archive/beta?item=company-0007-b",
			"redirected": "https://docs.example.com/guides/moved?item=company-0007-b",
		},
	}, {
		name:  "endpoint-scope-wildcard-two-origins",
		scope: []string{scopeWildcard},
		pages: [2]string{"https://alpha.example.com/notes/one?item=company-0007-a", "https://beta.example.com/notes/two?item=company-0007-b"},
	}}
}

// scopePlan is the run of one research source, company-pages, asking for
// answer and page: its AI profile answers, and its page profile, pages, of
// class generated, declaring the capability page, its endpoint null and its
// scope scope, reads the two pages. Its read copies the record's whole
// fields to /company, each answer citing the responses it rests on. Every
// URL the run names is of a host of example.com.
func scopePlan(h *researchStandIn, scope []string, pages [2]string) researchPlan {
	page := h.profile("pages", "web", "adapter-web", "", &ResearchDeclaration{Capabilities: []string{"page"}})
	page.Endpoint, page.EndpointScope = nil, scope
	// The AI profile pins one endpoint, at a host of example.com too.
	ai, answers := h.aiProfile("assistant", "/answers"), "https://answers.example.com/answers"
	ai.Endpoint = &answers
	return researchPlan{
		example:    "endpoint-scope",
		parameters: map[string]Parameter{"companyNumber": {Pointer: "/companyNumber", Type: "integer"}},
		caseDoc:    `{"companyNumber":7,` + researchRequestCase + `}`,
		sources: []researchSourcePlan{{
			name:         "company-pages",
			capabilities: []string{"answer", "page"},
			question:     `{"$text":"Describe the synthetic company numbered {{companyNumber}} from its public pages: its identifier, its headcount and whether it is active."}`,
			constraints:  `{"language":"en","synthetic":true}`,
			evidence:     []string{"the pages that state each figure"},
			fields: []map[string]any{
				declare("company-id", "text", true, "The company's synthetic identifier."),
				declare("employee-count", "integer", true, "The company's headcount, as its first page states it."),
				declare("active", "boolean", true, "Whether the company trades, as its second page states it."),
			},
			read: `{"copy":{"facts":[{"target":"/company","source":"/fields"}],"evidence":[]}}`,
			ai:   "assistant", web: map[string]string{"page": "pages"},
			asks: []researchAsk{{"answer", "assistant", ""}, {"page", "pages", pages[0]}, {"page", "pages", pages[1]}},
			answers: map[string]any{
				"company-id":     found("synthetic-company-0007", 0, 1),
				"employee-count": found(json.Number("240"), 1),
				"active":         found(true, 2),
			},
		}},
		admits:   []string{"/company"},
		profiles: []InputProfile{ai, page},
	}
}

// Each endpoint-scope run is evaluated, signed, witnessed and bound, and
// verified offline as the research runs are (research_vectors_test.go): its
// two page receipts name two endpoints, each the final URL of its page with
// the query removed, and both verify under the one scoped profile. The
// exports in testdata/research were written by this test, with
// JPACK_SCOPE_EXPORT_DIR set, under the Runtime CI pins, and the vectors were
// generated from them: unretained.json, beside the first, holds the two
// responses the stand-in served next in the run's session and the run did
// not retain, each as the research record would retain it.
func TestScopedPageRunsAreSignedWitnessedAndBound(t *testing.T) {
	testConfig(t)
	dir := os.Getenv("JPACK_SCOPE_EXPORT_DIR")
	for _, r := range scopeRecordings() {
		t.Run(r.name, func(t *testing.T) {
			h := newResearchStandIn(t)
			h.redirects["docs.example.com/guides/moved"] = "https://elsewhere.example.com/landing"
			plan := scopePlan(h, r.scope, r.pages)
			run := testConfig(t)
			keyPath := signingKey(t)
			run.SigningKey = keyPath
			run.Runtime = signingRuntimeWithKey(t, run.Runtime, filepath.Join(t.TempDir(), "signing.log"), keyPath)
			run.InputProfiles = plan.profiles
			at := time.Now().UTC().Truncate(time.Second)
			input := h.researchInput(t, plan, at)
			unretained := map[string]any{}
			for name, page := range r.unretained {
				unretained[name] = h.retain(t, plan.profiles[1], "page", "", page, at)
			}
			// The endpoints the receipts name: each page's final URL, its
			// query removed, the moved page's the one it was redirected to.
			endpoints := map[string]string{}
			var record struct {
				Result struct {
					Responses []struct {
						Request struct {
							URL string `json:"url"`
						} `json:"request"`
						Receipt struct {
							Acquisition struct {
								Endpoint string `json:"endpoint"`
							} `json:"acquisition"`
						} `json:"receipt"`
					} `json:"responses"`
				} `json:"result"`
			}
			if err := json.Unmarshal(input.Source.Sources["company-pages"].Response, &record); err != nil || len(record.Result.Responses) != 3 {
				t.Fatal("the record does not retain an answer and two pages:", err)
			}
			for _, response := range record.Result.Responses[1:] {
				endpoints[response.Request.URL] = response.Receipt.Acquisition.Endpoint
			}
			for name, value := range unretained {
				response := value.(map[string]any)
				endpoints[r.unretained[name]] = response["receipt"].(map[string]any)["acquisition"].(map[string]any)["endpoint"].(string)
			}
			want := map[string]string{
				"https://docs.example.com/guides/alpha?item=company-0007-a":        "https://docs.example.com/guides/alpha",
				"https://docs.example.com/guides/beta?item=company-0007-b":         "https://docs.example.com/guides/beta",
				"https://docs.example.com/guides-archive/beta?item=company-0007-b": "https://docs.example.com/guides-archive/beta",
				"https://docs.example.com/guides/moved?item=company-0007-b":        "https://elsewhere.example.com/landing",
				"https://alpha.example.com/notes/one?item=company-0007-a":          "https://alpha.example.com/notes/one",
				"https://beta.example.com/notes/two?item=company-0007-b":           "https://beta.example.com/notes/two",
			}
			for requested, endpoint := range endpoints {
				if want[requested] != endpoint {
					t.Fatalf("the page requested at %s names the endpoint %s, not %s", requested, endpoint, want[requested])
				}
			}
			bundle := signedAndWitnessedExportOf(t, run, input, plan.profiles, "Endpoint scope: "+r.name, r.name)
			v, err := VerifyInputs(encode(bundle), plan.profiles, bundle.ReleaseDigest)
			if err != nil {
				t.Fatal(err)
			}
			if bundle.Run.Input.Source.Import == nil || v.Classes.Generated == 0 {
				t.Fatalf("the run is not of a research dialect bound at import: %+v", v.Classes)
			}
			if runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN")) && (v.Binding == nil || v.Binding.Status != BindingMatches) {
				t.Fatalf("the record does not bind the preparation: %+v", v.Binding)
			}
			if dir != "" {
				out := filepath.Join(dir, r.name)
				if err = os.MkdirAll(out, 0o755); err != nil {
					t.Fatal(err)
				}
				writeBoundFixtureOf(t, out, bundle, plan.profiles)
				if len(unretained) > 0 {
					if err = os.WriteFile(filepath.Join(out, "unretained.json"), encode(unretained), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
