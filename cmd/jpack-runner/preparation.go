package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

func verifyPreparation(args []string, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("verify-preparation", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	capabilities := flags.Bool("capabilities", false, "Report this offline verifier's supported inputs")
	dialect := flags.String("dialect", "", "Exact dialect JSON file")
	casePath := flags.String("case", "", "Exact case JSON file")
	acquisitions := flags.String("acquisitions", "", "Retained source envelopes (default {})")
	profiles := flags.String("profiles", "", "Fixture profiles, not installation authority")
	preparation := flags.String("preparation", "", "Expected preparation file, including historical verifiedAt")
	binding := flags.String("binding", "", "Import binding for an unbound dialect")
	expected := flags.String("expected-digest", "", "SHA-256 of the expected file from the verified catalog")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 3
	}
	if *capabilities {
		if len(args) != 1 {
			return 3
		}
		if json.NewEncoder(out).Encode(map[string]any{"version": 1, "formats": []string{"2", "3", "2.0.0", "3.0.0"}, "sourceKinds": []string{"operation", "research", "selected-file/local-file"}, "offline": true, "binding": "not-checked"}) != nil {
			return 3
		}
		return 0
	}
	if *dialect == "" || *casePath == "" || *profiles == "" || *preparation == "" || *expected == "" {
		fmt.Fprintln(diagnostics, "verify-preparation requires dialect, case, profiles, preparation and expected-digest")
		return 3
	}
	f := runner.PreparationFixture{ExpectedDigest: *expected}
	inputs := []struct {
		path  string
		dest  *[]byte
		limit int
	}{{*dialect, &f.Dialect, 128 << 10}, {*casePath, &f.Case, 2 << 20}, {*profiles, &f.Profiles, 64 << 10}, {*preparation, &f.Preparation, 8 << 20}, {*acquisitions, &f.Acquisitions, 2 << 20}, {*binding, &f.Binding, 64 << 10}}
	for _, item := range inputs {
		if item.path == "" {
			continue
		}
		file, err := os.Open(item.path)
		if err != nil {
			fmt.Fprintln(diagnostics, "Could not read a verification input.")
			return 3
		}
		raw, err := io.ReadAll(io.LimitReader(file, int64(item.limit)+1))
		file.Close()
		if err != nil {
			fmt.Fprintln(diagnostics, "Could not read a verification input.")
			return 3
		}
		if len(raw) > item.limit {
			report, _ := (runner.PreparationReport{Result: "refusal", Refusal: "input-over-bound"}).CanonicalBytes()
			if _, err := out.Write(append(report, '\n')); err != nil {
				return 3
			}
			return 1
		}
		*item.dest = raw
	}
	report := runner.VerifyPreparation(f)
	raw, err := report.CanonicalBytes()
	if err != nil {
		return 3
	}
	if _, err = out.Write(append(raw, '\n')); err != nil {
		return 3
	}
	return report.ExitCode()
}
