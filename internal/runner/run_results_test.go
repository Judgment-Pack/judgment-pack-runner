package runner

import (
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// resultMembersSQL is a SQL test that an entry's detail holds a member of a
// run's or an occurrence's end (run_results.go).
const resultMembersSQL = "(json_extract(detail,'$.disposition') IS NOT NULL OR json_extract(detail,'$.errorClass') IS NOT NULL OR json_extract(detail,'$.counts') IS NOT NULL)"

// entryVersionsBesideResults holds every entry that carries a result member to
// resultEntryVersion, and gives the versions of every other entry.
func entryVersionsBesideResults(t *testing.T, db *sql.DB) string {
	t.Helper()
	var wrong int
	must(t, db.QueryRow("SELECT count(*) FROM events WHERE "+resultMembersSQL+" AND entry_version<>?", resultEntryVersion).Scan(&wrong))
	if wrong != 0 {
		t.Fatalf("%d entries hold a result member at another version than %s", wrong, resultEntryVersion)
	}
	var versions sql.NullString
	must(t, db.QueryRow("SELECT group_concat(DISTINCT entry_version) FROM events WHERE NOT "+resultMembersSQL).Scan(&versions))
	return versions.String
}

// wantEntryVersion is the version the journal's rule gives an entry: the
// highest whose member it holds.
func wantEntryVersion(e Event) string {
	switch {
	case e.Disposition != nil || e.ErrorClass != "" || e.Counts != nil:
		return resultEntryVersion
	case e.Attempt != nil:
		return attemptEntryVersion
	case e.Scope != "":
		return scopedEntryVersion
	case e.By.Kind == "caller":
		return callerEntryVersion
	}
	return EventEntryVersion
}

// reasonCode is the form of a reason that is a code rather than a sentence.
var reasonCode = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Every reason code the package gives a run, an occurrence or an attempt
// names exactly one class, or is a skip that names none: the table is total
// over the codes, read from the package's own source, where each is a literal
// or a constant assigned to a Reason, or passed to a function that assigns a
// parameter of its own to one (stopPreparation), at each call of it.
func TestEveryReasonCodeHasOneClass(t *testing.T) {
	files, err := filepath.Glob("*.go")
	must(t, err)
	fset := token.NewFileSet()
	constants := map[string]string{}
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		must(t, err)
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, spec := range g.Specs {
					v := spec.(*ast.ValueSpec)
					for i, n := range v.Names {
						if i < len(v.Values) {
							if lit, ok := v.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								constants[n.Name], _ = strconv.Unquote(lit.Value)
							}
						}
					}
				}
			}
		}
	}
	// The records whose reason is a run's, an occurrence's or an attempt's.
	owners := map[string]bool{"Run": true, "Occurrence": true, "Attempt": true}
	codes := map[string]string{}
	value := func(e ast.Expr) (string, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		case *ast.Ident:
			s, ok := constants[v.Name]
			return s, ok
		}
		return "", false
	}
	note := func(e ast.Expr) {
		if s, ok := value(e); ok && reasonCode.MatchString(s) {
			codes[s] = fset.Position(e.Pos()).String()
		}
	}
	// carriers are the functions that assign a parameter of their own to a
	// Reason, by name, with the parameter's place among their arguments.
	carriers := map[string]int{}
	parameter := func(fd *ast.FuncDecl, e ast.Expr) (int, bool) {
		id, ok := e.(*ast.Ident)
		if !ok {
			return 0, false
		}
		i := 0
		for _, field := range fd.Type.Params.List {
			for _, n := range field.Names {
				if n.Name == id.Name {
					return i, true
				}
				i++
			}
			if len(field.Names) == 0 {
				i++
			}
		}
		return 0, false
	}
	reason := func(fd *ast.FuncDecl, e ast.Expr) {
		if i, ok := parameter(fd, e); ok {
			carriers[fd.Name.Name] = i
			return
		}
		note(e)
	}
	var funcs []*ast.FuncDecl
	for _, f := range parsed {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				funcs = append(funcs, fd)
			}
		}
	}
	for _, fd := range funcs {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Reason" && i < len(n.Rhs) && len(n.Lhs) == len(n.Rhs) {
						reason(fd, n.Rhs[i])
					}
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && owners[id.Name] {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Reason" {
								reason(fd, kv.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
	// A reason reaches a carrier as an argument, which is noted at each call;
	// one that is in turn a parameter of its caller makes the caller a
	// carrier, until no new one is found.
	for found := true; found; {
		found = false
		for _, fd := range funcs {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				if i, ok := carriers[name]; ok && i < len(call.Args) {
					if j, ok := parameter(fd, call.Args[i]); ok {
						if _, known := carriers[fd.Name.Name]; !known {
							carriers[fd.Name.Name], found = j, true
						}
					} else {
						note(call.Args[i])
					}
				}
				return true
			})
		}
	}
	if _, ok := carriers["stopPreparation"]; !ok {
		t.Fatalf("the reason stopPreparation is given is not followed to its calls: %v", carriers)
	}
	// The codes the policy hands back as an occurrence's ending.
	codes[occurrenceAttemptLimit], codes[occurrenceWaitTooLong] = "retries.go", "retries.go"
	for code, at := range codes {
		_, failure := reasonClasses[code]
		skip := slices.Contains(notFailureReasons, code)
		if failure == skip {
			t.Errorf("the reason code %q (%s) is in %d of the class table and the skips, want exactly one", code, at, map[bool]int{true: 1}[failure]+map[bool]int{true: 1}[skip])
		}
	}
	for code, class := range reasonClasses {
		if !slices.Contains(errorClasses, class) {
			t.Errorf("the reason code %q names %q, which is not a class", code, class)
		}
		if _, assigned := codes[code]; !assigned {
			t.Errorf("the class table names %q, which no record is given", code)
		}
	}
	for _, code := range notFailureReasons {
		if _, assigned := codes[code]; !assigned {
			t.Errorf("the skips name %q, which no record is given", code)
		}
	}
	// The table's own lists stay as the record names them.
	if !slices.Equal(errorClasses, []string{"source", "preparation", "evaluation", "configuration", "limit", "interrupted", "internal"}) {
		t.Fatalf("the classes are %v", errorClasses)
	}
	doc := readOpenAPI(t)
	var documented []string
	for _, c := range doc.schema("ErrorClass")["enum"].([]any) {
		documented = append(documented, c.(string))
	}
	if !slices.Equal(documented, errorClasses) {
		t.Fatalf("the document lists the classes %v, Runner gives %v", documented, errorClasses)
	}
}

