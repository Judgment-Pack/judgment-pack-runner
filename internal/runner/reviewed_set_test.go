package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Runner #210: a release check held to a reviewed-set lock made outside
// Runner, which the pinned Runtime's packs covers compares with the pack's
// exact bytes.

// sha is the digest of b in the form the lock and Runner write, computed here
// and not by the package under test.
func sha(b string) string {
	sum := sha256.Sum256([]byte(b))
	return "sha256:" + hex.EncodeToString(sum[:])
}

const (
	// lockedConfigDigest is the configuration digest the hand-written lock pins.
	lockedConfigDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// coversCall is the invocation a release check makes of packs covers.
	coversCall = "packs covers --lock reviewed-set.lock.json --id journal --pack pack.json --format json"
)

// suppliedLock is a reviewed-set lock written by hand, as packs lock writes
// one, that names the journal pack's exact bytes under the id journal.
var suppliedLock = `{"lockVersion":"1","config":{"digest":"` + lockedConfigDigest + `"},"packs":{"journal":{"path":"packs/journal.json","digest":"` + sha(journalPack) + `"}},"graphs":{}}`

// coversReport is a packs covers report of Runtime 0.41.0, written out by
// hand: its status, the lock's digest, the id, the pack's digest, the entry or
// none, and the inside of its findings list.
func coversReport(status, lockDigest, id, packDigest, entry, findings string) string {
	if entry != "" {
		entry = `,"entry":` + entry
	}
	return `{"outputVersion":"2","tool":{"name":"jpack","version":"0.41.0"},"command":"packs covers","status":"` + status + `","kind":"non-normative-runtime-convention","lockPath":"reviewed-set.lock.json","lockDigest":"` + lockDigest + `","lockVersion":"1","configDigest":"` + lockedConfigDigest + `","id":"` + id + `","packDigest":"` + packDigest + `"` + entry + `,"findings":[` + findings + `]}` + "\n"
}

// coversRefusal is a packs covers refusal of Runtime 0.41.0, written out by
// hand: its status and its diagnostic's code.
func coversRefusal(status, code string) string {
	return `{"outputVersion":"2","tool":{"name":"jpack","version":"0.41.0"},"command":"packs covers","status":"` + status + `","diagnostics":[{"code":"` + code + `","codeStability":"provisional","layer":"operation","severity":"error","instancePath":"","message":"The reviewed-set lock reviewed-set.lock.json has a member \"Packs\" that packs lock does not write."}]}` + "\n"
}

func journalEntry(digest string) string {
	return `{"kind":"pack","id":"journal","path":"packs/journal.json","digest":"` + digest + `"}`
}

// covered is the report of a Runtime whose packs covers says suppliedLock
// covers the journal pack.
var covered = coversReport("valid", sha(suppliedLock), "journal", sha(journalPack), journalEntry(sha(journalPack)), "")

// answerCovers has the Runtime of a checkService answer packs covers with
// answer, exiting exit.
func answerCovers(t *testing.T, log, answer string, exit int) {
	t.Helper()
	covers := filepath.Join(filepath.Dir(log), "covers")
	must(t, os.WriteFile(covers+".answer", []byte(answer), 0600))
	must(t, os.WriteFile(covers+".exit", []byte(strconv.Itoa(exit)), 0600))
}

func reviewedRequest(set *ReviewedSetRequest) string {
	return string(encode(PreviewRequest{Pack: journalPack, Input: journalInput(), ReviewedSet: set}))
}

// checkHeld checks a release of the journal pack held to suppliedLock under
// journal, which the Runtime must say it covers.
func checkHeld(t *testing.T, s *Service, log string) Release {
	t.Helper()
	answerCovers(t, log, covered, 0)
	res := serve(t, s, "POST", "/v1/previews", reviewedRequest(&ReviewedSetRequest{Lock: suppliedLock, ID: "journal"}), nil)
	if res.Code != 201 {
		t.Fatalf("release check: %d %s", res.Code, res.Body.String())
	}
	var r Release
	must(t, json.Unmarshal(res.Body.Bytes(), &r))
	return r
}

