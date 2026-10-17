package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func workflowReaderInput(t *testing.T, w workflowInventory, profiles []InputProfile) workflowReadInput {
	t.Helper()
	if profiles == nil {
		profiles = []InputProfile{}
	}
	files, err := w.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	input := workflowReadInput{Files: files, WorkflowDigest: w.Execution.WorkflowDigest, ExecutionDigest: strings.TrimSpace(string(files["execution.sha256"])), RuntimeDigest: w.Execution.Frozen[0].RuntimeDigest, Profiles: map[string][]byte{}}
	for _, s := range w.Document.Stages {
		input.Profiles[s.ID] = encode(profiles)
	}
	return input
}
func workflowReaderWant(t *testing.T, in workflowReadInput, result, code string) workflowReadReport {
	t.Helper()
	report, err := readWorkflowRecording(in)
	if err != nil || report.Result != result || report.Code != code {
		t.Fatalf("got %+v, %v; want %s %s", report, err, result, code)
	}
	return report
}
func workflowReaderCopy(in workflowReadInput) workflowReadInput {
	out := in
	out.Files = map[string][]byte{}
	for p, b := range in.Files {
		out.Files[p] = bytes.Clone(b)
	}
	out.Profiles = map[string][]byte{}
	for p, b := range in.Profiles {
		out.Profiles[p] = bytes.Clone(b)
	}
	return out
}
func workflowReaderExecution(t *testing.T, in *workflowReadInput, edit func(*workflowExecution)) {
	t.Helper()
	var e workflowExecution
	if strictJSON(in.Files["execution.json"], &e) != nil {
		t.Fatal("execution form")
	}
	edit(&e)
	raw, err := workflowCanonical(e, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	in.Files["execution.json"] = raw
	in.ExecutionDigest = digest(raw)
	in.Files["execution.sha256"] = []byte(in.ExecutionDigest + "\n")
}
func TestWorkflowReaderUnstartedPartialAndIndependentPins(t *testing.T) {
	w := workflowUnstartedFixture(t)
	var release Release
	if strictJSON(w.Artifacts["release/"+w.Execution.Frozen[0].ReleaseDigest], &release) != nil {
		t.Fatal("release")
	}
	in := workflowReaderInput(t, w, release.InputProfiles)
	report := workflowReaderWant(t, in, ResultReading, "")
	if report.Completion != "partial" || report.Instances != 1 || report.Outputs != 0 {
		t.Fatal(report)
	}
	changed := workflowReaderCopy(in)
	changed.WorkflowDigest = digest([]byte("different"))
	workflowReaderWant(t, changed, ResultFinding, "digest-mismatch")
	changed = workflowReaderCopy(in)
	delete(changed.Files, "root-case.json")
	workflowReaderWant(t, changed, ResultRefusal, "inventory-mismatch")
	changed = workflowReaderCopy(in)
	changed.Profiles = map[string][]byte{}
	workflowReaderWant(t, changed, ResultFinding, "stage-binding-mismatch")
}
func TestWorkflowReaderRealCompanySelectedPeople(t *testing.T) {
	w, profiles := workflowCompanyPeopleRecording(t)
	in := workflowReaderInput(t, w, profiles)
	report := workflowReaderWant(t, in, ResultReading, "")
	if report.Completion != "complete" || report.Stages != 2 || report.Instances != 3 || report.Outputs != 5 || report.Selections != 1 {
		t.Fatal(report)
	}
	line := report.line()
	var decoded map[string]any
	if len(line) > 4096 || line[len(line)-1] != '\n' || json.Unmarshal(line, &decoded) != nil || decoded["authentication"] != "not-established" || decoded["dispositions"] != "not-replayed" {
		t.Fatal(string(line))
	}
	t.Run("missing selected child", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		workflowReaderExecution(t, &changed, func(e *workflowExecution) { e.Instances = e.Instances[:2] })
		workflowReaderWant(t, changed, ResultFinding, "inventory-mismatch")
	})
	t.Run("completed zero cannot become empty failure", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		workflowReaderExecution(t, &changed, func(e *workflowExecution) {
			e.Instances[2].State = "failed"
			e.Instances[2].Failure = workflowPin("output-invalid")
			e.Instances[2].Attempts[0].State = "failed"
			e.Instances[2].Attempts[0].Failure = workflowPin("output-invalid")
		})
		workflowReaderWant(t, changed, ResultFinding, "inventory-mismatch")
	})
	t.Run("malformed unselected disposition precedes digest", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		path := "instances/instance-1/attempts/1/audit.json"
		a := decodedJSON(t, changed.Files[path])
		a["disposition"] = map[string]any{"kind": "unresolved", "reasons": []any{}, "handoff": map[string]any{"state": "none"}}
		changed.Files[path] = encode(a)
		changed.WorkflowDigest = digest([]byte("also different"))
		workflowReaderWant(t, changed, ResultRefusal, codeMalformed)
	})
}
func decodedJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func workflowReaderReplaceOutput(t *testing.T, in *workflowReadInput, path string, edit func(*workflowOutputRecord)) {
	t.Helper()
	var o workflowOutputRecord
	if err := strictJSON(in.Files[path], &o); err != nil {
		t.Fatal(err)
	}
	edit(&o)
	raw, err := workflowCanonical(o, 65536)
	if err != nil {
		t.Fatal(err)
	}
	in.Files[path] = raw
}
func TestWorkflowReaderArtifactIdentityAndPhaseOrder(t *testing.T) {
	w, profiles := workflowCompanyPeopleRecording(t)
	in := workflowReaderInput(t, w, profiles)
	for _, path := range []string{"root-case.json", "instances/instance-1/attempts/1/preparation.json", "outputs/companies/instance-1/0/payload.json"} {
		if _, ok := in.Files[path]; !ok {
			continue
		}
		t.Run("literal bytes "+path, func(t *testing.T) {
			changed := workflowReaderCopy(in)
			changed.Files[path] = append(changed.Files[path], '\n')
			workflowReaderWant(t, changed, ResultRefusal, codeMalformed)
		})
	}
	t.Run("forged output owner cannot bypass digest", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		path := "outputs/companies/instance-1/0/record.json"
		workflowReaderReplaceOutput(t, &changed, path, func(o *workflowOutputRecord) { o.Lineage.Instance = "foreign-instance" })
		workflowReaderWant(t, changed, ResultFinding, "digest-mismatch")
	})
	t.Run("inventory precedes output mismatch", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		// A successful zero-output child with a fabricated output-invalid failure is
		// an inventory defect even alongside a correctly pinned wrong relation.
		path := "outputs/people/instance-2/0/record.json"
		workflowReaderReplaceOutput(t, &changed, path, func(o *workflowOutputRecord) { o.Lineage.Relation.Label = "wrong-relation" })
		pin := digest(changed.Files[path])
		workflowReaderExecution(t, &changed, func(e *workflowExecution) {
			e.OutputSets[1].Records[0] = pin
			e.Instances[2].State = "failed"
			e.Instances[2].Failure = workflowPin("output-invalid")
			e.Instances[2].Attempts[0].State = "failed"
			e.Instances[2].Attempts[0].Failure = workflowPin("output-invalid")
		})
		set := decodedJSON(t, changed.Files["sets/people.json"])
		records := set["records"].([]any)
		records[0] = pin
		changed.Files["sets/people.json"], _ = workflowCanonical(set, 8<<20)
		workflowReaderWant(t, changed, ResultFinding, "inventory-mismatch")
	})
	t.Run("relation mismatch", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		path := "outputs/people/instance-2/0/record.json"
		workflowReaderReplaceOutput(t, &changed, path, func(o *workflowOutputRecord) { o.Lineage.Relation.Label = "wrong-relation" })
		pin := digest(changed.Files[path])
		workflowReaderExecution(t, &changed, func(e *workflowExecution) { e.OutputSets[1].Records[0] = pin })
		set := decodedJSON(t, changed.Files["sets/people.json"])
		set["records"].([]any)[0] = pin
		changed.Files["sets/people.json"], _ = workflowCanonical(set, 8<<20)
		workflowReaderWant(t, changed, ResultFinding, "output-mismatch")
	})
	t.Run("run facts must agree with preparation", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		path := "instances/instance-1/attempts/1/run.json"
		var b VerificationBundle
		_ = strictJSON(changed.Files[path], &b)
		b.Run.Input.Facts = []byte(`{"organisations":[]}`)
		changed.Files[path] = encode(b)
		workflowReaderExecution(t, &changed, func(e *workflowExecution) {
			e.Instances[0].Attempts[0].RunDigest = workflowPin(digest(changed.Files[path]))
		})
		workflowReaderWant(t, changed, ResultFinding, "preparation-mismatch")
	})
}

