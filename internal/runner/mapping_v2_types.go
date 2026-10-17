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
	// EndpointScope, when set, is the profile's endpoint constraint in
	// place of one exact endpoint, which is then null: the endpoint prefixes,
	// or the wildcard, a page receipt's endpoint must match (spec/dialect.md
	// §5.1, endpoint_scope.go). Its digest pins the whole scope. Absent, the
	// profile encodes as before.
	EndpointScope EndpointScope `json:"endpointScope,omitempty"`
	Tools         []string      `json:"tools,omitempty"`
	// Calculator, when set, is the installation's statement that each allowed
	// tool computes a deterministic function of the inputs it echoes and the
	// tables it reports, calling no model. Absent, the profile encodes as before.
	Calculator *CalculatorPin `json:"calculator,omitempty"`
	// Research, when set, is what the channel serves of a research
	// source's capabilities, answer, search and page, and the model that
	// answers when it serves answer (spec/dialect.md §5.1): the form a
	// gateway's web-search status row gives as its member research. It is
	// the installation's declaration, never an observation of the model,
	// and its digest pins it. Absent, the profile encodes as before.
	Research *ResearchDeclaration `json:"research,omitempty"`
	// canonDigest is set only on a profile the preparation verifier is
	// given: the digest of its canon bytes as given (spec/dialect.md §5.1,
	// verifierProfiles), which profileHash names for it. An installation's
	// profile has none, and is digested as Runner admitted it.
	canonDigest string
}

// ResearchDeclaration is a profile's research member (spec/dialect.md
// §5.1): capabilities, from answer, search and page in that order, and the
// model exactly when they include answer. Model is a pointer so that a
// model written empty is told from one not written.
type ResearchDeclaration struct {
	Capabilities []string `json:"capabilities"`
	Model        *string  `json:"model,omitempty"`
}
type CalculatorPin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
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
	Calculation   *CalculationBinding  `json:"calculation,omitempty"`
	// The members below are those dialect version 3 gives a source
	// (spec/dialect.md §4.3, §4.4), kept as written so that one written
	// with any value, null included, is told from one not written at all.
	// A version-2 document has none of them. Platform, Operation, Shape and
	// Record are an unbound operation's, which the import step replaces
	// with Profile and ProfileDigest (dialect_v3.go). The others are the
	// members of the kinds http, sql, web and research, which Runner refuses
	// by name: they are read here only so that a source of those kinds is
	// refused for its kind, and not as a request with a member Runner does
	// not know. Question, Constraints, Model and Evidence are a research
	// source's, which Runner prepares from the responses its caller retains
	// (research.go).
	Platform    json.RawMessage `json:"platform,omitempty"`
	Operation   json.RawMessage `json:"operation,omitempty"`
	Shape       json.RawMessage `json:"shape,omitempty"`
	Record      json.RawMessage `json:"record,omitempty"`
	Method      json.RawMessage `json:"method,omitempty"`
	Path        json.RawMessage `json:"path,omitempty"`
	Query       json.RawMessage `json:"query,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	Headers     json.RawMessage `json:"headers,omitempty"`
	Statement   json.RawMessage `json:"statement,omitempty"`
	Bindings    json.RawMessage `json:"bindings,omitempty"`
	URL         json.RawMessage `json:"url,omitempty"`
	Question    json.RawMessage `json:"question,omitempty"`
	Constraints json.RawMessage `json:"constraints,omitempty"`
	Model       json.RawMessage `json:"model,omitempty"`
	Evidence    json.RawMessage `json:"evidence,omitempty"`
	// Fields and Capabilities complete a research source's recipe, and
	// WebProfiles, with Profile and ProfileDigest, its bound form
	// (spec/dialect.md §4.3, §4.4; research.go).
	Fields       json.RawMessage `json:"fields,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	WebProfiles  json.RawMessage `json:"webProfiles,omitempty"`
}

// CalculationBinding binds a calculator's answer to the case: each input it
// echoes to a parameter of the source, and each table it reports to the oldest
// its contents may be, in seconds.
type CalculationBinding struct {
	Inputs map[string]string `json:"inputs"`
	Tables map[string]int    `json:"tables"`
}
type SourceRead struct {
	Unwrap []string        `json:"unwrap,omitempty"`
	Copy   *CopyMapping    `json:"copy,omitempty"`
	Rule   json.RawMessage `json:"rule,omitempty"`
	// Sources is a reconciliation read's earlier research sources, by name,
	// kept as written (spec/dialect.md §6.4): with Rule alone, on a research
	// source of version 3, and on no other read.
	Sources json.RawMessage `json:"sources,omitempty"`
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
	// Receipts is, for an acquired research source, every citation it adds
	// to the preparation's cites, in their order: each retained response's,
	// then the record's own when it has one (spec/dialect.md §9.3). It is
	// written even when empty, and absent for every other source.
	Receipts    *[]Citation         `json:"receipts,omitempty"`
	Calculation *CalculationLineage `json:"calculation,omitempty"`
}

// CalculationLineage records, for a calculated source, which calculator answered,
// its status, where each input it echoed came from, and its tables' instants.
type CalculationLineage struct {
	Calculator CalculatorPin      `json:"calculator"`
	Status     string             `json:"status"`
	Inputs     []CalculationInput `json:"inputs"`
	AsOf       map[string]string  `json:"asOf"`
}
type CalculationInput struct {
	Name      string `json:"name"`
	Parameter string `json:"parameter"`
	Source    string `json:"source"`
	Pointer   string `json:"pointer"`
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
