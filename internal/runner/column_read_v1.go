package runner

// Draft RFC0029 integration. These helpers deliberately do not advertise a
// capability: generated provenance and negotiated transport remain separate gates.
import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
)

func hasColumnCopy(c CopyMapping) bool {
	for _, f := range c.Facts {
		if len(f.Conversion) > 0 {
			return true
		}
	}
	for _, f := range c.Evidence {
		if len(f.Availability) > 0 {
			return true
		}
	}
	return false
}
func hasColumnMapping(m InputMapping) bool {
	if m.Case != nil && hasColumnCopy(CopyMapping{Facts: m.Case.Facts, Evidence: m.Case.Evidence}) {
		return true
	}
	for _, s := range m.Sources {
		if s.Read.Copy != nil && hasColumnCopy(*s.Read.Copy) {
			return true
		}
	}
	return false
}
func columnError(err error) error {
	var r *retainedcolumns.Refusal
	if errors.As(err, &r) {
		return withCode(r.Code, err)
	}
	return err
}
func columnPlan(raw json.RawMessage, availability bool) (retainedcolumns.Plan, error) {
	p, err := retainedcolumns.ParsePlan(raw)
	if err != nil {
		return p, columnError(err)
	}
	m, err := exactMembers(raw)
	if err != nil {
		return p, err
	}
	_, kind := m["kind"]
	if kind == availability {
		return p, withCode(codeMalformed, errors.New("conversion and availability leaves are distinct closed variants"))
	}
	return p, nil
}
func validateColumnCopy(c CopyMapping) error {
	for _, f := range c.Facts {
		if len(f.Conversion) > 0 {
			if _, e := columnPlan(f.Conversion, false); e != nil {
				return e
			}
		}
	}
	for _, f := range c.Evidence {
		if len(f.Availability) > 0 {
			if _, e := columnPlan(f.Availability, true); e != nil {
				return e
			}
		}
	}
	return nil
}
func validateColumnMapping(m InputMapping) error {
	switch m.Profile {
	case "", "base":
	case "extended":
		return withCode("profile-unsupported", errors.New("extended retained-column preparation is not supported"))
	default:
		return withCode(codeMalformed, errors.New("unknown retained-column profile"))
	}
	if m.Case != nil {
		if e := validateColumnCopy(CopyMapping{Facts: m.Case.Facts, Evidence: m.Case.Evidence}); e != nil {
			return e
		}
	}
	for _, s := range m.Sources {
		if s.Read.Copy != nil && hasColumnCopy(*s.Read.Copy) {
			if e := validateColumnCopy(*s.Read.Copy); e != nil {
				return e
			}

		}
	}
	return nil
}

// Destination validation reuses v1's overlap checks without presenting its
// already validated dialect-only leaves as v1 policy.
func ordinaryFactTargets(fs []FactMapping) []FactMapping {
	r := make([]FactMapping, len(fs))
	copy(r, fs)
	for i := range r {
		r[i].Conversion = nil
	}
	return r
}
func ordinaryEvidenceTargets(es []EvidenceMapping) []EvidenceMapping {
	r := make([]EvidenceMapping, len(es))
	copy(r, es)
	for i := range r {
		r[i].Availability = nil
	}
	return r
}

