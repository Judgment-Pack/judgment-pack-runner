package runner

// The rows the sqlite kind writes are the rows a Runner before the widened
// store contract (v0.31.0) wrote for the same requests: the journal's steps,
// which make every kind of change (runJournalSteps), then a brief, a profile's
// registration, a job with a trigger upgraded, a job under a scope identifier
// and a job with a retry policy.
//
// testdata/store-rows-v0.31.0.txt holds that Runner's rows. It was written by
// this test, with JPACK_STORE_ROWS_WRITE naming the file, run in a checkout
// of v0.31.0 (9913c34) with this file copied beside its own tests: the steps
// and the requests are the same calls in both. The rows are read with
// typeof() and quote(), table by table in name order and row by row in rowid
// order. Every value that differs from one run to the next (an id, a time,
// a digest, a key, a signature, the state directory) is replaced by a
// placeholder numbered in the order it first appears, so that two values the
// same in one store are the same placeholder in the other, and every other
// byte is compared as it is.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const storeRowsGolden = "testdata/store-rows-v0.31.0.txt"

func TestTheSQLiteKindWritesTheRowsAnEarlierRunnerWrote(t *testing.T) {
	cfg := journalConfig(t)
	w := runJournalSteps(t, cfg, false)
	s := w.s

	// A brief, begun and completed.
	snapshot, err := s.briefSnapshot("job", w.job.ID)
	must(t, err)
	brief := func(c BriefCommand) BriefStore {
		t.Helper()
		r := serve(t, s, "POST", "/v1/jobs/"+w.job.ID+"/briefs", string(encode(c)), nil)
		var reply struct {
			Content BriefStore `json:"content"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &reply) != nil {
			t.Fatal("a brief:", r.Code, r.Body.String())
		}
		return reply.Content
	}
	c := BriefCommand{Action: "begin", Subject: "job:" + w.job.ID, Snapshot: snapshot, Model: "fixture"}
	begun := brief(c)
	c.Action, c.Token = "complete", begun.Subjects[c.Subject].Pending.Token
	c.Text = BriefText{"Context.", "Findings.", "Unverified.", "Review."}
	brief(c)

	// A profile's registration, written to the store's metadata.
	profile := scopedPageProfile(func(p map[string]any) { p["id"] = "pages" })
	if r := serve(t, s, "POST", "/v1/input-profiles", string(encode(profile)), nil); r.Code != 201 {
		t.Fatal("a registration:", r.Code, r.Body.String())
	}

	// A job with a trigger, upgraded: a revision of the job and of its
	// trigger.
	tc := eventTriggerConfig()
	upgraded, err := s.createJobConfigured("Upgraded", journalRelease(t, s, s.runtimeDigest).ID, &tc)
	must(t, err)
	_, err = s.upgradeJob(context.Background(), upgraded.ID, UpgradeJobRequest{ReleaseID: journalRelease(t, s, s.runtimeDigest).ID, BaseReleaseID: upgraded.ReleaseID, BaseRevision: 1, Reviewed: true})
	must(t, err)

	// A job under a scope identifier, then one with a retry policy: each
	// raises the store's schema.
	_, err = s.createJobBy(requester{}, "Scoped", journalRelease(t, s, s.runtimeDigest).ID, "scope-a", nil)
	must(t, err)
	_, err = s.createJobWith(requester{}, "Retrying", journalRelease(t, s, s.runtimeDigest).ID, "", nil, &RetryPolicy{MaxAttempts: 2, BackoffSeconds: 5, MaxBackoffSeconds: 5})
	must(t, err)

	s.Close()
	w.s = nil
	got := keptRows(t, cfg.Dir)
	if path := os.Getenv("JPACK_STORE_ROWS_WRITE"); path != "" {
		must(t, os.WriteFile(path, []byte(got), 0600))
		t.Skip("wrote the rows to", path)
	}
	want, err := os.ReadFile(storeRowsGolden)
	must(t, err)
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Fatalf("row %d differs from the earlier Runner's:\n got: %s\nwant: %s", i+1, g, w)
		}
	}
}

// Values that differ from one run to the next: times, and the base64 runs
// of keys and signatures and the hex runs of ids and digests.
var (
	storeRowsTime   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	storeRowsBase64 = regexp.MustCompile(`[A-Za-z0-9+/_-]{40,}={0,2}`)
	storeRowsHex    = regexp.MustCompile(`[0-9a-f]{16,}`)
)

// keptRows reads every row of the store under dir, as text in which each
// value that differs from run to run is a numbered placeholder.
func keptRows(t *testing.T, dir string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteStoreFile)+"?mode=ro")
	must(t, err)
	defer db.Close()
	var tables []string
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	must(t, err)
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	must(t, rows.Err())
	rows.Close()
	var out strings.Builder
	for _, table := range tables {
		var columns []string
		info, err := db.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		must(t, err)
		for info.Next() {
			var name string
			must(t, info.Scan(&name))
			columns = append(columns, name)
		}
		must(t, info.Err())
		info.Close()
		selects := []string{"rowid"}
		for _, c := range columns {
			// A blob is read as its text, so that what it holds is
			// compared as any other value is.
			selects = append(selects, fmt.Sprintf(`typeof("%[1]s")||':'||CASE typeof("%[1]s") WHEN 'blob' THEN quote(CAST("%[1]s" AS TEXT)) ELSE quote("%[1]s") END`, c))
		}
		values, err := db.Query("SELECT " + strings.Join(selects, ",") + " FROM \"" + table + "\" ORDER BY rowid")
		must(t, err)
		for values.Next() {
			cells := make([]string, len(selects))
			dest := make([]any, len(cells))
			for i := range cells {
				dest[i] = &cells[i]
			}
			must(t, values.Scan(dest...))
			fmt.Fprintf(&out, "%s %s", table, cells[0])
			for i, c := range columns {
				fmt.Fprintf(&out, " %s=%s", c, cells[i+1])
			}
			out.WriteByte('\n')
		}
		must(t, values.Err())
		values.Close()
	}
	return placeholders(out.String(), t)
}

// placeholders replaces the test's directories, times and opaque values with
// placeholders numbered by first appearance.
func placeholders(s string, t *testing.T) string {
	temp := regexp.MustCompile(regexp.QuoteMeta(filepath.Clean(os.TempDir())) + `/` + regexp.QuoteMeta(t.Name()) + `[0-9]+/[0-9]+`)
	s = temp.ReplaceAllString(s, "<dir>")
	s = storeRowsTime.ReplaceAllString(s, "<time>")
	seen := map[string]string{}
	number := func(v string) string {
		p, ok := seen[v]
		if !ok {
			p = fmt.Sprintf("<%d>", len(seen)+1)
			seen[v] = p
		}
		return p
	}
	s = storeRowsBase64.ReplaceAllStringFunc(s, func(v string) string {
		// A long name of the record's own holds no digit.
		if !strings.ContainsAny(v, "0123456789") {
			return v
		}
		return number(v)
	})
	return storeRowsHex.ReplaceAllStringFunc(s, number)
}
