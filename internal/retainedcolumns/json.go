package retainedcolumns

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

func sha256Bytes(raw []byte) [32]byte { return sha256.Sum256(raw) }

// No JSON number is decoded through float64. Strict strings are checked before
// encoding/json can replace an unpaired escaped surrogate with U+FFFD.
func parseJSON(raw []byte, depth int, folded, strict bool) (any, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) || strict && !scalarEscapes(raw) {
		return nil, refuse("input-malformed")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(level int) (any, error) {
		if level > depth {
			return nil, refuse("input-over-bound")
		}
		tok, err := d.Token()
		if err != nil {
			return nil, refuse("input-malformed")
		}
		delim, container := tok.(json.Delim)
		if !container {
			return tok, nil
		}
		switch delim {
		case '{':
			m := map[string]any{}
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, refuse("input-malformed")
				}
				name, ok := k.(string)
				if !ok {
					return nil, refuse("input-malformed")
				}
				identity := name
				if folded {
					// RFC 0029 pins Unicode 15.0 simple lowercase, not full folding.
					if unicode.Version != "15.0.0" {
						return nil, refuse("input-malformed")
					}
					identity = strings.Map(unicode.ToLower, name)
				}
				if seen[identity] {
					return nil, refuse("input-malformed")
				}
				seen[identity] = true
				v, e := read(level + 1)
				if e != nil {
					return nil, e
				}
				m[name] = v
			}
			if end, e := d.Token(); e != nil || end != json.Delim('}') {
				return nil, refuse("input-malformed")
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				v, e := read(level + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			if end, e := d.Token(); e != nil || end != json.Delim(']') {
				return nil, refuse("input-malformed")
			}
			return a, nil
		}
		return nil, refuse("input-malformed")
	}
	v, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, refuse("input-malformed")
	}
	return v, nil
}

func hex4(b []byte) int {
	if len(b) < 4 {
		return -1
	}
	n := 0
	for _, c := range b[:4] {
		n *= 16
		switch {
		case c >= '0' && c <= '9':
			n += int(c - '0')
		case c >= 'a' && c <= 'f':
			n += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int(c-'A') + 10
		default:
			return -1
		}
	}
	return n
}
func scalarEscapes(b []byte) bool {
	quoted := false
	for i := 0; i < len(b); i++ {
		if b[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || b[i] != '\\' {
			continue
		}
		i++
		if i >= len(b) {
			return false
		}
		if b[i] != 'u' {
			continue
		}
		code := hex4(b[i+1:])
		if code < 0 {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return false
			}
			low := hex4(b[i+3:])
			if low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// compactString follows the Draft's fixed ordinary-column representation:
// literal UTF-8, C0/quote/backslash escapes, no HTML/solidus/U+2028 escaping.
func compactString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	const hex = "0123456789abcdef"
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 32 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&15])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
