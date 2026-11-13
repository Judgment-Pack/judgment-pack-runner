package dialect4

import (
	"regexp"
	"strconv"
	"strings"
)

// Format is the dialect format this package reads, and RuleVersion the rule
// version it adds.
const (
	Format      = "4.0.0"
	RuleVersion = "2.0.0"
)

// The bounds of spec/dialect.md §2.2.2 and §2.2.7, of the base profile, the
// one profile read: the extended profile's are not, until the Spec decides
// its literal bound (spec issue #324).
const (
	MaxDerived           = 16
	MaxLiteralBytes      = 8192
	MaxListElements      = 1024
	MaxListElementBytes  = 1024
	MaxDecimalCharacters = 128
	maxClauses           = 64
	maxListed            = 64
)

// The parameter types: format 3.0.0's three, and the three format 4.0.0
// adds (spec/dialect.md §2.2.2).
const (
	TypeString           = "string"
	TypeInteger          = "integer"
	TypeTimestamp        = "timestamp"
	TypeDecimal          = "decimal"
	TypeStringList       = "string-list"
	TypeAddressRangeList = "address-range-list"
)

// KnownType reports whether t is one of the six parameter types of format
// 4.0.0.
func KnownType(t string) bool {
	switch t {
	case TypeString, TypeInteger, TypeTimestamp, TypeDecimal, TypeStringList, TypeAddressRangeList:
		return true
	}
	return false
}

// AddedType reports whether t is one of the three types format 4.0.0 adds,
// which a document of an earlier format does not declare.
func AddedType(t string) bool {
	return t == TypeDecimal || t == TypeStringList || t == TypeAddressRangeList
}

// IsList reports whether t is a list type.
func IsList(t string) bool { return t == TypeStringList || t == TypeAddressRangeList }

// OfType reports whether v is a value of the parameter type t.
func OfType(v any, t string) bool {
	switch t {
	case TypeString:
		_, ok := v.(string)
		return ok
	case TypeInteger:
		return IsInteger(v)
	case TypeTimestamp:
		_, ok := Instant(v)
		return ok
	case TypeDecimal:
		return IsDecimal(v)
	case TypeStringList:
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, x := range a {
			if _, ok := x.(string); !ok {
				return false
			}
		}
		return true
	case TypeAddressRangeList:
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, x := range a {
			if _, ok := rangeOf(x); !ok {
				return false
			}
		}
		return true
	}
	return false
}

// ListBounds holds the value a list parameter takes to §2.2.7's bounds, when
// the parameter takes it and before its type is checked: at most 1024
// elements, and each string element at most 1024 bytes.
func ListBounds(v any) error {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	if len(a) > MaxListElements {
		return failure(CodeOverBound, "a list of more than %d elements", MaxListElements)
	}
	for _, x := range a {
		if s, ok := x.(string); ok && len(s) > MaxListElementBytes {
			return failure(CodeOverBound, "a list element of more than %d bytes", MaxListElementBytes)
		}
	}
	return nil
}

// An address is its width, 32 or 128 bits, and its value, high and low
// halves (spec/dialect.md §2.2.3).
type address struct {
	width  int
	hi, lo uint64
}

// An addressRange is a base address and a prefix length.
type addressRange struct {
	base   address
	prefix int
}

var (
	octet     = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,2})$`)
	hextet    = regexp.MustCompile(`^[0-9A-Fa-f]{1,4}$`)
	prefixLen = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,2})$`)
)

func ipv4(text string) (uint64, bool) {
	parts := strings.Split(text, ".")
	if len(parts) != 4 {
		return 0, false
	}
	var v uint64
	for _, p := range parts {
		if !octet.MatchString(p) {
			return 0, false
		}
		n, _ := strconv.Atoi(p)
		if n > 255 {
			return 0, false
		}
		v = v<<8 | uint64(n)
	}
	return v, true
}

func hextets(text string, last bool) ([]uint64, bool) {
	if text == "" {
		return nil, true
	}
	items := strings.Split(text, ":")
	var groups []uint64
	for i, item := range items {
		switch {
		case last && i == len(items)-1 && strings.Contains(item, "."):
			low, ok := ipv4(item)
			if !ok {
				return nil, false
			}
			groups = append(groups, low>>16, low&0xffff)
		case hextet.MatchString(item):
			n, _ := strconv.ParseUint(item, 16, 16)
			groups = append(groups, n)
		default:
			return nil, false
		}
	}
	return groups, true
}

func ipv6(text string) (hi, lo uint64, ok bool) {
	var groups []uint64
	if head, tail, found := strings.Cut(text, "::"); found {
		if strings.Contains(tail, "::") {
			return 0, 0, false
		}
		before, ok1 := hextets(head, false)
		after, ok2 := hextets(tail, true)
		if !ok1 || !ok2 || len(before)+len(after) > 7 {
			return 0, 0, false
		}
		groups = append(groups, before...)
		groups = append(groups, make([]uint64, 8-len(before)-len(after))...)
		groups = append(groups, after...)
	} else {
		found, ok := hextets(text, true)
		if !ok || len(found) != 8 {
			return 0, 0, false
		}
		groups = found
	}
	for i, g := range groups {
		if i < 4 {
			hi = hi<<16 | g
		} else {
			lo = lo<<16 | g
		}
	}
	return hi, lo, true
}

// addressOf reads an address (spec/dialect.md §2.2.3): of family IPv4
// exactly when it holds no colon.
func addressOf(v any) (address, bool) {
	text, ok := v.(string)
	if !ok {
		return address{}, false
	}
	if strings.Contains(text, ":") {
		hi, lo, ok := ipv6(text)
		return address{128, hi, lo}, ok
	}
	lo, ok := ipv4(text)
	return address{32, 0, lo}, ok
}

// rangeOf reads an address range: an address, a slash and a prefix length,
// every bit after the prefix zero.
func rangeOf(v any) (addressRange, bool) {
	text, ok := v.(string)
	if !ok || strings.Count(text, "/") != 1 {
		return addressRange{}, false
	}
	base, length, _ := strings.Cut(text, "/")
	a, ok := addressOf(base)
	if !ok || !prefixLen.MatchString(length) {
		return addressRange{}, false
	}
	prefix, _ := strconv.Atoi(length)
	if prefix > a.width || !hostBitsZero(a, a.width-prefix) {
		return addressRange{}, false
	}
	return addressRange{a, prefix}, true
}

// hostBitsZero reports whether the low n bits of a are zero.
func hostBitsZero(a address, n int) bool {
	low := func(x uint64, bits int) uint64 {
		if bits >= 64 {
			return x
		}
		return x & (1<<uint(bits) - 1)
	}
	if n <= 64 {
		return low(a.lo, n) == 0
	}
	return a.lo == 0 && low(a.hi, n-64) == 0
}

// in reports whether a is in r: of one family, and equal to r's base in its
// first prefix bits. The families never meet.
func (r addressRange) in(a address) bool {
	if a.width != r.base.width {
		return false
	}
	top := func(x address, n int) (uint64, uint64) {
		if x.width == 32 {
			if n == 0 {
				return 0, 0
			}
			return 0, x.lo >> uint(32-n)
		}
		switch {
		case n == 0:
			return 0, 0
		case n <= 64:
			return x.hi >> uint(64-n), 0
		case n == 128:
			return x.hi, x.lo
		}
		return x.hi, x.lo >> uint(128-n)
	}
	h1, l1 := top(a, r.prefix)
	h2, l2 := top(r.base, r.prefix)
	return h1 == h2 && l1 == l2
}
