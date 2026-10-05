package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The contract Desk holds Runner's journal to (desk #218), held here with a
// stand-in Desk: it follows a route from cursor 0 by next, across a restart,
// and reads every answer against the OpenAPI document. testdata/
// journal-entries.json is one entry of every kind, each with every member its
// kind can carry: what Desk's own tests read.

// openAPI is the repository's API document, read with JSON numbers kept as
// written.
type openAPI map[string]any

func readOpenAPI(t *testing.T) openAPI {
	t.Helper()
	raw, e := os.ReadFile("../../openapi.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc openAPI
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if e = d.Decode(&doc); e != nil {
		t.Fatal(e)
	}
	return doc
}

func (doc openAPI) schema(name string) map[string]any {
	return doc["components"].(map[string]any)["schemas"].(map[string]any)[name].(map[string]any)
}

// check holds a value to a schema, for the parts of JSON Schema the API
// document uses: $ref, oneOf, type, const, enum, properties, required,
// additionalProperties false, items, maxItems and minimum.
func (doc openAPI) check(path string, schema map[string]any, v any) []string {
	if ref, ok := schema["$ref"].(string); ok {
		return doc.check(path, doc.schema(strings.TrimPrefix(ref, "#/components/schemas/")), v)
	}
	var problems []string
	if one, ok := schema["oneOf"].([]any); ok {
		matched := 0
		for _, s := range one {
			if len(doc.check(path, s.(map[string]any), v)) == 0 {
				matched++
			}
		}
		if matched != 1 {
			problems = append(problems, fmt.Sprintf("%s: %d of oneOf match", path, matched))
		}
	}
	if types, ok := schema["type"]; ok {
		allowed := []any{types}
		if list, ok := types.([]any); ok {
			allowed = list
		}
		if !slices.ContainsFunc(allowed, func(t any) bool { return jsonType(t.(string), v) }) {
			problems = append(problems, fmt.Sprintf("%s: %v is not %v", path, v, types))
		}
	}
	if c, ok := schema["const"]; ok && fmt.Sprint(c) != fmt.Sprint(v) {
		problems = append(problems, fmt.Sprintf("%s: %v is not %v", path, v, c))
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(v) }) {
		problems = append(problems, fmt.Sprintf("%s: %v is not one of %v", path, v, enum))
	}
	switch x := v.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if _, ok := x[r.(string)]; !ok {
				problems = append(problems, fmt.Sprintf("%s: no %s", path, r))
			}
		}
		for k, value := range x {
			p, ok := properties[k].(map[string]any)
			if !ok {
				if schema["additionalProperties"] == false {
					problems = append(problems, fmt.Sprintf("%s: %s is not in the document", path, k))
				}
				continue
			}
			problems = append(problems, doc.check(path+"/"+k, p, value)...)
		}
	case []any:
		if max, ok := schema["maxItems"].(json.Number); ok {
			if n, _ := max.Int64(); int64(len(x)) > n {
				problems = append(problems, fmt.Sprintf("%s: %d items", path, len(x)))
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range x {
				problems = append(problems, doc.check(path+"/"+strconv.Itoa(i), items, item)...)
			}
		}
	case json.Number:
		if min, ok := schema["minimum"].(json.Number); ok {
			if a, b := x.String(), min.String(); len(a) < len(b) || len(a) == len(b) && a < b || strings.HasPrefix(a, "-") {
				problems = append(problems, fmt.Sprintf("%s: %s is below %s", path, a, b))
			}
		}
	}
	return problems
}

func jsonType(t string, v any) bool {
	switch x := v.(type) {
	case nil:
		return t == "null"
	case bool:
		return t == "boolean"
	case string:
		return t == "string"
	case json.Number:
		return t == "number" || t == "integer" && !strings.ContainsAny(x.String(), ".eE")
	case []any:
		return t == "array"
	case map[string]any:
		return t == "object"
	}
	return false
}

func decodeNumbers(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if e := d.Decode(&v); e != nil {
		t.Fatal(e)
	}
	return v
}

// standInDesk reads the journal as Desk does: page by page, from a cursor it
// keeps, holding each answer to the document.
type standInDesk struct {
	t   *testing.T
	doc openAPI
	s   *Service
}

func (d standInDesk) page(path string, after int64) EventPage {
	d.t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	got := serve(d.t, d.s, "GET", path+sep+"after="+strconv.FormatInt(after, 10), "", nil)
	if got.Code != 200 || got.Header().Get("Content-Type") != "application/json" || got.Header().Get("Cache-Control") != "no-store" {
		d.t.Fatalf("%s after %d: %d %v %s", path, after, got.Code, got.Header(), got.Body)
	}
	if problems := d.doc.check("JournalPage", d.doc.schema("JournalPage"), decodeNumbers(d.t, got.Body.Bytes())); len(problems) > 0 {
		d.t.Fatalf("%s after %d is not the documented page: %v", path, after, problems)
	}
	var page EventPage
	if e := strictJSON(got.Body.Bytes(), &page); e != nil {
		d.t.Fatal(e)
	}
	return page
}

