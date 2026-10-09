package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// The saved version-5 vector is signed and witnessed. Only the displayed
// answer changes; the record, sidecar and held checkpoint remain untouched.
func TestVerifyRunBindsASignedDisposition(t *testing.T) {
	v := readNotMappedVector(t, "v1-file-mapping-run")
	extra := []string{"--chain", v.chain, "--expect", v.held, "--require-witnessed", "--public-key", key1, "--require-signed"}
	out, _, err := v.verify(extra...)
	if err != nil || !strings.Contains(out, `"retainedDisposition":"matches-audit"`) || !strings.Contains(out, `"status":"signed"`) || !strings.Contains(out, `"witnessed":true`) {
		t.Fatal(err, out)
	}
	raw, err := os.ReadFile(v.dir + "run.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle runner.VerificationBundle
	if err = json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(bundle.Run.Result)
	for name, disposition := range map[string]json.RawMessage{
		"fabricated": json.RawMessage(`{"kind":"outcome","outcomeId":"fabricated-approval"}`),
		"missing":    nil,
		"null":       json.RawMessage(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			var result map[string]json.RawMessage
			if err := json.Unmarshal(original, &result); err != nil {
				t.Fatal(err)
			}
			if disposition == nil {
				delete(result, "disposition")
			} else {
				result["disposition"] = disposition
			}
			bundle.Run.Result, _ = json.Marshal(result)
			changed, err := json.Marshal(bundle)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), "run.json")
			if err := os.WriteFile(file, changed, 0600); err != nil {
				t.Fatal(err)
			}
			for _, replay := range []bool{false, true} {
				args := append([]string{}, extra...)
				if replay {
					args = append(args, "--runtime", filepath.Join(t.TempDir(), "must-not-run"))
				}
				out, human, err := verifyFiles(file, v.dir+"profiles.json", v.digest, args...)
				if !errors.Is(err, runner.ErrAuditDispositionMismatch) || out != auditDispositionMismatchReport || human != "" {
					t.Fatal(err, out, human)
				}
			}
		})
	}
}

func TestVerificationCoverageInHelpAndGuide(t *testing.T) {
	var out, help bytes.Buffer
	if err := verifyRun([]string{"--help"}, &out, &help); !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/MAPPING-V2.md")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"help": help.String(), "guide": string(doc)} {
		if !strings.Contains(strings.Join(strings.Fields(text), " "), verificationCoverage) {
			t.Fatalf("%s omits the verification coverage sentences", name)
		}
	}
}
