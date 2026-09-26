package runner

// Source inputs carry a caller-selected, retained document. Acquisition and
// credentials stay in Gateway; Desk verifies receipts against its current pin.
// Runner binds the exact supplied bytes to a deterministic, frozen mapping. It
// does not treat a caller's proof or an availability declaration as verified truth.
import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxInputDocument = 200_000

type FactMapping struct {
	Target string `json:"target"`
	Source string `json:"source"`
}
type EvidenceMapping struct {
	Requirement string `json:"requirement"`
	Source      string `json:"source"`
}
type InputMapping struct {
	Version  int               `json:"version"`
	Provider string            `json:"provider"`
	Facts    []FactMapping     `json:"facts"`
	Evidence []EvidenceMapping `json:"evidence"`
}
type SourceInput struct {
	Mapping       InputMapping    `json:"mapping"`
	Snapshot      json.RawMessage `json:"snapshot"`
	MappingDigest string          `json:"mappingDigest,omitempty"`
}
type sourceSnapshot struct {
	Version    int    `json:"version"`
	SelectedAt string `json:"selectedAt,omitempty"`
	Original   struct {
		Name      string `json:"name"`
		MediaType string `json:"mediaType"`
		Bytes     string `json:"bytes"`
		SHA256    string `json:"sha256"`
	} `json:"original"`
	Proof struct {
		Session   string `json:"session"`
		Source    string `json:"source"`
		Authority string `json:"authority"`
		PublicKey string `json:"publicKey"`
		Response  string `json:"response"`
		Registry  string `json:"registry"`
		Drive     struct {
			FileID string `json:"fileId"`
			Grant  string `json:"grant"`
		} `json:"drive"`
	} `json:"proof"`
}
type sourceRecord struct {
	Document struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		MediaType string `json:"mediaType"`
		Size      int    `json:"size"`
	} `json:"document"`
	Original struct {
		Bytes string `json:"bytes"`
	} `json:"original"`
	Provenance struct {
		ObservedAt string `json:"observedAt"`
		Source     struct {
			Kind    string `json:"kind"`
			FileID  string `json:"fileId"`
			Version string `json:"version"`
		} `json:"source"`
	} `json:"provenance"`
}

func inputProblem(message string) error { return bad("invalid_source_input", message) }

