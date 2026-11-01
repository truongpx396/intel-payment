package domain

import (
	"errors"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// Errors of the fiat layer. The metering errors (ErrInvalid, ErrNotFound, ErrIdemConflict, …) are reused as they
// are; these are the ones only a payment boundary can raise.
var (
	// ErrInvalidSignature is a webhook whose signature does not verify on the raw body, within the tolerance,
	// with the route's account secret: 400. A SECURITY event, never processed.
	ErrInvalidSignature = errors.New("webhook signature invalid")
	// ErrVerificationUnavailable is a webhook that could be neither verified nor rejected because verification
	// itself could not run (PayPal's API is down): the delivery is parked with its raw body and answered 202.
	ErrVerificationUnavailable = errors.New("webhook verification unavailable")
	// ErrUnknownAccount is a provider account that does not exist, is inactive, or belongs to another provider: 404.
	ErrUnknownAccount = errors.New("unknown provider account")
	// ErrUnknownPrice is a provider price id (or a plan code and currency) the catalogue does not know. It never
	// falls back to a default plan (FR-021).
	ErrUnknownPrice = errors.New("unknown price")
	// ErrBelowUsage is a plan change that would take an entitlement below what the scope is using now (FR-029).
	ErrBelowUsage = errors.New("plan change would take an entitlement below current usage")
	// ErrUnsupported is an operation the provider cannot do at all — saving a payment method for later, charging one
	// off-session. It is not an outage and retrying will not help: the adapter says so without calling the provider, and
	// the caller reports it rather than falling back on something it was not asked for.
	ErrUnsupported = errors.New("the provider does not support this")
	// ErrChargeDeclined is a charge the provider REFUSED: the card was declined, the saved method is not there, the customer
	// must authenticate. The outcome is known and no retry will change it, so it counts toward switching an auto top-up
	// off. Any other failure of ChargeSaved leaves the outcome unknown, and the same key is tried again.
	ErrChargeDeclined = errors.New("the provider refused the charge")
	// ErrNotImplemented is what a stub answers; production code never returns it.
	ErrNotImplemented = errors.New("not implemented")
)

// The engine's errors, which billing reuses as they are (a payment can be invalid, missing, already there, or a replay of
// another request), named here so that a layer that speaks for billing — a door — need not import the engine's domain to
// recognise them. They are the same values, so errors.Is finds either name.
var (
	ErrInvalid       = md.ErrInvalid
	ErrNotFound      = md.ErrNotFound
	ErrAlreadyExists = md.ErrAlreadyExists
	ErrIdemConflict  = md.ErrIdemConflict
)
