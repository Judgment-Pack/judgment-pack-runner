package runner

import (
	"encoding/json"
	"errors"
	"strings"
)

// Historical two-member copies keep their decoder. A new three-member variant
// is closed and case-sensitive even on a preparation/live input path.
func (f *FactMapping) UnmarshalJSON(raw []byte) error {
	type plain FactMapping
	var v plain
	if e := strictJSON(raw, &v); e != nil {
		return e
	}
	if e := columnLeafKeys(raw, "conversion", "target", "source"); e != nil {
		return e
	}
	*f = FactMapping(v)
	return nil
}
func (f *EvidenceMapping) UnmarshalJSON(raw []byte) error {
	type plain EvidenceMapping
	var v plain
	if e := strictJSON(raw, &v); e != nil {
		return e
	}
	if e := columnLeafKeys(raw, "availability", "requirement", "source"); e != nil {
		return e
	}
	*f = EvidenceMapping(v)
	return nil
}
func columnLeafKeys(raw []byte, leaf, first, second string) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return errors.New("invalid copy member")
	}
	found := false
	for k := range m {
		if strings.EqualFold(k, leaf) {
			found = true
		}
	}
	if !found {
		return nil
	}
	if len(m) != 3 || m[leaf] == nil || m[first] == nil || m[second] == nil {
		return errors.New("retained-column copy must have exactly its three case-sensitive members")
	}
	return nil
}
