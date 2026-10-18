package runner

// Private implementation experiment for proposed RFC 0022, not an adopted
// format or public API. No writer transition grants acquisition permission.
import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

var errWorkflowV2Recording = errors.New("workflow v2 recording unavailable; material remains in custody")

type workflowV2Begin struct {
	Instance string         `json:"instance"`
	Number   int            `json:"number"`
	Retry    *workflowRetry `json:"retry"`
}
type workflowV2Cancel struct {
	Instance string `json:"instance"`
	Actor    string `json:"actor"`
	At       string `json:"at"`
}
type workflowV2Observation struct {
	Instance              string  `json:"instance"`
	Number                int     `json:"number"`
	State                 string  `json:"state"`
	Failure               *string `json:"failure"`
	RunDigest             *string `json:"runDigest"`
	PreparationDigest     *string `json:"preparationDigest"`
	AuditDigest           *string `json:"auditDigest"`
	EvaluationErrorDigest *string `json:"evaluationErrorDigest"`
	AuditSignaturesDigest *string `json:"auditSignaturesDigest"`
}
type workflowV2Proof struct {
	AdmissionProofVersion int                 `json:"admissionProofVersion"`
	WorkflowDigest        string              `json:"workflowDigest"`
	Execution             string              `json:"execution"`
	RootCaseDigest        string              `json:"rootCaseDigest"`
	FrozenDigest          string              `json:"frozenDigest"`
	RootInstance          string              `json:"rootInstance"`
	EpochBlocks           [][]json.RawMessage `json:"epochBlocks"`
}

// Epochs have closed kind-dependent forms; unused members are never serialized.
type workflowV2Epoch struct {
	Number              int                     `json:"number"`
	PreviousProofDigest string                  `json:"previousProofDigest"`
	Kind                string                  `json:"kind"`
	UpstreamStage       string                  `json:"upstreamStage"`
	Revision            int                     `json:"revision"`
	SelectionCoreDigest string                  `json:"selectionCoreDigest"`
	ChildIDs            []string                `json:"childIds"`
	Reservation         int                     `json:"reservation"`
	Starts              []workflowV2Begin       `json:"starts"`
	Deferred            []string                `json:"deferred"`
	Cancelled           []workflowV2Cancel      `json:"cancelled"`
	Start               workflowV2Begin         `json:"start"`
	Results             []workflowV2Observation `json:"results"`
}

func workflowV2Assertion(actor, at string) bool {
	t, err := time.Parse("2006-01-02T15:04:05Z", at)
	return len(actor) > 0 && len(actor) <= 256 && utf8.ValidString(actor) && err == nil && t.Format("2006-01-02T15:04:05Z") == at
}
func (p workflowV2Proof) bytes() ([]byte, error) { return workflowCanonical(p, 8<<20) }
func (p workflowV2Proof) last() (workflowV2Epoch, error) {
	var e workflowV2Epoch
	if len(p.EpochBlocks) == 0 || len(p.EpochBlocks[len(p.EpochBlocks)-1]) == 0 {
		return e, errWorkflowForm
	}
	err := json.Unmarshal(p.EpochBlocks[len(p.EpochBlocks)-1][len(p.EpochBlocks[len(p.EpochBlocks)-1])-1], &e)
	return e, err
}
func (p workflowV2Proof) closed() bool {
	e, err := p.last()
	if err != nil {
		return false
	}
	return (e.Kind != "group" && e.Kind != "retry") || len(e.Results) == len(e.begins())
}
func (e workflowV2Epoch) begins() []workflowV2Begin {
	if e.Kind == "retry" {
		return []workflowV2Begin{e.Start}
	}
	return e.Starts
}
func (p *workflowV2Proof) append(kind string, fields map[string]any) error {
	if len(p.EpochBlocks) > 0 && !p.closed() {
		return errWorkflowTransition
	}
	previous, err := p.bytes()
	if err != nil {
		return err
	}
	number := 1
	for _, block := range p.EpochBlocks {
		number += len(block)
	}
	e := map[string]any{"number": number, "previousProofDigest": digest(previous), "kind": kind}
	for k, v := range fields {
		e[k] = v
	}
	raw, err := workflowCanonical(e, 8<<20)
	if err != nil {
		return err
	}
	if len(p.EpochBlocks) == 0 || len(p.EpochBlocks[len(p.EpochBlocks)-1]) == 4096 {
		if len(p.EpochBlocks) == 32 {
			return errWorkflowV2Recording
		}
		p.EpochBlocks = append(p.EpochBlocks, []json.RawMessage{})
	}
	n := len(p.EpochBlocks) - 1
	p.EpochBlocks[n] = append(p.EpochBlocks[n], raw)
	_, err = p.bytes()
	return err
}
func (p *workflowV2Proof) observe(o workflowV2Observation) error {
	e, err := p.last()
	if err != nil || p.closed() {
		return errWorkflowTransition
	}
	starts := e.begins()
	expected := starts[len(e.Results)]
	if o.Instance != expected.Instance || o.Number != expected.Number {
		return errWorkflowTransition
	}
	b := len(p.EpochBlocks) - 1
	n := len(p.EpochBlocks[b]) - 1
	var object map[string]json.RawMessage
	_ = json.Unmarshal(p.EpochBlocks[b][n], &object)
	e.Results = append(e.Results, o)
	object["results"] = encode(e.Results)
	raw, err := workflowCanonical(object, 8<<20)
	if err != nil {
		return err
	}
	p.EpochBlocks[b][n] = raw
	_, err = p.bytes()
	return err
}

