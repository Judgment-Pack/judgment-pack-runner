package runner

// Dialect version 3 (the specification's companion document spec/dialect.md,
// §4.3 and §4.4; runner #82). Version 3 is version 2 with the unbound form
// of an operation and four kinds more. Runner reads it so:
//
//   - A version-3 document whose sources are all bound is prepared as a
//     version-2 document with the same sources is: the same derivation, the
//     same lineage and the same canonical form. Its preparation's version is
//     3, as the document's is (§9.1), and its mappingDigest is the digest of
//     the document's canon bytes (§2).
//   - An unbound operation names the catalog platform and operation it is
//     read through, the shape that serves it and the record it is written
//     against, and no input profile (§4.4). Runner binds it at import: the
//     request names the published dialect and a binding document, which
//     binds each unbound source to an input profile of this installation,
//     and Runner keeps the bound dialect as the run's mapping, the documents
//     beside it (source.import). A preparation commits the bound dialect by
//     its digest, and neither document's digest.
//   - Of the kinds, operation is version 2's, of every adapter shape, and
//     selected-file is version 2's, read from the Runner's selected
//     snapshot, local-file and google-drive alike: both are prepared as
//     version 2 prepares them. research is prepared from the responses its
//     caller retains, replayed and never asked again (research.go), bound to
//     an AI profile and web profiles, or published unbound and bound at
//     import. http, sql and web are refused by name. None is read as another
//     kind.
//
// A version-2 document is read as before: nothing here changes how one is
// read, kept, digested or prepared.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// dialect says whether the mapping is a dialect document: version 2, the
// Runner's mapping v2, or version 3.
func (m InputMapping) dialect() bool { return m.Version == 2 || m.Version == 3 }

// MarshalJSON is a mapping's encoding, which its digest is taken over. A
// mapping of version 1 or 2 is encoded as it always was, version 2 with
// the version-1 members provider, facts and evidence written empty, as the
// dialect's version 2 records (spec/dialect.md §2). Version 3 carries none
// of them: its encoding is the document with no member Runner adds.
func (m InputMapping) MarshalJSON() ([]byte, error) {
	if m.originalDocument != nil {
		return m.originalDocument, nil
	}
	type plain InputMapping
	if m.FormatVersion != "" {
		return m.marshalArtifact()
	}
	if m.Version != 3 {
		return json.Marshal(plain(m))
	}
	return json.Marshal(struct {
		Profile          string          `json:"profile,omitempty"`
		Case             *CaseMapping    `json:"case,omitempty"`
		Sources          []MappingSource `json:"sources,omitempty"`
		Admits           *Admission      `json:"admits,omitempty"`
		Unmapped         []string        `json:"unmapped,omitempty"`
		UnmappedEvidence []string        `json:"unmappedEvidence,omitempty"`
		Version          int             `json:"version"`
	}{m.Profile, m.Case, m.Sources, m.Admits, m.Unmapped, m.UnmappedEvidence, m.Version})
}

// DialectImport is the import step's request member, source.import: the
// published dialect, of version 3, and the binding document that binds its
// unbound sources (spec/dialect.md §4.4), each kept as given.
type DialectImport struct {
	Dialect json.RawMessage `json:"dialect"`
	Binding json.RawMessage `json:"binding"`
}

// The bounds of a published dialect and of a binding document
// (spec/dialect.md §12): a dialect's is the one a mapping is held to.
const (
	maxPublishedDialect = 128 << 10
	maxBindingDocument  = 64 << 10
)

// version3Member is a member dialect version 3 gives a source, and whether
// the source writes it.
type version3Member struct {
	name  string
	value json.RawMessage
}

// version3Members are a source's members of version 3, in the order the
// specification lists them: an unbound operation's (§4.4), then the kinds'
// (§4.3).
func version3Members(s MappingSource) []version3Member {
	return []version3Member{
		{"platform", s.Platform}, {"operation", s.Operation}, {"shape", s.Shape}, {"record", s.Record},
		{"method", s.Method}, {"path", s.Path}, {"query", s.Query}, {"body", s.Body}, {"headers", s.Headers},
		{"statement", s.Statement}, {"bindings", s.Bindings},
		{"url", s.URL},
		{"question", s.Question}, {"constraints", s.Constraints}, {"model", s.Model}, {"evidence", s.Evidence},
		{"fields", s.Fields}, {"capabilities", s.Capabilities}, {"webProfiles", s.WebProfiles},
	}
}

