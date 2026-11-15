package runner

// The postgresql kind's writes (docs/design/store-postgresql.md): the sqlite
// kind's statements translated, the claim fenced by its token and lease in
// the statement that writes (D3), and the installation's rules held by the
// rows no policy narrows (D9), moved in the statements of the operations that
// change what they count.

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// pgLeaseUntil is a lease from the database's clock, from the start of the
// transaction that sets it: the earlier time, so the shorter lease. A
// claimant's write is fenced by a live lease at the start of its statement
// (statement_timestamp()), after any wait for journal_head's lock, so that
// one that waited past its lease's end writes nothing (D3); the tick's test
// of a lease run out keeps now(), which takes it to run out later.
var pgLeaseUntil = "now() + interval '" + strconv.Itoa(int(claimLease/time.Second)) + " seconds'"

// The states a count holds a record in (D9).
var (
	pgActiveStates  = []string{"queued", "running"}
	pgPendingStates = []string{"accepted", "preparing", "waiting"}
)

func errNoProfileRow(key string) error { return fmt.Errorf("no profile row is held under %q", key) }

func (t *pgTx) exec(op, query string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(t.ctx(), query, args...)
	if err != nil {
		return 0, pgFault(op, err, true)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, pgFault(op, err, true)
	}
	return n, nil
}

// move moves a count by the change of a record from one state to another.
func (t *pgTx) move(name string, states []string, from, to string) error {
	by := 0
	if slices.Contains(states, to) {
		by++
	}
	if slices.Contains(states, from) {
		by--
	}
	if by == 0 {
		return nil
	}
	_, err := t.exec("count", "UPDATE counts SET value=value+$1 WHERE name=$2", by, name)
	return err
}

func (t *pgTx) heldCount(name string) (int, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx(), "SELECT value FROM counts WHERE name=$1", name).Scan(&n)
	return n, pgFault("count", err, false)
}

func (t *pgTx) activeRuns() (int, error) { return t.heldCount("active-runs") }

func (t *pgTx) pendingOccurrences() (int, error) { return t.heldCount("pending-occurrences") }

func (t *pgTx) createRun(r Run, caller, key, requestDigest string) error {
	if e := validateStoredRun(r); e != nil {
		return e
	}
	if _, err := t.exec("run", "INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES ($1,$2,$3,$4,$5,$6,$7)", r.ID, r.JobID, caller, key, requestDigest, r.State, runText(r)); err != nil {
		return err
	}
	return t.move("active-runs", pgActiveStates, "", r.State)
}

// putRun writes r over the run held under its id, fenced as D3 says, in the
// statement that writes: an expected claim must be the one held, by its token
// and attempt, with its lease live by the database's clock; a running
// expectation with no claim must find none; an interruption's must find none
// or one whose lease ran out. A run written under its claim keeps the claim's
// lease, one written under a new claim takes a lease from now, and one
// written under none holds none.
func (t *pgTx) putRun(r Run, want runExpected) (string, error) {
	if e := validateStoredRun(r); e != nil {
		return "", e
	}
	var from string
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT state FROM runs WHERE id=$1 FOR UPDATE", r.ID).Scan(&from); err != nil {
		return "", pgFault("run", err, false)
	}
	if want.state != anyState && from != want.state {
		return from, storeFailure(faultConflict, "run", fmt.Errorf("the run %s is %s, not %s", r.ID, from, want.state))
	}
	var a pgArgs
	id := a.add(r.ID)
	set := "state=" + a.add(r.State) + ",record=" + a.add(runText(r))
	if r.claim == (runClaim{}) {
		set += ",claim_id=NULL,claim_attempt=NULL,lease_until=NULL"
	} else {
		token, attempt := a.add(r.claim.token), a.add(r.claim.attempt)
		set += ",lease_until=CASE WHEN claim_id=" + token + " AND claim_attempt=" + attempt + " THEN lease_until ELSE " + pgLeaseUntil + " END,claim_id=" + token + ",claim_attempt=" + attempt
	}
	where := "id=" + id
	switch {
	case want.lapsed:
		where += " AND (claim_id IS NULL OR lease_until<=now())"
	case want.state == "running" && want.claim == (runClaim{}):
		where += " AND claim_id IS NULL"
	case want.claim != (runClaim{}):
		where += " AND claim_id=" + a.add(want.claim.token) + " AND claim_attempt=" + a.add(want.claim.attempt) + " AND lease_until>statement_timestamp()"
	}
	n, err := t.exec("run", "UPDATE runs SET "+set+" WHERE "+where, a...)
	if err != nil {
		return from, err
	}
	if n == 0 {
		return from, storeFailure(faultConflict, "run", errors.New("the run is held under a claim the writer does not hold, or its claim's lease ran out"))
	}
	return from, t.move("active-runs", pgActiveStates, from, r.State)
}

