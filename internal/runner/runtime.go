package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != filepath.Clean(path) {
		return errors.New("runner state must not use symlink directories")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("runner state directory must be private (0700)")
	}
	return checkOwner(st)
}
func privateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("runner state file must be private and regular")
	}
	return checkOwner(st)
}
func saveFile(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	return err
}
func readLimit(path string, limit int64) ([]byte, error) {
	if err := privateFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("artifact size limit exceeded")
	}
	return b, err
}
func (s *Service) pinRuntime(path string) (string, string, error) {
	// Runtime comes from the trusted host configuration, never an HTTP request.
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 128<<20+1))
	if err != nil {
		return "", "", err
	}
	if len(b) > 128<<20 {
		return "", "", errors.New("runtime binary exceeds limit")
	}
	hash := digest(b)
	target := filepath.Join(s.cfg.Dir, "runtimes", strings.TrimPrefix(hash, "sha256:"))
	if _, err = os.Lstat(target); os.IsNotExist(err) {
		err = saveFile(target, b, 0700)
	}
	if err != nil {
		return "", "", err
	}
	stored, err := readLimit(target, 128<<20)
	if err != nil {
		return "", "", err
	}
	if digest(stored) != hash {
		return "", "", errors.New("pinned runtime changed")
	}
	return target, hash, nil
}

type invocationError struct{ cause error }

func (e *invocationError) Error() string {
	return "Runtime refused or could not complete this invocation. Inspect the retained attempt diagnostics."
}
func (e *invocationError) Unwrap() error { return e.cause }

