package runner

// The in-memory kind's triggers and occurrences, held as the sqlite kind
// holds them (store_sqlite_triggers.go): records as their encoded bytes, in
// the order they were made, under the same unique keys.

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
)

type memoryTrigger struct {
	seq                                int64
	id, job                            string
	record                             []byte
	hash, observed, pending, pendingAt string
	// keyRevision is never changed in place, so a copy shares it.
	keyRevision *int
}

type memoryOccurrence struct {
	seq                                        int64
	id, job, trigger, identity, payload, state string
	record                                     []byte
}

// memorySnapshot is a trigger's revision, the key of its snapshot.
type memorySnapshot struct {
	trigger  string
	revision int
}

// memoryAutomation is the kind's triggers and occurrences.
type memoryAutomation struct {
	triggers  []memoryTrigger
	snapshots map[memorySnapshot][]byte
	// occurrences are in the order they were admitted; keys bind an
	// occurrence to the digest of the delivery key that created it.
	occurrences []memoryOccurrence
	keys        map[string]string
	// lastTrigger and lastOccurrence are the last orders given, which a
	// transaction that fails gives back.
	lastTrigger, lastOccurrence int64
}

func (a memoryAutomation) clone() memoryAutomation {
	a.triggers = slices.Clone(a.triggers)
	a.snapshots = maps.Clone(a.snapshots)
	a.occurrences = slices.Clone(a.occurrences)
	a.keys = maps.Clone(a.keys)
	return a
}

// newMemoryAutomation is the state of a new store: none of either.
func newMemoryAutomation() memoryAutomation {
	return memoryAutomation{snapshots: map[memorySnapshot][]byte{}, keys: map[string]string{}}
}

// errTaken is the cause of the in-memory kind's faultTaken here.
var errTaken = errors.New("a record is held under that key")

func (h memoryTrigger) decode() (Trigger, error) {
	var t Trigger
	if err := json.Unmarshal(h.record, &t); err != nil {
		return t, storeFailure(faultUnreadable, "trigger", err)
	}
	t.HasKey = h.hash != ""
	t.observed, t.pending, t.pendingAt = h.observed, h.pending, h.pendingAt
	if h.keyRevision != nil {
		v := *h.keyRevision
		t.keyRevision = &v
	}
	return t, nil
}

func (h memoryOccurrence) decode() (Occurrence, error) {
	var o Occurrence
	if err := json.Unmarshal(h.record, &o); err != nil {
		return o, storeFailure(faultUnreadable, "occurrence", err)
	}
	return o, nil
}

// cloudKey is the unique key the sqlite kind indexes a cloud trigger's
// binding under (triggers_cloud_binding), and whether t has one.
func cloudKey(t Trigger) (string, bool) {
	if t.Config.Kind != "cloud" || t.Config.Cloud == nil {
		return "", false
	}
	return t.Config.Cloud.Connection + "\x00" + t.Config.Cloud.Job, true
}

// boundElsewhere says whether a trigger other than t holds t's cloud binding.
func (a *memoryAutomation) boundElsewhere(t Trigger) (bool, error) {
	key, ok := cloudKey(t)
	if !ok {
		return false, nil
	}
	for _, h := range a.triggers {
		if h.id == t.ID {
			continue
		}
		held, err := h.decode()
		if err != nil {
			return false, err
		}
		if other, ok := cloudKey(held); ok && other == key {
			return true, nil
		}
	}
	return false, nil
}

// bindings counts, as the sqlite kind's statement does, the triggers r's
// scope reaches other than except whose record names a cloud binding to
// connection and job, whatever their kind.
func (r memoryReads) bindings(except, connection, job string) (int, []string, error) {
	count := 0
	var ids []string
	for _, h := range r.s.automation.triggers {
		if !r.sees(h.record) {
			continue
		}
		t, err := h.decode()
		if err != nil {
			return 0, nil, err
		}
		if h.id != except && t.Config.Cloud != nil && t.Config.Cloud.Connection == connection && t.Config.Cloud.Job == job {
			count++
			ids = append(ids, h.id)
		}
	}
	return count, ids, nil
}

