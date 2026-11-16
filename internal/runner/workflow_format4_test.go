package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Spec's workflow stages of dialect formats 4.0.0 and 4.1.0 (RFC 0020,
// the amendment proposal of 2026-11-15, staged for specVersion 0.6.0),
// copied byte for byte at its commit 0d5bdea79cea2f2c00fd2c802bac476e6386c002
// (vectors/workflow-format-4).
const workflowFormat4Vectors = "../../vectors/workflow-format-4/spec"

// The SHA-256 of each file at that commit, taken from the Spec's bytes (git
// show), not from anything this repository computes.
var workflowFormat4Pins = map[string]string{
	"README.md":                       "2d23130de5d033393dd3ca2962a4a020366c8f19b3fbf8b972e101488a658820",
	"manifest.json":                   "858ae84e73cbfcba4d54bd8e3934746d06866f1dd200e2631d75868b473ec392",
	"stage-4.0.0.release.json":        "f885c319e58f2713e9fefba980e0b5efe29bf67951b34d333bb1f25daae493cb",
	"stage-4.0.0.runner-release.json": "caf93fe15fa555d7fdc58f2c9dbb7c18e6318c68eff49e27b7d1c9789904bcc0",
	"stage-4.0.0.workflow.json":       "c3eec9348f3f1df52d275de8c83633c55fe7312694d81f4df0d4709893686166",
	"stage-4.1.0.release.json":        "00efefbdb04d0f1ad5f5e552a94b91aca54204cfb52de1c3506cd3e5049da46e",
	"stage-4.1.0.workflow.json":       "e0e8251bdb01342de2222a6282ce0057edeb119ebfd3d7bf30be4b9e4b78306b",
}

// The stages' dialects are the staged format vectors' own, which this
// repository vendors beside their format's other vectors: the manifest names
// each under the Spec's conformance/dialect/, read here from those copies.
var workflowFormat4Dialects = map[string]string{
	"format-4/":   "../../vectors/dialect-format-4/spec/",
	"format-4.1/": "../../vectors/dialect-format-4.1/spec/",
}

type workflowFormat4Case struct {
	Name          string `json:"name"`
	FormatVersion string `json:"formatVersion"`
	Dialect       string `json:"dialect"`
	Workflow      string `json:"workflow"`
	Release       string `json:"release"`
	DialectDigest string `json:"dialectDigest"`
	ReleaseDigest string `json:"releaseDigest"`
	Expected      struct {
		StagedSchema    string `json:"stagedSchema"`
		PublishedSchema string `json:"publishedSchema"`
		WorkflowReader  string `json:"workflowReader"`
	} `json:"expected"`
}

func workflowFormat4File(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workflowFormat4Vectors, name))
	must(t, err)
	return b
}

func workflowFormat4Dialect(t *testing.T, name string) []byte {
	t.Helper()
	for prefix, directory := range workflowFormat4Dialects {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			b, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(rest)))
			must(t, err)
			return b
		}
	}
	t.Fatal("the manifest names a dialect outside the vendored formats:", name)
	return nil
}

