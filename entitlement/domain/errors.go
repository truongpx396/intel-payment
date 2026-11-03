package domain

import (
	"fmt"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ErrInvalid is the engine's: a malformed key, value, declaration or grant is the caller's mistake, 400 on every transport.
var ErrInvalid = md.ErrInvalid

// ErrReasonRequired is an override that does not say who approved it and why (invariant 5). An unexplained override is
// indistinguishable from a bug, and is usually discovered during a revenue investigation.
var ErrReasonRequired = fmt.Errorf("%w: an override must say who approved it and why", ErrInvalid)
