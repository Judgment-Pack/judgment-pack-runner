package runner

// The postgresql kind's reads (docs/design/store-postgresql.md, section 3):
// the sqlite kind's statements, translated as the design says, each held by
// the row policies to the scope of the view or transaction it is made
// through.

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
)

// one reads one row of query into dest.
func (r pgReads) one(op string, query string, args []any, dest ...any) error {
	return r.read(op, func(q pgQuerier) error {
		return pgFault(op, q.QueryRowContext(pgContext, query, args...).Scan(dest...), false)
	})
}

// each reads the record column of every row of query, in order.
func (r pgReads) each(op, query string, args []any, each func([]byte) error) error {
	return r.read(op, func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, query, args...)
		if err != nil {
			return pgFault(op, err, false)
		}
		defer rows.Close()
		for rows.Next() {
			var b []byte
			if err = rows.Scan(&b); err != nil {
				return pgFault(op, err, false)
			}
			if err = each(b); err != nil {
				return storeFailure(faultUnreadable, op, err)
			}
		}
		return pgFault(op, rows.Err(), false)
	})
}

// pgRecordOf decodes a record read as JSON.
func pgRecordOf[T any](op string, b []byte, err error) (T, error) {
	var v T
	if err != nil {
		return v, err
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return v, storeFailure(faultUnreadable, op, e)
	}
	return v, nil
}

func (r pgReads) record(op, query string, args ...any) ([]byte, error) {
	var b []byte
	err := r.one(op, query, args, &b)
	return b, err
}

func (r pgReads) release(id string) (Release, error) {
	b, err := r.record("release", "SELECT record FROM releases WHERE id=$1", id)
	rel, err := pgRecordOf[Release]("release", b, err)
	if err != nil {
		return rel, err
	}
	if e := validateStoredRelease(rel); e != nil {
		return rel, storeFailure(faultUnreadable, "release", e)
	}
	return rel, nil
}

func (r pgReads) job(id string) (Job, error) {
	b, err := r.record("job", "SELECT record FROM jobs WHERE id=$1", id)
	return pgRecordOf[Job]("job", b, err)
}

func (r pgReads) jobCreation(release string) (Job, error) {
	b, err := r.record("job", "SELECT record FROM job_creations WHERE release_id=$1", release)
	return pgRecordOf[Job]("job", b, err)
}

func (r pgReads) count(op, query string, args ...any) (int, error) {
	var n int
	err := r.one(op, query, args, &n)
	return n, err
}

func (r pgReads) releaseUses(release string) (int, error) {
	return r.count("job", "SELECT count(*) FROM job_revisions WHERE release_id=$1", release)
}

func (r pgReads) jobRevision(job string, revision int) (Job, string, error) {
	var raw []byte
	var request string
	err := r.one("job", "SELECT record,request_digest FROM job_revisions WHERE job_id=$1 AND revision=$2", []any{job, revision}, &raw, &request)
	if err != nil {
		return Job{}, "", err
	}
	j, err := pgRecordOf[Job]("job", raw, nil)
	return j, request, err
}

// packConflicts and dialectConflicts read the identities every revision's
// release made, for every scope (D9).
func (r pgReads) packConflicts(pack, version, digest string) (int, error) {
	return r.count("job", "SELECT count(*) FROM pack_identities WHERE pack=$1 AND version=$2 AND digest<>$3", pack, version, digest)
}

func (r pgReads) dialectConflicts(dialect, version, digest string) (int, error) {
	return r.count("job", "SELECT count(*) FROM dialect_identities WHERE dialect=$1 AND version=$2 AND digest<>$3", dialect, version, digest)
}

func (r pgReads) requestedRun(jobID, caller, key string) (heldRequest, error) {
	var held heldRequest
	err := r.one("run", "SELECT id,request_digest,record FROM runs WHERE job_id=$1 AND caller=$2 AND idem=$3", []any{jobID, caller, key}, &held.id, &held.digest, &held.record)
	if err != nil {
		return heldRequest{}, err
	}
	return held, nil
}

func (r pgReads) entrySequence(run string) (int64, error) {
	var sequence int64
	err := r.one("chain", "SELECT sequence FROM run_chain WHERE run=$1", []any{run}, &sequence)
	return sequence, err
}

