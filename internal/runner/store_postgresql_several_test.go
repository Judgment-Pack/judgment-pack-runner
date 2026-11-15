package runner

// Several Runners on one postgresql store (JPACK_TEST_POSTGRESQL), each with
// a state directory of its own, as on a host of its own
// (docs/design/store-postgresql.md, D17 to D20): one due time makes one
// occurrence, one ready occurrence is worked by one Runner, a Runner that
// stops has its work interrupted one lease later and not repeated, a slow
// holder's late write is refused, and a Runner's start leaves another's live
// work to it.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// schedule makes a schedule trigger of the Runners' job on sv and resumes it,
// and returns it, as resumed, and the time it falls due.
func (p *pgStoreRunners) schedule(sv *Service) (Trigger, time.Time) {
	t := p.t
	t.Helper()
	got := serve(t, sv, "POST", "/v1/jobs/"+p.job.ID+"/triggers?scope=team-a", string(encode(map[string]any{"config": scheduleTriggerConfig(time.Now())})), nil)
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var created Trigger
	must(t, json.Unmarshal(got.Body.Bytes(), &created))
	got = serve(t, sv, "POST", "/v1/triggers/"+created.ID+"/state?scope=team-a", string(encode(map[string]any{"revision": created.Revision, "paused": false, "reviewed": true})), nil)
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var resumed struct {
		Trigger Trigger `json:"trigger"`
	}
	must(t, json.Unmarshal(got.Body.Bytes(), &resumed))
	due, err := time.Parse(time.RFC3339Nano, resumed.Trigger.NextAt)
	must(t, err)
	return resumed.Trigger, due
}

// occurrences are the job's occurrences, as sv reads them.
func (p *pgStoreRunners) occurrences(sv *Service) []Occurrence {
	t := p.t
	t.Helper()
	got := serve(t, sv, "GET", "/v1/jobs/"+p.job.ID+"/occurrences?scope=team-a", "", nil)
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var page struct {
		Items []Occurrence `json:"items"`
	}
	must(t, json.Unmarshal(got.Body.Bytes(), &page))
	return page.Items
}

// admitted is the one occurrence the first Runner's tick of a new schedule
// trigger admits at its due time, ready to dispatch.
func (p *pgStoreRunners) admitted(sv *Service) (Occurrence, time.Time) {
	t := p.t
	t.Helper()
	tr, due := p.schedule(sv)
	held, hash := storedTrigger(t, sv, tr.ID)
	must(t, sv.scheduleTick(held, hash, due.Add(time.Second)))
	items := p.occurrences(sv)
	if len(items) != 1 || items[0].State != "accepted" {
		t.Fatalf("the tick admitted %+v, not one accepted occurrence", items)
	}
	return storedOccurrence(t, sv, items[0].ID), due
}

// entriesOfKind counts the entries of kind in st's journal that concern id.
func entriesOfKind(t *testing.T, st runStore, kind, id string) int {
	t.Helper()
	n := 0
	for after := int64(0); ; {
		items, more, err := st.journalPage(journalFilter{}, journalRange{after: after, limit: 50})
		must(t, err)
		for _, e := range items {
			after = e.Sequence
			if e.Kind == kind && (e.Concerns.Run == id || e.Concerns.Occurrence == id) {
				n++
			}
		}
		if !more {
			return n
		}
	}
}

// tickRace holds each of the stores it wraps, all sharing read, at its first
// read of a trigger until every one has read it, so that their ticks meet on
// one due time.
type tickRace struct {
	runStore
	read *sync.WaitGroup
	once sync.Once
}

func (q *tickRace) leasesClaims() bool { return leases(q.runStore) }

func (q *tickRace) updateWithin(d time.Duration, fn func(runTx) error) error {
	return updateWithin(q.runStore, d, fn)
}

func (q *tickRace) trigger(id string) (Trigger, string, error) {
	tr, hash, err := q.runStore.trigger(id)
	q.once.Do(func() {
		q.read.Done()
		q.read.Wait()
	})
	return tr, hash, err
}