// Duplicate object keys and excessive nesting are refused before values are
// projected. json.Number preserves decimals and integer spelling in the runner.
// encoding/json replaces unpaired surrogate escapes; reject those before decoding.
func validInputEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
func readInputJSON(data []byte) (any, error) {
	if !utf8.Valid(data) || !validInputEscapes(data) {
		return nil, errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		if depth > 32 {
			return nil, errors.New("input nesting exceeds limit")
		}
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch t {
		case json.Delim('{'):
			o := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				name, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				if _, exists := o[name]; exists {
					return nil, errors.New("duplicate object key")
				}
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				o[name] = v
			}
			_, e = d.Token()
			return o, e
		case json.Delim('['):
			a := []any{}
			for d.More() {
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, e = d.Token()
			return a, e
		}
		if _, ok := t.(json.Delim); ok {
			return nil, errors.New("invalid JSON")
		}
		return t, nil
	}
	v, err := value(0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("one JSON value required")
	}
	return v, nil
}
func pointerTokens(pointer string, root bool) ([]string, error) {
	if pointer == "" && root {
		return nil, nil
	}
	if len(pointer) > 1024 || !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("use an absolute JSON pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	if len(parts) > 32 {
		return nil, errors.New("mapping nesting exceeds limit")
	}
	for i, p := range parts {
		for at := 0; at < len(p); at++ {
			if p[at] == '~' {
				if at+1 >= len(p) || (p[at+1] != '0' && p[at+1] != '1') {
					return nil, errors.New("invalid pointer escape")
				}
				at++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
		if parts[i] == "__proto__" || parts[i] == "constructor" || parts[i] == "prototype" {
			return nil, errors.New("unsupported mapping field")
		}
	}
	return parts, nil
}
func pointerRead(v any, parts []string) (any, bool) {
	for _, key := range parts {
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[key]
			if !ok {
				return nil, false
			}
		case []any:
			n, err := strconv.Atoi(key)
			if err != nil || strconv.Itoa(n) != key || n < 0 || n >= len(x) {
				return nil, false
			}
			v = x[n]
		default:
			return nil, false
		}
	}
	return v, true
}
func (m InputMapping) validate() error {
	if m.Version != 1 || (m.Provider != "google-drive" && m.Provider != "local-file") || m.Facts == nil || m.Evidence == nil || len(m.Facts) > 256 || len(m.Evidence) > 128 {
		return inputProblem("Use a supported file input mapping.")
	}
	targets := map[string]bool{}
	ids := map[string]bool{}
	for _, f := range m.Facts {
		if _, err := pointerTokens(f.Target, false); err != nil {
			return inputProblem(err.Error())
		}
		if _, err := pointerTokens(f.Source, true); err != nil {
			return inputProblem(err.Error())
		}
		for held := range targets {
			if held == f.Target || strings.HasPrefix(held, f.Target+"/") || strings.HasPrefix(f.Target, held+"/") {
				return inputProblem("Fact mappings must have distinct, non-overlapping destinations.")
			}
		}
		targets[f.Target] = true
	}
	for _, f := range m.Evidence {
		if f.Requirement == "" || len(f.Requirement) > 256 || ids[f.Requirement] {
			return inputProblem("Choose each evidence requirement once.")
		}
		ids[f.Requirement] = true
		if _, err := pointerTokens(f.Source, true); err != nil {
			return inputProblem(err.Error())
		}
	}
	return nil
}
func mappingHash(m InputMapping) string { return digest(encode(m)) }
func sourceDocument(c *SourceInput) (any, sourceSnapshot, sourceRecord, error) {
	var s sourceSnapshot
	var r sourceRecord
	fail := func() (any, sourceSnapshot, sourceRecord, error) {
		return nil, s, r, inputProblem("Choose one JSON file up to 200 KB. Drive files must retain their source proof.")
	}
	if len(c.Snapshot) > 1<<20 {
		return fail()
	}
	if _, err := readInputJSON(c.Snapshot); err != nil {
		return fail()
	}
	if json.Unmarshal(c.Snapshot, &s) != nil || s.Version != 1 || s.Original.Name == "" || len(s.Original.Name) > 1024 || strings.ContainsAny(s.Original.Name, "\x00\r\n") {
		return fail()
	}
	if s.Original.MediaType != "application/json" && !(s.Original.MediaType == "text/plain" && strings.HasSuffix(strings.ToLower(s.Original.Name), ".json")) {
		return fail()
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s.Original.Bytes)
	if err != nil || len(raw) == 0 || len(raw) > maxInputDocument || base64.StdEncoding.EncodeToString(raw) != s.Original.Bytes || digest(raw) != s.Original.SHA256 {
		return fail()
	}
	if c.Mapping.Provider == "local-file" {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(c.Snapshot, &fields)
		if _, exists := fields["proof"]; exists {
			return fail()
		}
		if _, err = time.Parse(time.RFC3339Nano, s.SelectedAt); err != nil {
			return fail()
		}
		r.Provenance.ObservedAt = s.SelectedAt
		r.Provenance.Source.Kind = "local-file"
		r.Provenance.Source.Version = s.Original.SHA256
	} else {
		if s.Proof.Source != "drive" || s.Proof.Drive.FileID == "" || len(s.Proof.Drive.FileID) > 256 || s.Proof.Session == "" || len(s.Proof.PublicKey) != 64 || s.Proof.Registry == "" || s.Proof.Authority == "" {
			return fail()
		}
		var response struct {
			Result  sourceRecord    `json:"result"`
			Receipt json.RawMessage `json:"receipt"`
		}
		if _, err = readInputJSON([]byte(s.Proof.Response)); err != nil {
			return fail()
		}
		if json.Unmarshal([]byte(s.Proof.Response), &response) != nil || len(response.Receipt) == 0 {
			return fail()
		}
		r = response.Result
		if r.Document.ID != s.Original.SHA256 || r.Document.Name != s.Original.Name || r.Document.MediaType != s.Original.MediaType || r.Document.Size != len(raw) || r.Original.Bytes != s.Original.Bytes || r.Provenance.Source.Kind != "google-drive" || r.Provenance.Source.FileID != s.Proof.Drive.FileID || r.Provenance.Source.Version == "" {
			return fail()
		}
		if _, err = time.Parse(time.RFC3339Nano, r.Provenance.ObservedAt); err != nil {
			return fail()
		}
	}
	data, err := readInputJSON(raw)
	if err != nil {
		return nil, s, r, inputProblem("The source must be valid UTF-8 JSON without duplicate keys, nested at most 32 levels.")
	}
	if _, ok := data.(map[string]any); !ok {
		return nil, s, r, inputProblem("Choose one JSON object describing a single input record.")
	}
	return data, s, r, nil
}
func normalizeInput(i Input) (Input, error) {
	if i.Source == nil {
		return i, i.validate()
	}
	c := i.Source
	if err := c.Mapping.validate(); err != nil {
		return i, err
	}
	hash := mappingHash(c.Mapping)
	if c.MappingDigest != "" && c.MappingDigest != hash {
		return i, inputProblem("The input mapping changed. Preview it again.")
	}
	source, _, _, err := sourceDocument(c)
	if err != nil {
		return i, err
	}
	facts := map[string]any{}
	availability := map[string]string{}
	for _, f := range c.Mapping.Facts {
		from, _ := pointerTokens(f.Source, true)
		v, present := pointerRead(source, from)
		if !present {
			continue
		}
		to, _ := pointerTokens(f.Target, false)
		at := facts
		for _, k := range to[:len(to)-1] {
			if at[k] == nil {
				at[k] = map[string]any{}
			}
			at = at[k].(map[string]any)
		}
		at[to[len(to)-1]] = v
	}
	for _, f := range c.Mapping.Evidence {
		from, _ := pointerTokens(f.Source, true)
		v, present := pointerRead(source, from)
		if !present {
			continue
		}
		value, ok := v.(string)
		if !ok || (value != "present" && value != "absent" && value != "unknown") {
			return i, inputProblem("Mapped evidence values must be present, absent or unknown. Missing values remain unknown.")
		}
		availability[f.Requirement] = value
	}
	mapped := Input{Facts: encode(facts), Source: c}
	if len(c.Mapping.Evidence) > 0 {
		mapped.Evidence = encode(availability)
	}
	if len(i.Facts) > 0 {
		a, e := canonical(i.Facts)
		if e != nil || !bytes.Equal(a, mapped.Facts) {
			return i, inputProblem("Supplied facts do not match the retained source mapping.")
		}
	}
	if len(i.Evidence) > 0 {
		a, e := canonical(i.Evidence)
		if e != nil || !bytes.Equal(a, mapped.Evidence) {
			return i, inputProblem("Supplied evidence does not match the retained source mapping.")
		}
	}
	c.MappingDigest = hash
	return mapped, mapped.validate()
}
func mappingMatchesRelease(r Release, i Input) bool {
	if r.InputMapping == nil {
		return i.Source == nil
	}
	return i.Source != nil && mappingHash(*r.InputMapping) == mappingHash(i.Source.Mapping)
}

// Briefs expose facts and provenance metadata, not base64 originals or consumed
// selection grants. The complete proof remains available in the retained record.
func briefInput(i Input) any {
	if i.Source == nil {
		return i
	}
	_, s, r, err := sourceDocument(i.Source)
	if err != nil {
		return map[string]any{"facts": i.Facts, "evidence": i.Evidence, "sourceProblem": "Retained source unavailable"}
	}
	return map[string]any{"facts": i.Facts, "evidence": i.Evidence, "mapping": i.Source.Mapping, "mappingDigest": i.Source.MappingDigest, "source": map[string]any{"provider": i.Source.Mapping.Provider, "name": s.Original.Name, "fileId": r.Provenance.Source.FileID, "version": r.Provenance.Source.Version, "sha256": s.Original.SHA256, "observedAt": r.Provenance.ObservedAt, "authority": s.Proof.Authority, "verification": "Source metadata records acquisition, not evidence truth. Availability values remain declarations."}}
}

// Indent without a decode/re-encode so numeric spelling remains exact for UI consumers.
func inputJSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") != nil {
		return string(raw)
	}
	return out.String()
}
