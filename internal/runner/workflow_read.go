package runner

// Private offline reader for the Draft workflow contract. It accepts retained
// bytes and independent expectations; it has no filesystem or network access.
import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
)

type workflowReadInput struct {
	Files                                          map[string][]byte
	WorkflowDigest, ExecutionDigest, RuntimeDigest string
	// Profiles are supplied independently for each stage, never taken from the
	// recording's profiles.json or embedded release as installation authority.
	Profiles map[string][]byte
	// Selections optionally locates the current revision for a stage when a
	// caller retains several revisions and the current bytes no longer match
	// their expected digest. Without a locator, matching content (or a sole
	// candidate) identifies the revision; history is never interpreted.
	Selections map[string]string
}
type workflowReadReport struct {
	Result, Code                                string
	WorkflowDigest, ExecutionDigest, Completion string
	Stages, Instances, Outputs, Selections      int
}

func (r workflowReadReport) line() []byte {
	value := map[string]any{"result": r.Result}
	if r.Result == ResultRefusal {
		value["refusal"] = r.Code
	} else if r.Result == ResultFinding {
		value["finding"] = r.Code
	} else {
		value["workflowDigest"], value["executionDigest"], value["completion"] = r.WorkflowDigest, r.ExecutionDigest, r.Completion
		value["consistency"], value["preparations"], value["authentication"], value["dispositions"] = "agrees", "agree", "not-established", "not-replayed"
		value["counts"] = map[string]int{"stages": r.Stages, "instances": r.Instances, "outputs": r.Outputs, "selections": r.Selections}
	}
	raw, _ := workflowCanonical(value, 4095)
	return append(raw, '\n')
}

type workflowReadProblem struct{ result, code string }

func (e workflowReadProblem) Error() string { return e.result + ": " + e.code }
func workflowRefusal(code string) error     { return workflowReadProblem{ResultRefusal, code} }
func workflowFinding(code string) error     { return workflowReadProblem{ResultFinding, code} }
func workflowReferenceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errWorkflowBound) {
		return workflowRefusal(codeOverBound)
	}
	if errors.Is(err, errWorkflowVersion) {
		return workflowRefusal("version-unread")
	}
	return workflowRefusal(codeMalformed)
}

type workflowReadStage struct {
	published, binding, bound, pack, release, recordedProfiles []byte
	profiles                                                   []InputProfile
	releaseValue                                               Release
	set                                                        workflowSet
	selection                                                  *workflowSelection
	selectionRaw                                               []byte
	outputs                                                    []workflowReadOutput
}
type workflowReadOutput struct {
	record       workflowOutputRecord
	raw, payload []byte
	path, pin    string
}
type workflowReadAttempt struct {
	run, preparation, audit, sources, evaluationError, signatures []byte
	bundle                                                        VerificationBundle
	prepared                                                      *Input
}
type workflowReader struct {
	in                  workflowReadInput
	w                   workflowInventory
	workflow, execution []byte
	stages              []workflowReadStage
	attempts            [][]workflowReadAttempt
	used                map[string]bool
	ignored             map[string]bool
	selectionPaths      map[string]string
	missing             bool
	inherited           []map[workflowTarget]workflowInfluence
	admissionRefused    []bool
}

