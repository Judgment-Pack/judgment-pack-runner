package runner

// The conformance cases of jobs as they are made and revised behind the store
// contract (issue #244, docs/design/store-interface.md, section 9). Every
// kind runs them, as it runs storeCases.

import (
	"slices"
	"testing"
)

// otherRelease keeps another release of the world's pack, with digest and
// dialect.
func (w storeWorld) otherRelease(t *testing.T, id, digest string, dialect *DialectIdentity) Release {
	t.Helper()
	r := w.release
	r.ID, r.PackDigest, r.Dialect = id, digest, dialect
	must(t, w.st.update(func(tx runTx) error { return tx.createRelease(r) }))
	return r
}

var jobStoreCases = []storeCase{
	{"a job is made once from its release and revised once per revision and release", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		first, second := w.otherRelease(t, "rel_first", "sha256:1", nil), w.otherRelease(t, "rel_second", "sha256:1", nil)
		j := Job{SchemaVersion: "1", ID: "job_made", Name: "Made", ReleaseID: first.ID, Revision: 1, CreatedAt: "2026-11-01T10:00:00Z", Workspace: "conformance", Owner: "owner"}
		must(t, w.st.update(func(tx runTx) error { return tx.createJob(j) }))
		again := j
		again.Name, again.ReleaseID, again.Revision = "Again", second.ID, 7
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createJob(again) }), faultTaken, "a second job under one id")
		again.ID, again.ReleaseID, again.Revision = "job_again", first.ID, 1
		wantFault(t, w.st.update(func(tx runTx) error { return tx.createJob(again) }), faultTaken, "a second job from one release")
		if made, err := w.st.jobCreation(first.ID); err != nil || made.ID != j.ID || made.Name != "Made" {
			t.Fatalf("the release's creation reads %+v, %v", made, err)
		}
		_, err := w.st.jobCreation(second.ID)
		wantFault(t, err, faultAbsent, "a release that made no job")
		revised := j
		revised.ReleaseID, revised.Revision = second.ID, 2
		must(t, w.st.update(func(tx runTx) error { return tx.reviseJob(revised, "sha256:request") }))
		if held, err := w.st.job(j.ID); err != nil || held.Revision != 2 || held.ReleaseID != second.ID {
			t.Fatalf("the revised job reads %+v, %v", held, err)
		}
		if made, err := w.st.jobCreation(first.ID); err != nil || made.Revision != 1 {
			t.Fatalf("a revision changed the creation: %+v, %v", made, err)
		}
		for revision, want := range map[int]string{1: "", 2: "sha256:request"} {
			held, request, err := w.st.jobRevision(j.ID, revision)
			if err != nil || held.Revision != revision || request != want {
				t.Fatalf("revision %d reads %+v, %q, %v", revision, held, request, err)
			}
		}
		_, _, err = w.st.jobRevision(j.ID, 3)
		wantFault(t, err, faultAbsent, "a revision not made")
		for release, want := range map[string]int{first.ID: 1, second.ID: 1, "rel_none": 0} {
			if n, err := w.st.releaseUses(release); err != nil || n != want {
				t.Fatalf("%s is used by %d revisions, %v; want %d", release, n, err, want)
			}
		}
		third := w.otherRelease(t, "rel_third", "sha256:1", nil)
		taken := revised
		taken.ReleaseID = third.ID
		wantFault(t, w.st.update(func(tx runTx) error { return tx.reviseJob(taken, "") }), faultTaken, "a revision made twice")
		taken.Revision, taken.ReleaseID = 3, second.ID
		wantFault(t, w.st.update(func(tx runTx) error { return tx.reviseJob(taken, "") }), faultTaken, "a revision naming a release another names")
		taken.ID, taken.ReleaseID = "job_none", third.ID
		wantFault(t, w.st.update(func(tx runTx) error { return tx.reviseJob(taken, "") }), faultAbsent, "a revision of a job not held")
		if held, err := w.st.job(j.ID); err != nil || held.Revision != 2 {
			t.Fatalf("a refused revision wrote %+v, %v", held, err)
		}
	}},
	{"pack and dialect conflicts are counted over the releases job revisions name", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		dialect := &DialectIdentity{ID: "dialect", Version: "1.0.0", Digest: "sha256:d1"}
		used := w.otherRelease(t, "rel_used", "sha256:p1", dialect)
		w.otherRelease(t, "rel_unused", "sha256:p2", &DialectIdentity{ID: "dialect", Version: "1.0.0", Digest: "sha256:d2"})
		must(t, w.st.update(func(tx runTx) error {
			return tx.createJob(Job{SchemaVersion: "1", ID: "job_used", Name: "Used", ReleaseID: used.ID, Revision: 1, CreatedAt: "2026-11-01T10:00:00Z", Workspace: "conformance", Owner: "owner"})
		}))
		for _, c := range []struct {
			what, version, digest string
			want                  int
		}{{"the same content", "1.0.0", "sha256:p1", 0}, {"other content", "1.0.0", "sha256:p3", 1}, {"another version", "2.0.0", "sha256:p3", 0}} {
			// The world's own release is the pack's too, at its own digest.
			want := c.want
			if c.version == w.release.PackVersion && w.release.PackDigest != c.digest {
				want++
			}
			if n, err := w.st.packConflicts(w.release.PackID, c.version, c.digest); err != nil || n != want {
				t.Fatalf("pack conflicts with %s: %d, %v; want %d", c.what, n, err, want)
			}
		}
		for _, c := range []struct {
			what, id, digest string
			want             int
		}{{"the same content", "dialect", "sha256:d1", 0}, {"other content", "dialect", "sha256:d3", 1}, {"another dialect", "other", "sha256:d3", 0}} {
			if n, err := w.st.dialectConflicts(c.id, "1.0.0", c.digest); err != nil || n != c.want {
				t.Fatalf("dialect conflicts with %s: %d, %v; want %d", c.what, n, err, c.want)
			}
		}
	}},
	{"a job's triggers are revised in their records only, and its open occurrences counted", func(t *testing.T, k storeKindUnderTest) {
		w := newStoreWorld(t, k)
		first, second, other := w.worldTrigger("trg_1"), w.worldTrigger("trg_2"), w.worldTrigger("trg_3")
		other.JobID = "job_other"
		open, done := w.worldOccurrence("occ_1", "trg_1", "needs-attention"), w.worldOccurrence("occ_2", "trg_1", "submitted")
		w.keep(t, []Trigger{first, other, second}, open, done)
		first.observed = "sha256:seen"
		must(t, w.st.update(func(tx runTx) error { return tx.putTrigger(first, "sha256:key", true) }))
		must(t, w.st.update(func(tx runTx) error {
			triggers, err := tx.triggerRecords(w.job.ID)
			if err != nil {
				return err
			}
			var ids []string
			for _, tr := range triggers {
				ids = append(ids, tr.ID)
			}
			if !slices.Equal(ids, []string{"trg_1", "trg_2"}) {
				t.Errorf("the job's triggers %v", ids)
			}
			if n, err := tx.openOccurrences(w.job.ID); err != nil || n != 1 {
				t.Errorf("%d open occurrences, %v", n, err)
			}
			revised := triggers[0]
			revised.Revision, revised.UpdatedAt = 2, "2026-11-02T10:00:00Z"
			return tx.reviseTrigger(revised)
		}))
		held, hash, err := w.st.trigger(first.ID)
		if err != nil || held.Revision != 2 || held.UpdatedAt != "2026-11-02T10:00:00Z" || hash != "sha256:key" || held.observed != "sha256:seen" {
			t.Fatalf("the revised trigger reads %+v, key %q, %v", held, hash, err)
		}
		wantFault(t, w.st.update(func(tx runTx) error { return tx.reviseTrigger(held) }), faultTaken, "a revision's snapshot kept twice")
		wantFault(t, w.st.update(func(tx runTx) error { return tx.reviseTrigger(w.worldTrigger("trg_none")) }), faultAbsent, "a revision of a trigger not held")
	}},
}