// A release check that supplies a reviewed set asks the pinned Runtime's packs
// covers, given the lock's exact bytes and the pack's, before anything else is
// asked of the pack, and keeps the lock's bytes and digest when it says the
// lock covers the pack. Its runs are given the release's own configuration and
// lock only, and are served with reviewedSetOrigin supplied and the lock's
// digest, after the store is reopened too.
func TestReleaseHeldToASuppliedReviewedSet(t *testing.T) {
	evaluate := "experimental evaluate --pack-id target --config jpack.json --facts facts.json --format json"
	doc := readOpenAPI(t)
	request := reviewedRequest(&ReviewedSetRequest{Lock: suppliedLock, ID: "journal"})
	if problems := doc.check("PreviewRequest", doc.schema("PreviewRequest"), decodeNumbers(t, []byte(request))); len(problems) > 0 {
		t.Fatal(problems)
	}
	s, log := checkService(t)
	answerCovers(t, log, covered, 0)
	res := serve(t, s, "POST", "/v1/previews", request, nil)
	if res.Code != 201 {
		t.Fatalf("release check: %d %s", res.Code, res.Body.String())
	}
	member := `,"suppliedReviewedSet":{"id":"journal","lock":` + string(encode(suppliedLock)) + `,"lockDigest":"` + sha(suppliedLock) + `"}}`
	if !strings.HasSuffix(res.Body.String(), member+"\n") {
		t.Fatalf("the release was served as\n%s", res.Body.String())
	}
	if problems := doc.check("Release", doc.schema("Release"), decodeNumbers(t, res.Body.Bytes())); len(problems) > 0 {
		t.Fatal(problems)
	}
	if described, _ := doc.schema("Release")["properties"].(map[string]any)["suppliedReviewedSet"].(map[string]any); described == nil {
		t.Fatal("the API document does not describe suppliedReviewedSet")
	}
	var r Release
	must(t, json.Unmarshal(res.Body.Bytes(), &r))
	want := coversCall + " | " + configWithout + " | none\n" +
		"given " + suppliedLock + " | " + journalPack + "\n" +
		"spec validate pack.json --format json | " + configWithout + " | none\n" +
		"packs lock --config jpack.json --format json | " + configWithout + " | none\n" +
		evaluate + " --rehearsal | " + configWithout + " | " + checkLock + "\n"
	if got := takeLog(t, log); got != want {
		t.Fatalf("the check invoked\n%s\nnot\n%s", got, want)
	}
	var stored string
	must(t, s.db.QueryRow("SELECT record FROM releases WHERE id=?", r.ID).Scan(&stored))
	if !strings.HasSuffix(stored, member) || r.Config != configWithout || r.Lock != checkLock {
		t.Fatalf("the store holds\n%s", stored)
	}
	// The Runtime was handed the lock in a file its owner alone can read.
	if info, e := os.Lstat(filepath.Join(s.cfg.Dir, "releases", r.ID, reviewedSetFile)); e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("the supplied lock was written as", info, e)
	}

	j, e := s.createJob("Held", r.ID)
	must(t, e)
	run := runOnce(t, s, j.ID, "one")
	if run.State != "completed" {
		t.Fatal("the run ended", run.State, run.Problem)
	}
	if got, want := takeLog(t, log), evaluate+" | "+configWithout+" | "+checkLock+"\n"; got != want {
		t.Fatalf("the run invoked\n%s\nnot\n%s", got, want)
	}
	if _, e := os.Lstat(filepath.Join(s.cfg.Dir, "attempts", run.ID, reviewedSetFile)); !os.IsNotExist(e) {
		t.Fatal("the run was given the supplied lock:", e)
	}
	served := strings.TrimSuffix(string(encode(served(run))), "}") + `,"reviewedSetOrigin":"supplied","suppliedLockDigest":"` + sha(suppliedLock) + `"}` + "\n"
	if described, _ := doc.schema("Run")["properties"].(map[string]any)["suppliedLockDigest"].(map[string]any); described == nil {
		t.Fatal("the API document does not describe suppliedLockDigest")
	}
	cfg := s.cfg
	for _, opened := range []string{"opened", "reopened"} {
		for name, res := range map[string]string{
			"GET /v1/runs/{run}":                     serve(t, s, "GET", "/v1/runs/"+run.ID, "", nil).Body.String(),
			"POST /v1/jobs/{job}/runs, a repetition": serve(t, s, "POST", "/v1/jobs/"+j.ID+"/runs", string(encode(journalInput())), map[string]string{"Idempotency-Key": "one"}).Body.String(),
		} {
			if res != served {
				t.Fatalf("%s, %s, served\n%s\nnot\n%s", opened, name, res, served)
			}
			if problems := doc.check("Run", doc.schema("Run"), decodeNumbers(t, []byte(res))); len(problems) > 0 {
				t.Fatal(opened, name, problems)
			}
		}
		var record string
		must(t, s.db.QueryRow("SELECT record FROM runs WHERE id=?", run.ID).Scan(&record))
		if strings.Contains(record, "reviewedSetOrigin") || strings.Contains(record, "suppliedLockDigest") {
			t.Fatal("the stored run carries the origin:", record)
		}
		if held, e := s.release(r.ID); e != nil || held.SuppliedReviewedSet == nil || *held.SuppliedReviewedSet != (SuppliedReviewedSet{ID: "journal", Lock: suppliedLock, LockDigest: sha(suppliedLock)}) {
			t.Fatal(opened, "the store reads the release as", held.SuppliedReviewedSet, e)
		}
		s.Close()
		var err error
		if s, err = Open(cfg); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
	}
}

