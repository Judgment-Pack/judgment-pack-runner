package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
)

// Source is descriptive Desk metadata, never evidence of a successful check.
type TestSource struct {
	PackKey          string            `json:"packKey"`
	SuiteRevision    int               `json:"suiteRevision"`
	CaseNames        map[string]string `json:"caseNames,omitempty"`
	ExploratoryCount int               `json:"exploratoryCount,omitempty"`
}

// A check and its inputs are retained with one immutable release. Runtime owns
// matrix admission, comparison and coverage; Runner checks the response binding.
type ReleaseTests struct {
	Status        string          `json:"status"`
	Matrix        string          `json:"matrix"`
	MatrixDigest  string          `json:"matrixDigest"`
	PackDigest    string          `json:"packDigest"`
	RuntimeDigest string          `json:"runtimeDigest"`
	CheckedAt     string          `json:"checkedAt"`
	Source        *TestSource     `json:"source,omitempty"`
	Report        json.RawMessage `json:"report,omitempty"`
	Problem       string          `json:"problem,omitempty"`
}
type testCounts struct {
	Total      int `json:"total"`
	Passed     int `json:"passed"`
	Mismatched int `json:"mismatched"`
}
type testReport struct {
	OutputVersion string     `json:"outputVersion"`
	Command       string     `json:"command"`
	Status        string     `json:"status"`
	Experimental  bool       `json:"experimental"`
	Spec          string     `json:"evaluatorSpecVersion"`
	Summary       testCounts `json:"summary"`
	Packs         []struct {
		ID          string     `json:"id"`
		PackID      string     `json:"packId"`
		PackVersion string     `json:"packVersion"`
		Status      string     `json:"status"`
		Summary     testCounts `json:"summary"`
		Rows        []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"rows"`
	} `json:"packs"`
}

func checkedReport(r Release, matrix string, out []byte) (string, error) {
	invalid := errors.New("Runtime did not return a complete test report for this release.")
	var input struct {
		Cases []struct {
			ID string `json:"id"`
		} `json:"cases"`
	}
	if json.Unmarshal([]byte(matrix), &input) != nil || len(input.Cases) == 0 {
		return "error", invalid
	}
	ids := make(map[string]bool, len(input.Cases))
	for _, c := range input.Cases {
		if c.ID == "" || ids[c.ID] {
			return "error", invalid
		}
		ids[c.ID] = true
	}
	var report testReport
	if json.Unmarshal(out, &report) != nil || report.OutputVersion != "2" || report.Command != "packs test" || !report.Experimental || report.Spec != "0.2.0-draft" || len(report.Packs) != 1 {
		return "error", invalid
	}
	p := report.Packs[0]
	if p.ID != "target" || p.PackID != r.PackID || p.PackVersion != r.PackVersion || len(p.Rows) != len(ids) {
		return "error", invalid
	}
	counts := testCounts{Total: len(ids)}
	for _, row := range p.Rows {
		if !ids[row.ID] {
			return "error", invalid
		}
		delete(ids, row.ID)
		switch row.Status {
		case "passed":
			counts.Passed++
		case "mismatch":
			counts.Mismatched++
		default:
			return "error", invalid
		}
	}
	status, readiness := "passed", "passed"
	if counts.Mismatched > 0 {
		status, readiness = "mismatch", "failed"
	}
	if p.Summary != counts || report.Summary != counts || p.Status != status || report.Status != status {
		return "error", invalid
	}
	return readiness, nil
}
func (s *Service) checkReleaseTests(ctx context.Context, r Release, matrix string, source *TestSource, dir string) *ReleaseTests {
	e := &ReleaseTests{Status: "error", Matrix: matrix, MatrixDigest: digest([]byte(matrix)), PackDigest: r.PackDigest, RuntimeDigest: r.RuntimeDigest, Source: source}
	defer func() { e.CheckedAt = now() }()
	fail := func() *ReleaseTests {
		e.Problem = "The saved tests could not complete. Check the cases in Packs, then check this release again."
		return e
	}
	// Never trust an arbitrary executable or pack in an old attempt directory.
	bin, hash, err := s.pinRuntime(s.runtime)
	if err != nil || hash != r.RuntimeDigest || digest([]byte(r.Pack)) != r.PackDigest {
		return fail()
	}
	if err = privateDir(dir); err != nil {
		return fail()
	}
	config := `{"configVersion":"3","packs":{"target":{"path":"pack.json","matrix":"matrix.json"}}}`
	for name, b := range map[string][]byte{"pack.json": []byte(r.Pack), "matrix.json": []byte(matrix), "jpack.json": []byte(config)} {
		if err = saveFile(filepath.Join(dir, name), b, 0600); err != nil {
			return fail()
		}
	}
	out, runErr := invoke(ctx, bin, dir, "test", "packs", "test", "--id", "target", "--config", "jpack.json", "--format", "json")
	if json.Valid(out) {
		e.Report = out
	}
	status, err := checkedReport(r, matrix, out)
	if err != nil || ctx.Err() != nil {
		return fail()
	}
	// A mismatch deliberately exits 1. Every other failed invocation, including
	// a killed process that happened to emit JSON, remains an incomplete check.
	var exit *exec.ExitError
	if status == "passed" && runErr != nil || status == "failed" && (!errors.As(runErr, &exit) || exit.ExitCode() != 1) {
		return fail()
	}
	e.Status = status
	return e
}
func releaseReady(r Release) bool {
	if r.Tests == "not-run" {
		return r.TestEvidence == nil && digest([]byte(r.Pack)) == r.PackDigest
	}
	e := r.TestEvidence
	if r.Tests != "passed" || e == nil || e.Status != "passed" || e.Problem != "" || e.CheckedAt == "" || e.MatrixDigest != digest([]byte(e.Matrix)) || e.PackDigest != r.PackDigest || e.RuntimeDigest != r.RuntimeDigest || digest([]byte(r.Pack)) != r.PackDigest {
		return false
	}
	status, err := checkedReport(r, e.Matrix, e.Report)
	return err == nil && status == "passed"
}
