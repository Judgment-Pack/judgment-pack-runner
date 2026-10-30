package runner

// Runner's path domain (#202). Every path a host gives Runner (the boot
// line's and a Go host's dir, workspace, runtime and inputRoot, and a
// relocation's dir, workspace, previousInputRoot and inputRoot) and every
// stored input root binding is valid UTF-8 with no NUL. A request outside it is
// refused, never normalized: encoding/json would decode an invalid byte, or an
// unpaired surrogate escape, as U+FFFD, which names another path. Raw-byte
// paths are not supported, and Runner never guesses the path a stored binding
// outside the domain stood for.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// The refusal codes of the path domain. Each refusal's message ends with its
// code in parentheses.
const (
	codeRequestNotUTF8           = "request-not-utf8"
	codePathUnpairedSurrogate    = "path-unpaired-surrogate"
	codePathNotUTF8              = "path-not-utf8"
	codePathContainsNUL          = "path-contains-nul"
	codeStoredBindingNotUTF8     = "stored-binding-not-utf8"
	codeStoredBindingContainsNUL = "stored-binding-contains-nul"
)

// bootPathMembers are the boot line's members in the path domain;
// relocationPathMembers are a relocation request's.
var (
	bootPathMembers       = []string{"dir", "workspace", "runtime", "inputRoot"}
	relocationPathMembers = []string{"dir", "workspace", "previousInputRoot", "inputRoot"}
)

// CheckBootLinePaths refuses a boot line, raw as it arrived, outside the path
// domain (checkRequestPaths), before it is decoded and before any state is
// opened.
func CheckBootLinePaths(line []byte) error {
	return checkRequestPaths("the boot line", line, bootPathMembers)
}

// checkRequestPaths refuses a request, raw as it arrived, whose bytes are not
// valid UTF-8, or one of whose path members is a JSON string with an unpaired
// surrogate escape. It runs before the request is decoded and names what it
// refuses as what: "the boot line" or "the relocation request". A member is
// one of members by name, compared as encoding/json compares a member with a
// field (an escaped or case-variant spelling is the member too), and every
// occurrence of it is checked, so a duplicate cannot hide one. The escapes
// are read as written (validInputEscapes), not as decoded: the decoder
// replaces an unpaired surrogate with U+FFFD. A request that is not one JSON
// object passes here, for the caller's decoding to refuse it.
func checkRequestPaths(what string, raw []byte, members []string) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("%s is not valid UTF-8; nothing was opened or recorded (%s)", what, codeRequestNotUTF8)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil
		}
		name, _ := t.(string)
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil
		}
		for _, m := range members {
			if strings.EqualFold(name, m) && !validInputEscapes(value) {
				return fmt.Errorf("%s member %q has an unpaired surrogate escape, which is no path; nothing was opened or recorded (%s)", what, m, codePathUnpairedSurrogate)
			}
		}
	}
	return nil
}

// checkPath refuses a host path outside the path domain, by the name of its
// member, without echoing it.
func checkPath(member, path string) error {
	if !utf8.ValidString(path) {
		return fmt.Errorf("%s is not valid UTF-8; Runner takes no raw-byte path (%s)", member, codePathNotUTF8)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%s contains NUL (%s)", member, codePathContainsNUL)
	}
	return nil
}

// checkPaths refuses the first of name, path pairs outside the path domain.
func checkPaths(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := checkPath(pairs[i], pairs[i+1]); err != nil {
			return err
		}
	}
	return nil
}

// checkStoredBinding refuses a store's input root binding outside the path
// domain, which only a direct Go host could have stored: by name, with its
// bytes escaped for diagnosis. Runner does not guess the path it stood for.
func checkStoredBinding(binding string) error {
	code := ""
	switch {
	case !utf8.ValidString(binding):
		code = codeStoredBindingNotUTF8
	case strings.IndexByte(binding, 0) >= 0:
		code = codeStoredBindingContainsNUL
	default:
		return nil
	}
	return fmt.Errorf("the store's inputRoot binding %q is outside Runner's path domain (valid UTF-8, no NUL); Runner does not guess the path it stood for and opens or relocates nothing (%s)", binding, code)
}

// InputRootBinding is what Runner reports of the store's input root binding:
// whether it holds U+FFFD, which may be a literal character or may have
// replaced a byte lost before the path reached Runner (Runner cannot tell
// which and does not try), and whether the bound root exists as a directory.
type InputRootBinding struct {
	Root                 string
	ReplacementCharacter bool
	Exists               bool
}

// InputRootBinding is the store's input root binding as it is now. Open holds
// the stored binding equal to the configured one, so it is the configured
// root. Without an input root it is the zero value.
func (s *Service) InputRootBinding() InputRootBinding {
	b := InputRootBinding{Root: s.cfg.InputRoot}
	if b.Root == "" {
		return b
	}
	b.ReplacementCharacter = strings.ContainsRune(b.Root, utf8.RuneError)
	info, err := os.Stat(b.Root)
	b.Exists = err == nil && info.IsDir()
	return b
}

// String is the open diagnostic for a binding that holds U+FFFD, and empty for
// any other.
func (b InputRootBinding) String() string {
	if !b.ReplacementCharacter {
		return ""
	}
	exists := "the bound root exists"
	if !b.Exists {
		exists = "the bound root does not exist"
	}
	return fmt.Sprintf("the inputRoot binding %q contains U+FFFD, which may stand for a byte lost before the path reached Runner; %s", b.Root, exists)
}