// renewClaim holds the claim's lease for claimLease from the database's
// clock, while the run is held running under the claim and its lease is
// live when the statement starts.
func (t *pgTx) renewClaim(run string, claim runClaim) error {
	if claim == (runClaim{}) {
		return storeFailure(faultConflict, "run", errors.New("the run is not held running under a claim"))
	}
	n, err := t.exec("run", "UPDATE runs SET lease_until="+pgLeaseUntil+" WHERE id=$1 AND state='running' AND claim_id=$2 AND claim_attempt=$3 AND lease_until>statement_timestamp()", run, claim.token, claim.attempt)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultConflict, "run", errors.New("the run is not held running under the claim, or its lease ran out"))
	}
	return nil
}

func (t *pgTx) runningRuns() ([]Run, error) {
	var runs []Run
	err := t.each("queue", "SELECT record FROM runs WHERE state='running' ORDER BY seq", nil, func(b []byte) error {
		r, e := decodeRun(b)
		runs = append(runs, r)
		return e
	})
	return runs, err
}

// lastEntry locks the chain's head row first, so that a second finisher waits
// for the first (D14).
func (t *pgTx) lastEntry() (int64, []byte, error) {
	var last int64
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT last FROM chain_head FOR UPDATE").Scan(&last); err != nil {
		return 0, nil, pgFault("chain", err, false)
	}
	if last == 0 {
		return 0, nil, storeFailure(faultAbsent, "chain", sql.ErrNoRows)
	}
	var line []byte
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT line FROM run_chain WHERE sequence=$1", last).Scan(&line); err != nil {
		return 0, nil, pgFault("chain", err, false)
	}
	return last, line, nil
}

func (t *pgTx) createEntry(sequence int64, run string, line []byte) error {
	if _, err := t.exec("chain", "INSERT INTO run_chain(sequence,run,line) VALUES ($1,$2,$3)", sequence, run, line); err != nil {
		return err
	}
	_, err := t.exec("chain", "UPDATE chain_head SET last=$1 WHERE last<$1", sequence)
	return err
}

func (t *pgTx) createRelease(r Release) error {
	if e := validateStoredRelease(r); e != nil {
		return e
	}
	_, err := t.exec("release", "INSERT INTO releases(id,record) VALUES ($1,$2)", r.ID, string(encode(r)))
	return err
}

// appendJournal takes the entry's sequence from journal_head, whose lock the
// transaction holds (D13).
func (t *pgTx) appendJournal(c change) (int64, error) {
	e := journalRowOf(c)
	var sequence int64
	if err := t.tx.QueryRowContext(t.ctx(), "UPDATE journal_head SET last=last+1 RETURNING last").Scan(&sequence); err != nil {
		return 0, pgFault("journal", err, true)
	}
	_, err := t.exec("journal", "INSERT INTO events(sequence,entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)",
		sequence, e.version, e.kind, e.at, e.by, e.concerns, sql.NullInt64{Int64: e.revision.value, Valid: e.revision.held}, sql.NullString{String: e.from.value, Valid: e.from.held}, sql.NullString{String: e.to.value, Valid: e.to.held}, e.reason, e.detail, e.job, e.release, []byte(e.refusal))
	return sequence, err
}

// pgSchemaOrder orders the schemas a Runner of this kind knows.
var pgSchemaOrder = map[string]int{storeSchema: 1, scopedStoreSchema: 2, retryStoreSchema: 3, scheduleStoreSchema: 4}

// raiseSchema writes nothing: the migration wrote the schema row (D15). A
// schema the migration has not reached is refused.
func (t *pgTx) raiseSchema(schema string) error {
	if schema != scopedStoreSchema && schema != retryStoreSchema && schema != scheduleStoreSchema {
		return storeFailure(faultRefused, "schema", fmt.Errorf("the schema %q is not raised through the contract", schema))
	}
	var held string
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT value FROM metadata WHERE key='schema'").Scan(&held); err != nil {
		return pgFault("schema", err, false)
	}
	if pgSchemaOrder[held] < pgSchemaOrder[schema] {
		return storeFailure(faultRefused, "schema", fmt.Errorf("the store's migrations have not reached the schema %q", schema))
	}
	return nil
}

