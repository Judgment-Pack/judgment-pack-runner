package runner

// Draft RFC0029 desk-reviewed-input/1 consumer. The installation's explicit
// producer pin is authority; a carrier's origin or a caller's trust object is not.
import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const columnProducerGrammar = "desk-reviewed-input/1"
const columnRevisionDomain = "judgment-pack/retained-column/1/desk-reviewed-input/revision\x00"

var columnRunID = regexp.MustCompile(`^research_[a-f0-9]{32}$`)
var columnRecipeVersion = regexp.MustCompile(`^[1-9][0-9]*$`)

type RetainedColumnProducer struct {
	ContractVersion string `json:"contractVersion"`
	Grammar         string `json:"grammar"`
}

func (p *RetainedColumnProducer) UnmarshalJSON(raw []byte) error {
	type plain RetainedColumnProducer
	var v plain
	m, e := exactMembers(raw)
	if e != nil || len(m) != 2 || m["contractVersion"] == nil || m["grammar"] == nil {
		return errors.New("retainedColumnProducer requires exactly contractVersion and grammar")
	}
	if e = strictJSON(raw, &v); e != nil {
		return e
	}
	*p = RetainedColumnProducer(v)
	return nil
}
func columnProducerProfile(p InputProfile) error {
	pin := p.RetainedColumnProducer
	if pin == nil || pin.ContractVersion != "1" || pin.Grammar != columnProducerGrammar {
		return withCode("capability-unsupported", errors.New("retained-column source requires the original-inventory producer contract desk-reviewed-input/1"))
	}
	if !mappingName.MatchString(p.ID) || p.Authority == "" || len(p.Authority) > 256 || p.Class != "generated" || p.Source != "desk-inputs" || p.Shape != "mcp" || p.Endpoint == nil || *p.Endpoint != "desk-local:reviewed-inputs" || p.EndpointScope != nil || len(p.Tools) != 1 || p.Tools[0] != "read_reviewed_input" || p.Adapter.Name != "jpack-desk/reviewed-input" || p.Adapter.Version != "2" || !validDigest(p.Adapter.Digest) || p.Calculator != nil || p.Research != nil {
		return withCode(codeMismatch, errors.New("retained-column producer differs from the trusted Desk source/adapter tuple"))
	}
	return nil
}
func preflightColumnSources(m InputMapping, profiles []InputProfile) error {
	for _, s := range m.Sources {
		if s.Read.Copy == nil || !hasColumnCopy(*s.Read.Copy) {
			continue
		}
		if m.Profile != "base" {
			return withCode("capability-unsupported", errors.New("generated retained columns require explicit Draft base profile"))
		}
		if s.Kind != "operation" || len(s.Read.Unwrap) > 0 || s.Calculation != nil {
			return withCode(codeMismatch, errors.New("retained columns require a directly bound producer operation"))
		}
		p, e := profileFor(s, profiles)
		if e != nil {
			return e
		}
		if m.selectedConversion != nil {
			if e = m.selectedConversion.validate("producer-carrier"); e != nil {
				return e
			}
			limits, err := conversionCapabilityLimits(p.ConversionContracts, "producer-carrier", true)
			if err != nil {
				return err
			}
			if err = conversionArtifactBound(limits, "profilesBytes", conversionProfilesBytes(profiles)); err != nil {
				return err
			}
		}
		if e = columnProducerProfile(p); e != nil {
			return e
		}
		// Every column leaf selects the one retained frame in this closed carrier.
		for _, f := range s.Read.Copy.Facts {
			if len(f.Conversion) > 0 && f.Source != "/retained" {
				return withCode(codeMalformed, errors.New("producer conversion source must be /retained"))
			}
		}
		for _, f := range s.Read.Copy.Evidence {
			if len(f.Availability) > 0 && f.Source != "/retained" {
				return withCode(codeMalformed, errors.New("producer availability source must be /retained"))
			}
		}
	}
	return nil
}
func columnSnapshotMember(raw []byte, path ...string) (json.RawMessage, error) {
	v := json.RawMessage(raw)
	for _, name := range path {
		m, e := exactMembers(v)
		if e != nil || m[name] == nil {
			return nil, errors.New("original snapshot projection member missing")
		}
		v = m[name]
	}
	return v, nil
}
func columnSnapshotString(raw []byte) (string, bool) {
	if !validInputEscapes(raw) {
		return "", false
	}
	var s string
	e := json.Unmarshal(raw, &s)
	return s, e == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
func columnRevision(rawID, rawVersion []byte) (string, error) {
	id, ok := columnSnapshotString(rawID)
	v := string(bytes.TrimSpace(rawVersion))
	n, e := strconv.ParseUint(v, 10, 64)
	if !ok || !columnRunID.MatchString(id) || !columnRecipeVersion.MatchString(v) || e != nil || n > 9007199254740991 {
		return "", errors.New("original run identity or recipe version is invalid")
	}
	return digest([]byte(columnRevisionDomain + id + "\x00" + strconv.FormatUint(n, 10))), nil
}

// validateColumnProducerRelation establishes offline snapshot/frame consistency.
// It cannot prove the producer actually loaded the original recipe history. That
// is an explicit precondition of the separately reviewed and pinned adapter v2.
func validateColumnProducerRelation(c retainedColumnCarrier) error {
	malformed := func() error {
		return withCode(codeMalformed, errors.New("invalid original reviewed snapshot projection"))
	}
	differs := func() error {
		return withCode(codeReadFailed, errors.New("retained frame differs from the complete original snapshot inventory, revision or values"))
	}
	member := func(path ...string) json.RawMessage {
		v, _ := columnSnapshotMember(c.OriginalSnapshot, path...)
		return v
	}
	snapshotClass, _ := columnSnapshotString(member("class"))
	recordClass, _ := columnSnapshotString(member("record", "class"))
	if string(bytes.TrimSpace(member("version"))) != "1" || snapshotClass != "generated" || recordClass != "generated" {
		return malformed()
	}
	revision, e := columnRevision(member("research", "id"), member("research", "recipeVersion"))
	if e != nil {
		return malformed()
	}
	other, e := columnRevision(member("record", "research", "runId"), member("record", "research", "recipeVersion"))
	if e != nil {
		return malformed()
	}
	if revision != other {
		return differs()
	}
	var frame struct {
		Values   map[string]json.RawMessage `json:"values"`
		Research struct {
			RecordRecipeRevision string `json:"recordRecipeRevision"`
			Declaration          *struct {
				RecipeRevision string `json:"recipeRevision"`
				Fields         []struct {
					Name      string `json:"name"`
					FieldJSON string `json:"fieldJson"`
				} `json:"fields"`
			} `json:"declaration"`
			Fields map[string]struct {
				Status    string  `json:"status"`
				ValueJSON *string `json:"valueJson"`
			} `json:"fields"`
		} `json:"research"`
	}
	if json.Unmarshal(c.RawFrame, &frame) != nil {
		return malformed()
	}
	r := frame.Research
	if r.Declaration == nil || r.RecordRecipeRevision != revision || r.Declaration.RecipeRevision != revision {
		return differs()
	}
	values, e := exactMembers(member("record", "values"))
	if e != nil {
		return malformed()
	}
	if len(values) != len(frame.Values) {
		return differs()
	}
	for k, v := range values {
		s, ok := columnSnapshotString(v)
		if !ok {
			return malformed()
		}
		want, ok := columnSnapshotString(frame.Values[k])
		if !ok || s != want {
			return differs()
		}
	}
	var declarations []json.RawMessage
	if json.Unmarshal(member("research", "recipe", "fields"), &declarations) != nil || declarations == nil {
		return malformed()
	}
	if len(declarations) != len(r.Declaration.Fields) {
		return differs()
	}
	seen := map[string]bool{}
	for i, raw := range declarations {
		m, e := exactMembers(raw)
		if e != nil {
			return malformed()
		}
		name, ok := columnSnapshotString(m["name"])
		if !ok || name == "" || seen[name] {
			return malformed()
		}
		seen[name] = true
		kind, ok := columnSnapshotString(m["type"])
		if !ok {
			return malformed()
		}
		switch kind {
		case "text", "integer", "decimal", "boolean", "text-list", "range", "string", "number":
		default:
			return malformed()
		}
		var compact bytes.Buffer
		if json.Compact(&compact, raw) != nil {
			return malformed()
		}
		got := r.Declaration.Fields[i]
		if got.Name != name || got.FieldJSON != compact.String() {
			return differs()
		}
	}
	fields, e := exactMembers(member("record", "research", "entity", "fields"))
	if e != nil {
		return malformed()
	}
	if len(fields) != len(r.Fields) {
		return differs()
	}
	for name, raw := range fields {
		m, e := exactMembers(raw)
		if e != nil {
			return malformed()
		}
		status, ok := columnSnapshotString(m["status"])
		if !ok {
			return malformed()
		}
		switch status {
		case "supported", "interpretation", "missing", "conflicting", "inaccessible", "stale":
		default:
			return malformed()
		}
		got, ok := r.Fields[name]
		if !ok || got.Status != status {
			return differs()
		}
		v, present := m["value"]
		if present != (got.ValueJSON != nil) {
			return differs()
		}
		if present && string(v) != *got.ValueJSON {
			return differs()
		}
	}
	return nil
}

// A generated acquisition is either the historical response object on initial
// input, or sole exact text. The latter retains one envelope without a redundant
// serialized object. Signatures authenticate canonical content, not spelling.
func columnRetainedResponse(value SourceValue, selection ...*ConversionContract) ([]byte, error) {
	raw := value.Response
	if value.RetainedResponseJSON != "" {
		if len(value.Response) > 0 || len(value.Snapshot) > 0 {
			return nil, withCode(codeMalformed, errors.New("retained acquisition must supply exactly one response form"))
		}
		raw = []byte(value.RetainedResponseJSON)
	}
	if len(raw) == 0 || len(value.Snapshot) > 0 {
		return nil, withCode(codeMalformed, errors.New("retained acquisition must supply exactly one response form"))
	}
	if e := selectedConversion(selection).validate("producer-carrier"); e != nil {
		return nil, e
	}
	if len(raw) > responseLimit(selectedConversion(selection)) {
		return nil, withCode(codeOverBound, errors.New("complete retained response exceeds base limit"))
	}
	_, canonErr := proofCanon(raw)
	if selectedConversion(selection) != nil {
		_, canonErr = scopedJSON(raw, columnResponseLimit)
	}
	if e := canonErr; e != nil {
		return nil, withCode(codeMalformed, e)
	}
	return raw, nil
}

// Historical profile/source forms retain their decoder. The new optional exact
// text/pin variants are case-sensitive and cannot silently turn null into absent.
func (p *InputProfile) UnmarshalJSON(raw []byte) error {
	type plain InputProfile
	var v plain
	if e := strictJSON(raw, &v); e != nil {
		return e
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*p = InputProfile(v)
		return nil
	}
	m, e := exactMembers(raw)
	if e != nil {
		return e
	}
	for k, b := range m {
		if strings.EqualFold(k, "conversionContracts") && (k != "conversionContracts" || len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '[') {
			return errors.New("invalid conversionContracts member")
		}
		if strings.EqualFold(k, "retainedColumnProducer") && (k != "retainedColumnProducer" || bytes.Equal(bytes.TrimSpace(b), []byte("null"))) {
			return errors.New("invalid retainedColumnProducer member")
		}
	}
	*p = InputProfile(v)
	return nil
}
func (v *SourceValue) UnmarshalJSON(raw []byte) error {
	type plain SourceValue
	var p plain
	if e := strictJSON(raw, &p); e != nil {
		return e
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*v = SourceValue(p)
		return nil
	}
	m, e := exactMembers(raw)
	if e != nil {
		return e
	}
	for k, b := range m {
		if strings.EqualFold(k, "conversionContracts") && (k != "conversionContracts" || len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '[') {
			return errors.New("invalid conversionContracts member")
		}
		if strings.EqualFold(k, "retainedResponseJson") {
			if k != "retainedResponseJson" || len(m) != 1 || p.RetainedResponseJSON == "" || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
				return errors.New("invalid retainedResponseJson member")
			}
		}
	}
	*v = SourceValue(p)
	return nil
}
