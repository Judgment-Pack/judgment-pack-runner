package retainedcolumns

import (
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

var decimalSyntax = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
var numberSyntax = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
var integerSyntax = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

const maxSafe = 9007199254740991

func decimalValue(v any) bool {
	s, ok := v.(string)
	return ok && len(s) <= 128 && decimalSyntax.MatchString(s)
}
func integerValue(v any) bool {
	n, ok := v.(json.Number)
	if !ok || len(n) > 17 || !integerSyntax.MatchString(string(n)) {
		return false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return err == nil && i >= -maxSafe && i <= maxSafe
}
func rat(text string) *big.Rat { q, _ := new(big.Rat).SetString(text); return q }
func ten(e int) *big.Rat {
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(e))), nil)
	if e < 0 {
		return new(big.Rat).SetFrac(big.NewInt(1), power)
	}
	return new(big.Rat).SetInt(power)
}
func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
func floor(q *big.Rat) *big.Int       { return new(big.Int).Quo(q.Num(), q.Denom()) }
func distance(a, b *big.Rat) *big.Rat { return new(big.Rat).Abs(new(big.Rat).Sub(a, b)) }
func mul(a, b *big.Rat) *big.Rat      { return new(big.Rat).Mul(a, b) }
func div(a, b *big.Rat) *big.Rat      { return new(big.Rat).Quo(a, b) }

// shortestMagnitude implements the RFC's rational selection independently of
// host float formatting. The only float operations locate adjacent binary64s;
// interval membership, distances and all ties use exact integer arithmetic.
func shortestMagnitude(x float64) *big.Rat {
	if x == 0 {
		return new(big.Rat)
	}
	value := new(big.Rat).SetFloat64(x)
	previous := new(big.Rat).SetFloat64(math.Nextafter(x, 0))
	next := new(big.Rat).SetFloat64(math.Nextafter(x, math.Inf(1)))
	if next == nil {
		next = new(big.Rat).Add(value, new(big.Rat).Sub(value, previous))
	}
	lower := new(big.Rat).Quo(new(big.Rat).Add(previous, value), big.NewRat(2, 1))
	upper := new(big.Rat).Quo(new(big.Rat).Add(next, value), big.NewRat(2, 1))
	inclusive := math.Float64bits(x)&1 == 0
	magnitude := len(value.Num().String()) - len(value.Denom().String())
	if value.Cmp(ten(magnitude)) < 0 {
		magnitude--
	}
	for digits := 1; digits <= 17; digits++ {
		var best, bestDistance *big.Rat
		var bestCoefficient *big.Int
		minCoefficient := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits-1)), nil)
		maxCoefficient := new(big.Int).Sub(new(big.Int).Mul(minCoefficient, big.NewInt(10)), big.NewInt(1))
		for exponent := magnitude - digits; exponent <= magnitude-digits+2; exponent++ {
			scale := ten(exponent)
			lowRat, highRat := div(lower, scale), div(upper, scale)
			low, high := floor(lowRat), floor(highRat)
			if new(big.Rat).SetInt(low).Cmp(lowRat) < 0 || !inclusive {
				low.Add(low, big.NewInt(1))
			}
			if !inclusive && new(big.Rat).SetInt(high).Cmp(highRat) == 0 {
				high.Sub(high, big.NewInt(1))
			}
			if low.Cmp(minCoefficient) < 0 {
				low.Set(minCoefficient)
			}
			if high.Cmp(maxCoefficient) > 0 {
				high.Set(maxCoefficient)
			}
			if low.Cmp(high) > 0 {
				continue
			}
			near := floor(div(value, scale))
			candidates := []*big.Int{low, new(big.Int).Add(low, big.NewInt(1)), high, new(big.Int).Sub(high, big.NewInt(1)), near, new(big.Int).Add(near, big.NewInt(1))}
			for _, coefficient := range candidates {
				if coefficient.Cmp(low) < 0 || coefficient.Cmp(high) > 0 || new(big.Int).Mod(coefficient, big.NewInt(10)).Sign() == 0 {
					continue
				}
				candidate := mul(new(big.Rat).SetInt(coefficient), scale)
				delta := distance(candidate, value)
				better := best == nil
				if !better {
					cmp := delta.Cmp(bestDistance)
					better = cmp < 0 || cmp == 0 && (coefficient.Bit(0) < bestCoefficient.Bit(0) || coefficient.Bit(0) == bestCoefficient.Bit(0) && candidate.Cmp(best) < 0)
				}
				if better {
					best = candidate
					bestDistance = delta
					bestCoefficient = new(big.Int).Set(coefficient)
				}
			}
		}
		if best != nil {
			return best
		}
	}
	panic("binary64 has no 17-digit round-trip decimal")
}

func numberIssue(text string) string {
	if len(text) > 128 || !numberSyntax.MatchString(text) {
		return "number-encoding"
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || math.Abs(value) > maxSafe {
		return "number-range"
	}
	if at := strings.IndexAny(text, "eE"); at >= 0 {
		exponent := new(big.Int)
		exponent.SetString(text[at+1:], 10)
		if new(big.Int).Abs(exponent).Cmp(big.NewInt(308)) > 0 {
			return "number-exponent"
		}
	}
	selected := shortestMagnitude(math.Abs(value))
	if math.Signbit(value) {
		selected.Neg(selected)
	}
	if rat(text).Cmp(selected) != 0 {
		return "number-precision"
	}
	return ""
}
