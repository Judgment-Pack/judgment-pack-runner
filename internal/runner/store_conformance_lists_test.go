package runner

// The conformance cases of the job and run listings behind the store
// contract (issue #244, docs/design/store-interface.md, section 9.5). Every
// kind runs them, as it runs storeCases.

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"testing"
)

// listedJob makes a job named name under scope from a release of its own,
// titled title, in one transaction, and returns the job.
func (w storeWorld) listedJob(t *testing.T, id, name, scope, title string) Job {
	t.Helper()
	r := w.release
	r.ID, r.Title, r.PackVersion = "rel_"+id, title, "2."+id
	j := Job{SchemaVersion: "1", ID: id, Name: name, ReleaseID: r.ID, Revision: 1, CreatedAt: "2026-11-01T10:00:00Z", Workspace: "conformance", Owner: "owner", Scope: scope}
	must(t, w.st.update(func(tx runTx) error {
		if err := tx.createRelease(r); err != nil {
			return err
		}
		return tx.createJob(j)
	}))
	return j
}

// recordIDs are the ids of a listing's records, in the order listed.
func recordIDs(t *testing.T, items []listedRecord, err error) []string {
	t.Helper()
	must(t, err)
	ids := []string{}
	for _, item := range items {
		var record struct {
			ID string `json:"id"`
		}
		must(t, json.Unmarshal(item.record, &record))
		ids = append(ids, record.ID)
	}
	return ids
}

