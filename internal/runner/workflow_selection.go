package runner

// Selection checks are pure. Transactional reservation and consumed-selection
// immutability belong to the executor, not to a caller-supplied review string.
import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

type workflowSet struct {
	WorkflowDigest string   `json:"workflowDigest"`
	Execution      string   `json:"execution"`
	Stage          string   `json:"stage"`
	Records        []string `json:"records"`
}
type workflowReview struct {
	Actor     string `json:"actor"`
	Assertion string `json:"assertion"`
	At        string `json:"at"`
}
type workflowSelection struct {
	SelectionVersion int             `json:"selectionVersion"`
	WorkflowDigest   string          `json:"workflowDigest"`
	Execution        string          `json:"execution"`
	UpstreamStage    string          `json:"upstreamStage"`
	OutputSetDigest  string          `json:"outputSetDigest"`
	Selected         []string        `json:"selected"`
	Mode             string          `json:"mode"`
	Revision         int             `json:"revision"`
	Review           *workflowReview `json:"review,omitempty"`
	Rule             *workflowRule   `json:"rule,omitempty"`
	Refusal          string          `json:"refusal,omitempty"`
}

var errWorkflowSelection = errors.New("workflow selection does not match the closed offered set")

func readWorkflowSet(raw []byte, documentDigest, execution, stage string, maxRecords int) (workflowSet, []byte, error) {
	var set workflowSet
	v, canonical, err := workflowValue(raw, 8<<20, 64)
	if err != nil {
		return set, nil, err
	}
	if _, ok := workflowObject(v, "workflowDigest", "execution", "stage", "records"); !ok {
		return set, nil, errWorkflowForm
	}
	if err = json.Unmarshal(canonical, &set); err != nil {
		return set, nil, errWorkflowForm
	}
	if !workflowDigest.MatchString(documentDigest) || !workflowID.MatchString(execution) || !workflowID.MatchString(stage) || set.WorkflowDigest != documentDigest || set.Execution != execution || set.Stage != stage || set.Records == nil {
		return set, nil, errWorkflowForm
	}
	if maxRecords < 1 || maxRecords > 4096 || len(set.Records) > maxRecords {
		return set, nil, errWorkflowBound
	}
	seen := map[string]bool{}
	for _, record := range set.Records {
		if !workflowDigest.MatchString(record) || seen[record] {
			return set, nil, errWorkflowForm
		}
		seen[record] = true
	}
	return set, canonical, nil
}