// A reviewed set whose lock is empty or larger than 1 MiB, or whose id is not
// a decision id of at most 128 characters, is refused with these bytes before
// anything is written or any Runtime is invoked. A lock of 1 MiB and an id of
// 128 characters are given to the Runtime.
func TestReviewedSetRequestIsHeldToItsBounds(t *testing.T) {
	s, log := checkService(t)
	refusal := `{"error":{"code":"invalid_reviewed_set","message":"Supply reviewedSet as a reviewed-set lock of 1 byte to 1 MiB and the decision id the pack is locked under in it: lowercase letters and digits in words joined by single hyphens, at most 128 characters.","retryable":false}}` + "\n"
	for name, set := range map[string]ReviewedSetRequest{
		"an empty lock":                    {Lock: "", ID: "journal"},
		"a lock of 1 MiB and a byte":       {Lock: strings.Repeat("x", 1<<20+1), ID: "journal"},
		"no id":                            {Lock: suppliedLock, ID: ""},
		"an id read as a flag":             {Lock: suppliedLock, ID: "--format=human"},
		"an id that begins with a hyphen":  {Lock: suppliedLock, ID: "-journal"},
		"an id in capitals":                {Lock: suppliedLock, ID: "Journal"},
		"an id with two hyphens in a row":  {Lock: suppliedLock, ID: "journal--pack"},
		"an id that ends with a hyphen":    {Lock: suppliedLock, ID: "journal-"},
		"an id with a space":               {Lock: suppliedLock, ID: "journal pack"},
		"an id of 129 characters":          {Lock: suppliedLock, ID: strings.Repeat("j", 129)},
		"an id that names a path":          {Lock: suppliedLock, ID: "../journal"},
		"an id with a line break after it": {Lock: suppliedLock, ID: "journal\n"},
	} {
		res := serve(t, s, "POST", "/v1/previews", reviewedRequest(&set), nil)
		if res.Code != 422 || res.Body.String() != refusal {
			t.Fatalf("%s: answered %d %s", name, res.Code, res.Body.String())
		}
	}
	for name, body := range map[string]string{
		"a member a reviewed set does not have":    `{"pack":` + string(encode(journalPack)) + `,"input":{"facts":{}},"reviewedSet":{"lock":"{}","id":"journal","lockDigest":"` + sha("{}") + `"}}`,
		"a reviewed set that is a string":          `{"pack":` + string(encode(journalPack)) + `,"input":{"facts":{}},"reviewedSet":"{}"}`,
		"a reviewed set written null":              `{"pack":` + string(encode(journalPack)) + `,"input":{"facts":{}},"reviewedSet":null}`,
		"a reviewed set written null, in capitals": `{"pack":` + string(encode(journalPack)) + `,"input":{"facts":{}},"ReviewedSet":null}`,
	} {
		res := serve(t, s, "POST", "/v1/previews", body, nil)
		if res.Code != 400 || res.Body.String() != `{"error":{"code":"invalid_request","message":"Supply one bounded JSON request with supported fields and no duplicate members.","retryable":false}}`+"\n" {
			t.Fatalf("%s: answered %d %s", name, res.Code, res.Body.String())
		}
	}
	if got := takeLog(t, log); got != "" {
		t.Fatalf("a refused reviewed set invoked the Runtime\n%s", got)
	}
	if entries, e := os.ReadDir(filepath.Join(s.cfg.Dir, "releases")); (e != nil && !os.IsNotExist(e)) || len(entries) != 0 {
		t.Fatal("a refused reviewed set wrote", entries, e)
	}
	if n := releasesHeld(t, s); n != 0 {
		t.Fatal("a refused reviewed set saved a release:", n)
	}

	// At the bounds, the Runtime is asked, and its refusal of the lock is
	// answered.
	answerCovers(t, log, coversRefusal("error", "JPS-LOCK-JSON"), 1)
	refused := `{"error":{"code":"invalid_reviewed_set","message":"The pinned Runtime refused to compare the supplied reviewed-set lock (JPS-LOCK-JSON), and no release was saved. Supply the lock as the Runtime's packs lock wrote it.","retryable":false}}` + "\n"
	big := strings.Repeat("x", 1<<20)
	long := strings.Repeat("j", 128)
	for name, c := range map[string]struct{ set ReviewedSetRequest }{
		"a lock of 1 MiB":         {ReviewedSetRequest{Lock: big, ID: "journal"}},
		"an id of 128 characters": {ReviewedSetRequest{Lock: suppliedLock, ID: long}},
	} {
		res := serve(t, s, "POST", "/v1/previews", reviewedRequest(&c.set), nil)
		if res.Code != 422 || res.Body.String() != refused {
			t.Fatalf("%s: answered %d %s", name, res.Code, res.Body.String())
		}
		want := "packs covers --lock reviewed-set.lock.json --id " + c.set.ID + " --pack pack.json --format json | " + configWithout + " | none\n" +
			"given " + c.set.Lock + " | " + journalPack + "\n"
		if got := takeLog(t, log); got != want {
			t.Fatalf("%s: the Runtime was invoked\n%.300s", name, got)
		}
	}
}

