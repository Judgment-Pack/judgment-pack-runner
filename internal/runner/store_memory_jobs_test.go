package runner

// The in-memory kind's jobs as they are made and revised, held as the sqlite
// kind holds them (store_sqlite_jobs.go).

import (
	"encoding/json"
	"slices"
)

func (r memoryReads) jobCreation(release string) (Job, error) {
	var j Job
	b, ok := r.s.creations[release]
	if !ok || !r.sees(b) {
		return j, storeFailure(faultAbsent, "job", errNotHeld)
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, storeFailure(faultUnreadable, "job", err)
	}
	return j, nil
}

func (r memoryReads) releaseUses(release string) (int, error) {
	count := 0
	for _, v := range r.s.revisions {
		if v.release == release && r.sees(v.record) {
			count++
		}
	}
	return count, nil
}

func (r memoryReads) jobRevision(job string, revision int) (Job, string, error) {
	var j Job
	for _, v := range r.s.revisions {
		if v.job == job && v.revision == revision && r.sees(v.record) {
			if err := json.Unmarshal(v.record, &j); err != nil {
				return j, v.request, storeFailure(faultUnreadable, "job", err)
			}
			return j, v.request, nil
		}
	}
	return j, "", storeFailure(faultAbsent, "job", errNotHeld)
}

// revisionReleases are the releases the job revisions name, as the sqlite
// kind's join reads them: a revision whose release is not held is not read.
// Every scope's: a pack or dialect identity is the installation's (D9).
func (r memoryReads) revisionReleases() ([]Release, error) {
	var releases []Release
	for _, v := range r.s.revisions {
		b, ok := r.s.releases[v.release]
		if !ok {
			continue
		}
		var rel Release
		if err := json.Unmarshal(b, &rel); err != nil {
			return nil, storeFailure(faultUnreadable, "release", err)
		}
		releases = append(releases, rel)
	}
	return releases, nil
}

func (r memoryReads) packConflicts(pack, version, digest string) (int, error) {
	releases, err := r.revisionReleases()
	count := 0
	for _, rel := range releases {
		if rel.PackID == pack && rel.PackVersion == version && rel.PackDigest != digest {
			count++
		}
	}
	return count, err
}

// dialectConflicts compares as the sqlite kind's statement does: a release
// that names no dialect has none to conflict.
func (r memoryReads) dialectConflicts(dialect, version, digest string) (int, error) {
	releases, err := r.revisionReleases()
	count := 0
	for _, rel := range releases {
		if d := rel.Dialect; d != nil && d.ID == dialect && d.Version == version && d.Digest != digest {
			count++
		}
	}
	return count, err
}

func (m *memoryStore) jobCreation(release string) (Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().jobCreation(release)
}

func (m *memoryStore) releaseUses(release string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().releaseUses(release)
}

func (m *memoryStore) jobRevision(job string, revision int) (Job, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().jobRevision(job, revision)
}

func (m *memoryStore) packConflicts(pack, version, digest string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().packConflicts(pack, version, digest)
}

func (m *memoryStore) dialectConflicts(dialect, version, digest string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().dialectConflicts(dialect, version, digest)
}

// revisionFree refuses a revision held under job and revision, or naming
// release, in any scope (D9): the latter names job_revisions' key.
func (t *memoryTx) revisionFree(job string, revision int, release string) error {
	if slices.ContainsFunc(t.s.revisions, func(v memoryRevision) bool { return v.job == job && v.revision == revision }) {
		return storeFailure(faultTaken, "job", errTaken)
	}
	if slices.ContainsFunc(t.s.revisions, func(v memoryRevision) bool { return v.release == release }) {
		return takenOn("job", keyJobRevision, errTaken)
	}
	return nil
}

// createJob holds jobs' unique id, job_creations' key and job_revisions'
// keys, for every scope (D9), meeting the creation's key before the
// revision's, so that a release that made a job is named as the creation's.
// jobs' release follows from job_revisions': a job's release is its current
// revision's.
func (t *memoryTx) createJob(j Job) error {
	if err := t.admits("job", encode(j)); err != nil {
		return err
	}
	if _, held := t.s.jobs[j.ID]; held {
		return storeFailure(faultTaken, "job", errTaken)
	}
	if _, held := t.s.creations[j.ReleaseID]; held {
		return takenOn("job", keyJobCreation, errTaken)
	}
	if err := t.revisionFree(j.ID, j.Revision, j.ReleaseID); err != nil {
		return err
	}
	t.s.jobs[j.ID] = encode(j)
	t.s.creations[j.ReleaseID] = encode(j)
	t.s.revisions = append(t.s.revisions, memoryRevision{job: j.ID, revision: j.Revision, release: j.ReleaseID, record: encode(j)})
	return nil
}

func (t *memoryTx) reviseJob(j Job, request string) error {
	if b, held := t.s.jobs[j.ID]; !held || !t.sees(b) {
		return storeFailure(faultAbsent, "job", errNotHeld)
	}
	if err := t.admits("job", encode(j)); err != nil {
		return err
	}
	if err := t.revisionFree(j.ID, j.Revision, j.ReleaseID); err != nil {
		return err
	}
	t.s.jobs[j.ID] = encode(j)
	t.s.revisions = append(t.s.revisions, memoryRevision{job: j.ID, revision: j.Revision, release: j.ReleaseID, record: encode(j), request: request})
	return nil
}

func (t *memoryTx) openOccurrences(job string) (int, error) {
	count := 0
	for _, h := range t.s.automation.occurrences {
		if h.job == job && slices.Contains([]string{"accepted", "preparing", "waiting", "needs-attention"}, h.state) && t.sees(h.record) {
			count++
		}
	}
	return count, nil
}

func (t *memoryTx) triggerRecords(job string) ([]Trigger, error) {
	var triggers []Trigger
	for _, h := range t.s.automation.triggers {
		if h.job != job || !t.sees(h.record) {
			continue
		}
		var tr Trigger
		if err := json.Unmarshal(h.record, &tr); err != nil {
			return nil, storeFailure(faultUnreadable, "trigger", err)
		}
		triggers = append(triggers, tr)
	}
	return triggers, nil
}

func (t *memoryTx) reviseTrigger(tr Trigger) error {
	a := &t.s.automation
	i := slices.IndexFunc(a.triggers, func(h memoryTrigger) bool { return h.id == tr.ID && t.sees(h.record) })
	if i < 0 {
		return storeFailure(faultAbsent, "trigger", errNotHeld)
	}
	if err := t.admits("trigger", encode(tr)); err != nil {
		return err
	}
	if taken, err := a.boundElsewhere(tr); err != nil || taken {
		if err != nil {
			return err
		}
		return takenOn("trigger", keyCloudBinding, errTaken)
	}
	snapshot := memorySnapshot{tr.ID, tr.Revision}
	if _, held := a.snapshots[snapshot]; held {
		return storeFailure(faultTaken, "trigger", errTaken)
	}
	a.triggers[i].record = encode(tr)
	a.snapshots[snapshot] = encode(tr)
	return nil
}
