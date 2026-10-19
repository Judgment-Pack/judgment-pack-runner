package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type unreadableBoot struct{}

func (unreadableBoot) Read([]byte) (int, error) {
	panic("command read boot stdin")
}

func TestCapabilitiesCommandDoesNotReadBootOrInitializeRunner(t *testing.T) {
	// No Runtime or workspace exists; these environment settings cannot supply
	// boot authority or cause the probe to inspect local configuration.
	missing := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("XDG_CONFIG_HOME", missing)
	var stdout, stderr bytes.Buffer
	status := runnerCommand([]string{"capabilities", "--format", "json"}, unreadableBoot{}, &stdout, &stderr)
	want := "{\"version\":2,\"capabilities\":[\"jobs/1\",\"required-boot-capabilities/1\",\"executor-ownership/1\"],\"governanceAdmission\":false}\n"
	if status != 0 || stdout.String() != want || stderr.Len() != 0 || stdout.Len() > 512 {
		t.Fatalf("status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("probe accessed local state: %v", err)
	}
	decoder := json.NewDecoder(&stdout)
	var report map[string]json.RawMessage
	if err := decoder.Decode(&report); err != nil || len(report) != 3 {
		t.Fatalf("not one closed capability object: %v %v", report, err)
	}
	if err := decoder.Decode(&report); err != io.EOF {
		t.Fatalf("additional protocol output: %v", err)
	}
}

type unavailableCapabilityOutput struct{}

func (unavailableCapabilityOutput) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestCapabilitiesOutputFailureDoesNotStartRunner(t *testing.T) {
	var stderr bytes.Buffer
	status := runnerCommand([]string{"capabilities", "--format", "json"}, unreadableBoot{}, unavailableCapabilityOutput{}, &stderr)
	if status != 1 || !strings.Contains(stderr.String(), io.ErrClosedPipe.Error()) {
		t.Fatalf("output failure: status=%d stderr=%q", status, stderr.String())
	}
}

func TestRunnerArgumentErrorsNeverReadBoot(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"--format", "json"}, {"--help"}, {""},
		{"capabilities"}, {"capabilities", "--format"},
		{"capabilities", "--format=json"}, {"capabilities", "--format", "text"},
		{"capabilities", "--format", "json", "extra"},
		{"capabilities", "--format", "json", "--format", "json"},
		{"capabilities", "--Format", "json"},
		{"version", "extra"}, {"relocate-workspace", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := runnerCommand(args, unreadableBoot{}, &stdout, &stderr); status == 0 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunnerCommandPreservesExistingSubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"version"}, {"relocate-workspace", "--capabilities"},
		{"verify-preparation", "--capabilities"}, {"verify-preparation"}, {"verify-run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr, expectedOut, expectedErr bytes.Buffer
			status := runnerCommand(args, unreadableBoot{}, &stdout, &stderr)
			switch args[0] {
			case "version":
				if status != 0 || !strings.HasPrefix(stdout.String(), "jpack-runner ") || stderr.Len() != 0 {
					t.Fatalf("version: %d %q %q", status, stdout.String(), stderr.String())
				}
			case "relocate-workspace":
				if status != 0 || stdout.String() != "{\"version\":1}\n" || stderr.Len() != 0 {
					t.Fatalf("relocation probe: %d %q %q", status, stdout.String(), stderr.String())
				}
			case "verify-preparation":
				expected := verifyPreparation(args[1:], &expectedOut, &expectedErr)
				if status != expected || stdout.String() != expectedOut.String() || stderr.String() != expectedErr.String() {
					t.Fatalf("verification dispatch changed: %d %q %q; expected %d %q %q", status, stdout.String(), stderr.String(), expected, expectedOut.String(), expectedErr.String())
				}
			case "verify-run":
				if status != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "file, profiles and release-digest are required") {
					t.Fatalf("run verification: %d %q %q", status, stdout.String(), stderr.String())
				}
			}
		})
	}
	// The relocation command still consumes its own request on stdin, rather
	// than taking the companion's boot path. An empty request cannot mutate.
	var stdout, stderr bytes.Buffer
	if status := runnerCommand([]string{"relocate-workspace"}, strings.NewReader(""), &stdout, &stderr); status != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "boot") {
		t.Fatalf("relocation request dispatch: %d %q %q", status, stdout.String(), stderr.String())
	}
}

func capabilityBoot(t *testing.T, state, requirementMembers string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"dir": state, "runtime": filepath.Join(state, "missing-runtime"),
		"workspace": "workspace", "owner": "owner", "token": strings.Repeat("secret", 8),
		"inputRoot": filepath.Join(state, "missing-workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.TrimSuffix(string(raw), "}") + requirementMembers + "}\n")
}

func TestBootCapabilitiesPreserveLegacyConfiguration(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	legacy, token, err := bootConfig(capabilityBoot(t, state, ""))
	if err != nil || legacy.Dir != state || legacy.Runtime != filepath.Join(state, "missing-runtime") || legacy.Workspace != "workspace" || legacy.Owner != "owner" || len(token) < 32 {
		t.Fatalf("legacy boot refused or changed: %+v %q %v", legacy, token, err)
	}
	for _, member := range []string{
		`,"requiredCapabilities":{"version":1,"capabilities":[]}`,
		`,"requiredCapabilities":{"version":1,"capabilities":["jobs/1","required-boot-capabilities/1"]}`,
		`,"requiredCapabilities":{"capabilities":["required-boot-capabilities/1"],"version":1}`,
		`,"required\u0043apabilities":{"version":1,"capabilities":["jobs/1"]}`,
		// Legacy unknown boot fields continue to be ignored.
		`,"futureLegacySetting":true`,
	} {
		cfg, bearer, err := bootConfig(capabilityBoot(t, state, member))
		if err != nil || bearer != token || !reflect.DeepEqual(cfg, legacy) {
			t.Fatalf("supported boot changed configuration: %s %+v %v", member, cfg, err)
		}
	}
}

