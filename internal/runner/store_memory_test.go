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
	"sync"
)

// errNotHeld is the cause of the in-memory kind's faultAbsent.
var errNotHeld = errors.New("nothing is held under that id or key")

type memoryRun struct {
	seq                                  int64
	id, job, caller, idem, digest, state string
	record                               []byte
}

type memoryEntry struct {
	sequence int64
	run      string
	line     []byte
}

// memoryState is everything the kind holds. Byte slices are never changed
// in place, so a copy shares them.
type memoryState struct {
	releases map[string][]byte
	jobs     map[string][]byte
	// runs are in the order they were queued; lastSeq is the last order
	// given, which a transaction that fails gives back.
	runs    []memoryRun
	lastSeq int64
	// chain is in order of sequence.
	chain   []memoryEntry
	journal []change
}

func (m memoryState) clone() memoryState {
	return memoryState{releases: maps.Clone(m.releases), jobs: maps.Clone(m.jobs), runs: slices.Clone(m.runs), lastSeq: m.lastSeq, chain: slices.Clone(m.chain), journal: slices.Clone(m.journal)}
}

// memoryStore is the in-memory kind.
type memoryStore struct {
	mu    sync.RWMutex
	state memoryState
	// failCommit, when set, is the cause every commit fails with: the
	// kind's durability seam.
	failCommit error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{state: memoryState{releases: map[string][]byte{}, jobs: map[string][]byte{}}}
}

// seedJob keeps a job, as one made outside the contract.
func (m *memoryStore) seedJob(j Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.jobs[j.ID] = encode(j)
}

// failCommits makes every later commit fail with err, or succeed again for
// nil.
func (m *memoryStore) failCommits(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCommit = err
}

// journalKinds are the kinds of the journal's entries, in order.
func (m *memoryStore) journalKinds() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var kinds []string
	for _, c := range m.state.journal {
		kinds = append(kinds, c.kind)
	}
	return kinds
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
	if err := fn(&memoryTx{memoryReads{&next}}); err != nil {
		return err
	}
	if m.failCommit != nil {
		return storeFailure(faultDurability, "commit", m.failCommit)
	}
	m.state = next
	return nil
}

func (m *memoryStore) close() error { return nil }

func (m *memoryStore) reads() memoryReads { return memoryReads{&m.state} }

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
	for _, r := range m.state.runs {
		if r.id == id {
			return decodeHeld("run", r.record)
		}
	}
	return Run{}, storeFailure(faultAbsent, "run", errNotHeld)
}

func (m *memoryStore) oldestQueued() (Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.state.runs {
		if r.state == "queued" {
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

// memoryReads are the reads over one state: the kind's, under its read
// lock, or a transaction's copy.
type memoryReads struct{ s *memoryState }

func (r memoryReads) release(id string) (Release, error) {
	var rel Release
	b, ok := r.s.releases[id]
	if !ok {
		return rel, storeFailure(faultAbsent, "release", errNotHeld)
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return rel, storeFailure(faultUnreadable, "release", err)
	}
	return rel, nil
}

func (r memoryReads) job(id string) (Job, error) {
	var j Job
	b, ok := r.s.jobs[id]
	if !ok {
		return j, storeFailure(faultAbsent, "job", errNotHeld)
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, storeFailure(faultUnreadable, "job", err)
	}
	return j, nil
}

func (r memoryReads) requestedRun(jobID, caller, key string) (heldRequest, error) {
	for _, held := range r.s.runs {
		if held.job == jobID && held.caller == caller && held.idem == key {
			return heldRequest{digest: held.digest, record: held.record}, nil
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
type memoryTx struct{ memoryReads }

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
	for _, held := range t.s.runs {
		if held.id == r.ID || held.job == r.JobID && held.caller == caller && held.idem == key {
			return storeFailure(faultTaken, "run", errors.New("a run is held under that id or request key"))
		}
	}
	t.s.lastSeq++
	t.s.runs = append(t.s.runs, memoryRun{seq: t.s.lastSeq, id: r.ID, job: r.JobID, caller: caller, idem: key, digest: requestDigest, state: r.State, record: []byte(runText(r))})
	return nil
}

func (t *memoryTx) putRun(r Run, expected string) (string, error) {
	for i := range t.s.runs {
		held := &t.s.runs[i]
		if held.id != r.ID {
			continue
		}
		from := held.state
		if expected != anyState && from != expected {
			return from, storeFailure(faultConflict, "run", errors.New("the run is held in another state"))
		}
		held.state, held.record = r.State, []byte(runText(r))
		return from, nil
	}
	return "", storeFailure(faultAbsent, "run", errNotHeld)
}

func (t *memoryTx) runningRuns() ([]Run, error) {
	var runs []Run
	for _, held := range t.s.runs {
		if held.state != "running" {
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
	if _, ok := t.s.releases[r.ID]; ok {
		return storeFailure(faultTaken, "release", errors.New("a release is held under that id"))
	}
	t.s.releases[r.ID] = encode(r)
	return nil
}

func (t *memoryTx) appendJournal(c change) (int64, error) {
	if c.at == "" {
		c.at = now()
	}
	t.s.journal = append(t.s.journal, c)
	return int64(len(t.s.journal)), nil
}