func (r memoryReads) trigger(id string) (Trigger, string, error) {
	for _, h := range r.s.automation.triggers {
		if h.id == id && r.sees(h.record) {
			t, err := h.decode()
			return t, h.hash, err
		}
	}
	return Trigger{}, "", storeFailure(faultAbsent, "trigger", errNotHeld)
}

// triggerCount counts every scope's triggers: the bound is the
// installation's (D9).
func (r memoryReads) triggerCount() (int, error) { return len(r.s.automation.triggers), nil }

// cloudBindings counts the bindings r's scope reaches; the binding's key is
// the installation's, met by the write (boundElsewhere).
func (r memoryReads) cloudBindings(except, connection, job string) (int, error) {
	count, _, err := r.bindings(except, connection, job)
	return count, err
}

func (r memoryReads) occurrence(id string) (Occurrence, error) {
	for _, h := range r.s.automation.occurrences {
		if h.id == id && r.sees(h.record) {
			return h.decode()
		}
	}
	return Occurrence{}, storeFailure(faultAbsent, "occurrence", errNotHeld)
}

func (r memoryReads) identifiedOccurrence(trigger, identity string) (Occurrence, string, error) {
	for _, h := range r.s.automation.occurrences {
		if h.trigger == trigger && h.identity == identity && r.sees(h.record) {
			o, err := h.decode()
			return o, h.payload, err
		}
	}
	return Occurrence{}, "", storeFailure(faultAbsent, "occurrence", errNotHeld)
}

func (m *memoryStore) trigger(id string) (Trigger, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().trigger(id)
}

func (m *memoryStore) triggerCount() (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().triggerCount()
}

func (m *memoryStore) cloudBindings(except, connection, job string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().cloudBindings(except, connection, job)
}

func (m *memoryStore) occurrence(id string) (Occurrence, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().occurrence(id)
}

func (m *memoryStore) identifiedOccurrence(trigger, identity string) (Occurrence, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reads().identifiedOccurrence(trigger, identity)
}

func (m *memoryStore) enabledTriggers() ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := []string{}
	for _, h := range m.state.automation.triggers {
		if !m.reads().sees(h.record) {
			continue
		}
		t, err := h.decode()
		if err != nil {
			return nil, err
		}
		if !t.Paused {
			ids = append(ids, h.id)
		}
	}
	return ids, nil
}

// pendingInput says whether an occurrence's record holds input left to
// prepare, as json_extract(record,'$.pendingInput') IS NOT NULL does.
func pendingInput(record []byte) (bool, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(record, &members); err != nil {
		return false, storeFailure(faultUnreadable, "occurrence", err)
	}
	raw, ok := members["pendingInput"]
	return ok && string(raw) != "null", nil
}

func (m *memoryStore) readyOccurrence() (Occurrence, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, h := range m.state.automation.occurrences {
		if h.state != "accepted" || !m.reads().sees(h.record) {
			continue
		}
		if pending, err := pendingInput(h.record); err != nil || pending {
			if err != nil {
				return Occurrence{}, err
			}
			continue
		}
		return h.decode()
	}
	return Occurrence{}, storeFailure(faultAbsent, "occurrence", errNotHeld)
}

func (m *memoryStore) preparableOccurrences(each func(Occurrence) (bool, error)) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, h := range m.state.automation.occurrences {
		if !m.reads().sees(h.record) {
			continue
		}
		keep := h.state == "waiting"
		if h.state == "accepted" {
			pending, err := pendingInput(h.record)
			if err != nil {
				return err
			}
			keep = pending
		}
		if !keep {
			continue
		}
		o, err := h.decode()
		if err != nil {
			return err
		}
		if done, err := each(o); err != nil || done {
			return err
		}
	}
	return nil
}

