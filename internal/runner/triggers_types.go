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
	Name               string          `json:"name"`
	Kind               string          `json:"kind"` // schedule, event, file
	Cloud              *CloudBinding   `json:"cloud,omitempty"`
	Schedule           *Schedule       `json:"schedule,omitempty"`
	Input              *AutomaticInput `json:"input,omitempty"`
	Missed             string          `json:"missed"`  // skip, latest
	Overlap            string          `json:"overlap"` // skip, queue
	PreparationSeconds int             `json:"preparationSeconds,omitempty"`
	QueueSeconds       int             `json:"queueSeconds"`
	WatchPath          string          `json:"watchPath,omitempty"`
	StableSeconds      int             `json:"stableSeconds,omitempty"`
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
	// Scope is the scope identifier of the trigger's job (scope.go). A
	// trigger of a job created under none has none.
	Scope string `json:"scope,omitempty"`
	// Persistent file observer cursor, excluded from public API responses.
	observed, pending, pendingAt string
	// keyRevision is the revision at which the trigger's current event key
	// was issued: the key's identity in the journal, never the key or its
	// digest. Nil for a key issued before the journal began, or no key.
	keyRevision *int
	// version is the row version the store held the trigger at when it was
	// read, 0 for one made in the writer's transaction: putTrigger expects
	// it, so that a writer that read the trigger before another wrote it
	// does not write over that write (docs/design/store-postgresql.md,
	// D17). It changes with every write, a tick's included, where the
	// revision does not. The sqlite kind holds none and reads 0.
	version int64
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
	// Paging is what the occurrence read of each paged source
	// (paging_loop.go): its pages, each page's receipt, the loop's end, a
	// full re-read, and the cursor's advance, which the run writes when it
	// completes. A run of no paged source has none.
	Paging []PagedRead `json:"paging,omitempty"`
}
type Occurrence struct {
	Preparation        *SourcePreparation `json:"preparation,omitempty"`
	PreparationSeconds int                `json:"preparationSeconds,omitempty"`
	QueueSeconds       int                `json:"queueSeconds,omitempty"`
	ReadyUntil         string             `json:"readyUntil,omitempty"`
	ID                 string             `json:"id"`
	JobID              string             `json:"jobId"`
	ReleaseID          string             `json:"releaseId"`
	JobRevision        int                `json:"jobRevision"`
	TriggerID          string             `json:"triggerId"`
	TriggerRevision    int                `json:"triggerRevision"`
	Kind               string             `json:"kind"`
	EventID            string             `json:"eventId,omitempty"`
	ScheduledAt        string             `json:"scheduledAt,omitempty"`
	ReceivedAt         string             `json:"receivedAt"`
	ExpiresAt          string             `json:"expiresAt"`
	InputDigest        string             `json:"inputDigest,omitempty"`
	State              string             `json:"state"` // accepted, submitted, skipped, failed, expired
	RunID              string             `json:"runId,omitempty"`
	Reason             string             `json:"reason,omitempty"`
	MissedFrom         string             `json:"missedFrom,omitempty"`
	MissedThrough      string             `json:"missedThrough,omitempty"`
	PendingInput       *AutomaticInput    `json:"pendingInput,omitempty"`
	Input              *Input             `json:"input,omitempty"`
	// Scope is the scope identifier of the occurrence's job (scope.go). An
	// occurrence of a job created under none has none.
	Scope string `json:"scope,omitempty"`
	// Attempts are the occurrence's source acquisition attempts, for a job
	// with a retry policy (retries.go), and NextAttemptAt the time of the
	// next, while it waits for one. An occurrence of a job without a policy
	// has neither.
	Attempts      []Attempt `json:"attempts,omitempty"`
	NextAttemptAt string    `json:"nextAttemptAt,omitempty"`
	// HeldPages are a paged source's pages received before a throttle,
	// while the occurrence waits for the attempt that resumes at the
	// throttled page (paging_loop.go); Paging is what it read of each paged
	// source once its input is acquired, carried into its run. HeldPages are
	// private: no route serves them (publicOccurrence).
	HeldPages *HeldPages  `json:"heldPages,omitempty"`
	Paging    []PagedRead `json:"paging,omitempty"`
	// ErrorClass is the class, from a closed list, of an occurrence that
	// ended failed or expired (run_results.go), beside its reason. Any other
	// occurrence, and one that ended before Runner kept it, has none.
	ErrorClass string `json:"errorClass,omitempty"`
	// claim is the claim its writer holds it under, set by claimOccurrence
	// and never read back from the store (docs/design/store-postgresql.md,
	// D18): putOccurrence fences the write by it. An occurrence read
	// from the store holds none, so that its write is refused while another
	// Runner holds it.
	claim occurrenceClaim
}
type EventDelivery struct {
	ID         string `json:"id"`
	OccurredAt string `json:"occurredAt"`
	Input      Input  `json:"input"`
}

// RecordDelivery is an installation-owned durable outbox entry. Unlike public
// credential deliveries it is bound to a release and can survive downtime.
type RecordDelivery struct {
	ReleaseID string        `json:"releaseId"`
	Event     EventDelivery `json:"event"`
}
