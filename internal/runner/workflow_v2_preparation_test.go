package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func workflowV2ActualRelease(t *testing.T, release Release, published, binding, root []byte, profiles []InputProfile, pointer string) workflowV2Inventory {
	t.Helper()
	raw, in, _ := workflowFreezeFixture(t, published, binding, profiles)
	in.Release = release
	doc := decoded(t, raw).(map[string]any)
	doc["workflowVersion"] = 2
	doc["limits"].(map[string]any)["maxAttempts"] = 3
	stage := doc["stages"].([]any)[0].(map[string]any)
	stage["pack"] = map[string]any{"id": release.PackID, "digest": release.PackDigest}
	stage["outputs"] = map[string]any{"pointer": pointer, "cardinality": "one"}
	w, err := newWorkflowV2Inventory(encode(doc), root, "preparation-v2", "root", []workflowFreezeInput{in}, profiles, release.RuntimeDigest)
	if err != nil {
		t.Fatal(err)
	}
	return workflowV2StartRoot(t, w)
}
func TestWorkflowV2TerminalPreparationUsesExactCheckInstantAndClosedFailures(t *testing.T) {
	bundle, profiles, _ := completedExport(t, "record")
	source := bundle.Run.Input.Source
	valid := source.Sources["vendor"]
	var response map[string]any
	_ = json.Unmarshal(valid.Response, &response)
	receipt := response["receipt"].(map[string]any)
	served, err := time.Parse(time.RFC3339, receipt["servedAt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"stale", "invalid", "unknown-form", "pending"} {
		t.Run(kind, func(t *testing.T) {
			w := workflowV2ActualRelease(t, bundle.Release, encode(source.Mapping), nil, source.Case, profiles, "/request")
			_, db := openWorkflowStoreTest(t, t.TempDir()+"/preparation.sqlite")
			defer db.Close()
			store, err := newWorkflowV2Store(db)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.create("main", w); err != nil {
				t.Fatal(err)
			}
			clock := served
			terminal := time.Time{}
			calls := 0
			driver := workflowV2Driver{Store: store, Profiles: profiles, Trace: workflowV2JourneyTrace(t, "preparation/"+kind), Now: func() time.Time { at := clock; clock = clock.Add(time.Second); return at }}
			driver.Evaluate = func(context.Context, Release, Input, string) (workflowEvaluation, error) {
				t.Fatal("evaluated terminal/pending failure")
				return workflowEvaluation{}, nil
			}
			driver.Acquire = func(context.Context, workflowAcquisition) (SourceValue, error) {
				calls++
				switch kind {
				case "stale":
					clock = served.Add(301 * time.Second)
					terminal = clock
					return valid, nil
				case "invalid":
					terminal = clock
					var changed map[string]any
					_ = json.Unmarshal(valid.Response, &changed)
					changed["receipt"].(map[string]any)["signature"] = string(bytes.Repeat([]byte("0"), 128))
					return SourceValue{Response: encode(changed)}, nil
				default:
					return SourceValue{Response: []byte(`{}`)}, nil
				}
			}
			if kind == "pending" {
				input, err := workflowAttemptInput(w.Core, "root")
				if err != nil {
					t.Fatal(err)
				}
				plan, err := nextInput(input, profiles, served)
				if err != nil || plan.Next == nil || workflowV2PreparationFailure(Run{State: "failed", Input: input, FinishedAt: served.Format(time.RFC3339)}, profiles) != "" {
					t.Fatal("pending acquisition classified terminal", err)
				}
				driver.Acquire = nil
			}
			done, err := driver.run(context.Background(), "preparation-v2", "main")
			if kind == "unknown-form" {
				if err == nil || done.Proof.closed() || done.History["root"].State != "interrupted" {
					t.Fatal("unknown error converted to preparation failure", err)
				}
				held, holdErr := store.custody("preparation-v2", "main")
				if holdErr != nil || len(held) == 0 {
					t.Fatal("lost unread material")
				}
				workflowV2ExportJourney(t, "preparation/unknown-form/last-valid-prefix", done, profiles)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			instance := done.History["root"]
			attempt := instance.Attempts[0]
			if kind == "pending" {
				if calls != 0 || instance.Failure == nil || *instance.Failure != "acquisition-failed" {
					t.Fatal("missing acquisition became prep failure")
				}
				workflowV2ExportJourney(t, "preparation/pending/acquisition-failed", done, profiles)
				return
			}
			if calls != 1 || instance.Failure == nil || *instance.Failure != "preparation-failed" || attempt.PreparationDigest != nil || attempt.AuditDigest != nil || attempt.RunDigest == nil {
				t.Fatal("terminal phase")
			}
			var retained VerificationBundle
			_ = strictJSON(done.Core.Artifacts["run/"+*attempt.RunDigest], &retained)
			if retained.Run.FinishedAt != terminal.Format(time.RFC3339) || workflowV2PreparationFailure(retained.Run, profiles) != "preparation-failed" {
				t.Fatal("terminal instant overwritten", retained.Run.FinishedAt, terminal)
			}
			files, _ := done.recordingFiles()
			if files["instances/root/attempts/1/verified-at.txt"] != nil || files["instances/root/attempts/1/preparation.json"] != nil {
				t.Fatal("fabricated failed preparation")
			}
			if len(retained.Run.Input.Source.Sources["vendor"].Response) == 0 {
				t.Fatal("lost actual receipt")
			}
			for _, badTime := range []string{"", "null", "2026-01-01T00:00:00.1Z", "2026-01-01T00:00:00+00:00", "2026-02-30T00:00:00Z"} {
				bad := retained.Run
				bad.FinishedAt = badTime
				if workflowV2PreparationFailure(bad, profiles) != "" {
					t.Fatal("substituted failure clock")
				}
			}
			workflowV2ExportJourney(t, "preparation/"+kind+"/failed", done, profiles)
		})
	}
}
func TestWorkflowV2RetainedResearchMissingResultFailsAndFreshExplicitRetrySucceeds(t *testing.T) {
	docs := researchDocsOf(t, gtmPlan)
	validInput := docs.input(t)
	cfg := testConfig(t)
	cfg.InputProfiles = docs.profiles
	cfg.disableAutomation = true
	service, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	release, err := service.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: validInput})
	if err != nil {
		t.Fatal(err)
	}
	w := workflowV2ActualRelease(t, release, validInput.Source.Import.Dialect, validInput.Source.Import.Binding, validInput.Source.Case, docs.profiles, "/company")
	store, err := newWorkflowV2Store(service.db)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.create("main", w); err != nil {
		t.Fatal(err)
	}
	var missing map[string]any
	_ = json.Unmarshal(validInput.Source.Sources["person"].Response, &missing)
	record := missing["result"].(map[string]any)
	delete(record["responses"].([]any)[0].(map[string]any), "result")
	at := docs.at
	driver := service.workflowV2Driver(store)
	driver.Now = func() time.Time { return at }
	driver.Trace = workflowV2JourneyTrace(t, "preparation/retained-missing")
	calls := 0
	driver.Acquire = func(_ context.Context, request workflowAcquisition) (SourceValue, error) {
		calls++
		if request.Next.Name == "person" {
			return SourceValue{Response: encode(missing)}, nil
		}
		return validInput.Source.Sources[request.Next.Name], nil
	}
	failed, err := driver.run(context.Background(), "preparation-v2", "main")
	if err != nil {
		t.Fatal(err)
	}
	instance := failed.History["root"]
	if calls != 2 || instance.Failure == nil || *instance.Failure != "preparation-failed" || instance.Attempts[0].PreparationDigest != nil {
		t.Fatal("inside-retained missing response")
	}
	var retained VerificationBundle
	_ = strictJSON(failed.Core.Artifacts["run/"+*instance.Attempts[0].RunDigest], &retained)
	if workflowV2PreparationFailure(retained.Run, docs.profiles) != "preparation-failed" || len(retained.Run.Input.Source.Sources) != 2 {
		t.Fatal("failure not independently derivable at retained instant")
	}
	oldAttempt := encode(instance.Attempts[0])
	workflowV2ExportJourney(t, "preparation/retained-missing/failed", failed, docs.profiles)
	at = at.Add(time.Second)
	fresh := docs.h.researchInput(t, gtmPlan(docs.h), at) // New real stand-in calls and newly signed receipts; no live model.
	if bytes.Equal(fresh.Source.Sources["person"].Response, validInput.Source.Sources["person"].Response) {
		t.Fatal("retry reused receipts")
	}
	retry, err := beginWorkflowV2Retry(failed, workflowV2Begin{"root", 2, &workflowRetry{Actor: "operator", At: at.Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := store.load("preparation-v2", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.save("main", retry, revision); err != nil {
		t.Fatal(err)
	}
	driver.Acquire = func(_ context.Context, request workflowAcquisition) (SourceValue, error) {
		calls++
		return fresh.Source.Sources[request.Next.Name], nil
	}
	done, err := driver.run(context.Background(), "preparation-v2", "main")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || done.History["root"].State != "completed" || len(done.History["root"].Attempts) != 2 || !bytes.Equal(oldAttempt, encode(done.History["root"].Attempts[0])) {
		t.Fatal("retry changed old material or replayed implicitly")
	}
	workflowV2ExportJourney(t, "preparation/retained-missing/retried", done, docs.profiles)
}
