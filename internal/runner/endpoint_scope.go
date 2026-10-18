package runner

// Endpoint scopes (spec/dialect.md §5.1, "An endpoint constraint", "The
// endpoint form for a constrained page", the wildcard and "Matching"; §5.2,
// check 3; §11; §12; RFC 0018 round 15, judgment-pack-spec#207; runner #141;
// round 16, judgment-pack-spec#215, the path's traversal spellings; runner
// #152).
//
// An input profile may carry endpointScope in place of one exact endpoint, so
// that one reviewed page profile verifies reads of more than one URL. Every
// path that takes profiles holds the member to one form (endpointScopeForm),
// and every receipt verified under such a profile is held to one matcher
// (endpointInScope): a run's preparation, an import, verify-run and the
// offline verify-preparation alike. The form and the comparisons are written
// from the text, as the specification's readers write them: no URL library
// parses, decodes or normalizes either the profile's entries or a receipt's
// endpoint, and neither is rewritten.
//
// Runner sends no page request of its own. A research source's pages are
// read by its caller, who checks each requested URL against the scope before
// sending it (§5.1, "Admission before acquisition"); the one source Runner
// acquires itself, an operation, cannot be bound to a scoped profile
// (profileFor), so its plan refuses one before any source is called. A
// receipt outside the scope that a caller retains anyway is refused here, at
// check 3, acquisition-invalid.

import (
	"encoding/json"
	"errors"
)

// The bounds of §12 on an endpoint scope: entries per profile, and UTF-8
// bytes per entry after JSON decoding. They are counted before any form
// (§11's third phase), on a profile of any shape.
const (
	maxScopeEntries    = 32
	maxScopeEntryBytes = 2048
)

// scopeWildcard is the one entry that is not a prefix: it admits every
// endpoint in the constrained page form, and only a generated profile may
// carry it (§5.1).
const scopeWildcard = "https://*"

// EndpointScope is a profile's endpointScope: its entries, in order, as
// written. A member written null is refused when it is read, so that a
// profile carrying it is never taken for one without it; an empty array is
// read as given, and refused by its form (endpointScopeForm). Absent, the
// profile encodes as before, and its digest is unchanged.
type EndpointScope []string

// UnmarshalJSON reads the member as given: an array of strings, never null.
func (s *EndpointScope) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return errors.New("endpointScope is an array of endpoint prefixes, never null")
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	if entries == nil {
		entries = []string{}
	}
	*s = entries
	return nil
}

// endpointScopeOver says whether a profile's endpointScope, as read, is over
// §12's bounds: more than 32 entries, or an entry that is a string of more
// than 2048 UTF-8 bytes. Whatever else the member or its profile is, the
// count is taken wherever it can be (§11's third phase).
func endpointScopeOver(scope any) bool {
	entries, ok := scope.([]any)
	if !ok {
		return false
	}
	if len(entries) > maxScopeEntries {
		return true
	}
	for _, entry := range entries {
		if text, ok := entry.(string); ok && len(text) > maxScopeEntryBytes {
			return true
		}
	}
	return false
}

// scopeHolder is what §5.1's conditions on endpointScope read of the profile
// that carries it: its class, shape and adapter's name, whether its endpoint
// is null, and the capabilities its research declaration names, declared
// saying whether it has one.
type scopeHolder struct {
	class, shape, adapter string
	endpointNull          bool
	declared              bool
	capabilities          []string
}

