package runner

// The in-memory kind, for tests (docs/design/store-interface.md): the store
// contract's semantics held in memory, run beside the sqlite kind by the
// conformance suite. It keeps records as their encoded bytes, as the sqlite
// kind keeps them as text, so a record is written and read through the same
// encoding. A transaction works on a copy of the state, under the kind's own
// lock held from its start to its end, and the copy replaces the state only
// when the transaction commits: transactions are serializable, and one that
// fails keeps nothing.

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// errNotHeld is the cause of the in-memory kind's faultAbsent.
var errNotHeld = errors.New("nothing is held under that id or key")

type memoryRun struct {
	seq                                  int64
	id, job, caller, idem, digest, state string
	record                               []byte
	// claim is the claim the run is held under, its lease held until
	// leaseUntil by the store's clock (D3): the second kind's claim_id,
	// claim_attempt and lease_until.
	claim      runClaim
	leaseUntil time.Time
}

type memoryEntry struct {
	sequence int64
	run      string
	line     []byte
}

// memoryRevision is a job's revision, by the release it names, as it was
// made, with the digest of the upgrade request that made it.
type memoryRevision struct {
	job      string
	revision int
	release  string
	record   []byte
	request  string
}

// memoryState is everything the kind holds. Byte slices are never changed
// in place, so a copy shares them.
type memoryState struct {
	releases map[string][]byte
	jobs     map[string][]byte
	// revisions are every job's revisions, in the order they were made;
	// creations the jobs as their releases made them, by release.
	revisions []memoryRevision
	creations map[string][]byte
	// runs are in the order they were queued; lastSeq is the last order
	// given, which a transaction that fails gives back.
	runs    []memoryRun
	lastSeq int64
	// chain is in order of sequence.
	chain []memoryEntry
	// journal is in order of sequence, the sequence of journal[i] being
	// i+1.
	journal []journalRow
	// profiles are the rows of input profiles, by key; briefs the brief
	// histories, by subject.
	profiles map[string]string
	briefs   map[string][]byte
	// automation is the triggers and occurrences
	// (store_memory_triggers_test.go).
	automation memoryAutomation
	// schema is the last schema raised, or none.
	schema string
}

func (m memoryState) clone() memoryState {
	return memoryState{releases: maps.Clone(m.releases), jobs: maps.Clone(m.jobs), revisions: slices.Clone(m.revisions), creations: maps.Clone(m.creations), runs: slices.Clone(m.runs), lastSeq: m.lastSeq, chain: slices.Clone(m.chain), journal: slices.Clone(m.journal),
		profiles: maps.Clone(m.profiles), briefs: maps.Clone(m.briefs), automation: m.automation.clone(), schema: m.schema}
}

// memoryHeld is what every view of one in-memory store shares: its state,
// under its lock, and its seams.
type memoryHeld struct {
	mu    sync.RWMutex
	state memoryState
	// failCommit, when set, is the cause every commit fails with: the
	// kind's durability seam.
	failCommit error
	// ahead is how far the store's clock runs ahead of the process's: the
	// clock a claim's lease is held by, as the second kind's is the
	// database's now(), which no Runner's clock is compared with (D3).
	ahead time.Duration
}

// storeNow is the store's clock.
func (m *memoryHeld) storeNow() time.Time { return time.Now().Add(m.ahead) }

// passTime moves the store's clock d ahead, and not the process's.
func (m *memoryStore) passTime(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ahead += d
}

// memoryStore is the in-memory kind: a view of a store, held to a scope
// (heldTo), every scope's as it is opened. It holds the row policy of
// docs/design/store-postgresql.md, section 6, so that the conformance suite
// tests the policy on a kind that holds one: each operation reads and writes
// the records its scope reaches, and the rules held for the installation
// (D9) read every scope's.
type memoryStore struct {
	*memoryHeld
	scope storeScope
}