func (t *pgTx) lastRefusal(key string) (string, error) {
	var at string
	err := t.tx.QueryRowContext(t.ctx(), "SELECT at FROM events WHERE refusal=$1 ORDER BY sequence DESC LIMIT 1", []byte(key)).Scan(&at)
	return at, pgFault("journal", err, false)
}

func (t *pgTx) putProfileRow(key, row string) error {
	if !profileRowKeys[key] {
		return storeFailure(faultRefused, "profiles", errNoProfileRow(key))
	}
	_, err := t.exec("profiles", "INSERT INTO metadata(key,value) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value=excluded.value", key, row)
	return err
}

// putBrief keeps the record as the bytes it is given, with its subject's
// scope, read from the subject's row in the statement that writes it (D5): a
// subject the transaction does not reach is refused.
func (t *pgTx) putBrief(subject string, record []byte) error {
	table, id := pgSubject(subject)
	if table == "" {
		return storeFailure(faultRefused, "brief", errors.New("a brief's subject is a job or a run"))
	}
	column := "job_id"
	if table == "runs" {
		column = "run_id"
	}
	n, err := t.exec("brief", "INSERT INTO briefs(subject,record,"+column+",scope) SELECT $1,$2,id,scope FROM "+table+" WHERE id=$3 ON CONFLICT (subject) DO UPDATE SET record=excluded.record", subject, record, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultRefused, "brief", errors.New("the brief's subject is not held"))
	}
	return nil
}

// createTrigger raises counts.triggers while it is below the installation's
// trigger limit, as this Runner is configured with it (D9; the Runner checks
// it before it makes a trigger, store.go). Runners of one store configured
// with different limits each hold the row to their own.
func (t *pgTx) createTrigger(tr Trigger) error {
	if _, err := t.exec("trigger", "INSERT INTO triggers(id,job_id,record) VALUES ($1,$2,$3)", tr.ID, tr.JobID, string(encode(tr))); err != nil {
		return err
	}
	n, err := t.exec("trigger", "UPDATE counts SET value=value+1 WHERE name='triggers' AND value<$1", t.p.triggerLimit)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultRefused, "trigger", errors.New("the installation holds its limit of triggers"))
	}
	return nil
}

// putTrigger writes the trigger over the one held at the version it was read
// at, and moves the version on (D17): a writer that read it before another
// wrote it meets faultConflict and writes nothing.
func (t *pgTx) putTrigger(tr Trigger, hash string, history bool) error {
	var keyRevision any
	if tr.keyRevision != nil {
		keyRevision = *tr.keyRevision
	}
	var held int64
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT version FROM triggers WHERE id=$1 FOR UPDATE", tr.ID).Scan(&held); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storeFailure(faultAbsent, "trigger", errNoTrigger)
		}
		return pgFault("trigger", err, false)
	}
	if held != tr.version {
		return storeFailure(faultConflict, "trigger", fmt.Errorf("%w: %s", errTriggerWritten, tr.ID))
	}
	n, err := t.exec("trigger", "UPDATE triggers SET record=$1,key_hash=$2,observed=$3,pending=$4,pending_at=$5,key_revision=$6,version=version+1 WHERE id=$7 AND version=$8", string(encode(tr)), hash, tr.observed, tr.pending, tr.pendingAt, keyRevision, tr.ID, tr.version)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultConflict, "trigger", fmt.Errorf("%w: %s", errTriggerWritten, tr.ID))
	}
	if history {
		_, err = t.exec("trigger", "INSERT INTO trigger_revisions(trigger_id,revision,record) VALUES ($1,$2,$3)", tr.ID, tr.Revision, string(encode(tr)))
	}
	return err
}

// reviseTrigger moves the trigger's version on, as putTrigger does (D17).
func (t *pgTx) reviseTrigger(tr Trigger) error {
	n, err := t.exec("trigger", "UPDATE triggers SET record=$1,version=version+1 WHERE id=$2", string(encode(tr)), tr.ID)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultAbsent, "trigger", errNoTrigger)
	}
	_, err = t.exec("trigger", "INSERT INTO trigger_revisions(trigger_id,revision,record) VALUES ($1,$2,$3)", tr.ID, tr.Revision, string(encode(tr)))
	return err
}

func (t *pgTx) triggerRecords(job string) ([]Trigger, error) {
	var triggers []Trigger
	err := t.each("trigger", "SELECT record FROM triggers WHERE job_id=$1 ORDER BY seq", []any{job}, func(b []byte) error {
		tr, err := pgRecordOf[Trigger]("trigger", b, nil)
		triggers = append(triggers, tr)
		return err
	})
	return triggers, err
}