// D17 and D18. Two Runners tick one schedule trigger at once, each having
// read it at the same version: one writes it and admits the due time's
// occurrence, the other's write meets the version moved on and makes
// nothing, and neither fails, so that both automations go on. The occurrence
// is dispatched once.
func TestPostgreSQLTwoRunnersTickingOneScheduleMakeOneOccurrence(t *testing.T) {
	p := pgRunners(t)
	runners := [2]*Service{p.open(), p.open()}
	tr, due := p.schedule(runners[0])
	before, _ := storedTrigger(t, runners[0], tr.ID)
	var read sync.WaitGroup
	read.Add(len(runners))
	for _, sv := range runners {
		sv.store = &tickRace{runStore: sv.store, read: &read}
	}
	var errs [2]error
	var wg sync.WaitGroup
	for i, sv := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = sv.automationTick(due.Add(time.Second))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || runners[i].unhealthy.Load() {
			t.Fatalf("the tick of Runner %d failed, which stops its automation: %v", i, err)
		}
	}
	items := p.occurrences(runners[1])
	if len(items) != 1 {
		t.Fatalf("one due time made %d occurrences: %+v", len(items), items)
	}
	if items[0].State != "submitted" || items[0].RunID == "" {
		t.Fatalf("the occurrence was not dispatched: %+v", items[0])
	}
	if n := entriesOfKind(t, runners[0].store, "occurrence.submitted", items[0].ID); n != 1 {
		t.Fatal("the occurrence was submitted", n, "times")
	}
	after, _ := storedTrigger(t, runners[0], tr.ID)
	if after.version != before.version+1 {
		t.Fatalf("the trigger was written %d times by one due time", after.version-before.version)
	}
	if next, err := time.Parse(time.RFC3339Nano, after.NextAt); err != nil || !next.After(due) {
		t.Fatal("the trigger's next time did not move on:", after.NextAt, err)
	}
}

// workCount counts the occurrences a Runner worked past their claim: its
// reads of an occurrence's run by its request key, which dispatchClaimed
// makes first.
type workCount struct {
	runStore
	worked atomic.Int32
}

func (q *workCount) leasesClaims() bool { return leases(q.runStore) }

func (q *workCount) updateWithin(d time.Duration, fn func(runTx) error) error {
	return updateWithin(q.runStore, d, fn)
}

func (q *workCount) requestedRun(job, caller, key string) (heldRequest, error) {
	if strings.HasPrefix(caller, "trigger:") {
		q.worked.Add(1)
	}
	return q.runStore.requestedRun(job, caller, key)
}

// D18. Two Runners dispatch one ready occurrence they both read: one claims
// and works it, the other passes it by, making nothing and failing nothing.
func TestPostgreSQLTwoRunnersWorkOneReadyOccurrenceOnce(t *testing.T) {
	p := pgRunners(t)
	runners := [2]*Service{p.open(), p.open()}
	o, due := p.admitted(runners[0])
	var counts [2]*workCount
	for i, sv := range runners {
		counts[i] = &workCount{runStore: sv.store}
		sv.store = counts[i]
	}
	var errs [2]error
	var wg sync.WaitGroup
	for i, sv := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = sv.dispatchOccurrence(o, due.Add(time.Second))
		}()
	}
	wg.Wait()
	workers := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("the dispatch of Runner %d failed, which stops its automation: %v", i, err)
		}
		if counts[i].worked.Load() > 0 {
			workers++
		}
	}
	if workers != 1 {
		t.Fatalf("%d Runners worked the one ready occurrence", workers)
	}
	held := storedOccurrence(t, runners[1], o.ID)
	if held.State != "submitted" || held.RunID == "" {
		t.Fatalf("the occurrence was not dispatched: %+v", held)
	}
	if n := entriesOfKind(t, runners[0].store, "occurrence.submitted", o.ID); n != 1 {
		t.Fatal("the occurrence was submitted", n, "times")
	}
}

// slowHolder holds a Runner at its first read of an occurrence's run by its
// request key, after the read, until resume is closed: a holder slow past
// its claim's lease.
type slowHolder struct {
	runStore
	read, resume chan struct{}
	once         sync.Once
}

func (q *slowHolder) leasesClaims() bool { return leases(q.runStore) }

func (q *slowHolder) updateWithin(d time.Duration, fn func(runTx) error) error {
	return updateWithin(q.runStore, d, fn)
}

func (q *slowHolder) requestedRun(job, caller, key string) (heldRequest, error) {
	h, err := q.runStore.requestedRun(job, caller, key)
	q.once.Do(func() {
		close(q.read)
		<-q.resume
	})
	return h, err
}