// newMemoryStore is a new store, its journal begun, as a new sqlite store's
// is at opening.
func newMemoryStore() *memoryStore {
	began := journalRowOf(change{kind: "journal.began", by: runnerActor, detail: EventDetail{Held: &EventHeld{}}})
	return &memoryStore{memoryHeld: &memoryHeld{state: memoryState{releases: map[string][]byte{}, jobs: map[string][]byte{}, creations: map[string][]byte{}, profiles: map[string]string{}, briefs: map[string][]byte{}, automation: newMemoryAutomation(), journal: []journalRow{began}}}, scope: everyScope}
}

// heldTo is a view of the same store, held to sc.
func (m *memoryStore) heldTo(sc storeScope) runStore {
	return &memoryStore{memoryHeld: m.memoryHeld, scope: sc}
}

// errOtherScope is the cause of the in-memory kind's faultRefused for a
// record written under a scope its transaction is not held to: a row
// policy's check.
var errOtherScope = errors.New("the record carries a scope the transaction is not held to")

// sees says whether the reads' scope reaches a record, by its scope member as
// json_extract reads it: the record's own scope, never a second copy (D5).
func (r memoryReads) sees(record []byte) bool {
	if r.scope.every {
		return true
	}
	scope, _, err := memberText(record, "scope")
	return err == nil && r.scope.reaches(scope)
}

// admits refuses a record written under a scope the transaction is not held
// to.
func (r memoryReads) admits(op string, record []byte) error {
	if r.sees(record) {
		return nil
	}
	return storeFailure(faultRefused, op, errOtherScope)
}

// seesEntry is the journal's read policy (section 6): a scope's entries, and
// the none scope's entries that name no job.
func (r memoryReads) seesEntry(row journalRow) bool {
	if r.scope.every {
		return true
	}
	scope, _, err := memberText([]byte(row.detail), "scope")
	return err == nil && (scope == r.scope.id || scope == "" && row.job == "")
}

// subjectRecord is the record a brief's subject names (job:<id>, run:<id>),
// or nil: a brief carries its subject's scope.
func (r memoryReads) subjectRecord(subject string) []byte {
	kind, id, _ := strings.Cut(subject, ":")
	switch kind {
	case "job":
		return r.s.jobs[id]
	case "run":
		for _, held := range r.s.runs {
			if held.id == id {
				return held.record
			}
		}
	}
	return nil
}

// seesSubject says whether the reads' scope reaches a brief's subject. A
// subject not held carries no scope a held transaction can be shown to
// reach.
func (r memoryReads) seesSubject(subject string) bool {
	if r.scope.every {
		return true
	}
	record := r.subjectRecord(subject)
	return record != nil && r.sees(record)
}

// seedJob keeps a job and its first revision, without the record of its
// creation, as the sqlite kind's seedJob does.
func (m *memoryStore) seedJob(j Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.jobs[j.ID] = encode(j)
	m.state.revisions = append(m.state.revisions, memoryRevision{job: j.ID, revision: j.Revision, release: j.ReleaseID, record: encode(j)})
}

// failCommits makes every later commit fail with err, or succeed again for
// nil.
func (m *memoryStore) failCommits(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCommit = err
}

// spoilRun replaces a run's record with bytes that do not read as one.
func (m *memoryStore) spoilRun(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.state.runs {
		if m.state.runs[i].id == id {
			m.state.runs[i].record = []byte("{")
		}
	}
}