func (t *pgTx) createOccurrence(o Occurrence, identity, payload string) error {
	if _, err := t.exec("occurrence", "INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES ($1,$2,$3,$4,$5,$6,$7)", o.ID, o.JobID, o.TriggerID, identity, payload, o.State, string(encode(o))); err != nil {
		return err
	}
	return t.move("pending-occurrences", pgPendingStates, "", o.State)
}

// putOccurrence fences the write by the occurrence's claim as putRun does a
// run's (D18): a holder's by its claim and a lease live at the statement's
// start, after any wait for the lock; any other's by no live claim at all.
func (t *pgTx) putOccurrence(o Occurrence, want occurrenceExpected) (string, error) {
	var from string
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT state FROM occurrences WHERE id=$1 FOR UPDATE", o.ID).Scan(&from); err != nil {
		return "", pgFault("occurrence", err, false)
	}
	if want.state != anyState && from != want.state {
		return from, storeFailure(faultConflict, "occurrence", fmt.Errorf("the occurrence %s is %s, not %s", o.ID, from, want.state))
	}
	var a pgArgs
	id := a.add(o.ID)
	set := "state=" + a.add(o.State) + ",record=" + a.add(string(encode(o)))
	if !want.keep || o.claim == (occurrenceClaim{}) {
		set += ",claim_id=NULL,lease_until=NULL"
	}
	where := "id=" + id
	switch {
	case want.overrule:
	case o.claim == (occurrenceClaim{}):
		where += " AND " + pgUnclaimed
	default:
		where += " AND claim_id=" + a.add(o.claim.token) + " AND lease_until>statement_timestamp()"
	}
	n, err := t.exec("occurrence", "UPDATE occurrences SET "+set+" WHERE "+where, a...)
	if err != nil {
		return from, err
	}
	if n == 0 {
		return from, storeFailure(faultConflict, "occurrence", errors.New("the occurrence is held under a claim the writer does not hold, or its claim's lease ran out"))
	}
	return from, t.move("pending-occurrences", pgPendingStates, from, o.State)
}

// claimOccurrence takes the claim where the occurrence is held in state
// under none live, by the transaction's now() as the lapse test is (D3), and
// returns the record as held.
func (t *pgTx) claimOccurrence(id, state string, claim occurrenceClaim) (Occurrence, error) {
	if claim == (occurrenceClaim{}) {
		return Occurrence{}, storeFailure(faultConflict, "occurrence", errors.New("no claim to hold the occurrence under"))
	}
	var from string
	var free bool
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT state,"+pgUnclaimed+" FROM occurrences WHERE id=$1 FOR UPDATE", id).Scan(&from, &free); err != nil {
		return Occurrence{}, pgFault("occurrence", err, false)
	}
	if from != state || !free {
		return Occurrence{}, storeFailure(faultConflict, "occurrence", fmt.Errorf("the occurrence %s is %s, or held under a live claim", id, from))
	}
	var record []byte
	err := t.tx.QueryRowContext(t.ctx(), "UPDATE occurrences SET claim_id=$1,lease_until="+pgLeaseUntil+" WHERE id=$2 RETURNING record", claim.token, id).Scan(&record)
	o, err := pgRecordOf[Occurrence]("occurrence", record, pgFault("occurrence", err, true))
	o.claim = claim
	return o, err
}

// renewOccurrenceClaim holds the claim's lease for claimLease from the
// database's clock, while the occurrence is held under the claim and its
// lease is live when the statement starts.
func (t *pgTx) renewOccurrenceClaim(id string, claim occurrenceClaim) error {
	if claim == (occurrenceClaim{}) {
		return storeFailure(faultConflict, "occurrence", errors.New("the occurrence is not held under a claim"))
	}
	n, err := t.exec("occurrence", "UPDATE occurrences SET lease_until="+pgLeaseUntil+" WHERE id=$1 AND claim_id=$2 AND lease_until>statement_timestamp()", id, claim.token)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultConflict, "occurrence", errors.New("the occurrence is not held under the claim, or its lease ran out"))
	}
	return nil
}

// createOccurrenceKey keeps the key with its occurrence's scope, read from
// the occurrence's row (D5): an occurrence the transaction does not reach is
// refused.
func (t *pgTx) createOccurrenceKey(occurrence, hash string) error {
	n, err := t.exec("occurrence", "INSERT INTO occurrence_keys(occurrence_id,key_hash,scope) SELECT id,$2,scope FROM occurrences WHERE id=$1", occurrence, hash)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultRefused, "occurrence", errors.New("the key's occurrence is not held"))
	}
	return nil
}

