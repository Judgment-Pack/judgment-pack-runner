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
func requireConversionCapability(cs []ConversionCapability, role string, producer bool) error {
	if e := validateConversionCapabilities(cs); e != nil {
		return e
	}
	for _, c := range cs {
		if c.Role != role || producer && c.ProducerGrammar != columnProducerGrammar {
			continue
		}
		for k, n := range conversionRoleLimits(role) {
			if c.Limits[k] < n {
				return conversionFailure("capability-unsupported")
			}
		}
		return nil
	}
	return conversionFailure("capability-unsupported")
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
	if c == nil {
		return nil
	}
	if e := c.validate("document-validation"); e != nil {
		return e
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
		return e
	}
	if e := cmd.Wait(); e != nil {
		return conversionFailure("capability-unsupported")
	}
	var root map[string]json.RawMessage
	if strictJSON(out.Bytes(), &root) != nil {
		return conversionFailure(codeMalformed)
	}
	var capabilities map[string]json.RawMessage
	if strictJSON(root["capabilities"], &capabilities) != nil {
		return conversionFailure("capability-unsupported")
	}
	var cs []ConversionCapability
	if _, exists := capabilities["conversionContracts"]; !exists {
		return conversionFailure("capability-unsupported")
	}
	if err := strictJSON(capabilities["conversionContracts"], &cs); err != nil {
		_, code := verificationOutcome(err)
		if code != "capability-unsupported" {
			code = codeMalformed
		}
		return conversionFailure(code)
	}
	return requireConversionCapability(cs, "document-validation", false)
}
