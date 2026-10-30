package runner

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Runner's path domain (#202): every host path and every stored input root
// binding is valid UTF-8 with no NUL. These tests hold the refusals, the
// diagnosis of a stored binding, and the U+FFFD flag.

// stateBytes is every file under dir with the digest of its bytes: two equal
// snapshots mean nothing was created, removed or written.
func stateBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			files[path] = "dir"
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		files[path] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sameState(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// metadataRows is the store's metadata table, key by key, as its bytes.
func metadataRows(t *testing.T, dir string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "runs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT key, value FROM metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	held := map[string]string{}
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		held[k] = v
	}
	return held
}

// setBinding writes a stored input root binding as a direct Go host could
// have made it, bytes as given.
func setBinding(t *testing.T, dir, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "runs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE metadata SET value=? WHERE key='inputRoot'", value); err != nil {
		t.Fatal(err)
	}
	var held string
	if err = db.QueryRow("SELECT value FROM metadata WHERE key='inputRoot'").Scan(&held); err != nil || held != value {
		t.Fatalf("binding not stored as given: %q %v", held, err)
	}
}

// pathDomainStore is a closed store bound to an existing input root.
func pathDomainStore(t *testing.T) Config {
	t.Helper()
	cfg := journalConfig(t)
	cfg.InputRoot = relocationTempDir(t)
	openJournal(t, cfg).Close()
	return cfg
}

func TestOpenRefusesHostPathsOutsideTheDomain(t *testing.T) {
	for _, c := range []struct{ member, value, code string }{
		{"dir", "/state/\xff", "path-not-utf8"},
		{"dir", "/state/a\x00b", "path-contains-nul"},
		{"runtime", "/runtime/\xc3", "path-not-utf8"},
		{"runtime", "/runtime\x00", "path-contains-nul"},
		{"workspace", "work\xffspace", "path-not-utf8"},
		{"workspace", "work\x00space", "path-contains-nul"},
		{"inputRoot", "/project/\xed\xa0\x80", "path-not-utf8"},
		{"inputRoot", "/project/a\x00", "path-contains-nul"},
	} {
		t.Run(c.member+"/"+c.code, func(t *testing.T) {
			cfg := journalConfig(t)
			state := filepath.Join(cfg.Dir, "must-not-be-created")
			cfg.Dir = state
			switch c.member {
			case "dir":
				cfg.Dir = state + strings.TrimPrefix(c.value, "/state")
			case "runtime":
				cfg.Runtime = c.value
			case "workspace":
				cfg.Workspace = c.value
			case "inputRoot":
				cfg.InputRoot = c.value
			}
			s, err := Open(cfg)
			if err == nil {
				s.Close()
				t.Fatalf("opened with %s %q", c.member, c.value)
			}
			if !strings.Contains(err.Error(), "("+c.code+")") || !strings.Contains(err.Error(), c.member) {
				t.Fatalf("refusal does not name %s and %s: %v", c.member, c.code, err)
			}
			if _, e := os.Lstat(filepath.Dir(state)); e != nil {
				t.Fatal(e)
			}
			entries, _ := os.ReadDir(filepath.Dir(state))
			if len(entries) != 0 {
				t.Fatalf("a refused open created %v", entries)
			}
		})
	}
}