// rawPointer reads the original token rather than serializing a decoded frame.
// Its caller has already read the complete case strictly, including duplicates.
func rawPointer(raw []byte, pointer string) (json.RawMessage, bool) {
	tokens, e := pointerTokens(pointer, true)
	if e != nil {
		return nil, false
	}
	value := json.RawMessage(raw)
	for _, key := range tokens {
		var object map[string]json.RawMessage
		if json.Unmarshal(value, &object) == nil && object != nil {
			var ok bool
			value, ok = object[key]
			if !ok {
				return nil, false
			}
			continue
		}
		var array []json.RawMessage
		if json.Unmarshal(value, &array) != nil {
			return nil, false
		}
		// pointerRead supplies the existing strict array-index grammar.
		generic := make([]any, len(array))
		for i := range array {
			generic[i] = array[i]
		}
		v, ok := pointerRead(generic, []string{key})
		if !ok {
			return nil, false
		}
		value = v.(json.RawMessage)
	}
	return value, true
}
func deriveCaseColumn(raw []byte, source string, leaf json.RawMessage, availability bool, frames map[string]retainedcolumns.Frame, selection ...*ConversionContract) (*retainedcolumns.Transformation, error) {
	p, e := columnPlan(leaf, availability)
	if e != nil {
		return nil, e
	}
	frame, cached := frames[source]
	if !cached {
		frameRaw, ok := rawPointer(raw, source)
		if !ok {
			return nil, withCode(codeReadFailed, errors.New("retained frame is missing"))
		}
		if selectedConversion(selection) != nil {
			// CASE context cannot confer generated authority. Reject that known
			// origin before the standalone generated-frame structural reader;
			// exactMembers still rejects duplicate outer members here.
			members, err := exactMembers(frameRaw)
			if err == nil {
				var origin string
				if json.Unmarshal(members["origin"], &origin) == nil && origin == "generated" {
					return nil, withCode(codeReadFailed, errors.New("case retained frame must be asserted; generated frames require verified producer provenance"))
				}
			}
		}
		frame, e = retainedcolumns.ParseFrame(frameRaw, retainedcolumns.Base)
		if e != nil {
			return nil, columnError(e)
		}
		// Source context grants no generated authority to caller-supplied case data.
		if frame.Origin() != "asserted" {
			return nil, withCode(codeReadFailed, errors.New("case retained frame must be asserted; generated frames require verified producer provenance"))
		}
		frames[source] = frame
	}
	t, e := retainedcolumns.Derive(p, frame)
	return t, columnError(e)
}
func applyColumnCase(read SourceRead, raw []byte, artifact any, params map[string]any, selection ...*ConversionContract) (derivedClaim, map[string]*retainedcolumns.Transformation, error) {
	if read.Copy == nil || !hasColumnCopy(*read.Copy) {
		c, e := applyRead(read, artifact, params)
		return c, nil, e
	}
	mapFrames := map[string]retainedcolumns.Frame{}
	return applyColumnCopy(read, artifact, func(source string, leaf json.RawMessage, availability bool) (*retainedcolumns.Transformation, error) {
		return deriveCaseColumn(raw, source, leaf, availability, mapFrames, selectedConversion(selection))
	}, selectedConversion(selection))
}
func applyColumnCopy(read SourceRead, artifact any, derive func(string, json.RawMessage, bool) (*retainedcolumns.Transformation, error), selection ...*ConversionContract) (derivedClaim, map[string]*retainedcolumns.Transformation, error) {
	if e := boundedArtifact(artifact); e != nil {
		return derivedClaim{}, nil, withCode(codeOverBound, e)
	}
	ordinary := CopyMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}
	for _, f := range read.Copy.Facts {
		if len(f.Conversion) == 0 {
			ordinary.Facts = append(ordinary.Facts, f)
		}
	}
	for _, f := range read.Copy.Evidence {
		if len(f.Availability) == 0 {
			ordinary.Evidence = append(ordinary.Evidence, f)
		}
	}
	c, e := copyClaim(ordinary, artifact)
	if e != nil {
		return c, nil, withCode(codeReadFailed, e)
	}
	fv, e := readInputJSON(c.Facts)
	if e != nil {
		return c, nil, e
	}
	facts := fv.(map[string]any)
	ts := map[string]*retainedcolumns.Transformation{}
	for _, f := range read.Copy.Facts {
		if len(f.Conversion) == 0 {
			continue
		}
		t, e := derive(f.Source, f.Conversion, false)
		if e != nil {
			return derivedClaim{}, nil, e
		}
		ts["fact:"+f.Target] = t
		c.Basis = append(c.Basis, f.Source)
		if t.Result.JSON != nil {
			v, e := readInputJSON([]byte(*t.Result.JSON))
			if e != nil {
				return derivedClaim{}, nil, e
			}
			writeTarget(facts, f.Target, v)
		}
	}
	for _, f := range read.Copy.Evidence {
		if len(f.Availability) == 0 {
			continue
		}
		t, e := derive(f.Source, f.Availability, true)
		if e != nil {
			return derivedClaim{}, nil, e
		}
		ts["evidence:"+f.Requirement] = t
		c.Basis = append(c.Basis, f.Source)
		if t.Result.JSON != nil {
			var value string
			if json.Unmarshal([]byte(*t.Result.JSON), &value) != nil {
				return derivedClaim{}, nil, withCode(codeReadFailed, errors.New("invalid availability result"))
			}
			c.Evidence[f.Requirement] = value
		}
	}
	sort.Strings(c.Basis)
	// Dialect §6.2 basis is a set, even when several converted targets
	// select the same retained frame. Ordinary copyClaim remains unchanged.
	unique := c.Basis[:0]
	for _, pointer := range c.Basis {
		if len(unique) == 0 || unique[len(unique)-1] != pointer {
			unique = append(unique, pointer)
		}
	}
	c.Basis = unique
	c.Facts = encode(facts)
	_, e = proofCanon(encode(c))
	if selectedConversion(selection) != nil {
		_, e = scopedJSON(encode(c), columnCombinedLimit)
	}
	if e != nil {
		return derivedClaim{}, nil, withCode(codeReadFailed, e)
	}
	return c, ts, nil
}