type boundedWriter struct {
	w io.Writer
	n int
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > b.n {
		return 0, errors.New("runtime output limit exceeded")
	}
	n, e := b.w.Write(p)
	b.n -= n
	return n, e
}
func invoke(ctx context.Context, bin, dir, label string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := os.OpenFile(filepath.Join(dir, label+".stdout"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	stderr, err := os.OpenFile(filepath.Join(dir, label+".stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// No host provider credentials, config override, or inherited stdin.
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Stdout = &boundedWriter{out, maxOutput}
	cmd.Stderr = &boundedWriter{stderr, 1 << 20}
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	if err = out.Sync(); err != nil {
		return nil, err
	}
	if err = stderr.Sync(); err != nil {
		return nil, err
	}
	b, err := readLimit(filepath.Join(dir, label+".stdout"), maxOutput)
	if err != nil {
		return nil, err
	}
	if runErr != nil {
		return b, &invocationError{cause: runErr}
	}
	return b, nil
}
func (s *Service) preview(ctx context.Context, req PreviewRequest) (Release, error) {
	if len(req.Pack) == 0 || len(req.Pack) > 1<<20 {
		return Release{}, bad("invalid_pack", "Choose a pack smaller than 1 MiB.")
	}
	if len(req.Matrix) > 1<<20 {
		return Release{}, bad("invalid_matrix", "Choose a test matrix smaller than 1 MiB.")
	}
	normalized, err := normalizeInput(req.Input)
	if err != nil {
		return Release{}, err
	}
	req.Input = normalized
	select {
	case s.previewGate <- struct{}{}:
		defer func() { <-s.previewGate }()
	default:
		return Release{}, &apiError{429, "preview_busy", "A release preview is already running."}
	}
	var doc struct {
		ID          string `json:"id"`
		Version     string `json:"version"`
		Title       string `json:"title"`
		SpecVersion string `json:"specVersion"`
	}
	if json.Unmarshal([]byte(req.Pack), &doc) != nil || doc.SpecVersion != "0.2.0-draft" {
		return Release{}, bad("invalid_pack", "This runner supports the Runtime's experimental 0.2.0-draft evaluator contract.")
	}
	r := Release{SchemaVersion: "1", ID: id("release_"), Pack: req.Pack, PackDigest: digest([]byte(req.Pack)), PackID: doc.ID, PackVersion: doc.Version, Title: doc.Title, RuntimeDigest: s.runtimeDigest, CreatedAt: now(), Sample: req.Input, Tests: "not-run"}
	if req.Input.Source != nil {
		mapping := req.Input.Source.Mapping
		r.InputMapping = &mapping
	}
	r.Config = `{"configVersion":"3","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"}}`
	dir := filepath.Join(s.cfg.Dir, "releases", r.ID)
	if err := privateDir(dir); err != nil {
		return r, err
	}
	for name, b := range map[string][]byte{"pack.json": []byte(r.Pack), "jpack.json": []byte(r.Config)} {
		if err := saveFile(filepath.Join(dir, name), b, 0600); err != nil {
			return r, err
		}
	}
	validation, err := invoke(ctx, s.runtime, dir, "validate", "spec", "validate", "pack.json", "--format", "json")
	if err != nil {
		return r, bad("validation_failed", "The pack did not pass Runtime validation. Correct it in Packs before creating a job.")
	}
	r.Validation = validation
	if _, err = invoke(ctx, s.runtime, dir, "lock", "packs", "lock", "--config", "jpack.json", "--format", "json"); err != nil {
		return r, err
	}
	// Lock is generated by Runtime itself. Its permissions are host defaults;
	// containing directory is private and tighten the file before retention.
	if err = os.Chmod(filepath.Join(dir, "jpack.lock.json"), 0600); err != nil {
		return r, err
	}
	lock, err := readLimit(filepath.Join(dir, "jpack.lock.json"), MaxBody)
	if err != nil {
		return r, err
	}
	r.Lock = string(lock)
	result, _, err := s.evaluate(ctx, r, req.Input, filepath.Join(dir, "preview"), true)
	if err != nil {
		return r, bad("preview_failed", "The sample input could not be evaluated. Check its facts and evidence values.")
	}
	r.Preview = result
	if req.Matrix != "" {
		r.TestEvidence = s.checkReleaseTests(ctx, r, req.Matrix, req.TestSource, filepath.Join(dir, "tests"))
		r.Tests = r.TestEvidence.Status
	}
	_, err = s.db.Exec("INSERT INTO releases(id,record) VALUES (?,?)", r.ID, string(encode(r)))
	return r, err
}
func (s *Service) evaluate(ctx context.Context, r Release, input Input, dir string, rehearsal bool) (json.RawMessage, json.RawMessage, error) {
	if digest([]byte(r.Pack)) != r.PackDigest {
		return nil, nil, errors.New("release pack digest mismatch")
	}
	bin := filepath.Join(s.cfg.Dir, "runtimes", strings.TrimPrefix(r.RuntimeDigest, "sha256:"))
	b, err := readLimit(bin, 128<<20)
	if err != nil || digest(b) != r.RuntimeDigest {
		return nil, nil, errors.New("pinned Runtime is missing or changed")
	}
	// The directory is exclusively created per invocation and never reused.
	if err = os.Mkdir(dir, 0700); err != nil {
		return nil, nil, err
	}
	for name, b := range map[string][]byte{"pack.json": []byte(r.Pack), "jpack.json": []byte(r.Config), "jpack.lock.json": []byte(r.Lock), "facts.json": input.Facts} {
		if err = saveFile(filepath.Join(dir, name), b, 0600); err != nil {
			return nil, nil, err
		}
	}
	args := []string{"experimental", "evaluate", "--pack-id", "target", "--config", "jpack.json", "--facts", "facts.json", "--format", "json"}
	if len(input.Evidence) > 0 {
		if err = saveFile(filepath.Join(dir, "evidence.json"), input.Evidence, 0600); err != nil {
			return nil, nil, err
		}
		args = append(args, "--evidence", "evidence.json")
	}
	if rehearsal {
		args = append(args, "--rehearsal")
	}
	out, err := invoke(ctx, bin, dir, "evaluation", args...)
	if err != nil {
		return nil, nil, err
	}
	var result struct {
		OutputVersion string          `json:"outputVersion"`
		Status        string          `json:"status"`
		PackID        string          `json:"packId"`
		PackVersion   string          `json:"packVersion"`
		Spec          string          `json:"evaluatorSpecVersion"`
		Rehearsal     bool            `json:"rehearsal"`
		Disposition   json.RawMessage `json:"disposition"`
	}
	if json.Unmarshal(out, &result) != nil || result.OutputVersion != "2" || result.Status != "evaluated" || result.PackID != r.PackID || result.PackVersion != r.PackVersion || result.Spec != "0.2.0-draft" || result.Rehearsal != rehearsal || len(result.Disposition) == 0 {
		return nil, nil, errors.New("unsupported or mismatched Runtime response")
	}
	if rehearsal {
		return out, nil, nil
	}
	audit, err := readLimit(filepath.Join(dir, "audit", "evaluations.jsonl"), maxOutput)
	if err != nil {
		return nil, nil, errors.New("operational audit record could not be read")
	}
	var record struct {
		Version string `json:"recordVersion"`
		Kind    string `json:"kind"`
		Run     string `json:"run"`
		Pack    struct {
			Digest string `json:"digest"`
		} `json:"pack"`
		Inputs struct {
			Facts    json.RawMessage `json:"facts"`
			Evidence json.RawMessage `json:"evidence"`
			Supplied bool            `json:"evidenceSupplied"`
		} `json:"inputs"`
		Disposition json.RawMessage `json:"disposition"`
		Reviewed    bool            `json:"reviewed"`
	}
	same := func(a, b []byte) bool {
		ac, e := canonical(a)
		if e != nil {
			return false
		}
		bc, e := canonical(b)
		return e == nil && bytes.Equal(ac, bc)
	}
	if json.Unmarshal(audit, &record) != nil || record.Version != "1" || record.Kind != "evaluation" || record.Run == "" || !record.Reviewed || record.Pack.Digest != r.PackDigest || !same(record.Inputs.Facts, input.Facts) || record.Inputs.Supplied != (len(input.Evidence) > 0) || !same(record.Disposition, result.Disposition) || (len(input.Evidence) > 0 && !same(record.Inputs.Evidence, input.Evidence)) {
		return nil, nil, errors.New("operational audit does not match this invocation")
	}
	return out, bytes.TrimSpace(audit), nil
}
