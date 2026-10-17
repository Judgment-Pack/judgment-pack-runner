package runner

// Draft workflow execution records. These pure transitions are used under one
// writer transaction; acquisition never occurs in a transition. The persistence
// adapter must retain each transition before starting the admitted work.
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

type workflowRetry struct {
	Actor string `json:"actor"`
	At    string `json:"at"`
}
type workflowAttempt struct {
	Number                int            `json:"number"`
	State                 string         `json:"state"`
	Failure               *string        `json:"failure"`
	PreparationDigest     *string        `json:"preparationDigest"`
	AuditDigest           *string        `json:"auditDigest"`
	RunDigest             *string        `json:"runDigest"`
	EvaluationErrorDigest *string        `json:"evaluationErrorDigest"`
	Retry                 *workflowRetry `json:"retry"`
}
type workflowInstance struct {
	ID              string            `json:"id"`
	Stage           string            `json:"stage"`
	ParentRecord    *string           `json:"parentRecord"`
	SelectionDigest *string           `json:"selectionDigest"`
	InputDigest     string            `json:"inputDigest"`
	CaseDigest      *string           `json:"caseDigest"`
	State           string            `json:"state"`
	Failure         *string           `json:"failure"`
	Attempts        []workflowAttempt `json:"attempts"`
}
type workflowStageSet struct {
	Stage   string   `json:"stage"`
	Status  string   `json:"status"`
	Records []string `json:"records"`
}
type workflowExecution struct {
	ExecutionVersion int                   `json:"executionVersion"`
	WorkflowDigest   string                `json:"workflowDigest"`
	Execution        string                `json:"execution"`
	RootCaseDigest   string                `json:"rootCaseDigest"`
	Frozen           []workflowFrozenStage `json:"frozen"`
	Instances        []workflowInstance    `json:"instances"`
	OutputSets       []workflowStageSet    `json:"outputSets"`
	Selections       []*string             `json:"selections"`
}
type workflowOutputLineage struct {
	WorkflowDigest         string            `json:"workflowDigest"`
	Execution              string            `json:"execution"`
	Stage                  string            `json:"stage"`
	Instance               string            `json:"instance"`
	PublishedDialectDigest string            `json:"publishedDialectDigest"`
	BindingDocumentDigest  *string           `json:"bindingDocumentDigest"`
	BoundDialectDigest     string            `json:"boundDialectDigest"`
	PackDigest             string            `json:"packDigest"`
	ReleaseDigest          string            `json:"releaseDigest"`
	PreparationDigest      string            `json:"preparationDigest"`
	AuditDigest            string            `json:"auditDigest"`
	OutputPointer          string            `json:"outputPointer"`
	Index                  int               `json:"index"`
	RootCaseDigest         string            `json:"rootCaseDigest"`
	CaseDigest             string            `json:"caseDigest"`
	ParentRecord           *string           `json:"parentRecord"`
	SelectionDigest        *string           `json:"selectionDigest"`
	Relation               workflowRelation  `json:"relation"`
	Influence              workflowInfluence `json:"influence"`
}
type workflowOutputRecord struct {
	OutputRecordVersion int                   `json:"outputRecordVersion"`
	PayloadDigest       string                `json:"payloadDigest"`
	Lineage             workflowOutputLineage `json:"lineage"`
}

// workflowInventory is writer-owned state. Artifact keys are kind/digest pairs;
// the exporter maps them to files. Bytes are retained separately from pins.
// No result or installation assertion in this state grants acquisition authority.
type workflowInventory struct {
	Document  workflowDocument
	Execution workflowExecution
	Artifacts map[string][]byte
}

var errWorkflowTransition = errors.New("workflow transition is not admitted")
var errWorkflowConsumed = errors.New("workflow output set is consumed; retry requires a new execution branch")

