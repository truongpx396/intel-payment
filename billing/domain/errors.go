package domain

import "errors"

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
	// ErrNotImplemented is what a stub answers; production code never returns it.
	ErrNotImplemented = errors.New("not implemented")
)
