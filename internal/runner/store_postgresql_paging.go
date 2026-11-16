package runner

// The PostgreSQL kind's cursors (store_paging.go,
// postgresql/migrations/0007_paging_cursors.sql).

func (r pgReads) pagingCursors(job string) (map[string]PagingCursor, error) {
	return cursorRows(func(each func([]byte) error) error {
		return r.each("cursor", "SELECT record FROM paging_cursors WHERE job_id=$1 ORDER BY source", []any{job}, each)
	})
}

// advancePagingCursor writes the row with the job's scope in its record, as
// the migration's generated column reads it; the worker login writes it,
// and the row policy holds a request to its scope (0007_paging_cursors.sql).
func (t *pgTx) advancePagingCursor(c PagingCursor, read string, open *UnfinishedWindow) (bool, error) {
	end, position := unfinishedHeld(open)
	var n int64
	var err error
	if read == "" {
		n, err = t.exec("cursor", "INSERT INTO paging_cursors(job_id,source,record) VALUES ($1,$2,$3) ON CONFLICT (job_id,source) DO NOTHING", c.JobID, c.Source, string(encode(c)))
	} else {
		n, err = t.exec("cursor", "UPDATE paging_cursors SET record=$1 WHERE job_id=$2 AND source=$3 AND "+pgJSON("record", "value")+"=$4 AND coalesce("+pgJSON("record", "unfinished", "end")+",'')=$5 AND coalesce("+pgJSON("record", "unfinished", "position")+",'')=$6", string(encode(c)), c.JobID, c.Source, read, end, position)
	}
	return n == 1, err
}
