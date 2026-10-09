package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
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
	originalAudit := bytes.Clone(bundle.Run.Audit)
	for name, disposition := range map[string]json.RawMessage{
		"fabricated":          json.RawMessage(`{"kind":"outcome","outcomeId":"fabricated-approval"}`),
		"missing":             nil,
		"null":                json.RawMessage(`null`),
		"result case sibling": json.RawMessage(`{"kind":"outcome","outcomeId":"proceed"}`),
		"audit case sibling":  json.RawMessage(`{"kind":"outcome","outcomeId":"proceed"}`),
	} {
		t.Run(name, func(t *testing.T) {
			bundle.Run.Audit = bytes.Clone(originalAudit)
			var result map[string]json.RawMessage
			if err := json.Unmarshal(original, &result); err != nil {
				t.Fatal(err)
			}
			if name == "result case sibling" {
				result["Disposition"] = json.RawMessage(`{"kind":"outcome","outcomeId":"fabricated-approval"}`)
			} else if name == "audit case sibling" {
				var audit map[string]json.RawMessage
				if err := json.Unmarshal(originalAudit, &audit); err != nil {
					t.Fatal(err)
				}
				audit["Disposition"] = json.RawMessage(`{"kind":"outcome","outcomeId":"fabricated-approval"}`)
				bundle.Run.Audit, _ = json.Marshal(audit)
			} else if disposition == nil {
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
				if strings.Contains(name, "case sibling") {
					if !errors.Is(err, runner.ErrDispositionMemberName) || out != "" || human != "" {
						t.Fatal(err, out, human)
					}
				} else if !errors.Is(err, runner.ErrAuditDispositionMismatch) || out != auditDispositionMismatchReport || human != "" {
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
	for name, check := range map[string]struct {
		typeOf reflect.Type
		want   []string
	}{
		"verification bundle": {reflect.TypeOf(runner.VerificationBundle{}), []string{"version", "releaseDigest", "release", "run", "inputs", "chain"}},
		"run":                 {reflect.TypeOf(runner.Run{}), []string{"trigger", "schemaVersion", "id", "jobId", "releaseId", "revision", "state", "input", "createdAt", "startedAt", "finishedAt", "requestedBy", "attempt", "result", "audit", "auditBytes", "auditSignatures", "problem"}},
	} {
		var got []string
		for i := 0; i < check.typeOf.NumField(); i++ {
			tag := strings.Split(check.typeOf.Field(i).Tag.Get("json"), ",")[0]
			if tag != "-" {
				got = append(got, tag)
			}
		}
		if !reflect.DeepEqual(got, check.want) {
			t.Fatalf("%s export members changed: got %v; update verification coverage and this list together", name, got)
		}
	}
}
