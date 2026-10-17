package runner

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func workflowUnstartedFixture(t *testing.T) workflowInventory {
	t.Helper()
	f := newImportFixture(t)
	raw, in, runtime := workflowFreezeFixture(t, encode(f.direct), nil, []InputProfile{f.profile})
	doc := decoded(t, raw).(map[string]any)
	doc["limits"].(map[string]any)["maxAttempts"] = 3
	w, err := newWorkflowInventory(encode(doc), []byte(`{"id":7}`), "attempt-test", []workflowFreezeInput{in}, []InputProfile{f.profile}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func TestWorkflowPendingAttemptSurvivesRestartAndRequiresExplicitRetry(t *testing.T) {
	w := workflowUnstartedFixture(t)
	next, err := beginWorkflowAttempt(w, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginWorkflowAttempt(next, "instance-1", nil); err == nil {
		t.Fatal("active attempt started twice")
	}
	path := filepath.Join(t.TempDir(), "workflow.sqlite")
	store, db := openWorkflowStoreTest(t, path)
	if err := store.create(next); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, db = openWorkflowStoreTest(t, path)
	defer db.Close()
	held, revision, err := store.load(w.Execution.Execution)
	if err != nil {
		t.Fatal(err)
	}
	if held.Pending["instance-1"] != 1 {
		t.Fatal("lost interrupted activity")
	}
	recovered := recoverWorkflowAttempts(held)
	if len(recovered.Pending) != 0 || recovered.Execution.Instances[0].Attempts[0].State != "interrupted" {
		t.Fatal("recovery invented completion")
	}
	if err := store.save(recovered, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := beginWorkflowAttempt(recovered, "instance-1", nil); err == nil {
		t.Fatal("uncertain acquisition retried automatically")
	}
	retried, err := beginWorkflowAttempt(recovered, "instance-1", &workflowRetry{Actor: "operator", At: "2026-10-17T01:00:00Z"})
	if err != nil || len(retried.Execution.Instances[0].Attempts) != 2 {
		t.Fatal(err)
	}
	if len(held.Execution.Instances[0].Attempts) != 1 || len(w.Execution.Instances[0].Attempts) != 0 {
		t.Fatal("transition changed ancestor")
	}
}
func TestWorkflowInterruptedAndCancelledAreNotZeroSuccess(t *testing.T) {
	for _, state := range []string{"interrupted", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			w := workflowUnstartedFixture(t)
			active, err := beginWorkflowAttempt(w, "instance-1", nil)
			if err != nil {
				t.Fatal(err)
			}
			input, err := workflowAttemptInput(active, "instance-1")
			if err != nil {
				t.Fatal(err)
			}
			var release Release
			if strictJSON(w.Artifacts["release/"+w.Execution.Frozen[0].ReleaseDigest], &release) != nil {
				t.Fatal("release")
			}
			run := Run{SchemaVersion: "1", ID: "run_test", ReleaseID: release.ID, State: state, Input: input}
			raw := encode(verificationExport(release, run, 3))
			done, err := finishWorkflowAttempt(active, "instance-1", workflowAttemptResult{Run: raw, State: state}, release.InputProfiles)
			if err != nil || done.Execution.Instances[0].State != state || len(done.Execution.OutputSets[0].Records) != 0 || len(done.Pending) != 0 {
				t.Fatal(done.Execution, err)
			}
			attempt := done.Execution.Instances[0].Attempts[0]
			if attempt.RunDigest == nil || attempt.PreparationDigest != nil || attempt.AuditDigest != nil {
				t.Fatal(attempt)
			}
			if !bytes.Equal(done.Artifacts["run/"+*attempt.RunDigest], raw) {
				t.Fatal("lost exact export")
			}
			files, err := done.recordingFiles()
			if err != nil || len(files["instances/instance-1/attempts/1/run.json"]) == 0 {
				t.Fatal(err)
			}
			if _, err := finishWorkflowAttempt(done, "instance-1", workflowAttemptResult{State: state}, nil); err == nil {
				t.Fatal("completed attempt overwritten")
			}
		})
	}
}
func TestWorkflowFinishedAttemptReplaysRealRuntimeInputs(t *testing.T) {
	bundle, profiles, _ := completedExport(t, "asserted")
	raw, in, _ := workflowFreezeFixture(t, encode(*bundle.Release.InputMapping), nil, profiles)
	in.Release = bundle.Release
	doc := decoded(t, raw).(map[string]any)
	stage := doc["stages"].([]any)[0].(map[string]any)
	stage["pack"] = map[string]any{"id": bundle.Release.PackID, "digest": bundle.Release.PackDigest}
	stage["outputs"] = map[string]any{"pointer": "/request", "cardinality": "one"}
	w, err := newWorkflowInventory(encode(doc), bundle.Run.Input.Source.Case, "real-attempt", []workflowFreezeInput{in}, profiles, bundle.Release.RuntimeDigest)
	if err != nil {
		t.Fatal(err)
	}
	active, err := beginWorkflowAttempt(w, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	done, err := finishWorkflowAttempt(active, "instance-1", workflowAttemptResult{Run: encode(bundle), State: "completed"}, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if done.Execution.Instances[0].State != "completed" || len(done.Execution.OutputSets[0].Records) != 1 {
		t.Fatal(done.Execution)
	}
	attempt := done.Execution.Instances[0].Attempts[0]
	if *attempt.PreparationDigest != preparationDigest(bundle.Run.Input.Preparation) || *attempt.AuditDigest != digest(bundle.Run.Audit) {
		t.Fatal("artifact domains changed", attempt)
	}
	record, _, err := done.record(done.Execution.OutputSets[0].Records[0])
	if err != nil {
		t.Fatal(err)
	}
	if record.Lineage.PreparationDigest != *attempt.PreparationDigest || record.Lineage.AuditDigest != *attempt.AuditDigest {
		t.Fatal("output pins differ from attempt")
	}
	if _, err := beginWorkflowAttempt(done, "instance-1", nil); err == nil {
		t.Fatal("completed attempt rerun in place")
	}
	// Exact retained run changes are refused before output commitment.
	changed := bundle
	changed.Run.Input.Source = &SourceInput{Mapping: *bundle.Release.InputMapping, Case: json.RawMessage(`{"different":true}`)}
	if _, err := finishWorkflowAttempt(active, "instance-1", workflowAttemptResult{Run: encode(changed), State: "completed"}, profiles); err == nil {
		t.Fatal("foreign case admitted")
	}
}
func TestWorkflowEvaluationErrorRequiresCompleteCoreEnvelope(t *testing.T) {
	raw := []byte(`{"outputVersion":"2","tool":{"name":"jpack","version":"0.34.0"},"command":"experimental evaluate","status":"error","evaluationError":{"class":"malformed-input","phase":"preflight","evaluatorSpecVersion":"0.2.0-draft"},"diagnostics":[]}` + "\n")
	if !workflowEvaluationError(raw) {
		t.Fatal("valid complete envelope refused")
	}
	for _, bad := range [][]byte{
		bytes.Replace(raw, []byte(`"preflight"`), []byte(`"evaluation"`), 1),
		bytes.Replace(raw, []byte(`"malformed-input"`), []byte(`"timeout"`), 1),
		bytes.Replace(raw, []byte(`"diagnostics":[]`), []byte(`"diagnostics":[],"disposition":null`), 1),
		bytes.Replace(raw, []byte(`"outputVersion":"2"`), []byte(`"outputVersion":"2","outputVersion":"2"`), 1),
		bytes.TrimSuffix(raw, []byte("}\n")),
		[]byte(`{"message":"malformed-input"}`),
	} {
		if workflowEvaluationError(bad) {
			t.Fatal("invalid envelope admitted", string(bad))
		}
	}
	w := workflowUnstartedFixture(t)
	active, err := beginWorkflowAttempt(w, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finishWorkflowAttempt(active, "instance-1", workflowAttemptResult{State: "failed", Failure: "evaluation-error", EvaluationError: raw}, nil); !errors.Is(err, errWorkflowTransition) {
		t.Fatal("error admitted without evaluated preparation", err)
	}
}

func TestWorkflowAttemptOutputStatesAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name           string
		payload        any
		limit          int
		state, failure string
		count          int
	}{
		{"zero", []any{}, 5, "completed-zero", "", 0},
		{"duplicate-payloads", []any{map[string]any{"id": "same"}, map[string]any{"id": "same"}}, 5, "completed", "", 2},
		{"record-bound", []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}, map[string]any{"id": "c"}}, 2, "failed", "limit-exceeded", 0},
		{"missing-is-not-zero", nil, 5, "failed", "output-invalid", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			mapping := InputMapping{Version: 3, Case: &CaseMapping{Facts: []FactMapping{{Source: "/facts/request", Target: "/request"}, {Source: "/items", Target: "/items"}}, Evidence: []EvidenceMapping{{Source: "/evidence/intake-form", Requirement: "intake-form"}, {Source: "/evidence/sponsor-endorsement", Requirement: "sponsor-endorsement"}}}, UnmappedEvidence: []string{"sensitive-data-approvals"}}
			input := Input{Source: &SourceInput{Mapping: mapping, Case: encode(map[string]any{"facts": sample().Facts, "evidence": sample().Evidence, "items": tc.payload})}}
			bundle := completeExport(t, cfg, input, false)
			raw, in, _ := workflowFreezeFixture(t, encode(mapping), nil, nil)
			in.Release = bundle.Release
			doc := decoded(t, raw).(map[string]any)
			doc["limits"].(map[string]any)["maxRecords"] = tc.limit
			stage := doc["stages"].([]any)[0].(map[string]any)
			stage["pack"] = map[string]any{"id": bundle.Release.PackID, "digest": bundle.Release.PackDigest}
			stage["outputs"] = map[string]any{"pointer": "/items", "cardinality": "many"}
			w, err := newWorkflowInventory(encode(doc), bundle.Run.Input.Source.Case, "output-attempt", []workflowFreezeInput{in}, nil, bundle.Release.RuntimeDigest)
			if err != nil {
				t.Fatal(err)
			}
			active, err := beginWorkflowAttempt(w, "instance-1", nil)
			if err != nil {
				t.Fatal(err)
			}
			done, err := finishWorkflowAttempt(active, "instance-1", workflowAttemptResult{Run: encode(bundle), State: "completed"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			instance := done.Execution.Instances[0]
			failure := ""
			if instance.Failure != nil {
				failure = *instance.Failure
			}
			if instance.State != tc.state || failure != tc.failure || len(done.Execution.OutputSets[0].Records) != tc.count {
				t.Fatal(instance, done.Execution.OutputSets)
			}
			if instance.Attempts[0].PreparationDigest == nil || instance.Attempts[0].AuditDigest == nil {
				t.Fatal("lost successful evaluation on output refusal")
			}
			if tc.count == 2 {
				offered := done.Execution.OutputSets[0].Records
				a, first, _ := done.record(offered[0])
				b, second, _ := done.record(offered[1])
				if !bytes.Equal(first, second) || a.PayloadDigest != b.PayloadDigest || offered[0] == offered[1] {
					t.Fatal("duplicate payload identity collapsed")
				}
			}
		})
	}
}

func TestWorkflowAttemptCannotBypassAdmissionRefusal(t *testing.T) {
	w := workflowUnstartedFixture(t)
	w.Execution.Instances[0].State = "failed"
	w.Execution.Instances[0].Failure = workflowPin("admission-refused")
	before := encode(w)
	if _, err := beginWorkflowAttempt(w, "instance-1", nil); err == nil {
		t.Fatal("admission refusal became an acquisition")
	}
	if !bytes.Equal(before, encode(w)) {
		t.Fatal("refusal changed state")
	}
}
func TestWorkflowErrorRetainsExactStdoutAndPreparation(t *testing.T) {
	bundle, profiles, _ := completedExport(t, "asserted")
	raw, in, _ := workflowFreezeFixture(t, encode(*bundle.Release.InputMapping), nil, profiles)
	in.Release = bundle.Release
	doc := decoded(t, raw).(map[string]any)
	stage := doc["stages"].([]any)[0].(map[string]any)
	stage["pack"] = map[string]any{"id": bundle.Release.PackID, "digest": bundle.Release.PackDigest}
	w, err := newWorkflowInventory(encode(doc), bundle.Run.Input.Source.Case, "error-attempt", []workflowFreezeInput{in}, profiles, bundle.Release.RuntimeDigest)
	if err != nil {
		t.Fatal(err)
	}
	active, err := beginWorkflowAttempt(w, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Run.State = "failed"
	bundle.Run.Audit, bundle.Run.AuditBytes, bundle.Run.Result = nil, nil, nil
	stdout := []byte(`{"outputVersion":"2","tool":{"name":"jpack","version":"0.34.0"},"command":"experimental evaluate","status":"error","evaluationError":{"class":"resource-exhaustion","phase":"evaluation","evaluatorSpecVersion":"0.2.0-draft"},"diagnostics":[]}` + "\n")
	done, err := finishWorkflowAttempt(active, "instance-1", workflowAttemptResult{Run: encode(bundle), State: "failed", Failure: "evaluation-error", EvaluationError: stdout}, profiles)
	if err != nil {
		t.Fatal(err)
	}
	attempt := done.Execution.Instances[0].Attempts[0]
	if attempt.State != "failed" || attempt.Failure == nil || *attempt.Failure != "evaluation-error" || attempt.PreparationDigest == nil || attempt.AuditDigest != nil || attempt.EvaluationErrorDigest == nil || *attempt.EvaluationErrorDigest != digest(stdout) {
		t.Fatal(attempt)
	}
	files, err := done.recordingFiles()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(files["instances/instance-1/attempts/1/evaluation-error.json"], stdout) {
		t.Fatal("error stdout reconstructed")
	}
	if len(done.Execution.OutputSets[0].Records) != 0 {
		t.Fatal("error produced outputs")
	}
}

func TestWorkflowUnpreparedRunCannotSmuggleAuditSideMembers(t *testing.T) {
	w := workflowUnstartedFixture(t)
	active, err := beginWorkflowAttempt(w, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	input, err := workflowAttemptInput(active, "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	var release Release
	if strictJSON(w.Artifacts["release/"+w.Execution.Frozen[0].ReleaseDigest], &release) != nil {
		t.Fatal("release")
	}
	for _, state := range []string{"interrupted", "cancelled", "failed"} {
		t.Run(state, func(t *testing.T) {
			run := Run{SchemaVersion: "1", ID: "unfinished", ReleaseID: release.ID, State: state, Input: input, AuditBytes: []byte(`{"recordVersion":"1"}`)}
			bundle := VerificationBundle{Version: 3, ReleaseDigest: releaseDigest(release), Release: release, Run: run}
			result := workflowAttemptResult{Run: encode(bundle), State: state}
			if state == "failed" {
				result.Failure = "acquisition-failed"
			}
			if _, err := finishWorkflowAttempt(active, "instance-1", result, release.InputProfiles); err == nil {
				t.Fatal("unprepared audit side member accepted")
			}
		})
	}
}
func TestWorkflowStoreMigratesSelectionOnlyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection-only.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE workflow_executions (id TEXT PRIMARY KEY, revision INTEGER NOT NULL, document BLOB NOT NULL, record BLOB NOT NULL);CREATE TABLE workflow_artifacts(execution TEXT NOT NULL,key TEXT NOT NULL,body BLOB NOT NULL,PRIMARY KEY(execution,key),FOREIGN KEY(execution) REFERENCES workflow_executions(id));`)
	if err != nil {
		t.Fatal(err)
	}
	w := workflowUnstartedFixture(t)
	if _, err = db.Exec(`INSERT INTO workflow_executions VALUES(?,1,?,?)`, w.Execution.Execution, encode(w.Document), encode(w.Execution)); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = keepWorkflowArtifacts(tx, w); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store, db := openWorkflowStoreTest(t, path)
	defer db.Close()
	migrated, revision, err := store.load(w.Execution.Execution)
	if err != nil || revision != 1 || !bytes.Equal(encode(migrated), encode(w)) {
		t.Fatal("migration changed selection inventory", err)
	}
	active, err := beginWorkflowAttempt(migrated, "instance-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.save(active, revision); err != nil {
		t.Fatal(err)
	}
	if _, err = newWorkflowStore(db); err != nil {
		t.Fatal("migration is not repeatable", err)
	}
}

func TestWorkflowKnownSiblingBoundFailureStopsBeforeAcquisition(t *testing.T) {
	w := workflowInventoryFixture(t, 3)
	selected, err := selectWorkflow(w, workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:2], 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	// This is the state finish produces for the first over-bound sibling.
	selected.Execution.Instances[1].State = "failed"
	selected.Execution.Instances[1].Failure = workflowPin("limit-exceeded")
	next, err := beginWorkflowAttempt(selected, selected.Execution.Instances[2].ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := next.Execution.Instances[2]
	if child.State != "failed" || child.Failure == nil || *child.Failure != "limit-exceeded" || len(child.Attempts) != 0 || next.Pending[child.ID] != 0 {
		t.Fatal("known bound admitted acquisition", child)
	}
	if _, err := workflowAttemptInput(next, child.ID); err == nil {
		t.Fatal("known bound returned an acquisition input")
	}
}
func workflowSiblingFixture(t *testing.T, blockedAncestor bool) workflowInventory {
	t.Helper()
	return workflowInventoryFixture(t, 5, func(doc map[string]any) {
		stages := doc["stages"].([]any)
		sibling := decoded(t, encode(stages[1])).(map[string]any)
		sibling["id"] = "contacts"
		if blockedAncestor {
			stages[1].(map[string]any)["selection"] = map[string]any{"mode": "reviewer"}
			sibling["input"] = map[string]any{"fromStage": "people", "selected": true}
			later := decoded(t, encode(stages[1])).(map[string]any)
			later["id"] = "others"
			delete(later, "selection")
			stages = append(stages, sibling, later)
			doc["result"] = "others"
		} else {
			stages = append(stages, sibling)
			doc["result"] = "contacts"
		}
		doc["stages"] = stages
		doc["limits"].(map[string]any)["maxStages"] = len(stages)
		doc["limits"].(map[string]any)["maxAttempts"] = 3
	})
}
func TestWorkflowGlobalBudgetCommitWaitsForEarlierStages(t *testing.T) {
	w := workflowSiblingFixture(t, false)
	selected, err := selectWorkflow(w, workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:1], 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	earlier, later := selected.Execution.Instances[1].ID, selected.Execution.Instances[2].ID
	if _, err := beginWorkflowAttempt(selected, later, nil); err == nil {
		t.Fatal("later stage acquired before earlier reservation")
	}
	active, err := beginWorkflowAttempt(selected, earlier, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginWorkflowAttempt(active, later, nil); err == nil {
		t.Fatal("later stage bypassed active earlier stage")
	}
	ended, err := finishWorkflowAttempt(active, earlier, workflowAttemptResult{State: "interrupted"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	laterActive, err := beginWorkflowAttempt(ended, later, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginWorkflowAttempt(laterActive, earlier, &workflowRetry{Actor: "operator", At: "2026-10-17T01:00:00Z"}); err == nil {
		t.Fatal("earlier retry bypassed committed later admission")
	}
}
func TestWorkflowGlobalBudgetWaitsForEarlierBlockedInventory(t *testing.T) {
	w := workflowSiblingFixture(t, true)
	selected, err := selectWorkflow(w, workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:1], 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A stopped first consumer has not yet been reviewed, so its child stage
	// has no instances. That absence must not let a later sibling consume budget.
	selected.Execution.Instances[1].State = "cancelled"
	later := selected.Execution.Instances[2].ID
	if selected.Execution.OutputSets[2].Status != "blocked" {
		t.Fatal("fixture is not blocked")
	}
	if _, err := beginWorkflowAttempt(selected, later, nil); err == nil {
		t.Fatal("earlier blocked stage was skipped")
	}
	set := selected.outputSet(1)
	setBytes, err := workflowCanonical(set, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	selection := workflowSelection{SelectionVersion: 1, WorkflowDigest: set.WorkflowDigest, Execution: set.Execution, UpstreamStage: set.Stage, OutputSetDigest: digest(setBytes), Selected: []string{}, Mode: "reviewer", Revision: 1, Review: &workflowReview{Actor: "operator", Assertion: "No records selected", At: "2026-10-17T01:00:00Z"}}
	empty, err := selectWorkflow(selected, encode(selection), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beginWorkflowAttempt(empty, later, nil); err != nil {
		t.Fatal("explicit empty earlier stage did not unblock later stage", err)
	}
}

func TestWorkflowKnownBoundCannotRewriteCancelledSibling(t *testing.T) {
	w := workflowInventoryFixture(t, 3)
	selected, err := selectWorkflow(w, workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:2], 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	selected.Execution.Instances[1].State = "failed"
	selected.Execution.Instances[1].Failure = workflowPin("limit-exceeded")
	selected.Execution.Instances[2].State = "cancelled"
	before := encode(selected)
	if _, err := beginWorkflowAttempt(selected, selected.Execution.Instances[2].ID, nil); err == nil {
		t.Fatal("cancelled sibling began again")
	}
	if !bytes.Equal(before, encode(selected)) {
		t.Fatal("cancelled state changed")
	}
}