// TestWorkflowStagesOfFormat4AgreeWithTheSpecVectors holds Runner to each
// of the Spec's three cases (vectors/workflow-format-4): the stage's dialect
// is the one its reference pins, by the SHA-256 of its canon bytes, and is a
// stage's dialect Runner admits; the stage is read, its reference of the
// format; the release's digest is RFC 0020 section 1's, of the whole release
// as retained, which the pinned typed encoding does not give; a release with
// every member Runner retains is digested so from Runner's own encoding
// too; and Runner's workflow reader, a 0.5.x reader of the selector, refuses
// the stage version-unread.
func TestWorkflowStagesOfFormat4AgreeWithTheSpecVectors(t *testing.T) {
	for name, pin := range workflowFormat4Pins {
		if got := digest(workflowFormat4File(t, name)); got != "sha256:"+pin {
			t.Fatalf("%s is not the Spec's bytes at the pinned commit: %s", name, got)
		}
	}
	var manifest struct {
		Status string                `json:"status"`
		Cases  []workflowFormat4Case `json:"cases"`
	}
	must(t, json.Unmarshal(workflowFormat4File(t, "manifest.json"), &manifest))
	if manifest.Status != "staged" || len(manifest.Cases) != 3 {
		t.Fatalf("the manifest is not the three staged cases: %s, %d", manifest.Status, len(manifest.Cases))
	}
	agreed := 0
	for _, c := range manifest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dialect := workflowFormat4Dialect(t, c.Dialect)
			var m InputMapping
			must(t, strictJSON(dialect, &m))
			must(t, exactDialectForm(dialect, m))
			must(t, validateDialect(m, true))
			if m.FormatVersion != c.FormatVersion || digest(canonTest(t, dialect)) != c.DialectDigest || mappingV2Hash(m) != c.DialectDigest {
				t.Fatalf("the dialect is not the one the manifest pins: %s %s %s", m.FormatVersion, digest(canonTest(t, dialect)), mappingV2Hash(m))
			}
			// The stage is read, as the staged schema admits it
			// (expected.stagedSchema), its reference the dialect's. Two
			// members are set aside, which the schema admits and the
			// document does not hold to the prose: its sample, which the
			// Spec's reader test sets aside for the stage's form too and
			// Runner's executor does not read yet, in any format (issue
			// #170); and its selection, which spec/workflow.md gives only a
			// stage another stage consumes, and this stage is the
			// workflow's one, its result: Runner refuses that as the prose
			// does.
			workflow := workflowFormat4File(t, c.Workflow)
			form := decoded(t, workflow).(map[string]any)
			stage := form["stages"].([]any)[0].(map[string]any)
			delete(stage, "sample")
			if _, _, err := readWorkflow(encode(form)); !errors.Is(err, errWorkflowForm) {
				t.Fatal("a terminal stage's selection is read:", err)
			}
			delete(stage, "selection")
			doc, _, err := readWorkflow(encode(form))
			if err != nil || c.Expected.StagedSchema != "valid" {
				t.Fatal("the stage is not read:", err, c.Expected.StagedSchema)
			}
			ref := doc.Stages[0].Dialect
			if ref.Digest != c.DialectDigest || !workflowDialectMatches(ref, m) {
				t.Fatalf("the stage's reference is not the dialect's: %+v", ref)
			}
			// The release digest is RFC 0020 section 1's: SHA-256 of the
			// canon bytes of the whole release as retained, the preimage the
			// recording holds a release to (canonicalRelease).
			raw := workflowFormat4File(t, c.Release)
			preimage, err := canonicalRelease(raw)
			if err != nil || digest(preimage) != c.ReleaseDigest {
				t.Fatalf("the release digest is %s (%v), the manifest's %s", digest(preimage), err, c.ReleaseDigest)
			}
			var r Release
			must(t, strictJSON(raw, &r))
			// The release's inputMapping is the stage's dialect, and Runner's
			// typed encoding of it keeps every member, the case's rule and
			// a source's paging among them, which the encoding RFC 0020 once
			// pinned drops.
			members, err := exactMembers(raw)
			must(t, err)
			if digest(canonTest(t, members["inputMapping"])) != c.DialectDigest || mappingV2Hash(*r.InputMapping) != c.DialectDigest || r.PackDigest != doc.Stages[0].Pack.Digest {
				t.Fatal("the release's inputMapping or pack is not the stage's")
			}
			// A release with every member Runner's Release declares is
			// digested so from Runner's own typed encoding too: it keeps
			// every member, the case's rule among them.
			if c.Name == "stage-4.0.0-runner-members" {
				if got, err := releaseDigestOf(r); err != nil || got != c.ReleaseDigest {
					t.Fatalf("Runner's encoding of the release digests to %s (%v), the manifest's %s", got, err, c.ReleaseDigest)
				}
			}
			// The workflow reader stays a 0.5.x reader of the selector until
			// the format is accepted (RFC 0020, What stays open).
			if c.Expected.WorkflowReader != "version-unread" || !workflowReaderUnreadVersion("workflow.json", decoded(t, workflow)) || !workflowReaderUnreadVersion("stages/"+doc.Stages[0].ID+"/dialect.json", decoded(t, dialect)) {
				t.Fatal("the workflow reader reads the stage, or the manifest expects it to")
			}
			agreed++
		})
	}
	if agreed != len(manifest.Cases) {
		t.Fatalf("%d of %d cases agree", agreed, len(manifest.Cases))
	}
}

