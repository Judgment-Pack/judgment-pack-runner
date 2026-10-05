// Package runner owns durable operational invocations. It imports no Runtime internals.
package runner

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const MaxBody = 2 << 20
const maxOutput = 8 << 20
const queueLimit = 100

type Input struct {
	Preparation *Preparation    `json:"preparation,omitempty"`
	Facts       json.RawMessage `json:"facts,omitempty"`
	Evidence    json.RawMessage `json:"evidence,omitempty"`
	Source      *SourceInput    `json:"source,omitempty"`
}
type PreviewRequest struct {
	Pack       string      `json:"pack"`
	Input      Input       `json:"input"`
	Matrix     string      `json:"matrix,omitempty"`
	TestSource *TestSource `json:"testSource,omitempty"`
}
type Release struct {
	InputProfiles   []InputProfile  `json:"inputProfiles,omitempty"`
	MappingWarnings []string        `json:"mappingWarnings,omitempty"`
	SchemaVersion   string          `json:"schemaVersion"`
	ID              string          `json:"id"`
	Pack            string          `json:"pack"`
	PackDigest      string          `json:"packDigest"`
	PackID          string          `json:"packId"`
	PackVersion     string          `json:"packVersion"`
	Title           string          `json:"title"`
	RuntimeDigest   string          `json:"runtimeDigest"`
	CreatedAt       string          `json:"createdAt"`
	Config          string          `json:"config"`
	Lock            string          `json:"lock"`
	Validation      json.RawMessage `json:"validation"`
	Sample          Input           `json:"sample"`
	Preview         json.RawMessage `json:"preview"`
	Tests           string          `json:"tests"`
	TestEvidence    *ReleaseTests   `json:"testEvidence,omitempty"`
	InputMapping    *InputMapping   `json:"inputMapping,omitempty"`
}
type Job struct {
	InitialTriggerID     string `json:"initialTriggerId,omitempty"`
	InitialTriggerDigest string `json:"initialTriggerDigest,omitempty"`
	SchemaVersion        string `json:"schemaVersion"`
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	ReleaseID            string `json:"releaseId"`
	Revision             int    `json:"revision"`
	CreatedAt            string `json:"createdAt"`
	Workspace            string `json:"workspace"`
	Owner                string `json:"owner"`
}
type Run struct {
	Trigger       *TriggerOrigin  `json:"trigger,omitempty"`
	SchemaVersion string          `json:"schemaVersion"`
	ID            string          `json:"id"`
	JobID         string          `json:"jobId"`
	ReleaseID     string          `json:"releaseId"`
	Revision      int             `json:"revision"`
	State         string          `json:"state"`
	Input         Input           `json:"input"`
	CreatedAt     string          `json:"createdAt"`
	StartedAt     string          `json:"startedAt,omitempty"`
	FinishedAt    string          `json:"finishedAt,omitempty"`
	RequestedBy   string          `json:"requestedBy"`
	Attempt       int             `json:"attempt"`
	Result        json.RawMessage `json:"result,omitempty"`
	Audit         json.RawMessage `json:"audit,omitempty"`
	// AuditBytes is the audit record exactly as the Runtime wrote it, without
	// the newline that ends its line: what a digest of the record is taken over.
	// Audit is the same record parsed, which Runner encodes again, with other
	// bytes. A []byte encodes as base64, which no JSON encoder changes. Every
	// run Runner now records completed has them; a run recorded before Runner
	// kept them, or one Runner 0.4.0 completed without them, has none.
	AuditBytes []byte `json:"auditBytes,omitempty"`
	// AuditSignatures is the signature sidecar of the attempt's audit trail,
	// signatures.jsonl, exactly as the Runtime wrote it, newlines included:
	// the line that signs the record, when the attempt's Runtime was given a
	// signing key and could sign. A []byte encodes as base64. A run recorded
	// without a key, or before Runner kept the sidecar, has none.
	AuditSignatures []byte `json:"auditSignatures,omitempty"`
	Problem         string `json:"problem,omitempty"`
	// InterruptedAt is when Runner saw this run stop, at an orderly stop: the
	// moment the evaluation it was in was cancelled. FinishedAt keeps its
	// meaning, the time Runner recorded the run's end. A run found running at
	// a restart has none, since Runner did not see it stop; its journal entry
	// gives the window the stop lies in. It is kept and served with the run
	// (storedRun), and is not part of a verification export, whose version-2
	// readers decode strictly and would refuse a new member.
	InterruptedAt string `json:"-"`
}

// storedRun is a run as the store keeps it and the run routes serve it: the
// run, and when Runner saw it interrupted. A run without InterruptedAt encodes
// exactly as Run does.
type storedRun struct {
	Run
	InterruptedAt string `json:"interruptedAt,omitempty"`
}

func runText(r Run) string { return string(encode(storedRun{r, r.InterruptedAt})) }
func decodeRun(b []byte) (Run, error) {
	var held storedRun
	e := json.Unmarshal(b, &held)
	held.Run.InterruptedAt = held.InterruptedAt
	return held.Run, e
}

type apiError struct {
	Status        int
	Code, Message string
}

func (e *apiError) Error() string    { return e.Message }
func bad(code, message string) error { return &apiError{422, code, message} }
func id(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func now() string            { return time.Now().UTC().Format(time.RFC3339Nano) }
func digest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func canonical(b []byte) ([]byte, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("one JSON value required")
	}
	return json.Marshal(v)
}
func (i Input) validate() error {
	if len(i.Facts) == 0 || !json.Valid(i.Facts) {
		return bad("invalid_input", "Supply a JSON facts document.")
	}
	if len(i.Evidence) > 0 {
		var values map[string]string
		if bytes.Equal(bytes.TrimSpace(i.Evidence), []byte("null")) || json.Unmarshal(i.Evidence, &values) != nil {
			return bad("invalid_evidence", "Evidence must map requirement IDs to present, absent or unknown.")
		}
		for k, v := range values {
			if k == "" || (v != "present" && v != "absent" && v != "unknown") {
				return bad("invalid_evidence", "Evidence must map requirement IDs to present, absent or unknown.")
			}
		}
	}
	return nil
}
