package dialect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/dialect4"
)

// Format is the one dialect format this package reads.
const Format = dialect4.Format

// The codes an Error carries. The first five are spec/dialect.md §11's;
// sources-unread is this package's, for a document it does not prepare.
const (
	CodeMalformed          = dialect4.CodeMalformed
	CodeOverBound          = dialect4.CodeOverBound
	CodeParameter          = dialect4.CodeParameter
	CodeReadFailed         = dialect4.CodeReadFailed
	CodeAdmission          = "admission-refused"
	CodeProfileUnsupported = "profile-unsupported"
	CodeVersionUnread      = "version-unread"
	CodeSourcesUnread      = "sources-unread"
)

// Error is a refusal or a finding: its code, and a sentence saying why. A
// refusal (input-malformed, input-over-bound, profile-unsupported,
// version-unread, sources-unread) is of a document the package does not
// prepare through; a finding (parameter-invalid, read-failed,
// admission-refused) is of one case.
type Error struct {
	Code   string
	Reason string
}

func (e *Error) Error() string { return "dialect: " + e.Code + ": " + e.Reason }

func refuse(code, reason string) *Error { return &Error{Code: code, Reason: reason} }

func fromEngine(err error) *Error {
	var e *dialect4.Error
	if errors.As(err, &e) {
		return refuse(e.Code, e.Reason)
	}
	return refuse(CodeMalformed, err.Error())
}

// The bounds of spec/dialect.md §12 this package measures, of the base
// profile.
const (
	maxDialectBytes   = 131072
	maxEnvelopeBytes  = 2097152
	maxDocumentDepth  = 64
	maxValueDepth     = 32
	maxScope          = 64
	maxFactTargets    = 256
	maxEvidenceTarget = 128
)

// Parameter names are the Runner's: a letter, then up to 63 letters, digits,
// hyphens and underscores.
var parameterName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

var artifactVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type parameter struct {
	name, pointer, typ string
}

type copyEntry struct {
	target, source string
}

// Dialect is a document of format 4.0.0 with no source, held to its form.
// It is read-only, and safe for concurrent use.
type Dialect struct {
	document     any
	size         int
	digest       string
	readDigest   string
	parameters   []parameter
	facts        []copyEntry
	evidence     []copyEntry
	rule         *dialect4.Rule
	admits       map[[2]string][]string
	ruleFacts    []string
	ruleEvidence []string
}

// Read holds document, a dialect's bytes, to the form of format 4.0.0
// (spec/dialect.md §2, §2.2), in the order of §11's phases: the profile it
// names, its bytes (at most 128 KiB, nested at most 64 levels, every number
// literal bounded), its reading as one strict JSON document, its counts, its
// format and then its form. It returns an *Error for a document it does not
// prepare through.
func Read(document []byte) (*Dialect, error) {
	if len(document) <= 2*maxDialectBytes {
		if v, err := dialect4.ReadJSON(document, maxDocumentDepth); err == nil {
			if top, ok := v.(map[string]any); ok {
				if p, named := top["profile"]; named {
					switch p {
					case "base":
					case "extended":
						return nil, refuse(CodeProfileUnsupported, "the extended profile is not read until the Spec decides its literal bound (spec issue #324): this package reads the base profile")
					default:
						return nil, refuse(CodeMalformed, "the document's profile is not base or extended")
					}
				}
			}
		}
	}
	if len(document) > maxDialectBytes {
		return nil, refuse(CodeOverBound, "the dialect is over 131072 bytes")
	}
	if err := dialect4.Measure(document, maxDocumentDepth); err != nil {
		return nil, fromEngine(err)
	}
	v, err := dialect4.ReadJSON(document, maxDocumentDepth)
	if err != nil {
		return nil, refuse(CodeMalformed, "the dialect is not one JSON document read strictly: "+err.Error())
	}
	top, isObject := v.(map[string]any)
	if _, ok := top["sources"]; ok {
		return nil, refuse(CodeSourcesUnread, "the dialect names sources, which this package does not prepare: prepare it through the Runner")
	}
	if over(top) {
		return nil, refuse(CodeOverBound, "the dialect exceeds a counted bound of spec/dialect.md §12 or §2.2.7")
	}
	format, named := top["formatVersion"]
	switch {
	case !isObject:
		return nil, refuse(CodeMalformed, "the dialect is not an object")
	case named && (format == "2.0.0" || format == "3.0.0"):
		return nil, refuse(CodeVersionUnread, "the dialect is of an earlier format: this package reads format 4.0.0 alone")
	case !named && (dialect4.IsInteger(top["version"]) && (string(top["version"].(json.Number)) == "2" || string(top["version"].(json.Number)) == "3")):
		return nil, refuse(CodeVersionUnread, "the dialect is of an earlier version: this package reads format 4.0.0 alone")
	case format != Format:
		return nil, refuse(CodeMalformed, "the dialect names no format this document defines")
	}
	d, err := form(top)
	if err != nil {
		return nil, fromEngine(err)
	}
	d.document, d.size = top, len(document)
	return d, nil
}

