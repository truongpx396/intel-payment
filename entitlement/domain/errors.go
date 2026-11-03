package domain

import (
	"errors"
	"fmt"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ErrInvalid is the engine's: a malformed key, value, declaration or grant is the caller's mistake, 400 on every transport.
var ErrInvalid = md.ErrInvalid

// ErrReasonRequired is an override that does not say who approved it and why (invariant 5). An unexplained override is
// indistinguishable from a bug, and is usually discovered during a revenue investigation.
var ErrReasonRequired = fmt.Errorf("%w: an override must say who approved it and why", ErrInvalid)

// ErrUnavailable is an entitlement that could not be resolved: the store did not answer, or what it answered could not be
// read. It is never "not entitled" and never "entitled": the host denies (invariant 2), and a transport answers 503. The cause
// stays wrapped for the operator.
var ErrUnavailable = errors.New("entitlements unavailable")
