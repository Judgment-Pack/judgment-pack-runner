package runner

import "encoding/json"

// InputProfile is installation authority, supplied only by the trusted host.
// Its digest pins classification and adapter expectations into a release.
// Classification describes acquisition, not the ultimate authorship of bytes.
type InputProfile struct {
	ID        string     `json:"id"`
	PublicKey string     `json:"publicKey"`
	Class     string     `json:"class"`
	Source    string     `json:"source"`
	Authority string     `json:"authority"`
	Shape     string     `json:"shape"`
	Adapter   AdapterPin `json:"adapter"`
	Endpoint  *string    `json:"endpoint"`
	Tools     []string   `json:"tools,omitempty"`
}
type AdapterPin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}
type CaseMapping struct {
	Parameters map[string]Parameter `json:"parameters,omitempty"`
	Facts      []FactMapping        `json:"facts"`
	Evidence   []EvidenceMapping    `json:"evidence"`
}
type Parameter struct {
	From    string `json:"from,omitempty"`
	Pointer string `json:"pointer"`
	Type    string `json:"type"`
}
type MappingSource struct {
	Name          string               `json:"name"`
	Kind          string               `json:"kind"`
	Provider      string               `json:"provider,omitempty"`
	Profile       string               `json:"profile,omitempty"`
	ProfileDigest string               `json:"profileDigest,omitempty"`
	MaxAge        int                  `json:"maxAge,omitempty"`
	Parameters    map[string]Parameter `json:"parameters,omitempty"`
	Arguments     json.RawMessage      `json:"arguments,omitempty"`
	Read          SourceRead           `json:"read"`
}
type SourceRead struct {
	Unwrap []string        `json:"unwrap,omitempty"`
	Copy   *CopyMapping    `json:"copy,omitempty"`
	Rule   json.RawMessage `json:"rule,omitempty"`
}
type CopyMapping struct {
	Facts    []FactMapping     `json:"facts"`
	Evidence []EvidenceMapping `json:"evidence"`
}
type Admission struct {
	Facts    map[string][]string `json:"facts,omitempty"`
	Evidence map[string][]string `json:"evidence,omitempty"`
}
type SourceValue struct {
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}
type Citation struct {
	SessionID string `json:"sessionId"`
	CallIndex int    `json:"callIndex"`
	Signature string `json:"signature"`
}
type Dependency struct {
	Parameter string `json:"parameter"`
	Source    string `json:"source"`
	Pointer   string `json:"pointer"`
}
type TargetLineage struct {
	Target             string       `json:"target"`
	Kind               string       `json:"kind"`
	Source             string       `json:"source"`
	Class              string       `json:"class"`
	GeneratedInfluence bool         `json:"generatedInfluence"`
	Present            bool         `json:"present"`
	Status             string       `json:"status"`
	Reason             string       `json:"reason"`
	ReadDigest         string       `json:"readDigest"`
	Basis              []string     `json:"basis"`
	Candidates         []string     `json:"candidateFrom,omitempty"`
	Dependencies       []Dependency `json:"dependencies,omitempty"`
	Receipt            *Citation    `json:"receipt,omitempty"`
}
type SourceOutcome struct {
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason"`
	Parameters json.RawMessage `json:"parameters"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Claim      json.RawMessage `json:"claim"`
}
type Preparation struct {
	Version       int             `json:"version"`
	VerifiedAt    string          `json:"verifiedAt"`
	MappingDigest string          `json:"mappingDigest"`
	Verification  string          `json:"verification"`
	Cites         []Citation      `json:"cites"`
	Lineage       []TargetLineage `json:"lineage"`
	Outcomes      []SourceOutcome `json:"outcomes"`
}
type ruleDocument struct {
	Version    string            `json:"ruleVersion"`
	Parameters map[string]string `json:"parameters"`
	Clauses    []ruleClause      `json:"clauses"`
}
type ruleClause struct {
	When   json.RawMessage `json:"when"`
	Claim  ruleClaim       `json:"claim"`
	Reason string          `json:"reason"`
}
type ruleClaim struct {
	Facts    []ruleFact        `json:"facts"`
	Evidence map[string]string `json:"evidence"`
	Status   string            `json:"acquisitionStatus"`
}
type ruleFact struct {
	Pointer string `json:"pointer"`
	From    string `json:"from"`
}
type derivedClaim struct {
	Facts    json.RawMessage   `json:"facts"`
	Evidence map[string]string `json:"evidenceAvailability"`
	Status   string            `json:"acquisitionStatus"`
	Reason   string            `json:"reason"`
	Basis    []string          `json:"basis"`
}