// A release check is refused, with these bytes, and no release is saved,
// whenever the pinned Runtime's answer does not say that the supplied lock
// covers the pack's exact bytes under the id given: when it says the lock does
// not, when it refuses the lock, and when its answer is not one packs covers
// gives about that lock, id and pack, a Runtime before 0.41.0 among them.
// Without a reviewed set, the same Runtime is not asked.
func TestReviewedSetRefusedUnlessTheRuntimeSaysTheLockCoversThePack(t *testing.T) {
	notCovered := func(reported string) string {
		return `{"error":{"code":"pack_not_in_reviewed_set","message":"The supplied reviewed-set lock does not cover this pack's exact bytes under the id given: the pinned Runtime reports ` + reported + `. No release was saved.","retryable":false}}` + "\n"
	}
	refused := func(code string) string {
		return `{"error":{"code":"invalid_reviewed_set","message":"The pinned Runtime refused to compare the supplied reviewed-set lock (` + code + `), and no release was saved. Supply the lock as the Runtime's packs lock wrote it.","retryable":false}}` + "\n"
	}
	unsupported := `{"error":{"code":"reviewed_set_unsupported","message":"The pinned Runtime did not answer as packs covers answers, which a reviewedSet needs; Runtime 0.41.0 and later have it. Pin such a Runtime, or check the release without reviewedSet.","retryable":false}}` + "\n"
	lockDigest, packDigest := sha(suppliedLock), sha(journalPack)
	drift := `{"name":"document-drift","kind":"pack","id":"journal","path":"packs/journal.json","detail":"The pack document's bytes differ from the reviewed set."}`
	missing := `{"name":"lock-entry-missing","kind":"pack","id":"journal","detail":"The reviewed set does not name this pack."}`
	other := sha("another pack")
	for _, c := range []struct {
		name   string
		answer string
		exit   int
		want   string
	}{
		{"the pack's bytes differ from the entry", coversReport("invalid", lockDigest, "journal", packDigest, journalEntry(other), drift), 1, notCovered("document-drift")},
		{"the lock has no entry for the id", coversReport("invalid", lockDigest, "journal", packDigest, "", missing), 1, notCovered("lock-entry-missing")},
		{"two findings, one given twice", coversReport("invalid", lockDigest, "journal", packDigest, journalEntry(other), drift+","+missing+","+drift), 1, notCovered("document-drift, lock-entry-missing")},
		{"a lock of another shape", coversRefusal("error", "JPS-LOCK-SHAPE"), 1, refused("JPS-LOCK-SHAPE")},
		{"a lock that is not JSON", coversRefusal("error", "JPS-LOCK-JSON"), 1, refused("JPS-LOCK-JSON")},
		{"a lockVersion the Runtime does not read", coversRefusal("unsupported", "JPS-LOCK-VERSION"), 2, refused("JPS-LOCK-VERSION")},
		{"a lock past the Runtime's limit", coversRefusal("error", "JPS-RESOURCE-LOCK-BYTE-LIMIT"), 4, refused("JPS-RESOURCE-LOCK-BYTE-LIMIT")},
		{"a lock the Runtime could not read", coversRefusal("error", "JPS-LOCK-READ"), 4, refused("JPS-LOCK-READ")},
		{"a Runtime before 0.41.0", `{"outputVersion":"2","tool":{"name":"jpack","version":"0.40.0"},"command":"packs","status":"error","diagnostics":[{"code":"JPS-INVOCATION-ARGUMENTS","codeStability":"provisional","layer":"operation","severity":"error","instancePath":"","message":"unknown flag: --lock"}]}` + "\n", 3, unsupported},
		{"another command's report that the lock covers the pack", strings.Replace(covered, `"command":"packs covers"`, `"command":"packs verify"`, 1), 0, unsupported},
		{"a report that the lock covers the pack, exiting 1", covered, 1, unsupported},
		{"a report that the lock covers the pack, of another outputVersion", strings.Replace(covered, `"outputVersion":"2"`, `"outputVersion":"3"`, 1), 0, unsupported},
		{"a report that the lock covers the pack, about another id", coversReport("valid", lockDigest, "intake", packDigest, journalEntry(packDigest), ""), 0, unsupported},
		{"a report that the lock covers another pack", coversReport("valid", lockDigest, "journal", other, journalEntry(other), ""), 0, unsupported},
		{"a report that the lock covers the pack's bytes, about another pack", coversReport("valid", lockDigest, "journal", other, journalEntry(packDigest), ""), 0, unsupported},
		{"a report that another lock covers the pack", coversReport("valid", sha("another lock"), "journal", packDigest, journalEntry(packDigest), ""), 0, unsupported},
		{"a report that the lock covers the pack, with a finding", coversReport("valid", lockDigest, "journal", packDigest, journalEntry(packDigest), drift), 0, unsupported},
		{"a report that the lock covers the pack, with no findings member", strings.Replace(covered, `,"findings":[]`, ``, 1), 0, unsupported},
		{"a report that the lock covers the pack, with no entry", coversReport("valid", lockDigest, "journal", packDigest, "", ""), 0, unsupported},
		{"a report that the lock covers the pack, whose entry names other bytes", coversReport("valid", lockDigest, "journal", packDigest, journalEntry(other), ""), 0, unsupported},
		{"a report that the lock covers the pack, past 64 KiB", strings.TrimSuffix(covered, "\n") + strings.Repeat(" ", 64<<10) + "\n", 0, unsupported},
		{"a report of another status, exiting 0", strings.Replace(covered, `"status":"valid"`, `"status":"covered"`, 1), 0, unsupported},
		{"a report that the lock does not cover the pack, without findings", coversReport("invalid", lockDigest, "journal", packDigest, journalEntry(other), ""), 1, unsupported},
		{"a report that the lock does not cover the pack, about another id", coversReport("invalid", lockDigest, "intake", packDigest, "", missing), 1, unsupported},
		{"a report that the lock does not cover another pack", coversReport("invalid", lockDigest, "journal", other, journalEntry(packDigest), drift), 1, unsupported},
		{"a report that another lock does not cover the pack", coversReport("invalid", sha("another lock"), "journal", packDigest, journalEntry(other), drift), 1, unsupported},
		{"a finding whose name is not one", coversReport("invalid", lockDigest, "journal", packDigest, "", `{"name":"Not Covered","detail":"x"}`), 1, unsupported},
		{"the verb's refusal of its invocation", coversRefusal("error", "JPS-INVOCATION-COVERS"), 3, unsupported},
		{"a refusal whose code is not one", coversRefusal("error", "lock is bad"), 1, unsupported},
		{"a refusal without a code", strings.Replace(coversRefusal("error", "JPS-LOCK-SHAPE"), `"code":"JPS-LOCK-SHAPE",`, ``, 1), 1, unsupported},
		{"an unsupported status, exiting 1", coversRefusal("unsupported", "JPS-LOCK-VERSION"), 1, unsupported},
		{"an answer that is not JSON", "usage: jpack packs\n", 0, unsupported},
		{"an empty answer", "", 2, unsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, log := checkService(t)
			answerCovers(t, log, c.answer, c.exit)
			res := serve(t, s, "POST", "/v1/previews", reviewedRequest(&ReviewedSetRequest{Lock: suppliedLock, ID: "journal"}), nil)
			if res.Code != 422 || res.Body.String() != c.want {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if n := releasesHeld(t, s); n != 0 {
				t.Fatal("a refused check saved a release:", n)
			}
			want := coversCall + " | " + configWithout + " | none\n" + "given " + suppliedLock + " | " + journalPack + "\n"
			if got := takeLog(t, log); got != want {
				t.Fatalf("the Runtime was invoked\n%s", got)
			}
			if r := checkRelease(t, s, false); r.SuppliedReviewedSet != nil {
				t.Fatal("without a reviewed set the release holds", r.SuppliedReviewedSet)
			}
			if got := takeLog(t, log); strings.Contains(got, "packs covers") || !strings.HasPrefix(got, "spec validate ") {
				t.Fatalf("without a reviewed set the Runtime was invoked\n%s", got)
			}
		})
	}
}

