package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The boot line's requireComparableFacts reaches the service as written,
// true or false, and is off when absent (runner #210).
func TestBootRequireComparableFactsReachesTheService(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	legacy, token, err := bootConfig(capabilityBoot(t, state, ""))
	if err != nil || legacy.RequireComparableFacts {
		t.Fatalf("a boot without the member: %+v %v", legacy, err)
	}
	for member, want := range map[string]bool{
		`,"requireComparableFacts":true`:  true,
		`,"requireComparableFacts":false`: false,
	} {
		cfg, bearer, err := bootConfig(capabilityBoot(t, state, member))
		expected := legacy
		expected.RequireComparableFacts = want
		if err != nil || bearer != token || !reflect.DeepEqual(cfg, expected) {
			t.Fatalf("%s: %+v %v", member, cfg, err)
		}
	}
}

// A boot line whose requireComparableFacts is not true or false, or is spelled
// a second way that encoding/json would read as the same member, is refused
// before Runner opens anything: a requirement never disappears, or is replaced
// by a weaker one, on the way in.
func TestBootRequireComparableFactsRefusesAmbiguousSpellings(t *testing.T) {
	for name, member := range map[string]string{
		"null":                      `,"requireComparableFacts":null`,
		"a string":                  `,"requireComparableFacts":"true"`,
		"a number":                  `,"requireComparableFacts":1`,
		"a case variant alone":      `,"RequireComparableFacts":true`,
		"a case variant downgrade":  `,"requireComparableFacts":true,"REQUIRECOMPARABLEFACTS":false`,
		"an exact duplicate":        `,"requireComparableFacts":true,"requireComparableFacts":false`,
		"an escaped duplicate":      `,"requireComparableFacts":true,"requireComparable\u0046acts":false`,
		"a case variant of a false": `,"requireComparableFacts":false,"requirecomparablefacts":true`,
	} {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "must-not-be-created")
			var stdout, stderr bytes.Buffer
			status := runnerCommand(nil, bytes.NewReader(capabilityBoot(t, state, member)), &stdout, &stderr)
			if status != 1 || stdout.Len() != 0 || stderr.String() != "runner: invalid boot configuration\n" {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("a refused boot initialized Runner state: %v", err)
			}
		})
	}
}
