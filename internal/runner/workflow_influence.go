package runner

// Workflow influence is separate from the unchanged preparation lineage. These
// helpers only accept a checked dialect and recomputed preparation lineage; an
// unchecked exported lineage or actor assertion is never an authority input.
import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

type workflowInfluence struct {
	Classes            []string `json:"classes"`
	GeneratedInfluence bool     `json:"generatedInfluence"`
}
type workflowTarget struct{ Kind, Target string }

var errWorkflowInfluence = errors.New("invalid workflow influence")
var errWorkflowInfluenceAdmission = errors.New("workflow inherited generated influence is not explicitly admitted")

func checkedWorkflowInfluence(in workflowInfluence) bool {
	if in.Classes == nil || len(in.Classes) > 3 {
		return false
	}
	previous := ""
	for _, class := range in.Classes {
		if !classValid(class) || class <= previous {
			return false
		}
		previous = class
	}
	return true
}
func mergeWorkflowInfluence(a, b workflowInfluence) workflowInfluence {
	classes := map[string]bool{}
	for _, class := range a.Classes {
		classes[class] = true
	}
	for _, class := range b.Classes {
		classes[class] = true
	}
	out := workflowInfluence{Classes: []string{}, GeneratedInfluence: a.GeneratedInfluence || b.GeneratedInfluence}
	for class := range classes {
		out.Classes = append(out.Classes, class)
	}
	sort.Strings(out.Classes)
	return out
}
func workflowOverlaps(a, b string) bool {
	at, ae := workflowPointerTokens(a, true)
	bt, be := workflowPointerTokens(b, true)
	return ae == nil && be == nil && (workflowPrefix(at, bt) || workflowPrefix(bt, at))
}

// inheritedWorkflowInfluence projects the parent summary onto exactly the
// copied case values, then conservatively follows declared parameter scopes.
// A parameter need not occur in request text or a rule to carry influence.
// This is computed before acquisition, not inferred from a result's values.
func inheritedWorkflowInfluence(m InputMapping, bindings []workflowBinding, payload []byte, parent workflowInfluence) (map[workflowTarget]workflowInfluence, error) {
	if !checkedWorkflowInfluence(parent) {
		return nil, errWorkflowInfluence
	}
	if err := validateMappingV2(m); err != nil {
		return nil, err
	}
	if _, err := projectWorkflowCase(payload, bindings); err != nil {
		return nil, err
	}
	value, _, err := workflowCase(payload)
	if err != nil {
		return nil, err
	}
	copied := []string{}
	for _, binding := range bindings {
		tokens, _ := workflowPointer(binding.From, true)
		if _, present := pointerRead(value, tokens); present {
			copied = append(copied, binding.To)
		}
	}
	affectedCase := func(pointer string) bool {
		for _, copy := range copied {
			if workflowOverlaps(pointer, copy) {
				return true
			}
		}
		return false
	}
	inherited := map[workflowTarget]workflowInfluence{}
	mark := func(kind, target string) {
		inherited[workflowTarget{kind, target}] = mergeWorkflowInfluence(workflowInfluence{Classes: []string{}}, parent)
	}
	caseParameter := false
	if m.Case != nil {
		for _, p := range m.Case.Parameters {
			caseParameter = caseParameter || affectedCase(p.Pointer)
		}
		for _, f := range m.Case.Facts {
			if affectedCase(f.Source) {
				mark("fact", f.Target)
			}
		}
		for _, e := range m.Case.Evidence {
			if affectedCase(e.Source) {
				mark("evidence", e.Requirement)
			}
		}
	}
	// Distinguish a fact from another source even if a caller supplies a dialect
	// with malformed dependency metadata: validateMappingV2 already rejects it.
	sourceFacts := map[string][]string{}
	for _, source := range m.Sources {
		affected := caseParameter
		for _, p := range source.Parameters {
			for _, target := range sourceFacts[p.From] {
				if workflowOverlaps(p.Pointer, target) {
					affected = true
				}
			}
		}
		facts, evidence, err := readTargets(source.Read)
		if err != nil {
			return nil, err
		}
		if affected {
			for _, f := range facts {
				mark("fact", f.Target)
				sourceFacts[source.Name] = append(sourceFacts[source.Name], f.Target)
			}
			for _, e := range evidence {
				mark("evidence", e.Requirement)
			}
		}
	}
	// The ordinary source class is still checked by the existing dialect
	// admission. Inherited generation adds this explicit, pre-acquisition gate.
	for target, influence := range inherited {
		if !influence.GeneratedInfluence {
			continue
		}
		var classes []string
		if m.Admits != nil {
			if target.Kind == "fact" {
				classes = m.Admits.Facts[target.Target]
			} else {
				classes = m.Admits.Evidence[target.Target]
			}
		}
		allowed := false
		for _, class := range classes {
			allowed = allowed || class == "generated"
		}
		if !allowed {
			return nil, errWorkflowInfluenceAdmission
		}
	}
	return inherited, nil
}

// outputWorkflowInfluence includes every overlapping fact contributor, even
// absent/unused contributors to a container. A later copy of one field carries
// the parent envelope's whole summary, rather than laundering that container.
func outputWorkflowInfluence(pointer string, lineage []TargetLineage, inherited map[workflowTarget]workflowInfluence) (workflowInfluence, error) {
	out := workflowInfluence{Classes: []string{}}
	if _, err := workflowOutputPointer(pointer); err != nil {
		return out, err
	}
	for _, entry := range lineage {
		if entry.Kind != "fact" && entry.Kind != "evidence" {
			return out, errWorkflowInfluence
		}
		if !classValid(entry.Class) {
			return out, errWorkflowInfluence
		}
		if entry.Kind != "fact" {
			continue
		}
		if _, err := workflowPointer(entry.Target, false); err != nil {
			return out, err
		}
		if workflowOverlaps(pointer, entry.Target) {
			out = mergeWorkflowInfluence(out, workflowInfluence{[]string{entry.Class}, entry.GeneratedInfluence})
		}
	}
	for target, influence := range inherited {
		if !checkedWorkflowInfluence(influence) || (target.Kind != "fact" && target.Kind != "evidence") {
			return out, errWorkflowInfluence
		}
		if target.Kind == "fact" {
			if _, err := workflowPointer(target.Target, false); err != nil {
				return out, err
			}
			if workflowOverlaps(pointer, target.Target) {
				out = mergeWorkflowInfluence(out, influence)
			}
		}
	}
	return out, nil
}

// A full output facts pointer is either the declared pointer or that pointer
// followed by one canonical array index. Bounds on declarations stay intact.
func workflowOutputPointer(pointer string) ([]string, error) {
	parts, err := workflowPointer(pointer, false)
	if err == nil {
		return parts, nil
	}
	if len(pointer) > 1029 { // 1024 bytes plus /4095, the largest output index
		return nil, errWorkflowBound
	}
	at := strings.LastIndexByte(pointer, '/')
	if at <= 0 {
		return nil, err
	}
	index := pointer[at+1:]
	n, indexErr := strconv.Atoi(index)
	if indexErr != nil || n < 0 || n >= 4096 || strconv.Itoa(n) != index {
		return nil, errWorkflowForm
	}
	if _, baseErr := workflowPointer(pointer[:at], false); baseErr != nil {
		return nil, baseErr
	}
	return workflowPointerTokens(pointer, false)
}