// D18. A Runner that claimed a ready occurrence is slow past its lease: the
// other Runner claims and dispatches it, and the slow holder's late write,
// which would end it expired, is refused and writes nothing; neither fails.
func TestPostgreSQLASlowHolderLosesItsOccurrenceAndItsLateWriteIsRefused(t *testing.T) {
	p := pgRunners(t)
	slow, other := p.open(), p.open()
	o, _ := p.admitted(slow)
	holder := &slowHolder{runStore: slow.store, read: make(chan struct{}), resume: make(chan struct{})}
	slow.store = holder
	done := make(chan error, 1)
	go func() { done <- slow.dispatchOccurrence(o, time.Now().Add(2*time.Hour)) }()
	select {
	case <-holder.read:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow holder did not claim the occurrence")
	}
	pgExec(t, p.suite, "UPDATE occurrences SET lease_until=now()-interval '1 second' WHERE id=$1", o.ID)
	if err := other.dispatchOccurrence(o, time.Now()); err != nil {
		t.Fatal("the other Runner's dispatch of the lapsed occurrence:", err)
	}
	dispatched := storedOccurrence(t, other, o.ID)
	if dispatched.State != "submitted" || dispatched.RunID == "" {
		t.Fatalf("the other Runner did not dispatch the lapsed occurrence: %+v", dispatched)
	}
	close(holder.resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("the slow holder's refused write stopped its automation:", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the slow holder did not finish")
	}
	held := storedOccurrence(t, other, o.ID)
	if held.State != "submitted" || held.RunID != dispatched.RunID {
		t.Fatalf("the slow holder's late write was not refused: %+v", held)
	}
	if n := entriesOfKind(t, other.store, "occurrence.expired", o.ID); n != 0 {
		t.Fatal("the slow holder's late expiry was journaled")
	}
}

// D3, D11 and D19. A Runner stopped while its run's attempt is under way,
// which renews its claim no longer and finishes nothing, has the run
// interrupted by the other Runner's tick once the lease runs out by the
// database's clock, and not before; it is not run again, by the other's
// dispatcher or a later start; its retry on a host without the attempt's
// directory is refused (D11); and the stopped Runner's late finish writes
// nothing.
func TestPostgreSQLARunnerStoppedMidAttemptHasItsRunInterruptedAfterTheLease(t *testing.T) {
	p := pgRunners(t)
	stopped, other := p.open(), p.open()
	id := p.submit("one")
	byStopped, ok, err := claimRun(stopped.store, id)
	if err != nil || !ok {
		t.Fatal("the run was not claimed:", ok, err)
	}
	must(t, other.interruptLapsed())
	if r, err := other.store.run(id); err != nil || r.State != "running" {
		t.Fatal("a run under a live lease was interrupted:", r.State, err)
	}
	pgExec(t, p.suite, "UPDATE runs SET lease_until=now()-interval '1 second' WHERE id=$1", id)
	must(t, other.interruptLapsed())
	if d, err := other.dispatch(context.Background()); d != dispatchedNone || err != nil {
		t.Fatal("the interrupted run was dispatched again:", d, err)
	}
	later := p.open()
	r, err := later.store.run(id)
	if err != nil || r.State != "interrupted" || r.Attempt != 1 {
		t.Fatalf("the run is held %s at attempt %d, %v; want interrupted at 1", r.State, r.Attempt, err)
	}
	if n := entriesOfKind(t, later.store, "run.interrupted", id); n != 1 {
		t.Fatal("the run was interrupted", n, "times")
	}
	got := serve(t, other, "POST", "/v1/runs/"+id+"/retry?scope=team-a", `{"attempt":1}`, nil)
	if got.Code != 409 || !strings.Contains(got.Body.String(), "cannot be read") {
		t.Fatal("a retry on a host without the attempt's directory:", got.Code, got.Body.String())
	}
	wantFault(t, finishRunIn(stopped.store, completed(byStopped)), faultConflict, "the stopped Runner's finish after its lease ran out")
}

// D3 and D18. A Runner's start, on a store another Runner works, leaves the
// other's run under a live claim running and its preparation under a live
// claim preparing: its holder finishes both.
func TestPostgreSQLASecondRunnersStartLeavesTheFirstsWorkToIt(t *testing.T) {
	p := pgRunners(t)
	first := p.open()
	id := p.submit("one")
	r, ok, err := claimRun(first.store, id)
	if err != nil || !ok {
		t.Fatal("the run was not claimed:", ok, err)
	}
	o, _ := p.admitted(first)
	preparing, err := first.claimOccurrence(o.ID, "accepted")
	must(t, err)
	preparing.State = "preparing"
	must(t, first.keepOccurrence(preparing, "accepted"))
	second := p.open()
	if got, err := second.store.run(id); err != nil || got.State != "running" {
		t.Fatal("the second Runner's start interrupted the first's run:", got.State, err)
	}
	if got := storedOccurrence(t, second, o.ID); got.State != "preparing" {
		t.Fatal("the second Runner's start failed the first's preparation:", got.State)
	}
	must(t, finishRunIn(first.store, completed(r)))
	preparing.State = "accepted"
	must(t, first.saveOccurrence(preparing))
	for _, c := range []struct{ kind, id string }{{"run.interrupted", id}, {"occurrence.failed", o.ID}} {
		if n := entriesOfKind(t, second.store, c.kind, c.id); n != 0 {
			t.Fatalf("%s was journaled %d times", c.kind, n)
		}
	}
}

// D17, D18 and section 5: a store migrated to the file before
// 0004_several_runners.sql, as an earlier release left it, is refused at the
// start until it is migrated, and the migration then applies that file and
// the ones after it.
func TestPostgreSQLRefusesAStoreWithoutTheSeveralRunnersFile(t *testing.T) {
	pgRefusesAStoreWithout(t, "0004_")
}

// pgRefusesAStoreWithout makes a store migrated to the file before the one
// named prefix, as an earlier release left it, and holds the start to refuse
// it until the migration applies that file and the ones after it. It answers
// the store's migration login and database.
func pgRefusesAStoreWithout(t *testing.T, prefix string) (*pgTestServer, string) {
	t.Helper()
	s := pgServer(t)
	set, err := postgresqlMigrations()
	must(t, err)
	at := slices.IndexFunc(set, func(m pgMigration) bool { return strings.HasPrefix(m.name, prefix) })
	if at < 1 {
		t.Fatal("the set holds no file", prefix, "after another")
	}
	db := pgFreshDatabase(t, s)
	migration := PostgreSQLMigration{Connection: s.url(pgRoleMigration, db), Owner: s.role(pgRoleOwner), Request: s.role(pgRoleRequest), Worker: s.role(pgRoleWorker)}
	if applied, err := migratePostgreSQL(migration, set[:at]); err != nil || len(applied) != at {
		t.Fatal("the earlier store was not made:", applied, err)
	}
	k := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleWorker, db))
	if err := k.checkMigrations(); err == nil || !strings.Contains(err.Error(), "run jpack-runner store migrate") {
		t.Fatal("a store without", set[at].name, "was not refused at the start:", err)
	}
	var later []string
	for _, m := range set[at:] {
		later = append(later, m.version)
	}
	applied, err := MigratePostgreSQL(migration)
	if err != nil || !slices.Equal(applied, later) {
		t.Fatal("the migration did not apply", later, "alone:", applied, err)
	}
	must(t, k.checkMigrations())
	return s, db
}