// A refusal of packs covers whose code is not about the lock, such as one
// about the pack file Runner wrote beside it, is Runner's own fault and not
// the caller's: it is answered as any other failure of the request is, never
// as invalid_reviewed_set, and no release is saved.
func TestReviewedSetRefusalNotAboutTheLockIsRunnersFault(t *testing.T) {
	want := `{"error":{"code":"store_error","message":"The local runner could not complete this request.","retryable":false}}` + "\n"
	for _, c := range []struct {
		name, status, code string
		exit               int
	}{
		{"the pack file could not be read", "error", "JPS-INPUT-READ", 4},
		{"the pack file is not JSON", "error", "JPS-INPUT-JSON", 1},
		{"a resource limit not of the lock", "error", "JPS-RESOURCE-PACK-BYTE-LIMIT", 4},
		{"an unsupported pack", "unsupported", "JPS-PACK-VERSION", 2},
		{"a code that only begins as a lock's", "error", "JPS-LOCKS-SHAPE", 1},
		{"a code that is the lock prefix alone", "error", "JPS-LOCK", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, log := checkService(t)
			answerCovers(t, log, coversRefusal(c.status, c.code), c.exit)
			res := serve(t, s, "POST", "/v1/previews", reviewedRequest(&ReviewedSetRequest{Lock: suppliedLock, ID: "journal"}), nil)
			if res.Code != 500 || res.Body.String() != want {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if n := releasesHeld(t, s); n != 0 {
				t.Fatal("a failed check saved a release:", n)
			}
			if got := takeLog(t, log); !strings.HasPrefix(got, coversCall+" | ") || strings.Contains(got, "spec validate") {
				t.Fatalf("the Runtime was invoked\n%s", got)
			}
		})
	}
}

