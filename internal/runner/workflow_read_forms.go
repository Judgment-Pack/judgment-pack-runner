package runner

// Intrinsic forms for the offline workflow reader. These checks do not compare
// artifact identities, verify preparations, apply selections or replay policy.
import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// Measure known smaller workflow limits without accepting a form. A malformed
// sibling cannot hide a measurable count or string bound in another artifact.
func workflowReaderCountBounds(path string, value, workflow any) bool {
	object, list := workflowReadObject, workflowReadList
	long := func(v any, limit int) bool { s, ok := v.(string); return ok && len(s) > limit }
	count := func(v any, limit int) bool { return len(list(v)) > limit }
	number := func(v any, fallback int) int {
		n, ok := v.(json.Number)
		if !ok {
			return fallback
		}
		i, err := n.Int64()
		if err != nil || i < 1 || i > int64(fallback) {
			return fallback
		}
		return int(i)
	}
	limits := object(object(workflow)["limits"])
	o := object(value)
	pointer := func(v any) bool { s, ok := v.(string); return ok && (len(s) > 1024 || strings.Count(s, "/") > 32) }
	rule := func(v any) bool { return long(object(v)["outcomeId"], 256) }
	switch {
	case path == "workflow.json":
		for key, ceiling := range map[string]int{"maxStages": 32, "maxInstances": 4096, "maxRecords": 4096, "maxAttempts": 16, "maxBytes": 1 << 30} {
			if n, ok := limits[key].(json.Number); ok {
				if i, err := n.Int64(); err == nil && (i < 1 || i > int64(ceiling)) {
					return true
				}
			}
		}
		if count(o["stages"], number(limits["maxStages"], 32)) {
			return true
		}
		for _, value := range list(o["stages"]) {
			s := object(value)
			if long(s["id"], 64) || long(object(s["pack"])["id"], 2048) || long(object(s["dialect"])["id"], 1024) || count(s["caseBindings"], 64) || pointer(object(s["outputs"])["pointer"]) || rule(object(s["selection"])["rule"]) {
				return true
			}
			for _, b := range list(s["caseBindings"]) {
				if pointer(object(b)["from"]) || pointer(object(b)["to"]) {
					return true
				}
			}
		}
	case path == "execution.json":
		if count(o["frozen"], number(limits["maxStages"], 32)) || count(o["outputSets"], number(limits["maxStages"], 32)) || count(o["selections"], number(limits["maxStages"], 32)) || count(o["instances"], number(limits["maxInstances"], 4096)) {
			return true
		}
		outputs := 0
		for _, s := range list(o["outputSets"]) {
			outputs += len(list(object(s)["records"]))
		}
		if outputs > number(limits["maxRecords"], 4096) {
			return true
		}
		for _, value := range list(o["instances"]) {
			i := object(value)
			if count(i["attempts"], number(limits["maxAttempts"], 16)) {
				return true
			}
			for _, a := range list(i["attempts"]) {
				if long(object(object(a)["retry"])["actor"], 256) {
					return true
				}
			}
		}
	case strings.HasPrefix(path, "selections/"):
		if count(o["selected"], number(limits["maxRecords"], 4096)) || long(object(o["review"])["actor"], 256) || long(object(o["review"])["assertion"], 1024) || rule(o["rule"]) {
			return true
		}
	case strings.HasPrefix(path, "sets/"):
		return count(o["records"], number(limits["maxRecords"], 4096))
	case strings.HasPrefix(path, "outputs/") && strings.HasSuffix(path, "/record.json"):
		return pointer(object(o["lineage"])["outputPointer"])
	}
	return false
}

