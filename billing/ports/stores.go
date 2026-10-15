package ports

import (
	"context"
	"net/http"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
)

// Catalog resolves the realm's purchasable offers. Plans live in Postgres, not Go literals (constitution VIII).
type Catalog interface {
	List(ctx context.Context, realm domain.Realm, currency string) ([]domain.Plan, error)
	// Resolve returns the plan and the provider price id for one account and currency. An unknown code,
	// currency or mapping is domain.ErrUnknownPrice — never a default plan.
	Resolve(ctx context.Context, realm domain.Realm, code, currency, account string) (domain.Plan, string, error)
	// PlanForPrice maps a VERIFIED provider price id back to a plan — how a webhook learns what was bought.
	// ok=false means the account sells nothing at that price.
	PlanForPrice(ctx context.Context, account, providerPriceID string) (domain.Plan, bool, error)
}

// Customers maps scopes to provider customers.
type Customers interface {
	Get(ctx context.Context, s domain.Scope, account string) (domain.Customer, bool, error)
	Upsert(ctx context.Context, c domain.Customer) error
	// ResolveScope maps a VERIFIED provider customer id to a Scope — and so to a realm, which must be one the
	// account serves. The only sanctioned way a webhook learns whom it is about.
	ResolveScope(ctx context.Context, account, providerCustomerID string) (domain.Scope, bool, error)
}

// LeasedEvent is an inbox event claimed for processing.
type LeasedEvent struct {
	ID       string // the inbox row's id: what MarkProcessed / MarkFailed take
	Event    domain.Event
	Attempts int // including this one
}

// ParkedDelivery is a delivery whose verification was unavailable, kept with its raw body until it can be
// verified. The body is deleted the moment it verifies.
type ParkedDelivery struct {
	ID       string
	Provider domain.Provider
	Account  string
	Body     []byte
	Header   http.Header
	Attempts int
	// ReceivedAt is when the delivery FIRST arrived. Its signature is judged as of then, not as of the retry.
	ReceivedAt time.Time
}

// Inbox is the durable webhook queue (payment_events): a verified delivery is persisted here, acknowledged, and
// only then processed, with leases, retries and a dead state that pages (invariant 5).
type Inbox interface {
	// Persist stores a verified, normalized event. An existing (account, event id) is a no-op that still reports
	// success: the delivery is durable either way, so the ingress answers 200.
	Persist(ctx context.Context, e domain.Event) (inserted bool, err error)
	// Park stores a delivery whose verification is UNAVAILABLE (not failed), raw body included, until
	// verification can be retried. eventID is a stable name for the delivery (the body is not parsed), so a
	// provider's redelivery of the same bytes is one row.
	Park(ctx context.Context, provider domain.Provider, account, eventID string, raw []byte, headers http.Header) error

	// Lease claims up to n events due for processing (received, failed past backoff, or a processing lease that
	// expired) for `ttl`. A lease is a conditional UPDATE: two processors never hold the same event, and an event
	// is not leased while an earlier one of the same customer is still being worked, so a customer's events are
	// handled in the provider's order.
	Lease(ctx context.Context, n int, ttl time.Duration) ([]LeasedEvent, error)
	MarkProcessed(ctx context.Context, id string, outcome string) error // outcome: processed | ignored
	// MarkFailed records a failed attempt and schedules the next; past the attempt cap the event is dead and
	// pages. It reports whether the event is now dead.
	MarkFailed(ctx context.Context, id string, cause error, retryAt time.Time) (dead bool, err error)

	// LeaseParked claims up to n parked deliveries due for another verification attempt.
	LeaseParked(ctx context.Context, n int, ttl time.Duration) ([]ParkedDelivery, error)
	// Promote turns a parked delivery that has now verified into an ordinary received event and deletes its body.
	Promote(ctx context.Context, id string, e domain.Event) (inserted bool, err error)
	// RetryParked records that verification was still unavailable and schedules the next attempt.
	RetryParked(ctx context.Context, id string, retryAt time.Time) error
	// Reject marks a parked delivery whose verification FAILED on retry. Never trusted, never processed; the
	// body is dropped.
	Reject(ctx context.Context, id string, reason string) error

	// Oldest is the receipt time of the oldest verified event not yet processed — a payment someone made that has
	// not been fulfilled yet. ok=false when the inbox is drained.
	Oldest(ctx context.Context) (receivedAt time.Time, ok bool, err error)
	// Purge drops processed rows past retention, which MUST exceed the longest provider replay window.
	Purge(ctx context.Context, olderThan time.Time) (int, error)
}

