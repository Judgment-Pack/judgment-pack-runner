package runner

// This file implements the document/projection portion of draft RFC 0020.
// It does not admit a run, authenticate a selection, or verify an execution.
// Keep it private until the executor and offline reader can enforce the rest.
import (
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
)

var workflowID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var workflowDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type workflowLimits struct {
	MaxStages    int `json:"maxStages"`
	MaxRecords   int `json:"maxRecords"`
	MaxInstances int `json:"maxInstances"`
	MaxAttempts  int `json:"maxAttempts"`
	MaxBytes     int `json:"maxBytes"`
}
type workflowBinding struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}
type workflowRelation struct {
	Label     string `json:"label"`
	Direction string `json:"direction"`
}
type workflowOutputs struct {
	Pointer     string `json:"pointer"`
	Cardinality string `json:"cardinality"`
}
type workflowRule struct {
	Kind      string `json:"kind"`
	OutcomeID string `json:"outcomeId,omitempty"`
}
type workflowSelectionPolicy struct {
	Mode string        `json:"mode"`
	Rule *workflowRule `json:"rule,omitempty"`
}
type workflowDialectRef struct {
	FormatVersion string          `json:"formatVersion,omitempty"`
	ID            string          `json:"id,omitempty"`
	Version       json.RawMessage `json:"version"`
	Digest        string          `json:"digest"`
}

type workflowStage struct {
	ID      string             `json:"id"`
	Dialect workflowDialectRef `json:"dialect"`
	Pack    struct {
		ID     string `json:"id"`
		Digest string `json:"digest"`
	} `json:"pack"`
	Input struct {
		Root      bool   `json:"root,omitempty"`
		FromStage string `json:"fromStage,omitempty"`
		Selected  bool   `json:"selected,omitempty"`
	} `json:"input"`
	Execution    string                   `json:"execution"`
	CaseBindings []workflowBinding        `json:"caseBindings"`
	Outputs      workflowOutputs          `json:"outputs"`
	Relation     workflowRelation         `json:"relation"`
	Selection    *workflowSelectionPolicy `json:"selection,omitempty"`
}
type workflowDocument struct {
	WorkflowVersion int             `json:"workflowVersion"`
	Stages          []workflowStage `json:"stages"`
	Result          string          `json:"result"`
	Limits          workflowLimits  `json:"limits"`
}

var errWorkflowForm = errors.New("invalid draft workflow document")
var errWorkflowBound = errors.New("draft workflow value exceeds a bound")
var errWorkflowVersion = errors.New("unread draft workflow version")
var errWorkflowProjection = errors.New("workflow case projection failed")
var errWorkflowOutput = errors.New("workflow output projection failed")

// workflowValue holds the workflow's integer-only JSON domain, count/string
// limits and canonical byte limit. Case/payload serialization remains literal
// preserving: callers use encode(v), never the normalized canonical bytes.
func workflowValue(raw []byte, byteLimit, depthLimit int) (any, []byte, error) {
	if len(raw) > byteLimit || workflowDepthOver(raw, depthLimit) {
		return nil, nil, errWorkflowBound
	}
	v, err := readInputJSONDepth(raw, depthLimit+1)
	if err != nil {
		return nil, nil, errWorkflowForm
	}
	var bounded func(any, int) bool
	bounded = func(v any, parents int) bool {
		switch x := v.(type) {
		case map[string]any:
			if parents >= depthLimit || len(x) > 4096 {
				return false
			}
			for k, value := range x {
				if len(k) > 65536 || !bounded(value, parents+1) {
					return false
				}
			}
		case []any:
			if parents >= depthLimit || len(x) > 4096 {
				return false
			}
			for _, value := range x {
				if !bounded(value, parents+1) {
					return false
				}
			}
		case string:
			return len(x) <= 65536
		case json.Number:
			return len(x) <= 128
		}
		return true
	}
	if !bounded(v, 0) {
		return nil, nil, errWorkflowBound
	}
	canonical, err := derivation.Canon(raw)
	if err != nil {
		return nil, nil, errWorkflowForm
	}
	if len(canonical) > byteLimit {
		return nil, nil, errWorkflowBound
	}
	return v, canonical, nil
}