func workflowPin(value string) *string { return &value }
func workflowCanonical(value any, limit int) ([]byte, error) {
	_, canonical, err := workflowValue(encode(value), limit, 64)
	return canonical, err
}
func (w *workflowInventory) keep(kind string, raw []byte) string {
	pin := digest(raw)
	w.Artifacts[kind+"/"+pin] = bytes.Clone(raw)
	return pin
}
func (w workflowInventory) copy() workflowInventory {
	out := w
	var execution workflowExecution
	_ = json.Unmarshal(encode(w.Execution), &execution)
	out.Execution = execution
	out.Artifacts = make(map[string][]byte, len(w.Artifacts))
	// Retained artifact slices and the document are immutable after admission.
	// Copy only their index; copying all receipt bytes on every transition would
	// multiply the branch byte budget in memory.
	for key, raw := range w.Artifacts {
		out.Artifacts[key] = raw
	}
	return out
}
func (w workflowInventory) withinBounds() bool { return w.checkBounds() == nil }
func (w workflowInventory) checkBounds() error {
	files, err := w.recordingFiles()
	if err != nil {
		return err
	}
	if len(files) > 65536 {
		return errWorkflowBound
	}
	total := 0
	for _, raw := range files {
		total += len(raw)
	}
	if total > w.Document.Limits.MaxBytes {
		return errWorkflowBound
	}
	return nil
}
func newWorkflowInventory(raw, root []byte, execution string, inputs []workflowFreezeInput, profiles []InputProfile, runtime string) (workflowInventory, error) {
	var w workflowInventory
	doc, canonical, err := readWorkflow(raw)
	if err != nil {
		return w, err
	}
	if !workflowID.MatchString(execution) {
		return w, errWorkflowForm
	}
	_, rootCase, err := workflowCase(root)
	if err != nil {
		return w, err
	}
	stages, err := freezeWorkflowStages(raw, inputs, profiles, runtime)
	if err != nil {
		return w, err
	}
	w = workflowInventory{Document: doc, Artifacts: map[string][]byte{}, Execution: workflowExecution{ExecutionVersion: 1, WorkflowDigest: digest(canonical), Execution: execution, RootCaseDigest: digest(rootCase), Frozen: []workflowFrozenStage{}, Instances: []workflowInstance{}, OutputSets: []workflowStageSet{}, Selections: make([]*string, len(stages))}}
	w.keep("workflow", canonical)
	w.keep("case", rootCase)
	for i, stage := range stages {
		w.Execution.Frozen = append(w.Execution.Frozen, stage.Frozen)
		w.Artifacts["published/"+stage.Frozen.PublishedDialectDigest] = bytes.Clone(stage.Published)
		if stage.Frozen.BindingDocumentDigest != nil {
			w.Artifacts["binding/"+*stage.Frozen.BindingDocumentDigest] = bytes.Clone(stage.Binding)
		}
		w.Artifacts["bound/"+stage.Frozen.BoundDialectDigest] = bytes.Clone(stage.Bound)
		w.keep("pack", stage.Pack)
		// The release pin uses typed canonical encoding; its recording keeps
		// the original reference writer bytes, not the digest preimage.
		w.Artifacts["release/"+stage.Frozen.ReleaseDigest] = bytes.Clone(stage.Release)
		status := "blocked"
		if i == 0 {
			status = "closed"
		}
		w.Execution.OutputSets = append(w.Execution.OutputSets, workflowStageSet{stage.Frozen.Stage, status, []string{}})
	}
	// A reserved instance is explicitly interrupted until its attempt is
	// recorded. Recovery never silently invokes it. The host displays queued
	// or running activity separately and does not publish an in-flight set.
	w.Execution.Instances = append(w.Execution.Instances, workflowInstance{ID: "instance-1", Stage: doc.Stages[0].ID, InputDigest: digest(rootCase), CaseDigest: workflowPin(digest(rootCase)), State: "interrupted", Attempts: []workflowAttempt{}})
	if !w.withinBounds() {
		return workflowInventory{}, errWorkflowBound
	}
	return w, nil
}
func canonicalRelease(raw []byte) ([]byte, error) {
	var release Release
	if err := strictJSON(raw, &release); err != nil {
		return nil, err
	}
	return canonical(encode(release))
}
func (w workflowInventory) stageIndex(id string) int {
	for i, stage := range w.Document.Stages {
		if stage.ID == id {
			return i
		}
	}
	return -1
}
func (w workflowInventory) outputSet(stage int) workflowSet {
	return workflowSet{WorkflowDigest: w.Execution.WorkflowDigest, Execution: w.Execution.Execution, Stage: w.Document.Stages[stage].ID, Records: w.Execution.OutputSets[stage].Records}
}
func (w workflowInventory) record(pin string) (workflowOutputRecord, []byte, error) {
	var record workflowOutputRecord
	raw := w.Artifacts["output/"+pin]
	if len(raw) == 0 || digest(raw) != pin || strictJSON(raw, &record) != nil {
		return record, nil, errWorkflowTransition
	}
	payload := w.Artifacts["payload/"+record.PayloadDigest]
	if len(payload) == 0 || digest(payload) != record.PayloadDigest {
		return record, nil, errWorkflowTransition
	}
	return record, payload, nil
}