// researchMembers are the members of version 3 a research source has
// (§4.3, §4.4): its recipe's and its web assignments.
var researchMembers = map[string]bool{"question": true, "constraints": true, "model": true, "evidence": true, "fields": true, "capabilities": true, "webProfiles": true}

// unboundMembers are the members an unbound operation names in place of
// its profile (§4.4).
var unboundMembers = map[string]bool{"platform": true, "operation": true, "shape": true, "record": true}

// unboundOperation says whether a source names any member of an unbound
// operation: one that names one names all four, and no profile, or is
// refused (sourceForm).
func unboundOperation(s MappingSource) bool {
	return len(s.Platform) > 0 || len(s.Operation) > 0 || len(s.Shape) > 0 || len(s.Record) > 0
}

// refusedKind is the refusal of a source of a kind of version 3 that
// Runner does not prepare, naming the kind. None is read as another kind.
func refusedKind(s MappingSource) error {
	switch s.Kind {
	case "http":
		return withCode(codeKindUnread, fmt.Errorf("source %s is of kind http, which this Runner does not prepare: a gateway source of shape http is read as an operation", s.Name))
	case "sql":
		return withCode(codeKindUnread, fmt.Errorf("source %s is of kind sql, which this Runner does not prepare: a connection's statement is read as an operation of the shape its gateway source serves", s.Name))
	case "web":
		return withCode(codeKindUnread, fmt.Errorf("source %s is of kind web, which this Runner does not prepare: the gateway's web source is read as an operation of shape http", s.Name))
	}
	return nil
}

// sourceForm holds a source to the members its document's version gives
// its kind. A version-2 source has no member of version 3. A version-3
// source of kind http, sql or web is refused by name. An operation of
// version 3 is bound, naming its profile as version 2's does, or unbound,
// naming exactly the four members of §4.4 in its place; with open it is
// read, as the import step reads a published dialect, and otherwise it is
// refused by name: Runner prepares only a bound dialect. A research source
// has its recipe's members and its web assignments, and its form is
// readResearchRecipe's. Every other member of version 3 is refused on a
// source of another kind.
func sourceForm(version int, s MappingSource, open bool) error {
	members := version3Members(s)
	if version != 3 {
		for _, m := range members {
			if len(m.value) > 0 {
				return fmt.Errorf("source %s has the member %s, which only a source of dialect version 3 has", s.Name, m.name)
			}
		}
		return nil
	}
	if e := refusedKind(s); e != nil {
		return e
	}
	for _, m := range members {
		if len(m.value) > 0 && !(s.Kind == "operation" && unboundMembers[m.name] || isResearch(s) && researchMembers[m.name]) {
			if isResearch(s) {
				return researchFail(codeMalformed, "source %s has the member %s, which a source of kind %s does not have", s.Name, m.name, s.Kind)
			}
			return fmt.Errorf("source %s has the member %s, which a source of kind %s does not have", s.Name, m.name, s.Kind)
		}
	}
	if !unboundOperation(s) {
		return nil
	}
	if len(s.Platform) == 0 || len(s.Operation) == 0 || len(s.Shape) == 0 || len(s.Record) == 0 || s.Profile != "" || s.ProfileDigest != "" {
		return fmt.Errorf("source %s is an unbound operation, which names its platform, operation, shape and record, all four, and no profile or profile digest", s.Name)
	}
	for _, m := range members[:4] {
		var value string
		if json.Unmarshal(m.value, &value) != nil || value == "" || m.name == "shape" && !adapterShapes[value] {
			return fmt.Errorf("source %s is an unbound operation, whose platform, operation and record are non-empty strings and whose shape is airbyte, mcp or http", s.Name)
		}
	}
	if !open {
		return fmt.Errorf("source %s is unbound: it names the platform, operation, shape and record an installation binds, and no input profile. Import the dialect with a binding document (source.import), which binds it to an input profile of this installation, before it is prepared", s.Name)
	}
	return nil
}