func (t *pgTx) jobActivity(job string) (int, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx(), "SELECT (SELECT count(*) FROM runs WHERE job_id=$1 AND state IN ('queued','running'))+(SELECT count(*) FROM occurrences WHERE job_id=$1 AND state IN ('accepted','preparing','waiting','needs-attention'))", job).Scan(&n)
	return n, pgFault("occurrence", err, false)
}

func (t *pgTx) openOccurrences(job string) (int, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx(), "SELECT count(*) FROM occurrences WHERE job_id=$1 AND state IN ('accepted','preparing','waiting','needs-attention')", job).Scan(&n)
	return n, pgFault("occurrence", err, false)
}

func (t *pgTx) preparingOccurrences() ([]Occurrence, error) {
	var occurrences []Occurrence
	err := t.each("occurrence", "SELECT record FROM occurrences WHERE state='preparing' ORDER BY seq", nil, func(b []byte) error {
		o, err := pgRecordOf[Occurrence]("occurrence", b, nil)
		occurrences = append(occurrences, o)
		return err
	})
	return occurrences, err
}

func (t *pgTx) lastPreparing(occurrence string) (string, error) {
	var since string
	err := t.tx.QueryRowContext(t.ctx(), "SELECT at FROM events WHERE kind='occurrence.preparing' AND "+pgJSON("concerns", "occurrence")+"=$1 ORDER BY sequence DESC LIMIT 1", occurrence).Scan(&since)
	return since, pgFault("journal", err, false)
}

// createJob meets the release's keys in the order job_creations,
// job_revisions, jobs (D9), then holds the release's identities.
func (t *pgTx) createJob(j Job) error {
	record := string(encode(j))
	if _, err := t.exec("job", "INSERT INTO job_creations(release_id,record) VALUES ($1,$2)", j.ReleaseID, record); err != nil {
		return err
	}
	if _, err := t.exec("job", "INSERT INTO job_revisions(job_id,revision,release_id,record) VALUES ($1,$2,$3,$4)", j.ID, j.Revision, j.ReleaseID, record); err != nil {
		return err
	}
	if _, err := t.exec("job", "INSERT INTO jobs(id,release_id,record) VALUES ($1,$2,$3)", j.ID, j.ReleaseID, record); err != nil {
		return err
	}
	return t.holdIdentities(j.ReleaseID)
}

// reviseJob meets the revision's release key before the job's (D9).
func (t *pgTx) reviseJob(j Job, request string) error {
	var held int
	if err := t.tx.QueryRowContext(t.ctx(), "SELECT count(*) FROM jobs WHERE id=$1", j.ID).Scan(&held); err != nil {
		return pgFault("job", err, false)
	}
	if held == 0 {
		return storeFailure(faultAbsent, "job", errNoJob)
	}
	record := string(encode(j))
	if _, err := t.exec("job", "INSERT INTO job_revisions(job_id,revision,release_id,record,request_digest) VALUES ($1,$2,$3,$4,$5)", j.ID, j.Revision, j.ReleaseID, record, request); err != nil {
		return err
	}
	n, err := t.exec("job", "UPDATE jobs SET release_id=$1,record=$2 WHERE id=$3", j.ReleaseID, record, j.ID)
	if err != nil {
		return err
	}
	if n == 0 {
		return storeFailure(faultAbsent, "job", errNoJob)
	}
	return t.holdIdentities(j.ReleaseID)
}

// holdIdentities keeps the pack and dialect identities of a revision's
// release, for every scope (D9): one row for each release a revision names,
// so that packConflicts and dialectConflicts count the revisions as the
// sqlite kind counts them. It refuses nothing: the Runner checks a conflict
// in the same transaction, before it writes.
func (t *pgTx) holdIdentities(release string) error {
	for _, id := range []struct {
		table, name             string
		member, version, digest []string
	}{
		{"pack_identities", "pack", []string{"packId"}, []string{"packVersion"}, []string{"packDigest"}},
		{"dialect_identities", "dialect", []string{"dialect", "id"}, []string{"dialect", "version"}, []string{"dialect", "digest"}},
	} {
		member, version, digest := pgJSON("record", id.member...), pgJSON("record", id.version...), pgJSON("record", id.digest...)
		if _, err := t.exec("job", "INSERT INTO "+id.table+"(release_id,"+id.name+",version,digest) SELECT id,"+member+","+version+","+digest+" FROM releases WHERE id=$1 AND "+member+" IS NOT NULL AND "+version+" IS NOT NULL AND "+digest+" IS NOT NULL ON CONFLICT DO NOTHING", release); err != nil {
			return err
		}
	}
	return nil
}
