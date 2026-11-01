package domain

import (
	"time"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// The metering vocabulary billing speaks, named once. Credits are not money (the compiler must know), and a
// Scope is the opaque subject: billing never learns what a host means by it.
type (
	Money   = md.Money
	Scope   = md.Scope
	Realm   = md.Realm
	Credits = md.Credits
	Pool    = md.Pool
)

// Provider is an adapter name — an open vocabulary, not an enum, because a new provider is an adapter plus
// configuration, never a migration.
type Provider string

// ProviderAccount is one set of provider credentials. A deployment may hold several (one per product, or test
// and live), and one account may serve several realms. Webhooks arrive at /webhooks/{provider}/{account}, so
// the account — and therefore the secret and the realms it may touch — is known before a byte of the body is
// parsed.
type ProviderAccount struct {
	ID               string // "stripe-live", "stripe-acme"
	Provider         Provider
	Realms           []Realm
	Mode             string // "live" | "test"
	SecretRef        string // a reference ("env:STRIPE_SECRET_KEY") — never the secret itself
	WebhookSecretRef string
	Active           bool // an inactive account is a 404 at the route
}

// PlanKind is what a plan sells.
type PlanKind string

// The kinds of plan.
const (
	OneTime      PlanKind = "one_time"     // a credit pack
	Subscription PlanKind = "subscription" // a recurring allotment
)

// Interval is how often a subscription renews.
type Interval string

// The renewal intervals.
const (
	Monthly Interval = "month"
	Yearly  Interval = "year"
)

// Entitlement is one capability a plan unlocks besides credits. Exactly one of Bool, Int, Text is set, by Kind
// (flag, quota, enum); an Int of -1 is unlimited and is never an unset value. The resolver that reads these is
// the entitlement layer's.
type Entitlement struct {
	Key  string
	Kind string // "flag" | "quota" | "enum"
	Bool *bool
	Int  *int64
	Text *string
}

// Plan is a purchasable thing, defined per REALM. Code is the host's stable slug and the only identifier that
// ever crosses to a UI.
type Plan struct {
	Realm           Realm
	Code            string
	Name            string
	Kind            PlanKind
	CreditAllotment *Credits // the ONLY coupling between fiat and credits; nil = negotiated
	Pool            Pool     // the credit pool the allotment is granted into
	Interval        *Interval
	Active          bool
	SortOrder       int
	Prices          map[string]Money // ISO-4217 → amount
	Entitlements    []Entitlement
}

// ProviderPrice is the provider's price that sells a plan through one account in one currency. A verified payment finds
// its plan by it (FR-021), so it is never replaced once a plan is sold: a subscriber still renews at the old price.
type ProviderPrice struct {
	Account         string
	Currency        string // ISO-4217, upper case: the plan must be priced in it
	ProviderPriceID string
}

// Customer maps a scope to the provider's customer — the only sanctioned way a webhook learns whom it is about.
type Customer struct {
	Scope              Scope
	Account            string // provider account id
	ProviderCustomerID string
	BillingEmail       string
}

// SubStatus is where a subscription stands, as the provider last said.
type SubStatus string

// The subscription statuses.
const (
	Trialing   SubStatus = "trialing"
	Active     SubStatus = "active"
	PastDue    SubStatus = "past_due"
	Paused     SubStatus = "paused"
	Canceled   SubStatus = "canceled"
	Incomplete SubStatus = "incomplete"
)

// Sub is a subscription.
type Sub struct {
	Scope                  Scope
	PlanCode               string
	Account                string
	ProviderSubscriptionID string
	Status                 SubStatus // written ONLY by the webhook processor and the drift sweep
	CurrentPeriodStart     time.Time
	CurrentPeriodEnd       time.Time
	CancelAtPeriodEnd      bool
	GraceUntil             *time.Time // dunning: entitlements hold until this instant
	GraceLapsedAt          *time.Time // when the sweep announced the grace as over; nil until it has
	LastEventAt            time.Time  // provider timestamp of the newest state applied (invariant 15)
}

// PaymentStatus is where a payment stands.
type PaymentStatus string

// The payment statuses.
const (
	Pending           PaymentStatus = "pending"
	Succeeded         PaymentStatus = "succeeded"
	Failed            PaymentStatus = "failed"
	Refunded          PaymentStatus = "refunded"
	PartiallyRefunded PaymentStatus = "partially_refunded"
	Disputed          PaymentStatus = "disputed"
	ChargedBack       PaymentStatus = "charged_back"
)

// Payment is a fiat transaction. It is NOT a ledger row; IdemKey joins the two.
type Payment struct {
	Scope             Scope
	Account           string
	ProviderPaymentID string
	PlanCode          string // empty for ad-hoc
	Kind              string // "one_time" | "subscription_invoice" | "auto_recharge"
	Amount            Money
	TaxAmount         *Money // whatever the provider reported; never computed here
	CreditsGranted    Credits
	Pool              Pool
	RefundedMinor     int64   // cumulative, as the provider reports it
	CreditsRevoked    Credits // cumulative clawback (invariant 16)
	Status            PaymentStatus
	DisputeID         string
	DisputeStatus     string  // "" | "open" | "won" | "lost"
	DisputeClawed     Credits // what a chargeback took, so a won dispute can give back exactly that (invariant 17)
	ReceiptURL        string
	FailureReason     string
	IdemKey           string // == the grant's idem_key
	CreatedAt         time.Time
}

// EventType is the provider-neutral kind of a verified event.
type EventType string

// The event types.
const (
	PaymentSucceeded       EventType = "payment_succeeded"
	PaymentFailed          EventType = "payment_failed"
	PaymentRefunded        EventType = "payment_refunded" // Object.AmountRefunded is CUMULATIVE
	SubscriptionUpdated    EventType = "subscription_updated"
	SubscriptionDeleted    EventType = "subscription_deleted"
	DisputeOpened          EventType = "dispute_opened"           // funds not yet taken
	DisputeFundsWithdrawn  EventType = "dispute_funds_withdrawn"  // the chargeback happens here
	DisputeFundsReinstated EventType = "dispute_funds_reinstated" // won: the chargeback reverses
	DisputeClosed          EventType = "dispute_closed"           // Object.Status: "won" | "lost"
	// Ignored is a FIRST-CLASS outcome, not an error: providers send dozens of types nobody subscribed to, and
	// treating them as errors turns normal traffic into alert noise.
	Ignored EventType = "ignored"
)

// Event is a VERIFIED, NORMALIZED provider event. Constructing one outside a successful VerifyWebhook is a
// defect: the type's existence is the proof that verification happened.
type Event struct {
	Provider        Provider
	Account         string
	ProviderEventID string
	CreatedAt       time.Time // the PROVIDER's timestamp; orders state, never delivery order
	Type            EventType
	Object          EventObject // provider-neutral facts; one dispatcher serves every provider
	RawHash         string      // SHA-256 of the verified body — stored instead of the body (PII)
}

// EventObject is what a verified event says, in no provider's vocabulary.
type EventObject struct {
	ProviderCustomerID     string
	ProviderPaymentID      string
	ProviderSubscriptionID string
	ProviderPriceID        string
	ProviderRefundID       string
	DisputeID              string
	Amount                 *Money
	AmountRefunded         *Money // cumulative for the payment
	TaxAmount              *Money
	Status                 string
	PeriodStart, PeriodEnd *time.Time
	CancelAtPeriodEnd      bool
	ReceiptURL             string
	FailureReason          string
	PaymentMethodRef       string // a saved method's provider reference (auto top-up); never card data
}
