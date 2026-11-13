package runner

// The sqlite kind's jobs as they are made and revised: the statements the
// Runner made before they stood behind the store contract, unchanged
// (docs/design/store-interface.md, section 9).

import (
	"encoding/json"
	"errors"
)

// errNoJob is the cause of reviseJob's faultAbsent.
var errNoJob = errors.New("no job is held under that id")

func (r sqliteReads) jobCreation(release string) (Job, error) {
	var j Job
	var b []byte
	if err := r.q.QueryRow("SELECT record FROM job_creations WHERE release_id=?", release).Scan(&b); err != nil {
		return j, sqliteFault("job", err, false)
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, storeFailure(faultUnreadable, "job", err)
	}
	return j, nil
}

func (r sqliteReads) releaseUses(release string) (int, error) {
	var count int
	if err := r.q.QueryRow("SELECT count(*) FROM job_revisions WHERE release_id=?", release).Scan(&count); err != nil {
		return 0, sqliteFault("job", err, false)
	}
	return count, nil
}

func (r sqliteReads) jobRevision(job string, revision int) (Job, string, error) {
	var j Job
	var raw, request string
	if err := r.q.QueryRow("SELECT record,request_digest FROM job_revisions WHERE job_id=? AND revision=?", job, revision).Scan(&raw, &request); err != nil {
		return j, "", sqliteFault("job", err, false)
	}
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return j, request, storeFailure(faultUnreadable, "job", err)
	}
	return j, request, nil
}

func (r sqliteReads) packConflicts(pack, version, digest string) (int, error) {
	var count int
	if err := r.q.QueryRow(`SELECT count(*) FROM job_revisions j JOIN releases r ON r.id=j.release_id WHERE json_extract(r.record,'$.packId')=? AND json_extract(r.record,'$.packVersion')=? AND json_extract(r.record,'$.packDigest')<>?`, pack, version, digest).Scan(&count); err != nil {
		return 0, sqliteFault("job", err, false)
	}
	return count, nil
}

func (r sqliteReads) dialectConflicts(dialect, version, digest string) (int, error) {
	var count int
	if err := r.q.QueryRow(`SELECT count(*) FROM job_revisions j JOIN releases r ON r.id=j.release_id WHERE json_extract(r.record,'$.dialect.id')=? AND json_extract(r.record,'$.dialect.version')=? AND json_extract(r.record,'$.dialect.digest')<>?`, dialect, version, digest).Scan(&count); err != nil {
		return 0, sqliteFault("job", err, false)
	}
	return count, nil
}

func (t *sqliteTx) createJob(j Job) error {
	if _, err := t.tx.Exec("INSERT INTO jobs(id,release_id,record)VALUES(?,?,?)", j.ID, j.ReleaseID, string(encode(j))); err != nil {
		return sqliteFault("job", err, true)
	}
	if _, err := t.tx.Exec("INSERT INTO job_creations(release_id,record) VALUES(?,?)", j.ReleaseID, string(encode(j))); err != nil {
		return sqliteFault("job", err, true)
	}
	_, err := t.tx.Exec("INSERT INTO job_revisions(job_id,revision,release_id,record) VALUES(?,?,?,?)", j.ID, j.Revision, j.ReleaseID, string(encode(j)))
	return sqliteFault("job", err, true)
}

func (t *sqliteTx) reviseJob(j Job, request string) error {
	res, err := t.tx.Exec("UPDATE jobs SET release_id=?,record=? WHERE id=?", j.ReleaseID, string(encode(j)), j.ID)
	if err != nil {
		return sqliteFault("job", err, true)
	}
	if n, err := res.RowsAffected(); err != nil {
		return sqliteFault("job", err, true)
	} else if n == 0 {
		return storeFailure(faultAbsent, "job", errNoJob)
	}
	_, err = t.tx.Exec("INSERT INTO job_revisions(job_id,revision,release_id,record,request_digest) VALUES(?,?,?,?,?)", j.ID, j.Revision, j.ReleaseID, string(encode(j)), request)
	return sqliteFault("job", err, true)
}

func (t *sqliteTx) openOccurrences(job string) (int, error) {
	var count int
	if err := t.tx.QueryRow("SELECT count(*) FROM occurrences WHERE job_id=? AND state IN ('accepted','preparing','waiting','needs-attention')", job).Scan(&count); err != nil {
		return 0, sqliteFault("occurrence", err, false)
	}
	return count, nil
}

// triggerRecords reads in the order SQLite gives the job's index on triggers,
// which is the order they were made.
func (t *sqliteTx) triggerRecords(job string) ([]Trigger, error) {
	rows, err := t.tx.Query("SELECT record FROM triggers WHERE job_id=?", job)
	if err != nil {
		return nil, sqliteFault("trigger", err, false)
	}
	defer rows.Close()
	var triggers []Trigger
	for rows.Next() {
		var b []byte
		var tr Trigger
		if err = rows.Scan(&b); err != nil {
			return nil, sqliteFault("trigger", err, false)
		}
		if err = json.Unmarshal(b, &tr); err != nil {
			return nil, storeFailure(faultUnreadable, "trigger", err)
		}
		triggers = append(triggers, tr)
	}
	return triggers, sqliteFault("trigger", rows.Err(), false)
}

func (t *sqliteTx) reviseTrigger(tr Trigger) error {
	res, err := t.tx.Exec("UPDATE triggers SET record=? WHERE id=?", string(encode(tr)), tr.ID)
	if err != nil {
		return sqliteFault("trigger", err, true)
	}
	if n, err := res.RowsAffected(); err != nil {
		return sqliteFault("trigger", err, true)
	} else if n == 0 {
		return storeFailure(faultAbsent, "trigger", errNoTrigger)
	}
	_, err = t.tx.Exec("INSERT INTO trigger_revisions(trigger_id,revision,record) VALUES(?,?,?)", tr.ID, tr.Revision, string(encode(tr)))
	return sqliteFault("trigger", err, true)
}
