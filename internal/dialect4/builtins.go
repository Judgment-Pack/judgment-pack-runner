package dialect4

import (
	"encoding/json"
	"math/big"
	"strconv"
)

// The fixed-point arithmetic of distance (spec/dialect.md §2.2.5): every
// value an integer standing for itself over scale.
var (
	scale    = new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	piScaled = mustInt("3141592653589793238462643383279")
	halfPi   = new(big.Int).Div(piScaled, big.NewInt(2))
	radiusMM = big.NewInt(6371008800)
	sinHalf  = sinScaled(halfPi)
)

func mustInt(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return n
}

// floorDiv is ⌊a / b⌋ for b > 0.
func floorDiv(a, b *big.Int) *big.Int { return new(big.Int).Div(a, b) }

func clamp(total *big.Int) *big.Int {
	if total.Sign() < 0 {
		return new(big.Int)
	}
	if total.Cmp(scale) > 0 {
		return new(big.Int).Set(scale)
	}
	return total
}

// sinScaled is §2.2.5's sine of x at scale: the series x - x³/3! + ...,
// each term rounded down from the one before, stopping before the first
// term that is zero, the sum held to [0, scale].
func sinScaled(x *big.Int) *big.Int {
	square := floorDiv(new(big.Int).Mul(x, x), scale)
	term := new(big.Int).Set(x)
	total := new(big.Int).Set(x)
	divisor := new(big.Int)
	for k := int64(1); ; k++ {
		divisor.Mul(scale, big.NewInt(2*k*(2*k+1)))
		term = floorDiv(term.Mul(term, square), divisor)
		if term.Sign() == 0 {
			break
		}
		if k%2 == 1 {
			total.Sub(total, term)
		} else {
			total.Add(total, term)
		}
	}
	return clamp(total)
}

// cosScaled is §2.2.5's cosine: the series 1 - x²/2! + ..., as sinScaled.
func cosScaled(x *big.Int) *big.Int {
	square := floorDiv(new(big.Int).Mul(x, x), scale)
	term := new(big.Int).Set(scale)
	total := new(big.Int).Set(scale)
	divisor := new(big.Int)
	for k := int64(1); ; k++ {
		divisor.Mul(scale, big.NewInt((2*k-1)*2*k))
		term = floorDiv(term.Mul(term, square), divisor)
		if term.Sign() == 0 {
			break
		}
		if k%2 == 1 {
			total.Sub(total, term)
		} else {
			total.Add(total, term)
		}
	}
	return clamp(total)
}

// asinScaled is §2.2.5's inverse sine of s: halfPi when sin(halfPi) ≤ s, and
// otherwise the largest y found by bisection over [0, halfPi] with
// sin(y) ≤ s.
func asinScaled(s *big.Int) *big.Int {
	if sinHalf.Cmp(s) <= 0 {
		return new(big.Int).Set(halfPi)
	}
	low, high := new(big.Int), new(big.Int).Set(halfPi)
	one := big.NewInt(1)
	gap, middle := new(big.Int), new(big.Int)
	for gap.Sub(high, low).Cmp(one) > 0 {
		middle.Add(low, high)
		middle.Rsh(middle, 1)
		if sinScaled(middle).Cmp(s) <= 0 {
			low.Set(middle)
		} else {
			high.Set(middle)
		}
	}
	return low
}

// angleScaled is ⌊q × π / k⌋ at scale, exactly, for q ≥ 0: k 180 for an
// angle, 360 for its half.
func angleScaled(q *big.Rat, k int64) *big.Int {
	n := new(big.Int).Mul(q.Num(), piScaled)
	return floorDiv(n, new(big.Int).Mul(big.NewInt(k), q.Denom()))
}