// follow reads a route from a cursor to its end, and returns what it read,
// the cursor it ends at, and how many pages it took.
func (d standInDesk) follow(path string, after int64) ([]Event, int64, int) {
	d.t.Helper()
	var read []Event
	pages := 0
	for {
		page := d.page(path, after)
		pages++
		for _, e := range page.Items {
			if e.Sequence <= after {
				d.t.Fatalf("%s: entry %d after cursor %d", path, e.Sequence, after)
			}
			after = e.Sequence
		}
		if page.Next != after {
			d.t.Fatalf("%s: next %d, last read %d", path, page.Next, after)
		}
		read = append(read, page.Items...)
		if !page.More {
			return read, after, pages
		}
		if len(page.Items) != eventPage {
			d.t.Fatalf("%s: a page of %d says there is more", path, len(page.Items))
		}
	}
}

func membersOf(t *testing.T, v any) []string {
	t.Helper()
	var names []string
	for k := range v.(map[string]any) {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

func TestAStandInDeskFollowsTheJournal(t *testing.T) {
	doc := readOpenAPI(t)
	entry := doc.schema("JournalEntry")
	var documented []string
	for _, k := range entry["properties"].(map[string]any)["kind"].(map[string]any)["enum"].([]any) {
		documented = append(documented, k.(string))
	}
	if !slices.Equal(documented, eventKinds) {
		t.Fatalf("the document names the kinds %v, Runner writes %v", documented, eventKinds)
	}

	raw, e := os.ReadFile("testdata/journal-entries.json")
	if e != nil {
		t.Fatal(e)
	}
	fixture := map[string][]string{}
	for i, item := range decodeNumbers(t, raw).([]any) {
		if problems := doc.check("fixture/"+strconv.Itoa(i), entry, item); len(problems) > 0 {
			t.Fatalf("the fixture is not the documented entry: %v", problems)
		}
		kind := item.(map[string]any)["kind"].(string)
		if _, twice := fixture[kind]; twice {
			t.Fatal("the fixture has two entries of", kind)
		}
		fixture[kind] = membersOf(t, item)
	}
	if len(fixture) != len(eventKinds) {
		t.Fatalf("the fixture has %d kinds, Runner writes %d", len(fixture), len(eventKinds))
	}

	cfg := journalConfig(t)
	w := runJournalSteps(t, cfg, false)
	// More than a page, so the stand-in turns one.
	for i := 0; i < 30; i++ {
		_, _, e := w.s.submit(w.failingJob.ID, "page-"+strconv.Itoa(i), journalInput())
		must(t, e)
	}
	desk := standInDesk{t, doc, w.s}
	all, cursor, pages := desk.follow("/v1/events", 0)
	if pages < 2 {
		t.Fatalf("%d entries in %d page", len(all), pages)
	}
	for n, e := range all {
		if e.Sequence != int64(n+1) {
			t.Fatalf("entry %d has sequence %d", n+1, e.Sequence)
		}
		served := decodeNumbers(t, encode(e))
		for _, member := range membersOf(t, served) {
			if !slices.Contains(fixture[e.Kind], member) {
				t.Errorf("a %s entry carries %s, which the fixture's does not", e.Kind, member)
			}
		}
	}

	// A job's route serves its entries and its release's alone, in order;
	// the store-wide route with the job adds Runner's own.
	job, _, _ := desk.follow("/v1/jobs/"+w.job.ID+"/events", 0)
	filtered, _, _ := desk.follow("/v1/events?job="+w.job.ID, 0)
	var wantJob, wantFiltered []int64
	for _, e := range all {
		mine := e.Concerns.Job == w.job.ID || e.Concerns.Job == "" && e.Concerns.Release == w.release.ID
		if mine {
			wantJob = append(wantJob, e.Sequence)
		}
		if mine || slices.Contains([]string{"journal.began", "runner.started", "runner.stopped"}, e.Kind) {
			wantFiltered = append(wantFiltered, e.Sequence)
		}
	}
	sequences := func(entries []Event) []int64 {
		var s []int64
		for _, e := range entries {
			s = append(s, e.Sequence)
		}
		return s
	}
	if !slices.Equal(sequences(job), wantJob) || len(wantJob) == 0 {
		t.Fatalf("the job's route served %v, want %v", sequences(job), wantJob)
	}
	if !slices.Equal(sequences(filtered), wantFiltered) {
		t.Fatalf("the store-wide route for the job served %v, want %v", sequences(filtered), wantFiltered)
	}

	// Across a restart the stand-in goes on from its cursor: it reads the
	// stop and the start, then the next change, and nothing twice.
	w.s.Close()
	w.s = openJournal(t, cfg)
	desk.s = w.s
	resumed, cursor, _ := desk.follow("/v1/events", cursor)
	if got := kindsOf(resumed); !slices.Equal(got, []string{"runner.stopped", "runner.started"}) {
		t.Fatalf("after the restart the stand-in read %v", got)
	}
	_, _, e = w.s.submit(w.failingJob.ID, "after-restart", journalInput())
	must(t, e)
	next, _, _ := desk.follow("/v1/events", cursor)
	if got := kindsOf(next); !slices.Equal(got, []string{"run.queued"}) {
		t.Fatalf("then it read %v", got)
	}
	again, _, _ := desk.follow("/v1/events", 0)
	if !slices.Equal(sequences(again), sequences(slices.Concat(all, resumed, next))) {
		t.Fatal("reading from 0 again does not give what the stand-in read in turn")
	}
}
