package runner

// The jobs' cursors between runs (paging_loop.go; docs/design/paging.md,
// section 4.2): one row per job and paged source, in a table of its own,
// whose record carries the job's scope identifier. Each kind's statements
// are in its own files (store_sqlite_paging.go, store_postgresql_paging.go).

import "encoding/json"

// pagingStoreSchema is the schema of a store that holds a job keeping a
// cursor, raised in the transaction that creates the first, from any schema
// before it (raiseSchema). An earlier Runner would drop the job's paging
// member from every job it rewrites, and complete its runs without moving
// their cursors; every earlier Runner refuses a store at a schema not its
// own. A store that holds none keeps its schema, byte for byte.
const pagingStoreSchema = "8"

// cursorRows reads every row of a job's cursors, by source.
func cursorRows(each func(func([]byte) error) error) (map[string]PagingCursor, error) {
	out := map[string]PagingCursor{}
	err := each(func(b []byte) error {
		var c PagingCursor
		if err := json.Unmarshal(b, &c); err != nil {
			return err
		}
		out[c.Source] = c
		return nil
	})
	return out, err
}