// Store admission checks syntax/linkage only. This is not an independent reader.
func readWorkflowV2Proof(raw []byte) (workflowV2Proof, error) {
	var p workflowV2Proof
	v, canonical, err := workflowValue(raw, 8<<20, 64)
	if err != nil || !bytes.Equal(raw, canonical) {
		return p, errWorkflowForm
	}
	if _, ok := workflowObject(v, "admissionProofVersion", "workflowDigest", "execution", "rootCaseDigest", "frozenDigest", "rootInstance", "epochBlocks"); !ok {
		return p, errWorkflowForm
	}
	if json.Unmarshal(raw, &p) != nil || p.AdmissionProofVersion != 1 || !workflowDigest.MatchString(p.WorkflowDigest) || !workflowDigest.MatchString(p.RootCaseDigest) || !workflowDigest.MatchString(p.FrozenDigest) || !workflowID.MatchString(p.Execution) || !workflowID.MatchString(p.RootInstance) || len(p.EpochBlocks) < 1 || len(p.EpochBlocks) > 32 {
		return p, errWorkflowForm
	}
	prefix := p
	prefix.EpochBlocks = [][]json.RawMessage{}
	number := 0
	for i, block := range p.EpochBlocks {
		if len(block) < 1 || len(block) > 4096 || i < len(p.EpochBlocks)-1 && len(block) != 4096 {
			return p, errWorkflowForm
		}
		for _, rawEpoch := range block {
			if number > 0 && !prefix.closed() {
				return p, errWorkflowTransition
			}
			var e workflowV2Epoch
			var object map[string]any
			parsed, _, err := workflowValue(rawEpoch, 8<<20, 64)
			if err != nil {
				return p, err
			}
			object, _ = parsed.(map[string]any)
			if json.Unmarshal(rawEpoch, &e) != nil {
				return p, errWorkflowForm
			}
			keys := []string{"number", "previousProofDigest", "kind"}
			switch e.Kind {
			case "root":
				if number != 0 {
					return p, errWorkflowForm
				}
			case "selection", "fork":
				keys = append(keys, "upstreamStage", "revision", "selectionCoreDigest", "childIds")
			case "group":
				keys = append(keys, "reservation", "starts", "deferred", "cancelled", "results")
			case "retry":
				keys = append(keys, "start", "results")
			default:
				return p, errWorkflowForm
			}
			if _, ok := workflowObject(object, keys...); !ok || number == 0 && e.Kind != "root" {
				return p, errWorkflowForm
			}
			if err := workflowV2EpochForm(e, object); err != nil {
				return p, err
			}
			before, err := prefix.bytes()
			if err != nil || e.PreviousProofDigest != digest(before) || e.Number != number+1 {
				return p, errWorkflowTransition
			}
			if e.Kind == "group" || e.Kind == "retry" {
				if len(e.Results) > len(e.begins()) {
					return p, errWorkflowTransition
				}
				for j, result := range e.Results {
					if result.Instance != e.begins()[j].Instance || result.Number != e.begins()[j].Number {
						return p, errWorkflowTransition
					}
				}
			}
			if len(prefix.EpochBlocks) == 0 || len(prefix.EpochBlocks[len(prefix.EpochBlocks)-1]) == 4096 {
				prefix.EpochBlocks = append(prefix.EpochBlocks, []json.RawMessage{})
			}
			j := len(prefix.EpochBlocks) - 1
			prefix.EpochBlocks[j] = append(prefix.EpochBlocks[j], bytes.Clone(rawEpoch))
			number++
		}
	}
	return p, nil
}

