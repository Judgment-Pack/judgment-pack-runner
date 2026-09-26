// Command derive is a clean-room second implementation of the portable
// derivation rule external contract ("Portable derivation rule — external
// contract"), written in Go from the specification text alone.
//
// Agreement interface: read exactly one JSON object
//
//	{"rule": <rule document>, "artifact": <artifact value>, "params": {...}}
//
// from standard input. If the rule and artifact derive a claim, write the
// derived claim's canonical bytes to standard output with no trailing newline
// and exit 0. If the rule or artifact is rejected (spec "Errors"), write
// nothing to standard output and exit nonzero.
//
// Standard library only.
package derivation

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// JSON value model
//
// A hand-written parser is used instead of encoding/json so that:
//   - numbers keep their exact source token (integer-vs-float distinction and
//     arbitrary magnitude survive; encoding/json's default float64 decoding
//     would destroy both),
//   - lone surrogate escapes are rejected rather than silently folded to
//     U+FFFD (canon admits only strings of Unicode scalar values),
//   - "absent" is a first-class value distinct from JSON null.
// ---------------------------------------------------------------------------

// Value is one of: nullT, absentT, bool, string, Num, Arr, Obj.
type Value interface{}

type nullT struct{}
type absentT struct{}

var jnull = nullT{}
var absent = absentT{}

// Num holds a JSON number as its exact source token.
type Num struct{ Raw string }

// Obj and Arr are the container shapes.
type Obj = map[string]Value
type Arr = []Value

const maxCanonInt = int64(9007199254740991) // 2^53 - 1

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type parser struct {
	b []byte
	i int
}

func (p *parser) errf(format string, args ...interface{}) error {
	return fmt.Errorf("json: "+format+" (offset %d)", append(args, p.i)...)
}

func (p *parser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) parseValue() (Value, error) {
	p.ws()
	if p.i >= len(p.b) {
		return nil, p.errf("unexpected end of input")
	}
	c := p.b[p.i]
	switch {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		return p.parseString()
	case c == 't':
		return p.lit("true", Value(true))
	case c == 'f':
		return p.lit("false", Value(false))
	case c == 'n':
		return p.lit("null", Value(jnull))
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	}
	return nil, p.errf("unexpected character %q", string(rune(c)))
}

func (p *parser) lit(word string, v Value) (Value, error) {
	if p.i+len(word) > len(p.b) || string(p.b[p.i:p.i+len(word)]) != word {
		return nil, p.errf("invalid literal")
	}
	p.i += len(word)
	return v, nil
}

func (p *parser) parseObject() (Value, error) {
	p.i++ // '{'
	o := Obj{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return o, nil
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, p.errf("expected object member name")
		}
		k, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, p.errf("expected ':'")
		}
		p.i++
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		// Duplicate member names: last occurrence wins.
		o[k.(string)] = v
		p.ws()
		if p.i >= len(p.b) {
			return nil, p.errf("unexpected end of input in object")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
			continue
		case '}':
			p.i++
			return o, nil
		default:
			return nil, p.errf("expected ',' or '}'")
		}
	}
}

func (p *parser) parseArray() (Value, error) {
	p.i++ // '['
	a := Arr{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return a, nil
	}
	for {
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		p.ws()
		if p.i >= len(p.b) {
			return nil, p.errf("unexpected end of input in array")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
			continue
		case ']':
			p.i++
			return a, nil
		default:
			return nil, p.errf("expected ',' or ']'")
		}
	}
}

