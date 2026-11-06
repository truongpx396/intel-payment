package ports

import (
	"context"
	"net/http"
)

// IngressResult is the answer to one provider delivery.
type IngressResult struct {
	Status int    // the HTTP status to answer
	Code   string // the error code when Status is not 2xx: unknown_account | invalid_signature | …
}

// WebhookIngress is stage 1 (paymentd, synchronous, provider-facing): verify, persist, acknowledge. Nothing in it
// touches money.
type WebhookIngress interface {
	// Handle takes the RAW body and headers of one delivery to /webhooks/{provider}/{account}.
	Handle(ctx context.Context, provider, account string, body []byte, header http.Header) IngressResult
	// Retry verifies the parked deliveries again (billing.webhook.tick): one that now verifies becomes an
	// ordinary event and loses its raw body; one that fails is rejected — a security event, never processed;
	// one that still cannot be verified waits for its next attempt.
	Retry(ctx context.Context) error
}

// DriftReport is what a subscription drift sweep came to. Checked is every live subscription it asked the provider about, and is the sum
// of the four outcomes below it; Accounts is how many accounts it visited.
type DriftReport struct {
	Accounts  int
	Checked   int
	Repaired  int // the books differed from the provider and now agree: a webhook was missed
	Unchanged int // the provider and the books already agreed, or the provider's answer was older than the state held: nothing was written
	Skipped   int // the provider's object names a plan the catalogue does not sell: nothing was written
	Failed    int // the provider could not answer, or the repair could not be applied: reported in the sweep's error
}

// SubscriptionDrift is the backstop for a webhook that never arrived (R110a): it asks the provider for the current object of every live
// subscription and applies it through the one door that writes status, so a state that has not changed is neither written nor announced.
// It is idempotent, and safe from many workers at once: the write is conditional on being newer.
type SubscriptionDrift interface {
	// SweepSubscriptions sweeps one account. An account that does not exist or is switched off is domain.ErrUnknownAccount, and nothing
	// has been asked of the provider. One subscription failing does not stop the others; the failures are returned together, by id, with
	// the report of what did happen.
	SweepSubscriptions(ctx context.Context, account string) (DriftReport, error)
	// SweepAllSubscriptions sweeps every active account that holds a live subscription, and sums their reports.
	SweepAllSubscriptions(ctx context.Context) (DriftReport, error)
}

// WebhookProcessor is stage 2 (payment-worker): it fulfils verified events from the inbox.
type WebhookProcessor interface {
	// Tick leases the events due and processes them; it also retries parked deliveries. Safe to run from many
	// processors at once: a lease keeps two off one event.
	Tick(ctx context.Context) error
}