// D18. A Runner preparing an occurrence commits a new task, its step's
// intent before its network call, after another Runner's cancellation
// request could have read the occurrence and before its write: the
// cancellation keeps the task and asks it cancelled, and the preparing
// Runner's next write is refused.
func TestPostgreSQLACancellationKeepsATaskAnotherRunnerCommittedBeforeItsWrite(t *testing.T) {
	p := pgRunners(t)
	canceller, preparer := p.open(), p.open()
	o, _ := p.admitted(canceller)
	prep, err := preparer.claimOccurrence(o.ID, "accepted")
	must(t, err)
	prep.State = "waiting"
	prep.Preparation = &SourcePreparation{StartedAt: now(), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Tasks: []SourceTask{}}
	must(t, preparer.keepOccurrence(prep, "accepted"))
	canceller.store = &commitBefore{runStore: canceller.store, change: func(runStore) error {
		prep.Preparation.Tasks = append(prep.Preparation.Tasks, SourceTask{Name: "detail", ID: "op-late", State: "waiting", StartedAt: now()})
		return preparer.keepPreparation(prep)
	}}
	got, err := canceller.cancelOccurrence(context.Background(), o.ID)
	must(t, err)
	held := storedOccurrence(t, preparer, o.ID)
	for _, c := range []struct {
		name string
		o    Occurrence
	}{{"stored", held}, {"answered", got}} {
		if c.o.State != "cancelled" || c.o.Preparation == nil || len(c.o.Preparation.Tasks) != 1 || c.o.Preparation.Tasks[0].State != "cancel-requested" {
			t.Fatalf("the %s cancellation did not keep the task committed before its write: %+v", c.name, c.o.Preparation)
		}
	}
	if err := preparer.savePreparation(prep); !errors.Is(err, errPreparationStopped) {
		t.Fatal("the preparing Runner's write after the cancellation was not refused:", err)
	}
}
