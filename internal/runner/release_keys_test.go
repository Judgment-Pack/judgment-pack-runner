package runner

import (
	"strings"
	"testing"
)

// onTheMemoryKind opens every store of the default kind, for the rest of the
// test, as a new in-memory store: a Service whose transactions are held to
// section 6's row policy (docs/design/store-postgresql.md), as the second
// kind's will be, so that its answers can be read beside the sqlite kind's.
func onTheMemoryKind(t *testing.T) {
	t.Helper()
	open := storeKinds[defaultStoreKind]
	storeKinds[defaultStoreKind] = func(o storeOpening) (runStore, error) {
		if o.afterLock != nil {
			if err := o.afterLock(); err != nil {
				return nil, err
			}
		}
		return newMemoryStore(), nil
	}
	t.Cleanup(func() { storeKinds[defaultStoreKind] = open })
}

// A release makes one job, and a job revision names a release once, for
// every scope (D9). The sqlite kind's reads before the write see every
// scope's jobs, and answer release_already_used; a store held to the scope
// the request names reads that scope's jobs alone, and meets another scope's
// use of the release at its key, which the Runner answers the same, with the
// message of the key met: a creation's or a revision's.
func TestAReleaseAnotherScopeHoldsIsAnsweredAsOnSQLite(t *testing.T) {
	for _, kind := range []string{"sqlite", "memory"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "memory" {
				onTheMemoryKind(t)
			}
			cfg := journalConfig(t)
			s := openJournal(t, cfg)
			t.Cleanup(func() { s.Close() })
			runtime := completingRuntime(t, cfg.Dir)
			wantUsed := func(what string, code int, body, message string) {
				t.Helper()
				if code != 409 || !strings.Contains(body, `"release_already_used"`) || !strings.Contains(body, message) {
					t.Fatalf("%s: %d %s", what, code, body)
				}
			}
			create := func(release, scope string) (int, string) {
				named := ""
				if scope != "" {
					named = `,"scope":"` + scope + `"`
				}
				got := serve(t, s, "POST", "/v1/jobs", `{"name":"Other","releaseId":"`+release+`","reviewed":true`+named+`}`, nil)
				return got.Code, got.Body.String()
			}
			upgrade := func(j Job, release, scope string) (int, string) {
				req := UpgradeJobRequest{ReleaseID: release, BaseReleaseID: j.ReleaseID, BaseRevision: j.Revision, Reviewed: true}
				got := serve(t, s, "POST", withScope("/v1/jobs/"+j.ID+"/upgrade", scope), string(encode(req)), nil)
				return got.Code, got.Body.String()
			}
			created := journalRelease(t, s, runtime)
			a, err := s.createJobBy(requester{}, "Scope A", created.ID, "a", nil)
			must(t, err)
			// The release scope a's job was created from.
			code, body := create(created.ID, "b")
			wantUsed("a job under scope b from scope a's creation", code, body, "already created a different job")
			// The release a revision of scope a's job names.
			revised := journalRelease(t, s, runtime)
			if code, body = upgrade(a, revised.ID, "a"); code != 200 {
				t.Fatalf("the upgrade under scope a: %d %s", code, body)
			}
			code, body = create(revised.ID, "b")
			wantUsed("a job under scope b from scope a's revision", code, body, "already belongs to a job revision")
			code, body = create(revised.ID, "")
			wantUsed("a job under none from scope a's revision", code, body, "already belongs to a job revision")
			// An upgrade under scope b to a release scope a's job holds.
			b, err := s.createJobBy(requester{}, "Scope B", journalRelease(t, s, runtime).ID, "b", nil)
			must(t, err)
			for _, release := range []string{created.ID, revised.ID} {
				code, body = upgrade(b, release, "b")
				wantUsed("an upgrade under scope b to scope a's release", code, body, "already belongs to a job revision")
			}
			// Nothing was written.
			held, err := s.job(b.ID)
			must(t, err)
			if held.Revision != 1 || held.ReleaseID != b.ReleaseID {
				t.Fatalf("a refused upgrade changed the job: %+v", held)
			}
			list := serve(t, s, "GET", "/v1/jobs", "", nil)
			if n := strings.Count(list.Body.String(), `"releaseId"`); n != 2 {
				t.Fatalf("the jobs held are %d: %s", n, list.Body)
			}
		})
	}
}