func readWorkflowRecording(in workflowReadInput) (workflowReadReport, error) {
	for _, pin := range []string{in.WorkflowDigest, in.ExecutionDigest, in.RuntimeDigest} {
		if !workflowDigest.MatchString(pin) {
			return workflowReadReport{}, errors.New("independent workflow, execution and Runtime pins are required")
		}
	}
	r := workflowReader{in: in, used: map[string]bool{}, ignored: map[string]bool{}}
	var err error
	if err = r.reference(); err == nil {
		err = r.preparations()
	}
	if err == nil {
		err = r.identities()
	}
	if err == nil {
		err = r.selections()
	}
	if err == nil {
		err = r.casesAndInfluence()
	}
	if err == nil {
		err = r.inventoryAndOutputs()
	}
	if err != nil {
		var problem workflowReadProblem
		if !errors.As(err, &problem) {
			return workflowReadReport{}, err
		}
		return workflowReadReport{Result: problem.result, Code: problem.code}, nil
	}
	report := workflowReadReport{Result: ResultReading, WorkflowDigest: in.WorkflowDigest, ExecutionDigest: in.ExecutionDigest, Completion: "complete", Stages: len(r.w.Document.Stages), Instances: len(r.w.Execution.Instances)}
	for i, set := range r.w.Execution.OutputSets {
		report.Outputs += len(set.Records)
		if r.w.Execution.Selections[i] != nil {
			report.Selections++
		}
		if set.Status != "closed" {
			report.Completion = "partial"
		}
		for _, stage := range r.w.Document.Stages {
			if stage.Input.FromStage == set.Stage && (r.stages[i].selection == nil || r.stages[i].selection.Refusal != "") {
				report.Completion = "partial"
			}
		}
	}
	for _, instance := range r.w.Execution.Instances {
		if instance.State != "completed" && instance.State != "completed-zero" {
			report.Completion = "partial"
		}
	}
	return report, nil
}
func (r *workflowReader) file(path string, required bool) []byte {
	raw, ok := r.in.Files[path]
	if !ok {
		r.missing = r.missing || required
		return nil
	}
	r.used[path] = true
	return raw
}
func workflowFileDomain(path string) (limit, depth int, integer bool) {
	switch {
	case path == "workflow.json":
		return 262144, 64, true
	case path == "execution.json" || strings.HasPrefix(path, "sets/"):
		return 8 << 20, 64, true
	case strings.HasPrefix(path, "selections/") || strings.HasPrefix(path, "outputs/") && strings.HasSuffix(path, "/record.json"):
		return 65536, 64, true
	case path == "root-case.json" || strings.HasSuffix(path, "/case.json") || strings.HasPrefix(path, "outputs/") && strings.HasSuffix(path, "/payload.json"):
		return 1 << 20, 32, true
	case strings.HasSuffix(path, "/dialect.json") || strings.HasSuffix(path, "/bound-dialect.json"):
		return maxPublishedDialect, 64, false
	case strings.HasSuffix(path, "/binding.json"):
		return maxBindingDocument, 64, false
	case strings.HasSuffix(path, "/profiles.json"):
		return maxProfilesDocument, 64, false
	case strings.HasSuffix(path, "/sources.json"):
		return MaxBody, 64, false
	case strings.HasSuffix(path, "/run.json"):
		return MaxExportSize, 64, false
	case workflowOpaqueSignatures(path):
		return maxSidecar, 64, false
	default:
		return 8 << 20, 64, false
	}
}

// Signature sidecars retain JSONL bytes for a separate authentication reader.
// This private workflow reader only bounds and associates their opaque bytes.
func workflowOpaqueSignatures(path string) bool {
	return strings.HasSuffix(path, "/audit-signatures.json")
}
func workflowJSONArtifact(path string) bool {
	return strings.HasSuffix(path, ".json") && !workflowOpaqueSignatures(path)
}

func workflowReadObject(value any) map[string]any { o, _ := value.(map[string]any); return o }
func workflowReadList(value any) []any            { a, _ := value.([]any); return a }
func workflowReadString(value any) string         { s, _ := value.(string); return s }