func TestRelocationRefusesHostPathsOutsideTheDomain(t *testing.T) {
	cfg := pathDomainStore(t)
	// A real directory whose name is not UTF-8, where the file system allows
	// one: the refusal must not depend on whether the destination exists.
	parent := relocationTempDir(t)
	raw := filepath.Join(parent, "moved\xff")
	_ = os.Mkdir(raw, 0700)
	valid := filepath.Join(parent, "moved")
	if err := os.Mkdir(valid, 0700); err != nil {
		t.Fatal(err)
	}
	base := RelocateRequest{Version: 1, ID: strings.Repeat("d", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: cfg.InputRoot, InputRoot: valid}
	for _, c := range []struct {
		name, code string
		change     func(*RelocateRequest)
	}{
		{"inputRoot not UTF-8", "path-not-utf8", func(r *RelocateRequest) { r.InputRoot = raw }},
		{"inputRoot NUL", "path-contains-nul", func(r *RelocateRequest) { r.InputRoot = valid + "\x00" }},
		{"previousInputRoot not UTF-8", "path-not-utf8", func(r *RelocateRequest) { r.PreviousInputRoot = cfg.InputRoot + "\xff" }},
		{"previousInputRoot NUL", "path-contains-nul", func(r *RelocateRequest) { r.PreviousInputRoot = cfg.InputRoot + "\x00" }},
		{"dir not UTF-8", "path-not-utf8", func(r *RelocateRequest) { r.Dir = cfg.Dir + "\xff" }},
		{"dir NUL", "path-contains-nul", func(r *RelocateRequest) { r.Dir = cfg.Dir + "\x00" }},
		{"workspace not UTF-8", "path-not-utf8", func(r *RelocateRequest) { r.Workspace = cfg.Workspace + "\xff" }},
		{"workspace NUL", "path-contains-nul", func(r *RelocateRequest) { r.Workspace = cfg.Workspace + "\x00" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := stateBytes(t, cfg.Dir)
			request := base
			c.change(&request)
			if _, err := RelocateWorkspace(request); err == nil || !strings.Contains(err.Error(), "("+c.code+")") {
				t.Fatalf("relocation not refused with %s: %v", c.code, err)
			}
			if !sameState(before, stateBytes(t, cfg.Dir)) {
				t.Fatal("a refused relocation wrote to the workspace")
			}
		})
	}
	// The valid request still relocates, so the refusals above were the path
	// domain's and not the request's.
	if result, err := RelocateWorkspace(base); err != nil || result.AlreadyApplied {
		t.Fatal(result, err)
	}
}

func TestRelocationRequestBytesAreRefusedBeforeDecoding(t *testing.T) {
	member := func(name, spelled string) string {
		return `{"version":1,"id":"` + strings.Repeat("e", 32) + `","dir":"/state","workspace":"w","owner":"o","previousInputRoot":"/old","inputRoot":"/new",` + `"` + name + `":"` + spelled + `"}`
	}
	for _, c := range []struct{ name, raw, code string }{
		{"raw byte", strings.Replace(member("x", ""), `"/new"`, "\"/new\xff\"", 1), "request-not-utf8"},
		{"raw byte outside a path", strings.Replace(member("x", ""), `"owner":"o"`, "\"owner\":\"o\xfe\"", 1), "request-not-utf8"},
		{"encoded surrogate", strings.Replace(member("x", ""), `"/new"`, "\"/new\xed\xa0\x80\"", 1), "request-not-utf8"},
		{"lone high", strings.Replace(member("x", ""), `"/new"`, `"/new\ud800"`, 1), "path-unpaired-surrogate"},
		{"lone low", strings.Replace(member("x", ""), `"/old"`, `"/old\udc00"`, 1), "path-unpaired-surrogate"},
		{"high then letter", strings.Replace(member("x", ""), `"/state"`, `"/state`+esc("d800")+esc("0041")+`"`, 1), "path-unpaired-surrogate"},
		{"high then high", strings.Replace(member("x", ""), `"w"`, `"w\ud800\ud800"`, 1), "path-unpaired-surrogate"},
		{"high at the end", strings.Replace(member("x", ""), `"/new"`, `"\ud83d"`, 1), "path-unpaired-surrogate"},
		{"case-aliased member", strings.Replace(member("x", ""), `"inputRoot":"/new"`, `"INPUTROOT":"/new\udfff"`, 1), "path-unpaired-surrogate"},
		{"escaped member name", strings.Replace(member("x", ""), `"inputRoot":"/new"`, `"input`+esc("0052")+`oot":"/new`+esc("dfff")+`"`, 1), "path-unpaired-surrogate"},
		{"case-variant duplicate after a valid one", strings.Replace(member("x", ""), `"inputRoot":"/new"`, `"inputRoot":"/new","InputRoot":"/x`+esc("d800")+`"`, 1), "path-unpaired-surrogate"},
		{"high then another escape and hex", strings.Replace(member("x", ""), `"/new"`, `"/new`+esc("d800")+`\nDC00"`, 1), "path-unpaired-surrogate"},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := strings.Replace(c.raw, `,"x":""`, "", 1)
			_, err := DecodeRelocateRequest([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), "("+c.code+")") {
				t.Fatalf("%s: not refused with %s: %v", raw, c.code, err)
			}
		})
	}
	// A paired escape, a literal U+FFFD and its escape decode exactly.
	for spelled, want := range map[string]string{`/new` + esc("d83d") + esc("de00"): "/new\U0001F600", "/new\xef\xbf\xbd": "/new\xef\xbf\xbd", `/new` + esc("fffd"): "/new\xef\xbf\xbd"} {
		raw := strings.Replace(member("x", ""), `"/new"`, `"`+spelled+`"`, 1)
		raw = strings.Replace(raw, `,"x":""`, "", 1)
		request, err := DecodeRelocateRequest([]byte(raw))
		if err != nil || request.InputRoot != want {
			t.Fatalf("%s: %q %v", spelled, request.InputRoot, err)
		}
	}
}

