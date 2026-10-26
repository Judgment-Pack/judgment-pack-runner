package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
	"net/http"
	"strings"
)

// ConversionSpec identifies installed, reviewed contract artifacts. The caller
// cannot configure these pins or grant support by echoing them in a selector.
type ConversionSpec struct {
	Version      string `json:"version"`
	Revision     string `json:"revision"`
	SchemaSHA256 string `json:"schemaSha256"`
	CorpusSHA256 string `json:"corpusSha256"`
}

// ConversionContract is the closed selector retained with complete-object
// preparation bindings. It grants no source, signature or producer authority.
type ConversionContract struct {
	Contract string         `json:"contract"`
	Spec     ConversionSpec `json:"spec"`
	Profile  string         `json:"profile"`
	Roles    []string       `json:"roles"`
}

// Experimental immutable Draft candidate, pending independent review/adoption.
// The compiled identity is never taken from caller or host configuration.
var installedConversionSpec = ConversionSpec{Version: "Draft RFC0029", Revision: "ade3914b0b81ef299e5b9a8d6147f37ff3f0976c", SchemaSHA256: "sha256:996cec17473987eebf1aa553536df0ce5a740971532567f411282ee70036879f", CorpusSHA256: "sha256:e2ac53f56cd5ab779e032d2847b49fa04438ffb9df57d456f75f2ba058dc12d2"}

var conversionRoles = [...]string{"document-validation", "preview", "preparation", "preparation-verification", "producer-carrier"}

const (
	columnDocumentLimit    = 1 << 20
	columnFrameLimit       = 2 << 20
	columnCombinedLimit    = 12 << 20
	columnCarrierLimit     = 3 << 20
	columnResponseLimit    = 5 << 20
	columnPreparationLimit = 16 << 20
	columnProfilesLimit    = 80 << 10
	columnRequestLimit     = 16 << 20
	columnReplyLimit       = 36 << 20
	columnStoredLimit      = 40 << 20
	columnRecordLimit      = 9 << 20
	columnExportBaseLimit  = 72 << 20
	ColumnExportWireLimit  = 86 << 20
)

func conversionFailure(code string) error {
	message := "The retained-column conversion selector or capability is malformed."
	if code == "capability-unsupported" {
		message = "This installed package or a required owner does not support the exact retained-column selection."
	}
	return causedError{&apiError{422, code, message}, codedError{code, errors.New(message)}}
}
func (c *ConversionContract) UnmarshalJSON(raw []byte) error {
	type plain ConversionContract
	var v plain
	if strictJSON(raw, &v) != nil {
		return conversionFailure(codeMalformed)
	}
	m, e := exactMembers(raw)
	if e != nil || len(m) != 4 {
		return conversionFailure(codeMalformed)
	}
	for _, k := range []string{"contract", "spec", "profile", "roles"} {
		if m[k] == nil {
			return conversionFailure(codeMalformed)
		}
	}
	spec, e := exactMembers(m["spec"])
	if e != nil || len(spec) != 4 {
		return conversionFailure(codeMalformed)
	}
	for _, k := range []string{"version", "revision", "schemaSha256", "corpusSha256"} {
		if spec[k] == nil {
			return conversionFailure(codeMalformed)
		}
	}
	if e := conversionIdentityForm(m); e != nil {
		return e
	}
	if len(bytes.TrimSpace(m["roles"])) == 0 || bytes.TrimSpace(m["roles"])[0] != '[' {
		return conversionFailure(codeMalformed)
	}
	var roles []json.RawMessage
	if json.Unmarshal(m["roles"], &roles) != nil {
		return conversionFailure(codeMalformed)
	}
	for _, role := range roles {
		role = bytes.TrimSpace(role)
		if len(role) == 0 || role[0] != '"' {
			return conversionFailure(codeMalformed)
		}
	}
	*c = ConversionContract(v)
	return nil
}
func (c *ConversionContract) validate(required ...string) error {
	if c == nil {
		return nil
	}
	if c.Contract != "retained-column/1" || c.Profile != "base" || installedConversionSpec.Version == "" || c.Spec != installedConversionSpec {
		return conversionFailure("capability-unsupported")
	}
	if len(c.Roles) == 0 {
		return conversionFailure(codeMalformed)
	}
	last := -1
	for _, role := range c.Roles {
		index := -1
		for j, known := range conversionRoles {
			if role == known {
				index = j
			}
		}
		if index < 0 {
			return conversionFailure("capability-unsupported")
		}
		if index <= last {
			return conversionFailure(codeMalformed)
		}
		last = index
	}
	for _, role := range required {
		found := false
		for _, held := range c.Roles {
			found = found || held == role
		}
		if !found {
			return conversionFailure("capability-unsupported")
		}
	}
	return nil
}
func sameConversion(a, b *ConversionContract) bool { return bytes.Equal(encode(a), encode(b)) }
func inputConversion(i Input) *ConversionContract {
	if i.Source != nil {
		return i.Source.ConversionContract
	}
	return nil
}
func combinedLimit(i Input) int {
	if inputConversion(i) != nil {
		return columnCombinedLimit
	}
	return MaxBody
}
func responseLimit(c *ConversionContract) int {
	if c != nil {
		return columnResponseLimit
	}
	return MaxBody
}
func recordLimit(i Input) int {
	if inputConversion(i) != nil {
		return columnRecordLimit
	}
	return maxOutput
}

