package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The boot line's requireSuppliedReviewedSet reaches the service as written,
// true or false, and is off when absent (runner #210).
func TestBootRequireSuppliedReviewedSetReachesTheService(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	legacy, token, err := bootConfig(capabilityBoot(t, state, ""))
	if err != nil || legacy.RequireSuppliedReviewedSet {
		t.Fatalf("a boot without the member: %+v %v", legacy, err)
	}
	for member, want := range map[string]bool{
		`,"requireSuppliedReviewedSet":true`:  true,
		`,"requireSuppliedReviewedSet":false`: false,
	} {
		cfg, bearer, err := bootConfig(capabilityBoot(t, state, member))
		expected := legacy
		expected.RequireSuppliedReviewedSet = want
		if err != nil || bearer != token || !reflect.DeepEqual(cfg, expected) {
			t.Fatalf("%s: %+v %v", member, cfg, err)
		}
	}
}

// A boot line whose requireSuppliedReviewedSet is not true or false, or is
// spelled a second way that encoding/json would read as the same member, is
// refused before Runner opens anything.
func TestBootRequireSuppliedReviewedSetRefusesAmbiguousSpellings(t *testing.T) {
	for name, member := range map[string]string{
		"null":                      `,"requireSuppliedReviewedSet":null`,
		"a string":                  `,"requireSuppliedReviewedSet":"true"`,
		"a number":                  `,"requireSuppliedReviewedSet":1`,
		"a case variant alone":      `,"RequireSuppliedReviewedSet":true`,
		"a case variant downgrade":  `,"requireSuppliedReviewedSet":true,"REQUIRESUPPLIEDREVIEWEDSET":false`,
		"an exact duplicate":        `,"requireSuppliedReviewedSet":true,"requireSuppliedReviewedSet":false`,
		"an escaped duplicate":      `,"requireSuppliedReviewedSet":true,"requireSupplied\u0052eviewedSet":false`,
		"a case variant of a false": `,"requireSuppliedReviewedSet":false,"requiresuppliedreviewedset":true`,
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