// Counts are what a run's record holds once it ended, and nothing else; the
// class is its writer's, else its reason's, else interrupted's.
func TestCountsAreWhatTheRecordHolds(t *testing.T) {
	two := 2
	result := func(disposition string) json.RawMessage {
		return json.RawMessage(`{"outputVersion":"2","disposition":` + disposition + `}`)
	}
	prepared := Input{Facts: journalFacts, Preparation: &Preparation{Outcomes: []SourceOutcome{{Name: "case"}, {Name: "lookup"}}}}
	for _, c := range []struct {
		name  string
		run   Run
		want  string
		class string
	}{
		{"an outcome", Run{State: "completed", Result: result(`{"kind":"outcome","outcomeId":"proceed","reasons":[],"handoff":{"state":"none"}}`)},
			`{"decisions":1,"byKind":{"outcome":1},"byOutcome":{"proceed":1}}`, ""},
		{"an unresolved decision is a decision, not a failure", Run{State: "completed", Result: result(`{"kind":"unresolved","reasons":["unknown"],"handoff":{"state":"requested"}}`)},
			`{"decisions":1,"byKind":{"unresolved":1},"byOutcome":{}}`, ""},
		{"not applicable", Run{State: "completed", Result: result(`{"kind":"not-applicable"}`)},
			`{"decisions":1,"byKind":{"not-applicable":1},"byOutcome":{}}`, ""},
		{"a kind that is not a string", Run{State: "completed", Result: result(`{"kind":7,"outcomeId":"proceed"}`)},
			`{"decisions":1,"byKind":{},"byOutcome":{}}`, ""},
		{"an outcome without an identifier", Run{State: "completed", Result: result(`{"kind":"outcome"}`)},
			`{"decisions":1,"byKind":{"outcome":1},"byOutcome":{}}`, ""},
		{"a prepared input's sources", Run{State: "completed", Input: prepared, Result: result(`{"kind":"outcome","outcomeId":"decline"}`)},
			`{"sources":2,"decisions":1,"byKind":{"outcome":1},"byOutcome":{"decline":1}}`, ""},
		{"a failure", Run{State: "failed", Input: prepared, ErrorClass: classEvaluation},
			`{"sources":2,"decisions":0,"byKind":{},"byOutcome":{}}`, classEvaluation},
		{"a failure with a reason code", Run{State: "failed", Reason: runProfileSetAside},
			`{"decisions":0,"byKind":{},"byOutcome":{}}`, classConfiguration},
		{"a failed run's result is not a decision", Run{State: "failed", Result: result(`{"kind":"outcome","outcomeId":"proceed"}`), ErrorClass: classEvaluation},
			`{"decisions":0,"byKind":{},"byOutcome":{}}`, classEvaluation},
		{"an interruption", Run{State: "interrupted"},
			`{"decisions":0,"byKind":{},"byOutcome":{}}`, classInterrupted},
	} {
		got := withResults(c.run)
		if string(encode(got.Counts)) != c.want || got.ErrorClass != c.class {
			t.Errorf("%s: %s %q, want %s %q", c.name, encode(got.Counts), got.ErrorClass, c.want, c.class)
		}
	}
	for _, state := range []string{"queued", "running"} {
		if got := withResults(Run{State: state, Counts: &RunCounts{Sources: &two}, ErrorClass: classLimit}); got.Counts != nil || got.ErrorClass != "" {
			t.Errorf("a %s run holds %+v %q", state, got.Counts, got.ErrorClass)
		}
	}
	if got := withResults(Run{State: "completed", ErrorClass: classEvaluation, Result: result(`{"kind":"outcome","outcomeId":"proceed"}`)}); got.ErrorClass != "" {
		t.Error("a completed run keeps an error class")
	}
	for state, class := range map[string]string{"failed": classInterrupted, "expired": classLimit, "skipped": "", "accepted": "", "needs-attention": "", "cancelled": ""} {
		o := Occurrence{State: state, Reason: runQueueExpired}
		if state == "failed" {
			o.ErrorClass = classInterrupted
		}
		if state == "skipped" {
			o.Reason = "overlap"
		}
		if got := withOccurrenceClass(o); got.ErrorClass != class {
			t.Errorf("an occurrence %s has class %q, want %q", state, got.ErrorClass, class)
		}
	}
	if acquisitionClass(&apiError{502, "gateway_unavailable", ""}) != classSource || acquisitionClass(&apiError{422, "gateway_connection_required", ""}) != classConfiguration || acquisitionClass(&apiError{422, "invalid_input", ""}) != classPreparation {
		t.Error("an acquisition's failure is not classed by its code")
	}
	if submissionClass(&apiError{503, "dispatcher_stopped", ""}) != classInternal || submissionClass(&apiError{422, "invalid_input", ""}) != classPreparation {
		t.Error("a submission's refusal is not classed by its status")
	}
}

// errorClassesHeld gives, for each class, where a record or an entry carries it.
func errorClassesHeld(t *testing.T, s *Service, cfg Config) map[string][]string {
	t.Helper()
	seen := map[string][]string{}
	for _, table := range []string{"runs", "occurrences"} {
		rows, e := s.db.Query("SELECT id, json_extract(record,'$.errorClass'), state FROM " + table + " WHERE json_extract(record,'$.errorClass') IS NOT NULL")
		must(t, e)
		for rows.Next() {
			var id, class, state string
			must(t, rows.Scan(&id, &class, &state))
			seen[class] = append(seen[class], table+" "+state)
			if state != "failed" && state != "interrupted" && state != "expired" {
				t.Errorf("%s %s is %s and holds the class %s", table, id, state, class)
			}
		}
		must(t, rows.Err())
		rows.Close()
	}
	for _, e := range journalEntries(t, cfg) {
		if e.ErrorClass != "" {
			seen[e.ErrorClass] = append(seen[e.ErrorClass], e.Kind)
			if !slices.Contains([]string{"run.failed", "run.expired", "run.interrupted", "occurrence.failed", "occurrence.expired"}, e.Kind) {
				t.Errorf("a %s entry holds the class %s", e.Kind, e.ErrorClass)
			}
		}
		if e.EntryVersion != wantEntryVersion(e) {
			t.Errorf("a %s entry is at version %s", e.Kind, e.EntryVersion)
		}
	}
	return seen
}

