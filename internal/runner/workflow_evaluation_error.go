package runner

// Reads the pinned Runtime output-v2 envelope; it does not infer a Core class
// from diagnostic text, a process exit status or a missing result. Keep exact
// stdout bytes separately, including their final newline, after this check.
func workflowEvaluationError(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maxOutput {
		return false
	}
	value, err := readInputJSONDepth(raw, 64)
	if err != nil {
		return false
	}
	object, ok := workflowObject(value, "outputVersion", "tool", "command", "status", "evaluationError", "diagnostics")
	if !ok || object["outputVersion"] != "2" || object["command"] != "experimental evaluate" || (object["status"] != "error" && object["status"] != "unsupported") {
		return false
	}
	tool, ok := workflowObject(object["tool"], "name", "version")
	if !ok {
		return false
	}
	if _, ok := tool["name"].(string); !ok {
		return false
	}
	if _, ok := tool["version"].(string); !ok {
		return false
	}
	failure, ok := workflowObject(object["evaluationError"], "class", "phase", "evaluatorSpecVersion")
	if !ok || failure["evaluatorSpecVersion"] != "0.2.0-draft" {
		return false
	}
	switch failure["class"] {
	case "pack-not-conformant", "malformed-input", "unsupported-required-extension":
		if failure["phase"] != "preflight" {
			return false
		}
	case "resource-exhaustion":
		if failure["phase"] != "preflight" && failure["phase"] != "evaluation" {
			return false
		}
	default:
		return false
	}
	diagnostics, ok := object["diagnostics"].([]any)
	if !ok {
		return false
	}
	for _, value := range diagnostics {
		diagnostic, ok := workflowObject(value, "code", "codeStability", "layer", "severity", "instancePath", "message")
		if !ok {
			return false
		}
		for _, value := range diagnostic {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	return true
}