func (p *parser) parseNumber() (Value, error) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	// int part
	if p.i >= len(p.b) {
		return nil, p.errf("truncated number")
	}
	if p.b[p.i] == '0' {
		p.i++
	} else if p.b[p.i] >= '1' && p.b[p.i] <= '9' {
		for p.i < len(p.b) && isDigit(p.b[p.i]) {
			p.i++
		}
	} else {
		return nil, p.errf("invalid number")
	}
	// frac
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if p.i >= len(p.b) || !isDigit(p.b[p.i]) {
			return nil, p.errf("invalid number fraction")
		}
		for p.i < len(p.b) && isDigit(p.b[p.i]) {
			p.i++
		}
	}
	// exp
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if p.i >= len(p.b) || !isDigit(p.b[p.i]) {
			return nil, p.errf("invalid number exponent")
		}
		for p.i < len(p.b) && isDigit(p.b[p.i]) {
			p.i++
		}
	}
	return Num{Raw: string(p.b[start:p.i])}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (p *parser) parseString() (Value, error) {
	p.i++ // '"'
	var sb strings.Builder
	for {
		if p.i >= len(p.b) {
			return nil, p.errf("unterminated string")
		}
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return sb.String(), nil
		case c == '\\':
			p.i++
			if p.i >= len(p.b) {
				return nil, p.errf("truncated escape")
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte(0x08)
			case 'f':
				sb.WriteByte(0x0C)
			case 'n':
				sb.WriteByte(0x0A)
			case 'r':
				sb.WriteByte(0x0D)
			case 't':
				sb.WriteByte(0x09)
			case 'u':
				r, err := p.hex4()
				if err != nil {
					return nil, err
				}
				if r >= 0xD800 && r <= 0xDBFF {
					// high surrogate: a low surrogate escape must follow
					if p.i+1 < len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
						save := p.i
						p.i += 2
						r2, err := p.hex4()
						if err != nil {
							return nil, err
						}
						if r2 >= 0xDC00 && r2 <= 0xDFFF {
							sb.WriteRune(rune(0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)))
							break
						}
						p.i = save
					}
					return nil, p.errf("lone high surrogate escape")
				}
				if r >= 0xDC00 && r <= 0xDFFF {
					return nil, p.errf("lone low surrogate escape")
				}
				sb.WriteRune(rune(r))
			default:
				return nil, p.errf("invalid escape")
			}
		case c < 0x20:
			return nil, p.errf("unescaped control character in string")
		case c < 0x80:
			sb.WriteByte(c)
			p.i++
		default:
			r, size := utf8.DecodeRune(p.b[p.i:])
			if r == utf8.RuneError && size <= 1 {
				return nil, p.errf("invalid UTF-8 in string")
			}
			sb.Write(p.b[p.i : p.i+size])
			p.i += size
		}
	}
}

func (p *parser) hex4() (int, error) {
	if p.i+4 > len(p.b) {
		return 0, p.errf("truncated \\u escape")
	}
	v := 0
	for k := 0; k < 4; k++ {
		c := p.b[p.i+k]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, p.errf("invalid hex digit in \\u escape")
		}
		v = v*16 + d
	}
	p.i += 4
	return v, nil
}