// RechargeClaim, RechargeRefusal and the refusals are the domain's: the rules of a slot are pure and live there, so every
// store applies the same ones.
type (
	RechargeClaim   = domain.RechargeClaim
	RechargeRefusal = domain.RechargeRefusal
)

// The refusals; the zero value means a slot was claimed.
const (
	RechargeOK        = domain.RechargeOK
	RechargeDisabled  = domain.RechargeDisabled
	RechargeMaxPerDay = domain.RechargeMaxPerDay
)

// AutoRecharges holds each scope's standing instruction to top up, and bounds what it may do.
type AutoRecharges interface {
	Get(ctx context.Context, s domain.Scope) (domain.AutoRecharge, bool, error)
	// Put stores the configuration, never the ledger, so editing an instruction cannot hand out more charges today.
	// Written enabled it clears the failures; written disabled it records the reason given. An invalid instruction is
	// md.ErrInvalid and is not stored.
	Put(ctx context.Context, a domain.AutoRecharge) error
	// Claim reserves the next charge slot, atomically: at most MaxPerDay distinct slots per UTC day however many
	// times a trigger fires, and while one is in flight (claimed, not settled, younger than staleAfter) another
	// claim RESUMES it — so a duplicated trigger names the same idempotency key and the provider charges once.
	Claim(ctx context.Context, s domain.Scope, now time.Time, staleAfter time.Duration) (RechargeClaim, RechargeRefusal, error)
	// Settle ends the in-flight slot: a good charge clears the failure count; a failed one adds one, and the third
	// in a row disables the instruction with the reason recorded. It reports whether this settling disabled it.
	Settle(ctx context.Context, s domain.Scope, ok bool, reason string, now time.Time) (disabled bool, err error)
}

// Payments are the fiat transactions. A payment is NOT a ledger row; its IdemKey joins the two.
type Payments interface {
	Get(ctx context.Context, account, providerPaymentID string) (domain.Payment, bool, error)
	// Upsert writes a payment keyed on (account, provider payment id).
	Upsert(ctx context.Context, p domain.Payment) error
	// Update runs fn on the payment under an exclusive lock on (account, provider payment id) and stores what it
	// returns. Two refund reports for one payment, handled by two processors, must not both read the same
	// cumulative figures: the lock is what makes "revoke exactly" hold under concurrency. fn may call
	// Meter.Grant: every call it makes is idempotent, so a failed commit is a retry, not a second effect.
	Update(ctx context.Context, account, providerPaymentID string, fn func(cur domain.Payment, found bool) (domain.Payment, error)) (domain.Payment, error)
	List(ctx context.Context, s domain.Scope, cursor string, limit int) ([]domain.Payment, string, error)
}

// Subs are subscriptions. Status has exactly one writer: the processor, and the drift sweep, which uses the same
// ApplyIfNewer (invariant 12).
type Subs interface {
	Get(ctx context.Context, s domain.Scope) (domain.Sub, bool, error)
	// ApplyIfNewer writes the state only when it is newer than the one held, by the total order of domain.Sub.Apply,
	// under an exclusive lock on the subscription, so concurrent appliers converge on the newest state. A second live
	// subscription for a scope is md.ErrAlreadyExists; a plan the realm does not sell is domain.ErrUnknownPrice.
	ApplyIfNewer(ctx context.Context, sub domain.Sub, at time.Time) (applied bool, err error)
	// DueForGraceExpiry lists past_due subscriptions whose grace window has ended and has not been announced.
	DueForGraceExpiry(ctx context.Context, now time.Time) ([]domain.Sub, error)
	// MarkGraceLapsed records that the end of sub's grace was announced, only while it is still the grace that
	// ended (a newer state clears it). It reports whether it recorded.
	MarkGraceLapsed(ctx context.Context, sub domain.Sub, at time.Time) (bool, error)
}