func TestAStoredBindingOutsideTheDomainIsRefusedByName(t *testing.T) {
	for _, c := range []struct{ binding, code, shown string }{
		{"/project/\xffold", "stored-binding-not-utf8", `"/project/\xffold"`},
		{"/project/a\x00b", "stored-binding-contains-nul", `"/project/a\x00b"`},
	} {
		t.Run(c.code, func(t *testing.T) {
			cfg := pathDomainStore(t)
			setBinding(t, cfg.Dir, c.binding)
			held := metadataRows(t, cfg.Dir)
			// Whatever the host now names, the stored binding is refused by
			// name, with its bytes escaped, and never guessed.
			for _, root := range []string{cfg.InputRoot, ""} {
				cfg.InputRoot = root
				s, err := Open(cfg)
				if err == nil {
					s.Close()
					t.Fatal("opened a store whose binding is outside the path domain")
				}
				if !strings.Contains(err.Error(), "("+c.code+")") || !strings.Contains(err.Error(), "inputRoot") || !strings.Contains(err.Error(), c.shown) {
					t.Fatalf("refusal does not name the binding with its bytes escaped: %v", err)
				}
			}
			request := RelocateRequest{Version: 1, ID: strings.Repeat("f", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: relocationTempDir(t), InputRoot: relocationTempDir(t)}
			if _, err := RelocateWorkspace(request); err == nil || !strings.Contains(err.Error(), "("+c.code+")") || !strings.Contains(err.Error(), c.shown) {
				t.Fatalf("relocation does not name the stored binding: %v", err)
			}
			after := metadataRows(t, cfg.Dir)
			if len(after) != len(held) {
				t.Fatal("a refusal recorded metadata", after)
			}
			for k, v := range held {
				if after[k] != v {
					t.Fatalf("a refusal changed %s", k)
				}
			}
		})
	}
}

type pathStatus struct {
	Replacement *bool `json:"inputRootReplacementCharacter"`
	Exists      *bool `json:"inputRootExists"`
}

func statusOf(t *testing.T, s *Service) pathStatus {
	t.Helper()
	var status pathStatus
	w := serve(t, s, "GET", "/v1/status", "", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return status
}

func TestTheStatusFlagsABindingHoldingUFFFD(t *testing.T) {
	parent := relocationTempDir(t)
	flagged := filepath.Join(parent, "project\xef\xbf\xbd")
	if err := os.Mkdir(flagged, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := journalConfig(t)
	cfg.InputRoot = flagged
	s := openJournal(t, cfg)
	status := statusOf(t, s)
	if status.Replacement == nil || !*status.Replacement || status.Exists == nil || !*status.Exists {
		t.Fatalf("status does not flag the binding: %+v", status)
	}
	binding := s.InputRootBinding()
	if binding.Root != flagged || !binding.ReplacementCharacter || !binding.Exists || !strings.Contains(binding.String(), "U+FFFD") || !strings.Contains(binding.String(), "the bound root exists") {
		t.Fatalf("open diagnostics: %+v %q", binding, binding.String())
	}
	if err := os.Remove(flagged); err != nil {
		t.Fatal(err)
	}
	if status = statusOf(t, s); status.Replacement == nil || !*status.Replacement || status.Exists == nil || *status.Exists {
		t.Fatalf("status does not say the bound root is gone: %+v", status)
	}
	if note := s.InputRootBinding().String(); !strings.Contains(note, "the bound root does not exist") {
		t.Fatalf("open diagnostics do not say the bound root is gone: %q", note)
	}
	s.Close()

	// A bound path that is a file exists, but not as a directory.
	file := filepath.Join(relocationTempDir(t), "project\xef\xbf\xbd")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	filed := journalConfig(t)
	filed.InputRoot = file
	s = openJournal(t, filed)
	if status = statusOf(t, s); status.Replacement == nil || !*status.Replacement || status.Exists == nil || *status.Exists || s.InputRootBinding().Exists {
		t.Fatalf("a file is reported as the bound directory: %+v", status)
	}
	s.Close()
	// The diagnostic quotes the root: a newline in its name stays escaped.
	if note := (InputRootBinding{Root: "/p\xef\xbf\xbd\nrunner: forged", ReplacementCharacter: true}).String(); strings.Contains(note, "\n") || !strings.Contains(note, `\nrunner: forged`) {
		t.Fatalf("the diagnostic does not quote the root: %q", note)
	}

	plain := journalConfig(t)
	plain.InputRoot = relocationTempDir(t)
	s = openJournal(t, plain)
	if status = statusOf(t, s); status.Replacement == nil || *status.Replacement || status.Exists == nil || !*status.Exists {
		t.Fatalf("status flags a plain binding: %+v", status)
	}
	if note := s.InputRootBinding().String(); note != "" {
		t.Fatalf("a plain binding has an open diagnostic: %q", note)
	}
	s.Close()

	unbound := openJournal(t, journalConfig(t))
	defer unbound.Close()
	if status = statusOf(t, unbound); status.Replacement != nil || status.Exists != nil {
		t.Fatalf("status reports a binding there is not: %+v", status)
	}
}

// A literal U+FFFD in a valid path is a path like any other: it opens,
// relocates, replays and binds byte for byte, and a non-UTF-8 spelling of
// the same name stays distinguishable because it is refused.
func TestALiteralUFFFDPathRoundTrips(t *testing.T) {
	parent := relocationTempDir(t)
	from := filepath.Join(parent, "from\xef\xbf\xbd")
	to := filepath.Join(parent, "to\xef\xbf\xbd")
	for _, d := range []string{from, to} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := journalConfig(t)
	cfg.InputRoot = from
	openJournal(t, cfg).Close()
	if got := metadataRows(t, cfg.Dir)["inputRoot"]; got != from {
		t.Fatalf("binding changed: %q", got)
	}
	raw, err := json.Marshal(RelocateRequest{Version: 1, ID: strings.Repeat("9", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: from, InputRoot: to})
	if err != nil {
		t.Fatal(err)
	}
	// The same request with each U+FFFD spelled as its escape decodes to
	// the same request.
	escaped := strings.ReplaceAll(string(raw), "\xef\xbf\xbd", esc("fffd"))
	if escaped == string(raw) {
		t.Fatal("marshal escaped U+FFFD")
	}
	for _, spelled := range []string{string(raw), escaped} {
		request, err := DecodeRelocateRequest([]byte(spelled))
		if err != nil || request.PreviousInputRoot != from || request.InputRoot != to {
			t.Fatalf("decoded %q %q %v", request.PreviousInputRoot, request.InputRoot, err)
		}
	}
	request, _ := DecodeRelocateRequest(raw)
	// A previous root that spells U+FFFD's bytes as one invalid byte is not
	// the binding.
	lossy := request
	lossy.PreviousInputRoot = filepath.Join(parent, "from\xff")
	if _, err = RelocateWorkspace(lossy); err == nil || !strings.Contains(err.Error(), "(path-not-utf8)") {
		t.Fatal("a non-UTF-8 previous root was not refused:", err)
	}
	result, err := RelocateWorkspace(request)
	if err != nil || result.AlreadyApplied || result.InputRoot != to {
		t.Fatal(result, err)
	}
	result, err = RelocateWorkspace(request)
	if err != nil || !result.AlreadyApplied || result.InputRoot != to {
		t.Fatal("replay:", result, err)
	}
	held := metadataRows(t, cfg.Dir)
	if held["inputRoot"] != to {
		t.Fatalf("binding after relocation: %q", held["inputRoot"])
	}
	var history struct{ Previous, Current string }
	if err = json.Unmarshal([]byte(held["inputRootRelocation:"+request.ID]), &history); err != nil || history.Previous != from || history.Current != to {
		t.Fatalf("relocation history: %+v %v", history, err)
	}
	cfg.InputRoot = to
	s := openJournal(t, cfg)
	defer s.Close()
	if status := statusOf(t, s); status.Replacement == nil || !*status.Replacement || status.Exists == nil || !*status.Exists {
		t.Fatalf("reopened binding: %+v", status)
	}
}

// esc is the JSON escape of one UTF-16 code unit, given in hexadecimal.
func esc(hex string) string { return "\\" + "u" + hex }
