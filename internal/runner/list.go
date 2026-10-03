package runner

import (
	"encoding/json"
	"math"
	"strings"
)

type recordFilter struct {
	Search string
	State  string
	Review bool
}

func validRunState(state string) bool {
	switch state {
	case "queued", "running", "completed", "failed", "interrupted":
		return true
	}
	return false
}

// Filter before pagination. Search is a literal substring, never a SQL pattern.
// Lists retain only public summaries, with no input snapshots or audit bodies.
func (s *Service) filteredRecords(kind, jobID string, after int64, filter recordFilter) ([]json.RawMessage, int64, error) {
	if after == 0 {
		after = math.MaxInt64
	}
	where := []string{"t.seq<?"}
	args := []any{after}
	table, joins := "jobs", " JOIN releases release ON release.id=t.release_id"
	extra := "json_extract(release.record,'$.title'),json_extract(release.record,'$.packVersion')"
	search := "json_extract(t.record,'$.name')"
	if kind == "runs" {
		table = "runs"
		joins = " JOIN jobs job ON job.id=t.job_id"
		extra = "json_extract(job.record,'$.name'),''"
		search = "t.id || ' ' || json_extract(job.record,'$.name')"
		if jobID != "" {
			where = append(where, "t.job_id=?")
			args = append(args, jobID)
		}
		if filter.State != "" {
			where = append(where, "t.state=?")
			args = append(args, filter.State)
		}
		if filter.Review {
			where = append(where, `(t.state IN ('failed','interrupted') OR (t.state='completed' AND (json_extract(t.record,'$.result.disposition.kind')='unresolved' OR json_extract(t.record,'$.result.disposition.handoff.state')='requested')))`)
		}
	}
	if filter.Search != "" {
		where = append(where, "instr(lower("+search+"),lower(?))>0")
		args = append(args, filter.Search)
	}
	rows, err := s.db.Query("SELECT t.seq,t.record,"+extra+" FROM "+table+" t"+joins+" WHERE "+strings.Join(where, " AND ")+" ORDER BY t.seq DESC LIMIT 51", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	summaries := []map[string]json.RawMessage{}
	var last, next int64
	for rows.Next() {
		var seq int64
		var raw []byte
		var label, version string
		if err = rows.Scan(&seq, &raw, &label, &version); err != nil {
			return nil, 0, err
		}
		if len(summaries) == 50 {
			next = last
			break
		}
		var summary map[string]json.RawMessage
		if err = json.Unmarshal(raw, &summary); err != nil {
			return nil, 0, err
		}
		if kind == "runs" {
			delete(summary, "input")
			delete(summary, "audit")
			delete(summary, "auditBytes")
			delete(summary, "auditSignatures")
			summary["jobName"] = encode(label)
			if raw := summary["result"]; len(raw) > 0 {
				var result map[string]json.RawMessage
				if err = json.Unmarshal(raw, &result); err != nil {
					return nil, 0, err
				}
				summary["result"] = encode(map[string]json.RawMessage{"disposition": result["disposition"], "handoffTarget": result["handoffTarget"]})
			}
		} else {
			summary["packTitle"] = encode(label)
			summary["packVersion"] = encode(version)
		}
		summaries = append(summaries, summary)
		last = seq
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	// The store intentionally permits one connection. Close this cursor before
	// querying recent runs; do not deadlock by holding it across another query.
	if err = rows.Close(); err != nil {
		return nil, 0, err
	}
	if kind == "jobs" && len(summaries) > 0 {
		ids := []any{}
		slots := []string{}
		byID := map[string]map[string]json.RawMessage{}
		recent := map[string][]json.RawMessage{}
		for _, item := range summaries {
			var id string
			if err = json.Unmarshal(item["id"], &id); err != nil {
				return nil, 0, err
			}
			ids = append(ids, id)
			slots = append(slots, "?")
			byID[id] = item
			recent[id] = []json.RawMessage{}
		}
		recentRows, e := s.db.Query(`SELECT job_id,id,state,created FROM (SELECT job_id,id,state,json_extract(record,'$.createdAt') AS created,row_number() OVER(PARTITION BY job_id ORDER BY seq DESC) AS rank FROM runs WHERE job_id IN (`+strings.Join(slots, ",")+`)) WHERE rank<=5 ORDER BY job_id,rank`, ids...)
		if e != nil {
			return nil, 0, e
		}
		for recentRows.Next() {
			var job, id, state, created string
			if e = recentRows.Scan(&job, &id, &state, &created); e != nil {
				recentRows.Close()
				return nil, 0, e
			}
			recent[job] = append(recent[job], encode(map[string]string{"id": id, "state": state, "createdAt": created}))
		}
		e = recentRows.Err()
		recentRows.Close()
		if e != nil {
			return nil, 0, e
		}
		for id, item := range byID {
			item["recentRuns"] = encode(recent[id])
		}
		triggerRows, e := s.db.Query("SELECT job_id,record FROM triggers WHERE job_id IN ("+strings.Join(slots, ",")+") ORDER BY seq", ids...)
		if e != nil {
			return nil, 0, e
		}
		triggers := map[string][]any{}
		for triggerRows.Next() {
			var job, raw string
			var t Trigger
			if e = triggerRows.Scan(&job, &raw); e == nil {
				e = json.Unmarshal([]byte(raw), &t)
			}
			if e != nil {
				triggerRows.Close()
				return nil, 0, e
			}
			triggers[job] = append(triggers[job], map[string]any{"id": t.ID, "kind": t.Config.Kind, "paused": t.Paused, "nextAt": t.NextAt})
		}
		e = triggerRows.Err()
		triggerRows.Close()
		if e != nil {
			return nil, 0, e
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