// asWritten refuses a dialect of version 3 that Runner would keep with
// other bytes than the ones written: a dialect's digest is the digest of
// the document as written (spec/dialect.md §2), and Runner keeps, digests
// and exports its own encoding of what it read. A version-3 document
// written as Runner encodes it is kept as written; one that writes an
// optional member empty, or the version-1 members provider, facts and
// evidence, which version 3 does not carry, is refused rather than kept
// with another digest.
func asWritten(written []byte, m InputMapping) error {
	given, e := proofCanon(written)
	if e != nil {
		return inputProblem("Mapping v3: " + e.Error())
	}
	kept, e := proofCanon(encode(m))
	if e != nil || !bytes.Equal(given, kept) {
		return inputProblem("Mapping v3: the dialect is written otherwise than Runner keeps it, and would be kept with another digest than its own: write no optional member empty, and no provider, facts or evidence, which version 3 does not carry")
	}
	return nil
}

// keptAsWritten holds the dialect decoded from a request at path, when it is
// of version 3, to asWritten, over the bytes the decoder read. Go's decoder
// takes a member whose name is a field's but for case (strings.EqualFold) as
// that field, and of two such members reads both into it, so a dialect of
// version 3 is read only where each member of its path is named exactly so
// and no member beside it is named so but for case: the canonical form
// admits one spelling. A request that names it otherwise is refused, and a
// lookup by the exact names that finds nothing refuses too: it never skips
// the check. A dialect of version 2, however its path is named, is read as
// before. A request's other faults are refused where they are read.
func keptAsWritten(request []byte, decoded InputMapping, path ...string) error {
	if decoded.Version != 3 && decoded.FormatVersion == "" {
		return nil
	}
	written, e := exactlyAt(request, path...)
	if e != nil {
		return e
	}
	return asWritten(written, decoded)
}

// exactlyAt is the value of the member of request at path, each member of
// the path named exactly so and none beside it named so but for case; any
// other request is refused, naming the member.
func exactlyAt(request []byte, path ...string) (json.RawMessage, error) {
	where := strings.Join(path, ".")
	at := json.RawMessage(request)
	for i, name := range path {
		members, e := exactMembers(at)
		for _, other := range sortedKeys(members) {
			if other != name && strings.EqualFold(other, name) {
				return nil, inputProblem(fmt.Sprintf("Mapping v3: the request names the member %s, which is %s but for case: a dialect of version 3 is read only at %s, each member of the path named exactly so and none beside it named so but for case, so that the document Runner keeps is the one written", other, name, where))
			}
		}
		value, ok := members[name]
		if e != nil || !ok {
			return nil, inputProblem(fmt.Sprintf("Mapping v3: the request has no member %s named exactly so: a dialect of version 3 is read only at %s, so that the document Runner keeps is the one written", strings.Join(path[:i+1], "."), where))
		}
		at = value
	}
	return at, nil
}

// namedAt says whether the decoder read a member of raw at path: at each
// step, a member named so or so but for case, as Go's decoder reads one.
func namedAt(raw json.RawMessage, path ...string) bool {
	if len(path) == 0 {
		return true
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return false
	}
	for name, value := range members {
		if strings.EqualFold(name, path[0]) && namedAt(value, path[1:]...) {
			return true
		}
	}
	return false
}

// importedInput is i with its import, if it names one, bound under
// profiles (importedSource).
func importedInput(i Input, profiles []InputProfile) (Input, error) {
	if i.Source == nil || i.Source.Import == nil {
		return i, nil
	}
	c, e := importedSource(i.Source, profiles)
	if e != nil {
		return i, e
	}
	i.Source = c
	return i, nil
}

