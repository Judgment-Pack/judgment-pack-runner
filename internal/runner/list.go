package runner

import (
	"encoding/json"
	"math"
)

type recordFilter struct {
	Search string
	State  string
	Review bool
	// Scope, when not empty, keeps the records that carry exactly this scope
	// identifier (scope.go), compared byte for byte.
	Scope string
	// Results keeps the runs of the outcomes and error classes named
	// (run_results.go).
	Results resultFilter
}

// A list is a stable public projection, independent of Run's durable encoding.
// Do not embed Run/storedRun or copy arbitrary stored members: private fields
// such as exact manual admission preimages must never become list metadata.
type runListSummary struct {
	Trigger       *TriggerOrigin `json:"trigger,omitempty"`
	SchemaVersion *string        `json:"schemaVersion,omitempty"`
	ID            *string        `json:"id,omitempty"`
	JobID         *string        `json:"jobId,omitempty"`
	ReleaseID     *string        `json:"releaseId,omitempty"`
	Revision      *int           `json:"revision,omitempty"`
	State         *string        `json:"state,omitempty"`
	CreatedAt     *string        `json:"createdAt,omitempty"`
	StartedAt     *string        `json:"startedAt,omitempty"`
	FinishedAt    *string        `json:"finishedAt,omitempty"`
	RequestedBy   *string        `json:"requestedBy,omitempty"`
	Attempt       *int           `json:"attempt,omitempty"`
	Problem       *string        `json:"problem,omitempty"`
	InterruptedAt *string        `json:"interruptedAt,omitempty"`
	Reason        *string        `json:"reason,omitempty"`
	Signing       *RunSigning    `json:"signing,omitempty"`
	Binding       *RunBinding    `json:"binding,omitempty"`
	Scope         *string        `json:"scope,omitempty"`
	ErrorClass    *string        `json:"errorClass,omitempty"`
	Counts        *RunCounts     `json:"counts,omitempty"`
	Result        *runListResult `json:"result,omitempty"`
	JobName       string         `json:"jobName"`
}
type runListResult struct {
	Disposition   json.RawMessage `json:"disposition"`
	HandoffTarget json.RawMessage `json:"handoffTarget"`
}

func validRunState(state string) bool {
	switch state {
	case "queued", "running", "completed", "failed", "interrupted":
		return true
	}
	return false
}

// Filter before pagination, where the store reads (listedRecord). Lists
// retain only public summaries, with no input snapshots or audit bodies.
func (s *Service) filteredRecords(kind, jobID string, after int64, filter recordFilter) ([]json.RawMessage, int64, error) {
	if after == 0 {
		after = math.MaxInt64
	}
	var rows []listedRecord
	var err error
	if kind == "runs" {
		rows, err = s.store.listedRuns(jobID, after, filter, 51)
	} else {
		rows, err = s.store.listedJobs(after, filter, 51)
	}
	if err != nil {
		return nil, 0, err
	}
	summaries := []map[string]json.RawMessage{}
	var last, next int64
	for _, row := range rows {
		if len(summaries) == 50 {
			next = last
			break
		}
		raw := row.record
		var summary map[string]json.RawMessage
		if kind == "runs" {
			var public runListSummary
			if err = json.Unmarshal(raw, &public); err != nil {
				return nil, 0, err
			}
			public.JobName = row.label
			raw = encode(public)
		}
		if err = json.Unmarshal(raw, &summary); err != nil {
			return nil, 0, err
		}
		if kind != "runs" {
			summary["packTitle"] = encode(row.label)
			summary["packVersion"] = encode(row.version)
		}
		summaries = append(summaries, summary)
		last = row.seq
	}
	if kind == "jobs" && len(summaries) > 0 {
		ids := []string{}
		byID := map[string]map[string]json.RawMessage{}
		recent := map[string][]json.RawMessage{}
		for _, item := range summaries {
			var id string
			if err = json.Unmarshal(item["id"], &id); err != nil {
				return nil, 0, err
			}
			ids = append(ids, id)
			byID[id] = item
			recent[id] = []json.RawMessage{}
		}
		runs, e := s.store.recentRuns(ids, 5)
		if e != nil {
			return nil, 0, e
		}
		for _, r := range runs {
			recent[r.job] = append(recent[r.job], encode(map[string]string{"id": r.id, "state": r.state, "createdAt": r.createdAt}))
		}
		for id, item := range byID {
			item["recentRuns"] = encode(recent[id])
		}
		held, e := s.store.listedTriggers(ids)
		if e != nil {
			return nil, 0, e
		}
		triggers := map[string][]any{}
		for _, h := range held {
			t := h.trigger
			triggers[h.job] = append(triggers[h.job], map[string]any{"id": t.ID, "kind": t.Config.Kind, "paused": t.Paused, "nextAt": t.NextAt})
		}
		for id, item := range byID {
			if len(triggers[id]) > 0 {
				item["triggers"] = encode(triggers[id])
			}
		}

	}
	result := make([]json.RawMessage, 0, len(summaries))
	for _, item := range summaries {
		result = append(result, encode(item))
	}
	return result, next, nil
}
