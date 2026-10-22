package runner

import (
	"bytes"
	"encoding/json"
	"errors"
)

func (w workflowV2Inventory) recordingFiles() (map[string][]byte, error) {
	document, err := workflowCanonical(w.Core.Document, 262144)
	if err != nil || w.Core.Document.WorkflowVersion != 2 || w.Core.Execution.ExecutionVersion != 2 ||
		digest(document) != w.Core.Execution.WorkflowDigest || w.Proof.WorkflowDigest != w.Core.Execution.WorkflowDigest ||
		w.Proof.Execution != w.Core.Execution.Execution || w.Proof.RootCaseDigest != w.Core.Execution.RootCaseDigest {
		return nil, errWorkflowTransition
	}
	frozen, err := workflowCanonical(w.Core.Execution.Frozen, 8<<20)
	if err != nil || w.Proof.FrozenDigest != digest(frozen) {
		return nil, errWorkflowTransition
	}
	ids := sortedKeys(w.History)
	instances := make([]workflowInstance, 0, len(ids))
	for _, id := range ids {
		instances = append(instances, w.History[id])
	}
	files, err := w.Core.recordingFilesHistory(instances, w.Outputs, w.SelectionPins)
	if err != nil {
		return nil, err
	}
	proof, err := w.Proof.bytes()
	if err != nil {
		return nil, err
	}
	files["admission-proof.json"] = proof
	files["admission-proof.sha256"] = []byte(digest(proof) + "\n")
	var execution map[string]json.RawMessage
	if json.Unmarshal(files["execution.json"], &execution) != nil {
		return nil, errWorkflowTransition
	}
	execution["admissionProofDigest"] = encode(digest(proof))
	control, err := workflowCanonical(execution, 8<<20)
	if err != nil {
		return nil, err
	}
	files["execution.json"] = control
	files["execution.sha256"] = []byte(digest(control) + "\n")
	for path, raw := range files {
		files[path] = bytes.Clone(raw)
	}
	return files, nil
}

// Optional auxiliaries affect only final supplied input bounds, never an earlier refusal.
func (w workflowV2Inventory) finalize(aux map[string][]byte) (map[string][]byte, error) {
	files, err := w.recordingFiles()
	if err != nil {
		return nil, err
	}
	for path, raw := range aux {
		if !workflowV2Auxiliary(path) || files[path] != nil {
			return nil, errWorkflowForm
		}
		files[path] = bytes.Clone(raw)
	}
	total := 0
	for _, raw := range files {
		total += len(raw)
	}
	if len(files) > 65536 || total > w.Core.Document.Limits.MaxBytes {
		return nil, errWorkflowBound
	}
	return files, nil
}
func workflowV2Auxiliary(path string) bool {
	if path == "README.md" || path == "trust.json" || path == "expected-report.json" {
		return true
	}
	if len(path) < 5 || path[:4] != "aux/" {
		return false
	}
	for _, part := range bytes.Split([]byte(path), []byte("/")) {
		if len(part) == 0 || bytes.Equal(part, []byte(".")) || bytes.Equal(part, []byte("..")) || bytes.ContainsAny(part, "\\\x00") {
			return false
		}
	}
	return true
}

type workflowV2Result struct {
	Instance string
	Number   int
	workflowAttemptResult
}
type workflowV2Material struct {
	observation workflowV2Observation
	instance    workflowInstance
	prepared    Input
	bundle      VerificationBundle
	artifacts   map[string][]byte
}