// Discovery never reports a JSON or form error. It only identifies which
// retained artifacts the chosen execution references; the global passes below
// subsequently validate those artifacts in the required phase order.
func workflowDiscover(raw []byte, limit, depth int) any {
	if len(raw) > limit || workflowDepthOver(raw, depth) {
		return nil
	}
	v, _ := readInputJSONDepth(raw, depth)
	return v
}
func (r *workflowReader) discoverCurrent(paths []string, document, execution map[string]any) {
	r.selectionPaths = map[string]string{}
	stages, instances := map[string]bool{}, map[string]bool{}
	stageNames := []string{}
	for _, value := range workflowReadList(document["stages"]) {
		name := workflowReadString(workflowReadObject(value)["id"])
		if workflowID.MatchString(name) {
			stages[name] = true
		}
		stageNames = append(stageNames, name)
	}
	for _, value := range workflowReadList(execution["instances"]) {
		name := workflowReadString(workflowReadObject(value)["id"])
		if workflowID.MatchString(name) {
			instances[name] = true
		}
	}
	for i, value := range workflowReadList(execution["selections"]) {
		pin := workflowReadString(value)
		if i >= len(stageNames) || !workflowDigest.MatchString(pin) || !stages[stageNames[i]] {
			continue
		}
		stage := stageNames[i]
		prefix := "selections/" + stage + "/"
		if path := r.in.Selections[stage]; strings.HasPrefix(path, prefix) && strings.HasSuffix(path, ".json") {
			r.selectionPaths[stage] = path
			continue
		}
		candidates := []string{}
		for _, path := range paths {
			if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ".json") {
				continue
			}
			candidates = append(candidates, path)
			raw := r.in.Files[path]
			if len(raw) <= 65536 && !workflowDepthOver(raw, 64) {
				// Selection identity uses workflow integer canon, including -0 -> 0.
				if canonical, err := derivation.Canon(raw); err == nil && digest(canonical) == pin {
					r.selectionPaths[stage] = path
					break
				}
			}
		}
		if r.selectionPaths[stage] == "" && len(candidates) == 1 {
			r.selectionPaths[stage] = candidates[0]
		}
	}
	for _, path := range paths {
		parts := strings.Split(path, "/")
		switch {
		case strings.HasPrefix(path, "selections/"):
			if execution != nil && (len(parts) < 3 || r.selectionPaths[parts[1]] != path) {
				r.ignored[path] = true
			}
		case strings.HasPrefix(path, "instances/"):
			if execution != nil && len(parts) >= 3 && !instances[parts[1]] {
				r.ignored[path] = true
			}
		case strings.HasPrefix(path, "outputs/"):
			if execution != nil && len(parts) >= 4 && !instances[parts[2]] {
				r.ignored[path] = true
			}
		case strings.HasPrefix(path, "stages/"), strings.HasPrefix(path, "sets/"):
			name := ""
			if len(parts) >= 2 {
				name = strings.TrimSuffix(parts[1], ".json")
			}
			if document != nil && !stages[name] {
				r.ignored[path] = true
			}
		case path == "expected-report.json", path == "trust.json", path == "README.md":
			r.ignored[path] = true
		}
	}
}