func workflowObject(v any, required ...string) (map[string]any, bool) {
	o, ok := v.(map[string]any)
	if !ok || len(o) != len(required) {
		return nil, false
	}
	for _, k := range required {
		if _, ok := o[k]; !ok {
			return nil, false
		}
	}
	return o, true
}
func workflowPointer(p string, root bool) ([]string, error) {
	if len(p) > 1024 {
		return nil, errWorkflowForm
	}
	if strings.HasPrefix(p, "/") && strings.Count(p, "/") > 32 {
		return nil, errWorkflowBound
	}
	return workflowPointerTokens(p, root)
}

// Syntax is separate from declaration bounds: a many-output record appends
// its index to a valid declared pointer, which may already be at its bound.
func workflowPointerTokens(p string, root bool) ([]string, error) {
	if p == "" && root {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, errWorkflowForm
	}
	parts := strings.Split(p[1:], "/")
	for i, p := range parts {
		for at := 0; at < len(p); at++ {
			if p[at] == '~' {
				if at+1 == len(p) || (p[at+1] != '0' && p[at+1] != '1') {
					return nil, errWorkflowForm
				}
				at++
			}
		}
		// Go object maps have no prototype members. RFC 0020 admits these
		// ordinary names; the legacy form editor's field restrictions do not.
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return parts, nil
}
func workflowPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func checkWorkflowBindings(bindings []workflowBinding) error {
	if len(bindings) < 1 || len(bindings) > 64 {
		return errWorkflowForm
	}
	var destinations [][]string
	for _, b := range bindings {
		if _, err := workflowPointer(b.From, true); err != nil {
			return err
		}
		to, err := workflowPointer(b.To, false)
		if err != nil {
			return err
		}
		switch b.Type {
		case "string", "integer", "timestamp", "boolean", "object", "array", "null":
		default:
			return errWorkflowForm
		}
		for _, old := range destinations {
			if workflowPrefix(old, to) || workflowPrefix(to, old) {
				return errWorkflowForm
			}
		}
		destinations = append(destinations, to)
	}
	return nil
}

// readWorkflow checks only the document's intrinsic contract. Matching pack
// outcomes, artifact pins, bindings and generated influence requires freezing
// and is deliberately not claimed here. Unknown dialect versions stay unread.
func readWorkflow(raw []byte) (workflowDocument, []byte, error) {
	var d workflowDocument
	v, canonical, err := workflowValue(raw, 262144, 64)
	if err != nil {
		return d, nil, err
	}
	o, ok := v.(map[string]any)
	if !ok {
		return d, nil, errWorkflowForm
	}
	if version, exists := o["workflowVersion"]; exists && version != json.Number("1") {
		return d, nil, errWorkflowVersion
	}
	stages, ok := o["stages"].([]any)
	if !ok {
		return d, nil, errWorkflowForm
	}
	for _, rawStage := range stages {
		if stage, ok := rawStage.(map[string]any); ok {
			if pin, ok := stage["dialect"].(map[string]any); ok {
				if format, named := pin["formatVersion"]; named {
					if format != "2.0.0" && format != "3.0.0" {
						return d, nil, errWorkflowVersion
					}
				} else if version, exists := pin["version"]; exists && version != json.Number("2") && version != json.Number("3") {
					return d, nil, errWorkflowVersion
				}
			}
		}
	}
	if _, ok = workflowObject(o, "workflowVersion", "stages", "result", "limits"); !ok {
		return d, nil, errWorkflowForm
	}
	if _, ok := workflowObject(o["limits"], "maxStages", "maxRecords", "maxInstances", "maxAttempts", "maxBytes"); !ok {
		return d, nil, errWorkflowForm
	}
	if err := json.Unmarshal(canonical, &d); err != nil {
		return d, nil, errWorkflowForm
	}
	l := d.Limits
	if l.MaxStages < 1 || l.MaxStages > 32 || l.MaxRecords < 1 || l.MaxRecords > 4096 || l.MaxInstances < 1 || l.MaxInstances > 4096 || l.MaxAttempts < 1 || l.MaxAttempts > 16 || l.MaxBytes < 1 || l.MaxBytes > 1<<30 || len(d.Stages) < 1 || len(d.Stages) > l.MaxStages {
		return d, nil, errWorkflowBound
	}
	seen, consumed := map[string]bool{}, map[string]bool{}
	for i, stage := range d.Stages {
		keys := []string{"id", "dialect", "pack", "input", "execution", "caseBindings", "outputs", "relation"}
		if stage.Selection != nil {
			keys = append(keys, "selection")
		}
		s, ok := workflowObject(stages[i], keys...)
		if !ok || !workflowID.MatchString(stage.ID) || seen[stage.ID] {
			return d, nil, errWorkflowForm
		}
		dialectKeys := []string{"version", "digest"}
		if stage.Dialect.FormatVersion != "" {
			dialectKeys = append(dialectKeys, "formatVersion", "id")
			var version string
			if json.Unmarshal(stage.Dialect.Version, &version) != nil || !artifactVersionGrammar.MatchString(version) || stage.Dialect.ID == "" || len(stage.Dialect.ID) > 1024 || strings.IndexFunc(stage.Dialect.ID, func(c rune) bool { return c <= 32 || c == 127 }) >= 0 {
				return d, nil, errWorkflowForm
			}
		}
		if _, ok := workflowObject(s["dialect"], dialectKeys...); !ok {
			return d, nil, errWorkflowForm
		}
		if _, ok := workflowObject(s["pack"], "id", "digest"); !ok {
			return d, nil, errWorkflowForm
		}
		if !workflowURI(stage.Pack.ID) || !workflowDigest.MatchString(stage.Pack.Digest) || !workflowDigest.MatchString(stage.Dialect.Digest) {
			return d, nil, errWorkflowForm
		}
		bs, ok := s["caseBindings"].([]any)
		if !ok {
			return d, nil, errWorkflowForm
		}
		for _, b := range bs {
			obj, ok := workflowObject(b, "from", "to", "type", "required")
			if !ok {
				return d, nil, errWorkflowForm
			}
			for _, key := range []string{"from", "to", "type"} {
				if _, ok := obj[key].(string); !ok {
					return d, nil, errWorkflowForm
				}
			}
			if _, ok := obj["required"].(bool); !ok {
				return d, nil, errWorkflowForm
			}
		}
		if i == 0 {
			if _, ok := workflowObject(s["input"], "root"); !ok || !stage.Input.Root || stage.Execution != "once" || len(bs) != 0 {
				return d, nil, errWorkflowForm
			}
		} else {
			if _, ok := workflowObject(s["input"], "fromStage", "selected"); !ok || !seen[stage.Input.FromStage] || !stage.Input.Selected || stage.Execution != "each-selected-record" {
				return d, nil, errWorkflowForm
			}
			if err := checkWorkflowBindings(stage.CaseBindings); err != nil {
				return d, nil, err
			}
			consumed[stage.Input.FromStage] = true
		}
		if _, ok := workflowObject(s["outputs"], "pointer", "cardinality"); !ok {
			return d, nil, errWorkflowForm
		}
		if _, err := workflowPointer(stage.Outputs.Pointer, false); err != nil {
			return d, nil, err
		}
		if stage.Outputs.Cardinality != "one" && stage.Outputs.Cardinality != "many" {
			return d, nil, errWorkflowForm
		}
		if _, ok := workflowObject(s["relation"], "label", "direction"); !ok || !workflowID.MatchString(stage.Relation.Label) || (stage.Relation.Direction != "output-to-parent" && stage.Relation.Direction != "parent-to-output") {
			return d, nil, errWorkflowForm
		}
		if policy := stage.Selection; policy != nil {
			switch policy.Mode {
			case "reviewer":
				if _, ok := workflowObject(s["selection"], "mode"); !ok {
					return d, nil, errWorkflowForm
				}
			case "disposition":
				sel, ok := workflowObject(s["selection"], "mode", "rule")
				if !ok || policy.Rule == nil || stage.Outputs.Cardinality != "one" {
					return d, nil, errWorkflowForm
				}
				rkeys := []string{"kind"}
				switch policy.Rule.Kind {
				case "outcome":
					rkeys = append(rkeys, "outcomeId")
					if len(policy.Rule.OutcomeID) < 1 || len(policy.Rule.OutcomeID) > 256 {
						return d, nil, errWorkflowForm
					}
				case "unresolved", "not-applicable":
				default:
					return d, nil, errWorkflowForm
				}
				if _, ok := workflowObject(sel["rule"], rkeys...); !ok {
					return d, nil, errWorkflowForm
				}
			default:
				return d, nil, errWorkflowForm
			}
		}
		seen[stage.ID] = true
	}
	for _, stage := range d.Stages {
		if consumed[stage.ID] != (stage.Selection != nil) {
			return d, nil, errWorkflowForm
		}
	}
	if !seen[d.Result] || consumed[d.Result] {
		return d, nil, errWorkflowForm
	}
	return d, canonical, nil
}

func workflowTyped(v any, kind string) bool {
	switch kind {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		_, ok := v.(json.Number)
		return ok // workflowValue checked domain
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "timestamp":
		s, ok := v.(string)
		if !ok || len(s) != 20 {
			return false
		}
		t, err := time.Parse("2006-01-02T15:04:05Z", s)
		return err == nil && t.Format("2006-01-02T15:04:05Z") == s
	}
	return false
}

func workflowCase(raw []byte) (any, []byte, error) {
	v, _, err := workflowValue(raw, 1<<20, 32)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, nil, errWorkflowForm
	}
	b := encode(v) // dialect §9.5: retains -0; escapes HTML and U+2028/2029
	if len(b) > 1<<20 {
		return nil, nil, errWorkflowBound
	}
	return v, b, nil
}