// runWorkflowFour freezes a one-stage workflow on the dialect given, its
// release on it, and runs the stage's one instance from the root case given
// with Runner's own driver, through the Gateway connection the feed serves,
// if any: the stage completes, its output the case's value at output, and
// the recording holds the release to the digest it was frozen with, RFC 0020
// section 1's. The stage is evaluated by the real Runtime, whose record binds
// the stage's preparation, as a stage's attempt requires. Runner's workflow
// reader, a 0.5.x reader of the selector, refuses the recording
// version-unread.
func runWorkflowFour(t *testing.T, dialect []byte, root, output string, feed *pagedFeed) workflowInventory {
	t.Helper()
	cfg := testConfig(t)
	profiles := []InputProfile{}
	if feed != nil {
		profiles = []InputProfile{feed.p}
		cfg.InputProfiles = profiles
		cfg.GatewayConnections = []GatewayConnection{{Profile: feed.p.ID, URL: feed.url}}
	}
	s, err := Open(cfg)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	runtime := s.runtimeDigest
	// The release is the triage pack's, checked by the Runtime on a
	// dialect of format 4.0.0, and made a release on the dialect given as a
	// saved release on it is: its inputMapping, profiles and sample.
	pack, err := os.ReadFile("testdata/triage.pack.json")
	must(t, err)
	var base InputMapping
	must(t, strictJSON([]byte(triageFour), &base))
	r, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: Input{Source: &SourceInput{Mapping: base, Case: json.RawMessage(triageFourCase)}}})
	must(t, err)
	var m InputMapping
	must(t, strictJSON(dialect, &m))
	r.InputMapping, r.InputProfiles, r.Sample = &m, profilesUsed(m, profiles), Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(root)}}
	r.Dialect, err = releaseDialectIdentity(r)
	must(t, err)
	must(t, fourReleaseDigestible(r))
	stage := map[string]any{"id": "changes", "dialect": map[string]any{"formatVersion": m.FormatVersion, "id": m.ArtifactID, "version": m.ArtifactVersion, "digest": digest(canonTest(t, dialect))},
		"pack": map[string]any{"id": r.PackID, "digest": r.PackDigest}, "input": map[string]any{"root": true}, "execution": "once", "caseBindings": []any{},
		"outputs": map[string]any{"pointer": output, "cardinality": "one"}, "relation": map[string]any{"label": "discovered-for", "direction": "output-to-parent"}}
	doc := map[string]any{"workflowVersion": 1, "stages": []any{stage}, "result": "changes", "limits": map[string]any{"maxStages": 1, "maxRecords": 16, "maxInstances": 8, "maxAttempts": 2, "maxBytes": 1 << 20}}
	w, err := newWorkflowInventory(encode(doc), []byte(root), "format-four", []workflowFreezeInput{{Published: dialect, Release: r}}, profiles, runtime)
	if err != nil {
		t.Fatal("the stage is not frozen:", err)
	}
	if w.Execution.Frozen[0].ReleaseDigest != releaseDigest(r) {
		t.Fatal("the stage is frozen with another release digest:", w.Execution.Frozen[0].ReleaseDigest)
	}
	done, err := s.workflowDriver(workflowDriverStore(t, w)).run(context.Background(), w.Execution.Execution, "instance-1", nil)
	if err != nil || done.Execution.Instances[0].State != "completed" || len(done.Execution.OutputSets[0].Records) != 1 {
		t.Fatalf("the stage did not complete: %v %+v", err, done.Execution.Instances)
	}
	files, err := done.recordingFiles()
	must(t, err)
	preimage, err := canonicalRelease(files["stages/changes/release.json"])
	if err != nil || digest(preimage) != w.Execution.Frozen[0].ReleaseDigest {
		t.Fatal("the recording's release is not the frozen one:", err)
	}
	workflowReaderWant(t, workflowReaderInput(t, done, profiles), ResultRefusal, "version-unread")
	return done
}

// TestAWorkflowStageOfFormat4Runs runs a stage of each format through
// Runner's workflow driver and the real Runtime: one of format 4.0.0, its
// case's rule over the case, and one of format 4.1.0 whose
// paged source is read in its loop, three pages in one session, its pages
// retained in the stage's run. Before #300 Runner refused the stage's
// reference, held the recording's release to the pinned encoding, and read
// a paged source as one call.
func TestAWorkflowStageOfFormat4Runs(t *testing.T) {
	t.Run("4.0.0", func(t *testing.T) {
		done := runWorkflowFour(t, []byte(triageFour), triageFourCase, "/request", nil)
		run := retainedWorkflowRun(t, done)
		if p := run.Input.Preparation; p == nil || p.formatVersion != "4.0.0" {
			t.Fatal("the stage's run has no preparation of format 4.0.0")
		}
		doc := readOpenAPI(t)
		if problems := doc.check("run", doc.schema("Run"), decodeNumbers(t, encode(run))); len(problems) > 0 {
			t.Fatal("the stage's run is not as openapi.json says:", problems)
		}
	})
	t.Run("4.1.0", func(t *testing.T) {
		feed := newPagedFeed(t, 3, nil)
		done := runWorkflowFour(t, []byte(pagedFeedDialect(feed.p)), `{"since":"seed","from":"2026-01-01T00:00:00Z","until":"2026-01-01T00:00:00Z","label":"feed"}`, "/changes", feed)
		calls := feed.taken()
		if len(calls) != 3 || calls[0].cursor != "" || calls[1].cursor != "ref-1" || calls[2].cursor != "ref-2" || calls[2].session != calls[0].session || !feed.isSealed(calls[0].session) {
			t.Fatalf("the paged source was not read in its loop, in one session: %+v", calls)
		}
		run := retainedWorkflowRun(t, done)
		if pages := run.Input.Source.Sources["changes"].Pages; len(pages) != 3 {
			t.Fatalf("the stage's run retains %d pages", len(pages))
		}
		doc := readOpenAPI(t)
		if problems := doc.check("run", doc.schema("Run"), decodeNumbers(t, encode(run))); len(problems) > 0 {
			t.Fatal("the stage's run is not as openapi.json says:", problems)
		}
	})
}