// A packs covers invocation that did not exit gave no answer to read: the
// Runtime could not start, was killed, or was still running at the deadline,
// whatever it wrote before. That is Runner's own fault, answered as a release
// check's packs lock that did not exit is (500 store_error), and never as a
// Runtime without packs covers. No release is saved, and nothing else is asked
// of the pack.
func TestReviewedSetCoversThatDidNotExitIsRunnersFault(t *testing.T) {
	want := `{"error":{"code":"store_error","message":"The local runner could not complete this request.","retryable":false}}` + "\n"
	for _, c := range []struct {
		name, covers string
		deadline     time.Duration
	}{
		{"a Runtime that could not start", "", 0},
		{"a Runtime killed after it wrote that the lock covers the pack", "printf '%s' '" + covered + "'; kill -9 $$", 0},
		{"a Runtime still running at the deadline", "exec sleep 60", 2 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := journalConfig(t)
			log := filepath.Join(t.TempDir(), "invocations.log")
			// journalConfig's Runtime is a text file, which cannot start.
			if c.covers != "" {
				cfg.Runtime = filepath.Join(t.TempDir(), "covers-runtime")
				script := "#!/bin/sh\nPATH=/usr/bin:/bin\nexport PATH\nprintf '%s\\n' \"$*\" >> '" + log + "'\ncase \"$1 $2\" in\n'packs covers') " + c.covers + " ;;\nesac\nexit 9\n"
				must(t, os.WriteFile(cfg.Runtime, []byte(script), 0700))
			}
			s := openJournal(t, cfg)
			t.Cleanup(s.Close)
			req := httptest.NewRequest("POST", "/v1/previews", strings.NewReader(reviewedRequest(&ReviewedSetRequest{Lock: suppliedLock, ID: "journal"})))
			req.Header.Set("Authorization", "Bearer "+journalBearer)
			if c.deadline > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), c.deadline)
				defer cancel()
				req = req.WithContext(ctx)
			}
			res := httptest.NewRecorder()
			s.Handler(journalBearer).ServeHTTP(res, req)
			if res.Code != 500 || res.Body.String() != want {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if n := releasesHeld(t, s); n != 0 {
				t.Fatal("a failed check saved a release:", n)
			}
			if got := takeLog(t, log); c.covers != "" && got != coversCall+"\n" {
				t.Fatalf("the Runtime was invoked\n%s", got)
			}
		})
	}
}

// Under manual admission the executor admits no packs covers invocation, and a
// release check that supplies a reviewed set is refused as every feature the
// manual profile lacks is, before any Runtime is invoked.
func TestReviewedSetRefusedUnderManualAdmission(t *testing.T) {
	s, log := checkService(t)
	s.cfg.ExecutorAdmission = &ManualAdmissionBinding{}
	answerCovers(t, log, covered, 0)
	res := serve(t, s, "POST", "/v1/previews", reviewedRequest(&ReviewedSetRequest{Lock: suppliedLock, ID: "journal"}), nil)
	want := `{"error":{"code":"manual_profile_unsupported","message":"This profile requires an exact manual single-Pack job without triggers, mappings or workflow input.","retryable":false}}` + "\n"
	if res.Code != 409 || res.Body.String() != want {
		t.Fatalf("answered %d %s", res.Code, res.Body.String())
	}
	if got := takeLog(t, log); got != "" {
		t.Fatalf("the Runtime was invoked\n%s", got)
	}
}

// heldReleaseWith saves a release of the journal pack, with the release's own
// configuration and lock, held to set.
func heldReleaseWith(t *testing.T, s *Service, set *SuppliedReviewedSet) Release {
	t.Helper()
	r := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: sha(journalPack), PackID: "journal", PackVersion: "1.0.0", Title: "Journal", RuntimeDigest: s.runtimeDigest, CreatedAt: now(), Config: configWithout, Lock: checkLock, Sample: journalInput(), Tests: "not-run", SuppliedReviewedSet: set}
	must(t, s.saveRelease(r))
	return r
}

// Each attempt of a run of a release held to a supplied reviewed set checks,
// by digest, that the release still holds the lock it was checked against,
// and is not evaluated when it does not.
func TestEachAttemptChecksTheSuppliedLockByItsDigest(t *testing.T) {
	s, log := checkService(t)
	changed := heldReleaseWith(t, s, &SuppliedReviewedSet{ID: "journal", Lock: suppliedLock + " ", LockDigest: sha(suppliedLock)})
	j, e := s.createJob("Changed", changed.ID)
	must(t, e)
	run := runOnce(t, s, j.ID, "changed")
	if run.State != "failed" || run.Problem != "release reviewed-set lock digest mismatch" {
		t.Fatal("the run of a changed lock ended", run.State, run.Problem)
	}
	if got := takeLog(t, log); got != "" {
		t.Fatalf("the run of a changed lock invoked\n%s", got)
	}
	kept := heldReleaseWith(t, s, &SuppliedReviewedSet{ID: "journal", Lock: suppliedLock, LockDigest: sha(suppliedLock)})
	j, e = s.createJob("Kept", kept.ID)
	must(t, e)
	if run := runOnce(t, s, j.ID, "kept"); run.State != "completed" {
		t.Fatal("the run of a kept lock ended", run.State, run.Problem)
	}
}