// importedSource is a copy of src with its import bound under profiles:
// its mapping is the bound dialect the import makes, made from the import
// only when src names no mapping, and refused when src names another. A
// request that names a mapping member names a mapping, whatever its value,
// {} and null included (mappingGiven); a source built otherwise names none
// by the empty mapping. A source with no import is copied as it is. The
// documents are kept as given.
func importedSource(src *SourceInput, profiles []InputProfile) (*SourceInput, error) {
	if src == nil {
		return nil, nil
	}
	if e := src.ConversionContract.validate("preparation"); e != nil {
		return nil, e
	}
	c := *src
	c.Mapping.selectedConversion = c.ConversionContract
	if c.Import == nil {
		return &c, nil
	}
	bound, e := bindDialect(*c.Import, profiles, c.ConversionContract)
	if e != nil {
		return nil, e
	}
	if !c.mappingGiven && bytes.Equal(encode(c.Mapping), encode(InputMapping{})) {
		c.Mapping = bound
		return &c, nil
	}
	given, e1 := proofCanon(encode(c.Mapping))
	made, e2 := proofCanon(encode(bound))
	if e1 != nil || e2 != nil || !bytes.Equal(given, made) {
		return nil, inputProblem("Mapping v3: the mapping is not the bound dialect its import makes: name the import alone, or the import and the mapping it binds")
	}
	return &c, nil
}