// acquiringWorld is a retry world whose acquisitions are not evaluated: it
// needs no Runtime.
func acquiringWorld(t *testing.T, policy *RetryPolicy, answer func(n int) (int, string)) *retryWorld {
	t.Helper()
	return newRetryWorldOf(t, journalConfig(t), func(t *testing.T, s *Service) Release {
		return journalRelease(t, s, completingRuntime(t, s.cfg.Dir))
	}, policy, answer)
}

// Each class is produced by a change Runner makes (the acceptance of #239):
// the journal's walk through every change of state makes five, a failed
// acquisition through a Gateway connection makes source, and a release
// that cannot be read at dispatch makes internal. Each is carried by its
// record and by its entry, and by no record or entry that did not fail.
func TestEachErrorClassIsProduced(t *testing.T) {
	cfg := journalConfig(t)
	w := runJournalSteps(t, cfg, false)

	// A release that cannot be read when its run is dispatched is Runner's
	// own fault.
	release := journalRelease(t, w.s, completingRuntime(t, cfg.Dir))
	j, e := w.s.createJob("Unreadable release", release.ID)
	must(t, e)
	r, _, e := w.s.submit(j.ID, "internal", journalInput())
	must(t, e)
	_, e = w.s.db.Exec("UPDATE releases SET record='{' WHERE id=?", release.ID)
	must(t, e)
	// Runs the steps left queued are served first, in the queue's order.
	for i := 0; i < 10; i++ {
		if r, e = w.s.run(r.ID); e != nil || r.State != "queued" {
			break
		}
		_, e = w.s.dispatchNext(t.Context())
		must(t, e)
	}
	if r, e = w.s.run(r.ID); e != nil || r.State != "failed" || r.ErrorClass != classInternal || r.Counts == nil || r.Counts.Decisions != 0 {
		t.Fatalf("a run whose release cannot be read: %+v %v", r, e)
	}
	seen := errorClassesHeld(t, w.s, cfg)

	// A source acquisition the gateway refuses, not retried.
	source := acquiringWorld(t, nil, func(int) (int, string) { return 400, "" })
	if o := source.step(t); o.State != "failed" || o.ErrorClass != classSource {
		t.Fatalf("a refused acquisition: %+v", o)
	}
	for class, where := range errorClassesHeld(t, source.s, source.cfg) {
		seen[class] = append(seen[class], where...)
	}
	// An acquisition whose policy's attempts ran out ends with its code's
	// class.
	limited := acquiringWorld(t, &RetryPolicy{MaxAttempts: 2, BackoffSeconds: 1, MaxBackoffSeconds: 1}, func(int) (int, string) { return 503, "" })
	limited.step(t)
	limited.advance(2 * time.Second)
	if o := limited.step(t); o.State != "failed" || o.Reason != occurrenceAttemptLimit || o.ErrorClass != classLimit {
		t.Fatalf("an acquisition at its limit: %+v", o)
	}
	if entries := occurrenceEntries(t, limited.s, limited.o.ID); entries[len(entries)-1].ErrorClass != classLimit || entries[len(entries)-1].EntryVersion != resultEntryVersion {
		t.Fatalf("its entry: %+v", entries[len(entries)-1])
	}

	for _, class := range errorClasses {
		if len(seen[class]) == 0 {
			t.Errorf("no change produced the class %s", class)
		}
	}
	t.Logf("classes produced: %v", seen)
}

// resultsWorld is a store whose runs a test ends itself, of jobs in scopes a
// and b and in none.
type resultsWorld struct {
	cfg     Config
	s       *Service
	release Release
	a, b, c Job
	n       int
}

func newResultsWorld(t *testing.T) *resultsWorld {
	t.Helper()
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	runtime := completingRuntime(t, cfg.Dir)
	w := &resultsWorld{cfg: cfg, s: s}
	var e error
	for _, j := range []struct {
		job         *Job
		name, scope string
	}{{&w.a, "Alpha", "a"}, {&w.b, "Bravo", "b"}, {&w.c, "Charlie", ""}} {
		// A release makes one job.
		w.release = journalRelease(t, s, runtime)
		*j.job, e = s.createJobBy(requester{}, j.name, w.release.ID, j.scope, nil)
		must(t, e)
	}
	return w
}

// end queues a run of j and records its end: completed with disposition, or
// failed or interrupted with class, and reason when it is not "", and with a
// result of disposition when it is not "".
func (w *resultsWorld) end(t *testing.T, j Job, state, disposition, class, reason string) Run {
	t.Helper()
	w.n++
	r, _, e := w.s.submit(j.ID, "run-"+strconv.Itoa(w.n), journalInput())
	must(t, e)
	if reason == runQueueExpired {
		// A run that ends in the queue was never claimed.
		r.FinishedAt, r.State, r.Problem, r.Reason = now(), state, "Expired.", reason
		must(t, w.s.changeRun(r))
		r, e = w.s.run(r.ID)
		must(t, e)
		return r
	}
	r.StartedAt, r.State, r.Attempt = now(), "running", 1
	must(t, w.s.changeRun(r))
	r.FinishedAt, r.State = now(), state
	if disposition != "" {
		// An evaluation whose output did not pass Runner's checks fails with
		// the output it wrote.
		r.Result = json.RawMessage(`{"outputVersion":"2","status":"evaluated","packId":"journal","packVersion":"1.0.0","evaluatorSpecVersion":"0.2.0-draft","rehearsal":false,"disposition":` + disposition + `}`)
	}
	switch state {
	case "completed":
		r.AuditBytes = []byte(`{"recordVersion":"1","run":"` + r.ID + `"}`)
	case "interrupted":
		r.InterruptedAt, r.Problem = r.FinishedAt, "Stopped."
	default:
		r.Problem, r.ErrorClass, r.Reason = "Failed.", class, reason
	}
	must(t, w.s.finishRun(r))
	r, e = w.s.run(r.ID)
	must(t, e)
	return r
}

