package runner

// The sqlite kind's cursors (store_paging.go).

import (
	"database/sql"
	"errors"
)

// pagingCursorSchema is the sqlite kind's cursor table, made by the
// transaction that raises the store to pagingStoreSchema: a store with no
// job that keeps a cursor holds no such table, and its schema is the one an
// earlier Runner made, object for object.
const pagingCursorSchema = `CREATE TABLE IF NOT EXISTS paging_cursors(job_id TEXT NOT NULL,source TEXT NOT NULL,record TEXT NOT NULL,PRIMARY KEY(job_id,source));`

func (r sqliteReads) pagingCursors(job string) (map[string]PagingCursor, error) {
	// A store never raised to the paging schema has no table, and no cursor.
	var tables int
	if err := r.q.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='paging_cursors'").Scan(&tables); err != nil {
		return nil, sqliteFault("cursor", err, false)
	}
	if tables == 0 {
		return map[string]PagingCursor{}, nil
	}
	rows, err := r.q.Query("SELECT record FROM paging_cursors WHERE job_id=? ORDER BY source", job)
	if err != nil {
		return nil, sqliteFault("cursor", err, false)
	}
	defer rows.Close()
	out, err := cursorRows(func(each func([]byte) error) error {
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			if err := each([]byte(raw)); err != nil {
				return storeFailure(faultUnreadable, "cursor", err)
			}
		}
		return rows.Err()
	})
	if err != nil {
		var fault *storeError
		if errors.As(err, &fault) {
			return nil, err
		}
		return nil, sqliteFault("cursor", err, false)
	}
	return out, nil
}

func (t *sqliteTx) advancePagingCursor(c PagingCursor, read string) (bool, error) {
	var res sql.Result
	var err error
	if read == "" {
		res, err = t.tx.Exec("INSERT INTO paging_cursors(job_id,source,record) VALUES (?,?,?) ON CONFLICT DO NOTHING", c.JobID, c.Source, string(encode(c)))
	} else {
		res, err = t.tx.Exec("UPDATE paging_cursors SET record=? WHERE job_id=? AND source=? AND json_extract(record,'$.value')=?", string(encode(c)), c.JobID, c.Source, read)
	}
	if err != nil {
		return false, sqliteFault("cursor", err, true)
	}
	n, err := res.RowsAffected()
	return n == 1, sqliteFault("cursor", err, true)
}