// Manual admission checks a release itself before it binds, launches or
// admits a run of it (manualRelease), and so also that a release checked
// against a supplied reviewed set before the installation admitted manually
// still holds that lock.
func TestManualAdmissionChecksTheSuppliedLockByItsDigest(t *testing.T) {
	pack := `{"id":"manual","version":"1.0.0","specVersion":"0.2.0-draft"}`
	release := func(set *SuppliedReviewedSet) Release {
		return Release{SchemaVersion: "1", Pack: pack, PackDigest: sha(pack), PackID: "manual", PackVersion: "1.0.0", RuntimeDigest: sha("runtime"), Tests: "not-run", SuppliedReviewedSet: set}
	}
	for name, c := range map[string]struct {
		set  *SuppliedReviewedSet
		want bool
	}{
		"no supplied reviewed set":        {nil, true},
		"a kept lock":                     {&SuppliedReviewedSet{ID: "journal", Lock: suppliedLock, LockDigest: sha(suppliedLock)}, true},
		"a lock that is not its digest's": {&SuppliedReviewedSet{ID: "journal", Lock: suppliedLock + " ", LockDigest: sha(suppliedLock)}, false},
		"an id not of its form":           {&SuppliedReviewedSet{ID: "-journal", Lock: suppliedLock, LockDigest: sha(suppliedLock)}, false},
	} {
		if got := manualRelease(release(c.set)); got != c.want {
			t.Errorf("%s: manualRelease answered %v", name, got)
		}
	}
}

// A run's reviewed claim rests on the supplied lock exactly when its release
// was checked against one; a run that makes no claim says nothing of either.
// A run whose release cannot be read, or whose release's stored lock is no
// longer the bytes its digest names, is not served as resting on either lock.
func TestRunsOfAReleaseHeldToASuppliedReviewedSetSaySo(t *testing.T) {
	set := &SuppliedReviewedSet{ID: "journal", Lock: suppliedLock, LockDigest: sha(suppliedLock)}
	claim := Run{Result: json.RawMessage(`{"reviewed":true}`)}
	for _, c := range []struct {
		name           string
		run            Run
		release        Release
		origin, digest string
	}{
		{"a claim, of a release held to a supplied set", claim, Release{SuppliedReviewedSet: set}, "supplied", set.LockDigest},
		{"a record's claim, of a release held to a supplied set", Run{Audit: json.RawMessage(`{"reviewed":true}`)}, Release{SuppliedReviewedSet: set}, "supplied", set.LockDigest},
		{"a claim, of a release without one", claim, Release{}, "release", ""},
		{"no claim, of a release held to a supplied set", Run{Result: json.RawMessage(`{"reviewed":false}`)}, Release{SuppliedReviewedSet: set}, "", ""},
	} {
		if got := servedRun(c.run, c.release); got.ReviewedSetOrigin != c.origin || got.SuppliedLockDigest != c.digest {
			t.Fatalf("%s: %q %q", c.name, got.ReviewedSetOrigin, got.SuppliedLockDigest)
		}
	}

	s, _ := checkService(t)
	r := heldReleaseWith(t, s, set)
	j, e := s.createJob("Unreadable", r.ID)
	must(t, e)
	run := runOnce(t, s, j.ID, "unreadable")
	if run.State != "completed" {
		t.Fatal("the run ended", run.State, run.Problem)
	}
	want := `{"error":{"code":"store_error","message":"The local runner could not complete this request.","retryable":false}}` + "\n"
	if res := serve(t, s, "GET", "/v1/runs/"+run.ID, "", nil); res.Code != 200 || !strings.Contains(res.Body.String(), `"reviewedSetOrigin":"supplied","suppliedLockDigest":"`+set.LockDigest+`"`) {
		t.Fatalf("the run of a kept lock was served %d\n%s", res.Code, res.Body.String())
	}
	altered := r
	altered.SuppliedReviewedSet = &SuppliedReviewedSet{ID: set.ID, Lock: set.Lock + " ", LockDigest: set.LockDigest}
	if _, e := s.db.Exec("UPDATE releases SET record=? WHERE id=?", string(encode(altered)), r.ID); e != nil {
		t.Fatal(e)
	}
	for name, res := range map[string]*httptest.ResponseRecorder{
		"GET /v1/runs/{run}":                     serve(t, s, "GET", "/v1/runs/"+run.ID, "", nil),
		"POST /v1/jobs/{job}/runs, a repetition": serve(t, s, "POST", "/v1/jobs/"+j.ID+"/runs", string(encode(journalInput())), map[string]string{"Idempotency-Key": "unreadable"}),
	} {
		if res.Code != 500 || res.Body.String() != want {
			t.Fatalf("%s, of a release whose stored lock is not its digest's, served %d\n%s", name, res.Code, res.Body.String())
		}
	}
	if _, e := s.db.Exec("UPDATE releases SET record=? WHERE id=?", "{", r.ID); e != nil {
		t.Fatal(e)
	}
	for name, res := range map[string]interface{ String() string }{
		"GET /v1/runs/{run}":                     serve(t, s, "GET", "/v1/runs/"+run.ID, "", nil).Body,
		"POST /v1/jobs/{job}/runs, a repetition": serve(t, s, "POST", "/v1/jobs/"+j.ID+"/runs", string(encode(journalInput())), map[string]string{"Idempotency-Key": "unreadable"}).Body,
	} {
		if res.String() != want {
			t.Fatalf("%s, of a release that cannot be read, served\n%s", name, res.String())
		}
	}
}