func (m *memoryStore) update(fn func(runTx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.state.clone()
	if err := fn(&memoryTx{memoryReads: memoryReads{&next, m.scope}, now: m.storeNow()}); err != nil {
		return err
	}
	if m.failCommit != nil {
		return storeFailure(faultDurability, "commit", m.failCommit)
	}
	m.state = next
	return nil
}

func (m *memoryStore) close() error { return nil }

func (m *memoryStore) reads() memoryReads { return memoryReads{&m.state, m.scope} }

func (m *memoryStore) release(id string) (Release, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().release(id)
}

func (m *memoryStore) job(id string) (Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().job(id)
}

func (m *memoryStore) requestedRun(jobID, caller, key string) (heldRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().requestedRun(jobID, caller, key)
}

func (m *memoryStore) entrySequence(run string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().entrySequence(run)
}

func (m *memoryStore) run(id string) (Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().run(id)
}

func (r memoryReads) run(id string) (Run, error) {
	for _, held := range r.s.runs {
		if held.id == id && r.sees(held.record) {
			return decodeHeld("run", held.record)
		}
	}
	return Run{}, storeFailure(faultAbsent, "run", errNotHeld)
}

func (m *memoryStore) oldestQueued() (Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.state.runs {
		if r.state == "queued" && m.reads().sees(r.record) {
			return decodeHeld("queue", r.record)
		}
	}
	return Run{}, storeFailure(faultAbsent, "queue", errNotHeld)
}

func (m *memoryStore) entryLine(run string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.state.chain {
		if e.run == run {
			return e.line, nil
		}
	}
	return nil, storeFailure(faultAbsent, "chain", errNotHeld)
}

// chainPage reads as SQLite's LIMIT does: a negative limit reads every
// entry, and a limit of zero reads none.
func (m *memoryStore) chainPage(after int64, limit int) ([][]byte, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var lines [][]byte
	last := after
	for _, e := range m.state.chain {
		if limit >= 0 && len(lines) == limit {
			break
		}
		if e.sequence > after {
			lines = append(lines, e.line)
			last = e.sequence
		}
	}
	return lines, last, nil
}

func decodeHeld(op string, record []byte) (Run, error) {
	r, err := decodeRun(record)
	if err != nil {
		return r, storeFailure(faultUnreadable, op, err)
	}
	return r, nil
}

// memoryReads are the reads over one state, the kind's, under its read lock,
// or a transaction's copy, held to scope.
type memoryReads struct {
	s     *memoryState
	scope storeScope
}

func (r memoryReads) release(id string) (Release, error) {
	var rel Release
	b, ok := r.s.releases[id]
	if !ok {
		return rel, storeFailure(faultAbsent, "release", errNotHeld)
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return rel, storeFailure(faultUnreadable, "release", err)
	}
	if e := validateStoredRelease(rel); e != nil {
		return rel, storeFailure(faultUnreadable, "release", e)
	}
	return rel, nil
}

func (r memoryReads) job(id string) (Job, error) {
	var j Job
	b, ok := r.s.jobs[id]
	if !ok || !r.sees(b) {
		return j, storeFailure(faultAbsent, "job", errNotHeld)
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, storeFailure(faultUnreadable, "job", err)
	}
	return j, nil
}

func (r memoryReads) requestedRun(jobID, caller, key string) (heldRequest, error) {
	for _, held := range r.s.runs {
		if held.job == jobID && held.caller == caller && held.idem == key && r.sees(held.record) {
			return heldRequest{id: held.id, digest: held.digest, record: held.record}, nil
		}
	}
	return heldRequest{}, storeFailure(faultAbsent, "run", errNotHeld)
}

func (r memoryReads) entrySequence(run string) (int64, error) {
	for _, e := range r.s.chain {
		if e.run == run {
			return e.sequence, nil
		}
	}
	return 0, storeFailure(faultAbsent, "chain", errNotHeld)
}

// memoryTx is one transaction of the in-memory kind, over its copy.
// memoryTx is a transaction: its reads, and the store's clock as it read at
// the transaction's start, as the database's now() reads in each of its
// statements.
type memoryTx struct {
	memoryReads
	now time.Time
}

// activeRuns counts every scope's runs: the queue's limit is the
// installation's (D9).
func (t *memoryTx) activeRuns() (int, error) {
	count := 0
	for _, r := range t.s.runs {
		if r.state == "queued" || r.state == "running" {
			count++
		}
	}
	return count, nil
}

func (t *memoryTx) createRun(r Run, caller, key, requestDigest string) error {
	if e := validateStoredRun(r); e != nil {
		return e
	}
	if err := t.admits("run", []byte(runText(r))); err != nil {
		return err
	}
	for _, held := range t.s.runs {
		if held.id == r.ID || held.job == r.JobID && held.caller == caller && held.idem == key {
			return storeFailure(faultTaken, "run", errors.New("a run is held under that id or request key"))
		}
	}
	t.s.lastSeq++
	t.s.runs = append(t.s.runs, memoryRun{seq: t.s.lastSeq, id: r.ID, job: r.JobID, caller: caller, idem: key, digest: requestDigest, state: r.State, record: []byte(runText(r))})
	return nil
}

func (t *memoryTx) putRun(r Run, expected runExpected) (string, error) {
	if e := validateStoredRun(r); e != nil {
		return "", e
	}
	for i := range t.s.runs {
		held := &t.s.runs[i]
		if held.id != r.ID || !t.sees(held.record) {
			continue
		}
		if err := t.admits("run", []byte(runText(r))); err != nil {
			return held.state, err
		}
		from := held.state
		if expected.state != anyState && from != expected.state {
			return from, storeFailure(faultConflict, "run", errors.New("the run is held in another state"))
		}
		if err := t.holdsClaim(*held, expected.claim); err != nil {
			return from, err
		}
		switch r.claim {
		case runClaim{}:
			held.claim, held.leaseUntil = runClaim{}, time.Time{}
		case held.claim:
		default:
			held.claim, held.leaseUntil = r.claim, t.now.Add(claimLease)
		}
		held.state, held.record = r.State, []byte(runText(r))
		return from, nil
	}
	return "", storeFailure(faultAbsent, "run", errNotHeld)
}

// holdsClaim says whether held is held under claim, its lease not run out by
// the store's clock, as the second kind's statement says it: claim_id and
// claim_attempt equal, and lease_until > now(). No claim expected holds.
func (t *memoryTx) holdsClaim(held memoryRun, claim runClaim) error {
	if claim == (runClaim{}) {
		return nil
	}
	if held.claim != claim {
		return storeFailure(faultConflict, "run", errors.New("the run is held under another claim"))
	}
	if !t.now.Before(held.leaseUntil) {
		return storeFailure(faultConflict, "run", errors.New("the run's claim ran out"))
	}
	return nil
}

func (t *memoryTx) renewClaim(run string, claim runClaim) error {
	for i := range t.s.runs {
		held := &t.s.runs[i]
		if held.id != run || !t.sees(held.record) {
			continue
		}
		if held.state != "running" || claim == (runClaim{}) {
			return storeFailure(faultConflict, "run", errors.New("the run is not held running under a claim"))
		}
		if err := t.holdsClaim(*held, claim); err != nil {
			return err
		}
		held.leaseUntil = t.now.Add(claimLease)
		return nil
	}
	return storeFailure(faultConflict, "run", errNotHeld)
}

// raiseSchema raises as the sqlite kind does: a new store is at storeSchema
// (""), the scoped schema is raised only from it, and the retry schema from
// it or the scoped schema.
func (t *memoryTx) raiseSchema(schema string) error {
	switch {
	case schema == scopedStoreSchema && t.s.schema == "", schema == retryStoreSchema && (t.s.schema == "" || t.s.schema == scopedStoreSchema):
		t.s.schema = schema
	case schema != scopedStoreSchema && schema != retryStoreSchema:
		return storeFailure(faultRefused, "schema", errors.New("the schema is not raised through the contract"))
	}
	return nil
}

// heldSchema is the schema the kind holds, as the sqlite kind's metadata
// names it.
func (m *memoryStore) heldSchema() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.state.schema == "" {
		return storeSchema
	}
	return m.state.schema
}

