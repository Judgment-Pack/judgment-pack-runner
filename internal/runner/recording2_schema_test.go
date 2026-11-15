package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The Spec's dialect-recording schema version 2 (RFC 0018's amendment of
// 2026-11-15, staged for 0.6.0) and its staged vectors, copied byte for byte
// at its commit 91a2e198cc271f81790689667e03122222d698ae
// (vectors/dialect-recording-2).
const recording2Dir = "../../vectors/dialect-recording-2/spec"

// The SHA-256 of each file at that commit, taken from the Spec's bytes (git
// show), not from anything this repository computes.
var recording2Pins = map[string]string{
	"schema/preparation.schema.json": "e2994ee2507eb833a67a98c782ea1d4b8df3556b07fcdfb0867840a3f08bbbad",
	"schema/export.schema.json":      "0f8bc6f1b8b6410915f07b58e8e8072b7030d5acc09afed864b9aaecb7ddc039",
	"vectors/README.md":              "ee8b3b77e2cb2dff65c015b30bae0ca717bb4e97deb1e3adefe8a9bf995cf827",
	"vectors/manifest.json":          "aaf019edac9e97d2d940a12a57af597d711305996fc82b21d9bb71283354c8ea",
}

func recording2File(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(recording2Dir, filepath.FromSlash(name)))
	must(t, err)
	return b
}

// schema2 is the subset of JSON Schema 2020-12 that the recording schema of
// version 2 uses, read here for the tests alone: Runner validates no
// preparation against a schema at run time, and the tests hold this reading
// to the Spec's staged vectors before they rely on it.
type schema2 struct{ root map[string]any }

func readSchema2(t *testing.T) schema2 {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(recording2File(t, "schema/preparation.schema.json")))
	d.UseNumber()
	var root map[string]any
	must(t, d.Decode(&root))
	return schema2{root}
}

func (s schema2) Valid(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return false
	}
	return s.valid(s.root, v)
}

func schemaType(v any, typ string) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		return !strings.ContainsAny(string(n), ".eE")
	}
	return false
}

func schemaNumber(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func schemaEqual(a, b any) bool {
	ca, _ := canonical(encode(a))
	cb, _ := canonical(encode(b))
	return bytes.Equal(ca, cb)
}

// schemaPattern is an ECMA-262 pattern as RE2 reads it: the schema ends each
// with (?![\s\S]), the end of the text.
func schemaPattern(p string) *regexp.Regexp {
	return regexp.MustCompile(strings.ReplaceAll(p, `(?![\s\S])`, `\z`))
}

func (s schema2) valid(node any, v any) bool {
	switch n := node.(type) {
	case bool:
		return n
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			name := strings.TrimPrefix(ref, "#/$defs/")
			defs, _ := s.root["$defs"].(map[string]any)
			target, ok := defs[name]
			if !ok || !s.valid(target, v) {
				return false
			}
		}
		if typ, ok := n["type"]; ok {
			match := false
			switch t := typ.(type) {
			case string:
				match = schemaType(v, t)
			case []any:
				for _, each := range t {
					match = match || schemaType(v, each.(string))
				}
			}
			if !match {
				return false
			}
		}
		if c, ok := n["const"]; ok && !schemaEqual(c, v) {
			return false
		}
		if e, ok := n["enum"].([]any); ok {
			found := false
			for _, each := range e {
				found = found || schemaEqual(each, v)
			}
			if !found {
				return false
			}
		}
		if text, ok := v.(string); ok {
			if m, ok := schemaNumber(n["minLength"]); ok && float64(utf8.RuneCountInString(text)) < m {
				return false
			}
			if p, ok := n["pattern"].(string); ok && !schemaPattern(p).MatchString(text) {
				return false
			}
		}
		if f, ok := schemaNumber(v); ok {
			if m, ok := schemaNumber(n["minimum"]); ok && f < m {
				return false
			}
			if m, ok := schemaNumber(n["maximum"]); ok && f > m {
				return false
			}
		}
		if o, ok := v.(map[string]any); ok {
			props, _ := n["properties"].(map[string]any)
			for _, r := range asList(n["required"]) {
				if _, ok := o[r.(string)]; !ok {
					return false
				}
			}
			for k, each := range o {
				if p, ok := props[k]; ok {
					if !s.valid(p, each) {
						return false
					}
				} else if extra, ok := n["additionalProperties"]; ok && !s.valid(extra, each) {
					return false
				}
				if names, ok := n["propertyNames"]; ok && !s.valid(names, k) {
					return false
				}
			}
		}
		if a, ok := v.([]any); ok {
			prefix := asList(n["prefixItems"])
			for i, each := range a {
				if i < len(prefix) {
					if !s.valid(prefix[i], each) {
						return false
					}
				} else if items, ok := n["items"]; ok && !s.valid(items, each) {
					return false
				}
			}
			if m, ok := schemaNumber(n["minItems"]); ok && float64(len(a)) < m {
				return false
			}
			if m, ok := schemaNumber(n["maxItems"]); ok && float64(len(a)) > m {
				return false
			}
			if n["uniqueItems"] == true {
				for i := range a {
					for j := i + 1; j < len(a); j++ {
						if schemaEqual(a[i], a[j]) {
							return false
						}
					}
				}
			}
		}
		for _, each := range asList(n["allOf"]) {
			if !s.valid(each, v) {
				return false
			}
		}
		if some := asList(n["anyOf"]); len(some) > 0 {
			found := false
			for _, each := range some {
				found = found || s.valid(each, v)
			}
			if !found {
				return false
			}
		}
		if one := asList(n["oneOf"]); len(one) > 0 {
			count := 0
			for _, each := range one {
				if s.valid(each, v) {
					count++
				}
			}
			if count != 1 {
				return false
			}
		}
		if not, ok := n["not"]; ok && s.valid(not, v) {
			return false
		}
		if cond, ok := n["if"]; ok {
			if s.valid(cond, v) {
				if then, ok := n["then"]; ok && !s.valid(then, v) {
					return false
				}
			} else if otherwise, ok := n["else"]; ok && !s.valid(otherwise, v) {
				return false
			}
		}
		return true
	}
	return false
}