// projectWorkflowCase copies only declared values. An optional absent value
// is omitted; a present null or mistyped optional value is never discarded.
func projectWorkflowCase(payload []byte, bindings []workflowBinding) ([]byte, error) {
	if err := checkWorkflowBindings(bindings); err != nil {
		return nil, err
	}
	v, _, err := workflowCase(payload)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	for _, b := range bindings {
		from, _ := workflowPointer(b.From, true)
		value, present := pointerRead(v, from)
		if !present {
			if b.Required {
				return nil, errWorkflowProjection
			}
			continue
		}
		if !workflowTyped(value, b.Type) {
			return nil, errWorkflowProjection
		}
		to, _ := workflowPointer(b.To, false)
		parent := result
		for _, token := range to[:len(to)-1] {
			if parent[token] == nil {
				parent[token] = map[string]any{}
			}
			parent = parent[token].(map[string]any)
		}
		parent[to[len(to)-1]] = value
	}
	_, raw, err := workflowCase(encode(result))
	return raw, err
}

// outputsWorkflow reads the prepared facts. Missing/null is not an empty
// result. This checks payload shape only; execution checks lineage and bounds.
func outputsWorkflow(facts []byte, output workflowOutputs) ([][]byte, error) {
	parts, err := workflowPointer(output.Pointer, false)
	if err != nil {
		return nil, err
	}
	// Prepared facts have their own number domain. Only selected output
	// payloads enter the workflow's integer domain and 1 MiB object bound.
	if len(facts) > maxOutput {
		return nil, errWorkflowBound
	}
	v, err := readInputJSONDepth(facts, 64)
	if err != nil {
		return nil, errWorkflowForm
	}
	value, exists := pointerRead(v, parts)
	if !exists {
		return nil, errWorkflowOutput
	}
	var values []any
	switch output.Cardinality {
	case "one":
		values = []any{value}
	case "many":
		var ok bool
		values, ok = value.([]any)
		if !ok {
			return nil, errWorkflowOutput
		}
	default:
		return nil, errWorkflowForm
	}
	if len(values) > 4096 {
		return nil, errWorkflowBound
	}
	result := make([][]byte, 0, len(values))
	for _, value := range values {
		_, raw, err := workflowCase(encode(value))
		if err != nil {
			if errors.Is(err, errWorkflowBound) {
				return nil, err
			}
			return nil, errWorkflowOutput
		}
		result = append(result, raw)
	}
	return result, nil
}