func (r pgReads) run(id string) (Run, error) {
	b, err := r.record("run", "SELECT record FROM runs WHERE id=$1", id)
	if err != nil {
		return Run{}, err
	}
	run, err := decodeRun(b)
	if err != nil {
		return run, storeFailure(faultUnreadable, "run", err)
	}
	return run, nil
}

func (r pgReads) recordScope(records, id string) (string, error) {
	if !scopeCarriers[records] {
		return "", pgRefusedKind(records)
	}
	var scope string
	err := r.one("scope", "SELECT scope FROM "+records+" WHERE id=$1", []any{id}, &scope)
	return scope, err
}

func (r pgReads) profileRow(key string) (string, error) {
	if !profileRowKeys[key] {
		return "", storeFailure(faultRefused, "profiles", errNoProfileRow(key))
	}
	var row string
	err := r.one("profiles", "SELECT value FROM metadata WHERE key=$1", []any{key}, &row)
	return row, err
}

func (r pgReads) brief(subject string) ([]byte, error) {
	return r.record("brief", "SELECT record FROM briefs WHERE subject=$1", subject)
}

func (r pgReads) trigger(id string) (Trigger, string, error) {
	var t Trigger
	var raw []byte
	var hash, observed, pending, pendingAt string
	var keyRevision sql.NullInt64
	if err := r.one("trigger", "SELECT record,key_hash,observed,pending,pending_at,key_revision FROM triggers WHERE id=$1", []any{id}, &raw, &hash, &observed, &pending, &pendingAt, &keyRevision); err != nil {
		return t, "", err
	}
	t, err := pgRecordOf[Trigger]("trigger", raw, nil)
	if err != nil {
		return t, hash, err
	}
	t.HasKey = hash != ""
	t.observed, t.pending, t.pendingAt = observed, pending, pendingAt
	if keyRevision.Valid {
		v := int(keyRevision.Int64)
		t.keyRevision = &v
	}
	return t, hash, nil
}

// triggerCount is the installation's count (D9).
func (r pgReads) triggerCount() (int, error) {
	return r.count("trigger", "SELECT value FROM counts WHERE name='triggers'")
}

func (r pgReads) cloudBindings(except, connection, job string) (int, error) {
	return r.count("trigger", "SELECT count(*) FROM triggers WHERE id<>$1 AND "+pgJSON("record", "config", "cloud", "connection")+"=$2 AND "+pgJSON("record", "config", "cloud", "job")+"=$3", except, connection, job)
}

func (r pgReads) occurrence(id string) (Occurrence, error) {
	b, err := r.record("occurrence", "SELECT record FROM occurrences WHERE id=$1", id)
	return pgRecordOf[Occurrence]("occurrence", b, err)
}

func (r pgReads) identifiedOccurrence(trigger, identity string) (Occurrence, string, error) {
	var raw []byte
	var payload string
	if err := r.one("occurrence", "SELECT record,payload_digest FROM occurrences WHERE trigger_id=$1 AND identity=$2", []any{trigger, identity}, &raw, &payload); err != nil {
		return Occurrence{}, "", err
	}
	o, err := pgRecordOf[Occurrence]("occurrence", raw, nil)
	return o, payload, err
}

func (r pgReads) journalBegan() (string, error) {
	var at string
	err := r.one("journal", "SELECT at FROM events WHERE kind='journal.began' ORDER BY sequence LIMIT 1", nil, &at)
	return at, err
}

// pgIn is a test that expr is any of values, false for none.
func pgIn(expr string, values []string, a *pgArgs) string {
	if len(values) == 0 {
		return "false"
	}
	slots := make([]string, len(values))
	for i, v := range values {
		slots[i] = a.add(v)
	}
	return expr + " IN (" + strings.Join(slots, ",") + ")"
}

// pgResultConditions are sqliteResultConditions translated: the tests of f
// over a record column holding a run (runs) or over a journal entry's detail
// and kind, each member compared as its JSON text.
func pgResultConditions(f resultFilter, record, kindColumn string, runs bool, a *pgArgs) []string {
	var where []string
	if len(f.Outcomes) > 0 {
		path := []string{"disposition"}
		if runs {
			path = []string{"result", "disposition"}
			where = append(where, "t.state='completed'")
		}
		where = append(where, pgJSON(record, append(path[:len(path):len(path)], "kind")...)+"='outcome'")
		where = append(where, pgIn(pgJSON(record, append(path[:len(path):len(path)], "outcomeId")...), f.Outcomes, a))
	}
	if len(f.Classes) > 0 {
		where = append(where, pgIn(pgJSON(record, "errorClass"), f.Classes, a))
	}
	if len(f.Kinds) > 0 {
		where = append(where, pgIn(kindColumn, f.Kinds, a))
	}
	return where
}