// Scan only format selectors in known artifact positions. Artifact identity
// versions, such as a named dialect's string version, are checked as forms.
func workflowReaderUnreadVersion(path string, value any) bool {
	object, list := workflowReadObject, workflowReadList
	selector := func(o map[string]any, key string, allowed ...any) bool {
		v, present := o[key]
		if !present {
			return false
		}
		for _, a := range allowed {
			if reflect.DeepEqual(v, a) {
				return false
			}
		}
		return true
	}
	mapping := func(v any) bool {
		o := object(v)
		if _, named := o["formatVersion"]; named {
			if selector(o, "formatVersion", "2.0.0", "3.0.0") {
				return true
			}
		} else if selector(o, "version", json.Number("2"), json.Number("3")) {
			return true
		}
		for _, s := range list(o["sources"]) {
			if selector(object(object(object(s)["read"])["rule"]), "ruleVersion", "1") {
				return true
			}
		}
		return false
	}
	preparation := func(v any) bool { return selector(object(v), "version", json.Number("2"), json.Number("3")) }
	input := func(v any) bool {
		o := object(v)
		s := object(o["source"])
		return preparation(o["preparation"]) || mapping(s["mapping"]) || mapping(object(s["import"])["dialect"])
	}
	release := func(v any) bool {
		o := object(v)
		return selector(o, "schemaVersion", "1") || mapping(o["inputMapping"]) || input(o["sample"])
	}
	o := object(value)
	switch {
	case path == "workflow.json":
		if selector(o, "workflowVersion", json.Number("1")) {
			return true
		}
		for _, s := range list(o["stages"]) {
			if mapping(object(s)["dialect"]) {
				return true
			}
		}
	case path == "execution.json":
		return selector(o, "executionVersion", json.Number("1"))
	case strings.HasPrefix(path, "selections/"):
		return selector(o, "selectionVersion", json.Number("1"))
	case strings.HasPrefix(path, "outputs/") && strings.HasSuffix(path, "/record.json"):
		return selector(o, "outputRecordVersion", json.Number("1"))
	case strings.HasSuffix(path, "/dialect.json"), strings.HasSuffix(path, "/bound-dialect.json"):
		return mapping(o)
	case strings.HasSuffix(path, "/preparation.json"):
		return preparation(o)
	case strings.HasSuffix(path, "/release.json"):
		return release(o)
	case strings.HasSuffix(path, "/audit.json"):
		return selector(o, "recordVersion", "1")
	case strings.HasSuffix(path, "/evaluation-error.json"):
		return selector(o, "outputVersion", "2")
	case strings.HasSuffix(path, "/run.json"):
		run := object(o["run"])
		return selector(o, "version", json.Number("2"), json.Number("3")) || release(o["release"]) || selector(run, "schemaVersion", "1") || input(run["input"]) || selector(object(run["audit"]), "recordVersion", "1") || selector(object(run["result"]), "outputVersion", "2")
	}
	return false
}