// An installation booted with requireSuppliedReviewedSet creates and upgrades
// jobs only from releases checked against a supplied reviewed set whose lock
// is still the bytes its digest names, refusing every other with these bytes.
// A release checked before Runner #210, stored as a Runner then stored it,
// still reads and runs: a job made from it before is returned again, keeps
// running, and its runs' claims rest on the release's own lock.
func TestBootRequirementHoldsJobsToReleasesCheckedAgainstASuppliedReviewedSet(t *testing.T) {
	s, log := checkService(t)
	// A release as a Runner before #210 stored it, written out by hand.
	old := "release_checkedbefore210"
	record := `{"schemaVersion":"1","id":"` + old + `","pack":` + string(encode(journalPack)) + `,"packDigest":"` + sha(journalPack) + `","packId":"journal","packVersion":"1.0.0","title":"Journal","runtimeDigest":"` + s.runtimeDigest + `","createdAt":"2026-10-01T00:00:00Z","config":` + string(encode(configWithout)) + `,"lock":` + string(encode(checkLock)) + `,"validation":{"outputVersion":"2","status":"valid"},"sample":{"facts":{"request":{"marker":"facts-never-journaled-7f3a"}}},"preview":{"outputVersion":"2","status":"evaluated"},"tests":"not-run"}`
	if _, e := s.db.Exec("INSERT INTO releases(id,record) VALUES(?,?)", old, record); e != nil {
		t.Fatal(e)
	}
	if held, e := s.release(old); e != nil || held.SuppliedReviewedSet != nil || held.Lock != checkLock {
		t.Fatal("an earlier release reads as", held, e)
	}
	before, e := s.createJob("Before", old)
	must(t, e)

	s.cfg.RequireSuppliedReviewedSet = true
	again, e := s.createJob("Before", old)
	if e != nil || again != before {
		t.Fatal("repeating the creation of a job made before:", again, e)
	}
	run := runOnce(t, s, before.ID, "after")
	if run.State != "completed" {
		t.Fatal("a job made before stopped running:", run.State, run.Problem)
	}
	if got, want := serve(t, s, "GET", "/v1/runs/"+run.ID, "", nil).Body.String(), strings.TrimSuffix(string(encode(served(run))), "}")+`,"reviewedSetOrigin":"release"}`+"\n"; got != want {
		t.Fatalf("a run of an earlier release was served\n%s\nnot\n%s", got, want)
	}
	takeLog(t, log)

	createRefusal := `{"error":{"code":"release_reviewed_set_required","message":"This installation creates jobs only from releases checked against a supplied reviewed-set lock. Check a new release with reviewedSet.","retryable":false}}` + "\n"
	upgradeRefusal := `{"error":{"code":"release_reviewed_set_required","message":"This installation upgrades jobs only to releases checked against a supplied reviewed-set lock. Check a new release with reviewedSet.","retryable":false}}` + "\n"
	for name, set := range map[string]*SuppliedReviewedSet{
		"none":                               {},
		"a lock a byte away from its digest": {ID: "journal", Lock: suppliedLock + " ", LockDigest: sha(suppliedLock)},
		"an empty lock with its digest":      {ID: "journal", Lock: "", LockDigest: sha("")},
		"an id that is not a decision id":    {ID: "Journal", Lock: suppliedLock, LockDigest: sha(suppliedLock)},
		"a digest of another form":           {ID: "journal", Lock: suppliedLock, LockDigest: strings.TrimPrefix(sha(suppliedLock), "sha256:")},
	} {
		if name == "none" {
			set = nil
		}
		r := heldReleaseWith(t, s, set)
		res := serve(t, s, "POST", "/v1/jobs", `{"name":"Refused","releaseId":"`+r.ID+`","reviewed":true}`, nil)
		if res.Code != 409 || res.Body.String() != createRefusal {
			t.Fatalf("%s: creating answered %d %s", name, res.Code, res.Body.String())
		}
		res = serve(t, s, "POST", "/v1/jobs/"+before.ID+"/upgrade", string(encode(UpgradeJobRequest{ReleaseID: r.ID, BaseReleaseID: old, BaseRevision: 1, Reviewed: true})), nil)
		if res.Code != 409 || res.Body.String() != upgradeRefusal {
			t.Fatalf("%s: upgrading answered %d %s", name, res.Code, res.Body.String())
		}
	}
	if held, e := s.job(before.ID); e != nil || held != before {
		t.Fatal("a refused upgrade changed the job:", held, e)
	}

	checked := checkHeld(t, s, log)
	if res := serve(t, s, "POST", "/v1/jobs", `{"name":"Held","releaseId":"`+checked.ID+`","reviewed":true}`, nil); res.Code != 201 {
		t.Fatalf("a release checked against a supplied set: %d %s", res.Code, res.Body.String())
	}
	upgradeTo := checkHeld(t, s, log)
	res := serve(t, s, "POST", "/v1/jobs/"+before.ID+"/upgrade", string(encode(UpgradeJobRequest{ReleaseID: upgradeTo.ID, BaseReleaseID: old, BaseRevision: 1, Reviewed: true})), nil)
	if res.Code != 200 {
		t.Fatalf("upgrading to a release checked against a supplied set: %d %s", res.Code, res.Body.String())
	}
}