var listStoreCases = []storeCase{
	{"jobs are listed newest first, labelled by their release, and filtered where the kind reads", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		w.listedJob(t, "job_alpha", "Alpha Report", "team-a", "Alpha")
		w.listedJob(t, "job_beta", "beta report", "", "Beta")
		w.listedJob(t, "job_gamma", "Gamma", "team-A", "Gamma")
		w.listedJob(t, "job_eclair", "Éclair", "", "Eclair")
		w.listedJob(t, "job_rate", "Rate_100%", "", "Rate")
		// A job whose release is not held is not listed.
		must(t, w.st.update(func(tx runTx) error {
			return tx.createJob(Job{SchemaVersion: "1", ID: "job_orphan", Name: "Orphan Report", ReleaseID: "rel_none", Revision: 1, CreatedAt: "2026-11-01T10:00:00Z", Workspace: "conformance", Owner: "owner"})
		}))
		all, err := w.st.listedJobs(math.MaxInt64, recordFilter{}, 51)
		if ids := recordIDs(t, all, err); !slices.Equal(ids, []string{"job_rate", "job_eclair", "job_gamma", "job_beta", "job_alpha", w.job.ID}) {
			t.Fatalf("the jobs are listed as %v", ids)
		}
		if all[4].label != "Alpha" || all[4].version != "2.job_alpha" || all[5].label != w.release.Title || all[5].version != w.release.PackVersion {
			t.Fatalf("the jobs are labelled %+v", all)
		}
		for i := 1; i < len(all); i++ {
			if all[i].seq >= all[i-1].seq {
				t.Fatalf("the orders are not newest first: %d after %d", all[i].seq, all[i-1].seq)
			}
		}
		for _, c := range []struct {
			filter recordFilter
			want   []string
		}{
			{recordFilter{Search: "REPORT"}, []string{"job_beta", "job_alpha"}},
			{recordFilter{Search: "é"}, []string{}},
			{recordFilter{Search: "É"}, []string{"job_eclair"}},
			{recordFilter{Search: "_100%"}, []string{"job_rate"}},
			{recordFilter{Search: "%"}, []string{"job_rate"}},
			{recordFilter{Search: "e_1"}, []string{"job_rate"}},
			{recordFilter{Search: "a_"}, []string{}},
			{recordFilter{Scope: "team-a"}, []string{"job_alpha"}},
			{recordFilter{Scope: "team-A"}, []string{"job_gamma"}},
			{recordFilter{Scope: "team"}, []string{}},
			{recordFilter{Scope: "team-a", Search: "gamma"}, []string{}},
		} {
			items, err := w.st.listedJobs(math.MaxInt64, c.filter, 51)
			if ids := recordIDs(t, items, err); !slices.Equal(ids, c.want) {
				t.Errorf("%+v lists %v; want %v", c.filter, ids, c.want)
			}
		}
		page, err := w.st.listedJobs(math.MaxInt64, recordFilter{}, 2)
		if ids := recordIDs(t, page, err); !slices.Equal(ids, []string{"job_rate", "job_eclair"}) {
			t.Fatalf("a page of two lists %v", ids)
		}
		next, err := w.st.listedJobs(page[1].seq, recordFilter{}, 2)
		if ids := recordIDs(t, next, err); !slices.Equal(ids, []string{"job_gamma", "job_beta"}) {
			t.Fatalf("the next page lists %v", ids)
		}
		// A job revised to another release is labelled by it, in the order it
		// was made.
		revised := Job{SchemaVersion: "1", ID: "job_beta", Name: "beta report", ReleaseID: "rel_beta_2", Revision: 2, CreatedAt: "2026-11-01T10:00:00Z", Workspace: "conformance", Owner: "owner"}
		r := w.release
		r.ID, r.Title, r.PackVersion = revised.ReleaseID, "Beta Two", "3.0.0"
		must(t, w.st.update(func(tx runTx) error {
			if err := tx.createRelease(r); err != nil {
				return err
			}
			return tx.reviseJob(revised, "sha256:request")
		}))
		items, err := w.st.listedJobs(math.MaxInt64, recordFilter{Search: "beta"}, 51)
		if ids := recordIDs(t, items, err); !slices.Equal(ids, []string{"job_beta"}) || items[0].label != "Beta Two" || items[0].version != "3.0.0" || items[0].seq != all[3].seq {
			t.Fatalf("the revised job is listed as %+v", items)
		}
	}},
	{"runs are listed newest first, labelled by their job, and filtered where the kind reads", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		other := w.listedJob(t, "job_other", "Other Job", "team-a", "Other")
		run := func(id string, job Job, state, scope, class string, result any) {
			t.Helper()
			r := w.queuedRun(id)
			r.JobID, r.ReleaseID, r.State, r.Scope, r.ErrorClass = job.ID, job.ReleaseID, state, scope, class
			if result != nil {
				r.Result = encode(result)
			}
			must(t, w.st.update(func(tx runTx) error { return tx.createRun(r, "owner", id, "sha256:"+id) }))
		}
		outcome := func(id string) any {
			return map[string]any{"disposition": map[string]any{"kind": "outcome", "outcomeId": id, "handoff": map[string]string{"state": "none"}}}
		}
		run("run_queued", w.job, "queued", "", "", nil)
		run("run_approved", w.job, "completed", "", "", outcome("approved"))
		run("run_rejected", other, "completed", "team-a", "", outcome("rejected"))
		run("run_unresolved", w.job, "completed", "", "", map[string]any{"disposition": map[string]any{"kind": "unresolved", "handoff": map[string]string{"state": "none"}}})
		run("run_handoff", other, "completed", "team-a", "", map[string]any{"disposition": map[string]any{"kind": "outcome", "outcomeId": "approved", "handoff": map[string]string{"state": "requested"}}})
		run("run_failed", w.job, "failed", "", classSource, nil)
		run("run_interrupted", other, "interrupted", "team-A", classInterrupted, nil)
		// An outcome a run holds without having completed is no outcome.
		run("run_running", w.job, "running", "", "", outcome("approved"))
		// A run whose job is not held is not listed.
		orphan := w.job
		orphan.ID = "job_none"
		run("run_orphan", orphan, "failed", "", classSource, nil)
		all, err := w.st.listedRuns("", math.MaxInt64, recordFilter{}, 51)
		if ids := recordIDs(t, all, err); !slices.Equal(ids, []string{"run_running", "run_interrupted", "run_failed", "run_handoff", "run_unresolved", "run_rejected", "run_approved", "run_queued"}) {
			t.Fatalf("the runs are listed as %v", ids)
		}
		if all[1].label != "Other Job" || all[2].label != w.job.Name || all[1].version != "" {
			t.Fatalf("the runs are labelled %+v", all)
		}
		for _, c := range []struct {
			job    string
			filter recordFilter
			want   []string
		}{
			{other.ID, recordFilter{}, []string{"run_interrupted", "run_handoff", "run_rejected"}},
			{"", recordFilter{State: "completed"}, []string{"run_handoff", "run_unresolved", "run_rejected", "run_approved"}},
			{"", recordFilter{Review: true}, []string{"run_interrupted", "run_failed", "run_handoff", "run_unresolved"}},
			{"", recordFilter{Results: resultFilter{Outcomes: []string{"approved"}}}, []string{"run_handoff", "run_approved"}},
			{"", recordFilter{Results: resultFilter{Outcomes: []string{"approved", "rejected"}}, State: "completed", Review: true}, []string{"run_handoff"}},
			{"", recordFilter{Results: resultFilter{Classes: []string{classSource, classInterrupted}}}, []string{"run_interrupted", "run_failed"}},
			{"", recordFilter{Scope: "team-a"}, []string{"run_handoff", "run_rejected"}},
			{"", recordFilter{Search: "OTHER"}, []string{"run_interrupted", "run_handoff", "run_rejected"}},
			{"", recordFilter{Search: "run_fail"}, []string{"run_failed"}},
			{"", recordFilter{Search: "ed conf"}, []string{"run_failed", "run_unresolved", "run_approved", "run_queued"}},
			{w.job.ID, recordFilter{Search: "other"}, []string{}},
		} {
			items, err := w.st.listedRuns(c.job, math.MaxInt64, c.filter, 51)
			if ids := recordIDs(t, items, err); !slices.Equal(ids, c.want) {
				t.Errorf("%q %+v lists %v; want %v", c.job, c.filter, ids, c.want)
			}
		}
		page, err := w.st.listedRuns("", math.MaxInt64, recordFilter{State: "completed"}, 3)
		if ids := recordIDs(t, page, err); !slices.Equal(ids, []string{"run_handoff", "run_unresolved", "run_rejected"}) {
			t.Fatalf("a page of three lists %v", ids)
		}
		next, err := w.st.listedRuns("", page[2].seq, recordFilter{State: "completed"}, 3)
		if ids := recordIDs(t, next, err); !slices.Equal(ids, []string{"run_approved"}) {
			t.Fatalf("the next page lists %v", ids)
		}
	}},
	{"a listing's recent runs are each job's newest, by job", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		other := w.listedJob(t, "job_a", "A", "", "A")
		for i := range 7 {
			r := w.queuedRun(fmt.Sprintf("run_%d", i))
			r.CreatedAt = fmt.Sprintf("2026-11-01T10:00:0%dZ", i)
			if i%3 == 0 {
				r.JobID, r.ReleaseID = other.ID, other.ReleaseID
			}
			must(t, w.st.update(func(tx runTx) error { return tx.createRun(r, "owner", r.ID, "sha256:"+r.ID) }))
		}
		got, err := w.st.recentRuns([]string{w.job.ID, other.ID, "job_none"}, 3)
		must(t, err)
		want := []recentRun{
			{other.ID, "run_6", "queued", "2026-11-01T10:00:06Z"}, {other.ID, "run_3", "queued", "2026-11-01T10:00:03Z"}, {other.ID, "run_0", "queued", "2026-11-01T10:00:00Z"},
			{w.job.ID, "run_5", "queued", "2026-11-01T10:00:05Z"}, {w.job.ID, "run_4", "queued", "2026-11-01T10:00:04Z"}, {w.job.ID, "run_2", "queued", "2026-11-01T10:00:02Z"},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the recent runs are %v; want %v", got, want)
		}
		if got, err := w.st.recentRuns([]string{"job_none"}, 3); err != nil || len(got) != 0 {
			t.Fatalf("a job with no runs has %v, %v", got, err)
		}
	}},
	{"a listing's triggers are its jobs' triggers, in the order they were made", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		other := w.listedJob(t, "job_a", "A", "", "A")
		third := w.listedJob(t, "job_b", "B", "", "B")
		for i, job := range []Job{w.job, other, w.job, third, other} {
			tr := w.worldTrigger(fmt.Sprintf("trg_%d", i))
			tr.JobID, tr.NextAt, tr.Paused = job.ID, fmt.Sprintf("2026-11-02T10:00:0%dZ", i), i%2 == 0
			must(t, w.st.update(func(tx runTx) error { return tx.createTrigger(tr) }))
		}
		got, err := w.st.listedTriggers([]string{other.ID, w.job.ID})
		must(t, err)
		var listed []string
		for _, h := range got {
			listed = append(listed, fmt.Sprintf("%s:%s:%s:%t:%s", h.job, h.trigger.ID, h.trigger.Config.Kind, h.trigger.Paused, h.trigger.NextAt))
		}
		want := []string{
			w.job.ID + ":trg_0:event:true:2026-11-02T10:00:00Z",
			other.ID + ":trg_1:event:false:2026-11-02T10:00:01Z",
			w.job.ID + ":trg_2:event:true:2026-11-02T10:00:02Z",
			other.ID + ":trg_4:event:true:2026-11-02T10:00:04Z",
		}
		if !slices.Equal(listed, want) {
			t.Fatalf("the listed triggers are %v; want %v", listed, want)
		}
	}},
}
