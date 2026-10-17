package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verify-run reports each preparation vector (vectors/preparation, runner
// #75) as the vectors' manifest records it: the report on standard output,
// byte for byte, and the refusal, with nothing else on standard error. The
// manifest's digest, and the vectors' bytes against the Runner's own
// computation, are held by TestPreparationVectorsAreByteExact
// (internal/runner). No Runtime is run: no vector asks for a re-execution.
func TestVerifyRunReportsEachPreparationVector(t *testing.T) {
	dir := "../../vectors/preparation/"
	raw, err := os.ReadFile(dir + "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Vectors []struct {
			Name      string `json:"name"`
			VerifyRun struct {
				Arguments []string `json:"arguments"`
				Exit      int      `json:"exit"`
				Status    *string  `json:"status"`
				Report    *string  `json:"report"`
				Error     *string  `json:"error"`
			} `json:"verifyRun"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &m); err != nil || len(m.Vectors) == 0 {
		t.Fatal("no vectors:", err)
	}
	for _, v := range m.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			want := v.VerifyRun
			// File arguments name the vector's files, relative to its directory.
			args := append([]string(nil), want.Arguments...)
			for i := 1; i < len(args); i++ {
				switch args[i-1] {
				case "--file", "--profiles", "--expect", "--chain":
					args[i] = filepath.Join(dir, v.Name, args[i])
				}
			}
			var stdout, stderr bytes.Buffer
			err := verifyRun(args, &stdout, &stderr)
			report := ""
			if want.Report != nil {
				report = *want.Report + "\n"
			}
			if stdout.String() != report {
				t.Fatalf("verify-run reports\n%s\nnot\n%s", stdout.String(), report)
			}
			if want.Status != nil {
				var status struct {
					Status string `json:"status"`
				}
				if json.Unmarshal(stdout.Bytes(), &status) != nil || status.Status != *want.Status {
					t.Fatalf("the report's status is not %s", *want.Status)
				}
			}
			if want.Error == nil {
				if err != nil || want.Exit != 0 || stderr.Len() == 0 {
					t.Fatal("verify-run refuses a vector the manifest says it verifies:", err, want.Exit, stderr.String())
				}
				return
			}
			// Only the finding of a disposition that differs from the audit record
			// says anything on standard error: what was checked beside it.
			if err == nil || err.Error() != *want.Error || want.Exit != 1 || (stderr.Len() != 0) != (want.Status != nil && *want.Status == "audit-disposition-mismatch" && want.Report != nil && strings.Contains(*want.Report, `"targetsByClass"`)) {
				t.Fatalf("verify-run refuses with %v and says %q, not %q", err, stderr.String(), *want.Error)
			}
		})
	}
}
