package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// backwardFixture is a store whose journal holds thousands of entries written
// through the journal's own writer: some of a job, some of another job, some
// of neither. Every time is fixed, so a page's bytes are the same on every
// run once the random ids are named.
type backwardFixture struct {
	s       *Service
	job     Job
	release Release
	all     []Event // every entry, in the journal's order
	mine    []Event // the entries the job's route serves
}

func newBackwardFixture(t *testing.T, n int) *backwardFixture {
	t.Helper()
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	release := journalRelease(t, s, completingRuntime(t, cfg.Dir))
	j, e := s.createJob("Backward", release.ID)
	must(t, e)
	tx, e := s.db.Begin()
	must(t, e)
	for i := 0; i < n; i++ {
		c := change{kind: "run.queued", by: runnerActor, reason: fmt.Sprintf("entry %d", i)}
		switch i % 3 {
		case 0:
			c.concerns = EventConcerns{Job: j.ID, Release: release.ID}
		case 1:
			c.concerns = EventConcerns{Job: "job_other", Release: "release_other"}
		}
		if i%7 == 0 {
			c.states, c.from, c.to = true, "queued", "running"
		}
		_, e = appendEvent(tx, c)
		must(t, e)
	}
	must(t, tx.Commit())
	_, e = s.db.Exec("UPDATE events SET at='2026-10-10T00:00:00.000000Z'")
	must(t, e)
	f := &backwardFixture{s: s, job: j, release: release}
	for after := int64(0); ; {
		page, e := eventPageFrom(s.db, after, "1=1")
		must(t, e)
		f.all = append(f.all, page.Items...)
		after = page.Next
		if !page.More {
			break
		}
	}
	for after := int64(0); ; {
		page, e := eventPageFrom(s.db, after, jobEntries, j.ID, j.ReleaseID)
		must(t, e)
		f.mine = append(f.mine, page.Items...)
		after = page.Next
		if !page.More {
			break
		}
	}
	return f
}

// get answers a route with the fixture's random ids named.
func (f *backwardFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	w := serve(t, f.s, "GET", path, "", nil)
	body := strings.NewReplacer(f.job.ID, "JOB", f.release.ID, "RELEASE").Replace(w.Body.String())
	return w.Code, body
}

func (f *backwardFixture) page(t *testing.T, path string) EventPage {
	t.Helper()
	w := serve(t, f.s, "GET", path, "", nil)
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	var p EventPage
	must(t, json.Unmarshal(w.Body.Bytes(), &p))
	return p
}

// goldenForward holds the SHA-256 of the answers to requests that send neither
// new parameter, taken from the code before the parameters existed.
var goldenForward = map[string]string{
	"/v1/events":                    "200 a2a0ea4854c3cbbc3cc56c6de6e861d9ec6cdba511feea65d5c83509c02b4a3b",
	"/v1/events?after=0":            "200 a2a0ea4854c3cbbc3cc56c6de6e861d9ec6cdba511feea65d5c83509c02b4a3b",
	"/v1/events?after=50":           "200 7d5778b47bedcc395a487e24090598525aa6f5ccc315f918760e86bb1729dac9",
	"/v1/events?after=1000":         "200 6f4ed1abd77386f9632ee88ffda7fa05bff53ef7eac3f6870ada762fa97d4c90",
	"/v1/events?after=2090":         "200 f33ea1e895522ebc9fecf268c8bbe1b9aaf69492ce1181f9e83f04752ca19e30",
	"/v1/events?after=2099":         "200 d8a8d949fa45a48a9e4a87401a38c8875daa242083f3e7a43c70152b80193acc",
	"/v1/events?after=999999":       "200 9bfed2caf0c4924574c7bbd2b3ab2e19551b033e69ac7d011fe17df3a67cceb7",
	"/v1/events?job=JOB":            "200 74207a9be39cf9f7af80227d58b235a0bf6c46c0708be26a12172d97ef3a48cc",
	"/v1/jobs/JOB/events":           "200 f06b50c759b869b2df5aed992fc09fd7bf7a8668f0e25e94f58717fac48678dd",
	"/v1/jobs/JOB/events?after=300": "200 2e0c49543f74d8759e3a432fc50a94e0dc97493161f0d681bac558e3ab635717",
}

var goldenForwardPaths = []string{
	"/v1/events",
	"/v1/events?after=0",
	"/v1/events?after=50",
	"/v1/events?after=1000",
	"/v1/events?after=2090",
	"/v1/events?after=2099",
	"/v1/events?after=999999",
	"/v1/events?job=JOB",
	"/v1/jobs/JOB/events",
	"/v1/jobs/JOB/events?after=300",
}

