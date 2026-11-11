package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A release check held to a supplied reviewed set through the pinned Runtime
// whose packs covers it asks (Runtime 0.41.0, scripts/checks/build-pinned.sh),
// with locks written by hand: Runner reads the verb's own answers as it reads
// the scripted ones. A lock that covers the pack's exact bytes saves the
// release, which keeps the lock; a lock with no entry for the pack, and one
// that is not JSON, are refused with these bytes, and no release is saved.
func TestSuppliedReviewedSetAgreesWithTheRuntimesPacksCovers(t *testing.T) {
	bin := os.Getenv("JPACK_COVERS_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_COVERS_TEST_BIN to the pinned Runtime with packs covers (scripts/checks/build-pinned.sh)")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("JPACK_COVERS_TEST_BIN must name the pinned Runtime by absolute path")
	}
	pack, e := os.ReadFile("testdata/triage.pack.json")
	must(t, e)
	lock := func(packs string) string {
		return `{"lockVersion":"1","config":{"digest":"` + lockedConfigDigest + `"},"packs":{` + packs + `},"graphs":{}}`
	}
	covering := lock(`"triage":{"path":"packs/triage.json","digest":"` + sha(string(pack)) + `"}`)
	for _, c := range []struct {
		name, lock string
		code       int
		want       string
	}{
		{"covered", covering, 201, ""},
		{"no-entry", lock(``), 422, `{"error":{"code":"pack_not_in_reviewed_set","message":"The supplied reviewed-set lock does not cover this pack's exact bytes under the id given: the pinned Runtime reports lock-entry-missing. No release was saved.","retryable":false}}` + "\n"},
		{"malformed", `{"lockVersion":"1"`, 422, `{"error":{"code":"invalid_reviewed_set","message":"The pinned Runtime refused to compare the supplied reviewed-set lock (JPS-LOCK-JSON), and no release was saved. Supply the lock as the Runtime's packs lock wrote it.","retryable":false}}` + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, e := filepath.EvalSymlinks(t.TempDir())
			must(t, e)
			must(t, os.Chmod(dir, 0700))
			s, e := Open(Config{Dir: dir, Runtime: bin, Workspace: "test-workspace", Owner: "local-owner"})
			must(t, e)
			defer s.Close()
			request := string(encode(PreviewRequest{Pack: string(pack), Input: sample(), ReviewedSet: &ReviewedSetRequest{Lock: c.lock, ID: "triage"}}))
			res := serve(t, s, "POST", "/v1/previews", request, nil)
			if res.Code != c.code {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if c.code != 201 {
				if res.Body.String() != c.want {
					t.Fatalf("answered %d %s", res.Code, res.Body.String())
				}
				if n := releasesHeld(t, s); n != 0 {
					t.Fatal("a refused check saved a release:", n)
				}
				return
			}
			var r Release
			must(t, json.Unmarshal(res.Body.Bytes(), &r))
			want := SuppliedReviewedSet{ID: "triage", Lock: covering, LockDigest: sha(covering)}
			if r.SuppliedReviewedSet == nil || *r.SuppliedReviewedSet != want {
				t.Fatal("the release was served holding", r.SuppliedReviewedSet)
			}
			if held, e := s.release(r.ID); e != nil || held.SuppliedReviewedSet == nil || *held.SuppliedReviewedSet != want {
				t.Fatal("the store reads the release as", held.SuppliedReviewedSet, e)
			}
		})
	}
}
