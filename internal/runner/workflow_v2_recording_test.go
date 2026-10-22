package runner

// Recorded v2 journeys execute Runner preparation and the pinned Runtime. The
// operation endpoint is a signed test stand-in; this is neither live research
// nor evidence that the stand-in is an installed Gateway adapter.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type workflowV2JourneyFixture struct {
	service  *Service
	document []byte
	stages   []workflowFreezeInput
	profiles []InputProfile
	calls    func() []string
}

// Export is explicitly opt-in and writes only to a test-selected directory.
// The manifest is outside the bundle: it is independent verifier input, not a
// portable execution member that changes admission's byte count.
func workflowV2ExportJourney(t *testing.T, name string, w workflowV2Inventory, profiles []InputProfile) {
	t.Helper()
	files, err := w.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	hashes := map[string]string{}
	for name, raw := range files {
		total += len(raw)
		hashes[name] = digest(raw)
	}
	t.Logf("recording %s: %d files, %d bytes, %d retained instances, %d retained outputs", name, len(files), total, len(w.History), len(w.Outputs))
	root := os.Getenv("JPACK_WORKFLOW_V2_RECORDING_DIR")
	if root == "" {
		return
	}
	directory := filepath.Join(root, name)
	for name, raw := range files {
		path := filepath.Join(directory, "bundle", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := "signed synthetic stand-in; actual Runner preparation and Runtime evaluation"
	if len(profiles) == 0 {
		source = "asserted synthetic case inputs; actual Runner preparation and Runtime evaluation; no source acquisitions"
	}
	activeOutputs, activeSelections := 0, 0
	for _, set := range w.Core.Execution.OutputSets {
		activeOutputs += len(set.Records)
	}
	for _, selection := range w.Core.Execution.Selections {
		if selection != nil {
			activeSelections++
		}
	}
	manifest := map[string]any{"workflowDigest": w.Core.Execution.WorkflowDigest, "executionDigest": digest(files["execution.json"]), "execution": w.Core.Execution.Execution,
		"frozen": w.Core.Execution.Frozen, "files": hashes, "bytes": total, "fileCount": len(files),
		"retainedInstances": len(w.History), "retainedOutputs": len(w.Outputs),
		"activeInstances": len(w.Core.Execution.Instances), "activeOutputs": activeOutputs,
		"activeStages": len(w.Core.Document.Stages), "activeSelections": activeSelections, "source": source}
	if err := os.WriteFile(filepath.Join(directory, "expected-pins.json"), encode(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if profiles == nil {
		profiles = []InputProfile{}
	}
	if err := os.WriteFile(filepath.Join(directory, "historical-profiles.json"), encode(profiles), 0600); err != nil {
		t.Fatal(err)
	}
}

func workflowV2JourneyStore(t *testing.T, w workflowV2Inventory) *workflowV2Store {
	t.Helper()
	_, db := openWorkflowStoreTest(t, filepath.Join(t.TempDir(), "journey.sqlite"))
	t.Cleanup(func() { db.Close() })
	store, err := newWorkflowV2Store(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.create("main", w); err != nil {
		t.Fatal(err)
	}
	return store
}

func workflowV2JourneyTrace(t *testing.T, name string) func(workflowV2Comparison) {
	t.Helper()
	sequence := 0
	return func(c workflowV2Comparison) {
		sequence++
		root := os.Getenv("JPACK_WORKFLOW_V2_RECORDING_DIR")
		if root == "" {
			return
		}
		directory := filepath.Join(root, name, "comparisons")
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		// Byte slices encode as base64, preserving raw artifacts without an
		// alternate JSON serialization. This trace is never a bundle input.
		path := filepath.Join(directory, fmt.Sprintf("%03d.json", sequence))
		if err := os.WriteFile(path, encode(c), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func workflowV2JourneyBegin(t *testing.T, store *workflowV2Store, w workflowV2Inventory, reservation int, ids ...string) workflowV2Inventory {
	t.Helper()
	starts := []workflowV2Begin{}
	for _, id := range ids {
		starts = append(starts, workflowV2Begin{Instance: id, Number: 1})
	}
	next, err := beginWorkflowV2Group(w, reservation, starts, []string{}, []workflowV2Cancel{})
	if err != nil {
		t.Fatal("begin", err)
	}
	_, revision, err := store.load(w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.save("main", next, revision); err != nil {
		t.Fatal(err)
	}
	return next
}

func TestWorkflowV2OperationRecording(t *testing.T) {
	f := workflowV2OperationJourneyFixture(t)
	trace := workflowV2JourneyTrace(t, "operation")
	w, err := newWorkflowV2Inventory(f.document, []byte(`{"query":"synthetic companies","private":"not projected"}`), "company-people-v2", "root", f.stages, f.profiles, f.stages[0].Release.RuntimeDigest, trace)
	if err != nil {
		t.Fatal("freeze", err)
	}
	store := workflowV2JourneyStore(t, w)
	driver := f.service.workflowV2Driver(store)
	driver.Trace = trace
	workflowV2ExportJourney(t, "operation/00-root", w, f.profiles)
	w = workflowV2JourneyBegin(t, store, w, 1, "root")
	workflowV2ExportJourney(t, "operation/01-root-begun", w, f.profiles)
	w, err = driver.run(context.Background(), w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal("root", err)
	}
	if w.Core.Execution.Instances[0].State != "completed" || len(w.Core.Execution.OutputSets[0].Records) != 3 {
		t.Fatal(w.Core.Execution)
	}
	workflowV2ExportJourney(t, "operation/02-root-complete", w, f.profiles)
	offered := w.Core.Execution.OutputSets[0].Records
	w, err = selectWorkflowV2(w, workflowSelectionFor(t, w.Core, []string{offered[0], offered[2]}, 1), []string{"people-a", "people-c"}, false)
	if err != nil {
		t.Fatal("select", err)
	}
	_, revision, err := store.load(w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.save("main", w, revision); err != nil {
		t.Fatal(err)
	}
	workflowV2ExportJourney(t, "operation/03-selected", w, f.profiles)
	w = workflowV2JourneyBegin(t, store, w, w.Reservations["people-a"], "people-a", "people-c")
	workflowV2ExportJourney(t, "operation/04-children-begun", w, f.profiles)
	w, err = driver.run(context.Background(), w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal("children", err)
	}
	if len(w.Core.Execution.Instances) != 3 || w.Core.Execution.Instances[1].State != "completed" || w.Core.Execution.Instances[2].State != "completed-zero" || len(w.Core.Execution.OutputSets[1].Records) != 2 || len(w.Outputs) != 5 {
		t.Fatal(w.Core.Execution)
	}
	got := f.calls()
	if len(got) != 3 || got[1] != `research/people:{"organisationId":"a"}` || got[2] != `research/people:{"organisationId":"c"}` {
		t.Fatal("unexpected acquisitions", got)
	}
	for _, instance := range w.Core.Execution.Instances[1:] {
		if bytes.Contains(w.Core.Artifacts["case/"+*instance.CaseDigest], []byte("private")) {
			t.Fatal("private root field projected")
		}
	}
	for _, pin := range w.Core.Execution.OutputSets[1].Records {
		record, _, err := w.Core.record(pin)
		if err != nil || !record.Lineage.Influence.GeneratedInfluence || record.Lineage.ParentRecord == nil || *record.Lineage.ParentRecord != offered[0] {
			t.Fatal("lineage", record, err)
		}
	}
	workflowV2ExportJourney(t, "operation/05-complete", w, f.profiles)
	parentFiles, err := w.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err = store.load(w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal(err)
	}
	forkSelection := workflowSelectionFor(t, w.Core, []string{offered[1]}, 2)
	if _, err = store.fork(w.Core.Execution.Execution, "main", "reselected", revision, forkSelection, []string{"people-b"}, trace); err != nil {
		t.Fatal("fork", err)
	}
	forked, forkRevision, err := store.load(w.Core.Execution.Execution, "reselected")
	if err != nil {
		t.Fatal(err)
	}
	if len(forked.History) != 4 || len(forked.Core.Execution.Instances) != 2 || len(forked.Outputs) != 5 || len(forked.Core.Execution.OutputSets[1].Records) != 0 {
		t.Fatal("fork lost retained history or kept retired outputs active", forked.Core.Execution)
	}
	workflowV2ExportJourney(t, "operation/06-fork-selected", forked, f.profiles)
	forked.Trace = trace
	forked, err = beginWorkflowV2Group(forked, forked.Reservations["people-b"], []workflowV2Begin{{Instance: "people-b", Number: 1}}, []string{}, []workflowV2Cancel{})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.save("reselected", forked, forkRevision); err != nil {
		t.Fatal(err)
	}
	forked, err = driver.run(context.Background(), w.Core.Execution.Execution, "reselected")
	if err != nil {
		t.Fatal("fork child", err)
	}
	if len(forked.Outputs) != 6 || len(forked.Core.Execution.OutputSets[1].Records) != 1 || forked.Core.Execution.Instances[1].State != "completed" {
		t.Fatal(forked.Core.Execution)
	}
	parent, parentRevision, err := store.load(w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := parent.recordingFiles()
	if err != nil || parentRevision != revision || !bytes.Equal(encode(parentFiles), encode(unchanged)) {
		t.Fatal("fork modified parent branch", err)
	}
	got = f.calls()
	if len(got) != 4 || got[3] != `research/people:{"organisationId":"b"}` {
		t.Fatal("fork acquisitions", got)
	}
	workflowV2ExportJourney(t, "operation/07-fork-complete", forked, f.profiles)
}

// The research host uses the existing signed TLS stand-in, planner and
// preparer. It does not introduce a second model loop into Runner.
func TestWorkflowV2ResearchRecording(t *testing.T) {
	h := newResearchStandIn(t)
	base := gtmPlan(h)
	plans := []researchPlan{base, base}
	plans[0].sources = []researchSourcePlan{base.sources[0]}
	plans[0].parameters = map[string]Parameter{"companyNumber": {Pointer: "/companyNumber", Type: "integer"}}
	plans[0].caseDoc = `{"companyNumber":7}`
	plans[0].admits = []string{"/company"}
	plans[1].sources = []researchSourcePlan{base.sources[1]}
	plans[1].parameters = map[string]Parameter{"organisationId": {Pointer: "/organisationId", Type: "string"}}
	plans[1].caseDoc = `{"organisationId":"synthetic-company-0007"}`
	plans[1].sources[0].question = `{"$param":"organisationId"}`
	plans[1].sources[0].constraints = `{"language":"en","synthetic":true,"purpose":"Describe a synthetic public professional at the identified company."}`
	plans[1].admits = []string{"/person/id", "/qualification/contactable"}
	inputFor := func(plan researchPlan, at time.Time) Input {
		input := h.researchInput(t, plan, at)
		published := decoded(t, input.Source.Import.Dialect).(map[string]any)
		c := published["case"].(map[string]any)
		c["facts"], c["evidence"] = []any{}, []any{}
		published["unmapped"] = []string{"/request/appropriateness", "/request/completeness", "/request/embargoedInformationToUnauthorizedRecipients", "/request/type"}
		published["unmappedEvidence"] = []string{"intake-form", "sponsor-endorsement", "sensitive-data-approvals"}
		input.Source.Import.Dialect = encode(published)
		binding := decoded(t, input.Source.Import.Binding).(map[string]any)
		binding["dialect"] = digest(canonTest(t, input.Source.Import.Dialect))
		input.Source.Import.Binding = encode(binding)
		return input
	}
	cfg := testConfig(t)
	cfg.InputProfiles = base.profiles
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	doc := decoded(t, workflowExample()).(map[string]any)
	doc["workflowVersion"] = 2
	rows := doc["stages"].([]any)
	stages := []workflowFreezeInput{}
	for i, plan := range plans {
		input := inputFor(plan, time.Now().UTC().Truncate(time.Second))
		release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
		if err != nil {
			t.Fatal("research preview", i, err)
		}
		stages = append(stages, workflowFreezeInput{Published: input.Source.Import.Dialect, Binding: input.Source.Import.Binding, Release: release})
		row := rows[i].(map[string]any)
		row["dialect"] = map[string]any{"version": 3, "digest": digest(canonTest(t, input.Source.Import.Dialect))}
		row["pack"] = map[string]any{"id": release.PackID, "digest": release.PackDigest}
		pointer := "/company"
		if i == 1 {
			pointer = "/person"
			row["caseBindings"] = []any{map[string]any{"from": "/company-id/value", "to": "/organisationId", "type": "string", "required": true}}
		}
		row["outputs"] = map[string]any{"pointer": pointer, "cardinality": "one"}
	}
	trace := workflowV2JourneyTrace(t, "research")
	w, err := newWorkflowV2Inventory(encode(doc), []byte(plans[0].caseDoc), "research-company-person-v2", "company", stages, base.profiles, stages[0].Release.RuntimeDigest, trace)
	if err != nil {
		t.Fatal("research freeze", err)
	}
	store := workflowV2JourneyStore(t, w)
	driver := s.workflowV2Driver(store)
	driver.Trace = trace
	acquisitions := []string{}
	driver.Acquire = func(ctx context.Context, request workflowAcquisition) (SourceValue, error) {
		index := 0
		if request.Instance == "person" {
			index = 1
		}
		plan := plans[index]
		plan.caseDoc = string(request.Input.Source.Case)
		input := inputFor(plan, time.Now().UTC().Truncate(time.Second))
		if !bytes.Equal(canonTest(t, input.Source.Import.Dialect), canonTest(t, stages[index].Published)) {
			t.Fatal("acquisition changed frozen dialect")
		}
		acquisitions = append(acquisitions, request.Instance+":"+plan.caseDoc)
		return input.Source.Sources[plan.sources[0].name], nil
	}
	workflowV2ExportJourney(t, "research/00-root", w, base.profiles)
	w = workflowV2JourneyBegin(t, store, w, 1, "company")
	w, err = driver.run(context.Background(), w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal("research company", err)
	}
	if w.Core.Execution.Instances[0].State != "completed" || len(w.Core.Execution.OutputSets[0].Records) != 1 {
		t.Fatal(w.Core.Execution)
	}
	workflowV2ExportJourney(t, "research/01-company-complete", w, base.profiles)
	selected := w.Core.Execution.OutputSets[0].Records
	w, err = selectWorkflowV2(w, workflowSelectionFor(t, w.Core, selected, 1), []string{"person"}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := store.load(w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.save("main", w, revision); err != nil {
		t.Fatal(err)
	}
	w = workflowV2JourneyBegin(t, store, w, w.Reservations["person"], "person")
	w, err = driver.run(context.Background(), w.Core.Execution.Execution, "main")
	if err != nil {
		t.Fatal("research person", err)
	}
	if w.Core.Execution.Instances[1].State != "completed" || len(w.Core.Execution.OutputSets[1].Records) != 1 {
		t.Fatal(w.Core.Execution)
	}
	if len(acquisitions) != 2 || acquisitions[1] != `person:{"organisationId":"synthetic-company-0007"}` {
		t.Fatal(acquisitions)
	}
	if h.next != 8 {
		t.Fatalf("expected four preview and four execution TLS receipts, got %d", h.next)
	}
	record, _, err := w.Core.record(w.Core.Execution.OutputSets[1].Records[0])
	if err != nil || record.Lineage.ParentRecord == nil || *record.Lineage.ParentRecord != selected[0] || !record.Lineage.Influence.GeneratedInfluence {
		t.Fatal(record, err)
	}
	workflowV2ExportJourney(t, "research/02-complete", w, base.profiles)
}

func workflowV2OperationJourneyFixture(t *testing.T) workflowV2JourneyFixture {
	t.Helper()
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
			} else if company == "b" {
				result = []byte(`{"people":[{"id":"person-3"}]}`)
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
	t.Cleanup(gateway.Close)
	cfg.GatewayConnections = []GatewayConnection{{Profile: organisations.ID, URL: gateway.URL}, {Profile: people.ID, URL: gateway.URL}}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	stages := []workflowFreezeInput{}
	doc := decoded(t, workflowExample()).(map[string]any)
	rows := doc["stages"].([]any)
	// Capacity includes the retired first branch after the later reselection.
	doc["limits"].(map[string]any)["maxInstances"] = 4
	doc["limits"].(map[string]any)["maxRecords"] = 6
	for index, profile := range profiles {
		name, parameter := "organisations", "query"
		root := []byte(`{"query":"synthetic companies"}`)
		if index == 1 {
			name, parameter = "people", "organisationId"
			root = []byte(`{"organisationId":"a"}`)
		}
		request := map[string]any{parameter: map[string]any{"$param": parameter}}
		if index == 0 {
			// adapter-http query is an object of string templates, never a
			// template replacing the whole query object (dialect section 4.1).
			request = map[string]any{"query": map[string]any{"q": map[string]any{"$param": parameter}}}
		}
		m := InputMapping{Version: 3, Case: &CaseMapping{Parameters: map[string]Parameter{parameter: {Pointer: "/" + parameter, Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{{Name: name, Kind: "operation", Profile: profile.ID, ProfileDigest: profileHash(profile), MaxAge: 300, Arguments: encode(request), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/" + name, Source: "/" + name}}, Evidence: []EvidenceMapping{}}}}}, Unmapped: []string{"/request/appropriateness", "/request/completeness", "/request/embargoedInformationToUnauthorizedRecipients", "/request/type"}, UnmappedEvidence: []string{"intake-form", "sponsor-endorsement", "sensitive-data-approvals"}, Admits: &Admission{Facts: map[string][]string{"/" + name: {"generated"}}}}
		published := encode(unboundOf(m, name, "research-test", name, "http", "adapter-http"))
		binding := bindingOf(t, published, map[string]InputProfile{name: profile})
		arguments := []byte(`{"query":{"q":"synthetic companies"}}`)
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
	doc["workflowVersion"] = 2
	return workflowV2JourneyFixture{service: s, document: encode(doc), stages: stages, profiles: profiles,
		calls: func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }}
}
