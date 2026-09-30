package domain

import (
	"fmt"
	"math"
)

// Credits is the single INTERNAL accounting unit — a signed delta. Never a float.
//
// The unit should be FINE. The reference configuration is 1 credit = 1 µ$ of list price: rounding
// up once per event then costs under a millionth of a dollar, and int64 still holds $9.2 trillion.
// How credits are DISPLAYED ("$12.40", "12,400 credits") is UnitLabels.scale in the UI — never a
// reason to coarsen the unit the books are kept in (D31).
type Credits int64

// MaxOperationAmount is the default ceiling on a single operation: 2^50. It keeps every hot balance
// inside the range Redis Lua represents exactly (±2^53).
const MaxOperationAmount Credits = 1 << 50

// MaxHotAmount is the largest operation any configuration may allow: 2^52.
const MaxHotAmount Credits = 1 << 52

// Add returns c + o, or an error on int64 overflow. Money is never allowed to wrap.
func (c Credits) Add(o Credits) (Credits, error) {
	if (o > 0 && c > math.MaxInt64-o) || (o < 0 && c < math.MinInt64-o) {
		return 0, fmt.Errorf("%w: %d + %d overflows", ErrAmountOutOfRange, c, o)
	}
	return c + o, nil
}

// SumCredits totals the balances of several pools, failing on overflow.
func SumCredits(cs ...Credits) (Credits, error) {
	var t Credits
	for _, c := range cs {
		var err error
		if t, err = t.Add(c); err != nil {
			return 0, err
		}
	}
	return t, nil
}
