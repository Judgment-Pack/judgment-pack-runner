package runner

// The sqlite kind's triggers and occurrences: the statements the Runner made
// on them before they stood behind the store contract, unchanged, so that
// what the kind writes is byte for byte what it wrote
// (docs/design/store-interface.md, section 9).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func (r sqliteReads) trigger(id string) (Trigger, string, error) {
	var t Trigger
	var raw, hash, observed, pending, pendingAt string
	var keyRevision sql.NullInt64
	if err := r.q.QueryRow("SELECT record,key_hash,observed,pending,pending_at,key_revision FROM triggers WHERE id=?", id).Scan(&raw, &hash, &observed, &pending, &pendingAt, &keyRevision); err != nil {
		return t, "", sqliteFault("trigger", err, false)
	}
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return t, hash, storeFailure(faultUnreadable, "trigger", err)
	}
	t.HasKey = hash != ""
	t.observed, t.pending, t.pendingAt = observed, pending, pendingAt
	if keyRevision.Valid {
		v := int(keyRevision.Int64)
		t.keyRevision = &v
	}
	return t, hash, nil
}

func (r sqliteReads) triggerCount() (int, error) {
	var count int
	if err := r.q.QueryRow("SELECT count(*) FROM triggers").Scan(&count); err != nil {
		return 0, sqliteFault("trigger", err, false)
	}
	return count, nil
}

func (r sqliteReads) cloudBindings(except, connection, job string) (int, error) {
	var count int
	if err := r.q.QueryRow(`SELECT count(*) FROM triggers WHERE id<>? AND json_extract(record,'$.config.cloud.connection')=? AND json_extract(record,'$.config.cloud.job')=?`, except, connection, job).Scan(&count); err != nil {
		return 0, sqliteFault("trigger", err, false)
	}
	return count, nil
}

func (r sqliteReads) occurrence(id string) (Occurrence, error) {
	return sqliteOccurrence(r.q.QueryRow("SELECT record FROM occurrences WHERE id=?", id))
}

func (r sqliteReads) identifiedOccurrence(trigger, identity string) (Occurrence, string, error) {
	var o Occurrence
	var raw, payload string
	if err := r.q.QueryRow("SELECT record,payload_digest FROM occurrences WHERE trigger_id=? AND identity=?", trigger, identity).Scan(&raw, &payload); err != nil {
		return o, "", sqliteFault("occurrence", err, false)
	}
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return o, payload, storeFailure(faultUnreadable, "occurrence", err)
	}
	return o, payload, nil
}

// sqliteOccurrence decodes the one record a statement selects.
func sqliteOccurrence(row *sql.Row) (Occurrence, error) {
	var o Occurrence
	var raw string
	if err := row.Scan(&raw); err != nil {
		return o, sqliteFault("occurrence", err, false)
	}
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return o, storeFailure(faultUnreadable, "occurrence", err)
	}
	return o, nil
}

func (k *sqliteStore) enabledTriggers() ([]string, error) {
	rows, err := k.db.Query("SELECT id FROM triggers WHERE json_extract(record,'$.paused')=0 ORDER BY seq")
	if err != nil {
		return nil, sqliteFault("trigger", err, false)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, sqliteFault("trigger", err, false)
		}
		ids = append(ids, id)
	}
	return ids, sqliteFault("trigger", rows.Err(), false)
}

func (k *sqliteStore) readyOccurrence() (Occurrence, error) {
	return sqliteOccurrence(k.db.QueryRow("SELECT record FROM occurrences WHERE state='accepted' AND json_extract(record,'$.pendingInput') IS NULL ORDER BY seq LIMIT 1"))
}

func (k *sqliteStore) preparableOccurrences(each func(Occurrence) (bool, error)) error {
	rows, err := k.db.Query(`SELECT record FROM occurrences WHERE (state='accepted' AND json_extract(record,'$.pendingInput') IS NOT NULL) OR state='waiting' ORDER BY seq`)
	if err != nil {
		return sqliteFault("occurrence", err, false)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var o Occurrence
		if err = rows.Scan(&raw); err != nil {
			return sqliteFault("occurrence", err, false)
		}
		if err = json.Unmarshal([]byte(raw), &o); err != nil {
			return storeFailure(faultUnreadable, "occurrence", err)
		}
		if done, err := each(o); err != nil || done {
			return err
		}
	}
	return sqliteFault("occurrence", rows.Err(), false)
}