// parseJSON parses exactly one JSON value; only trailing whitespace is allowed.
func parseJSON(b []byte) (Value, error) {
	p := &parser{b: b}
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, p.errf("trailing content after JSON value")
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Canonicalization ("canon")
//
// Object members ordered by ascending Unicode code point (for well-formed
// UTF-8 this is exactly the UTF-8 byte order Go's string < gives), compact
// separators "," and ":", raw UTF-8, and a value domain restricted to objects,
// arrays, strings of Unicode scalar values, booleans, null, and integers in
// -(2^53-1)..2^53-1.
// ---------------------------------------------------------------------------

func canon(v Value) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanon(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanon(buf *bytes.Buffer, v Value) error {
	switch t := v.(type) {
	case nullT:
		buf.WriteString("null")
		return nil
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		return writeCanonString(buf, t)
	case Num:
		n, err := canonInt(t)
		if err != nil {
			return err
		}
		buf.WriteString(strconv.FormatInt(n, 10))
		return nil
	case Arr:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanon(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case Obj:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // byte order == code point order for valid UTF-8
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanon(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case absentT:
		return fmt.Errorf("canon: absent is not a JSON value")
	default:
		return fmt.Errorf("canon: unsupported value of type %T", v)
	}
}

// canonInt reports the integer value of a number token, rejecting any token
// that is not written as an integer (contains '.', 'e' or 'E') and any integer
// outside -(2^53-1)..2^53-1.
func canonInt(n Num) (int64, error) {
	if strings.ContainsAny(n.Raw, ".eE") {
		return 0, fmt.Errorf("canon: non-integer number %s", n.Raw)
	}
	v, err := strconv.ParseInt(n.Raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("canon: number %s out of integer range", n.Raw)
	}
	if v > maxCanonInt || v < -maxCanonInt {
		return 0, fmt.Errorf("canon: integer %s outside -(2^53-1)..2^53-1", n.Raw)
	}
	return v, nil
}

// writeCanonString emits a JSON string: raw UTF-8, with escapes only for '"',
// '\\' and U+0000..U+001F (the five named short escapes, \u00xx lowercase hex
// otherwise).
func writeCanonString(buf *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("canon: string is not valid UTF-8")
	}
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			switch {
			case c == '"':
				buf.WriteString(`\"`)
			case c == '\\':
				buf.WriteString(`\\`)
			case c == 0x08:
				buf.WriteString(`\b`)
			case c == 0x09:
				buf.WriteString(`\t`)
			case c == 0x0A:
				buf.WriteString(`\n`)
			case c == 0x0C:
				buf.WriteString(`\f`)
			case c == 0x0D:
				buf.WriteString(`\r`)
			case c < 0x20:
				buf.WriteString(`\u00`)
				const hexdig = "0123456789abcdef"
				buf.WriteByte(hexdig[(c>>4)&0xF])
				buf.WriteByte(hexdig[c&0xF])
			default:
				buf.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return fmt.Errorf("canon: string is not valid UTF-8")
		}
		if r >= 0xD800 && r <= 0xDFFF {
			return fmt.Errorf("canon: string contains a surrogate code point")
		}
		buf.WriteString(s[i : i+size])
		i += size
	}
	buf.WriteByte('"')
	return nil
}

// checkCanonDomain verifies a whole value tree lies inside the canon domain
// without producing bytes.
func checkCanonDomain(v Value) error {
	switch t := v.(type) {
	case nullT, bool:
		return nil
	case string:
		var discard bytes.Buffer
		return writeCanonString(&discard, t)
	case Num:
		_, err := canonInt(t)
		return err
	case Arr:
		for _, e := range t {
			if err := checkCanonDomain(e); err != nil {
				return err
			}
		}
		return nil
	case Obj:
		for k, e := range t {
			var discard bytes.Buffer
			if err := writeCanonString(&discard, k); err != nil {
				return err
			}
			if err := checkCanonDomain(e); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("canon: unsupported value of type %T", v)
	}
}

// ---------------------------------------------------------------------------
// RFC 6901 JSON Pointers
// ---------------------------------------------------------------------------

func parsePointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if p[0] != '/' {
		return nil, fmt.Errorf("pointer %q does not start with '/'", p)
	}
	raw := strings.Split(p[1:], "/")
	toks := make([]string, len(raw))
	for i, t := range raw {
		u, err := unescapeToken(t)
		if err != nil {
			return nil, err
		}
		toks[i] = u
	}
	return toks, nil
}

func unescapeToken(t string) (string, error) {
	if !strings.Contains(t, "~") {
		return t, nil
	}
	var sb strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] != '~' {
			sb.WriteByte(t[i])
			continue
		}
		if i+1 >= len(t) {
			return "", fmt.Errorf("pointer token has dangling '~'")
		}
		switch t[i+1] {
		case '0':
			sb.WriteByte('~')
		case '1':
			sb.WriteByte('/')
		default:
			return "", fmt.Errorf("pointer token has invalid '~' escape")
		}
		i++
	}
	return sb.String(), nil
}

