package dialect4

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
)

// The codes a reading refuses or finds with (spec/dialect.md §11).
const (
	CodeMalformed  = "input-malformed"
	CodeOverBound  = "input-over-bound"
	CodeParameter  = "parameter-invalid"
	CodeReadFailed = "read-failed"
)

// Error is a refusal or a finding: its code, and a sentence saying why.
type Error struct {
	Code   string
	Reason string
}

func (e *Error) Error() string { return e.Reason }

func failure(code, format string, args ...any) *Error {
	return &Error{Code: code, Reason: fmt.Sprintf(format, args...)}
}

func malformed(format string, args ...any) *Error {
	return failure(CodeMalformed, format, args...)
}

// CodeOf is the code err carries, or "" when it carries none.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// largest is 2^53 - 1, the largest magnitude of an integer (spec/dialect.md §1).
const largest = 1<<53 - 1

var integerLiteral = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

// IsInteger reports whether v is a number written as an integer, of
// magnitude at most 2^53 - 1 (spec/dialect.md §1).
func IsInteger(v any) bool {
	n, ok := v.(json.Number)
	if !ok || !integerLiteral.MatchString(string(n)) {
		return false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return err == nil && i <= largest && i >= -largest
}

func integerOf(v any) int64 {
	i, _ := strconv.ParseInt(string(v.(json.Number)), 10, 64)
	return i
}

var decimalGrammar = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)

// IsDecimal reports whether v is a string of Core §2.2's decimal grammar, of
// at most 128 characters (spec/dialect.md §2.2.2), in every profile.
func IsDecimal(v any) bool {
	s, ok := v.(string)
	return ok && utf8.RuneCountInString(s) <= MaxDecimalCharacters && decimalGrammar.MatchString(s)
}

// Instant is the number of seconds since 1970-01-01T00:00:00Z that a
// timestamp names (spec/dialect.md §1): YYYY-MM-DDThh:mm:ssZ, a time that
// exists.
func Instant(v any) (int64, bool) {
	s, ok := v.(string)
	if !ok || len(s) != 20 || s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' || s[19] != 'Z' {
		return 0, false
	}
	field := func(from, to int) (int, bool) {
		n := 0
		for i := from; i < to; i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
			n = n*10 + int(s[i]-'0')
		}
		return n, true
	}
	year, ok1 := field(0, 4)
	month, ok2 := field(5, 7)
	day, ok3 := field(8, 10)
	hour, ok4 := field(11, 13)
	minute, ok5 := field(14, 16)
	second, ok6 := field(17, 19)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) || month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59 {
		return 0, false
	}
	leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
	days := [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[month-1]
	if month == 2 && leap {
		days = 29
	}
	if day < 1 || day > days {
		return 0, false
	}
	return daysFromCivil(int64(year), int64(month), int64(day))*86400 + int64(hour*3600+minute*60+second), true
}

func daysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := y
	if y < 0 {
		era = y - 399
	}
	era /= 400
	yoe := y - era*400
	shift := int64(-3)
	if m <= 2 {
		shift = 9
	}
	doy := (153*(m+shift)+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// Tokens are a pointer's reference tokens, as the Runner reads every pointer
// of a dialect: an absolute pointer of at most 1024 bytes and 32 tokens, with
// no token __proto__, constructor or prototype; with root, the empty pointer
// too.
func Tokens(pointer string, root bool) ([]string, error) {
	if pointer == "" && root {
		return nil, nil
	}
	if len(pointer) > 1024 || !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("use an absolute JSON pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	if len(parts) > 32 {
		return nil, errors.New("mapping nesting exceeds limit")
	}
	for i, p := range parts {
		for at := 0; at < len(p); at++ {
			if p[at] == '~' {
				if at+1 >= len(p) || (p[at+1] != '0' && p[at+1] != '1') {
					return nil, errors.New("invalid pointer escape")
				}
				at++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
		if parts[i] == "__proto__" || parts[i] == "constructor" || parts[i] == "prototype" {
			return nil, errors.New("unsupported mapping field")
		}
	}
	return parts, nil
}

func isPointer(v any, root bool) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, err := Tokens(s, root)
	return err == nil
}

// Resolve is the value a pointer resolves to in v, and whether it resolves
// (spec/dialect.md §1).
func Resolve(v any, pointer string) (any, bool) {
	parts, err := Tokens(pointer, true)
	if err != nil {
		return nil, false
	}
	for _, key := range parts {
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = x[key]; !ok {
				return nil, false
			}
		case []any:
			n, err := strconv.Atoi(key)
			if err != nil || strconv.Itoa(n) != key || n < 0 || n >= len(x) {
				return nil, false
			}
			v = x[n]
		default:
			return nil, false
		}
	}
	return v, true
}

// Write writes value at a fact target of root, creating each object on the
// way (spec/dialect.md §8.1).
func Write(root map[string]any, pointer string, value any) {
	parts, _ := Tokens(pointer, false)
	at := root
	for _, k := range parts[:len(parts)-1] {
		next, ok := at[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			at[k] = next
		}
		at = next
	}
	at[parts[len(parts)-1]] = value
}

// Equal is JSON equality (spec/dialect.md §6.3): of type and value, numbers
// by their mathematical value.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		p, ok1 := new(big.Rat).SetString(string(x))
		q, ok2 := new(big.Rat).SetString(string(y))
		return ok1 && ok2 && p.Cmp(q) == 0
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

// Canon is the gateway canonical form of v (spec/dialect.md §1), refused for
// a value outside its domain.
func Canon(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return derivation.Canon(raw)
}

func inCanonDomain(v any) bool {
	_, err := Canon(v)
	return err == nil
}

// ReadJSON reads one JSON document strictly, as the Runner reads every
// document (spec/dialect.md §1): UTF-8 with no lone surrogate escape, no
// member named twice, nested at most maxDepth levels, numbers kept as their
// literals.
func ReadJSON(data []byte, maxDepth int) (any, error) {
	if !utf8.Valid(data) || !validEscapes(data) {
		return nil, errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		if depth > maxDepth {
			return nil, errors.New("input nesting exceeds limit")
		}
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch t {
		case json.Delim('{'):
			o := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				name, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				if _, exists := o[name]; exists {
					return nil, errors.New("duplicate object key")
				}
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				o[name] = v
			}
			_, e = d.Token()
			return o, e
		case json.Delim('['):
			a := []any{}
			for d.More() {
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, e = d.Token()
			return a, e
		}
		if _, ok := t.(json.Delim); ok {
			return nil, errors.New("invalid JSON")
		}
		return t, nil
	}
	v, err := value(0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("one JSON value required")
	}
	return v, nil
}

// validEscapes refuses a \u escape of a lone surrogate, which encoding/json
// would read as U+FFFD. It is the Runner's own check (validInputEscapes).
func validEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// Measure holds a document's bytes to the bounds spec/dialect.md §12
// measures before a document is read: its nesting, every object and every
// array one level, and each number literal's characters (at most 128) and
// exponent (the digits after its first e or E, past one sign there, of a
// value at most 308), a literal being a maximal run of the characters 0-9,
// +, -, ., e and E outside strings. It is the Runner's own measure
// (measureBytes).
func Measure(data []byte, maxDepth int) error {
	const literal = "0123456789+-.eE"
	depth, inString, escaped := 0, false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
			if depth > maxDepth {
				return failure(CodeOverBound, "a value nested more than %d levels", maxDepth)
			}
		case c == ']' || c == '}':
			depth--
		case strings.IndexByte(literal, c) >= 0:
			end := i
			for end < len(data) && strings.IndexByte(literal, data[end]) >= 0 {
				end++
			}
			if e := literalBounds(string(data[i:end])); e != nil {
				return e
			}
			i = end - 1
		}
	}
	return nil
}

func literalBounds(literal string) error {
	if len(literal) > 128 {
		return failure(CodeOverBound, "a number literal of more than 128 characters")
	}
	at := strings.IndexAny(literal, "eE")
	if at < 0 {
		return nil
	}
	rest := literal[at+1:]
	if rest != "" && (rest[0] == '+' || rest[0] == '-') {
		rest = rest[1:]
	}
	digits := rest[:len(rest)-len(strings.TrimLeft(rest, "0123456789"))]
	trimmed := strings.TrimLeft(digits, "0")
	if len(trimmed) > 3 || len(trimmed) == 3 && trimmed > "308" {
		return failure(CodeOverBound, "a number literal whose exponent is of a magnitude over 308")
	}
	return nil
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
