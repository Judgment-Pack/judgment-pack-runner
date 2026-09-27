package runner

import "encoding/json"

type AutomaticInput struct {
	Kind  string            `json:"kind"` // constant, input-file, mapped-files
	Value string            `json:"value,omitempty"`
	Path  string            `json:"path,omitempty"` // project-relative input envelope or case file
	Case  json.RawMessage   `json:"case,omitempty"`
	Files map[string]string `json:"files,omitempty"` // named local mapping sources
}
type TriggerConfig struct {
	Name          string          `json:"name"`
	Kind          string          `json:"kind"` // schedule, event, file
	Cloud         *CloudBinding   `json:"cloud,omitempty"`
	Schedule      *Schedule       `json:"schedule,omitempty"`
	Input         *AutomaticInput `json:"input,omitempty"`
	Missed        string          `json:"missed"`  // skip, latest
	Overlap       string          `json:"overlap"` // skip, queue
	QueueSeconds  int             `json:"queueSeconds"`
	WatchPath     string          `json:"watchPath,omitempty"`
	StableSeconds int             `json:"stableSeconds,omitempty"`
}
type Trigger struct {
	ID        string        `json:"id"`
	JobID     string        `json:"jobId"`
	Revision  int           `json:"revision"`
	Authority string        `json:"authority"`
	Config    TriggerConfig `json:"config"`
	Paused    bool          `json:"paused"`
	CreatedAt string        `json:"createdAt"`
	UpdatedAt string        `json:"updatedAt"`
	NextAt    string        `json:"nextAt,omitempty"`
	HasKey    bool          `json:"hasKey"`
	Problem   string        `json:"problem,omitempty"`
	// Persistent file observer cursor, excluded from public API responses.
	observed, pending, pendingAt string
}
type TriggerOrigin struct {
	OccurrenceID    string `json:"occurrenceId"`
	TriggerID       string `json:"triggerId"`
	TriggerRevision int    `json:"triggerRevision"`
	Kind            string `json:"kind"`
	ScheduledAt     string `json:"scheduledAt,omitempty"`
	EventID         string `json:"eventId,omitempty"`
	InputDigest     string `json:"inputDigest,omitempty"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
}
type Occurrence struct {
	ID              string          `json:"id"`
	JobID           string          `json:"jobId"`
	ReleaseID       string          `json:"releaseId"`
	JobRevision     int             `json:"jobRevision"`
	TriggerID       string          `json:"triggerId"`
	TriggerRevision int             `json:"triggerRevision"`
	Kind            string          `json:"kind"`
	EventID         string          `json:"eventId,omitempty"`
	ScheduledAt     string          `json:"scheduledAt,omitempty"`
	ReceivedAt      string          `json:"receivedAt"`
	ExpiresAt       string          `json:"expiresAt"`
	InputDigest     string          `json:"inputDigest,omitempty"`
	State           string          `json:"state"` // accepted, submitted, skipped, failed, expired
	RunID           string          `json:"runId,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	MissedFrom      string          `json:"missedFrom,omitempty"`
	MissedThrough   string          `json:"missedThrough,omitempty"`
	PendingInput    *AutomaticInput `json:"pendingInput,omitempty"`
	Input           *Input          `json:"input,omitempty"`
}
type EventDelivery struct {
	ID         string `json:"id"`
	OccurredAt string `json:"occurredAt"`
	Input      Input  `json:"input"`
}