func TestWorkflowReaderBlockedEmptyAndStoppedRemainDistinct(t *testing.T) {
	w, profiles := workflowCompanyPeopleRecording(t)
	root := w.copy()
	root.Execution.Instances = root.Execution.Instances[:1]
	root.Execution.OutputSets[1].Status = "blocked"
	root.Execution.OutputSets[1].Records = []string{}
	root.Execution.Selections[0] = nil
	blocked := workflowReaderWant(t, workflowReaderInput(t, root, profiles), ResultReading, "")
	if blocked.Completion != "partial" || blocked.Selections != 0 || blocked.Instances != 1 {
		t.Fatal(blocked)
	}
	empty, err := selectWorkflow(root, workflowSelectionFor(t, root, []string{}, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	report := workflowReaderWant(t, workflowReaderInput(t, empty, profiles), ResultReading, "")
	if report.Completion != "complete" || report.Selections != 1 || report.Instances != 1 {
		t.Fatal(report)
	}
	stopped := w.copy()
	stopped.Execution.OutputSets[1].Records = []string{}
	for i := 1; i < len(stopped.Execution.Instances); i++ {
		stopped.Execution.Instances[i].State = "cancelled"
		stopped.Execution.Instances[i].Failure = nil
		stopped.Execution.Instances[i].Attempts = []workflowAttempt{}
	}
	report = workflowReaderWant(t, workflowReaderInput(t, stopped, profiles), ResultReading, "")
	if report.Completion != "partial" || report.Instances != 3 || report.Outputs != 3 {
		t.Fatal(report)
	}
}
func TestWorkflowReaderDoesNotRequireWriterInstanceNames(t *testing.T) {
	w := workflowUnstartedFixture(t)
	w.Execution.Instances[0].ID = "my-root"
	var release Release
	_ = strictJSON(w.Artifacts["release/"+w.Execution.Frozen[0].ReleaseDigest], &release)
	workflowReaderWant(t, workflowReaderInput(t, w, release.InputProfiles), ResultReading, "")
}

func TestWorkflowReaderProjectionBoundCannotBeInstanceProjectionFailure(t *testing.T) {
	large := make([]string, 20)
	for i := range large {
		large[i] = strings.Repeat("x", 40000)
	}
	payload := encode(map[string]any{"large": large})
	if _, _, err := workflowCase(payload); err != nil {
		t.Fatal("parent must fit its own domain", err)
	}
	parentPin := digest([]byte("parent"))
	r := workflowReader{in: workflowReadInput{Files: map[string][]byte{}}, w: workflowInventory{Document: workflowDocument{Stages: []workflowStage{{ID: "companies"}, {ID: "people", CaseBindings: []workflowBinding{{From: "/large", To: "/a", Type: "array", Required: true}, {From: "/large", To: "/b", Type: "array", Required: true}}}}}, Execution: workflowExecution{Instances: []workflowInstance{{ID: "child", Stage: "people", ParentRecord: &parentPin, State: "failed", Failure: workflowPin("projection-invalid"), Attempts: []workflowAttempt{}}}}}, stages: []workflowReadStage{{outputs: []workflowReadOutput{{pin: parentPin, payload: payload}}}, {}}, attempts: [][]workflowReadAttempt{{}}}
	err := r.casesAndInfluence()
	var problem workflowReadProblem
	if !errors.As(err, &problem) || problem.result != ResultFinding || problem.code != "case-mismatch" {
		t.Fatal(err)
	}
}
