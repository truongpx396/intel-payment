package domain

import (
	"encoding/hex"
	"fmt"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// Valid reports whether t is one of the event types.
func (t EventType) Valid() bool {
	switch t {
	case PaymentSucceeded, PaymentFailed, PaymentRefunded, SubscriptionUpdated, SubscriptionDeleted,
		DisputeOpened, DisputeFundsWithdrawn, DisputeFundsReinstated, DisputeClosed, Ignored:
		return true
	}
	return false
}

// Validate refuses an Event that a successful VerifyWebhook could not have produced: the type's existence is
// the proof that verification happened, so one built by hand — or by an adapter that skipped a step — fails
// closed at the inbox instead of being trusted. An Ignored event is valid; it is normal traffic.
func (e Event) Validate() error {
	switch {
	case e.Provider == "":
		return fmt.Errorf("%w: event has no provider", md.ErrInvalid)
	case e.Account == "":
		return fmt.Errorf("%w: event has no provider account", md.ErrInvalid)
	case e.ProviderEventID == "":
		return fmt.Errorf("%w: event has no provider event id", md.ErrInvalid)
	case e.CreatedAt.IsZero():
		return fmt.Errorf("%w: event %s carries no provider timestamp — subscription state is ordered by it", md.ErrInvalid, e.ProviderEventID)
	case !e.Type.Valid():
		return fmt.Errorf("%w: event %s has unknown type %q", md.ErrInvalid, e.ProviderEventID, e.Type)
	}
	if raw, err := hex.DecodeString(e.RawHash); err != nil || len(raw) != 32 {
		return fmt.Errorf("%w: event %s has no SHA-256 of its verified body", md.ErrInvalid, e.ProviderEventID)
	}
	return nil
}