func asList(v any) []any {
	a, _ := v.([]any)
	return a
}

// TestRecording2SchemaReadsTheSpecVectors holds the vendored files to their
// pins, and this reading of the schema to the Spec's staged answers: both
// readings' preparations valid, and each one changed in a rule version 2
// adds invalid. A test that validates a run's preparation against the schema
// relies on this one.
func TestRecording2SchemaReadsTheSpecVectors(t *testing.T) {
	for name, hash := range recording2Pins {
		if got := digest(recording2File(t, name)); got != "sha256:"+hash {
			t.Fatalf("%s is not the Spec's bytes at the pinned commit: %s", name, got)
		}
	}
	var manifest struct {
		Cases []struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			SHA256   string `json:"sha256"`
			Expected struct {
				Schema string `json:"schema"`
			} `json:"expected"`
		} `json:"cases"`
	}
	must(t, json.Unmarshal(recording2File(t, "vectors/manifest.json"), &manifest))
	schema := readSchema2(t)
	valid, invalid := 0, 0
	for _, c := range manifest.Cases {
		raw := recording2File(t, "vectors/"+c.Path)
		if got := digest(raw); got != "sha256:"+c.SHA256 {
			t.Fatalf("%s differs from the manifest: %s", c.Path, got)
		}
		got := "invalid"
		if schema.Valid(raw) {
			got = "valid"
		}
		if got != c.Expected.Schema {
			t.Errorf("%s: read %s, the Spec says %s", c.Name, got, c.Expected.Schema)
		}
		if got == "valid" {
			valid++
		} else {
			invalid++
		}
	}
	if valid != 2 || invalid != 15 {
		t.Fatalf("expected 2 valid and 15 invalid, read %d and %d", valid, invalid)
	}
	// A preparation of version 1's form is no preparation of version 2.
	if schema.Valid([]byte(`{"version":3,"verifiedAt":"2026-01-01T00:00:00Z","mappingDigest":"sha256:` + strings.Repeat("0", 64) + `","verification":"x","cites":[],"lineage":[],"outcomes":[]}`)) {
		t.Fatal("a preparation of version 1's form validated against version 2")
	}
}
