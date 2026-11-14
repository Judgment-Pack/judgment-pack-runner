package runner

// Two Runners on one postgresql store (JPACK_TEST_POSTGRESQL) meeting on one
// queued run: the dispatcher that loses the claim works nothing and goes on.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// queueRace holds each of the stores it wraps, all sharing read, at its
// first read of the queue until every one has read it, so that their claims
// meet on one run.
type queueRace struct {
	runStore
	read *sync.WaitGroup
	once sync.Once
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
	return r, err
}

// Two dispatchers, each a Runner of its own on one store, read the same
// queued run: exactly one claims and works it, the other works nothing and
// returns no error, so that its dispatcher goes on, and it works the next
// run queued.
func TestPostgreSQLADispatcherThatLosesAClaimGoesOn(t *testing.T) {
	s := pgServer(t)
	db := s.database(t)
	connections := s.connections(db)
	shortClaimTimes(t, 30*time.Millisecond, 2*time.Second)
	var runners [2]*Service
	var runtime string
	for i := range runners {
		cfg := journalConfig(t)
		cfg.PostgreSQL = &connections
		runners[i] = openJournal(t, cfg)
		defer runners[i].Close()
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
	submit := func(key string) string {
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
	raced := submit("one")
	var read sync.WaitGroup
	read.Add(len(runners))
	for _, sv := range runners {
		sv.store = &queueRace{runStore: sv.store, read: &read}
	}
	type result struct {
		worked bool
		err    error
	}
	var results [2]result
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
