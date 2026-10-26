package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"time"
)

// ConversionCapability is trusted installation metadata, separate from a
// caller's selector. Existing profile approval and digest bind producer support.
type ConversionCapability struct {
	Contract        string         `json:"contract"`
	Spec            ConversionSpec `json:"spec"`
	Profile         string         `json:"profile"`
	Role            string         `json:"role"`
	Limits          map[string]int `json:"limits"`
	ProducerGrammar string         `json:"producerGrammar,omitempty"`
}

func conversionRoleLimits(role string) map[string]int {
	switch role {
	case "document-validation":
		return map[string]int{"documentBytes": columnDocumentLimit, "frameBytes": columnFrameLimit}
	case "producer-carrier":
		return map[string]int{"originalSnapshotBytes": 524288, "carrierBytes": columnCarrierLimit, "grantBytes": columnCarrierLimit, "adapterStdoutBytes": columnCarrierLimit, "responseBytes": columnResponseLimit, "profilesBytes": columnProfilesLimit}
	case "preview":
		return map[string]int{"documentBytes": columnDocumentLimit, "combinedBytes": columnCombinedLimit, "frameBytes": columnFrameLimit, "profilesBytes": columnProfilesLimit, "requestBodyBytes": columnRequestLimit, "inputReplyBytes": columnReplyLimit}
	case "preparation", "preparation-verification":
		return map[string]int{"documentBytes": columnDocumentLimit, "combinedBytes": columnCombinedLimit, "frameBytes": columnFrameLimit, "preparationBytes": columnPreparationLimit, "recordBytes": columnRecordLimit, "profilesBytes": columnProfilesLimit, "requestBodyBytes": columnRequestLimit, "inputReplyBytes": columnReplyLimit, "releaseStoredBytes": columnStoredLimit, "runStoredBytes": columnStoredLimit, "exportBaseBytes": columnExportBaseLimit, "exportWireBytes": ColumnExportWireLimit}
	}
	return nil
}
func (c *ConversionCapability) UnmarshalJSON(raw []byte) error {
	type plain ConversionCapability
	var p plain
	if strictJSON(raw, &p) != nil {
		return conversionFailure(codeMalformed)
	}
	m, e := exactMembers(raw)
	if e != nil || len(m) < 5 || len(m) > 6 {
		return conversionFailure(codeMalformed)
	}
	for _, k := range []string{"contract", "spec", "profile", "role", "limits"} {
		if m[k] == nil {
			return conversionFailure(codeMalformed)
		}
	}
	// Five required members; producerGrammar is the only optional member.
	if len(m) != 5 && len(m) != 6 {
		return conversionFailure(codeMalformed)
	}
	for k := range m {
		if k != "contract" && k != "spec" && k != "profile" && k != "role" && k != "limits" && k != "producerGrammar" {
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
	for _, k := range []string{"role"} {
		v := bytes.TrimSpace(m[k])
		if len(v) == 0 || v[0] != '"' {
			return conversionFailure(codeMalformed)
		}
	}
	if v, ok := m["producerGrammar"]; ok {
		if p.Role != "preparation" && p.Role != "preparation-verification" && p.Role != "producer-carrier" {
			return conversionFailure(codeMalformed)
		}
		if p.ProducerGrammar == "" {
			return conversionFailure(codeMalformed)
		}
		v = bytes.TrimSpace(v)
		if len(v) == 0 || v[0] != '"' {
			return conversionFailure(codeMalformed)
		}
	}
	limits, e := exactMembers(m["limits"])
	if e != nil {
		return conversionFailure(codeMalformed)
	}
	for _, v := range limits {
		n, e := strconv.ParseInt(string(v), 10, 64)
		if e != nil || n <= 0 || n > 9007199254740991 || strconv.FormatInt(n, 10) != string(v) {
			return conversionFailure(codeMalformed)
		}
	}
	*c = ConversionCapability(p)
	return c.validate()
}
func (c ConversionCapability) validate() error {
	if c.Contract != "retained-column/1" || c.Spec != installedConversionSpec || c.Profile != "base" {
		return conversionFailure("capability-unsupported")
	}
	expected := conversionRoleLimits(c.Role)
	if expected == nil {
		return conversionFailure("capability-unsupported")
	}
	if len(c.Limits) != len(expected) {
		return conversionFailure(codeMalformed)
	}
	for k := range expected {
		if c.Limits[k] <= 0 || c.Limits[k] > 9007199254740991 {
			return conversionFailure(codeMalformed)
		}
	}
	grammar := c.Role == "preparation" || c.Role == "preparation-verification" || c.Role == "producer-carrier"
	if grammar && c.ProducerGrammar != "" && c.ProducerGrammar != columnProducerGrammar {
		return conversionFailure("capability-unsupported")
	}
	if !grammar && c.ProducerGrammar != "" {
		return conversionFailure(codeMalformed)
	}
	return nil
}
func validateConversionCapabilities(cs []ConversionCapability) error {
	if len(cs) > 32 {
		return conversionFailure(codeMalformed)
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if e := c.validate(); e != nil {
			return e
		}
		key := c.Role + "/" + c.ProducerGrammar
		if seen[key] {
			return conversionFailure(codeMalformed)
		}
		seen[key] = true
	}
	return nil
}

// Effective limits never enlarge this installation's profile. A lower positive
// report is supported; owners must measure their actual complete artifacts.
func conversionCapabilityLimits(cs []ConversionCapability, role string, producer bool) (map[string]int, error) {
	if e := validateConversionCapabilities(cs); e != nil {
		return nil, e
	}
	for _, c := range cs {
		if c.Role != role || producer && c.ProducerGrammar != columnProducerGrammar {
			continue
		}
		limits := conversionRoleLimits(role)
		for k, n := range limits {
			limits[k] = min(n, c.Limits[k])
		}
		return limits, nil
	}
	return nil, conversionFailure("capability-unsupported")
}
func requireConversionCapability(cs []ConversionCapability, role string, producer bool) error {
	_, e := conversionCapabilityLimits(cs, role, producer)
	return e
}
func conversionArtifactBound(limits map[string]int, key string, size int) error {
	if n, ok := limits[key]; ok && size > n {
		return codedProblem(codeOverBound, "complete retained-column "+key+" exceeds negotiated limit")
	}
	return nil
}

// These are downward-only caller ceilings, derived from an approved profile by
// Runner. Persisting them does not mint producer or installation authority.
func validateProducerLimits(retainedColumn bool, c *ConversionContract, limits map[string]int) error {
	if !retainedColumn {
		if limits != nil {
			return conversionFailure(codeMalformed)
		}
		return nil
	}
	if c == nil || len(limits) != len(conversionRoleLimits("producer-carrier")) {
		return conversionFailure(codeMalformed)
	}
	for k, n := range conversionRoleLimits("producer-carrier") {
		if limits[k] <= 0 || limits[k] > n {
			return conversionFailure(codeMalformed)
		}
	}
	return nil
}
func producerResponseLimit(c *ConversionContract, limits map[string]int) int {
	n := responseLimit(c)
	if v := limits["responseBytes"]; v > 0 {
		n = min(n, v)
	}
	return n
}

// ConversionCapabilities is the experimental installed package's declaration.
// Producer support belongs to each independently approved producer profile.
func ConversionCapabilities(roles ...string) []ConversionCapability {
	out := []ConversionCapability{}
	for _, role := range roles {
		limits := conversionRoleLimits(role)
		if limits == nil {
			continue
		}
		c := ConversionCapability{Contract: "retained-column/1", Spec: installedConversionSpec, Profile: "base", Role: role, Limits: limits}
		if role == "preparation" || role == "preparation-verification" {
			c.ProducerGrammar = columnProducerGrammar
		}
		out = append(out, c)
	}
	return out
}
func runtimeConversionAdmission(ctx context.Context, bin, dir string, c *ConversionContract, starters ...func(*exec.Cmd) error) error {
	_, e := runtimeConversionLimits(ctx, bin, dir, c, starters...)
	return e
}
func runtimeConversionLimits(ctx context.Context, bin, dir string, c *ConversionContract, starters ...func(*exec.Cmd) error) (map[string]int, error) {
	if c == nil {
		return nil, nil
	}
	if e := c.validate("document-validation"); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "dialect", "contract", "--draft-contract", c.Contract+"@"+c.Spec.Revision, "--draft-profile", "base", "--format", "json")
	cmd.Dir = dir
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var out, errout bytes.Buffer
	cmd.Stdout = &boundedWriter{&out, 80 << 10}
	cmd.Stderr = &boundedWriter{&errout, 64 << 10}
	start := (*exec.Cmd).Start
	if len(starters) > 0 {
		start = starters[0]
	}
	if e := start(cmd); e != nil {
		return nil, e
	}
	if e := cmd.Wait(); e != nil {
		return nil, conversionFailure("capability-unsupported")
	}
	var root map[string]json.RawMessage
	if strictJSON(out.Bytes(), &root) != nil {
		return nil, conversionFailure(codeMalformed)
	}
	var capabilities map[string]json.RawMessage
	if strictJSON(root["capabilities"], &capabilities) != nil {
		return nil, conversionFailure("capability-unsupported")
	}
	var cs []ConversionCapability
	if _, exists := capabilities["conversionContracts"]; !exists {
		return nil, conversionFailure("capability-unsupported")
	}
	if err := strictJSON(capabilities["conversionContracts"], &cs); err != nil {
		_, code := verificationOutcome(err)
		if code != "capability-unsupported" {
			code = codeMalformed
		}
		return nil, conversionFailure(code)
	}
	return conversionCapabilityLimits(cs, "document-validation", false)
}

// Check each document and frame that the selected Runtime will consume before
// creating an evaluation directory or invoking validation/evaluation.
func runtimeConversionInput(limits map[string]int, input Input, pack string) error {
	if limits == nil {
		return nil
	}
	if e := conversionArtifactBound(limits, "documentBytes", len(pack)); e != nil {
		return e
	}
	c := input.Source
	if c == nil {
		return nil
	}
	for _, raw := range [][]byte{encode(c.Mapping)} {
		if e := conversionArtifactBound(limits, "documentBytes", len(raw)); e != nil {
			return e
		}
	}
	if c.Import != nil {
		if e := conversionArtifactBound(limits, "documentBytes", len(c.Import.Dialect)); e != nil {
			return e
		}
	}

	checkFrame := func(raw []byte, pointer string) error {
		frame, ok := rawPointer(raw, pointer)
		if !ok {
			return nil
		} // shared preparation owns missing/malformed reads
		return conversionArtifactBound(limits, "frameBytes", len(frame))
	}
	caseRaw, caseErr := columnCaseBytes(c)
	if caseErr != nil {
		return caseErr
	}
	if c.Mapping.Case != nil {
		for _, f := range c.Mapping.Case.Facts {
			if len(f.Conversion) > 0 {
				if e := checkFrame(caseRaw, f.Source); e != nil {
					return e
				}
			}
		}
		for _, f := range c.Mapping.Case.Evidence {
			if len(f.Availability) > 0 {
				if e := checkFrame(caseRaw, f.Source); e != nil {
					return e
				}
			}
		}
	}
	for _, source := range c.Mapping.Sources {
		if source.Read.Copy == nil || !hasColumnCopy(*source.Read.Copy) {
			continue
		}
		v := c.Sources[source.Name]
		raw := v.Response
		if v.RetainedResponseJSON != "" {
			raw = []byte(v.RetainedResponseJSON)
		}
		envelope, e := exactMembers(raw)
		if e == nil {
			if e = checkFrame(envelope["result"], "/retained"); e != nil {
				return e
			}
		}
	}
	return nil
}

// The serialized list is the artifact for native host profiles. Offline file
// readers additionally retain the measured received list, including whitespace.
func conversionProfilesBytes(profiles []InputProfile) int {
	n := len(encode(profiles))
	for _, p := range profiles {
		n = max(n, p.profilesDocumentBytes)
	}
	return n
}