// arrayIndex accepts only the RFC 6901 index production: "0" | [1-9][0-9]*.
func arrayIndex(tok string) (int, bool) {
	if tok == "" {
		return 0, false
	}
	if tok == "0" {
		return 0, true
	}
	if tok[0] < '1' || tok[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(tok); i++ {
		if !isDigit(tok[i]) {
			return 0, false
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, false
	}
	return n, true
}

// get resolves a pointer against a document, yielding a JSON value or absent.
func get(doc Value, pointer string) Value {
	toks, err := parsePointer(pointer)
	if err != nil {
		return absent
	}
	cur := doc
	for _, t := range toks {
		switch c := cur.(type) {
		case Obj:
			v, ok := c[t]
			if !ok {
				return absent
			}
			cur = v
		case Arr:
			idx, ok := arrayIndex(t)
			if !ok || idx >= len(c) {
				return absent
			}
			cur = c[idx]
		default:
			// descends into a non-container (including absent)
			return absent
		}
	}
	return cur
}

// setPointer writes val at pointer in the facts document, creating
// intermediate objects along the path.
func setPointer(root Obj, pointer string, val Value) (Value, error) {
	toks, err := parsePointer(pointer)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		// The whole facts document is the copied value.
		return val, nil
	}
	cur := root
	for i := 0; i < len(toks)-1; i++ {
		nxt, ok := cur[toks[i]]
		if !ok {
			m := Obj{}
			cur[toks[i]] = m
			cur = m
			continue
		}
		m, ok := nxt.(Obj)
		if !ok {
			return nil, fmt.Errorf("fact pointer %q traverses a non-object", pointer)
		}
		cur = m
	}
	cur[toks[len(toks)-1]] = val
	return root, nil
}

// ---------------------------------------------------------------------------
// JSON equality
// ---------------------------------------------------------------------------

// jsonEquals compares by JSON type and value. Numbers compare by mathematical
// value (so 1 and 1.0 are equal); objects compare as unordered member sets; a
// comparison where either side is absent is false.
func jsonEquals(a, b Value) bool {
	if _, ok := a.(absentT); ok {
		return false
	}
	if _, ok := b.(absentT); ok {
		return false
	}
	switch x := a.(type) {
	case nullT:
		_, ok := b.(nullT)
		return ok
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case Num:
		y, ok := b.(Num)
		if !ok {
			return false
		}
		return numEquals(x, y)
	case Arr:
		y, ok := b.(Arr)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEquals(x[i], y[i]) {
				return false
			}
		}
		return true
	case Obj:
		y, ok := b.(Obj)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonEquals(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

func numEquals(a, b Num) bool {
	if a.Raw == b.Raw {
		return true
	}
	ra, ok1 := new(big.Rat).SetString(a.Raw)
	rb, ok2 := new(big.Rat).SetString(b.Raw)
	if !ok1 || !ok2 {
		return false
	}
	return ra.Cmp(rb) == 0
}

// ---------------------------------------------------------------------------
// Timestamps
//
// Exact form YYYY-MM-DDThh:mm:ssZ; instant is seconds since
// 1970-01-01T00:00:00Z in the proleptic Gregorian calendar.
// ---------------------------------------------------------------------------

func instant(v Value) (int64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	if len(s) != 20 {
		return 0, false
	}
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' || s[19] != 'Z' {
		return 0, false
	}
	digitRuns := [][2]int{{0, 4}, {5, 7}, {8, 10}, {11, 13}, {14, 16}, {17, 19}}
	for _, r := range digitRuns {
		for i := r[0]; i < r[1]; i++ {
			if !isDigit(s[i]) {
				return 0, false
			}
		}
	}
	atoi := func(lo, hi int) int {
		n := 0
		for i := lo; i < hi; i++ {
			n = n*10 + int(s[i]-'0')
		}
		return n
	}
	y := atoi(0, 4)
	mo := atoi(5, 7)
	d := atoi(8, 10)
	h := atoi(11, 13)
	mi := atoi(14, 16)
	sec := atoi(17, 19)
	if mo < 1 || mo > 12 {
		return 0, false
	}
	if d < 1 || d > daysInMonth(y, mo) {
		return 0, false
	}
	if h > 23 || mi > 59 || sec > 59 {
		return 0, false
	}
	return daysFromCivil(int64(y), mo, d)*86400 + int64(h)*3600 + int64(mi)*60 + int64(sec), true
}

func isLeap(y int) bool { return (y%4 == 0 && y%100 != 0) || y%400 == 0 }

func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if isLeap(y) {
			return 29
		}
		return 28
	}
	return 0
}

