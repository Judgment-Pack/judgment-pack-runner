package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWorkflowHostRetainsMismatchedGatewayResponse(t *testing.T) {
	w, input, profiles, at := workflowDriverFixture(t, 1)
	response := input.Source.Sources["vendor"].Response
	calls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seal" {
			w.Write([]byte(`{}`))
			return
		}
		calls++
		w.Write(response) // valid JSON, deliberately unrelated session
	}))
	defer gateway.Close()
	s := &Service{cfg: Config{InputProfiles: profiles, GatewayConnections: []GatewayConnection{{Profile: profiles[0].ID, URL: gateway.URL}}}}
	driver := s.workflowDriver(workflowDriverStore(t, w))
	driver.Now = func() time.Time { return at }
	done, err := driver.run(context.Background(), w.Execution.Execution, "instance-1", nil)
	if err != nil || calls != 1 || done.Execution.Instances[0].State != "interrupted" {
		t.Fatal(err, calls, done.Execution)
	}
	retained := retainedWorkflowRun(t, done).Input.Source.Sources["vendor"].Response
	if !sameJSON(retained, response) {
		t.Fatal("discarded complete mismatched receipt")
	}
}

// This is a synthetic operation journey over real Runner planning and Runtime
// evaluation. No live model, website, true person or workflow verifier is claimed.
func TestWorkflowHostCompaniesSelectedPeople(t *testing.T) {
	cfg := testConfig(t)
	base, key := testProfile(t)
	base.Class = "generated"
	base.Shape = "http"
	base.Tools = nil
	base.Adapter = AdapterPin{Name: "adapter-http", Version: "workflow-stand-in", Digest: digest([]byte("synthetic workflow adapter"))}
	organisations, people := base, base
	organisations.ID, organisations.Source = "organisations", "research/organisations"
	people.ID, people.Source = "people", "research/people"
	endpointA, endpointB := "https://example.invalid/organisations", "https://example.invalid/people"
	organisations.Endpoint, people.Endpoint = &endpointA, &endpointB
	profiles := []InputProfile{organisations, people}
	cfg.InputProfiles = profiles
	var mu sync.Mutex
	calls := []string{}
	responseFor := func(profile InputProfile, args []byte, session string) []byte {
		var value map[string]any
		if err := json.Unmarshal(args, &value); err != nil {
			t.Error(err)
		}
		result := []byte(`{"organisations":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)
		if profile.ID == "people" {
			company, _ := value["organisationId"].(string)
			if company == "a" {
				result = []byte(`{"people":[{"id":"person-1"},{"id":"person-2"}]}`)
			} else if company == "c" {
				result = []byte(`{"people":[]}`)
			} else {
				t.Errorf("unselected company acquired: %s", company)
			}
		}
		raw := signResponse(t, profile, key, args, result, time.Now(), 0)
		var envelope map[string]any
		_ = json.Unmarshal(raw, &envelope)
		receipt := envelope["receipt"].(map[string]any)
		receipt["sessionId"] = session
		signReceipt(t, receipt, key)
		return encode(envelope)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seal" {
			w.Write([]byte(`{}`))
			return
		}
		var req struct {
			Session   string          `json:"session"`
			Source    string          `json:"source"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		profile := organisations
		if req.Source == people.Source {
			profile = people
		}
		mu.Lock()
		calls = append(calls, req.Source+":"+string(req.Arguments))
		mu.Unlock()
		w.Write(responseFor(profile, req.Arguments, req.Session))
	}))
	defer gateway.Close()
	cfg.GatewayConnections = []GatewayConnection{{Profile: organisations.ID, URL: gateway.URL}, {Profile: people.ID, URL: gateway.URL}}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	stages := []workflowFreezeInput{}
	doc := decoded(t, workflowExample()).(map[string]any)
	rows := doc["stages"].([]any)
	for index, profile := range profiles {
		name, parameter := "organisations", "query"
		root := []byte(`{"query":"synthetic companies"}`)
		if index == 1 {
			name, parameter = "people", "organisationId"
			root = []byte(`{"organisationId":"a"}`)
		}
		m := InputMapping{Version: 3, Case: &CaseMapping{Parameters: map[string]Parameter{parameter: {Pointer: "/" + parameter, Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{{Name: name, Kind: "operation", Profile: profile.ID, ProfileDigest: profileHash(profile), MaxAge: 300, Arguments: encode(map[string]any{parameter: map[string]any{"$param": parameter}}), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/" + name, Source: "/" + name}}, Evidence: []EvidenceMapping{}}}}}, Unmapped: []string{"/request/appropriateness", "/request/completeness", "/request/embargoedInformationToUnauthorizedRecipients", "/request/type"}, UnmappedEvidence: []string{"intake-form", "sponsor-endorsement", "sensitive-data-approvals"}, Admits: &Admission{Facts: map[string][]string{"/" + name: {"generated"}}}}
		published := encode(unboundOf(m, name, "research-test", name, "http", "adapter-http"))
		binding := bindingOf(t, published, map[string]InputProfile{name: profile})
		arguments := []byte(`{"query":"synthetic companies"}`)
		if index == 1 {
			arguments = []byte(`{"organisationId":"a"}`)
		}
		input := Input{Source: &SourceInput{Mapping: m, Import: &DialectImport{Dialect: published, Binding: binding}, Case: root, Sources: map[string]SourceValue{name: {Response: responseFor(profile, arguments, "preview-"+name)}}}}
		release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
		if err != nil {
			t.Fatal("preview", name, err)
		}
		stages = append(stages, workflowFreezeInput{Published: published, Binding: binding, Release: release})
		row := rows[index].(map[string]any)
		row["dialect"] = map[string]any{"version": 3, "digest": digest(canonTest(t, published))}
		row["pack"] = map[string]any{"id": release.PackID, "digest": release.PackDigest}
		row["outputs"] = map[string]any{"pointer": "/" + name, "cardinality": "many"}
		if index == 1 {
			row["caseBindings"] = []any{map[string]any{"from": "/id", "to": "/organisationId", "type": "string", "required": true}}
		}
	}
	w, err := newWorkflowInventory(encode(doc), []byte(`{"query":"synthetic companies","private":"not projected"}`), "company-people", stages, profiles, stages[0].Release.RuntimeDigest)
	if err != nil {
		t.Fatal(err)
	}
	store := workflowDriverStore(t, w)
	driver := s.workflowDriver(store)
	root, err := driver.run(context.Background(), w.Execution.Execution, "instance-1", nil)
	if err != nil {
		t.Fatal("root", err)
	}
	if root.Execution.Instances[0].State != "completed" || len(root.Execution.OutputSets[0].Records) != 3 {
		t.Fatal(root.Execution)
	}
	offered := root.Execution.OutputSets[0].Records
	selection := workflowSelectionFor(t, root, []string{offered[0], offered[2]}, 1)
	selected, err := selectWorkflow(root, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := store.load(w.Execution.Execution)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.save(selected, revision); err != nil {
		t.Fatal(err)
	}
	for _, instance := range selected.Execution.Instances[1:] {
		root, err = driver.run(context.Background(), w.Execution.Execution, instance.ID, nil)
		if err != nil {
			t.Fatal("child", err)
		}
	}
	if len(root.Execution.Instances) != 3 || root.Execution.Instances[1].State != "completed" || root.Execution.Instances[2].State != "completed-zero" || len(root.Execution.OutputSets[1].Records) != 2 {
		t.Fatal(root.Execution)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 3 || got[1] != `research/people:{"organisationId":"a"}` || got[2] != `research/people:{"organisationId":"c"}` {
		t.Fatal(got)
	}
	for _, instance := range root.Execution.Instances[1:] {
		if bytes.Contains(root.Artifacts["case/"+*instance.CaseDigest], []byte("private")) {
			t.Fatal("root private field leaked into child")
		}
	}
	for _, pin := range root.Execution.OutputSets[1].Records {
		record, _, err := root.record(pin)
		if err != nil || !record.Lineage.Influence.GeneratedInfluence || record.Lineage.ParentRecord == nil || *record.Lineage.ParentRecord != offered[0] {
			t.Fatal(record, err)
		}
	}
	files, err := root.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files["instances/instance-3/attempts/1/audit.json"]) == 0 {
		t.Fatal("empty child has no successful evaluation")
	}
	// Optional local artifact for a subsequent independent verifier fixture;
	// writing is explicitly selected by the test caller, never to user data.
	if directory := os.Getenv("JPACK_WORKFLOW_RECORDING_DIR"); directory != "" {
		for path, raw := range files {
			target := filepath.Join(directory, path)
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
