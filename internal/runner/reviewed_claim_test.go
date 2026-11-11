package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Runner #210: whose reviewed set a run's reviewed claim is about.

const (
	// configWithout is the configuration every release checked without
	// requireComparableFacts has, written out by hand.
	configWithout = `{"configVersion":"3","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"}}`
	// checkLock is what the fake Runtime's packs lock writes: the release's
	// own lock.
	checkLock = `{"lockVersion":"1","fake":"release-lock"}`
)

// checkRuntime is a script that answers a whole release check of the journal
// pack, and its operational runs, as the Runtime does: packs schema with
// schema, exiting schemaExit; spec validate; packs lock, which writes
// checkLock; and experimental evaluate, a rehearsal or not, an operational one
// with its audit record. Each invocation writes a line to log first: its
// arguments, then the jpack.json and the jpack.lock.json it was given, or none.
func checkRuntime(t *testing.T, schema string, schemaExit int, log string) string {
	t.Helper()
	answer := func(rehearsal string) string {
		return `{"outputVersion":"2","status":"evaluated","packId":"journal","packVersion":"1.0.0","evaluatorSpecVersion":"0.2.0-draft","rehearsal":` + rehearsal + `,"disposition":` + journalDisposition + `}`
	}
	script := `#!/bin/sh
PATH=/usr/bin:/bin
export PATH
umask 077
config=none
lock=none
if [ -f jpack.json ]; then config=$(cat jpack.json); fi
if [ -f jpack.lock.json ]; then lock=$(cat jpack.lock.json); fi
printf '%s | %s | %s\n' "$*" "$config" "$lock" >> '` + log + `'
case "$1 $2" in
'packs schema')
	printf '%s\n' '` + schema + `'
	exit ` + strconv.Itoa(schemaExit) + ` ;;
'spec validate')
	printf '%s\n' '{"outputVersion":"2","status":"valid"}' ;;
'packs lock')
	printf '%s' '` + checkLock + `' > jpack.lock.json
	printf '%s\n' '{"outputVersion":"2","status":"locked"}' ;;
'experimental evaluate')
	case " $* " in
	*' --rehearsal '*)
		printf '%s\n' '` + answer("true") + `' ;;
	*)
		mkdir -p audit
		printf '{"recordVersion":"1","kind":"evaluation","run":"fake","reviewed":true,"pack":{"digest":"%s"},"inputs":{"facts":%s,"evidenceSupplied":false},"disposition":` + journalDisposition + `}\n' '` + digest([]byte(journalPack)) + `' "$(cat facts.json)" > audit/evaluations.jsonl
		printf '%s\n' '` + answer("false") + `' ;;
	esac ;;
*)
	exit 9 ;;
esac
`
	path := filepath.Join(t.TempDir(), "check-runtime")
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return path
}

// checkService opens a journal store whose own Runtime is checkRuntime,
// reading every configVersion, and returns it with the Runtime's log.
func checkService(t *testing.T) (*Service, string) {
	return checkServiceAnswering(t, schemaReadsFive, 0)
}

// checkServiceAnswering is checkService, whose Runtime answers packs schema
// with schema and exits schemaExit.
func checkServiceAnswering(t *testing.T, schema string, schemaExit int) (*Service, string) {
	t.Helper()
	cfg := journalConfig(t)
	log := filepath.Join(t.TempDir(), "invocations.log")
	cfg.Runtime = checkRuntime(t, schema, schemaExit, log)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	return s, log
}

// runOnce submits one run of the journal input to job and evaluates it.
func runOnce(t *testing.T, s *Service, job, key string) Run {
	t.Helper()
	queued, _, e := s.submit(job, key, journalInput())
	must(t, e)
	if ran, e := s.dispatchNext(context.Background()); !ran || e != nil {
		t.Fatal("the run was not dispatched", ran, e)
	}
	r, e := s.run(queued.ID)
	must(t, e)
	return r
}

// heldRelease saves a release of the journal pack with config, for the
// Runtime pinned as runtime.
func heldRelease(t *testing.T, s *Service, config, runtime string) Release {
	t.Helper()
	r := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), PackID: "journal", PackVersion: "1.0.0", Title: "Journal", RuntimeDigest: runtime, CreatedAt: now(), Config: config, Lock: checkLock, Sample: journalInput(), Tests: "not-run"}
	must(t, s.saveRelease(r))
	return r
}

