package runner

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/buildinfo"
)

// The journal of job activity (docs/design/activity-journal.md, accepted).
//
// Runner keeps each record's latest state. The journal keeps one entry for
// each change of state Runner makes, written in the transaction that makes the
// change, so that a change and its entry commit together or not at all. Each
// writer reads the record's stored state inside its transaction, and an entry
// is written exactly when the state it writes differs from the state it read.
// Entries are only ever appended, are served in order of their sequence, and
// are never pruned.
//
// What the journal does not establish: anything against the operator, who
// keeps the store and can rewrite it. It is the operator's own log, not
// chained and not signed. Its times are Runner's clock. Its identities are what
// Runner can know: the installation (whoever holds the owner bearer), a
// trigger, a trigger's credential by the revision its key was issued at, a
// cloud connection, or Runner itself; never a person.

const (
	// storeSchema is the store's schema since its journal began. An earlier
	// Runner holds a store to schema "1" and refuses this one, so no Runner
	// that writes no entries changes a store the journal began in.
	storeSchema = "2"
	// EventEntryVersion is the shape of an entry. A member added later raises
	// it.
	EventEntryVersion = "1"
	// eventPage is the most entries a page of the journal holds.
	eventPage = 50
	// maxEventReason bounds the reason an entry copies from its record.
	maxEventReason = 1024
	// refusalWindow is how long after a refusal an identical one is not
	// written again.
	refusalWindow = 60 * time.Second
)

const eventsSchema = `CREATE TABLE IF NOT EXISTS events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, entry_version TEXT NOT NULL, kind TEXT NOT NULL, at TEXT NOT NULL, by_actor TEXT NOT NULL, concerns TEXT NOT NULL, revision INTEGER, from_state TEXT, to_state TEXT, reason TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '{}', job_id TEXT NOT NULL DEFAULT '', release_id TEXT NOT NULL DEFAULT '', refusal TEXT NOT NULL DEFAULT '');
 CREATE INDEX IF NOT EXISTS events_job ON events(job_id,sequence);
 CREATE INDEX IF NOT EXISTS events_release ON events(release_id,sequence);
 CREATE INDEX IF NOT EXISTS events_refusal ON events(refusal,sequence) WHERE refusal<>'';`

// eventKinds are the kinds of entry Runner writes, in the record's order.
var eventKinds = []string{
	"trigger.configured", "trigger.paused", "trigger.resumed", "trigger.key-rotated",
	"occurrence.received", "occurrence.skipped", "occurrence.preparing", "occurrence.ready",
	"occurrence.submitted", "occurrence.failed", "occurrence.expired", "occurrence.needs-attention",
	"occurrence.cancelled", "occurrence.reconciled",
	"run.queued", "run.started", "run.expired", "run.completed", "run.failed", "run.interrupted",
	"release.previewed", "job.created", "admission.refused",
	"journal.began", "runner.started", "runner.stopped",
}

// Event is one entry of the journal, as the routes serve it.
type Event struct {
	Sequence     int64         `json:"sequence"`
	EntryVersion string        `json:"entryVersion"`
	Kind         string        `json:"kind"`
	At           string        `json:"at"`
	By           EventActor    `json:"by"`
	Concerns     EventConcerns `json:"concerns"`
	Revision     *int          `json:"revision,omitempty"`
	// From and To are the record's state before and after, for a kind that
	// concerns a record with a state. From is null when the change created
	// the record.
	From   json.RawMessage `json:"from,omitempty"`
	To     json.RawMessage `json:"to,omitempty"`
	Reason string          `json:"reason,omitempty"`
	EventDetail
}

// EventActor is who Runner can say initiated a change.
type EventActor struct {
	// Kind is installation, trigger, trigger-credential, cloud-connection or
	// runner.
	Kind string `json:"kind"`
	// Owner is the boot line's owner, for the installation: whoever holds the
	// owner bearer, not a person.
	Owner string `json:"owner,omitempty"`
	// Trigger and Revision name a trigger acting at a revision, or the
	// trigger whose credential an event delivery carried.
	Trigger  string `json:"trigger,omitempty"`
	Revision int    `json:"revision,omitempty"`
	// KeyRevision is the trigger revision at which the credential's key was
	// issued: null for a key issued before the journal began. Never the token
	// or its digest.
	KeyRevision json.RawMessage `json:"keyRevision,omitempty"`
	Connection  string          `json:"connection,omitempty"`
}

