//go:build linux || darwin || freebsd

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// The second storage kind in a service configuration (D1): the store member
// names the postgresql kind and its connection file, which is held to the
// configuration's rules; anything else is refused before anything is opened.
func TestAServiceConfigurationNamesTheSecondStorageKindByItsConnectionFile(t *testing.T) {
	f := newServiceFixture(t)
	connections := `{"request":"postgres://request@db.invalid/jobs","worker":"postgres://worker@db.invalid/jobs","owner":"jobs_owner"}`
	file := f.write("store.json", []byte(connections))
	store := func(v any) string {
		return f.config("service.json", f.members("127.0.0.1:8470", map[string]any{"store": v}))
	}
	c, err := readServiceConfig(store(map[string]any{"kind": "postgresql", "connection": file}))
	if err != nil {
		t.Fatal(err)
	}
	if c.runner.PostgreSQL == nil || *c.runner.PostgreSQL != (runner.PostgreSQLConnections{Request: "postgres://request@db.invalid/jobs", Worker: "postgres://worker@db.invalid/jobs", Owner: "jobs_owner"}) {
		t.Fatalf("the store member was not read: %+v", c.runner.PostgreSQL)
	}
	// Without it the store is the sqlite kind, as it always was.
	if c, err = readServiceConfig(f.config("plain.json", f.members("127.0.0.1:8470", nil))); err != nil || c.runner.PostgreSQL != nil {
		t.Fatal("a configuration without store did not keep the sqlite kind:", err)
	}

	open := f.write("open.json", []byte(connections))
	if err = os.Chmod(open, 0o640); err != nil {
		t.Fatal(err)
	}
	extra := f.write("extra.json", []byte(strings.TrimSuffix(connections, "}")+`,"migration":"postgres://m@db.invalid/jobs"}`))
	if err = os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(f.state, "store.json")
	if err = os.WriteFile(inside, []byte(connections), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		store any
		says  string
	}{
		{"not an object", "postgresql", "store, when present"},
		{"another kind", map[string]any{"kind": "sqlite", "connection": file}, "store, when present"},
		{"no kind", map[string]any{"connection": file}, "store, when present"},
		{"a member more", map[string]any{"kind": "postgresql", "connection": file, "pool": 4}, "store, when present"},
		{"a relative path", map[string]any{"kind": "postgresql", "connection": "store.json"}, "store, when present"},
		{"no such file", map[string]any{"kind": "postgresql", "connection": filepath.Join(f.dir, "absent.json")}, "connection file is refused"},
		{"a file its group may read", map[string]any{"kind": "postgresql", "connection": open}, "connection file is refused"},
		{"a file inside the state directory", map[string]any{"kind": "postgresql", "connection": inside}, "connection file is refused"},
		{"a file with a member more", map[string]any{"kind": "postgresql", "connection": extra}, "connection file is one JSON object"},
	} {
		_, err := readServiceConfig(store(c.store))
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want a refusal saying %q", c.name, err, c.says)
			continue
		}
		if strings.Contains(err.Error(), f.dir) || strings.Contains(err.Error(), "db.invalid") {
			t.Errorf("%s: the refusal repeats a path or a value: %v", c.name, err)
		}
	}
}

// `jpack-runner store migrate --connection <path>` takes that and nothing
// else, its file held to the configuration's rules and read exactly.
func TestStoreMigrateTakesItsConnectionFileAndNothingElse(t *testing.T) {
	f := newServiceFixture(t)
	good := `{"migration":"postgres://migration@127.0.0.1:1/jobs?connect_timeout=1","owner":"jobs_owner","request":"jobs_request","worker":"jobs_worker"}`
	file := f.write("migrate.json", []byte(good))
	short := f.write("short.json", []byte(`{"migration":"postgres://m@127.0.0.1:1/jobs","owner":"o","request":"r"}`))
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"store"}, "store takes migrate"},
		{[]string{"store", "migrate"}, "store takes migrate"},
		{[]string{"store", "migrate", "--connection"}, "store takes migrate"},
		{[]string{"store", "migrate", "--connection", "migrate.json"}, "store takes migrate"},
		{[]string{"store", "migrate", "--connection", file, "again"}, "store takes migrate"},
		{[]string{"store", "migrate", "--connection", file, "--even-with-live-claims", "again"}, "store takes migrate"},
		{[]string{"store", "migrate", "--even-with-live-claims", "--connection", file}, "store takes migrate"},
		{[]string{"store", "remove-scope", "--connection", file}, "store takes migrate"},
		{[]string{"store", "migrate", "--connection", short}, "one JSON object with migration, owner, request and worker"},
		// A database that cannot be reached is an error, after the file was
		// read whole.
		{[]string{"store", "migrate", "--connection", file}, "runner:"},
		{[]string{"store", "migrate", "--connection", file, "--even-with-live-claims"}, "runner:"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runnerCommand(c.args, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), c.says) || stdout.Len() != 0 {
			t.Errorf("%v: %d %q %q, want a refusal saying %q", c.args, code, stdout.String(), stderr.String(), c.says)
		}
	}
	m, err := runner.PostgreSQLMigrationFile([]byte(good))
	if err != nil || m != (runner.PostgreSQLMigration{Connection: "postgres://migration@127.0.0.1:1/jobs?connect_timeout=1", Owner: "jobs_owner", Request: "jobs_request", Worker: "jobs_worker"}) {
		t.Fatal("the migration's file was not read:", m, err)
	}
}
