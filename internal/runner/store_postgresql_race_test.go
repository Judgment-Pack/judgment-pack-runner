package runner

// Two Runners on one postgresql store (JPACK_TEST_POSTGRESQL) meeting on one
// queued run: the dispatcher that loses it works nothing and goes on.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// queueRace holds each of the stores it wraps, all sharing read, at its
// first read of the queue until every one has read it, so that their writes
// meet on one run. queued and released, when set, change the run and the
// release as the dispatcher reads them.
type queueRace struct {
	runStore
	read     *sync.WaitGroup
	once     sync.Once
	queued   func(Run) Run
	released func(Release) Release
}

func (q *queueRace) leasesClaims() bool { return leases(q.runStore) }

func (q *queueRace) updateWithin(d time.Duration, fn func(runTx) error) error {
	return updateWithin(q.runStore, d, fn)
}

func (q *queueRace) oldestQueued() (Run, error) {
	r, err := q.runStore.oldestQueued()
	q.once.Do(func() {
		q.read.Done()
		q.read.Wait()
	})
	if err == nil && q.queued != nil {
		r = q.queued(r)
	}
	return r, err
}

func (q *queueRace) release(id string) (Release, error) {
	r, err := q.runStore.release(id)
	if err == nil && q.released != nil {
		r = q.released(r)
	}
	return r, err
}

// pgTwoRunners are two Runners, each with a state directory of its own, on
// one store, and the function that queues a run of a job of theirs whose
// Runtime completes.
func pgTwoRunners(t *testing.T) ([2]*Service, func(key string) string) {
	t.Helper()
	s := pgServer(t)
	db := s.database(t)
	connections := s.connections(db)
	var runners [2]*Service
	var runtime string
	for i := range runners {
		cfg := journalConfig(t)
		cfg.PostgreSQL = &connections
		runners[i] = openJournal(t, cfg)
		t.Cleanup(runners[i].Close)
		pinned := completingRuntime(t, cfg.Dir)
		if runtime != "" && pinned != runtime {
			t.Fatal("the two Runners pinned different Runtimes")
		}
		runtime = pinned
	}
	first := runners[0]
	release := journalRelease(t, first, runtime)
	got := serve(t, first, "POST", "/v1/jobs", `{"name":"Raced","releaseId":"`+release.ID+`","reviewed":true,"scope":"team-a"}`, nil)
	if got.Code != 201 {
		t.Fatal(got.Code, got.Body.String())
	}
	var j Job
	must(t, json.Unmarshal(got.Body.Bytes(), &j))
	return runners, func(key string) string {
		t.Helper()
		got := serve(t, first, "POST", "/v1/jobs/"+j.ID+"/runs?scope=team-a", string(encode(journalInput())), map[string]string{"Idempotency-Key": key})
		if got.Code != 202 {
			t.Fatal(got.Code, got.Body.String())
		}
		var queued struct {
			ID string `json:"id"`
		}
		must(t, json.Unmarshal(got.Body.Bytes(), &queued))
		return queued.ID
	}
}

// raceForTheQueue holds each Runner's store as a queueRace and dispatches on
// both at once. Both must return no error, so that each dispatcher goes on,
// and exactly one must have worked the run: it returns the index of the
// other.
func raceForTheQueue(t *testing.T, runners [2]*Service, queued func(Run) Run, released func(Release) Release) int {
	t.Helper()
	var read sync.WaitGroup
	read.Add(len(runners))
	for _, sv := range runners {
		sv.store = &queueRace{runStore: sv.store, read: &read, queued: queued, released: released}
	}
	var results [2]struct {
		worked bool
		err    error
	}
	var dispatched sync.WaitGroup
	for i, sv := range runners {
		dispatched.Add(1)
		go func() {
			defer dispatched.Done()
			results[i].worked, results[i].err = sv.dispatchNext(context.Background())
		}()
	}
	dispatched.Wait()
	loser := -1
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("the dispatcher of Runner %d met an error, which stops it: %v", i, r.err)
		}
		if !r.worked {
			loser = i
		}
	}
	if loser < 0 || !results[1-loser].worked {
		t.Fatalf("not exactly one dispatcher worked the run: %+v", results)
	}
	return loser
}

// Two dispatchers, each a Runner of its own on one store, read the same
// queued run: exactly one claims and works it, the other works nothing and
// returns no error, so that its dispatcher goes on, and it works the next
// run queued.
func TestPostgreSQLADispatcherThatLosesAClaimGoesOn(t *testing.T) {
	shortClaimTimes(t, 30*time.Millisecond, 2*time.Second)
	runners, submit := pgTwoRunners(t)
	first := runners[0]
	raced := submit("one")
	loser := raceForTheQueue(t, runners, nil, nil)
	r, err := first.store.run(raced)
	must(t, err)
	if r.State != "completed" || r.Attempt != 1 {
		t.Fatalf("the run raced for was not worked once: %s, attempt %d", r.State, r.Attempt)
	}
	next := submit("two")
	if worked, err := runners[loser].dispatchNext(context.Background()); !worked || err != nil {
		t.Fatal("the dispatcher that lost the claim did not work the next run:", worked, err)
	}
	if r, err = first.store.run(next); err != nil || r.State != "completed" {
		t.Fatal("the next run was not worked:", r.State, err)
	}
}

// The same race on a run each dispatcher ends in the queue, before it
// starts: one expired there, and one whose release names a profile set
// aside. Exactly one ends it, the other works nothing and returns no error.
func TestPostgreSQLADispatcherThatLosesARunEndedInTheQueueGoesOn(t *testing.T) {
	for _, c := range []struct {
		name     string
		set      func(*Service)
		queued   func(Run) Run
		released func(Release) Release
		reason   string
	}{
		{
			name: "expired in the queue",
			queued: func(r Run) Run {
				r.Trigger = &TriggerOrigin{Kind: "event", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
				return r
			},
			reason: runQueueExpired,
		},
		{
			name: "a profile set aside",
			set:  func(sv *Service) { sv.setAside = []SetAsideProfile{{ID: "set-aside", Reason: "the test set it aside"}} },
			released: func(r Release) Release {
				r.InputMapping = &InputMapping{Sources: []MappingSource{{Name: "case", Kind: "document", Profile: "set-aside"}}}
				return r
			},
			reason: runProfileSetAside,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			runners, submit := pgTwoRunners(t)
			if c.set != nil {
				for _, sv := range runners {
					c.set(sv)
				}
			}
			raced := submit("one")
			raceForTheQueue(t, runners, c.queued, c.released)
			r, err := runners[0].store.run(raced)
			must(t, err)
			if r.State != "failed" || r.Reason != c.reason || r.Attempt != 0 {
				t.Fatalf("the run raced for was not ended in the queue once: %s, %s, attempt %d", r.State, r.Reason, r.Attempt)
			}
		})
	}
}