func (t *memoryTx) runningRuns() ([]Run, error) {
	var runs []Run
	for _, held := range t.s.runs {
		if held.state != "running" || !t.sees(held.record) {
			continue
		}
		r, err := decodeHeld("queue", held.record)
		runs = append(runs, r)
		if err != nil {
			return runs, err
		}
	}
	return runs, nil
}

func (t *memoryTx) lastEntry() (int64, []byte, error) {
	if len(t.s.chain) == 0 {
		return 0, nil, storeFailure(faultAbsent, "chain", errNotHeld)
	}
	last := t.s.chain[len(t.s.chain)-1]
	return last.sequence, last.line, nil
}

func (t *memoryTx) createEntry(sequence int64, run string, line []byte) error {
	if sequence < 1 {
		return storeFailure(faultRefused, "chain", errors.New("an entry's sequence is 1 or more"))
	}
	at := len(t.s.chain)
	for i, e := range t.s.chain {
		if e.sequence == sequence || e.run == run {
			return storeFailure(faultTaken, "chain", errors.New("the chain has an entry at that sequence or for that run"))
		}
		if e.sequence > sequence && at == len(t.s.chain) {
			at = i
		}
	}
	t.s.chain = slices.Insert(t.s.chain, at, memoryEntry{sequence: sequence, run: run, line: slices.Clone(line)})
	return nil
}

