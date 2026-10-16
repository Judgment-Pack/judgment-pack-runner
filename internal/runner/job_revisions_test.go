package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestJobUpgradePinsAndReceipts(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	first := journalRelease(t, s, s.runtimeDigest)
	j, err := s.createJob("Fixed", first.ID)
	must(t, err)
	run, _, err := s.submit(j.ID, "before", journalInput())
	must(t, err)
	next := journalRelease(t, s, s.runtimeDigest)
	req := UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true}
	updated, err := s.upgradeJob(context.Background(), j.ID, req)
	must(t, err)
	if updated.Revision != 2 || updated.ReleaseID != next.ID {
		t.Fatal(updated)
	}
	held, err := s.run(run.ID)
	must(t, err)
	if held.ReleaseID != first.ID || held.Revision != 1 {
		t.Fatal("historical run changed", held)
	}
	newer, _, err := s.submit(j.ID, "after", journalInput())
	must(t, err)
	if newer.ReleaseID != next.ID || newer.Revision != 2 {
		t.Fatal(newer)
	}
	receipt, err := s.createJob("Fixed", first.ID)
	must(t, err)
	if receipt != j {
		t.Fatal("creation receipt changed", receipt)
	}
	if _, err = s.createJob("Duplicate", next.ID); err == nil {
		t.Fatal("upgrade release reused")
	}
	repeated, err := s.upgradeJob(context.Background(), j.ID, req)
	must(t, err)
	if repeated != updated {
		t.Fatal("upgrade receipt changed")
	}
	changed := req
	changed.ReleaseID = first.ID
	if _, err = s.upgradeJob(context.Background(), j.ID, changed); err == nil {
		t.Fatal("stale upgrade accepted")
	}
	events := serve(t, s, "GET", "/v1/jobs/"+j.ID+"/events", "", nil)
	var page EventPage
	must(t, json.Unmarshal(events.Body.Bytes(), &page))
	var previews, upgrades int
	for _, e := range page.Items {
		if e.Kind == "release.previewed" {
			previews++
		}
		if e.Kind == "job.upgraded" {
			upgrades++
		}
	}
	if previews != 2 || upgrades != 1 {
		t.Fatal("history lost or duplicated", string(events.Body.Bytes()))
	}
	s.Close()
	s = openJournal(t, cfg)
	repeated, err = s.upgradeJob(context.Background(), j.ID, req)
	must(t, err)
	if repeated != updated {
		t.Fatal("receipt lost on restart")
	}
}

func TestJobUpgradeAdmissionAndRollback(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	first := journalRelease(t, s, s.runtimeDigest)
	j, err := s.createJob("Guarded", first.ID)
	must(t, err)
	next := journalRelease(t, s, s.runtimeDigest)
	req := UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true}
	trigger, err := s.configureTrigger(j.ID, "", 0, eventTriggerConfig())
	must(t, err)
	trigger, _, err = s.setTriggerState(trigger.ID, trigger.Revision, false, true)
	must(t, err)
	if _, err = s.upgradeJob(context.Background(), j.ID, req); err == nil {
		t.Fatal("enabled automation accepted")
	}
	trigger, _, err = s.setTriggerState(trigger.ID, trigger.Revision, true, false)
	must(t, err)
	s.cfg.RequireTestedReleases = true
	if _, err = s.upgradeJob(context.Background(), j.ID, req); err == nil {
		t.Fatal("untested upgrade accepted")
	}
	s.cfg.RequireTestedReleases = false
	_, err = s.db.Exec(`CREATE TRIGGER reject_upgrade BEFORE INSERT ON events WHEN NEW.kind='job.upgraded' BEGIN SELECT RAISE(ABORT,'test'); END`)
	must(t, err)
	if _, err = s.upgradeJob(context.Background(), j.ID, req); err == nil {
		t.Fatal("event failure ignored")
	}
	held, err := s.job(j.ID)
	must(t, err)
	if held != j {
		t.Fatal("partial upgrade", held)
	}
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM job_revisions WHERE job_id=?", j.ID).Scan(&count))
	if count != 1 {
		t.Fatal("partial receipt")
	}
	_, err = s.db.Exec("DROP TRIGGER reject_upgrade")
	must(t, err)
	_, err = s.upgradeJob(context.Background(), j.ID, req)
	must(t, err)
	if _, _, err = s.setTriggerState(trigger.ID, trigger.Revision, false, true); err == nil {
		t.Fatal("a pre-upgrade trigger review enabled the new release")
	}
}

func TestJobUpgradeUsesRuntimeVersions(t *testing.T) {
	cfg := testConfig(t)
	cfg.disableDispatcher = true
	cfg.disableAutomation = true
	s, err := Open(cfg)
	must(t, err)
	defer s.Close()
	first := testRelease(t, s)
	j, err := s.createJob("Versioned", first.ID)
	must(t, err)
	for _, tc := range []struct {
		version string
		accept  bool
	}{{first.PackVersion, false}, {"0.0.0", false}, {"999999999999999999999999.0.0", true}} {
		pack := strings.Replace(first.Pack, `"version": "`+first.PackVersion+`"`, `"version": "`+tc.version+`"`, 1)
		var p map[string]json.RawMessage
		must(t, json.Unmarshal([]byte(pack), &p))
		p["version"] = encode(tc.version)
		p["title"] = encode("Changed")
		pack = string(encode(p))
		next, err := s.preview(context.Background(), PreviewRequest{Pack: pack, Input: sample()})
		must(t, err)
		_, err = s.upgradeJob(context.Background(), j.ID, UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true})
		if (err == nil) != tc.accept {
			t.Fatalf("version %s: %v", tc.version, err)
		}
	}
}

