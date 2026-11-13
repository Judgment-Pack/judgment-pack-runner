package runner

import (
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
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
// or a constant assigned to a Reason.
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
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Reason" && i < len(n.Rhs) && len(n.Lhs) == len(n.Rhs) {
						note(n.Rhs[i])
					}
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && owners[id.Name] {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Reason" {
								note(kv.Value)
							}
						}
					}
				}
			}
			return true
		})
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