func (t *memoryTx) createRelease(r Release) error {
	if e := validateStoredRelease(r); e != nil {
		return e
	}
	if _, ok := t.s.releases[r.ID]; ok {
		return storeFailure(faultTaken, "release", errors.New("a release is held under that id"))
	}
	t.s.releases[r.ID] = encode(r)
	return nil
}

// appendJournal writes an entry only under the scope its transaction is held
// to: the read policy's second branch, the none scope's entries naming no
// job, admits no write.
func (t *memoryTx) appendJournal(c change) (int64, error) {
	row := journalRowOf(c)
	if !t.scope.every {
		if scope, _, err := memberText([]byte(row.detail), "scope"); err != nil || scope != t.scope.id {
			return 0, storeFailure(faultRefused, "journal", errOtherScope)
		}
	}
	t.s.journal = append(t.s.journal, row)
	return int64(len(t.s.journal)), nil
}

func (t *memoryTx) lastRefusal(key string) (string, error) {
	for i := len(t.s.journal) - 1; i >= 0; i-- {
		if t.s.journal[i].refusal == key && t.seesEntry(t.s.journal[i]) {
			return t.s.journal[i].at, nil
		}
	}
	return "", storeFailure(faultAbsent, "journal", errNotHeld)
}

func (t *memoryTx) putProfileRow(key, row string) error {
	if !profileRowKeys[key] {
		return storeFailure(faultRefused, "profiles", errors.New("no profile row is held under that key"))
	}
	t.s.profiles[key] = row
	return nil
}

func (t *memoryTx) putBrief(subject string, record []byte) error {
	if !t.seesSubject(subject) {
		return storeFailure(faultRefused, "brief", errOtherScope)
	}
	t.s.briefs[subject] = slices.Clone(record)
	return nil
}

func (r memoryReads) profileRow(key string) (string, error) {
	if !profileRowKeys[key] {
		return "", storeFailure(faultRefused, "profiles", errors.New("no profile row is held under that key"))
	}
	row, ok := r.s.profiles[key]
	if !ok {
		return "", storeFailure(faultAbsent, "profiles", errNotHeld)
	}
	return row, nil
}

func (r memoryReads) brief(subject string) ([]byte, error) {
	b, ok := r.s.briefs[subject]
	if !ok || !r.seesSubject(subject) {
		return nil, storeFailure(faultAbsent, "brief", errNotHeld)
	}
	return b, nil
}

// recordScope reads the record's scope member as SQLite's json_extract
// does: a text member is its value, and none is "".
func (r memoryReads) recordScope(records, id string) (string, error) {
	if !scopeCarriers[records] {
		return "", storeFailure(faultRefused, "scope", errors.New("no record of that kind carries a scope"))
	}
	var record []byte
	switch records {
	case "jobs":
		record = r.s.jobs[id]
	case "runs":
		for _, held := range r.s.runs {
			if held.id == id {
				record = held.record
			}
		}
	case "triggers":
		for _, held := range r.s.automation.triggers {
			if held.id == id {
				record = held.record
			}
		}
	case "occurrences":
		for _, held := range r.s.automation.occurrences {
			if held.id == id {
				record = held.record
			}
		}
	}
	if record == nil || !r.sees(record) {
		return "", storeFailure(faultAbsent, "scope", errNotHeld)
	}
	scope, _, err := memberText(record, "scope")
	return scope, err
}

