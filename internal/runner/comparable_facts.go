package runner

import (
	"context"
	"encoding/json"
	"slices"
)

// Comparable facts (runner #210, runtime ADR-0046).
//
// A release is a frozen pack: Runner generates the configuration its Runtime
// reads, and no project's jpack.json reaches a run. A project's
// requireComparableFacts switch therefore reaches a run only when the release
// is checked with it: a release checked with requireComparableFacts has a
// configuration that sets the switch, and its Runtime then refuses an
// evaluation, a rehearsal included, in which a fact some comparison of the
// pack reads is present and of a JSON type that comparison can never match,
// such as a JSON number in an ordered comparison
// (JPS-FACTS-COMPARABLE-REQUIRED). Without the switch such a fact is evaluated
// as Core says, and the comparison is unknown.
//
// The switch needs configVersion "5", which Runtime 0.25.0 and later read. A
// release checked without it keeps the configuration every earlier release
// has, configVersion "3", byte for byte, so that a Runtime before 0.25.0 still
// runs it. A release checked with it asks the pinned Runtime first, through
// its packs schema answer, whether it reads configVersion "5", and is refused
// when the answer does not say so, whether the Runtime says it does not or its
// answer cannot be read.
//
// An installation booted with requireComparableFacts creates and upgrades jobs
// only from releases whose configuration sets the switch. As with
// requireTestedReleases, a job made before keeps its release and keeps
// running, and repeating its creation returns it.

// comparableFactsConfigVersion is the configVersion that admits
// requireComparableFacts.
const comparableFactsConfigVersion = "5"

// The two configurations Runner generates for a release, exactly. Every
// release checked before #210 has the first.
const (
	releaseConfigDefault    = `{"configVersion":"3","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"}}`
	releaseConfigComparable = `{"configVersion":"` + comparableFactsConfigVersion + `","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"},"requireComparableFacts":true}`
)

// releaseConfig is the configuration Runner generates for a release, with or
// without the Runtime's requireComparableFacts.
func releaseConfig(comparableFacts bool) string {
	if comparableFacts {
		return releaseConfigComparable
	}
	return releaseConfigDefault
}

// releaseRefusesIncomparableFacts says whether a release's Runtime is given
// the requireComparableFacts switch: its configuration is, byte for byte, the
// one Runner generates with it. Any other configuration is not taken to set
// it.
func releaseRefusesIncomparableFacts(r Release) bool {
	return r.Config == releaseConfigComparable
}

// errComparableFactsUnsupported refuses a release check that asks for
// requireComparableFacts on a pinned Runtime that does not say it reads the
// configVersion the switch needs.
var errComparableFactsUnsupported = bad("comparable_facts_unsupported", `The pinned Runtime does not say that it reads configVersion "5", which requireComparableFacts needs; Runtime 0.25.0 and later read it. Pin such a Runtime, or check the release without requireComparableFacts.`)

// The refusals of an installation booted with requireComparableFacts, of a
// job from a release whose configuration does not set the switch.
var (
	errJobComparableFactsRequired     = &apiError{409, "release_comparable_facts_required", "This installation creates jobs only from releases checked with requireComparableFacts. Check a new release with requireComparableFacts."}
	errUpgradeComparableFactsRequired = &apiError{409, "release_comparable_facts_required", "This installation upgrades jobs only to releases checked with requireComparableFacts. Check a new release with requireComparableFacts."}
)

// runtimeReadsComparableFacts asks the pinned Runtime, invoked in dir, whether
// it reads the configVersion requireComparableFacts needs: its packs schema
// answer must list it among supportedConfigVersions. An answer that cannot be
// read, or that does not list it, refuses; neither is taken for a yes.
func (s *Service) runtimeReadsComparableFacts(ctx context.Context, dir string) error {
	out, err := s.invoke(ctx, s.runtime, dir, "config-schema", "packs", "schema", "--format", "json")
	var answer struct {
		OutputVersion string   `json:"outputVersion"`
		Command       string   `json:"command"`
		Status        string   `json:"status"`
		Supported     []string `json:"supportedConfigVersions"`
	}
	if err != nil || json.Unmarshal(out, &answer) != nil || answer.OutputVersion != "2" || answer.Command != "packs schema" || answer.Status != "valid" || !slices.Contains(answer.Supported, comparableFactsConfigVersion) {
		return errComparableFactsUnsupported
	}
	return nil
}
