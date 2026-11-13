//go:build linux || darwin || freebsd

package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ownDir is a directory of the test's own, named by its real path, which
// only the test's user can enter.
func ownDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	must(t, os.Chmod(dir, 0o700))
	return dir
}

// writeMode writes data at path with mode, whatever the umask.
func writeMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	must(t, os.WriteFile(path, data, mode))
	must(t, os.Chmod(path, mode))
}

// A service configuration is held to the signing key's rules, and read
// once: a refusal never names the path.
func TestAServiceConfigurationIsHeldAsTheSigningKeyIs(t *testing.T) {
	dir := ownDir(t)
	content := []byte(`{"listen":"127.0.0.1:8470"}`)
	good := filepath.Join(dir, "service.json")
	writeMode(t, good, content, 0o600)
	if got, err := ReadServiceConfig(good); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("%q %v", got, err)
	}
	full := filepath.Join(dir, "full.json")
	writeMode(t, full, bytes.Repeat([]byte(" "), MaxServiceConfig), 0o600)
	if got, err := ReadServiceConfig(full); err != nil || len(got) != MaxServiceConfig {
		t.Fatalf("%d %v", len(got), err)
	}

	cases := map[string]func() (string, string){
		"readable by its group": func() (string, string) {
			p := filepath.Join(dir, "group.json")
			writeMode(t, p, content, 0o640)
			return p, "neither its group nor others may read or write it"
		},
		"readable by others": func() (string, string) {
			p := filepath.Join(dir, "others.json")
			writeMode(t, p, content, 0o604)
			return p, "neither its group nor others may read or write it"
		},
		"writable by its group": func() (string, string) {
			p := filepath.Join(dir, "group-writes.json")
			writeMode(t, p, content, 0o620)
			return p, "neither its group nor others may read or write it"
		},
		"a second name": func() (string, string) {
			p := filepath.Join(dir, "linked.json")
			writeMode(t, p, content, 0o600)
			must(t, os.Link(p, filepath.Join(dir, "linked-again.json")))
			return p, "it must have one name"
		},
		"a symbolic link": func() (string, string) {
			p := filepath.Join(dir, "link.json")
			must(t, os.Symlink(good, p))
			return p, "no symbolic link may be anywhere in its path"
		},
		"a symbolic link on its path": func() (string, string) {
			real := filepath.Join(dir, "real")
			must(t, os.Mkdir(real, 0o700))
			writeMode(t, filepath.Join(real, "service.json"), content, 0o600)
			must(t, os.Symlink(real, filepath.Join(dir, "alias")))
			return filepath.Join(dir, "alias", "service.json"), "no symbolic link may be anywhere in its path"
		},
		"a relative path":  func() (string, string) { return "service.json", "its path must be absolute and clean" },
		"a path not clean": func() (string, string) { return dir + "/./service.json", "its path must be absolute and clean" },
		"a directory":      func() (string, string) { return dir, "it must be a regular file" },
		"no file":          func() (string, string) { return filepath.Join(dir, "absent.json"), "there is no file at its path" },
		"through a file": func() (string, string) {
			return filepath.Join(good, "service.json"), "passes through something that is not a directory"
		},
		"longer than allowed": func() (string, string) {
			p := filepath.Join(dir, "long.json")
			writeMode(t, p, bytes.Repeat([]byte(" "), MaxServiceConfig+1), 0o600)
			return p, "it is longer than"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			path, want := setup()
			got, err := ReadServiceConfig(path)
			if err == nil || !strings.Contains(err.Error(), want) || got != nil {
				t.Fatalf("%q %v, want a refusal saying %q", got, err, want)
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "service.json") {
				t.Fatalf("the refusal names the path: %v", err)
			}
		})
	}

	// Outside the state directory, which need not exist yet; never inside it.
	state := filepath.Join(dir, "state")
	if err := ServiceConfigOutsideState(good, state); err != nil {
		t.Fatal(err)
	}
	must(t, os.Mkdir(state, 0o700))
	if err := ServiceConfigOutsideState(good, state); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(state, "runtimes", "service.json")
	must(t, os.Mkdir(filepath.Dir(inside), 0o700))
	writeMode(t, inside, content, 0o600)
	if err := ServiceConfigOutsideState(inside, state); err == nil || !strings.Contains(err.Error(), "inside Runner's state directory") {
		t.Fatal(err)
	}
}