// daysFromCivil is the standard proleptic-Gregorian days-from-civil algorithm.
func daysFromCivil(y int64, m, d int) int64 {
	if m <= 2 {
		y--
	}
	var era int64
	if y >= 0 {
		era = y / 400
	} else {
		era = (y - 399) / 400
	}
	yoe := y - era*400 // [0, 399]
	var mp int64
	if m > 2 {
		mp = int64(m) - 3
	} else {
		mp = int64(m) + 9
	}
	doy := (153*mp+2)/5 + int64(d) - 1     // [0, 365]
	doe := yoe*365 + yoe/4 - yoe/100 + doy // [0, 146096]
	return era*146097 + doe - 719468
}

// ---------------------------------------------------------------------------
// Rule validation (spec "Errors")
// ---------------------------------------------------------------------------

var paramTypes = map[string]bool{"string": true, "integer": true, "timestamp": true}
var evidenceStates = map[string]bool{"present": true, "absent": true, "unknown": true}
var acquisitionStates = map[string]bool{"resolved": true, "absent": true, "unknown": true}

type rule struct {
	params  map[string]string
	clauses Arr
}

func validateRule(doc Value) (*rule, error) {
	// Every value carried by the rule document must lie in the canon domain.
	if err := checkCanonDomain(doc); err != nil {
		return nil, fmt.Errorf("rule: %v", err)
	}
	o, ok := doc.(Obj)
	if !ok {
		return nil, fmt.Errorf("rule: not a JSON object")
	}
	rv, ok := o["ruleVersion"]
	if !ok {
		return nil, fmt.Errorf("rule: missing ruleVersion")
	}
	if s, ok := rv.(string); !ok || s != "1" {
		return nil, fmt.Errorf("rule: ruleVersion is not \"1\"")
	}
	r := &rule{params: map[string]string{}}
	if pv, ok := o["parameters"]; ok {
		po, ok := pv.(Obj)
		if !ok {
			return nil, fmt.Errorf("rule: parameters is not an object")
		}
		for name, tv := range po {
			ts, ok := tv.(string)
			if !ok || !paramTypes[ts] {
				return nil, fmt.Errorf("rule: parameter %q has unknown type", name)
			}
			r.params[name] = ts
		}
	}
	cv, ok := o["clauses"]
	if !ok {
		return nil, fmt.Errorf("rule: missing clauses")
	}
	clauses, ok := cv.(Arr)
	if !ok {
		return nil, fmt.Errorf("rule: clauses is not an array")
	}
	if len(clauses) == 0 {
		return nil, fmt.Errorf("rule: clauses is empty")
	}
	for i, c := range clauses {
		co, ok := c.(Obj)
		if !ok {
			return nil, fmt.Errorf("rule: clause %d is not an object", i)
		}
		when, ok := co["when"]
		if !ok {
			return nil, fmt.Errorf("rule: clause %d has no when", i)
		}
		if err := validateCondition(when); err != nil {
			return nil, fmt.Errorf("rule: clause %d: %v", i, err)
		}
		if reason, ok := co["reason"]; !ok {
			return nil, fmt.Errorf("rule: clause %d has no reason", i)
		} else if _, ok := reason.(string); !ok {
			return nil, fmt.Errorf("rule: clause %d reason is not a string", i)
		}
		claim, ok := co["claim"]
		if !ok {
			return nil, fmt.Errorf("rule: clause %d has no claim", i)
		}
		if err := validateClaim(claim); err != nil {
			return nil, fmt.Errorf("rule: clause %d: %v", i, err)
		}
	}
	// The last clause must be the total one.
	last, _ := clauses[len(clauses)-1].(Obj)
	lastWhen, _ := last["when"].(Obj)
	if op, _ := lastWhen["op"].(string); op != "always" {
		return nil, fmt.Errorf("rule: final clause's when is not {\"op\":\"always\"}")
	}
	r.clauses = clauses
	return r, nil
}