func (m *memoryStore) cloudBoundTrigger(connection, job string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ids, err := m.reads().bindings("", connection, job)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", storeFailure(faultAbsent, "trigger", errNotHeld)
	}
	return ids[0], nil
}

func (m *memoryStore) jobTriggers(job string) ([]Trigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := []Trigger{}
	for _, h := range m.state.automation.triggers {
		if h.job != job || !m.reads().sees(h.record) {
			continue
		}
		var t Trigger
		if err := json.Unmarshal(h.record, &t); err != nil {
			return nil, storeFailure(faultUnreadable, "trigger", err)
		}
		t.HasKey = h.hash != ""
		items = append(items, t)
	}
	return items, nil
}

// occurrencePage reads as the sqlite kind's statement does: a preparation is
// held when the record's member is there and not null.
func (m *memoryStore) occurrencePage(job string, before int64, preparations bool, limit int) ([]Occurrence, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := []Occurrence{}
	var last int64
	held := m.state.automation.occurrences
	for i := len(held) - 1; i >= 0; i-- {
		h := held[i]
		if h.job != job || h.seq >= before || !m.reads().sees(h.record) {
			continue
		}
		if preparations {
			var members map[string]json.RawMessage
			if err := json.Unmarshal(h.record, &members); err != nil {
				return nil, 0, storeFailure(faultUnreadable, "occurrence", err)
			}
			if raw, ok := members["preparation"]; !ok || string(raw) == "null" || h.state == "submitted" {
				continue
			}
		}
		if len(items) == limit {
			return items, last, nil
		}
		o, err := h.decode()
		if err != nil {
			return nil, 0, err
		}
		items = append(items, o)
		last = h.seq
	}
	return items, 0, nil
}

func (m *memoryStore) keyedOccurrence(id, trigger, hash string) (Occurrence, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.state.automation
	bound, ok := a.keys[id]
	if !ok || bound != hash {
		return Occurrence{}, storeFailure(faultAbsent, "occurrence", errNotHeld)
	}
	for _, t := range a.triggers {
		if t.id == trigger && t.hash == hash && m.reads().sees(t.record) {
			for _, h := range a.occurrences {
				if h.id == id && h.trigger == trigger && m.reads().sees(h.record) {
					return h.decode()
				}
			}
		}
	}
	return Occurrence{}, storeFailure(faultAbsent, "occurrence", errNotHeld)
}

func (t *memoryTx) createTrigger(tr Trigger) error {
	a := &t.s.automation
	if err := t.admits("trigger", encode(tr)); err != nil {
		return err
	}
	for _, h := range a.triggers {
		if h.id == tr.ID {
			return storeFailure(faultTaken, "trigger", errTaken)
		}
	}
	if taken, err := a.boundElsewhere(tr); err != nil || taken {
		if err != nil {
			return err
		}
		return takenOn("trigger", keyCloudBinding, errTaken)
	}
	a.lastTrigger++
	a.triggers = append(a.triggers, memoryTrigger{seq: a.lastTrigger, id: tr.ID, job: tr.JobID, record: encode(tr)})
	return nil
}

func (t *memoryTx) putTrigger(tr Trigger, hash string, history bool) error {
	a := &t.s.automation
	i := slices.IndexFunc(a.triggers, func(h memoryTrigger) bool { return h.id == tr.ID && t.sees(h.record) })
	if i < 0 {
		return storeFailure(faultAbsent, "trigger", errNotHeld)
	}
	if err := t.admits("trigger", encode(tr)); err != nil {
		return err
	}
	if taken, err := a.boundElsewhere(tr); err != nil || taken {
		if err != nil {
			return err
		}
		return takenOn("trigger", keyCloudBinding, errTaken)
	}
	snapshot := memorySnapshot{tr.ID, tr.Revision}
	if _, held := a.snapshots[snapshot]; history && held {
		return storeFailure(faultTaken, "trigger", errTaken)
	}
	var keyRevision *int
	if tr.keyRevision != nil {
		v := *tr.keyRevision
		keyRevision = &v
	}
	h := &a.triggers[i]
	h.record, h.hash, h.observed, h.pending, h.pendingAt, h.keyRevision = encode(tr), hash, tr.observed, tr.pending, tr.pendingAt, keyRevision
	if history {
		a.snapshots[snapshot] = encode(tr)
	}
	return nil
}