// distanceMM is §2.2.5's distance between two points, in whole millimetres
// rounded down, every coordinate within its bounds.
func distanceMM(lat1, lon1, lat2, lon2 *big.Rat) int64 {
	across := new(big.Rat).Sub(lat2, lat1)
	across.Abs(across)
	around := new(big.Rat).Sub(lon2, lon1)
	around.Abs(around)
	if around.Cmp(big.NewRat(180, 1)) > 0 {
		around.Sub(big.NewRat(360, 1), around)
	}
	a := sinScaled(angleScaled(across, 360))
	b := sinScaled(angleScaled(around, 360))
	abs := func(q *big.Rat) *big.Rat { return new(big.Rat).Abs(q) }
	c := floorDiv(new(big.Int).Mul(cosScaled(angleScaled(abs(lat1), 180)), cosScaled(angleScaled(abs(lat2), 180))), scale)
	h := floorDiv(new(big.Int).Mul(a, a), scale)
	bb := floorDiv(new(big.Int).Mul(b, b), scale)
	h.Add(h, floorDiv(new(big.Int).Mul(c, bb), scale))
	if h.Cmp(scale) > 0 {
		h.Set(scale)
	}
	r := new(big.Int).Sqrt(new(big.Int).Mul(h, scale))
	y := asinScaled(r)
	d := floorDiv(new(big.Int).Mul(new(big.Int).Mul(big.NewInt(2), radiusMM), y), scale)
	return d.Int64()
}

// A derivedValue is a built-in's value, or none, and the fields and the
// parameters it read.
type derivedValue struct {
	value      json.Number
	none       bool
	fields     map[string]bool
	parameters map[string]bool
}

func operandOf(spec map[string]any, value any, params map[string]any, into *derivedValue) (any, bool) {
	if field, ok := spec["field"].(string); ok {
		into.fields[field] = true
		return Resolve(value, field)
	}
	name := spec["param"].(string)
	into.parameters[name] = true
	v, ok := params[name]
	return v, ok
}

func coordinate(v any, present bool, bound int64) (*big.Rat, bool) {
	if !present || !IsDecimal(v) {
		return nil, false
	}
	q, ok := new(big.Rat).SetString(v.(string))
	if !ok || q.Cmp(big.NewRat(-bound, 1)) < 0 || q.Cmp(big.NewRat(bound, 1)) > 0 {
		return nil, false
	}
	return q, true
}

// derive computes a rule's derived values in their order (spec/dialect.md
// §2.2.5), with integer arithmetic only.
func derive(declarations []map[string]any, value any, params map[string]any) map[string]*derivedValue {
	out := make(map[string]*derivedValue, len(declarations))
	for _, d := range declarations {
		v := &derivedValue{none: true, fields: map[string]bool{}, parameters: map[string]bool{}}
		switch d["builtin"] {
		case "duration":
			start, ok1 := operandOf(d["from"].(map[string]any), value, params, v)
			end, ok2 := operandOf(d["to"].(map[string]any), value, params, v)
			first, okA := Instant(start)
			second, okB := Instant(end)
			if ok1 && ok2 && okA && okB {
				v.value, v.none = json.Number(strconv.FormatInt(second-first, 10)), false
			}
		case "distance":
			var points []*big.Rat
			all := true
			for _, end := range []string{"from", "to"} {
				point := d[end].(map[string]any)
				for _, member := range []struct {
					name  string
					bound int64
				}{{"latitude", 90}, {"longitude", 180}} {
					found, present := operandOf(point[member.name].(map[string]any), value, params, v)
					q, ok := coordinate(found, present, member.bound)
					all = all && ok
					points = append(points, q)
				}
			}
			if all {
				v.value, v.none = json.Number(strconv.FormatInt(distanceMM(points[0], points[1], points[2], points[3]), 10)), false
			}
		default: // speed
			distance, duration := out[d["distance"].(string)], out[d["duration"].(string)]
			for k := range distance.fields {
				v.fields[k] = true
			}
			for k := range duration.fields {
				v.fields[k] = true
			}
			for k := range distance.parameters {
				v.parameters[k] = true
			}
			for k := range duration.parameters {
				v.parameters[k] = true
			}
			if !distance.none && !duration.none {
				t := integerOf(duration.value)
				if t < 0 {
					t = -t
				}
				if t != 0 {
					v.value, v.none = json.Number(strconv.FormatInt(integerOf(distance.value)/t, 10)), false
				}
			}
		}
		out[d["name"].(string)] = v
	}
	return out
}
