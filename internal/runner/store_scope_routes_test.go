package runner

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

// scopeRecorder is a store that records the scope each transaction was held
// to, by the function that opened it (transactionOpener), over the store it
// wraps: what the Runner asks of a kind, whether or not the kind holds a
// policy.
type scopeRecorder struct {
	runStore
	held storeScope
	log  *scopeLog
}

type scopeLog struct {
	mu   sync.Mutex
	held map[string][]storeScope
}

func (k *scopeRecorder) heldTo(sc storeScope) runStore {
	return &scopeRecorder{runStore: k.runStore.heldTo(sc), held: sc, log: k.log}
}

func (k *scopeRecorder) update(fn func(runTx) error) error {
	opener := transactionOpener()
	k.log.mu.Lock()
	k.log.held[opener] = append(k.log.held[opener], k.held)
	k.log.mu.Unlock()
	return k.runStore.update(fn)
}

// heldBy are the scopes the transactions opener opened were held to, and
// forgets them.
func (l *scopeLog) heldBy(opener string) []storeScope {
	l.mu.Lock()
	defer l.mu.Unlock()
	held := l.held[opener]
	delete(l.held, opener)
	return held
}

// recordingScopes opens every store of the default kind under a
// scopeRecorder, for the rest of the test.
func recordingScopes(t *testing.T) *scopeLog {
	t.Helper()
	log := &scopeLog{held: map[string][]storeScope{}}
	open := storeKinds[defaultStoreKind]
	storeKinds[defaultStoreKind] = func(o storeOpening) (runStore, error) {
		st, err := open(o)
		if err != nil {
			return nil, err
		}
		return &scopeRecorder{runStore: st, held: everyScope, log: log}, nil
	}
	t.Cleanup(func() { storeKinds[defaultStoreKind] = open })
	return log
}

// A request's transaction is held to the scope the request names, or to
// none when it names none (docs/design/store-postgresql.md, section 6): a job
// created under the scope its body names, and an upgrade under the scope its
// query names.
func TestARequestsTransactionIsHeldToTheScopeItNames(t *testing.T) {
	log := recordingScopes(t)
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	runtime := completingRuntime(t, cfg.Dir)
	want := func(opener string, scopes ...storeScope) {
		t.Helper()
		if got := log.heldBy(opener); !slices.Equal(got, scopes) {
			t.Fatalf("%s held its transactions to %+v, want %+v", opener, got, scopes)
		}
	}
	jobs := map[string]Job{}
	for _, scope := range []string{"a", ""} {
		named := ""
		if scope != "" {
			named = `,"scope":"` + scope + `"`
		}
		release := journalRelease(t, s, runtime)
		got := serve(t, s, "POST", "/v1/jobs", `{"name":"Held","releaseId":"`+release.ID+`","reviewed":true`+named+`}`, nil)
		if got.Code != 201 {
			t.Fatalf("%d %s", got.Code, got.Body)
		}
		want("(*Service).createJobWith", scopeHeld(scope))
		id := got.Body.String()
		id = id[strings.Index(id, `"id":"`)+6:]
		j, err := s.job(id[:strings.Index(id, `"`)])
		must(t, err)
		jobs[scope] = j
	}
	for scope, j := range jobs {
		req := UpgradeJobRequest{ReleaseID: journalRelease(t, s, runtime).ID, BaseReleaseID: j.ReleaseID, BaseRevision: j.Revision, Reviewed: true}
		if got := serve(t, s, "POST", withScope("/v1/jobs/"+j.ID+"/upgrade", scope), string(encode(req)), nil); got.Code != 200 {
			t.Fatalf("%d %s", got.Code, got.Body)
		}
		want("(*Service).upgradeJob", scopeHeld(scope))
	}
}