func (r *workflowReader) referenceInputs() (map[string]any, error) {
	if len(r.in.Files) > 65536 {
		return nil, workflowRefusal(codeOverBound)
	}
	paths := sortedKeys(r.in.Files)
	total := int64(0)
	for _, raw := range r.in.Files {
		total += int64(len(raw))
	}
	if total > 1<<30 {
		return nil, workflowRefusal(codeOverBound)
	}
	document := workflowReadObject(workflowDiscover(r.in.Files["workflow.json"], 262144, 64))
	execution := workflowReadObject(workflowDiscover(r.in.Files["execution.json"], 8<<20, 64))
	if n, ok := workflowReadObject(document["limits"])["maxBytes"].(json.Number); ok {
		if bound, err := n.Int64(); err == nil && bound >= 1 && bound <= 1<<30 && total > bound {
			return nil, workflowRefusal(codeOverBound)
		}
	}
	r.discoverCurrent(paths, document, execution)
	// Byte and nesting bounds apply globally, including independent trust
	// inputs. Historical artifacts count as supplied bytes but are not parsed.
	for _, path := range paths {
		if !r.ignored[path] && workflowOpaqueSignatures(path) && len(r.in.Files[path]) > maxSidecar {
			return nil, workflowRefusal(codeOverBound)
		}
		if !r.ignored[path] && workflowJSONArtifact(path) {
			limit, depth, _ := workflowFileDomain(path)
			if len(r.in.Files[path]) > limit || workflowDepthOver(r.in.Files[path], depth) {
				return nil, workflowRefusal(codeOverBound)
			}
		}
	}
	profileNames := sortedKeys(r.in.Profiles)
	for _, stage := range profileNames {
		raw := r.in.Profiles[stage]
		if len(raw) > maxProfilesDocument || workflowDepthOver(raw, maxDocumentDepth) {
			return nil, workflowRefusal(codeOverBound)
		}
	}
	for _, value := range workflowReadList(execution["instances"]) {
		instance := workflowReadObject(value)
		id, stage := workflowReadString(instance["id"]), workflowReadString(instance["stage"])
		for _, value := range workflowReadList(instance["attempts"]) {
			attempt := workflowReadObject(value)
			if attempt["preparationDigest"] == nil {
				continue
			}
			number := workflowReadString(attempt["number"])
			if n, ok := attempt["number"].(json.Number); ok {
				number = n.String()
			}
			base := "instances/" + id + "/attempts/" + number + "/"
			sources := r.in.Files[base+"sources.json"]
			if sources == nil {
				sources = []byte(`{}`)
			}
			if len(r.in.Files["stages/"+stage+"/dialect.json"])+len(r.in.Files["instances/"+id+"/case.json"])+len(sources) > MaxBody {
				return nil, workflowRefusal(codeOverBound)
			}
		}
	}
	parsed := map[string]any{}
	for _, path := range paths {
		if r.ignored[path] || !workflowJSONArtifact(path) {
			continue
		}
		_, depth, integer := workflowFileDomain(path)
		v, err := readInputJSONDepth(r.in.Files[path], depth)
		if err != nil {
			return nil, workflowRefusal(codeMalformed)
		}
		parsed[path] = v
		if integer {
			if _, err := derivation.Canon(r.in.Files[path]); err != nil {
				return nil, workflowRefusal(codeMalformed)
			}
		}
	}
	trusted := map[string]any{}
	for _, stage := range profileNames {
		raw := r.in.Profiles[stage]
		if raw == nil {
			raw = []byte(`[]`)
		}
		v, err := readInputJSONDepth(raw, maxDocumentDepth)
		if err != nil {
			return nil, workflowRefusal(codeMalformed)
		}
		trusted[stage] = v
	}
	// All count/string limits precede every selector and closed-form check.
	for _, path := range paths {
		if r.ignored[path] || !workflowJSONArtifact(path) {
			continue
		}
		limit, depth, integer := workflowFileDomain(path)
		if integer {
			if _, _, err := workflowValue(r.in.Files[path], limit, depth); err != nil {
				return nil, workflowReferenceError(err)
			}
		}
		if workflowReaderCountBounds(path, parsed[path], parsed["workflow.json"]) {
			return nil, workflowRefusal(codeOverBound)
		}
		if strings.HasSuffix(path, "/run.json") {
			// Decode only to measure the export's existing bounds; accepting its
			// closed form and versions is a later pass.
			var bundle VerificationBundle
			if json.Unmarshal(r.in.Files[path], &bundle) == nil && exportWithinLimits(r.in.Files[path], bundle) != nil {
				return nil, workflowRefusal(codeOverBound)
			}
		}
		if strings.HasSuffix(path, "/dialect.json") || strings.HasSuffix(path, "/bound-dialect.json") || strings.HasSuffix(path, "/preparation.json") || strings.HasSuffix(path, "/sources.json") || strings.HasSuffix(path, "/binding.json") || strings.HasSuffix(path, "/profiles.json") {
			if measureBytes(r.in.Files[path], depth) != nil {
				return nil, workflowRefusal(codeOverBound)
			}
		}
		if strings.HasSuffix(path, "/dialect.json") || strings.HasSuffix(path, "/bound-dialect.json") {
			parts := strings.Split(path, "/")
			stage := parts[1]
			if preparationCountBounds(parsed[path], nil, trusted[stage], parsed["stages/"+stage+"/binding.json"]) {
				return nil, workflowRefusal(codeOverBound)
			}
		}
		if strings.HasSuffix(path, "/profiles.json") && preparationCountBounds(nil, nil, parsed[path], nil) {
			return nil, workflowRefusal(codeOverBound)
		}
		if strings.HasSuffix(path, "/sources.json") {
			parts := strings.Split(path, "/")
			for _, value := range workflowReadList(workflowReadObject(parsed["execution.json"])["instances"]) {
				instance := workflowReadObject(value)
				if workflowReadString(instance["id"]) == parts[1] {
					stage := workflowReadString(instance["stage"])
					if preparationCountBounds(parsed["stages/"+stage+"/dialect.json"], parsed[path], trusted[stage], parsed["stages/"+stage+"/binding.json"]) {
						return nil, workflowRefusal(codeOverBound)
					}
				}
			}
		}
	}
	for _, stage := range profileNames {
		if measureBytes(r.in.Profiles[stage], maxDocumentDepth) != nil || preparationCountBounds(nil, nil, trusted[stage], nil) {
			return nil, workflowRefusal(codeOverBound)
		}
	}
	for _, path := range paths {
		if !r.ignored[path] && workflowReaderUnreadVersion(path, parsed[path]) {
			return nil, workflowRefusal("version-unread")
		}
	}
	for _, path := range paths {
		if !r.ignored[path] {
			if err := workflowReaderArtifactForm(path, r.in.Files[path], parsed, r.in.Files); err != nil {
				return nil, err
			}
		}
	}
	for _, stage := range profileNames {
		raw := r.in.Profiles[stage]
		if raw == nil {
			raw = []byte(`[]`)
		}
		if _, err := verifierProfiles(trusted[stage], raw); err != nil {
			return nil, workflowRefusal(codeMalformed)
		}
	}
	return parsed, nil
}