// pgJournalCondition is sqliteJournalCondition translated.
func pgJournalCondition(f journalFilter, a *pgArgs) string {
	where := "true"
	if f.job != "" {
		job := a.add(f.job)
		where = "job_id=" + job + " OR (job_id='' AND release_id IN (SELECT release_id FROM job_revisions WHERE job_id=" + job + "))"
		if f.runnerOwn {
			where += " OR kind IN ('journal.began','runner.started','runner.stopped')"
		}
	}
	if f.scope != "" {
		where = "(" + where + ") AND " + pgJSON("detail", "scope") + "=" + a.add(f.scope)
	}
	if !f.results.empty() {
		where = "(" + where + ") AND " + strings.Join(pgResultConditions(f.results, "detail", "kind", false, a), " AND ")
	}
	return where
}

// journalPage reads one row beyond the limit, to say whether there is more,
// and does not decode it.
func (r pgReads) journalPage(f journalFilter, g journalRange) ([]Event, bool, error) {
	var a pgArgs
	var query string
	if g.backward {
		query = "SELECT " + eventColumns + " FROM events WHERE (" + pgJournalCondition(f, &a) + ")"
		if !g.tail {
			query += " AND sequence<" + a.add(g.before)
		}
		query += " ORDER BY sequence DESC LIMIT " + a.add(g.limit+1)
	} else {
		after := a.add(g.after)
		query = "SELECT " + eventColumns + " FROM events WHERE sequence>" + after + " AND (" + pgJournalCondition(f, &a) + ") ORDER BY sequence LIMIT " + a.add(g.limit+1)
	}
	var items []Event
	more := false
	err := r.read("journal", func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, query, a...)
		if err != nil {
			return pgFault("journal", err, false)
		}
		defer rows.Close()
		for rows.Next() {
			if len(items) == g.limit {
				more = true
				break
			}
			var sequence int64
			var e journalRow
			var revision sql.NullInt64
			var from, to sql.NullString
			if err = rows.Scan(&sequence, &e.version, &e.kind, &e.at, &e.by, &e.concerns, &revision, &from, &to, &e.reason, &e.detail); err != nil {
				return pgFault("journal", err, false)
			}
			e.revision = nullInt{value: revision.Int64, held: revision.Valid}
			e.from, e.to = nullText{value: from.String, held: from.Valid}, nullText{value: to.String, held: to.Valid}
			event, err := e.event(sequence)
			if err != nil {
				return storeFailure(faultUnreadable, "journal", err)
			}
			items = append(items, event)
		}
		return pgFault("journal", rows.Err(), false)
	})
	if err != nil {
		return nil, false, err
	}
	return items, more, nil
}

func (k *pgStore) oldestQueued() (Run, error) {
	b, err := k.record("queue", "SELECT record FROM runs WHERE state='queued' ORDER BY seq LIMIT 1")
	if err != nil {
		return Run{}, err
	}
	r, err := decodeRun(b)
	if err != nil {
		return r, storeFailure(faultUnreadable, "queue", err)
	}
	return r, nil
}

func (k *pgStore) entryLine(run string) ([]byte, error) {
	return k.record("chain", "SELECT line FROM run_chain WHERE run=$1", run)
}

func (k *pgStore) chainPage(after int64, limit int) ([][]byte, int64, error) {
	var lines [][]byte
	last := after
	// A negative limit is none, as SQLite reads it.
	var bound any = limit
	if limit < 0 {
		bound = nil
	}
	err := k.read("chain", func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, "SELECT sequence,line FROM run_chain WHERE sequence>$1 ORDER BY sequence LIMIT $2", after, bound)
		if err != nil {
			return pgFault("chain", err, false)
		}
		defer rows.Close()
		for rows.Next() {
			var line []byte
			if err = rows.Scan(&last, &line); err != nil {
				return pgFault("chain", err, false)
			}
			lines = append(lines, line)
		}
		return pgFault("chain", rows.Err(), false)
	})
	if err != nil {
		return nil, 0, err
	}
	return lines, last, nil
}