type conversionContext struct{}

func requestConversion(r *http.Request) *ConversionContract {
	c, _ := r.Context().Value(conversionContext{}).(*ConversionContract)
	return c
}

// conversionRequest executes after authentication and before routing/body reads.
// Only these existing consumer operations have a scoped adapter; other paths
// explicitly refuse instead of inheriting expanded limits through a prefix.
func conversionRequest(r *http.Request) (*http.Request, error) {
	const prefix = "/v1/retained-column/1/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		if len(r.Header.Values("JPack-Conversion-Contract")) != 0 {
			return nil, conversionFailure("capability-unsupported")
		}
		return r, nil
	}
	values := r.Header.Values("JPack-Conversion-Contract")
	if len(values) != 1 || len(values[0]) > 4096 {
		return nil, conversionFailure(codeMalformed)
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(values[0])
	if e != nil || base64.RawURLEncoding.EncodeToString(raw) != values[0] {
		return nil, conversionFailure(codeMalformed)
	}
	var c ConversionContract
	if e = strictJSON(raw, &c); e != nil {
		return nil, conversionFailure(codeMalformed)
	}
	path := "/v1/" + strings.TrimPrefix(r.URL.Path, prefix)
	required := []string{"document-validation", "preview", "preparation"}
	parts := strings.Split(path, "/")
	runRoute := len(parts) == 5 && parts[1] == "v1" && parts[2] == "jobs" && parts[3] != "" && parts[4] == "runs"
	exportRoute := len(parts) == 5 && parts[1] == "v1" && parts[2] == "runs" && parts[3] != "" && parts[4] == "verification"
	supported := r.Method == "POST" && (path == "/v1/inputs/next" || path == "/v1/inputs/preview" || path == "/v1/previews" || runRoute)
	if r.Method == "GET" && exportRoute {
		supported = true
		required = []string{"preparation-verification"}
	}
	if !supported {
		return nil, conversionFailure("capability-unsupported")
	}
	if e = c.validate(required...); e != nil {
		return nil, e
	}
	r = r.Clone(context.WithValue(r.Context(), conversionContext{}, &c))
	r.URL.Path = path
	return r, nil
}
func admitInputConversion(i Input, selected *ConversionContract) error {
	c := inputConversion(i)
	if !sameConversion(c, selected) {
		return conversionFailure("capability-unsupported")
	}
	return c.validate("document-validation", "preview", "preparation")
}
func scopedJSON(raw []byte, limit int) ([]byte, error) {
	if len(raw) > limit {
		return nil, errors.New("value exceeds selected artifact limit")
	}
	// Retained case/response strings are opaque custody, not canonical claims.
	if e := measureBytes(raw, 32); e != nil {
		return nil, e
	}
	if _, e := readInputJSON(raw); e != nil {
		return nil, e
	}
	return derivation.Canon(raw)
}

func selectedConversion(cs []*ConversionContract) *ConversionContract {
	if len(cs) > 0 {
		return cs[0]
	}
	return nil
}
func artifactLimit(c *ConversionContract, ordinary, selected int) int {
	if c != nil {
		return selected
	}
	return ordinary
}
func preparationLimit(i Input) int {
	return artifactLimit(inputConversion(i), maxPreparationDocument, columnPreparationLimit)
}

