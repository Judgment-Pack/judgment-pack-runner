package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyPreparationCLIIsOfflineAndCanonical(t *testing.T) {
	root := filepath.Join("..", "..", "vectors", "preparation", "valid")
	preparation, err := os.ReadFile(filepath.Join(root, "preparation.json"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--dialect", filepath.Join(root, "mapping.json"), "--case", filepath.Join(root, "case.json"), "--profiles", filepath.Join(root, "profiles.json"), "--acquisitions", filepath.Join(root, "sources.json"), "--preparation", filepath.Join(root, "preparation.json"), "--expected-digest", fmt.Sprintf("sha256:%x", sha256.Sum256(preparation))}
	var out, diagnostics bytes.Buffer
	if code := verifyPreparation(args, &out, &diagnostics); code != 0 {
		t.Fatalf("%d %s %s", code, out.String(), diagnostics.String())
	}
	if !strings.HasPrefix(out.String(), "{\"binding\":\"not-checked\",") || !strings.Contains(out.String(), "\"result\":\"reading\"") {
		t.Fatal(out.String())
	}
	args[len(args)-1] = "sha256:" + strings.Repeat("0", 64)
	out.Reset()
	if code := verifyPreparation(args, &out, &diagnostics); code != 2 || out.String() != "{\"finding\":\"preparation-differs\",\"result\":\"finding\"}\n" {
		t.Fatalf("%d %s", code, out.String())
	}
	dir := t.TempDir()
	oversize := filepath.Join(dir, "oversize.json")
	os.WriteFile(oversize, []byte(strings.Repeat(" ", (128<<10)+1)), 0600)
	args[1] = oversize
	out.Reset()
	if code := verifyPreparation(args, &out, &diagnostics); code != 1 || out.String() != "{\"refusal\":\"input-over-bound\",\"result\":\"refusal\"}\n" {
		t.Fatalf("%d %s", code, out.String())
	}
	args[1] = filepath.Join(dir, "absent")
	out.Reset()
	if code := verifyPreparation(args, &out, &diagnostics); code != 3 || out.Len() != 0 {
		t.Fatalf("%d %s", code, out.String())
	}
	for _, args := range [][]string{nil, {"--dialect", "missing"}, {"--capabilities", "--case", "x"}, {"--unknown"}} {
		out.Reset()
		if code := verifyPreparation(args, &out, &diagnostics); code != 3 {
			t.Fatal(args, code)
		}
	}
	out.Reset()
	if code := verifyPreparation([]string{"--capabilities"}, &out, &diagnostics); code != 0 || !strings.Contains(out.String(), "\"offline\":true") {
		t.Fatalf("%d %s", code, out.String())
	}
}