// readWorkflowSelection checks a selection against the whole offered set and
// stage policy. Disposition mode requires every offered record's independently
// checked audit disposition, including those it will not select. It does not
// authenticate the actor or prove that the set is a complete execution.
func readWorkflowSelection(raw []byte, set workflowSet, stage workflowStage, maxRecords int, dispositions map[string]json.RawMessage) (workflowSelection, []byte, error) {
	var selected workflowSelection
	_, setBytes, err := readWorkflowSet(encode(set), set.WorkflowDigest, set.Execution, stage.ID, maxRecords)
	if err != nil {
		return selected, nil, err
	}
	v, canonical, err := workflowValue(raw, 65536, 64)
	if err != nil {
		return selected, nil, err
	}
	object, ok := v.(map[string]any)
	if !ok {
		return selected, nil, errWorkflowForm
	}
	if version, exists := object["selectionVersion"]; exists && version != json.Number("1") {
		return selected, nil, errWorkflowVersion
	}
	if err = json.Unmarshal(canonical, &selected); err != nil {
		return selected, nil, errWorkflowForm
	}
	keys := []string{"selectionVersion", "workflowDigest", "execution", "upstreamStage", "outputSetDigest", "selected", "mode", "revision"}
	if selected.Mode == "reviewer" {
		keys = append(keys, "review")
	} else if selected.Mode == "disposition" {
		keys = append(keys, "rule")
	} else {
		return selected, nil, errWorkflowForm
	}
	if _, exists := object["refusal"]; exists {
		keys = append(keys, "refusal")
		if selected.Refusal != "limit-exceeded" {
			return selected, nil, errWorkflowForm
		}
	}
	if _, ok := workflowObject(object, keys...); !ok || selected.SelectionVersion != 1 || selected.Revision < 1 || selected.Revision > 16 || selected.Selected == nil {
		return selected, nil, errWorkflowForm
	}
	if stage.Selection == nil || selected.Mode != stage.Selection.Mode || selected.WorkflowDigest != set.WorkflowDigest || selected.Execution != set.Execution || selected.UpstreamStage != stage.ID || selected.OutputSetDigest != digest(setBytes) {
		return selected, nil, errWorkflowSelection
	}
	if len(selected.Selected) > maxRecords {
		return selected, nil, errWorkflowBound
	}
	// A monotonically increasing subsequence rejects repeats, reordered items
	// and foreign records while retaining distinct envelopes of equal payloads.
	next := 0
	for _, record := range selected.Selected {
		for next < len(set.Records) && set.Records[next] != record {
			next++
		}
		if next == len(set.Records) {
			return selected, nil, errWorkflowSelection
		}
		next++
	}
	if selected.Mode == "reviewer" {
		if _, ok := workflowObject(object["review"], "actor", "assertion", "at"); !ok || selected.Review == nil {
			return selected, nil, errWorkflowForm
		}
		review := selected.Review
		at, err := time.Parse("2006-01-02T15:04:05Z", review.At)
		if err != nil || at.Format("2006-01-02T15:04:05Z") != review.At || len(review.Actor) < 1 || len(review.Actor) > 256 || len(review.Assertion) < 1 || len(review.Assertion) > 1024 {
			return selected, nil, errWorkflowForm
		}
	} else {
		if stage.Outputs.Cardinality != "one" || stage.Selection.Rule == nil || selected.Rule == nil || !bytes.Equal(encode(selected.Rule), encode(stage.Selection.Rule)) {
			return selected, nil, errWorkflowSelection
		}
		ruleKeys := []string{"kind"}
		if selected.Rule.Kind == "outcome" {
			ruleKeys = append(ruleKeys, "outcomeId")
		}
		if _, ok := workflowObject(object["rule"], ruleKeys...); !ok {
			return selected, nil, errWorkflowForm
		}
		expected, err := workflowDispositionSelection(set, stage.Selection.Rule, dispositions)
		if err != nil || !bytes.Equal(encode(expected), encode(selected.Selected)) {
			return selected, nil, errWorkflowSelection
		}
	}
	return selected, canonical, nil
}

func workflowDispositionSelection(set workflowSet, rule *workflowRule, dispositions map[string]json.RawMessage) ([]string, error) {
	if rule == nil || len(dispositions) != len(set.Records) {
		return nil, errWorkflowSelection
	}
	switch rule.Kind {
	case "outcome":
		if len(rule.OutcomeID) < 1 || len(rule.OutcomeID) > 256 {
			return nil, errWorkflowForm
		}
	case "unresolved", "not-applicable":
		if rule.OutcomeID != "" {
			return nil, errWorkflowForm
		}
	default:
		return nil, errWorkflowForm
	}
	selected := []string{}
	for _, record := range set.Records {
		raw, exists := dispositions[record]
		if !exists || len(raw) > maxOutput {
			return nil, errWorkflowSelection
		}
		v, err := readInputJSONDepth(raw, 64)
		if err != nil {
			return nil, errWorkflowSelection
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, errWorkflowSelection
		}
		kind, ok := obj["kind"].(string)
		if !ok {
			return nil, errWorkflowSelection
		}
		switch kind {
		case "outcome":
			if id, ok := obj["outcomeId"].(string); !ok || id == "" {
				return nil, errWorkflowSelection
			}
		case "unresolved", "not-applicable":
		default:
			return nil, errWorkflowSelection
		}
		if kind == rule.Kind && (kind != "outcome" || obj["outcomeId"] == rule.OutcomeID) {
			selected = append(selected, record)
		}
	}
	return selected, nil
}
