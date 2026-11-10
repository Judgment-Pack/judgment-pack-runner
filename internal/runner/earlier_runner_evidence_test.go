package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The refusals' bytes, written by hand.
const (
	wantPreparedByEarlierRunner = "This run was prepared by a Runner before 0.28.0, which left out its empty evidence-availability document. This Runner evaluates a dialect run only with the evidence its preparation derives, {} here, so the run was not evaluated. Submit its input again: this Runner's preparation derives the document."
	wantExportEvidenceLeftOut   = "the export is in the shape a Runner before 0.28.0 made: its run retained no evidence-availability document where the derivation is {}, and this Runner does not verify such an export"
	wantRetainedInputsDiffer    = "retained facts or evidence do not match verified derivation"
)

// leaveOutStored rewrites a stored run's record without members of its
// input, as a Runner before 0.28.0 stored a dialect run whose
// evidence-availability document was empty: it wrote no evidence member.
func leaveOutStored(t *testing.T, s *Service, runID string, members []string) {
	t.Helper()
	var record string
	if err := s.db.QueryRow("SELECT record FROM runs WHERE id=?", runID).Scan(&record); err != nil {
		t.Fatal(err)
	}
	var run map[string]any
	if err := json.Unmarshal([]byte(record), &run); err != nil {
		t.Fatal(err)
	}
	input := run["input"].(map[string]any)
	for _, member := range members {
		if _, held := input[member]; !held {
			t.Fatalf("the stored run holds no %s to leave out: %s", member, record)
		}
		delete(input, member)
	}
	if _, err := s.db.Exec("UPDATE runs SET record=? WHERE id=?", string(encode(run)), runID); err != nil {
		t.Fatal(err)
	}
}

// A dialect run queued by v0.27.0 whose evidence-availability document is
// empty retained no evidence member. This Runner derives {} for it and
// evaluates a run only with what its preparation derives: the run is not
// evaluated, without evidence or with {} filled in, and ends failed with the
// reason prepared-by-earlier-runner. verify-run refuses its export by its own
// sentence, not the mismatch sentence. Any other retained input that is not
// the derivation is refused with the mismatch sentence and no reason.
func TestQueuedRunFromAnEarlierRunnerIsNotEvaluated(t *testing.T) {
	type shape struct {
		name            string
		leftOut         []string
		emptyEvidence   bool
		reason, problem string
		verifySentence  string
	}
	for _, c := range []shape{
		{"v0.27.0 empty evidence", []string{"evidence"}, true, runPreparedByEarlierRunner, wantPreparedByEarlierRunner, wantExportEvidenceLeftOut},
		{"evidence derived and left out", []string{"evidence"}, false, "", wantRetainedInputsDiffer, wantRetainedInputsDiffer},
		{"facts left out", []string{"facts"}, true, "", wantRetainedInputsDiffer, wantRetainedInputsDiffer},
		{"facts and empty evidence left out", []string{"facts", "evidence"}, true, "", wantRetainedInputsDiffer, wantRetainedInputsDiffer},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := journalConfig(t)
			s := openJournal(t, cfg)
			defer s.Close()
			input, p, _, _ := v2Fixture(t)
			if c.emptyEvidence {
				// The source's read targets facts alone: no target names
				// a requirement, and the document is {}.
				input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/vendor/description", Source: "/description"}}, Evidence: []EvidenceMapping{}}}
			}
			if _, err := s.registerInputProfile(p); err != nil {
				t.Fatal(err)
			}
			runtime := completingRuntime(t, cfg.Dir)
			mapping := input.Source.Mapping
			release := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), PackID: "journal", PackVersion: "1.0.0", Title: "Journal", RuntimeDigest: runtime, CreatedAt: now(), Sample: input, Tests: "not-run", InputMapping: &mapping, InputProfiles: []InputProfile{p}}
			if err := s.saveRelease(release); err != nil {
				t.Fatal(err)
			}
			job, err := s.createJob("Vendors", release.ID)
			if err != nil {
				t.Fatal(err)
			}
			queued, _, err := s.submit(job.ID, "before-0.28.0", input)
			if err != nil || queued.State != "queued" {
				t.Fatal(queued.State, err)
			}
			if wantEvidence := map[bool]string{true: `{}`, false: `{"registry":"present"}`}[c.emptyEvidence]; string(queued.Input.Evidence) != wantEvidence {
				t.Fatalf("this Runner queued evidence %s, not %s", queued.Input.Evidence, wantEvidence)
			}
			leaveOutStored(t, s, queued.ID, c.leftOut)

			if more, err := s.dispatchNext(context.Background()); !more || err != nil {
				t.Fatal(more, err)
			}
			ended, err := s.run(queued.ID)
			if err != nil || ended.State != "failed" || ended.Reason != c.reason || ended.Problem != c.problem || ended.Result != nil || ended.Audit != nil {
				t.Fatalf("the queued run ended %s (reason %q), problem %q, result %s: %v", ended.State, ended.Reason, ended.Problem, ended.Result, err)
			}
			// No Runtime was invoked: the attempt directory was never made.
			if _, err := os.Stat(filepath.Join(cfg.Dir, "attempts", queued.ID)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("the run reached the Runtime:", err)
			}
			if c.reason != "" {
				served := serve(t, s, "GET", "/v1/runs/"+queued.ID, "", nil)
				var answer map[string]any
				if served.Code != 200 || json.Unmarshal(served.Body.Bytes(), &answer) != nil || answer["reason"] != c.reason {
					t.Fatal(served.Code, served.Body.String())
				}
				doc := readOpenAPI(t)
				if problems := doc.check("run/reason", doc.schema("Run")["properties"].(map[string]any)["reason"].(map[string]any), answer["reason"]); len(problems) != 0 {
					t.Fatal(problems)
				}
			}

			// verify-run: the export of the run as stored.
			profiles := []InputProfile{p}
			bundle := verificationExport(release, ended, 2)
			if _, err := verifyPreparation(bundle, profiles, nil); err == nil || err.Error() != c.verifySentence {
				t.Fatalf("verifyPreparation refused with %v, not %q", err, c.verifySentence)
			}
			if _, err := VerifyInputs(encode(bundle), profiles, bundle.ReleaseDigest); err == nil || err.Error() != c.verifySentence {
				t.Fatalf("VerifyInputs refused with %v, not %q", err, c.verifySentence)
			}
			// The same export with the derived inputs present verifies its
			// inputs: the refusal is of what was left out alone.
			whole := bundle
			whole.Run.Input = queued.Input
			if _, err := verifyPreparation(whole, profiles, nil); err != nil {
				t.Fatal("the export with its derived inputs does not verify:", err)
			}
		})
	}
}
