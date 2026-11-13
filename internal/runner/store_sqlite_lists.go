package runner

// The sqlite kind's listings (list.go): the job and run listings' statements
// and the result filter's conditions, as the Runner made them before the
// contract stood in front of them.

import (
	"encoding/json"
	"strconv"
	"strings"
)

// sqliteIn is a SQL test that expr is any of values, with its arguments.
func sqliteIn(expr string, values []string) (string, []any) {
	slots := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		slots[i], args[i] = "?", v
	}
	return expr + " IN (" + strings.Join(slots, ",") + ")", args
}

// sqliteResultConditions are the SQL tests of f over a record column holding
// a run (runs) or over a journal entry's detail and kind (the journal), each
// with its arguments. A run matches an outcome only when it completed with a
// decision of kind outcome; SQLite compares text by its bytes.
func sqliteResultConditions(f resultFilter, record, kindColumn string, runs bool) ([]string, []any) {
	var where []string
	var args []any
	add := func(expr string, a []any) { where, args = append(where, expr), append(args, a...) }
	if len(f.Outcomes) > 0 {
		disposition := "json_extract(" + record + ",'$.disposition"
		if runs {
			disposition = "json_extract(" + record + ",'$.result.disposition"
			add("t.state='completed'", nil)
		}
		add(disposition+".kind')='outcome'", nil)
		add(sqliteIn(disposition+".outcomeId')", f.Outcomes))
	}
	if len(f.Classes) > 0 {
		add(sqliteIn("json_extract("+record+",'$.errorClass')", f.Classes))
	}
	if len(f.Kinds) > 0 {
		add(sqliteIn(kindColumn, f.Kinds))
	}
	return where, args
}

func (k *sqliteStore) listedJobs(before int64, f recordFilter, limit int) ([]listedRecord, error) {
	return k.listed("jobs", "", before, f, limit)
}

func (k *sqliteStore) listedRuns(job string, before int64, f recordFilter, limit int) ([]listedRecord, error) {
	return k.listed("runs", job, before, f, limit)
}

// listed is the listings' one statement. Search is a literal substring,
// never a SQL pattern.
func (k *sqliteStore) listed(kind, jobID string, before int64, filter recordFilter, limit int) ([]listedRecord, error) {
	where := []string{"t.seq<?"}
	args := []any{before}
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
		results, resultArgs := sqliteResultConditions(filter.Results, "t.record", "", true)
		where, args = append(where, results...), append(args, resultArgs...)
		if filter.Review {
			where = append(where, `(t.state IN ('failed','interrupted') OR (t.state='completed' AND (json_extract(t.record,'$.result.disposition.kind')='unresolved' OR json_extract(t.record,'$.result.disposition.handoff.state')='requested')))`)
		}
	}
	if filter.Scope != "" {
		// SQLite compares text by its bytes (the BINARY collation): no case
		// is folded and no prefix matches.
		where = append(where, "json_extract(t.record,'$.scope')=?")
		args = append(args, filter.Scope)
	}
	if filter.Search != "" {
		where = append(where, "instr(lower("+search+"),lower(?))>0")
		args = append(args, filter.Search)
	}
	rows, err := k.db.Query("SELECT t.seq,t.record,"+extra+" FROM "+table+" t"+joins+" WHERE "+strings.Join(where, " AND ")+" ORDER BY t.seq DESC LIMIT "+strconv.Itoa(limit), args...)
	if err != nil {
		return nil, sqliteFault("list", err, false)
	}
	defer rows.Close()
	items := []listedRecord{}
	for rows.Next() {
		var item listedRecord
		if err = rows.Scan(&item.seq, &item.record, &item.label, &item.version); err != nil {
			return nil, sqliteFault("list", err, false)
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, sqliteFault("list", err, false)
	}
	return items, nil
}

func (k *sqliteStore) recentRuns(jobs []string, per int) ([]recentRun, error) {
	ids, slots := sqliteSlots(jobs)
	rows, err := k.db.Query(`SELECT job_id,id,state,created FROM (SELECT job_id,id,state,json_extract(record,'$.createdAt') AS created,row_number() OVER(PARTITION BY job_id ORDER BY seq DESC) AS rank FROM runs WHERE job_id IN (`+slots+`)) WHERE rank<=`+strconv.Itoa(per)+` ORDER BY job_id,rank`, ids...)
	if err != nil {
		return nil, sqliteFault("list", err, false)
	}
	defer rows.Close()
	items := []recentRun{}
	for rows.Next() {
		var r recentRun
		if err = rows.Scan(&r.job, &r.id, &r.state, &r.createdAt); err != nil {
			return nil, sqliteFault("list", err, false)
		}
		items = append(items, r)
	}
	if err = rows.Err(); err != nil {
		return nil, sqliteFault("list", err, false)
	}
	return items, nil
}

func (k *sqliteStore) listedTriggers(jobs []string) ([]listedTrigger, error) {
	ids, slots := sqliteSlots(jobs)
	rows, err := k.db.Query("SELECT job_id,record FROM triggers WHERE job_id IN ("+slots+") ORDER BY seq", ids...)
	if err != nil {
		return nil, sqliteFault("list", err, false)
	}
	defer rows.Close()
	items := []listedTrigger{}
	for rows.Next() {
		var item listedTrigger
		var raw string
		if err = rows.Scan(&item.job, &raw); err != nil {
			return nil, sqliteFault("list", err, false)
		}
		if err = json.Unmarshal([]byte(raw), &item.trigger); err != nil {
			return nil, storeFailure(faultUnreadable, "list", err)
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, sqliteFault("list", err, false)
	}
	return items, nil
}

// sqliteSlots are ids as a statement's arguments, and their placeholders.
func sqliteSlots(ids []string) ([]any, string) {
	args := make([]any, len(ids))
	slots := make([]string, len(ids))
	for i, id := range ids {
		args[i], slots[i] = id, "?"
	}
	return args, strings.Join(slots, ",")
}