// over is §11's third phase for a document with no source: the case's
// scope, its rule's counts (§2.2.7) and the targets (§12).
func over(top map[string]any) bool {
	c, _ := top["case"].(map[string]any)
	params, _ := c["parameters"].(map[string]any)
	if len(params)+1 > maxScope {
		return true
	}
	facts, evidence := map[string]bool{}, map[string]bool{}
	entries := func(v any) []any { a, _ := v.([]any); return a }
	for _, e := range entries(c["facts"]) {
		entry, _ := e.(map[string]any)
		if t, ok := entry["target"].(string); ok {
			facts[t] = true
		}
	}
	for _, e := range entries(c["evidence"]) {
		entry, _ := e.(map[string]any)
		if r, ok := entry["requirement"].(string); ok {
			evidence[r] = true
		}
	}
	if top["formatVersion"] == Format && dialect4.RuleCounts(c["rule"], facts, evidence) {
		return true
	}
	return len(facts) > maxFactTargets || len(evidence) > maxEvidenceTarget
}

func closed(v any, required, optional []string, what string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, refuse(CodeMalformed, what+" is not an object")
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, refuse(CodeMalformed, what+" lacks "+k)
		}
	}
	allowed := map[string]bool{}
	for _, k := range append(append([]string{}, required...), optional...) {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return nil, refuse(CodeMalformed, what+" has a member its form does not have")
		}
	}
	return m, nil
}