func TestBootCapabilityRefusalsPrecedeEveryStartupEffect(t *testing.T) {
	for name, members := range map[string]string{
		"governance unavailable":          `,"requiredCapabilities":{"version":1,"capabilities":["governance-admission/1"]}`,
		"unknown capability":              `,"requiredCapabilities":{"version":1,"capabilities":["future/1"]}`,
		"wrong capability version":        `,"requiredCapabilities":{"version":1,"capabilities":["jobs/2"]}`,
		"null requirements":               `,"requiredCapabilities":null`,
		"array requirements":              `,"requiredCapabilities":[]`,
		"string requirements":             `,"requiredCapabilities":"jobs/1"`,
		"missing version":                 `,"requiredCapabilities":{"capabilities":[]}`,
		"unknown version":                 `,"requiredCapabilities":{"version":2,"capabilities":[]}`,
		"fractional version":              `,"requiredCapabilities":{"version":1.0,"capabilities":[]}`,
		"string version":                  `,"requiredCapabilities":{"version":"1","capabilities":[]}`,
		"null version":                    `,"requiredCapabilities":{"version":null,"capabilities":[]}`,
		"missing capabilities":            `,"requiredCapabilities":{"version":1}`,
		"null capabilities":               `,"requiredCapabilities":{"version":1,"capabilities":null}`,
		"string capabilities":             `,"requiredCapabilities":{"version":1,"capabilities":"jobs/1"}`,
		"numeric capability":              `,"requiredCapabilities":{"version":1,"capabilities":[1]}`,
		"null capability":                 `,"requiredCapabilities":{"version":1,"capabilities":[null]}`,
		"empty capability":                `,"requiredCapabilities":{"version":1,"capabilities":[""]}`,
		"duplicate capability":            `,"requiredCapabilities":{"version":1,"capabilities":["jobs/1","jobs/1"]}`,
		"unknown member":                  `,"requiredCapabilities":{"version":1,"capabilities":[],"optional":true}`,
		"case aliased requirements":       `,"RequiredCapabilities":{"version":1,"capabilities":["governance-admission/1"]}`,
		"case aliased version":            `,"requiredCapabilities":{"Version":1,"capabilities":[]}`,
		"case aliased capability list":    `,"requiredCapabilities":{"version":1,"Capabilities":[]}`,
		"duplicate requirement downgrade": `,"requiredCapabilities":{"version":1,"capabilities":["governance-admission/1"]},"requiredCapabilities":{"version":1,"capabilities":[]}`,
		"escaped requirement downgrade":   `,"requiredCapabilities":{"version":1,"capabilities":["governance-admission/1"]},"required\u0043apabilities":{"version":1,"capabilities":[]}`,
		"case requirement downgrade":      `,"requiredCapabilities":{"version":1,"capabilities":["governance-admission/1"]},"REQUIREDCAPABILITIES":{"version":1,"capabilities":[]}`,
		"duplicate list downgrade":        `,"requiredCapabilities":{"version":1,"capabilities":["governance-admission/1"],"capabilities":[]}`,
		"escaped list downgrade":          `,"requiredCapabilities":{"version":1,"capabilities":["governance-admission/1"],"capabi\u006cities":[]}`,
		"duplicate version downgrade":     `,"requiredCapabilities":{"version":2,"version":1,"capabilities":[]}`,
		"duplicate other boot member":     `,"owner":"replacement"`,
		"duplicate nested boot member":    `,"futureLegacySetting":{"value":1,"value":2}`,
		"oversized list":                  `,"requiredCapabilities":{"version":1,"capabilities":[` + strings.Repeat(`"jobs/1",`, 16) + `"jobs/1"]}`,
		"oversized name":                  `,"requiredCapabilities":{"version":1,"capabilities":["` + strings.Repeat("x", 65) + `"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "must-not-be-created")
			boot := capabilityBoot(t, state, members)
			var stdout, stderr bytes.Buffer
			status := runnerCommand(nil, bytes.NewReader(boot), &stdout, &stderr)
			if status == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "boot") {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), state) || strings.Contains(stderr.String(), "secret") {
				t.Fatalf("boot authority leaked: %q", stderr.String())
			}
			// Open creates the state directory before pinning the absent
			// Runtime and before starting any worker. Its absence proves the
			// requirement refused before that boundary, not after a handshake.
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("refused boot initialized Runner state: %v", err)
			}
		})
	}
}

func TestBootCapabilitiesRejectMalformedEnvelopes(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `"boot"`, `{`, `{"token":"` + strings.Repeat("t", 32) + `"} {}`,
		`{"token":"` + strings.Repeat("t", 32) + `","requiredCapabilities":{"version":1,"capabilities":[]},}`,
		`{"extra":` + strings.Repeat("[", 130) + `0` + strings.Repeat("]", 130) + `}`,
		`{"padding":"` + strings.Repeat("x", 128<<10) + `"}`,
	} {
		if _, _, err := bootConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted malformed boot: %.100q", raw)
		}
	}
}