// Count containers before parsing, so a deeply nested malformed document has
// the same bound classification as a valid over-bound one (draft §9).
func workflowDepthOver(raw []byte, limit int) bool {
	depth, quoted := 0, false
	for i := 0; i < len(raw); i++ {
		if quoted {
			if raw[i] == '\\' {
				i++
			} else if raw[i] == '"' {
				quoted = false
			}
			continue
		}
		switch raw[i] {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > limit {
				return true
			}
		case '}', ']':
			depth--
		}
	}
	return false
}

var workflowScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
var workflowIPFuture = regexp.MustCompile(`^[vV][0-9A-Fa-f]+\.[A-Za-z0-9._~!$&'()*+,;=:-]+$`)

// RFC 3986 URI (not IRI). net/url alone accepts raw Unicode, bad opaque
// percent escapes and characters outside each component's grammar.
func workflowURI(uri string) bool {
	if len(uri) < 1 || len(uri) > 2048 {
		return false
	}
	scheme, rest, ok := strings.Cut(uri, ":")
	if !ok || !workflowScheme.MatchString(scheme) {
		return false
	}
	const base = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~!$&'()*+,;="
	component := func(s, extra string) bool {
		for i := 0; i < len(s); i++ {
			if s[i] == '%' {
				if i+2 >= len(s) || !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i+1])) || !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i+2])) {
					return false
				}
				i += 2
				continue
			}
			if !strings.ContainsRune(base+extra, rune(s[i])) {
				return false
			}
		}
		return true
	}
	hierarchy, fragment, hasFragment := strings.Cut(rest, "#")
	if hasFragment && !component(fragment, ":@/?") {
		return false
	}
	hierarchy, query, hasQuery := strings.Cut(hierarchy, "?")
	if hasQuery && !component(query, ":@/?") {
		return false
	}
	if strings.HasPrefix(hierarchy, "//") {
		authority, path, _ := strings.Cut(hierarchy[2:], "/")
		if !component(path, ":@/") {
			return false
		}
		host := authority
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			if !component(host[:at], ":") {
				return false
			}
			host = host[at+1:]
		}
		if strings.HasPrefix(host, "[") {
			close := strings.IndexByte(host, ']')
			if close < 0 {
				return false
			}
			address := host[1:close]
			if (net.ParseIP(address) == nil || !strings.Contains(address, ":")) && !workflowIPFuture.MatchString(address) {
				return false
			}
			host = host[close+1:]
			if host != "" && !strings.HasPrefix(host, ":") {
				return false
			}
		} else {
			name, port, hasPort := strings.Cut(host, ":")
			if !component(name, "") {
				return false
			}
			host = ""
			if hasPort {
				host = ":" + port
			}
		}
		if host != "" {
			for _, r := range host[1:] {
				if r < '0' || r > '9' {
					return false
				}
			}
		}
		return true
	}
	return component(hierarchy, ":@/")
}