// bindDialect is the import step (spec/dialect.md §4.4). It reads the
// published dialect and the binding document as given, and makes the bound
// dialect: each unbound source with its platform, operation, shape and
// record removed and the profile the binding document gives it added,
// every other member as written. It refuses, by name, a published dialect
// that is not of version 3, has no unbound source, or would be kept with
// another digest than its own; a binding document for another dialect;
// one that leaves an unbound source unbound, names a source the dialect
// does not have or one that is bound already, or names a member other than
// a profile's id and digest, which would change something else than the
// profile assignments; and a profile this installation does not hold by
// that id and digest, or whose shape is not the source's shape, or whose
// adapter is not the source's record. A research source is bound to its
// AI profile and its web profiles (bindResearch). The bound dialect is then
// held to the form every dialect is.
func bindDialect(imp DialectImport, profiles []InputProfile, selection ...*ConversionContract) (InputMapping, error) {
	fail := func(format string, a ...any) (InputMapping, error) {
		return InputMapping{}, inputProblem("Mapping v3: " + fmt.Sprintf(format, a...))
	}
	// mismatch is fail for a profile that does not fit its source
	// (spec/dialect.md §4.4, §5.1).
	mismatch := func(format string, a ...any) (InputMapping, error) {
		return InputMapping{}, codedProblem(codeMismatch, "Mapping v3: "+fmt.Sprintf(format, a...))
	}
	absent := func(raw json.RawMessage) bool {
		return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	}
	if absent(imp.Dialect) || absent(imp.Binding) {
		return fail("an import names the published dialect and its binding document")
	}
	if e := selectedConversion(selection).validate("preparation"); e != nil {
		return InputMapping{}, e
	}
	if len(imp.Dialect) > artifactLimit(selectedConversion(selection), maxPublishedDialect, columnDocumentLimit) || len(imp.Binding) > maxBindingDocument {
		return fail("a published dialect is at most 128 KiB, and a binding document at most 64 KiB")
	}

	// The published dialect, read as written.
	var m InputMapping
	if e := strictJSON(imp.Dialect, &m); e != nil {
		return fail("the published dialect is not a dialect document: %v", e)
	}
	m.selectedConversion = selectedConversion(selection)
	if m.Version != 3 {
		return fail("the published dialect is of version %d: only a dialect of version 3 has unbound sources to bind, and one of version 2 is given as the mapping", m.Version)
	}
	if e := asWritten(imp.Dialect, m); e != nil {
		return InputMapping{}, e
	}
	if e := validateDialect(m, true); e != nil {
		return InputMapping{}, e
	}
	published, _ := proofCanon(imp.Dialect)
	unbound := map[string]bool{}
	sources := map[string]bool{}
	for _, s := range m.Sources {
		sources[s.Name] = true
		unbound[s.Name] = unboundOperation(s) || unboundResearch(s)
	}
	if !anyTrue(unbound) {
		return fail("the dialect has no unbound source, and a binding document beside it binds nothing: give the dialect as the mapping")
	}

	// The binding document: exactly dialect and sources, each source's
	// entry exactly a profile's id and digest.
	if _, e := proofCanon(imp.Binding); e != nil {
		return fail("the binding document is not one JSON document of the gateway's canonical domain: %v", e)
	}
	var binding map[string]json.RawMessage
	if e := strictJSON(imp.Binding, &binding); e != nil || binding == nil {
		return fail("the binding document is not one JSON object")
	}
	for _, name := range sortedKeys(binding) {
		if name != "dialect" && name != "sources" {
			return fail("the binding document has the member %s: it is an object of exactly dialect and sources, which binds each unbound source to a profile and changes nothing else in the dialect", name)
		}
	}
	var named string
	if json.Unmarshal(binding["dialect"], &named) != nil || !validDigest(named) {
		return fail("the binding document's dialect is not the digest of the dialect it binds")
	}
	if named != digest(published) {
		return fail("the binding document binds the dialect %s, and this one's digest is %s", named, digest(published))
	}
	var entries map[string]map[string]json.RawMessage
	if json.Unmarshal(binding["sources"], &entries) != nil || entries == nil {
		return fail("the binding document's sources are not an object giving each unbound source its profile")
	}
	for _, name := range sortedKeys(entries) {
		if !sources[name] {
			return fail("the binding document binds %s, which is not a source of the dialect", name)
		}
		if !unbound[name] {
			return fail("the binding document binds %s, which is not an unbound source: a binding binds unbound sources only", name)
		}
	}

	// Each unbound source, in the dialect's order, bound to a profile this
	// installation holds, of the source's shape and record.
	bound := m
	bound.Sources = append([]MappingSource(nil), m.Sources...)
	for i, s := range bound.Sources {
		if !unbound[s.Name] {
			continue
		}
		entry, ok := entries[s.Name]
		if !ok {
			return fail("the binding document leaves source %s unbound: it binds every unbound source of the dialect", s.Name)
		}
		if isResearch(s) {
			b, e := bindResearch(s, entry, profiles)
			if e != nil {
				return InputMapping{}, inputProblemOf("Mapping v3: "+e.Error(), e)
			}
			bound.Sources[i] = b
			continue
		}
		for _, member := range sortedKeys(entry) {
			if member != "profile" && member != "profileDigest" {
				return fail("the binding document gives source %s the member %s: it binds a source to a profile, by its id and digest, and changes nothing else in the dialect", s.Name, member)
			}
		}
		var id, pinned string
		if json.Unmarshal(entry["profile"], &id) != nil || id == "" || json.Unmarshal(entry["profileDigest"], &pinned) != nil || !validDigest(pinned) {
			return fail("the binding document gives source %s no profile: an id and a digest", s.Name)
		}
		var shape, record string
		_ = json.Unmarshal(s.Shape, &shape)
		_ = json.Unmarshal(s.Record, &record)
		var profile *InputProfile
		for j := range profiles {
			if profiles[j].ID == id {
				profile = &profiles[j]
			}
		}
		switch {
		case profile == nil:
			return mismatch("the binding document binds source %s to the profile %s, which this installation does not hold", s.Name, id)
		case profileHash(*profile) != pinned:
			return mismatch("the binding document binds source %s to the profile %s by a digest that is not the digest of this installation's profile %s", s.Name, id, id)
		case profile.Shape != shape:
			return mismatch("the binding document binds source %s, served by shape %s, to the profile %s, of shape %s", s.Name, shape, id, profile.Shape)
		case profile.Adapter.Name != record:
			return mismatch("the binding document binds source %s, written against the record %s, to the profile %s, whose adapter is %s", s.Name, record, id, profile.Adapter.Name)
		}
		s.Platform, s.Operation, s.Shape, s.Record = nil, nil, nil, nil
		s.Profile, s.ProfileDigest = id, pinned
		bound.Sources[i] = s
	}
	if e := validateDialect(bound, false); e != nil {
		return InputMapping{}, e
	}
	return bound, nil
}

func anyTrue(m map[string]bool) bool {
	for _, v := range m {
		if v {
			return true
		}
	}
	return false
}