func workflowV2EpochForm(e workflowV2Epoch, object map[string]any) error {
	begin := func(value any, b workflowV2Begin, first bool) bool {
		if _, ok := workflowObject(value, "instance", "number", "retry"); !ok || !workflowID.MatchString(b.Instance) || b.Number < 1 || b.Number > 16 {
			return false
		}
		if first {
			return b.Number == 1 && b.Retry == nil && value.(map[string]any)["retry"] == nil
		}
		if b.Number < 2 || b.Retry == nil || !workflowV2Assertion(b.Retry.Actor, b.Retry.At) {
			return false
		}
		_, ok := workflowObject(value.(map[string]any)["retry"], "actor", "at")
		return ok
	}
	switch e.Kind {
	case "selection", "fork":
		if !workflowID.MatchString(e.UpstreamStage) || !workflowDigest.MatchString(e.SelectionCoreDigest) || e.Revision < 1 || e.Revision > 16 {
			return errWorkflowForm
		}
		if object["childIds"] != nil && e.ChildIDs == nil {
			return errWorkflowForm
		}
		seen := map[string]bool{}
		for _, id := range e.ChildIDs {
			if !workflowID.MatchString(id) || seen[id] {
				return errWorkflowForm
			}
			seen[id] = true
		}
	case "group":
		if e.Reservation < 1 || e.Reservation >= e.Number || e.Starts == nil || e.Deferred == nil || e.Cancelled == nil || e.Results == nil || len(e.Starts)+len(e.Cancelled) == 0 {
			return errWorkflowForm
		}
		starts, _ := object["starts"].([]any)
		cancelled, _ := object["cancelled"].([]any)
		seen := map[string]bool{}
		for i, b := range e.Starts {
			if !begin(starts[i], b, true) || seen[b.Instance] {
				return errWorkflowForm
			}
			seen[b.Instance] = true
		}
		for _, id := range e.Deferred {
			if !workflowID.MatchString(id) || seen[id] {
				return errWorkflowForm
			}
			seen[id] = true
		}
		for i, c := range e.Cancelled {
			if _, ok := workflowObject(cancelled[i], "instance", "actor", "at"); !ok || !workflowID.MatchString(c.Instance) || seen[c.Instance] || !workflowV2Assertion(c.Actor, c.At) {
				return errWorkflowForm
			}
			seen[c.Instance] = true
		}
	case "retry":
		if e.Results == nil || !begin(object["start"], e.Start, false) {
			return errWorkflowForm
		}
	}
	if e.Kind == "group" || e.Kind == "retry" {
		results, _ := object["results"].([]any)
		for i, o := range e.Results {
			if _, ok := workflowObject(results[i], "instance", "number", "state", "failure", "runDigest", "preparationDigest", "auditDigest", "evaluationErrorDigest", "auditSignaturesDigest"); !ok || !workflowID.MatchString(o.Instance) || o.Number < 1 || o.Number > 16 {
				return errWorkflowForm
			}
			for _, pin := range []*string{o.RunDigest, o.PreparationDigest, o.AuditDigest, o.EvaluationErrorDigest, o.AuditSignaturesDigest} {
				if pin != nil && !workflowDigest.MatchString(*pin) {
					return errWorkflowForm
				}
			}
			if o.State == "failed" {
				if o.Failure == nil || (*o.Failure != "acquisition-failed" && *o.Failure != "preparation-failed" && *o.Failure != "admission-refused" && *o.Failure != "evaluation-error") {
					return errWorkflowForm
				}
			} else if o.Failure != nil || (o.State != "evaluated" && o.State != "interrupted" && o.State != "cancelled" && o.State != "suppressed") {
				return errWorkflowForm
			}
		}
	}
	return nil
}
