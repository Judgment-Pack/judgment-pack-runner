package runner

// The in-memory kind's listings, keeping what the sqlite kind's statements
// select (store_sqlite_lists.go), each member read as json_extract reads it.

import (
	"encoding/json"
	"slices"
	"strings"
)

// memberAt is a record's member at path as text, and whether the record
// holds it as text: json_extract's value at '$.a.b', which a condition on it
// compares as text. A member under one that is not an object is none.
func memberAt(record []byte, path ...string) (string, bool, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(record, &members); err != nil {
		return "", false, storeFailure(faultUnreadable, "record", err)
	}
	for i, name := range path {
		raw, ok := members[name]
		if !ok {
			return "", false, nil
		}
		if i == len(path)-1 {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				return "", false, nil
			}
			return text, true, nil
		}
		members = nil
		if json.Unmarshal(raw, &members) != nil || members == nil {
			return "", false, nil
		}
	}
	return "", false, nil
}

// listingLabel is a listing's label: the member as text. A member not held as
// text is SQL's NULL, which the sqlite kind cannot read as a label.
func listingLabel(record []byte, name string) (string, error) {
	text, held, err := memberAt(record, name)
	if err == nil && !held {
		err = storeFailure(faultUnreadable, "list", errNotHeld)
	}
	return text, err
}

// listingSearched says whether text holds search, ASCII case folded.
func listingSearched(text string, held bool, search string) bool {
	return search == "" || held && strings.Contains(asciiLower(text), asciiLower(search))
}

// listingScoped says whether record carries scope, or scope is "".
func listingScoped(record []byte, scope string) (bool, error) {
	if scope == "" {
		return true, nil
	}
	held, ok, err := memberAt(record, "scope")
	return ok && held == scope, err
}

// memoryJobOrder is each job's order, by the order its first revision was
// made in, and the release its last revision names: jobs.seq and
// jobs.release_id.
func (m *memoryStore) memoryJobOrder() ([]string, map[string]string) {
	var order []string
	release := map[string]string{}
	for _, rev := range m.state.revisions {
		if _, seen := release[rev.job]; !seen {
			order = append(order, rev.job)
		}
		release[rev.job] = rev.release
	}
	return order, release
}

func (m *memoryStore) listedJobs(before int64, f recordFilter, limit int) ([]listedRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	order, releases := m.memoryJobOrder()
	items := []listedRecord{}
	for i := len(order) - 1; i >= 0 && len(items) < limit; i-- {
		seq, id := int64(i+1), order[i]
		record, held := m.state.jobs[id]
		release, released := m.state.releases[releases[id]]
		if seq >= before || !held || !released || !m.reads().sees(record) {
			continue
		}
		if keep, err := listingScoped(record, f.Scope); err != nil || !keep {
			if err != nil {
				return nil, err
			}
			continue
		}
		name, named, err := memberAt(record, "name")
		if err != nil {
			return nil, err
		}
		if !listingSearched(name, named, f.Search) {
			continue
		}
		title, err := listingLabel(release, "title")
		if err != nil {
			return nil, err
		}
		version, err := listingLabel(release, "packVersion")
		if err != nil {
			return nil, err
		}
		items = append(items, listedRecord{seq: seq, record: record, label: title, version: version})
	}
	return items, nil
}

func (m *memoryStore) listedRuns(job string, before int64, f recordFilter, limit int) ([]listedRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := []listedRecord{}
	for i := len(m.state.runs) - 1; i >= 0 && len(items) < limit; i-- {
		r := m.state.runs[i]
		jobRecord, held := m.state.jobs[r.job]
		if r.seq >= before || !held || job != "" && r.job != job || f.State != "" && r.state != f.State || !m.reads().sees(r.record) || !m.reads().sees(jobRecord) {
			continue
		}
		keep, err := memoryRunKept(r, f)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		name, named, err := memberAt(jobRecord, "name")
		if err != nil {
			return nil, err
		}
		if !listingSearched(r.id+" "+name, named, f.Search) {
			continue
		}
		title, err := listingLabel(jobRecord, "name")
		if err != nil {
			return nil, err
		}
		items = append(items, listedRecord{seq: r.seq, record: r.record, label: title})
	}
	return items, nil
}

// memoryRunKept says whether f's scope, results and review keep r.
func memoryRunKept(r memoryRun, f recordFilter) (bool, error) {
	if keep, err := listingScoped(r.record, f.Scope); err != nil || !keep {
		return false, err
	}
	if len(f.Results.Outcomes) > 0 {
		kind, _, err := memberAt(r.record, "result", "disposition", "kind")
		if err != nil {
			return false, err
		}
		outcome, held, _ := memberAt(r.record, "result", "disposition", "outcomeId")
		if r.state != "completed" || kind != "outcome" || !held || !slices.Contains(f.Results.Outcomes, outcome) {
			return false, nil
		}
	}
	if len(f.Results.Classes) > 0 {
		class, held, err := memberAt(r.record, "errorClass")
		if err != nil || !held || !slices.Contains(f.Results.Classes, class) {
			return false, err
		}
	}
	if f.Review {
		kind, kindHeld, err := memberAt(r.record, "result", "disposition", "kind")
		if err != nil {
			return false, err
		}
		handoff, handoffHeld, _ := memberAt(r.record, "result", "disposition", "handoff", "state")
		review := r.state == "failed" || r.state == "interrupted" || r.state == "completed" && (kindHeld && kind == "unresolved" || handoffHeld && handoff == "requested")
		if !review {
			return false, nil
		}
	}
	return true, nil
}

func (m *memoryStore) recentRuns(jobs []string, per int) ([]recentRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := slices.Clone(jobs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	items := []recentRun{}
	for _, job := range ids {
		taken := 0
		for i := len(m.state.runs) - 1; i >= 0 && taken < per; i-- {
			r := m.state.runs[i]
			if r.job != job || !m.reads().sees(r.record) {
				continue
			}
			created, err := listingLabel(r.record, "createdAt")
			if err != nil {
				return nil, err
			}
			items = append(items, recentRun{job: r.job, id: r.id, state: r.state, createdAt: created})
			taken++
		}
	}
	return items, nil
}

func (m *memoryStore) listedTriggers(jobs []string) ([]listedTrigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := []listedTrigger{}
	for _, h := range m.state.automation.triggers {
		if !slices.Contains(jobs, h.job) || !m.reads().sees(h.record) {
			continue
		}
		item := listedTrigger{job: h.job}
		if err := json.Unmarshal(h.record, &item.trigger); err != nil {
			return nil, storeFailure(faultUnreadable, "list", err)
		}
		items = append(items, item)
	}
	return items, nil
}