// An identity's key set is public: others may read its file, but no one but
// its owner, Runner's user or root, may write it, and it is held to the
// configuration's placement and kept outside the state directory.
func TestAKeySetFileIsHeldToItsPlacement(t *testing.T) {
	dir := ownDir(t)
	state := filepath.Join(dir, "state")
	signer := edSigner(t, "k1")
	set := encode(map[string]any{"keys": []any{signer.jwk}})
	config := IdentityConfig{Issuer: testIssuer, Audience: testAudience, Subjects: []string{"ops-service"}}
	for _, mode := range []os.FileMode{0o644, 0o600, 0o444} {
		config.Keys = filepath.Join(dir, "keys-"+mode.String()+".json")
		writeMode(t, config.Keys, set, mode)
		id, err := LoadIdentity(config, "journal-owner", state)
		if err != nil {
			t.Fatalf("%v: %v", mode, err)
		}
		if by, err := id.check([]string{"Bearer " + tokenOf(signer, claimsFor("ops-service", time.Now()))}, time.Now()); err != nil || by.subject != "ops-service" {
			t.Fatalf("%v: %+v %v", mode, by, err)
		}
	}
	must(t, os.Mkdir(state, 0o700))
	cases := map[string]func() (string, string){
		"writable by its group": func() (string, string) {
			p := filepath.Join(dir, "group.json")
			writeMode(t, p, set, 0o664)
			return p, "identity.keys is refused: neither its group nor others may write it"
		},
		"writable by others": func() (string, string) {
			p := filepath.Join(dir, "others.json")
			writeMode(t, p, set, 0o646)
			return p, "identity.keys is refused: neither its group nor others may write it"
		},
		"a symbolic link": func() (string, string) {
			p := filepath.Join(dir, "link.json")
			must(t, os.Symlink(filepath.Join(dir, "keys-"+os.FileMode(0o644).String()+".json"), p))
			return p, "identity.keys is refused: no symbolic link"
		},
		"a directory": func() (string, string) { return dir, "identity.keys is refused: it must be a regular file" },
		"inside the state directory": func() (string, string) {
			p := filepath.Join(state, "keys.json")
			writeMode(t, p, set, 0o644)
			return p, "identity.keys is refused: it is inside Runner's state directory"
		},
		"longer than a key set": func() (string, string) {
			p := filepath.Join(dir, "long.json")
			writeMode(t, p, bytes.Repeat([]byte(" "), maxKeySetBytes+1), 0o644)
			return p, "identity.keys is refused: it is longer than"
		},
		"not a key set": func() (string, string) {
			p := filepath.Join(dir, "not-a-set.json")
			writeMode(t, p, []byte(`{"keys":[{"kid":"k1","kty":"oct","k":"AA"}]}`), 0o644)
			return p, "identity.keys: key 0 of the key set holds the private member k"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := config
			var want string
			c.Keys, want = setup()
			id, err := LoadIdentity(c, "journal-owner", state)
			if err == nil || !strings.Contains(err.Error(), want) || id != nil {
				t.Fatalf("%v %v, want a refusal saying %q", id, err, want)
			}
			if strings.Contains(err.Error(), dir) {
				t.Fatalf("the refusal names the path: %v", err)
			}
		})
	}
}

// A file changed between its check at its path and its open is refused: one
// put in its place, by device and inode, and one whose mode was widened, by
// the attributes of the handle read.
func TestAServiceFileChangedBeforeItIsOpenedIsRefused(t *testing.T) {
	dir := ownDir(t)
	content := []byte(`{"listen":"127.0.0.1:8470"}`)
	path := filepath.Join(dir, "service.json")
	t.Cleanup(func() { placedOpenHook = nil })
	for name, c := range map[string]struct {
		change func()
		want   string
	}{
		"replaced": {func() {
			other := filepath.Join(dir, "other.json")
			writeMode(t, other, content, 0o600)
			must(t, os.Rename(other, path))
		}, "it was replaced while it was opened"},
		"made readable by others": {func() { must(t, os.Chmod(path, 0o644)) }, "neither its group nor others may read or write it"},
	} {
		t.Run(name, func(t *testing.T) {
			writeMode(t, path, content, 0o600)
			placedOpenHook = c.change
			defer func() { placedOpenHook = nil }()
			if got, err := ReadServiceConfig(path); err == nil || !strings.Contains(err.Error(), c.want) || got != nil {
				t.Fatalf("%q %v, want a refusal saying %q", got, err, c.want)
			}
		})
	}
	// A key set file made writable by others before its open is refused.
	state := filepath.Join(dir, "state")
	keys := filepath.Join(dir, "keys.json")
	writeMode(t, keys, encode(map[string]any{"keys": []any{edSigner(t, "k1").jwk}}), 0o644)
	placedOpenHook = func() { must(t, os.Chmod(keys, 0o666)) }
	defer func() { placedOpenHook = nil }()
	if id, err := LoadIdentity(IdentityConfig{Issuer: testIssuer, Audience: testAudience, Subjects: []string{"ops-service"}, Keys: keys}, "journal-owner", state); err == nil || !strings.Contains(err.Error(), "neither its group nor others may write it") || id != nil {
		t.Fatalf("%v %v", id, err)
	}
}
