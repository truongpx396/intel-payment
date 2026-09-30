package domain

import (
	"fmt"
	"math"
	"regexp"
)

// Money is a FIAT amount, used only at the payments boundary. Integer minor units + ISO-4217.
// Kept distinct from Credits: changing a price never alters granted credits, and the compiler
// refuses to add the two.
type Money struct {
	MinorUnits int64  // e.g. cents
	Currency   string // ISO-4217, e.g. "USD"
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Validate reports whether m names a currency in ISO-4217's shape. It does not check the code
// against the live list: the catalogue and the provider decide which currencies are sold.
func (m Money) Validate() error {
	if !currencyPattern.MatchString(m.Currency) {
		return fmt.Errorf("%w: currency %q is not an ISO-4217 code", ErrInvalid, m.Currency)
	}
	return nil
}

// Add returns m + o. The currencies must match — there is no FX in this system — and the sum must
// not overflow.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: cannot add %s to %s", ErrInvalid, o.Currency, m.Currency)
	}
	if (o.MinorUnits > 0 && m.MinorUnits > math.MaxInt64-o.MinorUnits) ||
		(o.MinorUnits < 0 && m.MinorUnits < math.MinInt64-o.MinorUnits) {
		return Money{}, fmt.Errorf("%w: money overflows", ErrAmountOutOfRange)
	}
	return Money{MinorUnits: m.MinorUnits + o.MinorUnits, Currency: m.Currency}, nil
}
