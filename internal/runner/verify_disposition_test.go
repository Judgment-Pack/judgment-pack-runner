package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// completedExport completes one mapping v2 run with the real Runtime and
// returns its export. With an operation source, the operational evaluation is
// given a citation, which a rehearsal never is.
func completedExport(t *testing.T, operation bool) (VerificationBundle, []InputProfile, string) {
	t.Helper()
	cfg := testConfig(t)
	copy := CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}}}
	// The case carries a member named like "facts" but for case. It is data, not
	// a field of the export, and the export still verifies.
	data := encode(sample())
	data = append(data[:len(data)-1], `,"Facts":{"note":"data"}}`...)
	input := Input{Source: &SourceInput{Mapping: InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &CaseMapping{Facts: copy.Facts, Evidence: copy.Evidence}}, Case: data}}
	if operation {
		fixture, p, key, at := v2Fixture(t)
		input = fixture
		cfg.InputProfiles = []InputProfile{p}
		input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
		input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &copy}
		input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), encode(sample()), at, 0)}
	}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	pack, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	release, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.createJob("Re-executed", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	run, _, e := s.submit(job.ID, "one", input)
	if e != nil {
		t.Fatal(e)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "completed" || operation != bytes.Contains(done.Audit, []byte(`"cites"`)) {
		t.Fatal(done.State, done.Problem, string(done.Audit))
	}
	return VerificationBundle{2, releaseDigest(release), release, done}, cfg.InputProfiles, cfg.Dir
}

// standIn writes a script that answers an evaluation with output, using shell
// builtins only, and returns its absolute path and digest.
func standIn(t *testing.T, output []byte, extra string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\nprintf '%s\\n' '" + strings.ReplaceAll(string(output), "'", `'\''`) + "'\n" + extra
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return path, digest([]byte(script))
}

// rehearsalOf is what the release's Runtime answers when the retained run's
// inputs are evaluated again as a rehearsal.
func rehearsalOf(t *testing.T, run Run) []byte {
	t.Helper()
	var out map[string]json.RawMessage
	if e := json.Unmarshal(run.Result, &out); e != nil {
		t.Fatal(e)
	}
	out["rehearsal"] = json.RawMessage("true")
	return encode(out)
}

// A tree listing with sizes and times: anything written under dir changes it.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	e := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintln(&b, path, info.Mode(), info.ModTime(), info.Size())
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return b.String()
}

