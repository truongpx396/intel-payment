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

// WebhookProcessor is stage 2 (payment-worker): it fulfils verified events from the inbox.
type WebhookProcessor interface {
	// Tick leases the events due and processes them; it also retries parked deliveries. Safe to run from many
	// processors at once: a lease keeps two off one event.
	Tick(ctx context.Context) error
}
