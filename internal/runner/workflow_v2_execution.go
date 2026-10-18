package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

// All history indexes are writer state. Portable membership comes from the
// retained proof, not an arbitrary file list or the SQLite artifact inventory.
type workflowV2Inventory struct {
	Trace          func(workflowV2Comparison) `json:"-"`
	Core           workflowInventory
	Proof          workflowV2Proof
	History        map[string]workflowInstance
	Outputs        []string
	SelectionPins  []string
	Reservations   map[string]int
	ProspectiveIDs []string
	Cutoffs        map[string]bool
}

func (w workflowV2Inventory) copy() workflowV2Inventory {
	out := w
	out.Core = w.Core.copy()
	out.Proof = workflowV2Proof{}
	out.History = nil
	out.Reservations = nil
	out.Cutoffs = nil
	_ = json.Unmarshal(encode(w.Proof), &out.Proof)
	_ = json.Unmarshal(encode(w.History), &out.History)
	_ = json.Unmarshal(encode(w.Reservations), &out.Reservations)
	_ = json.Unmarshal(encode(w.Cutoffs), &out.Cutoffs)
	out.Outputs = append([]string{}, w.Outputs...)
	out.SelectionPins = append([]string{}, w.SelectionPins...)
	out.ProspectiveIDs = append([]string{}, w.ProspectiveIDs...)
	return out
}
func (w *workflowV2Inventory) update(instance workflowInstance) {
	var owned workflowInstance
	_ = json.Unmarshal(encode(instance), &owned)
	instance = owned
	w.History[instance.ID] = instance
	for i := range w.Core.Execution.Instances {
		if w.Core.Execution.Instances[i].ID == instance.ID {
			w.Core.Execution.Instances[i] = instance
			return
		}
	}
	w.Core.Execution.Instances = append(w.Core.Execution.Instances, instance)
}
func (w workflowV2Inventory) instance(id string) (workflowInstance, error) {
	for _, i := range w.Core.Execution.Instances {
		if i.ID == id {
			var owned workflowInstance
			_ = json.Unmarshal(encode(i), &owned)
			return owned, nil
		}
	}
	return workflowInstance{}, errWorkflowTransition
}
func (w *workflowV2Inventory) order() {
	sort.SliceStable(w.Core.Execution.Instances, func(i, j int) bool {
		return w.Core.stageIndex(w.Core.Execution.Instances[i].Stage) < w.Core.stageIndex(w.Core.Execution.Instances[j].Stage)
	})
	rank := map[string]int{}
	for i, instance := range w.Core.Execution.Instances {
		rank[instance.ID] = i
	}
	for stage := range w.Core.Execution.OutputSets {
		pins := []string{}
		for _, pin := range w.Outputs {
			r, _, err := w.Core.record(pin)
			if err == nil && r.Lineage.Stage == w.Core.Document.Stages[stage].ID {
				if _, active := rank[r.Lineage.Instance]; active {
					pins = append(pins, pin)
				}
			}
		}
		sort.Slice(pins, func(i, j int) bool {
			a, _, _ := w.Core.record(pins[i])
			b, _, _ := w.Core.record(pins[j])
			if rank[a.Lineage.Instance] != rank[b.Lineage.Instance] {
				return rank[a.Lineage.Instance] < rank[b.Lineage.Instance]
			}
			return a.Lineage.Index < b.Lineage.Index
		})
		w.Core.Execution.OutputSets[stage].Records = pins
	}
}
func (w workflowV2Inventory) checkBounds() error {
	if len(w.History) > w.Core.Document.Limits.MaxInstances || len(w.Outputs) > w.Core.Document.Limits.MaxRecords {
		return errWorkflowBound
	}
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
	if total > w.Core.Document.Limits.MaxBytes {
		return errWorkflowBound
	}
	return nil
}
func workflowV2RequireRecording(w workflowV2Inventory) error {
	err := w.checkBounds()
	if errors.Is(err, errWorkflowBound) {
		return errWorkflowV2Recording
	}
	return err
}
func newWorkflowV2Inventory(raw, root []byte, execution, rootInstance string, inputs []workflowFreezeInput, profiles []InputProfile, runtime string, traces ...func(workflowV2Comparison)) (workflowV2Inventory, error) {
	var w workflowV2Inventory
	doc, canonical, err := readWorkflowVersion(raw, 2)
	if err != nil {
		return w, err
	}
	if !workflowID.MatchString(execution) || !workflowID.MatchString(rootInstance) {
		return w, errWorkflowForm
	}
	_, rootCase, err := workflowCase(root)
	if err != nil {
		return w, err
	}
	stages, err := freezeWorkflowDocumentStages(doc, inputs, profiles, runtime)
	if err != nil {
		return w, err
	}
	w = workflowV2Inventory{Core: workflowInventory{Document: doc, Artifacts: map[string][]byte{}, Pending: map[string]int{}, Execution: workflowExecution{ExecutionVersion: 2, WorkflowDigest: digest(canonical), Execution: execution, RootCaseDigest: digest(rootCase), Frozen: []workflowFrozenStage{}, Instances: []workflowInstance{}, OutputSets: []workflowStageSet{}, Selections: make([]*string, len(stages))}}, History: map[string]workflowInstance{}, Outputs: []string{}, SelectionPins: []string{}, Reservations: map[string]int{}, ProspectiveIDs: []string{rootInstance}, Cutoffs: map[string]bool{}}
	w.Core.keep("workflow", canonical)
	w.Core.keep("case", rootCase)
	for i, stage := range stages {
		w.Core.Execution.Frozen = append(w.Core.Execution.Frozen, stage.Frozen)
		w.Core.Artifacts["published/"+stage.Frozen.PublishedDialectDigest] = bytes.Clone(stage.Published)
		if stage.Frozen.BindingDocumentDigest != nil {
			w.Core.Artifacts["binding/"+*stage.Frozen.BindingDocumentDigest] = bytes.Clone(stage.Binding)
		}
		w.Core.Artifacts["bound/"+stage.Frozen.BoundDialectDigest] = bytes.Clone(stage.Bound)
		w.Core.keep("pack", stage.Pack)
		w.Core.Artifacts["release/"+stage.Frozen.ReleaseDigest] = bytes.Clone(stage.Release)
		status := "blocked"
		if i == 0 {
			status = "closed"
		}
		w.Core.Execution.OutputSets = append(w.Core.Execution.OutputSets, workflowStageSet{stage.Frozen.Stage, status, []string{}})
	}
	frozen, err := workflowCanonical(w.Core.Execution.Frozen, 8<<20)
	if err != nil {
		return w, err
	}
	w.Proof = workflowV2Proof{1, w.Core.Execution.WorkflowDigest, execution, digest(rootCase), digest(frozen), rootInstance, [][]json.RawMessage{}}
	if err = w.Proof.append("root", nil); err != nil {
		return workflowV2Inventory{}, err
	}
	w.update(workflowInstance{ID: rootInstance, Stage: doc.Stages[0].ID, InputDigest: digest(rootCase), CaseDigest: workflowPin(digest(rootCase)), State: "interrupted", Attempts: []workflowAttempt{}})
	w.Reservations[rootInstance] = 1
	if len(traces) > 1 {
		return workflowV2Inventory{}, errWorkflowForm
	}
	if len(traces) == 1 {
		w.Trace = traces[0]
	}
	err = workflowV2RequireRecording(w)
	w.traceRequired("root", err)
	if err != nil {
		return workflowV2Inventory{}, err
	}
	return w, nil
}
func selectWorkflowV2(w workflowV2Inventory, raw []byte, childIDs []string, fork bool) (workflowV2Inventory, error) {
	if !w.Proof.closed() {
		return w, errWorkflowTransition
	}
	var envelope struct {
		UpstreamStage string `json:"upstreamStage"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return w, errWorkflowForm
	}
	stage := w.Core.stageIndex(envelope.UpstreamStage)
	if stage < 0 || w.Core.Execution.OutputSets[stage].Status != "closed" {
		return w, errWorkflowTransition
	}
	offered := w.Core.outputSet(stage)
	dispositions := map[string]json.RawMessage{}
	for _, pin := range offered.Records {
		record, _, err := w.Core.record(pin)
		if err != nil {
			return w, err
		}
		dispositions[pin] = member(w.Core.Artifacts["audit/"+record.Lineage.AuditDigest], "disposition")
	}
	selection, core, err := readWorkflowSelection(raw, offered, w.Core.Document.Stages[stage], w.Core.Document.Limits.MaxRecords, dispositions)
	if err != nil {
		return w, err
	}
	if selection.Refusal != "" {
		return w, errWorkflowTransition
	}
	consumed := w.Core.consumed(stage)
	if consumed != fork {
		return w, errWorkflowConsumed
	}
	revision := 1
	for _, pin := range w.SelectionPins {
		var old workflowSelection
		if strictJSON(w.Core.Artifacts["selection/"+pin], &old) != nil {
			return w, errWorkflowTransition
		}
		if old.UpstreamStage == envelope.UpstreamStage && old.Revision >= revision {
			revision = old.Revision + 1
		}
	}
	if revision > 16 || selection.Revision != revision {
		return w, errWorkflowTransition
	}
	consumers := []int{}
	for i, s := range w.Core.Document.Stages {
		if s.Input.FromStage == envelope.UpstreamStage {
			consumers = append(consumers, i)
		}
	}
	if len(consumers) == 0 {
		return w, errWorkflowTransition
	}
	count := len(consumers) * len(selection.Selected)
	countRefused := len(w.History)+count > w.Core.Document.Limits.MaxInstances
	if countRefused {
		if childIDs != nil {
			return w, errWorkflowForm
		}
	} else {
		if childIDs == nil || len(childIDs) != count {
			return w, errWorkflowForm
		}
		seen := map[string]bool{}
		for _, id := range w.ProspectiveIDs {
			seen[id] = true
		}
		for _, id := range childIDs {
			if !workflowID.MatchString(id) || seen[id] {
				return w, errWorkflowForm
			}
			seen[id] = true
		}
	}
	common := w.copy()
	kind := "selection"
	if fork {
		kind = "fork"
	}
	if err = common.Proof.append(kind, map[string]any{"upstreamStage": envelope.UpstreamStage, "revision": revision, "selectionCoreDigest": digest(core), "childIds": childIDs}); err != nil {
		return w, err
	}
	common.ProspectiveIDs = append(common.ProspectiveIDs, childIDs...)
	if fork {
		retired := map[string]bool{}
		for _, s := range common.Core.Document.Stages {
			if s.Input.FromStage == envelope.UpstreamStage || retired[s.Input.FromStage] {
				retired[s.ID] = true
			}
		}
		active := []workflowInstance{}
		for _, i := range common.Core.Execution.Instances {
			if !retired[i.Stage] {
				active = append(active, i)
			}
		}
		common.Core.Execution.Instances = active
		for i, s := range common.Core.Document.Stages {
			if retired[s.ID] {
				common.Core.Execution.Selections[i] = nil
				common.Core.Execution.OutputSets[i] = workflowStageSet{s.ID, "blocked", []string{}}
			}
		}
	}
	accepted, refused := common.copy(), common.copy()
	pin := digest(core)
	accepted.Core.Execution.Selections[stage] = workflowPin(accepted.Core.keep("selection", core))
	accepted.SelectionPins = append(accepted.SelectionPins, pin)
	epoch, _ := common.Proof.last()
	offset := 0
	localBound := false
	if !countRefused {
		for _, childIndex := range consumers {
			child := w.Core.Document.Stages[childIndex]
			accepted.Core.Execution.OutputSets[childIndex].Status = "closed"
			for _, parent := range selection.Selected {
				id := childIDs[offset]
				offset++
				record, payload, err := w.Core.record(parent)
				if err != nil {
					return w, err
				}
				input, err := workflowCanonical(map[string]any{"parentRecord": parent, "selectionDigest": pin, "caseBindings": child.CaseBindings}, 262144)
				if err != nil {
					return w, err
				}
				instance := workflowInstance{ID: id, Stage: child.ID, ParentRecord: workflowPin(parent), SelectionDigest: workflowPin(pin), InputDigest: digest(input), State: "interrupted", Attempts: []workflowAttempt{}}
				projected, err := projectWorkflowCase(payload, child.CaseBindings)
				if errors.Is(err, errWorkflowBound) {
					localBound = true
					break
				}
				if err != nil {
					instance.State = "failed"
					instance.Failure = workflowPin("projection-invalid")
				} else {
					instance.CaseDigest = workflowPin(accepted.Core.keep("case", projected))
					var mapping InputMapping
					if strictJSON(w.Core.Artifacts["bound/"+w.Core.Execution.Frozen[childIndex].BoundDialectDigest], &mapping) != nil {
						return w, errWorkflowTransition
					}
					if _, err = inheritedWorkflowInfluence(mapping, child.CaseBindings, payload, record.Lineage.Influence); err != nil {
						instance.State = "failed"
						instance.Failure = workflowPin("admission-refused")
					}
				}
				accepted.update(instance)
				accepted.Reservations[id] = epoch.Number
			}
			if localBound {
				break
			}
		}
	}
	accepted.order()
	err = accepted.checkBounds()
	admitted := !countRefused && !localBound && err == nil
	if admitted && common.Trace == nil {
		return accepted, nil
	}
	if err != nil && !errors.Is(err, errWorkflowBound) {
		return w, err
	}
	selection.Refusal = "limit-exceeded"
	refusalBytes, err := workflowCanonical(selection, 65536)
	if err != nil {
		if admitted {
			common.traceDecision(kind, "", "fits", true, true, &accepted, nil)
			return accepted, nil
		}
		return w, err
	}
	refused.Core.Execution.Selections[stage] = workflowPin(refused.Core.keep("selection", refusalBytes))
	refused.SelectionPins = append(refused.SelectionPins, digest(refusalBytes))
	for _, i := range consumers {
		refused.Core.Execution.OutputSets[i] = workflowStageSet{w.Core.Document.Stages[i].ID, "blocked", []string{}}
	}
	cause := "aggregate-bound"
	var candidate *workflowV2Inventory = &accepted
	if countRefused {
		cause, candidate = "instance-count", nil
	} else if localBound {
		cause, candidate = "local-case-bound", nil
	} else if admitted {
		cause = "fits"
	}
	err = workflowV2RequireRecording(refused)
	common.traceDecision(kind, "", cause, admitted, admitted || err == nil, candidate, &refused)
	if admitted {
		return accepted, nil
	}
	if err != nil {
		return w, err
	}
	return refused, nil
}
func beginWorkflowV2Group(w workflowV2Inventory, reservation int, starts []workflowV2Begin, deferred []string, cancelled []workflowV2Cancel) (workflowV2Inventory, error) {
	if !w.Proof.closed() || len(starts)+len(cancelled) == 0 || starts == nil || deferred == nil || cancelled == nil {
		return w, errWorkflowTransition
	}
	eligible := []workflowInstance{}
	for _, i := range w.Core.Execution.Instances {
		if w.Reservations[i.ID] == reservation && i.State == "interrupted" && len(i.Attempts) == 0 {
			eligible = append(eligible, i)
		}
	}
	seen := map[string]bool{}
	nextStart, nextDefer, nextCancel := 0, 0, 0
	next := w.copy()
	for _, i := range eligible {
		switch {
		case nextStart < len(starts) && starts[nextStart].Instance == i.ID:
			b := starts[nextStart]
			nextStart++
			if b.Number != 1 || b.Retry != nil || w.Core.consumed(w.Core.stageIndex(i.Stage)) {
				return w, errWorkflowConsumed
			}
			i.Attempts = append(i.Attempts, workflowAttempt{Number: 1, State: "interrupted"})
			next.Core.Pending[i.ID] = 1
			next.update(i)
		case nextDefer < len(deferred) && deferred[nextDefer] == i.ID:
			nextDefer++
		case nextCancel < len(cancelled) && cancelled[nextCancel].Instance == i.ID:
			c := cancelled[nextCancel]
			nextCancel++
			if !workflowV2Assertion(c.Actor, c.At) {
				return w, errWorkflowForm
			}
			if w.Core.consumed(w.Core.stageIndex(i.Stage)) {
				return w, errWorkflowConsumed
			}
			i.State = "cancelled"
			next.update(i)
		default:
			return w, errWorkflowTransition
		}
		seen[i.ID] = true
	}
	if len(seen) == 0 || nextStart != len(starts) || nextDefer != len(deferred) || nextCancel != len(cancelled) {
		return w, errWorkflowTransition
	}
	if err := next.Proof.append("group", map[string]any{"reservation": reservation, "starts": starts, "deferred": deferred, "cancelled": cancelled, "results": []workflowV2Observation{}}); err != nil {
		return w, err
	}
	next.Cutoffs = map[string]bool{}
	err := workflowV2RequireRecording(next)
	next.traceRequired("group", err)
	if err != nil {
		return w, err
	}
	return next, nil
}
func beginWorkflowV2Retry(w workflowV2Inventory, b workflowV2Begin) (workflowV2Inventory, error) {
	if !w.Proof.closed() {
		return w, errWorkflowTransition
	}
	i, err := w.instance(b.Instance)
	if err != nil {
		return w, err
	}
	if (i.State != "failed" && i.State != "interrupted") || len(i.Attempts) == 0 || b.Number != len(i.Attempts)+1 || b.Number > w.Core.Document.Limits.MaxAttempts || b.Retry == nil || !workflowV2Assertion(b.Retry.Actor, b.Retry.At) {
		return w, errWorkflowTransition
	}
	if w.Core.consumed(w.Core.stageIndex(i.Stage)) {
		return w, errWorkflowConsumed
	}
	next := w.copy()
	held := *b.Retry
	i.Attempts = append(i.Attempts, workflowAttempt{Number: b.Number, State: "interrupted", Retry: &held})
	i.State = "interrupted"
	i.Failure = nil
	next.update(i)
	next.Core.Pending[i.ID] = b.Number
	if err = next.Proof.append("retry", map[string]any{"start": b, "results": []workflowV2Observation{}}); err != nil {
		return w, err
	}
	next.Cutoffs = map[string]bool{}
	err = workflowV2RequireRecording(next)
	next.traceRequired("retry", err)
	if err != nil {
		return w, err
	}
	return next, nil
}