func validateCondition(c Value) error {
	o, ok := c.(Obj)
	if !ok {
		return fmt.Errorf("condition is not an object")
	}
	op, ok := o["op"].(string)
	if !ok {
		return fmt.Errorf("condition has no string op")
	}
	needField := func() error {
		if _, ok := o["field"].(string); !ok {
			return fmt.Errorf("op %q needs a string field", op)
		}
		return nil
	}
	switch op {
	case "always":
		return nil
	case "exists", "isTrue", "isDecimalString":
		return needField()
	case "equals":
		if err := needField(); err != nil {
			return err
		}
		if _, ok := o["to"]; !ok {
			return fmt.Errorf("op equals needs to")
		}
		return nil
	case "equalsParam":
		if err := needField(); err != nil {
			return err
		}
		if _, ok := o["param"].(string); !ok {
			return fmt.Errorf("op equalsParam needs a string param")
		}
		return nil
	case "freshWithin":
		if err := needField(); err != nil {
			return err
		}
		if _, ok := o["asOf"].(string); !ok {
			return fmt.Errorf("op freshWithin needs a string asOf")
		}
		if _, ok := o["maxAge"].(string); !ok {
			return fmt.Errorf("op freshWithin needs a string maxAge")
		}
		return nil
	case "all", "any":
		of, ok := o["of"].(Arr)
		if !ok {
			return fmt.Errorf("op %q needs an array of", op)
		}
		for _, sub := range of {
			if err := validateCondition(sub); err != nil {
				return err
			}
		}
		return nil
	case "not":
		of, ok := o["of"]
		if !ok {
			return fmt.Errorf("op not needs of")
		}
		return validateCondition(of)
	}
	return fmt.Errorf("unknown op %q", op)
}