func (k *sqliteStore) cloudBoundTrigger(connection, job string) (string, error) {
	var id string
	if err := k.db.QueryRow(`SELECT id FROM triggers WHERE json_extract(record,'$.config.cloud.connection')=? AND json_extract(record,'$.config.cloud.job')=?`, connection, job).Scan(&id); err != nil {
		return "", sqliteFault("trigger", err, false)
	}
	return id, nil
}

func (k *sqliteStore) jobTriggers(job string) ([]Trigger, error) {
	rows, err := k.db.Query("SELECT record,key_hash FROM triggers WHERE job_id=? ORDER BY seq", job)
	if err != nil {
		return nil, sqliteFault("trigger", err, false)
	}
	defer rows.Close()
	items := []Trigger{}
	for rows.Next() {
		var raw, hash string
		var t Trigger
		if err = rows.Scan(&raw, &hash); err != nil {
			return nil, sqliteFault("trigger", err, false)
		}
		if err = json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, storeFailure(faultUnreadable, "trigger", err)
		}
		t.HasKey = hash != ""
		items = append(items, t)
	}
	return items, sqliteFault("trigger", rows.Err(), false)
}

func (k *sqliteStore) occurrencePage(job string, before int64, preparations bool, limit int) ([]Occurrence, int64, error) {
	rows, err := k.db.Query("SELECT seq,record FROM occurrences WHERE job_id=? AND seq<? AND (?=0 OR (json_extract(record,'$.preparation') IS NOT NULL AND state<>'submitted')) ORDER BY seq DESC LIMIT ?", job, before, preparations, limit+1)
	if err != nil {
		return nil, 0, sqliteFault("occurrence", err, false)
	}
	defer rows.Close()
	items := []Occurrence{}
	var last, next int64
	for rows.Next() {
		var seq int64
		var raw string
		if err = rows.Scan(&seq, &raw); err != nil {
			return nil, 0, sqliteFault("occurrence", err, false)
		}
		if len(items) == limit {
			next = last
			break
		}
		var o Occurrence
		if err = json.Unmarshal([]byte(raw), &o); err != nil {
			return nil, 0, storeFailure(faultUnreadable, "occurrence", err)
		}
		items = append(items, o)
		last = seq
	}
	return items, next, sqliteFault("occurrence", rows.Err(), false)
}

func (k *sqliteStore) keyedOccurrence(id, trigger, hash string) (Occurrence, error) {
	return sqliteOccurrence(k.db.QueryRow("SELECT o.record FROM occurrences o JOIN occurrence_keys k ON k.occurrence_id=o.id JOIN triggers t ON t.id=o.trigger_id WHERE o.id=? AND o.trigger_id=? AND k.key_hash=? AND t.key_hash=k.key_hash", id, trigger, hash))
}

func (t *sqliteTx) createTrigger(tr Trigger) error {
	_, err := t.tx.Exec("INSERT INTO triggers(id,job_id,record)VALUES(?,?,?)", tr.ID, tr.JobID, string(encode(tr)))
	return sqliteFault("trigger", err, true)
}

func (t *sqliteTx) putTrigger(tr Trigger, hash string, history bool) error {
	err := saveTrigger(t.tx, tr, hash, history)
	if faultOf(err) != "" {
		return err
	}
	return sqliteFault("trigger", err, true)
}

// errNoTrigger is the cause of putTrigger's faultAbsent.
var errNoTrigger = errors.New("no trigger is held under that id")