func TestOlderClientsGetTheAnswersTheyAlwaysGot(t *testing.T) {
	f := newBackwardFixture(t, 2100)
	for _, p := range goldenForwardPaths {
		code, body := f.get(t, strings.ReplaceAll(p, "JOB", f.job.ID))
		sum := sha256.Sum256([]byte(body))
		got := fmt.Sprintf("%d %s", code, hex.EncodeToString(sum[:]))
		if want, ok := goldenForward[p]; !ok || want != got {
			t.Errorf("%s: golden %q, got %q (body begins %.200s)", p, want, got, body)
		}
	}
	// A forward answer carries no new member.
	_, body := f.get(t, "/v1/events?after=2090")
	if strings.Contains(body, `"before"`) {
		t.Fatalf("a forward answer carries before: %s", body)
	}
}

func TestTheNewestPageIsOneRequest(t *testing.T) {
	f := newBackwardFixture(t, 2100)
	for _, c := range []struct {
		name, path string
		entries    []Event
	}{{"all", "/v1/events?tail=true", f.all}, {"job", "/v1/jobs/" + f.job.ID + "/events?tail=true", f.mine}, {"events filtered", "/v1/events?tail=true&job=" + f.job.ID, nil}} {
		p := f.page(t, c.path)
		if len(p.Items) != eventPage || !p.More {
			t.Fatalf("%s: %d items, more %v", c.name, len(p.Items), p.More)
		}
		if c.entries != nil {
			want := c.entries[len(c.entries)-eventPage:]
			if !slices.Equal(sequencesOf(p.Items), sequencesOf(want)) {
				t.Fatalf("%s: not the newest page", c.name)
			}
		}
		if p.Next != p.Items[len(p.Items)-1].Sequence || *p.Before != p.Items[0].Sequence {
			t.Fatalf("%s: next %d before %v", c.name, p.Next, p.Before)
		}
		if p.JournalBegan == "" {
			t.Fatalf("%s: the answer does not say where the journal begins", c.name)
		}
		var generic any
		w := serve(t, f.s, "GET", c.path, "", nil)
		d := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
		d.UseNumber()
		must(t, d.Decode(&generic))
		if problems := readOpenAPI(t).check("page", readOpenAPI(t).schema("JournalPage"), generic); len(problems) > 0 {
			t.Fatalf("%s: the answer does not hold to openapi.json: %v", c.name, problems)
		}
	}
}

func sequencesOf(es []Event) []int64 {
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.Sequence
	}
	return out
}

func TestWalkingBackReachesTheFirstEntryOnceEach(t *testing.T) {
	f := newBackwardFixture(t, 2100)
	for _, c := range []struct {
		name, path string
		entries    []Event
	}{{"all", "/v1/events", f.all}, {"job", "/v1/jobs/" + f.job.ID + "/events", f.mine}} {
		var seen []int64
		var pages int
		path := c.path + "?tail=true"
		for {
			p := f.page(t, path)
			pages++
			if !slices.IsSorted(sequencesOf(p.Items)) {
				t.Fatalf("%s: a page is out of the journal's order", c.name)
			}
			seen = append(sequencesOf(p.Items), seen...)
			if p.More != (len(seen) < len(c.entries)) {
				t.Fatalf("%s: page %d says more=%v with %d of %d read", c.name, pages, p.More, len(seen), len(c.entries))
			}
			if !p.More {
				if pages != (len(c.entries)+eventPage-1)/eventPage {
					t.Fatalf("%s: %d pages", c.name, pages)
				}
				break
			}
			path = c.path + "?before=" + fmt.Sprint(*p.Before)
		}
		if !slices.Equal(seen, sequencesOf(c.entries)) {
			t.Fatalf("%s: the walk saw %d entries, the journal holds %d", c.name, len(seen), len(c.entries))
		}
	}
}

func TestForwardPagingContinuesFromABackwardPage(t *testing.T) {
	f := newBackwardFixture(t, 2100)
	p := f.page(t, "/v1/events?tail=true")
	p = f.page(t, "/v1/events?before="+fmt.Sprint(*p.Before)) // the second newest page
	if p.Next != p.Items[len(p.Items)-1].Sequence {
		t.Fatal("next is not the last entry's sequence")
	}
	var seen []int64
	seen = append(seen, sequencesOf(p.Items)...)
	for p.More || len(seen) == len(p.Items) {
		p = f.page(t, "/v1/events?after="+fmt.Sprint(p.Next))
		seen = append(seen, sequencesOf(p.Items)...)
		if !p.More {
			break
		}
	}
	last := f.all[len(f.all)-1].Sequence
	if seen[len(seen)-1] != last {
		t.Fatalf("forward paging stopped at %d, not %d", seen[len(seen)-1], last)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]+1 {
			t.Fatalf("forward paging skipped or repeated near %d", seen[i])
		}
	}
}