// endpointScopeForm holds a profile's endpointScope to §5.1, every
// condition input-malformed: the member only on a profile of shape http
// whose adapter is adapter-web, with endpoint null; a research declaration,
// when present, of exactly the capability page, and one on every generated
// profile; one to 32 distinct entries, each a prefix in the constrained page
// form or exactly the wildcard; and the wildcard on no record profile.
// Distinct is unequal as strings. Its bounds are counted before it
// (endpointScopeOver). Each path names the profile, and codes the refusal,
// as it refuses any other form of a profile.
func endpointScopeForm(h scopeHolder, entries []string) error {
	malformed := errors.New
	pageOnly := len(h.capabilities) == 1 && h.capabilities[0] == "page"
	switch {
	case !h.endpointNull:
		return malformed("endpointScope is carried with endpoint null: exactly one endpoint rule applies")
	case h.shape != "http" || h.adapter != "adapter-web":
		return malformed("endpointScope is admitted only on a profile of shape http whose adapter is adapter-web")
	case h.declared && !pageOnly:
		return malformed("a profile carrying endpointScope declares, if anything, the research capability page alone")
	case h.class == classGenerated && !h.declared:
		return malformed("a generated profile carrying endpointScope declares the research capability page")
	case len(entries) == 0:
		return malformed("endpointScope has one entry at least")
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry] {
			return malformed("endpointScope names an entry twice")
		}
		seen[entry] = true
		if entry == scopeWildcard {
			if h.class != classGenerated {
				return malformed("the endpoint wildcard https://* is admitted only on a generated profile")
			}
			continue
		}
		if _, _, ok := pageEndpoint(entry); !ok {
			return malformed("an endpointScope entry is not an endpoint prefix in the constrained page form, nor the wildcard https://*")
		}
	}
	return nil
}

// pageEndpoint reads a string in the constrained page form (§5.1): at most
// 2048 characters, each from U+0021 to U+007E; exactly https://, then a host,
// then nothing, :443 or : followed by nothing, then nothing or a path that
// begins with /. The host is two or more dot-separated labels, each of 1 to
// 63 ASCII letters, digits and hyphens, starting and ending with a letter or
// a digit, the last starting with a letter; so no user, IP literal, trailing
// dot, underscore or wildcard. The path has no ?, # or backslash; each % is
// followed by two hexadecimal digits; it has no escaped slash or backslash;
// and no segment between literal slashes is a dot segment, alone or before
// its first ; or escaped delimiter (pathForm). Nothing is decoded. It returns
// the host with ASCII letters folded to lower case, for comparison only, and
// the path as written.
func pageEndpoint(s string) (host, path string, ok bool) {
	if len(s) > maxScopeEntryBytes {
		return "", "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", "", false
		}
	}
	const scheme = "https://"
	if len(s) < len(scheme) || s[:len(scheme)] != scheme {
		return "", "", false
	}
	rest := s[len(scheme):]
	authority := rest
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			authority, path = rest[:i], rest[i:]
			break
		}
	}
	host = authority
	for i := 0; i < len(authority); i++ {
		if authority[i] == ':' {
			host = authority[:i]
			if port := authority[i+1:]; port != "" && port != "443" {
				return "", "", false
			}
			break
		}
	}
	if !dnsName(host) || !pathForm(path) {
		return "", "", false
	}
	return asciiLower(host), path, true
}

// dnsName is §5.1's host: two or more labels separated by dots, each of 1 to
// 63 characters, ASCII letters, digits and hyphens, starting and ending with
// a letter or a digit; the last label starts with a letter.
func dnsName(host string) bool {
	labels, start := 0, 0
	for i := 0; i <= len(host); i++ {
		if i < len(host) && host[i] != '.' {
			continue
		}
		label := host[start:i]
		if len(label) < 1 || len(label) > 63 || !letterOrDigit(label[0]) || !letterOrDigit(label[len(label)-1]) {
			return false
		}
		for j := 0; j < len(label); j++ {
			if !letterOrDigit(label[j]) && label[j] != '-' {
				return false
			}
		}
		labels++
		if i == len(host) && !letter(label[0]) {
			return false
		}
		start = i + 1
	}
	return labels >= 2
}

// pathForm is §5.1's path: empty, or beginning with /, with no ?, # or
// backslash; each % followed by two hexadecimal digits of either case; no
// escaped slash or backslash, %2f, %2F, %5c or %5C, anywhere in it (round
// 16); and, split at each literal /, no segment that is a dot segment, nor
// one whose part before its first ;, %3b, %3B, %3f, %3F, %23 or %00,
// whichever comes first, is a dot segment (round 16). A segment with none of
// those is its own part before them, so one test holds both. Every other
// escape, %2e and %2E outside a dot segment included, stays literal bytes
// and supplies no boundary, and nothing is decoded.
func pathForm(path string) bool {
	if path == "" {
		return true
	}
	if path[0] != '/' {
		return false
	}
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '?', '#', '\\':
			return false
		case '%':
			if i+2 >= len(path) || !hexDigit(path[i+1]) || !hexDigit(path[i+2]) {
				return false
			}
			if escapeIs(path[i+1:i+3], "2f", "5c") {
				return false
			}
		}
	}
	start := 0
	for i := 0; i <= len(path); i++ {
		if i < len(path) && path[i] != '/' {
			continue
		}
		if dotSegment(segmentHead(path[start:i])) {
			return false
		}
		start = i + 1
	}
	return true
}