func (k *pgStore) enabledTriggers() ([]string, error) {
	ids := []string{}
	err := k.each("trigger", "SELECT id FROM triggers WHERE "+pgJSON("record", "paused")+"='false' ORDER BY seq", nil, func(b []byte) error {
		ids = append(ids, string(b))
		return nil
	})
	return ids, err
}

func (k *pgStore) readyOccurrence() (Occurrence, error) {
	b, err := k.record("occurrence", "SELECT record FROM occurrences WHERE state='accepted' AND "+pgJSON("record", "pendingInput")+" IS NULL ORDER BY seq LIMIT 1")
	return pgRecordOf[Occurrence]("occurrence", b, err)
}

// preparableOccurrences reads the occurrences first, then gives each, so that
// each may use the store while it is given one.
func (k *pgStore) preparableOccurrences(each func(Occurrence) (bool, error)) error {
	var held []Occurrence
	if err := k.each("occurrence", "SELECT record FROM occurrences WHERE (state='accepted' AND "+pgJSON("record", "pendingInput")+" IS NOT NULL) OR state='waiting' ORDER BY seq", nil, func(b []byte) error {
		var o Occurrence
		err := json.Unmarshal(b, &o)
		held = append(held, o)
		return err
	}); err != nil {
		return err
	}
	for _, o := range held {
		if done, err := each(o); err != nil || done {
			return err
		}
	}
	return nil
}

func (k *pgStore) cloudBoundTrigger(connection, job string) (string, error) {
	var id string
	err := k.one("trigger", "SELECT id FROM triggers WHERE "+pgJSON("record", "config", "cloud", "connection")+"=$1 AND "+pgJSON("record", "config", "cloud", "job")+"=$2", []any{connection, job}, &id)
	return id, err
}

func (k *pgStore) jobTriggers(job string) ([]Trigger, error) {
	items := []Trigger{}
	err := k.read("trigger", func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, "SELECT record,key_hash FROM triggers WHERE job_id=$1 ORDER BY seq", job)
		if err != nil {
			return pgFault("trigger", err, false)
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var hash string
			if err = rows.Scan(&raw, &hash); err != nil {
				return pgFault("trigger", err, false)
			}
			t, err := pgRecordOf[Trigger]("trigger", raw, nil)
			if err != nil {
				return err
			}
			t.HasKey = hash != ""
			items = append(items, t)
		}
		return pgFault("trigger", rows.Err(), false)
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (k *pgStore) occurrencePage(job string, before int64, preparations bool, limit int) ([]Occurrence, int64, error) {
	items := []Occurrence{}
	var next int64
	err := k.read("occurrence", func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, "SELECT seq,record FROM occurrences WHERE job_id=$1 AND seq<$2 AND (NOT $3::boolean OR ("+pgJSON("record", "preparation")+" IS NOT NULL AND state<>'submitted')) ORDER BY seq DESC LIMIT $4", job, before, preparations, limit+1)
		if err != nil {
			return pgFault("occurrence", err, false)
		}
		defer rows.Close()
		var last int64
		for rows.Next() {
			var seq int64
			var raw []byte
			if err = rows.Scan(&seq, &raw); err != nil {
				return pgFault("occurrence", err, false)
			}
			if len(items) == limit {
				next = last
				break
			}
			o, err := pgRecordOf[Occurrence]("occurrence", raw, nil)
			if err != nil {
				return err
			}
			items = append(items, o)
			last = seq
		}
		return pgFault("occurrence", rows.Err(), false)
	})
	if err != nil {
		return nil, 0, err
	}
	return items, next, nil
}

func (k *pgStore) keyedOccurrence(id, trigger, hash string) (Occurrence, error) {
	b, err := k.record("occurrence", "SELECT o.record FROM occurrences o JOIN occurrence_keys k ON k.occurrence_id=o.id JOIN triggers t ON t.id=o.trigger_id WHERE o.id=$1 AND o.trigger_id=$2 AND k.key_hash=$3 AND t.key_hash=k.key_hash", id, trigger, hash)
	return pgRecordOf[Occurrence]("occurrence", b, err)
}

func (k *pgStore) listedJobs(before int64, f recordFilter, limit int) ([]listedRecord, error) {
	return k.listed("jobs", "", before, f, limit)
}