// saveTrigger is putTrigger's statements, which a job's creation also makes
// (store.go) until it stands behind the contract.
func saveTrigger(tx *sql.Tx, t Trigger, hash string, history bool) error {
	var keyRevision any
	if t.keyRevision != nil {
		keyRevision = *t.keyRevision
	}
	res, e := tx.Exec("UPDATE triggers SET record=?,key_hash=?,observed=?,pending=?,pending_at=?,key_revision=? WHERE id=?", string(encode(t)), hash, t.observed, t.pending, t.pendingAt, keyRevision, t.ID)
	if e != nil {
		return e
	}
	if n, e := res.RowsAffected(); e != nil {
		return e
	} else if n == 0 {
		return storeFailure(faultAbsent, "trigger", errNoTrigger)
	}
	if history {
		_, e = tx.Exec("INSERT INTO trigger_revisions VALUES(?,?,?)", t.ID, t.Revision, string(encode(t)))
	}
	return e
}

func (t *sqliteTx) createOccurrence(o Occurrence, identity, payload string) error {
	_, err := t.tx.Exec("INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES(?,?,?,?,?,?,?)", o.ID, o.JobID, o.TriggerID, identity, payload, o.State, string(encode(o)))
	return sqliteFault("occurrence", err, true)
}

func (t *sqliteTx) putOccurrence(o Occurrence, expected string) (string, error) {
	var from string
	if err := t.tx.QueryRow("SELECT state FROM occurrences WHERE id=?", o.ID).Scan(&from); err != nil {
		return "", sqliteFault("occurrence", err, false)
	}
	if expected != anyState && from != expected {
		return from, storeFailure(faultConflict, "occurrence", fmt.Errorf("the occurrence %s is %s, not %s", o.ID, from, expected))
	}
	if _, err := t.tx.Exec("UPDATE occurrences SET state=?,record=? WHERE id=?", o.State, string(encode(o)), o.ID); err != nil {
		return from, sqliteFault("occurrence", err, true)
	}
	return from, nil
}

func (t *sqliteTx) createOccurrenceKey(occurrence, hash string) error {
	_, err := t.tx.Exec("INSERT INTO occurrence_keys(occurrence_id,key_hash) VALUES(?,?)", occurrence, hash)
	return sqliteFault("occurrence", err, true)
}

func (t *sqliteTx) pendingOccurrences() (int, error) {
	var count int
	if err := t.tx.QueryRow("SELECT count(*) FROM occurrences WHERE state IN ('accepted','preparing','waiting')").Scan(&count); err != nil {
		return 0, sqliteFault("occurrence", err, false)
	}
	return count, nil
}

func (t *sqliteTx) jobActivity(job string) (int, error) {
	var count int
	if err := t.tx.QueryRow("SELECT (SELECT count(*) FROM runs WHERE job_id=? AND state IN ('queued','running'))+(SELECT count(*) FROM occurrences WHERE job_id=? AND state IN ('accepted','preparing','waiting','needs-attention'))", job, job).Scan(&count); err != nil {
		return 0, sqliteFault("occurrence", err, false)
	}
	return count, nil
}

func (t *sqliteTx) preparingOccurrences() ([]Occurrence, error) {
	var occurrences []Occurrence
	if err := eachRecord(t.tx, "SELECT record FROM occurrences WHERE state='preparing' ORDER BY seq", func(b []byte) error {
		var o Occurrence
		e := json.Unmarshal(b, &o)
		occurrences = append(occurrences, o)
		return e
	}); err != nil {
		return occurrences, storeFailure(faultUnreadable, "occurrence", err)
	}
	return occurrences, nil
}

func (t *sqliteTx) lastPreparing(occurrence string) (string, error) {
	var since string
	if err := t.tx.QueryRow("SELECT at FROM events WHERE kind='occurrence.preparing' AND json_extract(concerns,'$.occurrence')=? ORDER BY sequence DESC LIMIT 1", occurrence).Scan(&since); err != nil {
		return "", sqliteFault("journal", err, false)
	}
	return since, nil
}

// eachRecord reads the record column of a query's rows, closing them before
// returning: the store's handle has one connection.
func eachRecord(tx *sql.Tx, query string, each func([]byte) error) error {
	rows, err := tx.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return err
		}
		if err = each(b); err != nil {
			return err
		}
	}
	return rows.Err()
}