// segmentHead is a segment's part before its first ;, %3b, %3B, %3f, %3F,
// %23 or %00, whichever comes first, or the whole segment when it has none.
// Each % in a path of the form begins an escape of two hexadecimal digits, so
// the escapes are read where each % stands.
func segmentHead(segment string) string {
	for i := 0; i < len(segment); i++ {
		switch {
		case segment[i] == ';':
			return segment[:i]
		case segment[i] == '%' && i+2 < len(segment) && escapeIs(segment[i+1:i+3], "3b", "3f", "23", "00"):
			return segment[:i]
		}
	}
	return segment
}

// escapeIs says whether the two hexadecimal digits after a % are one of
// escapes, ASCII letters compared in either case: %2f and %2F alike.
func escapeIs(digits string, escapes ...string) bool {
	folded := asciiLower(digits)
	for _, escape := range escapes {
		if folded == escape {
			return true
		}
	}
	return false
}

// dotSegment says whether a segment, or a segment's part, is one or two
// tokens, each ., %2e or %2E: a dot segment, literal or percent-spelled.
func dotSegment(segment string) bool {
	tokens := 0
	for segment != "" {
		switch {
		case segment[0] == '.':
			segment = segment[1:]
		case len(segment) >= 3 && segment[:2] == "%2" && (segment[2] == 'e' || segment[2] == 'E'):
			segment = segment[3:]
		default:
			return false
		}
		tokens++
	}
	return tokens == 1 || tokens == 2
}

// endpointInScope says whether a receipt's endpoint is admitted by a scope
// (§5.1, "Matching"): the endpoint in the constrained page form, and one
// entry admitting it. The wildcard admits every endpoint in that form. A
// prefix admits it when the hosts are equal, ASCII A to Z compared as a to z,
// every spelling of the port being 443; and when the prefix's path is absent
// or /, or the endpoint's path equals it, or starts with it followed by /,
// or, the prefix's path ending in /, starts with it. Paths are compared byte
// for byte, case and percent spelling included, with no decoding, slash
// collapse or other normalization, and no subdomain matches. The scope's
// entries were held to their form when the profile was admitted.
func endpointInScope(endpoint string, scope []string) bool {
	host, path, ok := pageEndpoint(endpoint)
	if !ok {
		return false
	}
	for _, entry := range scope {
		if entry == scopeWildcard {
			return true
		}
		prefixHost, prefix, ok := pageEndpoint(entry)
		if !ok || prefixHost != host {
			continue
		}
		if prefix == "" || prefix == "/" || path == prefix {
			return true
		}
		boundary := prefix
		if prefix[len(prefix)-1] != '/' {
			boundary += "/"
		}
		if len(path) >= len(boundary) && path[:len(boundary)] == boundary {
			return true
		}
	}
	return false
}

func letter(c byte) bool        { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func letterOrDigit(c byte) bool { return letter(c) || c >= '0' && c <= '9' }
func hexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// asciiLower folds the ASCII letters A to Z to a to z, and nothing else.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// scopeEntries is a scope's entries as a document holds them, for the count
// taken before any form (endpointScopeOver).
func scopeEntries(scope EndpointScope) []any {
	entries := make([]any, len(scope))
	for i, entry := range scope {
		entries[i] = entry
	}
	return entries
}

// installedScopeForm holds an installation's profile's endpointScope, when it
// carries one, to §5.1's form (endpointScopeForm), input-malformed.
func installedScopeForm(p InputProfile) error {
	if p.EndpointScope == nil {
		return nil
	}
	h := scopeHolder{class: p.Class, shape: p.Shape, adapter: p.Adapter.Name, endpointNull: p.Endpoint == nil}
	if p.Research != nil {
		h.declared, h.capabilities = true, p.Research.Capabilities
	}
	if err := endpointScopeForm(h, p.EndpointScope); err != nil {
		return researchFail(codeMalformed, "the input profile %s: %s", p.ID, err)
	}
	return nil
}