func (k *pgStore) listedRuns(job string, before int64, f recordFilter, limit int) ([]listedRecord, error) {
	return k.listed("runs", job, before, f, limit)
}

// listed is the sqlite kind's listing statement translated. Search is a
// literal substring, ASCII case folded as SQLite's lower() folds it.
func (k *pgStore) listed(kind, jobID string, before int64, filter recordFilter, limit int) ([]listedRecord, error) {
	var a pgArgs
	where := []string{"t.seq<" + a.add(before)}
	table, joins := "jobs", " JOIN releases release ON release.id=t.release_id"
	extra := pgJSON("release.record", "title") + "," + pgJSON("release.record", "packVersion")
	search := pgJSON("t.record", "name")
	if kind == "runs" {
		table = "runs"
		joins = " JOIN jobs job ON job.id=t.job_id"
		extra = pgJSON("job.record", "name") + ",''"
		search = "t.id || ' ' || " + pgJSON("job.record", "name")
		if jobID != "" {
			where = append(where, "t.job_id="+a.add(jobID))
		}
		if filter.State != "" {
			where = append(where, "t.state="+a.add(filter.State))
		}
		where = append(where, pgResultConditions(filter.Results, "t.record", "", true, &a)...)
		if filter.Review {
			where = append(where, "(t.state IN ('failed','interrupted') OR (t.state='completed' AND ("+pgJSON("t.record", "result", "disposition", "kind")+"='unresolved' OR "+pgJSON("t.record", "result", "disposition", "handoff", "state")+"='requested')))")
		}
	}
	if filter.Scope != "" {
		where = append(where, pgJSON("t.record", "scope")+"="+a.add(filter.Scope))
	}
	if filter.Search != "" {
		where = append(where, "strpos("+pgFold(search)+","+pgFold(a.add(filter.Search)+"::text")+")>0")
	}
	query := "SELECT t.seq,t.record," + extra + " FROM " + table + " t" + joins + " WHERE " + strings.Join(where, " AND ") + " ORDER BY t.seq DESC LIMIT " + strconv.Itoa(limit)
	items := []listedRecord{}
	err := k.read("list", func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, query, a...)
		if err != nil {
			return pgFault("list", err, false)
		}
		defer rows.Close()
		for rows.Next() {
			var item listedRecord
			if err = rows.Scan(&item.seq, &item.record, &item.label, &item.version); err != nil {
				return pgFault("list", err, false)
			}
			items = append(items, item)
		}
		return pgFault("list", rows.Err(), false)
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (k *pgStore) recentRuns(jobs []string, per int) ([]recentRun, error) {
	var a pgArgs
	in := pgIn("job_id", jobs, &a)
	query := "SELECT job_id,id,state,created FROM (SELECT job_id,id,state," + pgJSON("record", "createdAt") + " AS created,row_number() OVER (PARTITION BY job_id ORDER BY seq DESC) AS rank FROM runs WHERE " + in + ") ranked WHERE rank<=" + strconv.Itoa(per) + ` ORDER BY job_id COLLATE "C",rank`
	items := []recentRun{}
	err := k.read("list", func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, query, a...)
		if err != nil {
			return pgFault("list", err, false)
		}
		defer rows.Close()
		for rows.Next() {
			var r recentRun
			if err = rows.Scan(&r.job, &r.id, &r.state, &r.createdAt); err != nil {
				return pgFault("list", err, false)
			}
			items = append(items, r)
		}
		return pgFault("list", rows.Err(), false)
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (k *pgStore) listedTriggers(jobs []string) ([]listedTrigger, error) {
	var a pgArgs
	query := "SELECT job_id,record FROM triggers WHERE " + pgIn("job_id", jobs, &a) + " ORDER BY seq"
	items := []listedTrigger{}
	err := k.read("list", func(q pgQuerier) error {
		rows, err := q.QueryContext(pgContext, query, a...)
		if err != nil {
			return pgFault("list", err, false)
		}
		defer rows.Close()
		for rows.Next() {
			var item listedTrigger
			var raw []byte
			if err = rows.Scan(&item.job, &raw); err != nil {
				return pgFault("list", err, false)
			}
			if item.trigger, err = pgRecordOf[Trigger]("list", raw, nil); err != nil {
				return err
			}
			items = append(items, item)
		}
		return pgFault("list", rows.Err(), false)
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}