func (r *workflowReader) reference() error {
	parsed, err := r.referenceInputs()
	if err != nil {
		return err
	}
	rawWorkflow := r.file("workflow.json", true)
	rawExecution := r.file("execution.json", true)
	var doc workflowDocument
	var workflow, record []byte
	var execution workflowExecution
	limits := workflowLimits{MaxStages: 32, MaxRecords: 4096, MaxInstances: 4096, MaxAttempts: 16, MaxBytes: 1 << 30}
	if rawWorkflow != nil {
		doc, workflow, err = readWorkflow(rawWorkflow)
		if err != nil {
			return workflowReferenceError(err)
		}
		limits = doc.Limits
	}
	if rawExecution != nil {
		execution, record, err = readWorkflowExecution(rawExecution, limits)
		if err != nil {
			return workflowReferenceError(err)
		}
	}
	if rawWorkflow == nil || rawExecution == nil {
		return workflowRefusal("inventory-mismatch")
	}
	if len(execution.Frozen) != len(doc.Stages) || len(execution.OutputSets) != len(doc.Stages) || len(execution.Selections) != len(doc.Stages) {
		return workflowRefusal(codeMalformed)
	}
	r.workflow, r.execution = workflow, record
	r.w = workflowInventory{Document: doc, Execution: execution, Artifacts: map[string][]byte{}, Pending: map[string]int{}}
	r.w.Artifacts["workflow/"+execution.WorkflowDigest] = rawWorkflow
	r.file("workflow.sha256", true)
	r.file("execution.sha256", true)
	root := r.file("root-case.json", true)
	if root != nil {
		if _, _, err := workflowCase(root); err != nil {
			return workflowReferenceError(err)
		}
	}
	r.w.Artifacts["case/"+execution.RootCaseDigest] = root
	r.stages = make([]workflowReadStage, len(doc.Stages))
	for i, stage := range doc.Stages {
		frozen := execution.Frozen[i]
		if frozen.Stage != stage.ID || execution.OutputSets[i].Stage != stage.ID {
			return workflowRefusal(codeMalformed)
		}
		v := &r.stages[i]
		base := "stages/" + stage.ID + "/"
		v.published = r.file(base+"dialect.json", true)
		v.bound = r.file(base+"bound-dialect.json", true)
		v.pack = r.file(base+"pack.json", true)
		v.release = r.file(base+"release.json", true)
		v.recordedProfiles = r.file(base+"profiles.json", true)
		if frozen.BindingDocumentDigest != nil {
			v.binding = r.file(base+"binding.json", true)
		}
		if err := r.stageForm(i, parsed); err != nil {
			return err
		}
		for kind, artifact := range map[string]struct {
			pin string
			raw []byte
		}{"published": {frozen.PublishedDialectDigest, v.published}, "bound": {frozen.BoundDialectDigest, v.bound}, "pack": {frozen.PackDigest, v.pack}, "release": {frozen.ReleaseDigest, v.release}} {
			r.w.Artifacts[kind+"/"+artifact.pin] = artifact.raw
		}
		if frozen.BindingDocumentDigest != nil {
			r.w.Artifacts["binding/"+*frozen.BindingDocumentDigest] = v.binding
		}
		setRaw := r.file("sets/"+stage.ID+".json", true)
		if setRaw != nil {
			if strictJSON(setRaw, &v.set) != nil || !sameJSON(setRaw, encode(v.set)) || !workflowDigest.MatchString(v.set.WorkflowDigest) || !workflowID.MatchString(v.set.Execution) || !workflowID.MatchString(v.set.Stage) || v.set.Records == nil {
				return workflowRefusal(codeMalformed)
			}
			for _, pin := range v.set.Records {
				if !workflowDigest.MatchString(pin) {
					return workflowRefusal(codeMalformed)
				}
			}
		}
		if selectionPin := execution.Selections[i]; selectionPin != nil {
			prefix := "selections/" + stage.ID + "/"
			path := r.selectionPaths[stage.ID]
			if path == "" {
				r.missing = true
			} else {
				v.selectionRaw = r.file(path, true)
				selection, err := workflowSelectionForm(v.selectionRaw, doc.Limits.MaxRecords)
				if err != nil {
					return workflowReferenceError(err)
				}
				if path != fmt.Sprintf("%s%d.json", prefix, selection.Revision) {
					return workflowRefusal(codeMalformed)
				}
				v.selection = &selection
				r.w.Artifacts["selection/"+*selectionPin] = v.selectionRaw
			}
		}
	}
	r.attempts = make([][]workflowReadAttempt, len(execution.Instances))
	outputBases := map[string][]string{}
	for _, path := range sortedKeys(r.in.Files) {
		parts := strings.Split(path, "/")
		if r.ignored[path] || len(parts) != 5 || parts[0] != "outputs" || parts[4] != "record.json" {
			continue
		}
		index, err := strconv.Atoi(parts[3])
		if err != nil || index < 0 || index >= 4096 || strconv.Itoa(index) != parts[3] {
			continue
		}
		key := parts[1] + "/" + parts[2]
		outputBases[key] = append(outputBases[key], strings.TrimSuffix(path, "record.json"))
	}
	for _, bases := range outputBases {
		sort.Slice(bases, func(i, j int) bool {
			a, _ := strconv.Atoi(strings.Split(bases[i], "/")[3])
			b, _ := strconv.Atoi(strings.Split(bases[j], "/")[3])
			return a < b
		})
	}
	priorStage := -1
	for index, instance := range execution.Instances {
		stage := r.w.stageIndex(instance.Stage)
		if stage < priorStage || stage < 0 {
			return workflowRefusal(codeMalformed)
		}
		priorStage = stage
		if instance.CaseDigest != nil {
			caseBytes := r.file("instances/"+instance.ID+"/case.json", true)
			r.w.Artifacts["case/"+*instance.CaseDigest] = caseBytes
			if caseBytes != nil {
				if _, _, err := workflowCase(caseBytes); err != nil {
					return workflowReferenceError(err)
				}
			}
		}
		r.attempts[index] = make([]workflowReadAttempt, len(instance.Attempts))
		for j, attempt := range instance.Attempts {
			if err := r.attemptForm(index, j, attempt); err != nil {
				return err
			}
		}
		for _, base := range outputBases[instance.Stage+"/"+instance.ID] {
			raw := r.file(base+"record.json", true)
			output, _, err := readWorkflowOutput(raw)
			if err != nil {
				return workflowReferenceError(err)
			}
			payload := r.file(base+"payload.json", true)
			if payload != nil {
				if _, _, err := workflowCase(payload); err != nil {
					return workflowReferenceError(err)
				}
			}
			v := &r.stages[stage]
			pin := ""
			if len(v.outputs) < len(execution.OutputSets[stage].Records) {
				pin = execution.OutputSets[stage].Records[len(v.outputs)]
			}
			v.outputs = append(v.outputs, workflowReadOutput{output, raw, payload, base, pin})
			if pin != "" {
				r.w.Artifacts["output/"+pin] = raw
			}
			r.w.Artifacts["payload/"+output.PayloadDigest] = payload
		}
	}
	for i, v := range r.stages {
		if len(v.outputs) < len(execution.OutputSets[i].Records) {
			r.missing = true
		}
	}
	if r.missing {
		return workflowRefusal("inventory-mismatch")
	}
	for _, v := range r.stages {
		var dialect InputMapping
		_ = strictJSON(v.published, &dialect)
		for _, source := range dialect.Sources {
			if !verifiedKinds[source.Kind] {
				return workflowRefusal(codeKindUnread)
			}
		}
	}
	return nil
}
