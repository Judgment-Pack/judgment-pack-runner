package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The boot line's requireTestedReleases reaches the service as written, true
// or false, and is off when absent.
func TestBootRequireTestedReleasesReachesTheService(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	legacy, token, err := bootConfig(capabilityBoot(t, state, ""))
	if err != nil || legacy.RequireTestedReleases {
		t.Fatalf("a boot without the member: %+v %v", legacy, err)
	}
	for member, want := range map[string]bool{
		`,"requireTestedReleases":true`:  true,
		`,"requireTestedReleases":false`: false,
	} {
		cfg, bearer, err := bootConfig(capabilityBoot(t, state, member))
		expected := legacy
		expected.RequireTestedReleases = want
		if err != nil || bearer != token || !reflect.DeepEqual(cfg, expected) {
			t.Fatalf("%s: %+v %v", member, cfg, err)
		}
	}
}

// A boot line whose requireTestedReleases is not true or false, or is spelled
// a second way that encoding/json would read as the same member, is refused
// before Runner opens anything: a null, read as false, or a case variant would
// start the Runner with the gate off.
func TestBootRequireTestedReleasesRefusesAmbiguousSpellings(t *testing.T) {
	for name, member := range map[string]string{
		"null":                      `,"requireTestedReleases":null`,
		"a string":                  `,"requireTestedReleases":"true"`,
		"a number":                  `,"requireTestedReleases":1`,
		"a case variant alone":      `,"RequireTestedReleases":true`,
		"a case variant downgrade":  `,"requireTestedReleases":true,"REQUIRETESTEDRELEASES":false`,
		"an exact duplicate":        `,"requireTestedReleases":true,"requireTestedReleases":false`,
		"an escaped duplicate":      `,"requireTestedReleases":true,"requireTestedReleases":false`,
		"a case variant of a false": `,"requireTestedReleases":false,"requiretestedreleases":true`,
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