func (t *memoryTx) createOccurrence(o Occurrence, identity, payload string) error {
	a := &t.s.automation
	if err := t.admits("occurrence", encode(o)); err != nil {
		return err
	}
	for _, h := range a.occurrences {
		if h.id == o.ID || h.trigger == o.TriggerID && h.identity == identity {
			return storeFailure(faultTaken, "occurrence", errTaken)
		}
	}
	a.lastOccurrence++
	a.occurrences = append(a.occurrences, memoryOccurrence{seq: a.lastOccurrence, id: o.ID, job: o.JobID, trigger: o.TriggerID, identity: identity, payload: payload, state: o.State, record: encode(o)})
	return nil
}

func (t *memoryTx) putOccurrence(o Occurrence, expected string) (string, error) {
	a := &t.s.automation
	for i := range a.occurrences {
		h := &a.occurrences[i]
		if h.id != o.ID || !t.sees(h.record) {
			continue
		}
		if err := t.admits("occurrence", encode(o)); err != nil {
			return h.state, err
		}
		from := h.state
		if expected != anyState && from != expected {
			return from, storeFailure(faultConflict, "occurrence", errors.New("the occurrence is held in another state"))
		}
		h.state, h.record = o.State, encode(o)
		return from, nil
	}
	return "", storeFailure(faultAbsent, "occurrence", errNotHeld)
}

// createOccurrenceKey binds an occurrence its transaction's scope reaches: a
// key carries its occurrence's scope (D5).
func (t *memoryTx) createOccurrenceKey(occurrence, hash string) error {
	if _, err := t.occurrence(occurrence); err != nil && !t.scope.every {
		return storeFailure(faultRefused, "occurrence", errOtherScope)
	}
	if _, held := t.s.automation.keys[occurrence]; held {
		return storeFailure(faultTaken, "occurrence", errTaken)
	}
	t.s.automation.keys[occurrence] = hash
	return nil
}

// pendingOccurrences counts every scope's occurrences: the automatic queue's
// limit is the installation's (D9).
func (t *memoryTx) pendingOccurrences() (int, error) {
	count := 0
	for _, h := range t.s.automation.occurrences {
		if h.state == "accepted" || h.state == "preparing" || h.state == "waiting" {
			count++
		}
	}
	return count, nil
}

func (t *memoryTx) jobActivity(job string) (int, error) {
	count := 0
	for _, r := range t.s.runs {
		if r.job == job && (r.state == "queued" || r.state == "running") && t.sees(r.record) {
			count++
		}
	}
	for _, h := range t.s.automation.occurrences {
		if h.job == job && slices.Contains([]string{"accepted", "preparing", "waiting", "needs-attention"}, h.state) && t.sees(h.record) {
			count++
		}
	}
	return count, nil
}

func (t *memoryTx) preparingOccurrences() ([]Occurrence, error) {
	var occurrences []Occurrence
	for _, h := range t.s.automation.occurrences {
		if h.state != "preparing" || !t.sees(h.record) {
			continue
		}
		o, err := h.decode()
		if err != nil {
			return occurrences, err
		}
		occurrences = append(occurrences, o)
	}
	return occurrences, nil
}

func (t *memoryTx) lastPreparing(occurrence string) (string, error) {
	for i := len(t.s.journal) - 1; i >= 0; i-- {
		row := t.s.journal[i]
		if row.kind != "occurrence.preparing" || !t.seesEntry(row) {
			continue
		}
		concerned, _, err := memberText([]byte(row.concerns), "occurrence")
		if err != nil {
			return "", err
		}
		if concerned == occurrence {
			return row.at, nil
		}
	}
	return "", storeFailure(faultAbsent, "journal", errNotHeld)
}
