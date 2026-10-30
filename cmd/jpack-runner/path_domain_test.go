package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The boot line's paths are in Runner's path domain (#202): valid UTF-8 with
// no NUL. A boot line outside it is refused before any state exists, never
// normalized.
func TestBootLinePathsOutsideTheDomainAreRefusedBeforeAnyState(t *testing.T) {
	for _, c := range []struct{ name, from, to, code string }{
		{"raw byte in inputRoot", `missing-workspace"`, "missing-workspace\xff\"", "request-not-utf8"},
		{"raw byte outside a path", `"owner":"owner"`, "\"owner\":\"own\xfe\"", "request-not-utf8"},
		{"encoded surrogate in dir", `"dir":"`, "\"dir\":\"\xed\xa0\x80", "request-not-utf8"},
		{"lone high in inputRoot", `missing-workspace"`, `missing-workspace\ud800"`, "path-unpaired-surrogate"},
		{"lone low in runtime", `missing-runtime"`, `missing-runtime\udc00"`, "path-unpaired-surrogate"},
		{"high then letter in dir", `"dir":"`, `"dir":"` + esc("d800") + esc("0041"), "path-unpaired-surrogate"},
		{"lone high in workspace", `"workspace":"workspace"`, `"workspace":"work\udbff"`, "path-unpaired-surrogate"},
		{"case-aliased inputRoot", `"inputRoot":"`, `"InputRoot":"\udfff`, "path-unpaired-surrogate"},
		{"escaped member name", `"inputRoot":"`, `"input` + esc("0052") + `oot":"` + esc("d800"), "path-unpaired-surrogate"},
		{"NUL in inputRoot", `missing-workspace"`, `missing-workspace\u0000"`, "path-contains-nul"},
		{"NUL in dir", `"dir":"`, `"dir":"\u0000`, "path-contains-nul"},
		// Every occurrence is checked: a case-variant duplicate, which
		// encoding/json would let replace the first, cannot hide behind a
		// valid one.
		{"case-variant duplicate after a valid one", `missing-workspace"`, `missing-workspace","InputRoot":"/x` + esc("d800") + `"`, "path-unpaired-surrogate"},
		// A high surrogate is paired only by a \u escape: another escape
		// followed by four hexadecimal digits is not its low half.
		{"high then another escape and hex", `missing-workspace"`, `missing-workspace` + esc("d800") + `\nDC00"`, "path-unpaired-surrogate"},
	} {
		t.Run(c.name, func(t *testing.T) {
			parent := t.TempDir()
			state := filepath.Join(parent, "must-not-be-created")
			boot := capabilityBoot(t, state, "")
			changed := bytes.Replace(boot, []byte(c.from), []byte(c.to), 1)
			if bytes.Equal(changed, boot) {
				t.Fatal("the case changed nothing")
			}
			var stdout, stderr bytes.Buffer
			status := runnerCommand(nil, bytes.NewReader(changed), &stdout, &stderr)
			if status == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "("+c.code+")") {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "secret") || strings.Contains(stderr.String(), parent) {
				t.Fatalf("boot authority leaked: %q", stderr.String())
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 0 {
				t.Fatalf("a refused boot created %v", entries)
			}
		})
	}
}

// A paired escape and a literal U+FFFD, raw or escaped, are valid paths and
// reach the configuration exactly.
func TestBootLineKeepsValidPathsExactly(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	for spelled, want := range map[string]string{
		`missing-workspace` + esc("d83d") + esc("de00") + `"`: "missing-workspace\U0001F600",
		"missing-workspace\xef\xbf\xbd\"":                     "missing-workspace\xef\xbf\xbd",
		`missing-workspace` + esc("fffd") + `"`:               "missing-workspace\xef\xbf\xbd",
	} {
		boot := bytes.Replace(capabilityBoot(t, state, ""), []byte(`missing-workspace"`), []byte(spelled), 1)
		cfg, _, err := bootConfig(boot)
		if err != nil || cfg.InputRoot != filepath.Join(state, want) {
			t.Fatalf("%s: %q %v", spelled, cfg.InputRoot, err)
		}
	}
}

// Runner names a binding that holds U+FFFD on standard error when it opens,
// with whether the bound root exists.
func TestServeNamesABindingHoldingUFFFD(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(parent, "never-run")
	if err = os.WriteFile(runtime, []byte("not a Runtime: no test runs it\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "project\xef\xbf\xbd")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	// A root whose name holds a newline is quoted, so it cannot forge a
	// second line.
	forging := filepath.Join(parent, "project\xef\xbf\xbd\nrunner: forged")
	if err = os.Mkdir(forging, 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, inputRoot, want string
	}{
		{"flagged", root, "the bound root exists"},
		{"newline", forging, "the bound root exists"},
		{"plain", parent, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := filepath.Join(parent, "state-"+c.name)
			line, err := json.Marshal(map[string]string{"dir": state, "runtime": runtime, "workspace": "workspace", "owner": "owner", "token": strings.Repeat("secret", 8), "inputRoot": c.inputRoot})
			if err != nil {
				t.Fatal(err)
			}
			boot := string(line) + "\n"
			var stdout, stderr bytes.Buffer
			if status := runnerCommand(nil, strings.NewReader(boot), &stdout, &stderr); status != 0 {
				t.Fatalf("status=%d stderr=%q", status, stderr.String())
			}
			if c.want == "" {
				if strings.Contains(stderr.String(), "U+FFFD") {
					t.Fatalf("a plain binding was flagged: %q", stderr.String())
				}
				return
			}
			if !strings.Contains(stderr.String(), "U+FFFD") || !strings.Contains(stderr.String(), c.want) || !strings.Contains(stderr.String(), "inputRoot") {
				t.Fatalf("open diagnostics do not flag the binding: %q", stderr.String())
			}
			for _, l := range strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n") {
				if !strings.HasPrefix(l, "runner: ") || strings.HasPrefix(l, "runner: forged") {
					t.Fatalf("a binding's name forged a line on standard error: %q", stderr.String())
				}
			}
		})
	}
}

// esc is the JSON escape of one UTF-16 code unit, given in hexadecimal.
func esc(hex string) string { return "\\" + "u" + hex }