// EventConcerns are the ids a change is about.
type EventConcerns struct {
	Job        string `json:"job,omitempty"`
	Release    string `json:"release,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
	Occurrence string `json:"occurrence,omitempty"`
	Run        string `json:"run,omitempty"`
}

// EventDetail holds the members particular to a kind.
type EventDetail struct {
	PreviousRevision json.RawMessage    `json:"previousRevision,omitempty"`
	TriggerKind      string             `json:"triggerKind,omitempty"`
	KeyIssued        bool               `json:"keyIssued,omitempty"`
	NextAt           string             `json:"nextAt,omitempty"`
	OccurrenceKind   string             `json:"occurrenceKind,omitempty"`
	ScheduledAt      string             `json:"scheduledAt,omitempty"`
	MissedFrom       string             `json:"missedFrom,omitempty"`
	MissedThrough    string             `json:"missedThrough,omitempty"`
	ExpiresAt        string             `json:"expiresAt,omitempty"`
	Deadline         string             `json:"deadline,omitempty"`
	ReadyUntil       string             `json:"readyUntil,omitempty"`
	ChainSequence    int64              `json:"chainSequence,omitempty"`
	Interruption     *EventInterruption `json:"interruption,omitempty"`
	PackID           string             `json:"packId,omitempty"`
	PackVersion      string             `json:"packVersion,omitempty"`
	PackDigest       string             `json:"packDigest,omitempty"`
	RuntimeDigest    string             `json:"runtimeDigest,omitempty"`
	Tests            string             `json:"tests,omitempty"`
	Request          string             `json:"request,omitempty"`
	Status           int                `json:"status,omitempty"`
	Code             string             `json:"code,omitempty"`
	CoalescedSeconds int                `json:"coalescedSeconds,omitempty"`
	Held             *EventHeld         `json:"held,omitempty"`
	Version          string             `json:"version,omitempty"`
}

// EventInterruption is an interruption's own time: the moment Runner saw the
// stop, or, for one it found at a restart, the window the stop lies in, from
// the last time Runner recorded the work alive to the restart's own entry.
type EventInterruption struct {
	Seen             bool   `json:"seen"`
	At               string `json:"at,omitempty"`
	LastKnownRunning string `json:"lastKnownRunning,omitempty"`
	Restart          int64  `json:"restart,omitempty"`
}

// EventHeld is what a store held when its journal began, none of which has an
// entry.
type EventHeld struct {
	Jobs        int `json:"jobs"`
	Triggers    int `json:"triggers"`
	Occurrences int `json:"occurrences"`
	Runs        int `json:"runs"`
}

// EventPage is a page of the journal.
type EventPage struct {
	JournalBegan string  `json:"journalBegan"`
	Items        []Event `json:"items"`
	Next         int64   `json:"next"`
	More         bool    `json:"more"`
}

// change is an entry as a writer gives it.
type change struct {
	kind     string
	at       string
	by       EventActor
	concerns EventConcerns
	revision int // 0 for none: revisions start at 1
	states   bool
	from, to string // from is empty when the change created the record
	reason   string
	detail   EventDetail
	refusal  string
}

var runnerActor = EventActor{Kind: "runner"}

func (s *Service) installation() EventActor {
	return EventActor{Kind: "installation", Owner: s.cfg.Owner}
}
func triggerActor(trigger string, revision int) EventActor {
	return EventActor{Kind: "trigger", Trigger: trigger, Revision: revision}
}
func credentialActor(trigger string, keyRevision *int) EventActor {
	held := json.RawMessage("null")
	if keyRevision != nil {
		held = encode(*keyRevision)
	}
	return EventActor{Kind: "trigger-credential", Trigger: trigger, KeyRevision: held}
}
func triggerState(paused bool) string {
	if paused {
		return "paused"
	}
	return "enabled"
}
func revisionValue(revision int) json.RawMessage {
	if revision < 1 {
		return json.RawMessage("null")
	}
	return encode(revision)
}

// boundedReason is a reason cut at maxEventReason bytes, at a character.
func boundedReason(reason string) string {
	if len(reason) <= maxEventReason {
		return reason
	}
	cut := maxEventReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// appendEvent writes a change's entry inside tx, and returns its sequence.
func appendEvent(tx *sql.Tx, c change) (int64, error) {
	if c.at == "" {
		c.at = now()
	}
	var revision, from, to any
	if c.revision > 0 {
		revision = c.revision
	}
	if c.states {
		to = c.to
		if c.from != "" {
			from = c.from
		}
	}
	res, err := tx.Exec("INSERT INTO events(entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		EventEntryVersion, c.kind, c.at, string(encode(c.by)), string(encode(c.concerns)), revision, from, to, boundedReason(c.reason), string(encode(c.detail)), c.concerns.Job, c.concerns.Release, c.refusal)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// runEventKind names a run's change of state from one state to another; from
// is empty when the change created the run.
func runEventKind(from, to string) string {
	switch {
	case from == "":
		return "run.queued"
	case to == "running":
		return "run.started"
	case from == "queued" && to == "failed":
		// An automatic run whose queue time ran out before evaluation.
		return "run.expired"
	}
	return "run." + to
}

// occurrenceEventKind names an occurrence's change of state; from is empty
// when the change created the occurrence.
func occurrenceEventKind(from, to string) string {
	switch to {
	case "accepted":
		if from == "" {
			return "occurrence.received"
		}
		return "occurrence.ready"
	case "waiting":
		if from == "needs-attention" {
			return "occurrence.reconciled"
		}
		return "occurrence.preparing"
	case "preparing":
		return "occurrence.preparing"
	}
	return "occurrence." + to
}

func runChange(r Run, from string, by EventActor, detail EventDetail) change {
	c := change{kind: runEventKind(from, r.State), by: by, concerns: EventConcerns{Job: r.JobID, Release: r.ReleaseID, Run: r.ID}, states: true, from: from, to: r.State, reason: r.Problem, detail: detail}
	if r.Trigger != nil {
		c.concerns.Trigger = r.Trigger.TriggerID
		c.concerns.Occurrence = r.Trigger.OccurrenceID
		c.revision = r.Trigger.TriggerRevision
	}
	return c
}

func occurrenceChange(o Occurrence, from string, by EventActor) change {
	c := change{kind: occurrenceEventKind(from, o.State), by: by, concerns: EventConcerns{Job: o.JobID, Release: o.ReleaseID, Trigger: o.TriggerID, Occurrence: o.ID, Run: o.RunID}, revision: o.TriggerRevision, states: true, from: from, to: o.State, reason: o.Reason}
	switch {
	case from == "":
		c.detail = EventDetail{OccurrenceKind: o.Kind, ScheduledAt: o.ScheduledAt, MissedFrom: o.MissedFrom, MissedThrough: o.MissedThrough, ExpiresAt: o.ExpiresAt}
	case c.kind == "occurrence.preparing" && o.Preparation != nil:
		c.detail.Deadline = o.Preparation.Deadline
	case c.kind == "occurrence.ready":
		c.detail.ReadyUntil = o.ReadyUntil
	}
	return c
}

// recordRun writes a run's record inside tx, and the entry of its change of
// state, if its state changed.
func recordRun(tx *sql.Tx, r Run, by EventActor, detail EventDetail) error {
	var from string
	if err := tx.QueryRow("SELECT state FROM runs WHERE id=?", r.ID).Scan(&from); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE runs SET state=?,record=? WHERE id=?", r.State, runText(r), r.ID); err != nil {
		return err
	}
	if from == r.State {
		return nil
	}
	_, err := appendEvent(tx, runChange(r, from, by, detail))
	return err
}

// changeRun records a run's change of state and its entry in one transaction.
func (s *Service) changeRun(r Run) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = recordRun(tx, r, runnerActor, EventDetail{}); err != nil {
		return err
	}
	return tx.Commit()
}

// recordOccurrence writes an occurrence's record inside tx, and the entry of
// its change of state, if its state changed. With guard, the stored state must
// be guard, or nothing is written and errPreparationStopped is returned: a
// final state is not overwritten by a preparation still in flight.
func recordOccurrence(tx *sql.Tx, o Occurrence, guard string, by EventActor, interruption *EventInterruption) error {
	var from string
	if err := tx.QueryRow("SELECT state FROM occurrences WHERE id=?", o.ID).Scan(&from); err != nil {
		return err
	}
	if guard != "" && from != guard {
		return errPreparationStopped
	}
	if _, err := tx.Exec("UPDATE occurrences SET state=?,record=? WHERE id=?", o.State, string(encode(o)), o.ID); err != nil {
		return err
	}
	if from == o.State {
		return nil
	}
	c := occurrenceChange(o, from, by)
	c.detail.Interruption = interruption
	_, err := appendEvent(tx, c)
	return err
}

// changeOccurrence records an occurrence and the entry of its change of
// state, if any, in one transaction.
func (s *Service) changeOccurrence(o Occurrence, guard string, by EventActor) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = recordOccurrence(tx, o, guard, by, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateStore begins the journal of a store that has none, a new store or
// one an earlier Runner kept, in one transaction: it writes journal.began,
// with what the store held then and nothing back-filled, adds the column that
// names the revision a trigger's key was issued at, and raises the store's
// schema to storeSchema.
func (s *Service) migrateStore() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var schema string
	switch err = tx.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	}
	switch schema {
	case storeSchema:
		return nil
	case "", "1":
	default:
		return errors.New("runner store belongs to another owner, workspace or schema")
	}
	var held EventHeld
	for table, count := range map[string]*int{"jobs": &held.Jobs, "triggers": &held.Triggers, "occurrences": &held.Occurrences, "runs": &held.Runs} {
		if err = tx.QueryRow("SELECT count(*) FROM " + table).Scan(count); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("ALTER TABLE triggers ADD COLUMN key_revision INTEGER"); err != nil {
		return err
	}
	if _, err = appendEvent(tx, change{kind: "journal.began", by: runnerActor, detail: EventDetail{Held: &held}}); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO metadata VALUES ('schema',?)", storeSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// restart records this process's start, and in the same transaction marks
// what the last process left running, which Runner did not see stop: a run in
// evaluation is interrupted, and a legacy preparation failed. Neither is
// repeated. Each entry gives the window its stop lies in.
func (s *Service) restart() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := now()
	started, err := appendEvent(tx, change{kind: "runner.started", at: at, by: runnerActor, detail: EventDetail{Version: buildinfo.Version(), RuntimeDigest: s.runtimeDigest}})
	if err != nil {
		return err
	}
	var runs []Run
	if err = eachRecord(tx, "SELECT record FROM runs WHERE state='running' ORDER BY seq", func(b []byte) error {
		r, e := decodeRun(b)
		runs = append(runs, r)
		return e
	}); err != nil {
		return err
	}
	for _, r := range runs {
		r.State = "interrupted"
		r.FinishedAt = at
		r.Problem = "The runner stopped during evaluation. Retained attempt files may contain an audit record. This run was not automatically repeated."
		if err = recordRun(tx, r, runnerActor, EventDetail{Interruption: &EventInterruption{LastKnownRunning: r.StartedAt, Restart: started}}); err != nil {
			return err
		}
	}
	var occurrences []Occurrence
	if err = eachRecord(tx, "SELECT record FROM occurrences WHERE state='preparing' ORDER BY seq", func(b []byte) error {
		var o Occurrence
		e := json.Unmarshal(b, &o)
		occurrences = append(occurrences, o)
		return e
	}); err != nil {
		return err
	}
	for _, o := range occurrences {
		var since string
		if err = tx.QueryRow("SELECT at FROM events WHERE kind='occurrence.preparing' AND json_extract(concerns,'$.occurrence')=? ORDER BY sequence DESC LIMIT 1", o.ID).Scan(&since); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		o.State = "failed"
		o.Reason = "Source acquisition was interrupted. It was not repeated automatically."
		o.PendingInput = nil
		if err = recordOccurrence(tx, o, "", runnerActor, &EventInterruption{LastKnownRunning: since, Restart: started}); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// recordStop records an orderly stop, after the dispatcher, automation and
// background work have stopped. A process that is killed writes none.
func (s *Service) recordStop() {
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err = appendEvent(tx, change{kind: "runner.stopped", by: runnerActor}); err == nil {
		tx.Commit()
	}
}

// saveRelease stores a release that a preview made, with its entry.
func (s *Service) saveRelease(r Release) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO releases(id,record) VALUES (?,?)", r.ID, string(encode(r))); err != nil {
		return err
	}
	if _, err = appendEvent(tx, change{kind: "release.previewed", by: s.installation(), concerns: EventConcerns{Release: r.ID}, detail: EventDetail{PackID: r.PackID, PackVersion: r.PackVersion, PackDigest: r.PackDigest, RuntimeDigest: r.RuntimeDigest, Tests: r.Tests}}); err != nil {
		return err
	}
	return tx.Commit()
}

// refused journals a refused request to start work: one answered 400, 401,
// 409, 422 or 429 that names a job, trigger or release the store holds. A 404
// names nothing held, and a 5xx says the store or Runner failed; neither is
// journaled. The refusal is answered as it was whether or not its entry could
// be written.
func (s *Service) refused(request string, err error, by EventActor, concerns EventConcerns) {
	var api *apiError
	if !errors.As(err, &api) {
		return
	}
	switch api.Status {
	case 400, 401, 409, 422, 429:
	default:
		return
	}
	s.journalRefusal(change{kind: "admission.refused", by: by, concerns: concerns, detail: EventDetail{Request: request, Status: api.Status, Code: api.Code}})
}

// journalRefusal writes a refusal unless the journal holds an identical one,
// for the same request, records and code (or reason), written in the last
// minute. The check reads the journal in the refusal's own transaction, so it
// holds across a restart and keeps nothing in memory. A sender refused again
// and again cannot grow the journal without limit.
func (s *Service) journalRefusal(c change) {
	c.detail.CoalescedSeconds = int(refusalWindow / time.Second)
	c.refusal = strings.Join([]string{c.detail.Request, c.by.Connection, c.concerns.Job, c.concerns.Trigger, c.concerns.Release, c.detail.Code, c.reason}, "\x00")
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var last string
	switch err = tx.QueryRow("SELECT at FROM events WHERE refusal=? ORDER BY sequence DESC LIMIT 1", c.refusal).Scan(&last); {
	case err == nil:
		if at, e := time.Parse(time.RFC3339Nano, last); e == nil {
			if since := time.Since(at); since >= 0 && since < refusalWindow {
				return
			}
		}
	case !errors.Is(err, sql.ErrNoRows):
		return
	}
	if _, err = appendEvent(tx, c); err == nil {
		tx.Commit()
	}
}

// refusedRun journals a refused submission to a job the store holds.
func (s *Service) refusedRun(jobID string, err error) {
	if j, e := s.job(jobID); e == nil {
		s.refused("submit-run", err, s.installation(), EventConcerns{Job: j.ID, Release: j.ReleaseID})
	}
}

// refusedEvent journals a refused event delivery to a trigger the store
// holds. It is the credential's when the token is the trigger's current key,
// and the installation's otherwise.
func (s *Service) refusedEvent(triggerID, token string, err error) {
	t, hash, e := s.trigger(triggerID)
	if e != nil {
		return
	}
	j, e := s.job(t.JobID)
	if e != nil {
		return
	}
	by := s.installation()
	if len(token) == 64 && hash != "" && subtle.ConstantTimeCompare([]byte(digest([]byte(token))), []byte(hash)) == 1 {
		by = credentialActor(t.ID, t.keyRevision)
	}
	s.refused("deliver-event", err, by, EventConcerns{Job: j.ID, Release: j.ReleaseID, Trigger: t.ID})
}

// refusedJob journals a refused job creation that names a release the store
// holds.
func (s *Service) refusedJob(releaseID string, err error) {
	if _, e := s.release(releaseID); e == nil {
		s.refused("create-job", err, s.installation(), EventConcerns{Release: releaseID})
	}
}

// refusedCloudSignal journals a cloud signal Runner discards, with the reason
// it gives, and the trigger the signal was bound to, if any.
func (s *Service) refusedCloudSignal(connection string, t *Trigger, reason string) {
	c := change{kind: "admission.refused", by: EventActor{Kind: "cloud-connection", Connection: connection}, reason: reason, detail: EventDetail{Request: "cloud-signal"}}
	if t != nil {
		if j, e := s.job(t.JobID); e == nil {
			c.concerns = EventConcerns{Job: j.ID, Release: j.ReleaseID, Trigger: t.ID}
		}
	}
	s.journalRefusal(c)
}

// eventQuery reads a journal request's cursor and job: at most one of each, in
// a well-formed query, the cursor a decimal integer from 0.
func eventQuery(raw string) (int64, string, error) {
	q, err := url.ParseQuery(raw)
	if err != nil || len(q["after"]) > 1 {
		return 0, "", &apiError{400, "invalid_cursor", "Invalid page cursor."}
	}
	if len(q["job"]) > 1 {
		return 0, "", &apiError{400, "invalid_filter", "Name at most one job."}
	}
	var after int64
	if v := q.Get("after"); v != "" {
		if strings.Trim(v, "0123456789") != "" || len(v) > 18 {
			return 0, "", &apiError{400, "invalid_cursor", "Invalid page cursor."}
		}
		if after, err = strconv.ParseInt(v, 10, 64); err != nil {
			return 0, "", &apiError{400, "invalid_cursor", "Invalid page cursor."}
		}
	}
	return after, q.Get("job"), nil
}

// eventPageAfter is up to eventPage entries after a sequence, oldest first,
// among those the condition selects.
func (s *Service) eventPageAfter(after int64, where string, args ...any) (EventPage, error) {
	return eventPageFrom(s.db, after, where, args...)
}

// eventPageFrom reads a page of the journal from a store's database.
func eventPageFrom(db *sql.DB, after int64, where string, args ...any) (EventPage, error) {
	page := EventPage{Items: []Event{}, Next: after}
	rows, err := db.Query("SELECT sequence,entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail FROM events WHERE sequence>? AND ("+where+") ORDER BY sequence LIMIT ?", append(append([]any{after}, args...), eventPage+1)...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		if len(page.Items) == eventPage {
			page.More = true
			break
		}
		var e Event
		var by, concerns, detail string
		var revision sql.NullInt64
		var from, to sql.NullString
		if err = rows.Scan(&e.Sequence, &e.EntryVersion, &e.Kind, &e.At, &by, &concerns, &revision, &from, &to, &e.Reason, &detail); err != nil {
			rows.Close()
			return page, err
		}
		if err = errors.Join(json.Unmarshal([]byte(by), &e.By), json.Unmarshal([]byte(concerns), &e.Concerns), json.Unmarshal([]byte(detail), &e.EventDetail)); err != nil {
			rows.Close()
			return page, err
		}
		if revision.Valid {
			v := int(revision.Int64)
			e.Revision = &v
		}
		if to.Valid {
			e.To = encode(to.String)
			e.From = json.RawMessage("null")
			if from.Valid {
				e.From = encode(from.String)
			}
		}
		page.Items = append(page.Items, e)
		page.Next = e.Sequence
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	err = db.QueryRow("SELECT at FROM events WHERE kind='journal.began' ORDER BY sequence LIMIT 1").Scan(&page.JournalBegan)
	return page, err
}

// jobEventsHandler serves a job's entries: those naming it, and those naming
// its release alone (its preview, a refused creation).
func (s *Service) jobEventsHandler(w http.ResponseWriter, r *http.Request) {
	after, _, err := eventQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, err)
		return
	}
	j, err := s.job(r.PathValue("job"))
	if err != nil {
		failure(w, err)
		return
	}
	page, err := s.eventPageAfter(after, "job_id=? OR (job_id='' AND release_id=?)", j.ID, j.ReleaseID)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, page)
}

// eventsHandler serves every entry; with a job, that job's entries and
// Runner's own, which concern no job but explain a gap in every job's.
func (s *Service) eventsHandler(w http.ResponseWriter, r *http.Request) {
	after, job, err := eventQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, err)
		return
	}
	where, args := "1=1", []any{}
	if job != "" {
		if _, err = s.job(job); err != nil {
			failure(w, err)
			return
		}
		where, args = "job_id=? OR kind IN ('journal.began','runner.started','runner.stopped')", []any{job}
	}
	page, err := s.eventPageAfter(after, where, args...)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, page)
}