// A run whose result or audit record says reviewed is true is served with
// reviewedSetOrigin release, after every member the store keeps, by both run
// routes, as the API document says; a run that makes no claim has none. The
// store, run lists and verification exports do not carry it.
func TestRunsSayWhoseReviewedSetTheirClaimIsAbout(t *testing.T) {
	s, _ := checkService(t)
	r := heldRelease(t, s, configWithout, s.runtimeDigest)
	j, e := s.createJob("Claims", r.ID)
	must(t, e)
	body := string(encode(journalInput()))
	key := map[string]string{"Idempotency-Key": "claimed"}

	queued := serve(t, s, "POST", "/v1/jobs/"+j.ID+"/runs", body, key)
	var accepted Run
	must(t, json.Unmarshal(queued.Body.Bytes(), &accepted))
	held, e := s.run(accepted.ID)
	must(t, e)
	if queued.Code != 202 || queued.Body.String() != string(encode(served(held)))+"\n" {
		t.Fatalf("a queued run was served %d %s", queued.Code, queued.Body.String())
	}
	if ran, e := s.dispatchNext(context.Background()); !ran || e != nil {
		t.Fatal("the run was not dispatched", ran, e)
	}
	held, e = s.run(accepted.ID)
	must(t, e)
	if held.State != "completed" {
		t.Fatal("the run ended", held.State, held.Problem)
	}
	want := strings.TrimSuffix(string(encode(served(held))), "}") + `,"reviewedSetOrigin":"release"}` + "\n"
	doc := readOpenAPI(t)
	if described, _ := doc.schema("Run")["properties"].(map[string]any)["reviewedSetOrigin"].(map[string]any); described == nil {
		t.Fatal("the API document does not describe reviewedSetOrigin")
	}
	for name, res := range map[string]interface {
		String() string
	}{
		"GET /v1/runs/{run}":                     serve(t, s, "GET", "/v1/runs/"+held.ID, "", nil).Body,
		"POST /v1/jobs/{job}/runs, a repetition": serve(t, s, "POST", "/v1/jobs/"+j.ID+"/runs", body, key).Body,
	} {
		if res.String() != want {
			t.Fatalf("%s served\n%s\nnot\n%s", name, res.String(), want)
		}
		if problems := doc.check("Run", doc.schema("Run"), decodeNumbers(t, []byte(res.String()))); len(problems) > 0 {
			t.Fatal(name, problems)
		}
	}

	var stored string
	must(t, s.db.QueryRow("SELECT record FROM runs WHERE id=?", held.ID).Scan(&stored))
	for name, b := range map[string]string{
		"the stored run":          stored,
		"the run list":            serve(t, s, "GET", "/v1/runs", "", nil).Body.String(),
		"the verification export": serve(t, s, "GET", "/v1/runs/"+held.ID+"/verification?version=5", "", nil).Body.String(),
	} {
		if !strings.Contains(b, held.ID) || strings.Contains(b, "reviewedSetOrigin") {
			t.Fatalf("%s: %s", name, b)
		}
	}

	failing := heldRelease(t, s, configWithout, failingRuntime(t, s.cfg.Dir))
	fj, e := s.createJob("Failing", failing.ID)
	must(t, e)
	failed := runOnce(t, s, fj.ID, "failed")
	if failed.State != "failed" {
		t.Fatal("the failing run ended", failed.State)
	}
	if res := serve(t, s, "GET", "/v1/runs/"+failed.ID, "", nil).Body.String(); res != string(encode(served(failed)))+"\n" {
		t.Fatal("a run that makes no claim was served", res)
	}
	for _, c := range []struct {
		name string
		run  Run
		want string
	}{
		{"a result that says reviewed is true", Run{Result: json.RawMessage(`{"reviewed":true}`)}, "release"},
		{"a record that says reviewed is true", Run{Audit: json.RawMessage(`{"reviewed":true}`)}, "release"},
		{"a result that says reviewed is false", Run{Result: json.RawMessage(`{"reviewed":false}`), Audit: json.RawMessage(`{"reviewed":false}`)}, ""},
		{"a record whose reviewed is a string", Run{Audit: json.RawMessage(`{"reviewed":"true"}`)}, ""},
		{"no claim", Run{Result: json.RawMessage(`{"status":"evaluated"}`)}, ""},
	} {
		if got := reviewedSetOrigin(c.run); got != c.want {
			t.Fatalf("%s: %q", c.name, got)
		}
	}
}
