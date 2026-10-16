package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func namedMapping(m InputMapping, version string) InputMapping {
	m.FormatVersion = "2.0.0"
	if m.Version == 3 {
		m.FormatVersion = "3.0.0"
	}
	m.ArtifactID = "urn:judgmentpack:dialect:example"
	m.ArtifactVersion = version
	return m
}
func TestNamedDialectPreparesAndPreservesExactIdentity(t *testing.T) {
	for _, family := range []int{2, 3} {
		in, profile, _, at := v2Fixture(t)
		in.Source.Mapping.Version = family
		in.Source.Mapping = namedMapping(in.Source.Mapping, "0.1.0")
		raw := encode(in.Source.Mapping)
		var decoded InputMapping
		must(t, strictJSON(raw, &decoded))
		if decoded.FormatVersion != in.Source.Mapping.FormatVersion || !bytes.Equal(canonTest(t, raw), canonTest(t, encode(decoded))) {
			t.Fatal("named mapping identity changed")
		}
		prepared, err := normalizeV2(in, []InputProfile{profile}, at)
		must(t, err)
		if prepared.Preparation.Version != family || prepared.Preparation.MappingDigest != digest(canonTest(t, raw)) {
			t.Fatal("incorrect preparation identity", prepared.Preparation)
		}
		for _, fragment := range []string{`"provider":"",`, `"sources":[],`, `"ID":"duplicate",`} {
			corrupted := strings.Replace(string(raw), "{", "{"+fragment, 1)
			if err = strictJSON([]byte(corrupted), &decoded); err == nil {
				t.Fatal("noncanonical named mapping accepted", fragment)
			}
		}
		for _, version := range []string{`2`, `"1.0.0-a"`, `"01.0.0"`, `null`} {
			corrupted := strings.Replace(string(raw), `"version":"0.1.0"`, `"version":`+version, 1)
			if err = strictJSON([]byte(corrupted), &decoded); err == nil {
				t.Fatal("invalid artifact version", version)
			}
		}
	}
}
func TestNamedDialectBindingRetainsPublishedAndBoundIdentities(t *testing.T) {
	f := newImportFixture(t)
	f.published = namedMapping(f.published, "1.0.0")
	input := f.imported(t)
	normalized, err := importedInput(input, []InputProfile{f.profile})
	must(t, err)
	prepared, err := normalizeV2(normalized, []InputProfile{f.profile}, f.at)
	must(t, err)
	r := Release{Sample: normalized, InputMapping: &normalized.Source.Mapping}
	pin, err := releaseDialectIdentity(r)
	must(t, err)
	if pin.ID != f.published.ArtifactID || pin.Digest != mappingV2Hash(f.published) || pin.Digest == prepared.Preparation.MappingDigest {
		t.Fatal("published and bound identity conflated", pin)
	}
	if normalized.Source.Mapping.ArtifactID != pin.ID {
		t.Fatal("binding lost identity")
	}
}
func TestNamedDialectJobIdentityAndMappingOnlyUpgrade(t *testing.T) {
	cfg := testConfig(t)
	cfg.disableDispatcher = true
	cfg.disableAutomation = true
	s := openJournal(t, cfg)
	defer s.Close()
	first := journalRelease(t, s, s.runtimeDigest)
	m := namedMapping(InputMapping{Version: 2, Case: &CaseMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}}, "1.0.0")
	first.ID = id("release_")
	first.InputMapping = &m
	var err error
	first.Dialect, err = releaseDialectIdentity(first)
	must(t, err)
	must(t, s.saveRelease(first))
	j, err := s.createJob("Named", first.ID)
	must(t, err)
	next := journalRelease(t, s, s.runtimeDigest)
	changed := m
	changed.Unmapped = []string{"/new"}
	next.ID = id("release_")
	next.InputMapping = &changed
	next.Dialect, err = releaseDialectIdentity(next)
	must(t, err)
	must(t, s.saveRelease(next))
	if _, err = s.createJob("conflict", next.ID); err == nil {
		t.Fatal("same identity acquired different content")
	}
	if _, err = s.upgradeJob(context.Background(), j.ID, UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: j.ReleaseID, BaseRevision: 1, Reviewed: true}); err == nil {
		t.Fatal("same version mutation upgraded")
	}
	changed.ArtifactVersion = "1.0.1"
	next.ID = id("release_")
	next.Dialect, err = releaseDialectIdentity(next)
	must(t, err)
	must(t, s.saveRelease(next))
	updated, err := s.upgradeJob(context.Background(), j.ID, UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: j.ReleaseID, BaseRevision: 1, Reviewed: true})
	must(t, err)
	if updated.Revision != 2 {
		t.Fatal(updated)
	}
	old, err := s.release(first.ID)
	must(t, err)
	if old.Dialect.Version != "1.0.0" {
		t.Fatal("old release changed")
	}
	// The API JSON contains the actual named mapping, not a legacy projection.
	var wire map[string]any
	must(t, json.Unmarshal(encode(old), &wire))
	if wire["inputMapping"].(map[string]any)["version"] != "1.0.0" {
		t.Fatal("release lost artifact identity")
	}
}