func TestVerifyDispositionReExecutesWithTheReleaseRuntime(t *testing.T) {
	bin := os.Getenv("JPACK_TEST_BIN")
	for name, operation := range map[string]bool{"case": false, "operation with a citation": true} {
		t.Run(name, func(t *testing.T) {
			bundle, profiles, store := completedExport(t, operation)
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			before := tree(t, store)
			if e := VerifyDisposition(context.Background(), encode(bundle), profiles, bundle.ReleaseDigest, bin); e != nil {
				t.Fatal(e)
			}
			// The rehearsal ran outside the store and left nothing behind.
			if tree(t, store) != before {
				t.Fatal("re-execution wrote to the runner's store")
			}
			if left, _ := os.ReadDir(scratch); len(left) != 0 {
				t.Fatal("re-execution left files", left)
			}
			// Without a runtime, the inputs check is unchanged and still passes.
			if e := VerifyRun(encode(bundle), profiles, bundle.ReleaseDigest); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestVerifyDispositionRefusesWhatItCannotStandBehind(t *testing.T) {
	bin := os.Getenv("JPACK_TEST_BIN")
	bundle, profiles, _ := completedExport(t, false)
	t.Setenv("TMPDIR", t.TempDir())
	check := func(b VerificationBundle, runtime string) error {
		return VerifyDisposition(context.Background(), encode(b), profiles, b.ReleaseDigest, runtime)
	}
	decided := []byte(`"outcomeId":"proceed"`)
	other := []byte(`"outcomeId":"decline-redirect"`)
	if !bytes.Contains(bundle.Run.Result, decided) || !bytes.Contains(bundle.Run.Audit, decided) {
		t.Fatal("fixture has no disposition to change")
	}
	t.Run("result changed after the run", func(t *testing.T) {
		b := bundle
		b.Run.Result = bytes.Replace(b.Run.Result, decided, other, 1)
		if e := VerifyRun(encode(b), profiles, b.ReleaseDigest); e != nil {
			t.Fatal("inputs are intact", e)
		}
		if e := check(b, bin); !errors.Is(e, ErrDispositionDiffers) {
			t.Fatal(e)
		}
	})
	t.Run("audit record changed after the run", func(t *testing.T) {
		b := bundle
		b.Run.Audit = bytes.Replace(b.Run.Audit, decided, other, 1)
		if e := check(b, bin); !errors.Is(e, ErrDispositionDiffers) {
			t.Fatal(e)
		}
	})
	t.Run("run did not complete", func(t *testing.T) {
		b := bundle
		b.Run.State = "interrupted"
		if e := check(b, bin); e == nil || !strings.Contains(e.Error(), "only a completed run") {
			t.Fatal(e)
		}
	})
	// A stand-in that answers exactly what the release's Runtime would is still
	// not the release's Runtime.
	t.Run("another executable", func(t *testing.T) {
		path, _ := standIn(t, rehearsalOf(t, bundle.Run), "")
		if e := check(bundle, path); e == nil || !strings.Contains(e.Error(), "not the Runtime the release froze") {
			t.Fatal(e)
		}
	})
	// The remaining cases pin a stand-in as the release's Runtime, and trust
	// that release, to hold what is checked of whatever runs.
	pinned := func(t *testing.T, output []byte, extra string) (VerificationBundle, string) {
		path, hash := standIn(t, output, extra)
		b := bundle
		b.Release.RuntimeDigest = hash
		b.ReleaseDigest = releaseDigest(b.Release)
		return b, path
	}
	t.Run("the pinned stand-in answers as retained", func(t *testing.T) {
		if e := check(pinned(t, rehearsalOf(t, bundle.Run), "")); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("the pinned stand-in decides otherwise", func(t *testing.T) {
		if e := check(pinned(t, bytes.Replace(rehearsalOf(t, bundle.Run), decided, other, 1), "")); !errors.Is(e, ErrDispositionDiffers) {
			t.Fatal(e)
		}
	})
	t.Run("the pinned stand-in leaves an audit record", func(t *testing.T) {
		if e := check(pinned(t, rehearsalOf(t, bundle.Run), ": > audit\n")); e == nil || !strings.Contains(e.Error(), "left an audit record") {
			t.Fatal(e)
		}
	})
	t.Run("the pinned stand-in does not answer as a rehearsal", func(t *testing.T) {
		if e := check(pinned(t, bundle.Run.Result, "")); e == nil || errors.Is(e, ErrDispositionDiffers) {
			t.Fatal(e)
		}
	})
	t.Run("the pinned stand-in is not asked for a rehearsal", func(t *testing.T) {
		refuse := "case \" $* \" in *' --rehearsal '*) ;; *) exit 3 ;; esac\n"
		b, path := pinned(t, rehearsalOf(t, bundle.Run), "")
		script, _ := os.ReadFile(path)
		guarded := strings.Replace(string(script), "#!/bin/sh\n", "#!/bin/sh\n"+refuse, 1)
		if e := os.WriteFile(path, []byte(guarded), 0700); e != nil {
			t.Fatal(e)
		}
		b.Release.RuntimeDigest = digest([]byte(guarded))
		b.ReleaseDigest = releaseDigest(b.Release)
		if e := check(b, path); e != nil {
			t.Fatal("rehearsal was not requested", e)
		}
	})
}

// Go's decoder takes a member named like a field but for case as that field,
// and of two such members the last. A reader of the export takes the exact
// name. Neither a changed disposition nor changed inputs may hide behind one.
func TestVerifyReadsTheNamesAReaderReads(t *testing.T) {
	bin := os.Getenv("JPACK_TEST_BIN")
	bundle, profiles, _ := completedExport(t, false)
	t.Setenv("TMPDIR", t.TempDir())
	decided, other := []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`)
	// shadow appends to object a member named name, after the one it shadows.
	shadow := func(object []byte, name string, value []byte) []byte {
		out := append(bytes.Clone(object[:len(object)-1]), `,"`+name+`":`...)
		return append(append(out, value...), '}')
	}
	changedResult := bytes.Replace(bundle.Run.Result, decided, other, 1)
	changedAudit := bytes.Replace(bundle.Run.Audit, decided, other, 1)
	inResult, inAudit, inRun := bundle, bundle, bundle
	inResult.Run.Result = shadow(changedResult, "Disposition", member(bundle.Run.Result, "disposition"))
	inAudit.Run.Audit = shadow(changedAudit, "Disposition", member(bundle.Run.Audit, "disposition"))
	inRun.Run.Result = changedResult
	run := encode(inRun)
	run = append(shadow(run[:len(run)-1], "Result", bundle.Run.Result), '}')
	for name, raw := range map[string][]byte{"result": encode(inResult), "audit record": encode(inAudit), "run": run} {
		t.Run("disposition shadowed in the "+name, func(t *testing.T) {
			if e := VerifyDisposition(context.Background(), raw, profiles, bundle.ReleaseDigest, bin); e == nil {
				t.Fatal("a changed disposition verified")
			}
		})
	}
	t.Run("audit inputs shadowed", func(t *testing.T) {
		b := bundle
		inputs := member(b.Run.Audit, "inputs")
		changed := bytes.Replace(inputs, []byte(`"completeness":"complete"`), []byte(`"completeness":"incomplete"`), 1)
		if bytes.Equal(changed, inputs) {
			t.Fatal("fixture has no input to change")
		}
		b.Run.Audit = shadow(bytes.Replace(b.Run.Audit, inputs, changed, 1), "Inputs", inputs)
		if e := VerifyRun(encode(b), profiles, b.ReleaseDigest); e == nil {
			t.Fatal("changed audit inputs verified")
		}
	})
}
