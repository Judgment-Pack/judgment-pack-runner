package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

func inputPath(p string) bool {
	return p != "" && fs.ValidPath(p) && p != "." && !strings.Contains(p, "\\") && strings.HasSuffix(strings.ToLower(p), ".json")
}
func (s *Service) readAutomaticFile(name string) ([]byte, error) {
	if s.cfg.InputRoot == "" || !inputPath(name) {
		return nil, bad("invalid_input_path", "Choose a JSON file inside this job's project.")
	}
	root, e := os.OpenRoot(s.cfg.InputRoot)
	if e != nil {
		return nil, bad("input_unavailable", "The project input directory is unavailable.")
	}
	defer root.Close()
	f, e := openAutomaticFile(root, name)
	if e != nil {
		return nil, bad("input_unavailable", "The configured input file is unavailable.")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > maxInputDocument {
		return nil, bad("invalid_input_file", "Use one regular JSON file up to 200 KB.")
	}
	b, e := io.ReadAll(io.LimitReader(f, maxInputDocument+1))
	if e != nil || len(b) > maxInputDocument {
		return nil, bad("invalid_input_file", "The input file could not be read within its size limit.")
	}
	return b, nil
}
func localAutomaticSnapshot(name string, raw []byte) json.RawMessage {
	return encode(map[string]any{"version": 1, "selectedAt": now(), "original": map[string]string{"name": path.Base(name), "mediaType": "application/json", "bytes": base64.StdEncoding.EncodeToString(raw), "sha256": digest(raw)}})
}
func validateAutomatic(c *AutomaticInput, r Release, hasRoot bool) error {
	if c == nil {
		return bad("automatic_input_required", "Configure the inputs to read on each occurrence; release samples are never reused.")
	}
	if c.Kind == "constant" {
		if c.Value == "" || c.Path != "" || len(c.Files) > 0 || len(c.Case) > 0 {
			return bad("invalid_automatic_input", "Supply explicit constant inputs only.")
		}
		if r.InputMapping != nil && (r.InputMapping.Version != 2 || len(r.InputMapping.Sources) > 0) {
			return bad("unattended_source_unavailable", "This mapping needs fresh source acquisition. Interactive grants and retained responses cannot power a schedule.")
		}
		var value Input
		if strictJSON([]byte(c.Value), &value) != nil || value.Preparation != nil {
			return bad("invalid_automatic_input", "Supply a valid input envelope without computed preparation.")
		}
		if value.Source != nil && (len(value.Source.Sources) > 0 || len(value.Source.Snapshot) > 0) {
			return bad("invalid_automatic_input", "Scheduled constants cannot reuse source responses.")
		}
		if !mappingMatchesRelease(r, value) {
			return bad("mapping_mismatch", "Use the job's reviewed input mapping.")
		}
		return nil
	}
	if !hasRoot && (c.Path != "" || len(c.Files) > 0) {
		return bad("input_root_unavailable", "Local file automation requires an installation-authorized project input root.")
	}
	if c.Value != "" {
		return bad("invalid_automatic_input", "File inputs cannot silently fall back to constants.")
	}
	if c.Kind == "input-file" {
		if r.InputMapping != nil || !inputPath(c.Path) || len(c.Files) > 0 || len(c.Case) > 0 {
			return bad("invalid_automatic_input", "Select a JSON input envelope for this manual-input release.")
		}
		return nil
	}
	if (c.Kind != "mapped-files" && c.Kind != "mapped-sources") || r.InputMapping == nil {
		return bad("invalid_automatic_input", "Choose supported local input files.")
	}
	if c.Path != "" && (!inputPath(c.Path) || len(c.Case) > 0) {
		return bad("invalid_automatic_input", "Choose either a case file or constant case parameters.")
	}
	if len(c.Case) > 0 {
		if _, e := proofCanon(c.Case); e != nil {
			return bad("invalid_automatic_input", "Case parameters must use exact supported JSON values.")
		}
		var v map[string]any
		if strictJSON(c.Case, &v) != nil || v == nil {
			return bad("invalid_automatic_input", "Case parameters must be one JSON object.")
		}
	}
	expected := map[string]bool{}
	if r.InputMapping.Version == 1 {
		if r.InputMapping.Provider != "local-file" || c.Path != "" || len(c.Case) > 0 {
			return bad("unattended_source_unavailable", "Interactive source grants are unavailable for unattended execution.")
		}
		expected["file"] = true
	} else {
		for _, source := range r.InputMapping.Sources {
			if c.Kind == "mapped-sources" && source.Kind == "operation" {
				continue
			}
			if source.Kind != "selected-file" || source.Provider != "local-file" {
				return bad("unattended_source_unavailable", "Connected scheduled acquisition requires a standing Gateway grant and is not enabled yet.")
			}
			expected[source.Name] = true
		}
	}
	if len(expected) != len(c.Files) {
		return bad("invalid_automatic_input", "Configure a fresh file for every local mapping source.")
	}
	for name, p := range c.Files {
		if !expected[name] || !inputPath(p) {
			return bad("invalid_input_path", "Choose project-relative JSON files for the reviewed mapping sources.")
		}
	}
	return nil
}
func (s *Service) automaticInput(c *AutomaticInput, r Release) (Input, error) {
	return s.automaticInputSnapshot(c, r, nil)
}
func (s *Service) automaticInputSnapshot(c *AutomaticInput, r Release, snapshots map[string][]byte) (Input, error) {
	return s.automaticInputContext(context.Background(), c, r, snapshots)
}
func (s *Service) automaticInputContext(ctx context.Context, c *AutomaticInput, r Release, snapshots map[string][]byte) (Input, error) {
	read := func(name string) ([]byte, error) {
		if raw, ok := snapshots[name]; ok {
			return raw, nil
		}
		return s.readAutomaticFile(name)
	}
	if e := validateAutomatic(c, r, s.cfg.InputRoot != ""); e != nil {
		return Input{}, e
	}
	var input Input
	switch c.Kind {
	case "constant":
		if e := strictJSON([]byte(c.Value), &input); e != nil {
			return input, bad("invalid_automatic_input", "Supply a valid input envelope.")
		}
	case "input-file":
		raw, e := read(c.Path)
		if e != nil {
			return input, e
		}
		if strictJSON(raw, &input) != nil || input.Source != nil || input.Preparation != nil {
			return input, bad("invalid_automatic_input", "The file must contain facts and optional evidence availability.")
		}
	case "mapped-files", "mapped-sources":
		input.Source = &SourceInput{Mapping: *r.InputMapping}
		if r.InputMapping.Version == 2 {
			input.Source.Case = append(json.RawMessage(nil), c.Case...)
			input.Source.Sources = map[string]SourceValue{}
		}
		if c.Path != "" {
			raw, e := read(c.Path)
			if e != nil {
				return input, e
			}
			input.Source.Case = raw
		}
		for name, p := range c.Files {
			raw, e := read(p)
			if e != nil {
				return input, e
			}
			snapshot := localAutomaticSnapshot(p, raw)
			if r.InputMapping.Version == 1 {
				input.Source.Snapshot = snapshot
			} else {
				input.Source.Sources[name] = SourceValue{Snapshot: snapshot}
			}
		}
	}
	if !mappingMatchesRelease(r, input) {
		return input, bad("mapping_mismatch", "Automatic inputs do not match this job's release.")
	}
	if c.Kind == "mapped-sources" {
		var e error
		input, e = s.acquireAutomatic(ctx, input)
		if e != nil {
			return input, e
		}
	}
	// Validate now, but retain the original source request. Admission recomputes
	// verification; caller-provided preparation is never trusted.
	if _, e := s.normalizeInput(input); e != nil {
		return input, e
	}
	if len(encode(input)) > MaxBody {
		return input, bad("invalid_automatic_input", "Combined inputs exceed the request limit.")
	}
	return input, nil
}
func (s *Service) validateTrigger(c *TriggerConfig, r Release, at time.Time) error {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 160 {
		return bad("invalid_trigger", "Enter a trigger name up to 160 bytes.")
	}
	if c.Overlap != "skip" && c.Overlap != "queue" || c.Missed != "skip" && c.Missed != "latest" || c.QueueSeconds < 60 || c.QueueSeconds > 86400 {
		return bad("invalid_trigger", "Choose supported overlap, missed-run and queue expiry settings.")
	}
	if c.Kind != "cloud" && c.Cloud != nil {
		return bad("invalid_trigger", "Cloud bindings require a cloud trigger.")
	}
	if c.Input != nil && c.Input.Kind == "mapped-sources" {
		if e := s.validateGatewayMapping(r); e != nil {
			return e
		}
	}
	switch c.Kind {
	case "cloud":
		if c.Schedule != nil || c.WatchPath != "" || c.StableSeconds != 0 {
			return bad("invalid_trigger", "Cloud timing is managed by the scheduler.")
		}
		if e := s.validateCloud(c.Cloud); e != nil {
			return e
		}
		return validateAutomatic(c.Input, r, s.cfg.InputRoot != "")
	case "event":
		if c.Schedule != nil || c.Input != nil || c.WatchPath != "" || c.StableSeconds != 0 {
			return bad("invalid_trigger", "Event triggers take fresh inputs from each authenticated delivery.")
		}
	case "schedule":
		if c.Schedule == nil || c.WatchPath != "" || c.StableSeconds != 0 {
			return bad("invalid_trigger", "Configure a schedule.")
		}
		if c.Schedule.StartAt == "" {
			c.Schedule.StartAt = at.UTC().Truncate(time.Second).Format(time.RFC3339)
		}
		if e := c.Schedule.validate(); e != nil {
			return e
		}
		return validateAutomatic(c.Input, r, s.cfg.InputRoot != "")
	case "file":
		if c.Schedule != nil || !inputPath(c.WatchPath) || c.StableSeconds < 1 || c.StableSeconds > 3600 {
			return bad("invalid_trigger", "Choose a JSON file and a stability delay between 1 and 3600 seconds.")
		}
		if e := validateAutomatic(c.Input, r, s.cfg.InputRoot != ""); e != nil {
			return e
		}
		if c.Input.Kind == "mapped-sources" {
			return bad("unattended_source_unavailable", "Connected acquisition is supported for schedule and cloud triggers; use a local mapping for file changes.")
		}
		if c.Input.Kind == "constant" {
			return bad("invalid_trigger", "File triggers must read the changed file.")
		}
		referenced := c.Input.Path == c.WatchPath
		for _, p := range c.Input.Files {
			referenced = referenced || p == c.WatchPath
		}
		if !referenced {
			return bad("invalid_trigger", "The watched file must be used by this trigger's inputs.")
		}
	default:
		return bad("invalid_trigger", "Unsupported trigger type.")
	}
	return nil
}