func validateClaim(c Value) error {
	o, ok := c.(Obj)
	if !ok {
		return fmt.Errorf("claim is not an object")
	}
	fv, ok := o["facts"]
	if !ok {
		return fmt.Errorf("claim has no facts")
	}
	facts, ok := fv.(Arr)
	if !ok {
		return fmt.Errorf("claim facts is not an array")
	}
	for _, f := range facts {
		fo, ok := f.(Obj)
		if !ok {
			return fmt.Errorf("claim facts entry is not an object")
		}
		ptr, ok := fo["pointer"].(string)
		if !ok {
			return fmt.Errorf("claim facts entry has no string pointer")
		}
		if _, err := parsePointer(ptr); err != nil {
			return fmt.Errorf("claim facts entry: %v", err)
		}
		from, ok := fo["from"].(string)
		if !ok {
			return fmt.Errorf("claim facts entry has no string from")
		}
		if _, err := parsePointer(from); err != nil {
			return fmt.Errorf("claim facts entry: %v", err)
		}
	}
	ev, ok := o["evidence"]
	if !ok {
		return fmt.Errorf("claim has no evidence")
	}
	evo, ok := ev.(Obj)
	if !ok {
		return fmt.Errorf("claim evidence is not an object")
	}
	for id, st := range evo {
		s, ok := st.(string)
		if !ok || !evidenceStates[s] {
			return fmt.Errorf("claim evidence %q has an invalid availability", id)
		}
	}
	st, ok := o["acquisitionStatus"]
	if !ok {
		return fmt.Errorf("claim has no acquisitionStatus")
	}
	s, ok := st.(string)
	if !ok || !acquisitionStates[s] {
		return fmt.Errorf("claim acquisitionStatus is invalid")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

type evalCtx struct {
	artifact Value
	params   Obj
	basis    map[string]bool
}

// read resolves an artifact pointer and records it in the cumulative basis:
// a leaf op that is actually evaluated reads its field, whether or not the
// field is present.
func (c *evalCtx) read(pointer string) Value {
	c.basis[pointer] = true
	return get(c.artifact, pointer)
}

func (c *evalCtx) param(name string) Value {
	if v, ok := c.params[name]; ok {
		return v
	}
	return absent
}

func (c *evalCtx) eval(cond Value) bool {
	o := cond.(Obj)
	op, _ := o["op"].(string)
	switch op {
	case "always":
		return true
	case "exists":
		f, _ := o["field"].(string)
		_, isAbsent := c.read(f).(absentT)
		return !isAbsent
	case "equals":
		f, _ := o["field"].(string)
		return jsonEquals(c.read(f), o["to"])
	case "equalsParam":
		f, _ := o["field"].(string)
		name, _ := o["param"].(string)
		return jsonEquals(c.read(f), c.param(name))
	case "isTrue":
		f, _ := o["field"].(string)
		b, ok := c.read(f).(bool)
		return ok && b
	case "isDecimalString":
		f, _ := o["field"].(string)
		s, ok := c.read(f).(string)
		return ok && isDecimalString(s)
	case "freshWithin":
		f, _ := o["field"].(string)
		fieldVal := c.read(f)
		asOfName, _ := o["asOf"].(string)
		maxAgeName, _ := o["maxAge"].(string)
		tField, ok1 := instant(fieldVal)
		tAsOf, ok2 := instant(c.param(asOfName))
		if !ok1 || !ok2 {
			return false
		}
		maxAgeVal, ok := c.param(maxAgeName).(Num)
		if !ok {
			return false
		}
		maxAge, err := canonInt(maxAgeVal)
		if err != nil {
			return false
		}
		delta := tAsOf - tField
		return delta >= 0 && delta <= maxAge
	case "all":
		of, _ := o["of"].(Arr)
		for _, sub := range of {
			if !c.eval(sub) {
				return false // short-circuit: later terms are not read
			}
		}
		return true
	case "any":
		of, _ := o["of"].(Arr)
		for _, sub := range of {
			if c.eval(sub) {
				return true // short-circuit: later terms are not read
			}
		}
		return false
	case "not":
		return !c.eval(o["of"])
	}
	// unreachable: validateRule rejects unknown ops
	return false
}

func isDecimalString(s string) bool {
	if s == "" {
		return false
	}
	if s == "0" {
		return true
	}
	if s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Derivation
// ---------------------------------------------------------------------------

func derive(ruleDoc Value, artifact Value, params Obj) ([]byte, error) {
	r, err := validateRule(ruleDoc)
	if err != nil {
		return nil, err
	}
	ctx := &evalCtx{artifact: artifact, params: params, basis: map[string]bool{}}

	matchIndex := -1
	var match Obj
	for i, c := range r.clauses {
		co := c.(Obj)
		if ctx.eval(co["when"]) {
			matchIndex = i
			match = co
			break
		}
	}
	if matchIndex < 0 {
		// The final always clause guarantees a match; a rule without one is
		// already rejected by validateRule.
		return nil, fmt.Errorf("derive: no clause matched")
	}

	claim := match["claim"].(Obj)

	// facts
	var factsDoc Value = Obj{}
	for _, f := range claim["facts"].(Arr) {
		fo := f.(Obj)
		ptr, _ := fo["pointer"].(string)
		from, _ := fo["from"].(string)
		v := get(artifact, from)
		if _, isAbsent := v.(absentT); isAbsent {
			return nil, fmt.Errorf("derive: fact source %q resolves to absent", from)
		}
		root, ok := factsDoc.(Obj)
		if !ok {
			return nil, fmt.Errorf("derive: fact pointer %q traverses a non-object", ptr)
		}
		factsDoc, err = setPointer(root, ptr, v)
		if err != nil {
			return nil, fmt.Errorf("derive: %v", err)
		}
	}

	// basis
	names := make([]string, 0, len(ctx.basis))
	for p := range ctx.basis {
		names = append(names, p)
	}
	sort.Strings(names) // ascending Unicode code point
	basis := make(Arr, len(names))
	for i, n := range names {
		basis[i] = n
	}

	derived := Obj{
		"facts":                factsDoc,
		"evidenceAvailability": claim["evidence"],
		"acquisitionStatus":    claim["acquisitionStatus"],
		"reason":               match["reason"],
		"basis":                basis,
	}
	return canon(derived)
}