// ParseConversionSelector admits an explicit transport selection against only
// the installed manifest, before the selected artifact is read.
func ParseConversionSelector(encoded string, roles ...string) (*ConversionContract, error) {
	if encoded == "" {
		return nil, nil
	}
	if len(encoded) > 4096 {
		return nil, conversionFailure(codeMalformed)
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if e != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, conversionFailure(codeMalformed)
	}
	var c ConversionContract
	if strictJSON(raw, &c) != nil {
		return nil, conversionFailure(codeMalformed)
	}
	if e = c.validate(roles...); e != nil {
		return nil, e
	}
	return &c, nil
}

// ConversionExportLimit is a pre-read ceiling, granted only by a validated
// explicit selector, never by peeking into the untrusted export.
func ConversionExportLimit(c *ConversionContract) (int, error) {
	if e := c.validate("preparation-verification"); e != nil {
		return 0, e
	}
	return artifactLimit(c, MaxExportSize, ColumnExportWireLimit), nil
}

type conversionWriter struct {
	http.ResponseWriter
	limit int
}

func selectedReplyWriter(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
	if requestConversion(r) == nil {
		return w
	}
	limit := columnReplyLimit
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/verification") {
		limit = ColumnExportWireLimit
	}
	return conversionWriter{w, limit}
}

func conversionIdentityForm(m map[string]json.RawMessage) error {
	for _, k := range []string{"contract", "profile"} {
		v := bytes.TrimSpace(m[k])
		if len(v) == 0 || v[0] != '"' {
			return conversionFailure(codeMalformed)
		}
	}
	spec, e := exactMembers(m["spec"])
	if e != nil {
		return conversionFailure(codeMalformed)
	}
	for _, k := range []string{"version", "revision", "schemaSha256", "corpusSha256"} {
		v := bytes.TrimSpace(spec[k])
		if len(v) == 0 || v[0] != '"' {
			return conversionFailure(codeMalformed)
		}
	}
	var pin ConversionSpec
	if strictJSON(m["spec"], &pin) != nil || pin.Version == "" || !validHex(pin.Revision, 20) || !validDigest(pin.SchemaSHA256) || !validDigest(pin.CorpusSHA256) {
		return conversionFailure(codeMalformed)
	}
	return nil
}

func (p *Preparation) UnmarshalJSON(raw []byte) error {
	type plain Preparation
	var held plain
	if e := strictJSON(raw, &held); e != nil {
		return e
	}
	m, e := exactMembers(raw)
	if e != nil {
		return e
	}
	for k, v := range m {
		if strings.EqualFold(k, "conversionContract") && (k != "conversionContract" || bytes.Equal(bytes.TrimSpace(v), []byte("null"))) {
			return conversionFailure(codeMalformed)
		}
	}
	*p = Preparation(held)
	return nil
}

func taskConversionMatches(raw []byte, selected *ConversionContract) bool {
	var frozen sourceRequest
	return strictJSON(raw, &frozen) == nil && sameConversion(frozen.ConversionContract, selected)
}

func (r *sourceRequest) UnmarshalJSON(raw []byte) error {
	type plain sourceRequest
	var held plain
	if e := strictJSON(raw, &held); e != nil {
		return e
	}
	m, e := exactMembers(raw)
	if e != nil {
		return e
	}
	for k, v := range m {
		if strings.EqualFold(k, "conversionContract") && (k != "conversionContract" || bytes.Equal(bytes.TrimSpace(v), []byte("null"))) {
			return conversionFailure(codeMalformed)
		}
	}
	*r = sourceRequest(held)
	return nil
}
func (s *sourceStatus) UnmarshalJSON(raw []byte) error {
	type plain sourceStatus
	var held plain
	if e := strictJSON(raw, &held); e != nil {
		return e
	}
	m, e := exactMembers(raw)
	if e != nil {
		return e
	}
	for k, v := range m {
		if strings.EqualFold(k, "retainedResponseJson") && (k != "retainedResponseJson" || m["response"] != nil || held.RetainedResponseJSON == "" || bytes.Equal(bytes.TrimSpace(v), []byte("null"))) {
			return conversionFailure(codeMalformed)
		}
	}
	*s = sourceStatus(held)
	return nil
}
