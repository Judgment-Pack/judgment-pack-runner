package runner

// The sqlite kind's journal (docs/design/activity-journal.md): the events
// table, an entry's row as the kind writes and scans it, and its columns, as
// the Runner made them before the contract stood in front of them.

import "database/sql"

const eventsSchema = `CREATE TABLE IF NOT EXISTS events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, entry_version TEXT NOT NULL, kind TEXT NOT NULL, at TEXT NOT NULL, by_actor TEXT NOT NULL, concerns TEXT NOT NULL, revision INTEGER, from_state TEXT, to_state TEXT, reason TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '{}', job_id TEXT NOT NULL DEFAULT '', release_id TEXT NOT NULL DEFAULT '', refusal TEXT NOT NULL DEFAULT '');
 CREATE INDEX IF NOT EXISTS events_job ON events(job_id,sequence);
 CREATE INDEX IF NOT EXISTS events_release ON events(release_id,sequence);
 CREATE INDEX IF NOT EXISTS events_refusal ON events(refusal,sequence) WHERE refusal<>'';`

const eventColumns = "sequence,entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail"

// appendEvent writes a change's entry inside a sqlite kind's transaction, and
// returns its sequence.
func appendEvent(tx *sql.Tx, c change) (int64, error) {
	e := journalRowOf(c)
	res, err := tx.Exec("INSERT INTO events(entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		e.version, e.kind, e.at, e.by, e.concerns, sql.NullInt64{Int64: e.revision.value, Valid: e.revision.held}, sqliteNullText(e.from), sqliteNullText(e.to), e.reason, e.detail, e.job, e.release, e.refusal)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// sqliteNullText is a nullable column's value as the kind writes it.
func sqliteNullText(t nullText) sql.NullString {
	return sql.NullString{String: t.value, Valid: t.held}
}

// scanJournalRow reads one entry's sequence and row from a row of
// eventColumns, not yet decoded.
func scanJournalRow(rows *sql.Rows) (int64, journalRow, error) {
	var sequence int64
	var e journalRow
	var revision sql.NullInt64
	var from, to sql.NullString
	err := rows.Scan(&sequence, &e.version, &e.kind, &e.at, &e.by, &e.concerns, &revision, &from, &to, &e.reason, &e.detail)
	e.revision = nullInt{value: revision.Int64, held: revision.Valid}
	e.from, e.to = nullText{value: from.String, held: from.Valid}, nullText{value: to.String, held: to.Valid}
	return sequence, e, err
}