func TestJobUpgradePreservesReceiptsAcrossFurtherRevisions(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	first := journalRelease(t, s, s.runtimeDigest)
	j, e := s.createJob("History", first.ID)
	must(t, e)
	second := journalRelease(t, s, s.runtimeDigest)
	req := UpgradeJobRequest{ReleaseID: second.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true}
	receipt, e := s.upgradeJob(context.Background(), j.ID, req)
	must(t, e)
	third := journalRelease(t, s, s.runtimeDigest)
	_, e = s.upgradeJob(context.Background(), j.ID, UpgradeJobRequest{ReleaseID: third.ID, BaseReleaseID: second.ID, BaseRevision: 2, Reviewed: true})
	must(t, e)
	s.Close()
	s = openJournal(t, cfg)
	s.unhealthy.Store(true)
	replay, e := s.upgradeJob(context.Background(), j.ID, req)
	must(t, e)
	if replay != receipt {
		t.Fatal("earlier receipt was replaced", replay)
	}
	current, e := s.job(j.ID)
	must(t, e)
	if current.Revision != 3 || current.ReleaseID != third.ID {
		t.Fatal("replay rewound job", current)
	}
}

func TestJobUpgradeRefusesEveryPendingOccurrence(t *testing.T) {
	for _, state := range []string{"accepted", "preparing", "waiting", "needs-attention"} {
		t.Run(state, func(t *testing.T) {
			cfg := journalConfig(t)
			s := openJournal(t, cfg)
			defer s.Close()
			first := journalRelease(t, s, s.runtimeDigest)
			j, e := s.createJob("Pending", first.ID)
			must(t, e)
			next := journalRelease(t, s, s.runtimeDigest)
			_, e = s.db.Exec("INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES(?,?,?,?,?,?,?)", "occ-pending", j.ID, "test", "event", "digest", state, `{}`)
			must(t, e)
			req := UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true}
			if _, e = s.upgradeJob(context.Background(), j.ID, req); e == nil {
				t.Fatal("pending occurrence accepted")
			}
			_, e = s.db.Exec("UPDATE occurrences SET state='cancelled'")
			must(t, e)
			_, e = s.upgradeJob(context.Background(), j.ID, req)
			must(t, e)
		})
	}
}

func TestJobRevisionMigrationFromSchemaTwo(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	first := journalRelease(t, s, s.runtimeDigest)
	j, e := s.createJob("Existing", first.ID)
	must(t, e)
	tr, e := s.configureTrigger(j.ID, "", 0, eventTriggerConfig())
	must(t, e)
	_, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(t, e)
	_, e = s.db.Exec("DROP TABLE job_creations; DROP TABLE job_revisions; UPDATE metadata SET value='2' WHERE key='schema'")
	must(t, e)
	s.Close()
	s = openJournal(t, cfg)
	defer s.Close()
	receipt, e := s.createJob("Existing", first.ID)
	must(t, e)
	if receipt != j {
		t.Fatal("creation receipt changed during migration")
	}
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM job_revisions WHERE job_id=? AND revision=1", j.ID).Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
	restored, hash, e := s.trigger(tr.ID)
	must(t, e)
	if hash == "" || restored.keyRevision == nil {
		t.Fatal("trigger credential revision lost")
	}
	must(t, s.db.QueryRow("SELECT count(*) FROM events WHERE kind='journal.began'").Scan(&count))
	if count != 1 {
		t.Fatal("migration restarted journal")
	}
}

func TestJobCreationRefusesUsedPackIdentityMutation(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	r := journalRelease(t, s, s.runtimeDigest)
	_, e := s.createJob("Original", r.ID)
	must(t, e)
	r.ID = id("release_")
	r.Pack = strings.Replace(r.Pack, "Journal", "Changed", 1)
	r.PackDigest = digest([]byte(r.Pack))
	must(t, s.saveRelease(r))
	if _, e = s.createJob("Changed", r.ID); e == nil {
		t.Fatal("published identity changed")
	}
}

func TestUpgradeReceiptReadOnlyRecovery(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	first := journalRelease(t, s, s.runtimeDigest)
	j, err := s.createJob("Recovery", first.ID)
	must(t, err)
	next := journalRelease(t, s, s.runtimeDigest)
	req := UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true}
	path := "/v1/jobs/" + j.ID + "/upgrade-receipt?baseRevision=1&baseReleaseId=" + first.ID + "&releaseId=" + next.ID
	if r := serve(t, s, "GET", path, "", nil); r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	held, err := s.job(j.ID)
	must(t, err)
	if held != j {
		t.Fatal("receipt read changed job")
	}
	updated, err := s.upgradeJob(context.Background(), j.ID, req)
	must(t, err)
	newer := journalRelease(t, s, s.runtimeDigest)
	_, err = s.upgradeJob(context.Background(), j.ID, UpgradeJobRequest{ReleaseID: newer.ID, BaseReleaseID: next.ID, BaseRevision: 2, Reviewed: true})
	must(t, err)
	s.Close()
	s = openJournal(t, cfg)
	s.unhealthy.Store(true)
	r := serve(t, s, "GET", path, "", nil)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	var receipt Job
	must(t, json.Unmarshal(r.Body.Bytes(), &receipt))
	if receipt != updated {
		t.Fatal("historical receipt changed")
	}
	if r := serve(t, s, "GET", path+"&releaseId=other", "", nil); r.Code != 422 {
		t.Fatal("duplicate query accepted", r.Code)
	}
	if r := serve(t, s, "GET", strings.Replace(path, next.ID, first.ID, 1), "", nil); r.Code != 409 {
		t.Fatal("different intent accepted", r.Code)
	}
}
