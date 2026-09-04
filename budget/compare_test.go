package budget

import "math"

// below, atMost and above compare two floating-point results, counting a NaN on
// either side as a failed comparison.
//
// They exist because the obvious spelling is wrong here. Every use is an
// assertion whose whole purpose is to catch a damaged implementation, and a
// damaged floating-point implementation produces NaN — against which `a >= b`
// evaluates to false and reports success. Writing `!(a < b)` instead has the
// right behaviour and reads like a mistake; these keep the behaviour and say
// what it is for.
func below(a, b float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && a < b
}

func atMost(a, b float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && a <= b
}

func above(a, b float64) bool { return below(b, a) }