func pointer(v any, root bool) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	_, err := dialect4.Tokens(s, root)
	return s, err == nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func overlap(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// form is §11's fifth phase for a document of format 4.0.0 with no source.
func form(top map[string]any) (*Dialect, error) {
	malformed := func(reason string) error { return refuse(CodeMalformed, reason) }
	if _, err := closed(top, []string{"formatVersion", "id", "version", "case"}, []string{"profile", "admits", "unmapped", "unmappedEvidence"}, "the dialect"); err != nil {
		return nil, err
	}
	id, ok := top["id"].(string)
	if !ok || id == "" || len(id) > 1024 || strings.IndexFunc(id, func(c rune) bool { return c <= 32 || c == 127 }) >= 0 {
		return nil, malformed("the dialect id is not a bounded opaque identifier")
	}
	if version, ok := top["version"].(string); !ok || !artifactVersion.MatchString(version) {
		return nil, malformed("the dialect artifact version is not a three-component version")
	}
	for _, member := range []string{"unmapped", "unmappedEvidence"} {
		if a, ok := top[member].([]any); ok && len(a) == 0 {
			return nil, malformed(member + " is written empty, which the format leaves out")
		}
	}
	canonical, err := dialect4.Canon(top)
	if err != nil {
		return nil, malformed("the dialect is outside the canon domain")
	}
	c, err := closed(top["case"], []string{"facts", "evidence"}, []string{"parameters", "rule"}, "the case")
	if err != nil {
		return nil, err
	}
	d := &Dialect{digest: digestOf(canonical), admits: map[[2]string][]string{}}
	scope := map[string]string{"runAt": dialect4.TypeTimestamp}
	if raw, ok := c["parameters"]; ok {
		params, ok := raw.(map[string]any)
		if !ok {
			return nil, malformed("case parameters are not an object")
		}
		if len(params) == 0 {
			return nil, malformed("case parameters are written empty, which the format leaves out")
		}
		for _, name := range sortedKeys(params) {
			if !parameterName.MatchString(name) || name == "runAt" {
				return nil, malformed("a case parameter name is invalid or reserved")
			}
			p, err := closed(params[name], []string{"pointer", "type"}, nil, "case parameter "+name)
			if err != nil {
				return nil, err
			}
			at, ok1 := pointer(p["pointer"], true)
			typ, ok2 := p["type"].(string)
			if !ok1 || !ok2 || !dialect4.KnownType(typ) {
				return nil, malformed("case parameter " + name + " is not of its form")
			}
			d.parameters = append(d.parameters, parameter{name, at, typ})
			scope[name] = typ
		}
	}
	facts, ok1 := c["facts"].([]any)
	evidence, ok2 := c["evidence"].([]any)
	if !ok1 || !ok2 {
		return nil, malformed("the case's copy facts and evidence are arrays")
	}
	for _, e := range facts {
		entry, err := closed(e, []string{"target", "source"}, nil, "a case copy fact")
		if err != nil {
			return nil, err
		}
		target, ok1 := pointer(entry["target"], false)
		source, ok2 := pointer(entry["source"], true)
		if !ok1 || !ok2 {
			return nil, malformed("a case copy fact's target or source is not a pointer")
		}
		d.facts = append(d.facts, copyEntry{target, source})
	}
	for _, e := range evidence {
		entry, err := closed(e, []string{"requirement", "source"}, nil, "a case copy evidence entry")
		if err != nil {
			return nil, err
		}
		requirement, ok1 := entry["requirement"].(string)
		source, ok2 := pointer(entry["source"], true)
		if !ok1 || requirement == "" || len(requirement) > 256 || !ok2 {
			return nil, malformed("a case copy evidence entry is not of its form")
		}
		d.evidence = append(d.evidence, copyEntry{requirement, source})
	}
	read := map[string]any{"copy": map[string]any{"facts": facts, "evidence": evidence}}
	if raw, ok := c["rule"]; ok {
		if !dialect4.IsRule2(raw) {
			return nil, malformed("the case's rule is not of ruleVersion \"2.0.0\"")
		}
		rule, err := dialect4.CheckRule(raw, scope, "the case's rule")
		if err != nil {
			return nil, err
		}
		d.rule = rule
		read["rule"] = raw
		d.ruleFacts = append([]string(nil), rule.FactTargets...)
		d.ruleEvidence = append([]string(nil), rule.EvidenceTargets...)
		sort.Strings(d.ruleFacts)
		sort.Strings(d.ruleEvidence)
	}
	readCanonical, err := dialect4.Canon(read)
	if err != nil {
		return nil, malformed("the case's read is outside the canon domain")
	}
	d.readDigest = digestOf(readCanonical)

	// §8.1: no two fact targets overlap, and no requirement is the target
	// of two reads.
	var targets []string
	for _, f := range d.facts {
		targets = append(targets, f.target)
	}
	targets = append(targets, d.ruleFacts...)
	for i := range targets {
		for j := i + 1; j < len(targets); j++ {
			if overlap(targets[i], targets[j]) {
				return nil, malformed("two fact targets overlap")
			}
		}
	}
	requirements := map[string]bool{}
	for _, e := range d.evidence {
		if requirements[e.target] {
			return nil, malformed("a requirement is the evidence target of two entries")
		}
		requirements[e.target] = true
	}
	for _, r := range d.ruleEvidence {
		if requirements[r] {
			return nil, malformed("a requirement is the evidence target of the case's copy and its rule")
		}
		requirements[r] = true
	}
	written := map[string]bool{}
	for _, t := range targets {
		written[t] = true
	}
	if raw, ok := top["admits"]; ok {
		admits, err := closed(raw, nil, []string{"facts", "evidence"}, "admits")
		if err != nil {
			return nil, err
		}
		for _, member := range []string{"facts", "evidence"} {
			v, ok := admits[member]
			if !ok {
				continue
			}
			entries, ok := v.(map[string]any)
			if !ok || len(entries) == 0 {
				return nil, malformed("admits." + member + " is not an object of targets, written non-empty")
			}
			kind := "fact"
			known := written
			if member == "evidence" {
				kind, known = "evidence", requirements
			}
			for _, target := range sortedKeys(entries) {
				classes, ok := entries[target].([]any)
				if !known[target] || !ok || len(classes) == 0 || len(classes) > 3 {
					return nil, malformed("admission must name a mapped target and 1..3 classes")
				}
				seen := map[string]bool{}
				var list []string
				for _, x := range classes {
					class, _ := x.(string)
					if class != "asserted" && class != "record" && class != "generated" || seen[class] {
						return nil, malformed("invalid or duplicate admitted class")
					}
					seen[class] = true
					list = append(list, class)
				}
				d.admits[[2]string{kind, target}] = list
			}
		}
	}
	if raw, ok := top["unmapped"]; ok {
		list, ok := raw.([]any)
		if !ok || len(list) > 256 {
			return nil, malformed("unmapped is not an array of at most 256 pointers")
		}
		seen := map[string]bool{}
		for _, x := range list {
			p, ok := pointer(x, true)
			if !ok || seen[p] {
				return nil, malformed("unmapped names one that is not a pointer, or one twice")
			}
			seen[p] = true
			for t := range written {
				if p == "" || overlap(p, t) {
					return nil, malformed("mapped and intentionally unmapped pointers overlap")
				}
			}
		}
	}
	if raw, ok := top["unmappedEvidence"]; ok {
		list, ok := raw.([]any)
		if !ok || len(list) > 128 {
			return nil, malformed("unmappedEvidence is not an array of at most 128 requirements")
		}
		seen := map[string]bool{}
		for _, x := range list {
			r, ok := x.(string)
			if !ok || r == "" || len(r) > 256 || requirements[r] || seen[r] {
				return nil, malformed("invalid evidence omission")
			}
			seen[r] = true
		}
	}
	return d, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Digest is the dialect's digest, the SHA-256 of its canon bytes
// (spec/dialect.md §2), which a preparation names as its mappingDigest.
func (d *Dialect) Digest() string { return d.digest }