// selectWorkflow reserves every selected child of every direct consumer at
// once. This transition must be committed atomically before any child acquires.
func selectWorkflow(w workflowInventory, raw []byte, dispositions map[string]json.RawMessage) (workflowInventory, error) {
	var envelope struct {
		UpstreamStage string `json:"upstreamStage"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return w, errWorkflowForm
	}
	index := w.stageIndex(envelope.UpstreamStage)
	if index < 0 || w.Execution.OutputSets[index].Status != "closed" {
		return w, errWorkflowTransition
	}
	stage := w.Document.Stages[index]
	set := w.outputSet(index)
	selection, canonical, err := readWorkflowSelection(raw, set, stage, w.Document.Limits.MaxRecords, dispositions)
	if err != nil {
		return w, err
	}
	if selection.Refusal != "" {
		return w, errWorkflowTransition
	} // the writer determines bounds
	expectedRevision := 1
	if previous := w.Execution.Selections[index]; previous != nil {
		var old workflowSelection
		if strictJSON(w.Artifacts["selection/"+*previous], &old) != nil {
			return w, errWorkflowTransition
		}
		if old.Refusal == "" {
			if digest(canonical) == *previous {
				return w, nil
			} // exact retry is idempotent
			return w, errWorkflowConsumed
		}
		expectedRevision = old.Revision + 1
	}
	if selection.Revision != expectedRevision {
		return w, errWorkflowTransition
	}
	children := []int{}
	for i, child := range w.Document.Stages {
		if child.Input.FromStage == stage.ID {
			children = append(children, i)
		}
	}
	if len(children) == 0 {
		return w, errWorkflowTransition
	}
	next := w.copy()
	if len(w.Execution.Instances)+len(children)*len(selection.Selected) > w.Document.Limits.MaxInstances {
		return refuseWorkflowSelection(w, selection, index)
	}
	pin := digest(canonical)
	next.Execution.Selections[index] = workflowPin(next.keep("selection", canonical))
	for _, childIndex := range children {
		child := w.Document.Stages[childIndex]
		next.Execution.OutputSets[childIndex].Status = "closed"
		for _, parent := range selection.Selected {
			record, payload, err := w.record(parent)
			if err != nil {
				return w, err
			}
			input, err := workflowCanonical(map[string]any{"parentRecord": parent, "selectionDigest": pin, "caseBindings": child.CaseBindings}, 262144)
			if err != nil {
				return w, err
			}
			instance := workflowInstance{ID: fmt.Sprintf("instance-%d", len(next.Execution.Instances)+1), Stage: child.ID, ParentRecord: workflowPin(parent), SelectionDigest: workflowPin(pin), InputDigest: digest(input), State: "interrupted", Attempts: []workflowAttempt{}}
			projected, err := projectWorkflowCase(payload, child.CaseBindings)
			if errors.Is(err, errWorkflowBound) {
				return refuseWorkflowSelection(w, selection, index)
			}
			if err != nil {
				instance.State, instance.Failure = "failed", workflowPin("projection-invalid")
			} else {
				instance.CaseDigest = workflowPin(next.keep("case", projected))
				var mapping InputMapping
				if err := strictJSON(w.Artifacts["bound/"+w.Execution.Frozen[childIndex].BoundDialectDigest], &mapping); err != nil {
					return w, err
				}
				if _, err := inheritedWorkflowInfluence(mapping, child.CaseBindings, payload, record.Lineage.Influence); err != nil {
					instance.State, instance.Failure = "failed", workflowPin("admission-refused")
				}
			}
			next.Execution.Instances = append(next.Execution.Instances, instance)
		}
	}
	// Stage order is independent of which parent selection was reviewed first.
	ordered := make([]workflowInstance, 0, len(next.Execution.Instances))
	for _, stage := range next.Document.Stages {
		for _, instance := range next.Execution.Instances {
			if instance.Stage == stage.ID {
				ordered = append(ordered, instance)
			}
		}
	}
	next.Execution.Instances = ordered
	if err := next.checkBounds(); err != nil {
		if !errors.Is(err, errWorkflowBound) {
			return w, err
		}
		return refuseWorkflowSelection(w, selection, index)
	}
	return next, nil
}

// A bound refusal is recorded from the original state, never a partially
// projected/reserved candidate. Earlier refused selections remain in custody,
// outside this branch's current recording.
func refuseWorkflowSelection(w workflowInventory, selection workflowSelection, index int) (workflowInventory, error) {
	refused := w.copy()
	selection.Refusal = "limit-exceeded"
	raw, err := workflowCanonical(selection, 65536)
	if err != nil {
		return w, err
	}
	refused.Execution.Selections[index] = workflowPin(refused.keep("selection", raw))
	if err := refused.checkBounds(); err != nil {
		return w, err
	}
	return refused, nil
}

// workflowOutputRecords writes complete output envelopes from a successful,
// independently checked preparation and audit. Admission/state/bound accounting
// is the committing transition's responsibility, never payload truncation here.
func workflowOutputRecords(w workflowInventory, instance workflowInstance, preparation *Preparation, audit []byte, facts []byte, inherited map[workflowTarget]workflowInfluence) ([]workflowOutputRecord, [][]byte, error) {
	index := w.stageIndex(instance.Stage)
	if index < 0 || instance.CaseDigest == nil || preparation == nil || len(audit) == 0 {
		return nil, nil, errWorkflowTransition
	}
	stage, frozen := w.Document.Stages[index], w.Execution.Frozen[index]
	payloads, err := outputsWorkflow(facts, stage.Outputs)
	if err != nil {
		return nil, nil, err
	}
	records := make([]workflowOutputRecord, 0, len(payloads))
	for i, payload := range payloads {
		pointer := stage.Outputs.Pointer
		if stage.Outputs.Cardinality == "many" {
			pointer += "/" + strconv.Itoa(i)
		}
		influence, err := outputWorkflowInfluence(pointer, preparation.Lineage, inherited)
		if err != nil {
			return nil, nil, err
		}
		lineage := workflowOutputLineage{w.Execution.WorkflowDigest, w.Execution.Execution, stage.ID, instance.ID, frozen.PublishedDialectDigest, frozen.BindingDocumentDigest, frozen.BoundDialectDigest, frozen.PackDigest, frozen.ReleaseDigest, preparationDigest(preparation), digest(audit), stage.Outputs.Pointer, i, w.Execution.RootCaseDigest, *instance.CaseDigest, instance.ParentRecord, instance.SelectionDigest, stage.Relation, influence}
		records = append(records, workflowOutputRecord{1, digest(payload), lineage})
	}
	return records, payloads, nil
}