// Validate actual Runner exports through the existing preparation, binding,
// receipt and artifact checks. This does not replay the Runtime disposition.
func checkWorkflowV2Material(w workflowV2Inventory, result workflowV2Result, profiles []InputProfile) (workflowV2Material, error) {
	m := workflowV2Material{artifacts: map[string][]byte{}}
	i, err := w.instance(result.Instance)
	if err != nil || len(i.Attempts) == 0 || result.Number != len(i.Attempts) {
		return m, errWorkflowTransition
	}
	a := i.Attempts[len(i.Attempts)-1]
	if w.Core.Pending[i.ID] != a.Number {
		return m, errWorkflowTransition
	}
	stage := w.Core.stageIndex(i.Stage)
	frozen := w.Core.Execution.Frozen[stage]
	o := workflowV2Observation{Instance: i.ID, Number: a.Number, State: result.State}
	if result.State == "suppressed" {
		if !w.Cutoffs[i.Stage] || len(result.Run)+len(result.Signatures)+len(result.EvaluationError) > 0 || result.Failure != "" {
			return m, errWorkflowTransition
		}
		i.State = "failed"
		i.Failure = workflowPin("limit-exceeded")
		a.State = i.State
		a.Failure = i.Failure
		i.Attempts[len(i.Attempts)-1] = a
		m.instance = i
		m.observation = o
		return m, nil
	}
	if len(result.Run) > 0 {
		raw := result.Run
		if len(raw) > MaxExportSize {
			return m, errWorkflowBound
		}
		value, err := readInputJSONDepth(raw, 64)
		if err != nil {
			return m, errWorkflowForm
		}
		object, ok := value.(map[string]any)
		if !ok {
			return m, errWorkflowForm
		}
		if version, exists := object["version"]; exists && version != json.Number("2") && version != json.Number("3") {
			return m, errWorkflowVersion
		}
		b := &m.bundle
		if strictJSON(raw, b) != nil || !sameJSON(raw, encode(b)) || exportWithinLimits(raw, *b) != nil || (b.Version != 2 && b.Version != 3) || b.Inputs != "" || b.Chain != nil || b.ReleaseDigest != frozen.ReleaseDigest || releaseDigest(b.Release) != frozen.ReleaseDigest || b.Run.ReleaseID != b.Release.ID || b.Run.Attempt != a.Number || b.Run.Input.Source == nil || !mappingMatchesRelease(b.Release, b.Run.Input) || !workflowCheckpointIdentity(w.Core, i.ID, *b) {
			return m, errWorkflowTransition
		}
		if checkAuditBytes(*b) != nil || checkChainEntry(*b) != nil || checkAuditSignatures(*b) != nil {
			return m, errWorkflowTransition
		}
		_, caseBytes, err := workflowCase(b.Run.Input.Source.Case)
		if err != nil || i.CaseDigest == nil || digest(caseBytes) != *i.CaseDigest {
			return m, errWorkflowTransition
		}
		if b.Run.Input.Preparation == nil && (b.Run.State == "completed" || len(b.Run.Audit) > 0 || len(b.Run.Result) > 0) {
			return m, errWorkflowTransition
		}
		o.RunDigest = workflowPin(digest(raw))
		m.artifacts["run/"+*o.RunDigest] = bytes.Clone(raw)
		if b.Run.Input.Preparation != nil {
			checked, err := VerifyInputs(raw, profiles, frozen.ReleaseDigest)
			if err != nil {
				return m, err
			}
			if len(b.Run.Audit) > 0 && (checked.Binding == nil || checked.Binding.Status != BindingMatches) {
				return m, errWorkflowTransition
			}
			m.prepared = checked.prepared
			preparation, err := canonical(encode(m.prepared.Preparation))
			if err != nil {
				return m, err
			}
			o.PreparationDigest = workflowPin(digest(preparation))
			m.artifacts["preparation/"+*o.PreparationDigest] = preparation
		}
		if audit := workflowRunAudit(*b); len(audit) > 0 {
			o.AuditDigest = workflowPin(digest(audit))
			m.artifacts["audit/"+*o.AuditDigest] = bytes.Clone(audit)
		}
	}
	b := m.bundle
	switch result.State {
	case "completed":
		if b.Run.State != "completed" || len(b.Run.Result) == 0 || o.PreparationDigest == nil || o.AuditDigest == nil || len(result.EvaluationError) > 0 || result.Failure != "" {
			return m, errWorkflowTransition
		}
		o.State = "evaluated"
	case "interrupted", "cancelled":
		if result.Failure != "" || len(result.EvaluationError) > 0 || len(b.Run.Audit) > 0 || len(b.Run.Result) > 0 || len(result.Run) > 0 && b.Run.State != result.State {
			return m, errWorkflowTransition
		}
	case "failed":
		if len(result.Run) > 0 && b.Run.State != "failed" {
			return m, errWorkflowTransition
		}
		switch result.Failure {
		case "evaluation-error":
			if !workflowEvaluationError(result.EvaluationError) || o.PreparationDigest == nil || o.AuditDigest != nil || len(b.Run.Result) > 0 {
				return m, errWorkflowTransition
			}
			o.EvaluationErrorDigest = workflowPin(digest(result.EvaluationError))
			m.artifacts["evaluation-error/"+*o.EvaluationErrorDigest] = bytes.Clone(result.EvaluationError)
		case "preparation-failed", "admission-refused":
			if len(result.Run) == 0 || len(result.EvaluationError) > 0 || o.PreparationDigest != nil || o.AuditDigest != nil || len(b.Run.Result) > 0 || workflowV2PreparationFailure(b.Run, profiles) != result.Failure {
				return m, errWorkflowTransition
			}
		case "acquisition-failed":
			if len(result.EvaluationError) > 0 || o.PreparationDigest != nil || o.AuditDigest != nil || len(b.Run.Result) > 0 {
				return m, errWorkflowTransition
			}
		default:
			return m, errWorkflowTransition
		}
		o.Failure = workflowPin(result.Failure)
	default:
		return m, errWorkflowTransition
	}
	signatures := result.Signatures
	if len(b.Run.AuditSignatures) > 0 {
		if len(signatures) > 0 && !bytes.Equal(signatures, b.Run.AuditSignatures) {
			return m, errWorkflowTransition
		}
		signatures = b.Run.AuditSignatures
	}
	if len(signatures) > 0 {
		if o.AuditDigest == nil || len(signatures) > maxSidecar {
			return m, errWorkflowTransition
		}
		// The inherited workflow form retains opaque bounded JSONL; it does
		// not authenticate signatures or discard unreadable sidecar lines.
		o.AuditSignaturesDigest = workflowPin(digest(signatures))
		m.artifacts["audit-signatures/"+*o.AuditSignaturesDigest] = bytes.Clone(signatures)
		m.artifacts[workflowSignatureKey(i.ID, a.Number, *o.RunDigest)] = bytes.Clone(signatures)
	}
	a.State = result.State
	a.Failure = o.Failure
	a.RunDigest = o.RunDigest
	a.PreparationDigest = o.PreparationDigest
	a.AuditDigest = o.AuditDigest
	a.EvaluationErrorDigest = o.EvaluationErrorDigest
	i.State = a.State
	i.Failure = a.Failure
	i.Attempts[len(i.Attempts)-1] = a
	m.instance = i
	m.observation = o
	return m, nil
}
func observeWorkflowV2(w workflowV2Inventory, result workflowV2Result, profiles []InputProfile) (workflowV2Inventory, error) {
	e, err := w.Proof.last()
	if err != nil || w.Proof.closed() {
		return w, errWorkflowTransition
	}
	b := e.begins()[len(e.Results)]
	if result.Instance != b.Instance || result.Number != b.Number {
		return w, errWorkflowTransition
	}
	m, err := checkWorkflowV2Material(w, result, profiles)
	if err != nil {
		return w, err
	}
	common := w.copy()
	for key, raw := range m.artifacts {
		if old, ok := common.Core.Artifacts[key]; ok && !bytes.Equal(old, raw) {
			return w, errWorkflowTransition
		}
		common.Core.Artifacts[key] = raw
	}
	if err = common.Proof.observe(m.observation); err != nil {
		return w, err
	}
	common.update(m.instance)
	delete(common.Core.Pending, result.Instance)
	if m.observation.State != "evaluated" {
		err = workflowV2RequireRecording(common)
		common.traceRequired("result-phase", err)
		if err != nil {
			return w, err
		}
		return common, nil
	}
	stage := w.Core.stageIndex(m.instance.Stage)
	inherited := map[workflowTarget]workflowInfluence{}
	if m.instance.ParentRecord != nil {
		record, payload, err := w.Core.record(*m.instance.ParentRecord)
		if err != nil {
			return w, err
		}
		inherited, err = inheritedWorkflowInfluence(m.bundle.Run.Input.Source.Mapping, w.Core.Document.Stages[stage].CaseBindings, payload, record.Lineage.Influence)
		if err != nil {
			return w, err
		}
	}
	records, payloads, outputErr := workflowOutputRecords(w.Core, m.instance, m.prepared.Preparation, workflowRunAudit(m.bundle), m.prepared.Facts, inherited)
	if outputErr != nil && !errors.Is(outputErr, errWorkflowBound) {
		i := m.instance
		i.State = "failed"
		i.Failure = workflowPin("output-invalid")
		i.Attempts[len(i.Attempts)-1].State = i.State
		i.Attempts[len(i.Attempts)-1].Failure = i.Failure
		common.update(i)
		err = workflowV2RequireRecording(common)
		common.traceRequired("output-invalid", err)
		if err != nil {
			return w, err
		}
		return common, nil
	}
	accepted := common.copy()
	localBound := errors.Is(outputErr, errWorkflowBound)
	if !localBound {
		for n, record := range records {
			raw, err := workflowCanonical(record, 65536)
			if errors.Is(err, errWorkflowBound) {
				localBound = true
				break
			}
			if err != nil {
				return w, err
			}
			pin := accepted.Core.keep("output", raw)
			accepted.Outputs = append(accepted.Outputs, pin)
			accepted.Core.keep("payload", payloads[n])
		}
	}
	i := m.instance
	i.State = "completed"
	if len(records) == 0 {
		i.State = "completed-zero"
	}
	i.Failure = nil
	i.Attempts[len(i.Attempts)-1].State = i.State
	i.Attempts[len(i.Attempts)-1].Failure = nil
	accepted.update(i)
	accepted.order()
	err = accepted.checkBounds()
	admitted := !localBound && !w.Cutoffs[m.instance.Stage] && err == nil
	if admitted && common.Trace == nil {
		return accepted, nil
	}
	if err != nil && !errors.Is(err, errWorkflowBound) {
		return w, err
	}
	i = m.instance
	i.State = "failed"
	i.Failure = workflowPin("limit-exceeded")
	i.Attempts[len(i.Attempts)-1].State = i.State
	i.Attempts[len(i.Attempts)-1].Failure = i.Failure
	common.update(i)
	common.Cutoffs[i.Stage] = true
	cause := "aggregate-bound"
	var candidate *workflowV2Inventory = &accepted
	if w.Cutoffs[i.Stage] {
		cause = "cutoff"
	} else if localBound {
		cause, candidate = "local-output-bound", nil
	} else if len(accepted.Outputs) > accepted.Core.Document.Limits.MaxRecords {
		cause = "record-count"
	} else if admitted {
		cause = "fits"
	}
	err = workflowV2RequireRecording(common)
	common.traceDecision("result", result.Instance, cause, admitted, admitted || err == nil, candidate, &common)
	if admitted {
		return accepted, nil
	}
	if err != nil {
		return w, err
	}
	return common, nil
}