func workflowDigestOrNull(pin *string) bool { return pin == nil || workflowDigest.MatchString(*pin) }
func workflowState(value string) bool {
	switch value {
	case "completed", "completed-zero", "failed", "interrupted", "cancelled":
		return true
	}
	return false
}
func workflowFailureCode(value *string) bool {
	if value == nil {
		return true
	}
	switch *value {
	case "projection-invalid", "admission-refused", "acquisition-failed", "preparation-failed", "evaluation-error", "output-invalid", "limit-exceeded":
		return true
	}
	return false
}
func workflowRetryForm(retry *workflowRetry) bool {
	if retry == nil {
		return true
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", retry.At)
	return len(retry.Actor) > 0 && len(retry.Actor) <= 256 && err == nil && at.Format("2006-01-02T15:04:05Z") == retry.At
}
func readWorkflowExecution(raw []byte, limits workflowLimits) (workflowExecution, []byte, error) {
	var execution workflowExecution
	value, canonical, err := workflowValue(raw, 8<<20, 64)
	if err != nil {
		return execution, nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return execution, nil, errWorkflowForm
	}
	if version, ok := object["executionVersion"]; ok && version != json.Number("1") {
		return execution, nil, errWorkflowVersion
	}
	if err := strictJSON(raw, &execution); err != nil || !sameJSON(raw, encode(execution)) {
		return execution, nil, errWorkflowForm
	}
	if execution.ExecutionVersion != 1 || !workflowDigest.MatchString(execution.WorkflowDigest) || !workflowDigest.MatchString(execution.RootCaseDigest) || !workflowID.MatchString(execution.Execution) || execution.Frozen == nil || execution.Instances == nil || execution.OutputSets == nil || execution.Selections == nil {
		return execution, nil, errWorkflowForm
	}
	if len(execution.Frozen) > limits.MaxStages || len(execution.OutputSets) > limits.MaxStages || len(execution.Selections) > limits.MaxStages || len(execution.Instances) > limits.MaxInstances {
		return execution, nil, errWorkflowBound
	}
	for _, stage := range execution.Frozen {
		if !workflowID.MatchString(stage.Stage) || !workflowDigest.MatchString(stage.PublishedDialectDigest) || !workflowDigestOrNull(stage.BindingDocumentDigest) || !workflowDigest.MatchString(stage.BoundDialectDigest) || !workflowDigest.MatchString(stage.PackDigest) || !workflowDigest.MatchString(stage.ReleaseDigest) || !workflowDigest.MatchString(stage.RuntimeDigest) {
			return execution, nil, errWorkflowForm
		}
	}
	seen := map[string]bool{}
	for _, instance := range execution.Instances {
		if !workflowID.MatchString(instance.ID) || seen[instance.ID] || !workflowID.MatchString(instance.Stage) || !workflowDigestOrNull(instance.ParentRecord) || !workflowDigestOrNull(instance.SelectionDigest) || !workflowDigest.MatchString(instance.InputDigest) || !workflowDigestOrNull(instance.CaseDigest) || !workflowState(instance.State) || !workflowFailureCode(instance.Failure) || instance.Attempts == nil {
			return execution, nil, errWorkflowForm
		}
		seen[instance.ID] = true
		if len(instance.Attempts) > limits.MaxAttempts {
			return execution, nil, errWorkflowBound
		}
		for _, attempt := range instance.Attempts {
			if attempt.Number < 1 || attempt.Number > 16 || !workflowState(attempt.State) || !workflowFailureCode(attempt.Failure) || !workflowDigestOrNull(attempt.PreparationDigest) || !workflowDigestOrNull(attempt.AuditDigest) || !workflowDigestOrNull(attempt.RunDigest) || !workflowDigestOrNull(attempt.EvaluationErrorDigest) || !workflowRetryForm(attempt.Retry) {
				return execution, nil, errWorkflowForm
			}
		}
	}
	records := 0
	for _, set := range execution.OutputSets {
		if !workflowID.MatchString(set.Stage) || set.Status != "closed" && set.Status != "blocked" || set.Records == nil {
			return execution, nil, errWorkflowForm
		}
		records += len(set.Records)
		if records > limits.MaxRecords {
			return execution, nil, errWorkflowBound
		}
		seen := map[string]bool{}
		for _, pin := range set.Records {
			if !workflowDigest.MatchString(pin) || seen[pin] {
				return execution, nil, errWorkflowForm
			}
			seen[pin] = true
		}
	}
	for _, pin := range execution.Selections {
		if !workflowDigestOrNull(pin) {
			return execution, nil, errWorkflowForm
		}
	}
	return execution, canonical, nil
}

func readWorkflowOutput(raw []byte) (workflowOutputRecord, []byte, error) {
	var output workflowOutputRecord
	value, canonical, err := workflowValue(raw, 65536, 64)
	if err != nil {
		return output, nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return output, nil, errWorkflowForm
	}
	if version, ok := object["outputRecordVersion"]; ok && version != json.Number("1") {
		return output, nil, errWorkflowVersion
	}
	if strictJSON(raw, &output) != nil || !sameJSON(raw, encode(output)) || output.OutputRecordVersion != 1 || !workflowDigest.MatchString(output.PayloadDigest) {
		return output, nil, errWorkflowForm
	}
	l := output.Lineage
	for _, pin := range []string{l.WorkflowDigest, l.PublishedDialectDigest, l.BoundDialectDigest, l.PackDigest, l.ReleaseDigest, l.PreparationDigest, l.AuditDigest, l.RootCaseDigest, l.CaseDigest} {
		if !workflowDigest.MatchString(pin) {
			return output, nil, errWorkflowForm
		}
	}
	for _, id := range []string{l.Execution, l.Stage, l.Instance} {
		if !workflowID.MatchString(id) {
			return output, nil, errWorkflowForm
		}
	}
	if !workflowDigestOrNull(l.BindingDocumentDigest) || !workflowDigestOrNull(l.ParentRecord) || !workflowDigestOrNull(l.SelectionDigest) || l.Index < 0 || l.Index > 4095 || !checkedWorkflowInfluence(l.Influence) || !workflowID.MatchString(l.Relation.Label) || (l.Relation.Direction != "output-to-parent" && l.Relation.Direction != "parent-to-output") {
		return output, nil, errWorkflowForm
	}
	if _, err := workflowPointer(l.OutputPointer, true); err != nil {
		return output, nil, err
	}
	return output, canonical, nil
}

// Validate selection form independently before any offered-set or policy
// comparisons. readWorkflowSelection intentionally combines both for writers.
func workflowSelectionForm(raw []byte, limit int) (workflowSelection, error) {
	var selection workflowSelection
	value, _, err := workflowValue(raw, 65536, 64)
	if err != nil {
		return selection, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return selection, errWorkflowForm
	}
	if v, ok := object["selectionVersion"]; ok && v != json.Number("1") {
		return selection, errWorkflowVersion
	}
	if strictJSON(raw, &selection) != nil || !sameJSON(raw, encode(selection)) || selection.SelectionVersion != 1 || !workflowDigest.MatchString(selection.WorkflowDigest) || !workflowDigest.MatchString(selection.OutputSetDigest) || !workflowID.MatchString(selection.Execution) || !workflowID.MatchString(selection.UpstreamStage) || selection.Selected == nil || selection.Revision < 1 || selection.Revision > 16 || (selection.Refusal != "" && selection.Refusal != "limit-exceeded") {
		return selection, errWorkflowForm
	}
	if len(selection.Selected) > limit {
		return selection, errWorkflowBound
	}
	for _, pin := range selection.Selected {
		if !workflowDigest.MatchString(pin) {
			return selection, errWorkflowForm
		}
	}
	switch selection.Mode {
	case "reviewer":
		if selection.Rule != nil || selection.Review == nil || !workflowRetryForm(&workflowRetry{Actor: selection.Review.Actor, At: selection.Review.At}) || len(selection.Review.Assertion) < 1 || len(selection.Review.Assertion) > 1024 {
			return selection, errWorkflowForm
		}
	case "disposition":
		if selection.Review != nil || selection.Rule == nil {
			return selection, errWorkflowForm
		}
		switch selection.Rule.Kind {
		case "outcome":
			if selection.Rule.OutcomeID == "" || len(selection.Rule.OutcomeID) > 256 {
				return selection, errWorkflowForm
			}
		case "unresolved", "not-applicable":
			if selection.Rule.OutcomeID != "" {
				return selection, errWorkflowForm
			}
		default:
			return selection, errWorkflowForm
		}
	default:
		return selection, errWorkflowForm
	}
	return selection, nil
}

func workflowDispositionForm(raw []byte, pack []byte) bool {
	value, err := readInputJSONDepth(raw, 64)
	if err != nil {
		return false
	}
	o, ok := value.(map[string]any)
	if !ok {
		return false
	}
	keys := []string{"kind", "reasons", "handoff"}
	if o["kind"] == "outcome" {
		keys = append(keys, "outcomeId")
	}
	if _, ok := workflowObject(o, keys...); !ok {
		return false
	}
	reasons, ok := o["reasons"].([]any)
	if !ok {
		return false
	}
	set := map[string]bool{}
	previous := ""
	for i, v := range reasons {
		reason, ok := v.(string)
		if !ok || set[reason] || i > 0 && reason <= previous {
			return false
		}
		previous = reason
		switch reason {
		case "not-applicable", "missing-required-evidence", "unknown", "conflict", "no-match", "exception-escalation":
		default:
			return false
		}
		set[reason] = true
	}
	switch o["kind"] {
	case "outcome":
		if len(reasons) != 0 {
			return false
		}
		name, ok := o["outcomeId"].(string)
		if !ok || name == "" {
			return false
		}
		var p struct {
			Outcomes []struct {
				ID string `json:"id"`
			} `json:"outcomes"`
		}
		if pack != nil && json.Unmarshal(pack, &p) != nil {
			return false
		}
		found := 0
		for _, outcome := range p.Outcomes {
			if outcome.ID == name {
				found++
			}
		}
		if pack != nil && found != 1 {
			return false
		}
	case "not-applicable":
		if len(reasons) != 1 || !set["not-applicable"] {
			return false
		}
	case "unresolved":
		if len(reasons) == 0 {
			return false
		}
	default:
		return false
	}
	handoff, ok := o["handoff"].(map[string]any)
	if !ok {
		return false
	}
	if handoff["state"] == "none" {
		_, ok = workflowObject(handoff, "state")
		return ok
	}
	if handoff["state"] != "requested" {
		return false
	}
	if _, ok = workflowObject(handoff, "state", "triggeredBy"); !ok {
		return false
	}
	triggered, ok := handoff["triggeredBy"].([]any)
	if !ok || len(triggered) == 0 {
		return false
	}
	seen := map[string]bool{}
	previous = ""
	for i, v := range triggered {
		reason, ok := v.(string)
		if !ok || seen[reason] || !set[reason] || i > 0 && reason <= previous {
			return false
		}
		previous = reason
		seen[reason] = true
	}
	return true
}