func TestBackwardPagingBounds(t *testing.T) {
	f := newBackwardFixture(t, 120)
	first, last := f.all[0].Sequence, f.all[len(f.all)-1].Sequence

	p := f.page(t, "/v1/events?before=0")
	if len(p.Items) != 0 || p.More || p.Before == nil || *p.Before != 0 || p.Next != 0 {
		t.Fatalf("before=0: %+v", p)
	}
	p = f.page(t, "/v1/events?before="+fmt.Sprint(first))
	if len(p.Items) != 0 || p.More || *p.Before != first || p.JournalBegan == "" {
		t.Fatalf("before the first entry: %+v", p)
	}
	p = f.page(t, "/v1/events?before="+fmt.Sprint(first+1))
	if len(p.Items) != 1 || p.Items[0].Sequence != first || p.More {
		t.Fatalf("before the second entry: %+v", p)
	}
	p = f.page(t, "/v1/events?before=999999")
	if !slices.Equal(sequencesOf(p.Items), sequencesOf(f.all[len(f.all)-eventPage:])) || !p.More {
		t.Fatalf("before beyond the end is not the newest page: %+v", p)
	}
	p = f.page(t, "/v1/events?before="+fmt.Sprint(last))
	if p.Items[len(p.Items)-1].Sequence != last-1 {
		t.Fatal("before must exclude the sequence it names")
	}
	// tail=false is not a request for the end.
	if got, _ := f.get(t, "/v1/events?tail=false"); got != 200 {
		t.Fatal("tail=false refused")
	}
	_, a := f.get(t, "/v1/events?tail=false")
	_, b := f.get(t, "/v1/events")
	if a != b {
		t.Fatal("tail=false changes the answer")
	}
	_, a = f.get(t, "/v1/events?tail=false&after=50")
	_, b = f.get(t, "/v1/events?after=50")
	if a != b {
		t.Fatal("tail=false with after changes the answer")
	}
	for _, q := range []string{
		"before=x", "before=-1", "before=%2B1", "before=", "before=1.5", "before=1&before=2", "before=1234567890123456789",
		"tail=yes", "tail=1", "tail=TRUE", "tail=true&tail=true", "tail=",
		"tail=true&before=5", "tail=true&after=5", "before=5&after=2", "before=5&after=5", "tail=true&before=5&after=2",
	} {
		for _, route := range []string{"/v1/events?", "/v1/jobs/" + f.job.ID + "/events?"} {
			w := serve(t, f.s, "GET", route+q, "", nil)
			if w.Code != 400 {
				t.Errorf("%s%s: %d %s", route, q, w.Code, w.Body)
				continue
			}
			var e struct {
				Error struct{ Message string }
			}
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if e.Error.Message == "" || strings.Count(e.Error.Message, ".") != 1 || !strings.HasSuffix(e.Error.Message, ".") {
				t.Errorf("%s%s: not one sentence: %s", route, q, w.Body)
			}
		}
	}
	if code, _ := f.get(t, "/v1/jobs/job_unknown/events?tail=true"); code != 404 {
		t.Fatal("an unknown job is not 404")
	}
}

func TestTheStatusListsTheBackwardCapability(t *testing.T) {
	f := newBackwardFixture(t, 1)
	w := serve(t, f.s, "GET", "/v1/status", "", nil)
	if !strings.Contains(w.Body.String(), `"journal-backward"`) {
		t.Fatalf("status: %s", w.Body)
	}
}

// A journal that is a whole number of pages must end its backward walk on a
// page that says there is nothing older, and say so for exactly one page.
func TestAJournalOfWholePagesEndsOnTheLastPage(t *testing.T) {
	base := len(newBackwardFixture(t, 0).all)
	for _, pages := range []int{1, 2, 3} {
		f := newBackwardFixture(t, pages*eventPage-base)
		if len(f.all) != pages*eventPage {
			t.Fatalf("fixture holds %d entries", len(f.all))
		}
		p := f.page(t, "/v1/events?tail=true")
		for read := 1; ; read++ {
			if len(p.Items) != eventPage || p.More != (read < pages) {
				t.Fatalf("%d pages: page %d holds %d, more %v", pages, read, len(p.Items), p.More)
			}
			if !p.More {
				if p.Items[0].Sequence != f.all[0].Sequence {
					t.Fatalf("%d pages: the last page does not begin at the first entry", pages)
				}
				break
			}
			p = f.page(t, "/v1/events?before="+fmt.Sprint(*p.Before))
		}
	}
}