// memberText is a record's member as text, and whether the record has it as
// text: json_extract's value, which a condition on it compares as text.
func memberText(record []byte, name string) (string, bool, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(record, &members); err != nil {
		return "", false, storeFailure(faultUnreadable, "record", err)
	}
	var text string
	raw, ok := members[name]
	if !ok || json.Unmarshal(raw, &text) != nil {
		return "", false, nil
	}
	return text, true, nil
}

func (m *memoryStore) recordScope(records, id string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().recordScope(records, id)
}

func (m *memoryStore) profileRow(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().profileRow(key)
}

func (m *memoryStore) brief(subject string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().brief(subject)
}

func (m *memoryStore) journalBegan() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.state.journal {
		if e.kind == "journal.began" && m.reads().seesEntry(e) {
			return e.at, nil
		}
	}
	return "", storeFailure(faultAbsent, "journal", errNotHeld)
}

// journalPage keeps what sqliteJournalCondition selects: the filter's job by
// the releases of its revisions, a scope and the results by the detail's
// members, as json_extract reads them.
func (m *memoryStore) journalPage(f journalFilter, g journalRange) ([]Event, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	releases := map[string]bool{}
	for _, rev := range m.state.revisions {
		if rev.job == f.job {
			releases[rev.release] = true
		}
	}
	var items []Event
	read := func(i int) (bool, error) {
		row, sequence := m.state.journal[i], int64(i+1)
		if !m.reads().seesEntry(row) || !memoryKeeps(f, row, releases) {
			return false, nil
		}
		if len(items) == g.limit {
			return true, nil
		}
		e, err := row.event(sequence)
		if err != nil {
			return false, storeFailure(faultUnreadable, "journal", err)
		}
		items = append(items, e)
		return false, nil
	}
	if !g.backward {
		for i := int(max(g.after, 0)); i < len(m.state.journal); i++ {
			if more, err := read(i); more || err != nil {
				return items, more, err
			}
		}
		return items, false, nil
	}
	from := len(m.state.journal) - 1
	if !g.tail {
		from = int(min(g.before-1, int64(len(m.state.journal)))) - 1
	}
	for i := from; i >= 0; i-- {
		if more, err := read(i); more || err != nil {
			return items, more, err
		}
	}
	return items, false, nil
}

// memoryKeeps says whether f keeps row.
func memoryKeeps(f journalFilter, row journalRow, releases map[string]bool) bool {
	if f.job != "" {
		mine := row.job == f.job || row.job == "" && releases[row.release]
		own := f.runnerOwn && (row.kind == "journal.began" || row.kind == "runner.started" || row.kind == "runner.stopped")
		if !mine && !own {
			return false
		}
	}
	detail := []byte(row.detail)
	if f.scope != "" {
		if scope, held, err := memberText(detail, "scope"); err != nil || !held || scope != f.scope {
			return false
		}
	}
	if len(f.results.Outcomes) > 0 {
		var d struct {
			Disposition *EventDisposition `json:"disposition"`
		}
		if json.Unmarshal(detail, &d) != nil || d.Disposition == nil || d.Disposition.Kind != "outcome" || !slices.Contains(f.results.Outcomes, d.Disposition.OutcomeID) {
			return false
		}
	}
	if len(f.results.Classes) > 0 {
		if class, held, err := memberText(detail, "errorClass"); err != nil || !held || !slices.Contains(f.results.Classes, class) {
			return false
		}
	}
	return len(f.results.Kinds) == 0 || slices.Contains(f.results.Kinds, row.kind)
}