// old makes a run's record and its entries what a Runner before this change
// wrote: no counts, no class, no disposition on the entry, and the entry's
// version without them.
func (w *resultsWorld) old(t *testing.T, r Run) {
	t.Helper()
	_, e := w.s.db.Exec("UPDATE runs SET record=json_remove(record,'$.counts','$.errorClass') WHERE id=?", r.ID)
	must(t, e)
	_, e = w.s.db.Exec("UPDATE events SET detail=json_remove(detail,'$.counts','$.errorClass','$.disposition'), entry_version=CASE WHEN json_extract(detail,'$.scope') IS NOT NULL THEN '3' ELSE '1' END WHERE json_extract(concerns,'$.run')=?", r.ID)
	must(t, e)
}

// listedRuns are the runs a listing answers, by id, and its next cursor.
func listedRuns(t *testing.T, s *Service, path string) ([]string, int64, []map[string]json.RawMessage) {
	t.Helper()
	rec := serve(t, s, "GET", path, "", nil)
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
		Next  int64                        `json:"next"`
	}
	must(t, json.Unmarshal(rec.Body.Bytes(), &page))
	ids := []string{}
	for _, item := range page.Items {
		var id string
		must(t, json.Unmarshal(item["id"], &id))
		ids = append(ids, id)
	}
	return ids, page.Next, page.Items
}

// journalRuns are the runs the entries a journal route answers concern, with
// each entry's kind, and its page.
func journalRuns(t *testing.T, s *Service, path string) ([]string, EventPage) {
	t.Helper()
	rec := serve(t, s, "GET", path, "", nil)
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	var page EventPage
	must(t, json.Unmarshal(rec.Body.Bytes(), &page))
	out := []string{}
	for _, e := range page.Items {
		out = append(out, e.Kind+" "+e.Concerns.Run)
	}
	return out, page
}

func ids(runs ...Run) []string {
	out := []string{}
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

func ended(kind string, runs ...Run) []string {
	out := []string{}
	for _, r := range runs {
		out = append(out, kind+" "+r.ID)
	}
	return out
}

// The runs listings keep the runs of the outcomes and classes named, any of
// each, and AND them with every other filter, the scope among them; a run
// that ended before this change is matched by the disposition its result has
// always held, and by no class.
func TestTheRunsListingsFilterByOutcomeAndErrorClass(t *testing.T) {
	w := newResultsWorld(t)
	a1 := w.end(t, w.a, "completed", journalDisposition, "", "")
	a2 := w.end(t, w.a, "completed", `{"kind":"outcome","outcomeId":"decline","reasons":[],"handoff":{"state":"none"}}`, "", "")
	a3 := w.end(t, w.a, "completed", `{"kind":"unresolved","reasons":["unknown"],"handoff":{"state":"requested"}}`, "", "")
	a4 := w.end(t, w.a, "failed", "", classEvaluation, "")
	a5 := w.end(t, w.a, "interrupted", "", "", "")
	b1 := w.end(t, w.b, "completed", journalDisposition, "", "")
	b2 := w.end(t, w.b, "failed", "", "", runQueueExpired)
	c1 := w.end(t, w.c, "completed", journalDisposition, "", "")
	c2 := w.end(t, w.c, "failed", "", classPreparation, "")
	c3 := w.end(t, w.c, "completed", journalDisposition, "", "")
	c4 := w.end(t, w.c, "failed", "", classPreparation, "")
	c5 := w.end(t, w.c, "failed", journalDisposition, classEvaluation, "")
	w.old(t, c3)
	w.old(t, c4)
	queued, _, e := w.s.submit(w.c.ID, "queued", journalInput())
	must(t, e)
	if a5.ErrorClass != classInterrupted || b2.ErrorClass != classLimit || a3.ErrorClass != "" || a1.Counts == nil || a4.Counts == nil || queued.Counts != nil {
		t.Fatalf("the ends are not as written: %+v %+v %+v", a5, b2, a3)
	}
	for _, c := range []struct {
		path string
		want []string
	}{
		{"/v1/runs?outcome=proceed", ids(c3, c1, b1, a1)},
		{"/v1/runs?outcome=proceed&outcome=decline", ids(c3, c1, b1, a2, a1)},
		{"/v1/runs?outcome=Proceed", ids()},
		{"/v1/runs?outcome=proceed%20", ids()},
		{"/v1/runs?outcome=never-given", ids()},
		{"/v1/runs?outcome=unresolved", ids()},
		{"/v1/runs?errorClass=evaluation", ids(c5, a4)},
		{"/v1/runs?errorClass=interrupted", ids(a5)},
		{"/v1/runs?errorClass=limit&errorClass=preparation", ids(c2, b2)},
		{"/v1/runs?errorClass=source", ids()},
		{"/v1/runs?outcome=proceed&errorClass=evaluation", ids()},
		{"/v1/runs?outcome=proceed&state=completed", ids(c3, c1, b1, a1)},
		{"/v1/runs?outcome=proceed&state=failed", ids()},
		{"/v1/runs?outcome=proceed&errorClass=evaluation&state=failed", ids()},
		{"/v1/runs?errorClass=evaluation&state=failed", ids(c5, a4)},
		{"/v1/runs?errorClass=evaluation&review=true", ids(c5, a4)},
		{"/v1/runs?outcome=proceed&review=true", ids()},
		{"/v1/runs?outcome=proceed&q=Bravo", ids(b1)},
		{"/v1/runs?outcome=proceed&scope=a", ids(a1)},
		{"/v1/runs?outcome=proceed&outcome=decline&scope=a", ids(a2, a1)},
		{"/v1/runs?outcome=proceed&scope=b", ids(b1)},
		{"/v1/runs?errorClass=evaluation&scope=b", ids()},
		{"/v1/runs?errorClass=limit&scope=a", ids()},
		{"/v1/runs?errorClass=limit&scope=b", ids(b2)},
		{"/v1/runs?outcome=proceed&scope=c", ids()},
		{"/v1/jobs/" + w.a.ID + "/runs?outcome=proceed&scope=a", ids(a1)},
		{"/v1/jobs/" + w.a.ID + "/runs?errorClass=interrupted&errorClass=evaluation&scope=a", ids(a5, a4)},
		{"/v1/jobs/" + w.c.ID + "/runs?outcome=proceed", ids(c3, c1)},
		{"/v1/jobs/" + w.c.ID + "/runs?errorClass=preparation", ids(c2)},
		{"/v1/jobs/" + w.c.ID + "/runs", ids(queued, c5, c4, c3, c2, c1)},
	} {
		if got, next, _ := listedRuns(t, w.s, c.path); !slices.Equal(got, c.want) || next != 0 {
			t.Errorf("%s: %v next %d, want %v", c.path, got, next, c.want)
		}
	}
	// What a listing serves of each: the counts and the class of a run that
	// ended since, and neither of one before, or of one not yet ended.
	_, _, items := listedRuns(t, w.s, "/v1/jobs/"+w.c.ID+"/runs")
	for i, want := range []string{"", `"evaluation" {"decisions":0,"byKind":{},"byOutcome":{}}`, "", "", `"preparation" {"decisions":0,"byKind":{},"byOutcome":{}}`, `"" {"decisions":1,"byKind":{"outcome":1},"byOutcome":{"proceed":1}}`} {
		got := ""
		if items[i]["counts"] != nil {
			got = string(items[i]["errorClass"])
			if got == "" {
				got = `""`
			}
			got += " " + string(items[i]["counts"])
		} else if items[i]["errorClass"] != nil {
			got = "a class without counts"
		}
		if got != want {
			t.Errorf("item %d holds %s, want %s", i, got, want)
		}
	}
	// A run's own route serves both members, and the verification export,
	// whose readers decode strictly, carries neither.
	for _, r := range []Run{a1, a4} {
		rec := serve(t, w.s, "GET", "/v1/runs/"+r.ID+"?scope=a", "", nil)
		var served map[string]json.RawMessage
		must(t, json.Unmarshal(rec.Body.Bytes(), &served))
		if rec.Code != 200 || served["counts"] == nil || (r.State == "failed") != (served["errorClass"] != nil) {
			t.Errorf("the run %s is served %d %s", r.ID, rec.Code, rec.Body.String())
		}
		for _, version := range []int{2, 3, 4, 5} {
			if b := encode(verificationExport(w.release, r, version)); strings.Contains(string(b), `"counts"`) || strings.Contains(string(b), `"errorClass"`) {
				t.Errorf("the verification export %d of %s carries a result member: %s", version, r.ID, b)
			}
		}
	}

	// The journal keeps the entries of the outcomes, classes and kinds
	// named, with its other filters.
	completed := "run.completed"
	for _, c := range []struct {
		path string
		want []string
	}{
		{"/v1/events?outcome=proceed", ended(completed, a1, b1, c1)},
		{"/v1/events?outcome=proceed&outcome=decline", ended(completed, a1, a2, b1, c1)},
		{"/v1/events?outcome=never-given", ended(completed)},
		{"/v1/events?errorClass=evaluation", ended("run.failed", a4, c5)},
		{"/v1/events?errorClass=interrupted", ended("run.interrupted", a5)},
		{"/v1/events?errorClass=limit", ended("run.expired", b2)},
		{"/v1/events?errorClass=preparation", ended("run.failed", c2)},
		{"/v1/events?kind=run.completed", ended(completed, a1, a2, a3, b1, c1, c3)},
		{"/v1/events?kind=run.failed&kind=run.expired", append(append(ended("run.failed", a4), ended("run.expired", b2)...), ended("run.failed", c2, c4, c5)...)},
		{"/v1/events?kind=run.completed&outcome=decline", ended(completed, a2)},
		{"/v1/events?kind=run.failed&outcome=proceed", ended(completed)},
		{"/v1/events?kind=run.completed&scope=b", ended(completed, b1)},
		{"/v1/events?outcome=proceed&scope=a", ended(completed, a1)},
		{"/v1/events?errorClass=limit&scope=a", ended(completed)},
		{"/v1/events?errorClass=limit&scope=b", ended("run.expired", b2)},
		{"/v1/events?job=" + w.a.ID + "&outcome=proceed&scope=a", ended(completed, a1)},
		{"/v1/events?job=" + w.a.ID + "&kind=run.interrupted&scope=a", ended("run.interrupted", a5)},
		{"/v1/events?job=" + w.c.ID + "&kind=runner.started", []string{"runner.started "}},
		{"/v1/events?job=" + w.c.ID + "&kind=runner.started&kind=run.completed", append([]string{"runner.started "}, ended(completed, c1, c3)...)},
		{"/v1/events?job=" + w.c.ID + "&errorClass=preparation&kind=runner.started", ended(completed)},
		{"/v1/jobs/" + w.a.ID + "/events?outcome=proceed&outcome=decline&scope=a", ended(completed, a1, a2)},
		{"/v1/jobs/" + w.a.ID + "/events?kind=run.completed&scope=a", ended(completed, a1, a2, a3)},
		{"/v1/jobs/" + w.c.ID + "/events?errorClass=preparation", ended("run.failed", c2)},
		{"/v1/jobs/" + w.c.ID + "/events?kind=job.created", []string{"job.created "}},
		{"/v1/jobs/" + w.c.ID + "/events?kind=run.completed&tail=true", ended(completed, c1, c3)},
	} {
		if got, _ := journalRuns(t, w.s, c.path); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.path, got, c.want)
		}
	}
	// A job of another scope is not found under a filter, as without one.
	for _, path := range []string{"/v1/events?job=" + w.b.ID + "&outcome=proceed&scope=a", "/v1/jobs/" + w.b.ID + "/runs?outcome=proceed&scope=a", "/v1/jobs/" + w.b.ID + "/events?kind=run.completed&scope=a"} {
		if rec := serve(t, w.s, "GET", path, "", nil); rec.Code != 404 {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// A filter is applied before the page is cut: walking a filtered listing by
// its cursor answers each match once, in the listing's order, and nothing
// else, whatever lies between the matches.
func TestAFilteredListingPagesAsTheListingDoes(t *testing.T) {
	w := newResultsWorld(t)
	var proceeded, failed []string
	for i := 0; i < 55; i++ {
		proceeded = append(proceeded, w.end(t, w.c, "completed", journalDisposition, "", "").ID)
		failed = append(failed, w.end(t, w.c, "failed", "", classEvaluation, "").ID)
		w.end(t, w.a, "completed", journalDisposition, "", "")
	}
	slices.Reverse(proceeded)
	for _, route := range []string{"/v1/runs?outcome=proceed&q=Charlie", "/v1/jobs/" + w.c.ID + "/runs?outcome=proceed"} {
		var all []string
		pages := 0
		for after := int64(0); ; pages++ {
			path := route
			if after != 0 {
				path += "&after=" + strconv.FormatInt(after, 10)
			}
			got, next, _ := listedRuns(t, w.s, path)
			if len(got) > 50 || next != 0 && len(got) != 50 {
				t.Fatalf("%s: a page of %d, next %d", path, len(got), next)
			}
			all = append(all, got...)
			if next == 0 {
				break
			}
			after = next
		}
		if !slices.Equal(all, proceeded) || pages != 1 {
			t.Errorf("%s: %d runs in %d pages after the first, want the %d that proceeded", route, len(all), pages, len(proceeded))
		}
	}
	var all []string
	pages := 0
	for after := int64(0); ; pages++ {
		_, page := journalRuns(t, w.s, "/v1/jobs/"+w.c.ID+"/events?errorClass=evaluation&after="+strconv.FormatInt(after, 10))
		for _, e := range page.Items {
			all = append(all, e.Concerns.Run)
		}
		if !page.More {
			break
		}
		after = page.Next
	}
	if !slices.Equal(all, failed) || pages != 1 {
		t.Errorf("the journal answers %d failures in %d pages after the first, want %d", len(all), pages, len(failed))
	}
}

// A filter out of form is refused, never read as none; one well formed that
// matches nothing answers an empty page.
func TestAResultFilterOutOfFormIsRefused(t *testing.T) {
	w := newResultsWorld(t)
	w.end(t, w.c, "completed", journalDisposition, "", "")
	long := strings.Repeat("o", maxOutcomeFilter)
	ten := strings.Repeat("&outcome=proceed", maxFilterValues)
	for _, c := range []struct {
		query string
		code  int
	}{
		{"outcome=" + long, 200},
		{"outcome=" + long + "o", 400},
		{"outcome=", 400},
		{"outcome", 400},
		{"outcome=%01", 400},
		{"outcome=a%0Ab", 400},
		{"outcome=%7F", 400},
		{"outcome=%FF", 400},
		{"outcome=%zz", 400},
		{"outcome=proceed;x", 400},
		{"x=%zz&outcome=proceed", 400},
		{"x=%zz&state=completed", 200},
		{ten[1:], 200},
		{ten[1:] + "&outcome=decline", 400},
		{"errorClass=evaluation", 200},
		{"errorClass=Evaluation", 400},
		{"errorClass=", 400},
		{"errorClass=timeout", 400},
		{strings.Repeat("&errorClass=limit", maxFilterValues+1)[1:], 400},
		{"outcome=%E2%9C%93", 200},
	} {
		for _, route := range []string{"/v1/runs?", "/v1/jobs/" + w.c.ID + "/runs?", "/v1/events?", "/v1/jobs/" + w.c.ID + "/events?"} {
			code := c.code
			// The journal refuses every query that does not decode, as an
			// invalid cursor.
			undecodable := strings.Contains(c.query, "%zz") || strings.Contains(c.query, ";")
			journal := strings.Contains(route, "events")
			if journal && undecodable {
				code = 400
			}
			rec := serve(t, w.s, "GET", route+c.query, "", nil)
			if rec.Code != code {
				t.Errorf("%s%s: %d %s, want %d", route, c.query, rec.Code, rec.Body.String(), code)
			}
			if code == 400 && !strings.Contains(rec.Body.String(), `"invalid_`) {
				t.Errorf("%s%s: %s", route, c.query, rec.Body.String())
			}
			if c.code == 400 && !(journal && undecodable) && !strings.Contains(rec.Body.String(), `"invalid_filter"`) {
				t.Errorf("%s%s: %s, want invalid_filter", route, c.query, rec.Body.String())
			}
		}
	}
	if got, _, _ := listedRuns(t, w.s, "/v1/runs?outcome="+long); len(got) != 0 {
		t.Errorf("an outcome no run has answers %v", got)
	}
	for _, query := range []string{"kind=run.completed", "kind=" + strings.Join(eventKinds[:maxFilterValues], "&kind=")} {
		if rec := serve(t, w.s, "GET", "/v1/events?"+query, "", nil); rec.Code != 200 {
			t.Errorf("%s: %d %s", query, rec.Code, rec.Body.String())
		}
	}
	for _, query := range []string{"kind=", "kind=run", "kind=Run.completed", "kind=" + strings.Join(eventKinds[:maxFilterValues+1], "&kind=")} {
		if rec := serve(t, w.s, "GET", "/v1/events?"+query, "", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"invalid_filter"`) {
			t.Errorf("%s: %d %s", query, rec.Code, rec.Body.String())
		}
	}
	// The runs listings take no kind; the jobs listing no run filter.
	if got, _, _ := listedRuns(t, w.s, "/v1/runs?kind=run.failed"); len(got) != 1 {
		t.Errorf("the runs listing reads a kind: %v", got)
	}
	for _, query := range []string{"outcome=proceed", "errorClass=limit", "outcome=%zz"} {
		if rec := serve(t, w.s, "GET", "/v1/jobs?"+query, "", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"invalid_filter"`) {
			t.Errorf("/v1/jobs?%s: %d %s", query, rec.Code, rec.Body.String())
		}
	}
}

// What the status route names: a client tells a Runner that filters from one
// that would ignore the parameters and answer every run.
func TestTheStatusNamesRunResults(t *testing.T) {
	w := newResultsWorld(t)
	rec := serve(t, w.s, "GET", "/v1/status", "", nil)
	var status struct {
		Capabilities []string `json:"capabilities"`
	}
	must(t, json.Unmarshal(rec.Body.Bytes(), &status))
	if !slices.Contains(status.Capabilities, "run-results") {
		t.Fatalf("the status names %v", status.Capabilities)
	}
}

// Every entry that carries no result member is written byte for byte as
// before, at its version; an entry that carries one is at version 5, its
// earlier members are the bytes an earlier Runner wrote, in their order, and
// the new ones follow them.
func TestAnEntryBesideTheResultsIsWrittenAsBefore(t *testing.T) {
	w := newResultsWorld(t)
	w.end(t, w.a, "completed", journalDisposition, "", "")
	w.end(t, w.c, "failed", "", classEvaluation, "")
	w.end(t, w.c, "interrupted", "", "", "")
	rows, e := w.s.db.Query("SELECT entry_version, kind, coalesce(from_state,'-'), coalesce(to_state,'-'), coalesce(reason,''), detail FROM events ORDER BY sequence")
	must(t, e)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var version, kind, from, to, reason, detail string
		must(t, rows.Scan(&version, &kind, &from, &to, &reason, &detail))
		var d EventDetail
		must(t, json.Unmarshal([]byte(detail), &d))
		held := d.Disposition != nil || d.ErrorClass != "" || d.Counts != nil
		if held != (version == resultEntryVersion) {
			t.Errorf("%s at version %s: %s", kind, version, detail)
		}
		// What an earlier Runner wrote: the detail without the new members.
		earlier := d
		earlier.Disposition, earlier.ErrorClass, earlier.Counts = nil, "", nil
		before := string(encode(earlier))
		if held {
			rest := strings.TrimPrefix(detail, strings.TrimSuffix(before, "}"))
			if before == "{}" {
				rest = strings.TrimPrefix(detail, "{")
			} else if rest != detail {
				rest = strings.TrimPrefix(rest, ",")
			}
			if rest == detail || !strings.HasPrefix(rest, `"disposition"`) && !strings.HasPrefix(rest, `"errorClass"`) {
				t.Errorf("%s: %s does not begin with what an earlier Runner wrote, %s", kind, detail, before)
			}
		} else if detail != before {
			t.Errorf("%s: %s, written before as %s", kind, detail, before)
		}
		switch kind {
		case "journal.began", "runner.started", "release.previewed":
			got = append(got, version+" "+kind)
		case "run.interrupted":
			got = append(got, version+" "+kind+" "+from+">"+to+" "+reason+" "+strings.SplitN(detail, `"errorClass"`, 2)[1])
		default:
			got = append(got, version+" "+kind+" "+from+">"+to+" "+reason+" "+detail)
		}
	}
	must(t, rows.Err())
	want := []string{
		"1 journal.began",
		"1 runner.started",
		"1 release.previewed",
		`3 job.created ->-  {"scope":"a"}`,
		"1 release.previewed",
		`3 job.created ->-  {"scope":"b"}`,
		"1 release.previewed",
		`1 job.created ->-  {}`,
		`3 run.queued ->queued  {"scope":"a"}`,
		`3 run.started queued>running  {"scope":"a"}`,
		`5 run.completed running>completed  {"scope":"a","chainSequence":1,"disposition":{"kind":"outcome","outcomeId":"proceed"},"counts":{"decisions":1,"byKind":{"outcome":1},"byOutcome":{"proceed":1}}}`,
		`1 run.queued ->queued  {}`,
		`1 run.started queued>running  {}`,
		`5 run.failed running>failed Failed. {"errorClass":"evaluation","counts":{"decisions":0,"byKind":{},"byOutcome":{}}}`,
		`1 run.queued ->queued  {}`,
		`1 run.started queued>running  {}`,
		`5 run.interrupted running>interrupted Stopped. :"interrupted","counts":{"decisions":0,"byKind":{},"byOutcome":{}}}`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the journal is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A run queued again by an explicit retry loses its counts and its class with
// its problem, in what the retry answers, in its record and in its entry,
// until its next end.
func TestARetriedRunHoldsNoResultUntilItEnds(t *testing.T) {
	w := newResultsWorld(t)
	r := w.end(t, w.c, "interrupted", "", "", "")
	if r.ErrorClass != classInterrupted || r.Counts == nil {
		t.Fatalf("an interrupted run: %+v", r)
	}
	rec := serve(t, w.s, "POST", "/v1/runs/"+r.ID+"/retry", `{"attempt":1}`, map[string]string{"Content-Type": "application/json"})
	for _, body := range []string{rec.Body.String(), serve(t, w.s, "GET", "/v1/runs/"+r.ID, "", nil).Body.String()} {
		if rec.Code != 200 && rec.Code != 202 || !strings.Contains(body, `"state":"queued"`) || strings.Contains(body, `"counts"`) || strings.Contains(body, `"errorClass"`) {
			t.Fatalf("a retried run is served %d %s", rec.Code, body)
		}
	}
	var record string
	must(t, w.s.db.QueryRow("SELECT record FROM runs WHERE id=?", r.ID).Scan(&record))
	if strings.Contains(record, `"counts"`) || strings.Contains(record, `"errorClass"`) {
		t.Fatalf("a retried run's record holds %s", record)
	}
	_, page := journalRuns(t, w.s, "/v1/jobs/"+w.c.ID+"/events?tail=true")
	last := page.Items[len(page.Items)-1]
	if last.Concerns.Run != r.ID || last.Counts != nil || last.ErrorClass != "" || last.EntryVersion != attemptEntryVersion {
		t.Fatalf("the retry's entry: %+v", last)
	}
}

// The API document names what a run, an occurrence and a run in a listing
// are served with, and the filters each listing takes, as Runner serves and
// reads them.
func TestRunResultsAreDocumented(t *testing.T) {
	doc := readOpenAPI(t)
	decoded := func(body []byte) map[string]any {
		var v map[string]any
		d := json.NewDecoder(strings.NewReader(string(body)))
		d.UseNumber()
		must(t, d.Decode(&v))
		return v
	}
	held := func(schema, what string, v map[string]any, members ...string) {
		properties := doc.schema(schema)["properties"].(map[string]any)
		for _, m := range members {
			p, documented := properties[m].(map[string]any)
			if _, served := v[m]; !served || !documented {
				t.Errorf("%s: %s served %v, documented %v", what, m, served, documented)
				continue
			}
			if problems := doc.check(what+"/"+m, p, v[m]); len(problems) != 0 {
				t.Errorf("%v", problems)
			}
		}
	}
	w := newResultsWorld(t)
	completed := w.end(t, w.c, "completed", journalDisposition, "", "")
	failed := w.end(t, w.c, "failed", "", classEvaluation, "")
	held("Run", "a completed run", decoded(serve(t, w.s, "GET", "/v1/runs/"+completed.ID, "", nil).Body.Bytes()), "counts")
	held("Run", "a failed run", decoded(serve(t, w.s, "GET", "/v1/runs/"+failed.ID, "", nil).Body.Bytes()), "counts", "errorClass")
	_, _, items := listedRuns(t, w.s, "/v1/runs?errorClass=evaluation")
	held("Run", "a listed run", decoded(encode(items[0])), "counts", "errorClass")

	source := acquiringWorld(t, nil, func(int) (int, string) { return 400, "" })
	o := source.step(t)
	rec := serve(t, source.s, "GET", "/v1/jobs/"+o.JobID+"/occurrences", "", nil)
	var listed struct {
		Items []json.RawMessage `json:"items"`
	}
	if must(t, json.Unmarshal(rec.Body.Bytes(), &listed)); rec.Code != 200 || len(listed.Items) != 1 {
		t.Fatalf("the occurrences are served %d %s", rec.Code, rec.Body.String())
	}
	held("Occurrence", "a failed occurrence", decoded(listed.Items[0]), "errorClass")

	paths := doc["paths"].(map[string]any)
	for path, names := range map[string][]string{
		"/runs":              {"outcome", "errorClass"},
		"/jobs/{job}/runs":   {"outcome", "errorClass"},
		"/events":            {"outcome", "errorClass", "kind"},
		"/jobs/{job}/events": {"outcome", "errorClass", "kind"},
	} {
		parameters := map[string]map[string]any{}
		for _, p := range paths[path].(map[string]any)["get"].(map[string]any)["parameters"].([]any) {
			if p, ok := p.(map[string]any); ok && p["name"] != nil {
				parameters[p["name"].(string)] = p
			}
		}
		for _, name := range names {
			p := parameters[name]
			if p == nil || p["in"] != "query" || p["explode"] != true {
				t.Errorf("GET %s: the parameter %s is %v", path, name, p)
				continue
			}
			schema := p["schema"].(map[string]any)
			if n, _ := schema["maxItems"].(json.Number).Int64(); n != maxFilterValues {
				t.Errorf("GET %s: %s is bounded at %d", path, name, n)
			}
			items := schema["items"].(map[string]any)
			for _, value := range map[string][]string{"errorClass": errorClasses, "kind": eventKinds}[name] {
				if problems := doc.check(path+"/"+name, items, value); len(problems) != 0 {
					t.Errorf("%v", problems)
				}
			}
			if name != "outcome" && len(doc.check(path+"/"+name, items, "not-one")) == 0 {
				t.Errorf("GET %s: %s takes a value Runner refuses", path, name)
			}
		}
	}
	// The listing of jobs takes neither run filter, and says it refuses them
	// as Runner does.
	jobs := paths["/jobs"].(map[string]any)["get"].(map[string]any)
	refused, _ := jobs["responses"].(map[string]any)["400"].(map[string]any)
	description, _ := refused["description"].(string)
	for _, name := range []string{"outcome", "errorClass"} {
		if rec := serve(t, w.s, "GET", "/v1/jobs?"+name+"=x", "", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_filter") || !strings.Contains(description, name) {
			t.Errorf("GET /jobs with %s answers %d %s, and its 400 is documented as %q", name, rec.Code, rec.Body.String(), description)
		}
	}
}

// A run that fails before the Runtime's evaluation process has started fails
// by Runner's own fault, internal, and not by the evaluation's: a pinned
// Runtime that cannot be started, and a release whose pack no longer has its
// digest. A run whose Runtime started and failed, or answered out of form,
// is the evaluation's. A governed run refused before its start is internal
// too (requireNoLaunch, manual_admission_acceptance_test.go).
func TestAFailureBeforeTheRuntimeEvaluatesIsInternal(t *testing.T) {
	unstartable := func(t *testing.T, dir string) string {
		runtime := fakeRuntime(t, dir, "exit 0 # never started")
		must(t, os.Chmod(filepath.Join(dir, "runtimes", strings.TrimPrefix(runtime, "sha256:")), 0600))
		return runtime
	}
	answering := func(body string) func(*testing.T, string) string {
		return func(t *testing.T, dir string) string { return fakeRuntime(t, dir, body) }
	}
	for name, c := range map[string]struct {
		runtime    func(*testing.T, string) string
		packDigest string
		class      string
	}{
		"a Runtime that cannot be started":        {unstartable, "", classInternal},
		"a pack that no longer has its digest":    {completingRuntime, digest([]byte("another pack")), classInternal},
		"a Runtime that started and failed":       {failingRuntime, "", classEvaluation},
		"a Runtime that answered out of its form": {answering("echo '{}'"), "", classEvaluation},
	} {
		cfg := journalConfig(t)
		s := openJournal(t, cfg)
		release := journalRelease(t, s, c.runtime(t, cfg.Dir))
		j, e := s.createJob(name, release.ID)
		must(t, e)
		if c.packDigest != "" {
			_, e = s.db.Exec("UPDATE releases SET record=json_set(record,'$.packDigest',?) WHERE id=?", c.packDigest, release.ID)
			must(t, e)
		}
		run, _, e := s.submit(j.ID, "run", journalInput())
		must(t, e)
		if worked, e := s.dispatchNext(t.Context()); e != nil || !worked {
			t.Fatalf("%s: the run was not dispatched: %v %v", name, worked, e)
		}
		run, e = s.run(run.ID)
		must(t, e)
		if run.State != "failed" || run.ErrorClass != c.class || run.Counts == nil || run.Counts.Decisions != 0 {
			t.Errorf("%s: the run ended %s, classed %q, want failed and %q (%s)", name, run.State, run.ErrorClass, c.class, run.Problem)
		}
		s.Close()
	}
}

// A gateway that refused an acquisition 401 or 403 refused Runner's
// credential or the connection's access to its provider: the setup, not the
// source, whatever the retry policy does with the answer. Every other
// refusal stays the source's.
func TestAGatewayRefusingAccessIsConfiguration(t *testing.T) {
	policy := &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}
	for status, class := range map[int]string{401: classConfiguration, 403: classConfiguration, 400: classSource, 404: classSource, 502: classSource} {
		for _, p := range []*RetryPolicy{nil, policy} {
			w := acquiringWorld(t, p, func(int) (int, string) { return status, "" })
			o := w.step(t)
			if o.State != "failed" || o.ErrorClass != class || len(o.Attempts) > 1 {
				t.Errorf("an acquisition the gateway answered %d (policy %v): %s %q, want class %q", status, p != nil, o.State, o.ErrorClass, class)
			}
			if p != nil && (len(o.Attempts) != 1 || o.Attempts[0].Status != status) {
				t.Errorf("the attempt answered %d does not keep its status: %+v", status, o.Attempts)
			}
		}
	}
}
