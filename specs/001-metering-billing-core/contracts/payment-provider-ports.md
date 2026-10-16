# Contract: Payments & Providers (fiat boundary)

**Spec**: [../spec.md](../spec.md) | **Plan**: [../plan.md](../plan.md) | **Status**: Normative — the fiat layer of `intel-payment`.

This contract defines the layer that turns the credit engine into a monetized system. It is **strictly additive to consumption**: credits remain the single internal unit, and nothing here changes how they are spent. A payment provider converts fiat → credits (one-time top-up) or grants a recurring allotment (subscription), then the credits are created **through the same idempotent `Meter.Grant` any host would use**. The consumption hot path is untouched.

> **Provenance.** Derived from the Phase 2 Billing & Payments design in [aisat-intel](https://github.com/truongpx396/aisat-intel) (`specs/draft-plan.md`, commits `e6b689d` → `cc02bc4`), elevated into a ports contract. See [PROVENANCE.md](../../../PROVENANCE.md). The inbox, provider accounts, dispute lifecycle, partial-refund arithmetic and auto top-up were added in the second design review ([D34, D36, D43](../design-decisions.md#decisions-from-the-second-design-review)).

---

## Design principles

1. **Provider-agnostic core, thin adapters.** A `PaymentProvider` port exposes checkout, portal, webhook verification, event normalization, subscription fetch and off-session charges. Stripe, Polar and PayPal are interchangeable adapters behind it. **No code outside `billing/adapters/driven/<provider>/` may import a provider SDK** — enforced by `depguard`. Adding a provider is an adapter plus a `provider_accounts` row; no migration.
2. **Money is integer minor units.** All fiat is `BIGINT` minor units + an ISO-4217 `currency CHAR(3)`. Never floats, anywhere.
3. **Webhooks are the source of truth for fulfilment.** Credits are granted on a *verified* provider event, never on a checkout return. The user can close the tab, the redirect can be replayed, and the payment can fail after the redirect fires.
4. **Persist, acknowledge, then process.** A verified delivery is written to the **inbox** and only then acknowledged `200`; all processing happens afterwards, from the inbox, with its own retries. A crash anywhere after the acknowledgement loses nothing, because the provider's retry is no longer the reliability mechanism — the inbox is.
5. **Signature verification precedes everything.** Every webhook is verified on the **raw body** before any parse, lookup or side effect. A failure is `400`, a **security** event, and never processed.
6. **The fiat layer never writes the ledger.** It calls `Meter.Grant` through the metering port, exactly as any other host would (metering invariant 1).
7. **Fiat and credits are separate records with one reconciliation key.** A refund reverses fiat *and* claws back credits — two records, joined by the grant's `idem_key`.
8. **State follows the provider's clock, not the delivery order.** Providers do not order webhook deliveries; subscription state is taken from the provider's current object and guarded by the event timestamp, so a late, older event can never overwrite a newer state.

---

## Ports at a glance

```text
BROWSER                                  PROVIDER (Stripe · Polar · PayPal · yours)
   │ POST /v1/checkout                        │ POST /webhooks/{provider}/{account}
   ▼                                          ▼        (unauthenticated, signature-verified)
┌── Checkout (app) ──────────────┐   ┌── Ingress (paymentd) ────────────────────────────────┐
│  Catalog.Resolve(realm, code)  │   │  1. read RAW body (size-limited, no pre-parse)        │
│  Customers.Upsert(scope)       │   │  2. account → secrets;  VerifyWebhook  ← FIRST        │
│  PaymentProvider.CreateCheckout│   │  3. Inbox.Persist(normalized facts)  dup → no-op      │
└──────────────┬─────────────────┘   │  4. 200                          (nothing else here)  │
               │ redirectUrl          └──────────────────────┬───────────────────────────────┘
               ▼                                             │ billing.webhook.received (hint)
        (user pays at provider)      ┌── Processor (payment-worker) ─▼───────────────────────┐
                                     │  Inbox.Lease → resolve Scope from the VERIFIED customer │
                                     │  → dispatch → Meter.Grant / Subs / Payments → processed │
                                     │  failure → backoff retry → dead + page                  │
                                     └──────────────────────┬──────────────────────────────────┘
                                                            ▼
                        ┌── the ONLY way credits are created ──────────────────────────┐
                        │  Meter.Grant({scope, pool, amount, idem_key, reason})        │
                        │  idem_key = provider payment / invoice / refund / dispute id │
                        └──────────────────────────────────────────────────────────────┘
```

Five seams a host swaps independently: the **`PaymentProvider`** (who takes the money), the **provider accounts** (which credentials, for which realms), the **`Catalog`** (what is sold), the **`TaxStrategy`** (who computes tax), and the stores.

---

## Domain types

```go
package billing // billing/domain

import (
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

type Money = domain.Money

// Provider is an adapter name — an open vocabulary, not an enum, because a new provider is an
// adapter plus configuration, never a migration.
type Provider string // "stripe" | "polar" | "paypal" | …

// ProviderAccount is one set of provider credentials. A deployment may hold several (one per
// product, or test and live), and one account may serve several realms. Webhooks arrive at
// /webhooks/{provider}/{account}, so the account — and therefore the secret and the realms it
// may touch — is known before a byte of the body is parsed.
type ProviderAccount struct {
	ID               string // "stripe-live", "stripe-acme"
	Provider         Provider
	Realms           []domain.Realm
	Mode             string // "live" | "test"
	SecretRef        string // env var / secret-manager path — never the secret itself
	WebhookSecretRef string
}

type PlanKind string

const (
	OneTime      PlanKind = "one_time"     // a credit pack
	Subscription PlanKind = "subscription" // a recurring allotment
)

type Interval string

const (
	Monthly Interval = "month"
	Yearly  Interval = "year"
)

// Plan is a purchasable thing, defined per REALM. `Code` is the host's stable slug and the only
// identifier that ever crosses to a UI.
type Plan struct {
	Realm           domain.Realm
	Code            string
	Name            string
	Kind            PlanKind
	CreditAllotment *domain.Credits  // the ONLY coupling between fiat and credits; nil = negotiated
	Pool            domain.Pool      // the credit pool the allotment is granted into
	Interval        *Interval        // nil for OneTime
	Active          bool
	SortOrder       int
	Prices          map[string]Money // ISO-4217 → amount
	Entitlements    []Entitlement    // includes quotas that size metering limits (max_entitlement)
}

type Customer struct {
	Scope              domain.Scope
	Account            string // provider account id
	ProviderCustomerID string
	BillingEmail       string
}

type SubStatus string

const (
	Trialing   SubStatus = "trialing"
	Active     SubStatus = "active"
	PastDue    SubStatus = "past_due"
	Paused     SubStatus = "paused"
	Canceled   SubStatus = "canceled"
	Incomplete SubStatus = "incomplete"
)

type Sub struct {
	Scope                  domain.Scope
	PlanCode               string
	Account                string
	ProviderSubscriptionID string
	Status                 SubStatus // written ONLY by the webhook processor and the drift sweep
	CurrentPeriodStart     time.Time
	CurrentPeriodEnd       time.Time
	CancelAtPeriodEnd      bool
	GraceUntil             *time.Time // dunning: entitlements hold until this instant, and not after it
	GraceLapsedAt          *time.Time // when the sweep announced the end of the grace; nil until it has (once, not every sweep)
	LastEventAt            time.Time  // provider timestamp of the newest state applied (invariant 15)
}

type PaymentStatus string

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
	Scope             domain.Scope
	Account           string
	ProviderPaymentID string
	PlanCode          string // empty for ad-hoc
	Kind              string // "one_time" | "subscription_invoice" | "auto_recharge"
	Amount            Money
	TaxAmount         *Money // whatever the provider reported; never computed here
	CreditsGranted    domain.Credits
	Pool              domain.Pool
	RefundedMinor     int64          // cumulative, as the provider reports it
	CreditsRevoked    domain.Credits // cumulative clawback (invariant 16)
	Status            PaymentStatus
	DisputeID         string
	DisputeStatus     string // "" | "open" | "won" | "lost"
	DisputeClawed     domain.Credits // what a chargeback took, so a won dispute returns exactly that (invariant 17)
	ReceiptURL        string
	FailureReason     string
	IdemKey           string // == the grant's idem_key
	CreatedAt         time.Time
}

// Event is a VERIFIED, NORMALIZED provider event. Constructing one outside a successful
// VerifyWebhook is a defect: the type's existence is the proof that verification happened.
type Event struct {
	Provider        Provider
	Account         string
	ProviderEventID string
	CreatedAt       time.Time   // the PROVIDER's timestamp; orders state, never delivery order
	Type            EventType
	Object          EventObject // provider-neutral facts; one dispatcher serves every provider
	RawHash         string      // SHA-256 of the verified body — stored instead of the body (PII)
}

type EventType string

const (
	PaymentSucceeded     EventType = "payment_succeeded"
	PaymentFailed        EventType = "payment_failed"
	PaymentRefunded      EventType = "payment_refunded"       // Object.AmountRefunded is CUMULATIVE
	SubscriptionUpdated  EventType = "subscription_updated"
	SubscriptionDeleted  EventType = "subscription_deleted"
	DisputeOpened        EventType = "dispute_opened"         // funds not yet taken
	DisputeFundsWithdrawn EventType = "dispute_funds_withdrawn" // the chargeback happens here
	DisputeFundsReinstated EventType = "dispute_funds_reinstated" // won: the chargeback reverses
	DisputeClosed        EventType = "dispute_closed"         // Object.Status: "won" | "lost"
	// Ignored is a FIRST-CLASS outcome, not an error: providers send dozens of types nobody
	// subscribed to, and treating them as errors turns normal traffic into alert noise.
	Ignored EventType = "ignored"
)

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
```

---

## Port: `PaymentProvider` — the one provider-specific seam

```go
package ports // billing/ports

type PaymentProvider interface {
	Name() domain.Provider

	// EnsureCustomer creates the provider's customer for a scope — once, idempotent on in.IdemKey — and returns its
	// id. It runs BEFORE a checkout, so the mapping from provider customer to scope exists before any payment can
	// arrive: a webhook learns whom it is about only from our own customer records, never from metadata.
	EnsureCustomer(ctx context.Context, acct domain.ProviderAccount, in CustomerInput) (providerCustomerID string, err error)

	// CreateCheckout starts a hosted checkout and returns a URL to hand off to. Mode "payment"
	// and "subscription" sell a plan; mode "setup" only saves a payment method (for auto top-up).
	// A provider that cannot save a method answers mode "setup" — and ChargeSaved — with
	// domain.ErrUnsupported, without calling the provider: never a stub's ErrNotImplemented, never a page
	// that sells something else. Selling a plan is not optional.
	// It MUST NOT grant anything: fulfilment arrives later on a verified webhook (principle 3).
	CreateCheckout(ctx context.Context, acct domain.ProviderAccount, in CheckoutInput) (redirectURL string, err error)

	// CreatePortalSession returns a provider-hosted management portal (payment methods, invoices,
	// cancellation). Re-implementing card management means handling card data; the best
	// card-data posture is never to touch it.
	CreatePortalSession(ctx context.Context, acct domain.ProviderAccount, customerID, returnURL string) (portalURL string, err error)

	// VerifyWebhook authenticates the RAW body with the account's secret and returns the
	// normalized event. CRITICAL. It MUST operate on the unmodified body, compare in CONSTANT
	// TIME, enforce a timestamp tolerance, and return an error — never a partial Event — on any
	// failure. It MAY perform I/O (PayPal verifies by API call), so callers give it a deadline, and
	// it returns ErrVerificationUnavailable when verification itself cannot run.
	// A signature's age is measured against ports.ReceivedAt(ctx, now): when the delivery ARRIVED. The
	// ingress sets it when it verifies a parked delivery again, because the provider was answered 202 and will
	// not resend it — judged by the time of the retry, an outage longer than the tolerance would make every
	// genuine delivery in the parked half look like a replay, and a rejected parked delivery is dropped.
	VerifyWebhook(ctx context.Context, acct domain.ProviderAccount, rawBody []byte, headers http.Header) (domain.Event, error)

	// FetchSubscription returns the provider's CURRENT subscription object. The processor applies
	// this, not the event payload, so delivery order cannot matter (principle 8); the drift sweep
	// uses it to repair missed webhooks.
	FetchSubscription(ctx context.Context, acct domain.ProviderAccount, providerSubID string) (domain.Sub, time.Time, error)

	// ChangePlan applies an upgrade/downgrade under the deployment's PlanChangePolicy.
	// Proration math belongs to the provider, which already owns the invoice.
	ChangePlan(ctx context.Context, acct domain.ProviderAccount, providerSubID, providerPriceID string, policy domain.PlanChangePolicy) error

	// CancelAtPeriodEnd sets the flag at the provider. Local status changes only when the
	// resulting webhook is processed (invariant 12).
	CancelAtPeriodEnd(ctx context.Context, acct domain.ProviderAccount, providerSubID string) error

	// ChargeSaved charges a saved payment method off-session — auto top-up. Idempotent on
	// in.IdemKey at the provider. It returns the provider payment id; credits still arrive only
	// on the verified payment webhook (invariant 18). An error wraps domain.ErrChargeDeclined exactly when the
	// provider REFUSED the charge for good (a declined card, a method that is not there, a customer who must
	// authenticate); any other error — a timeout, a 5xx, a rate limit, an outage — leaves the outcome unknown,
	// and the caller tries the SAME key again, which charges at most once.
	ChargeSaved(ctx context.Context, acct domain.ProviderAccount, in OffSessionInput) (providerPaymentID string, err error)
}

type CheckoutInput struct {
	Scope           domain.Scope
	Mode            string // "payment" | "subscription" | "setup"
	PlanCode        string
	Currency        string
	ProviderPriceID string
	Amount          Money  // the catalogue's price for the plan in the currency (zero for setup): providers that price a purchase themselves charge it
	CustomerID      string // existing provider customer, or "" to create
	BillingEmail    string
	SuccessURL      string
	CancelURL       string
	IdemKey         string            // a double-clicked buy button creates one checkout
	Metadata        map[string]string // echoed back; correlation ONLY, never authority (invariant 4)
}

type CustomerInput struct {
	Scope        domain.Scope
	BillingEmail string
	IdemKey      string // customer:<account>:<scope tag> — two first checkouts at once create one customer
}

type OffSessionInput struct {
	CustomerID       string
	PaymentMethodRef string
	Amount           Money
	PlanCode         string
	IdemKey          string // recharge:<realm>/<scope>:<day>:<n> — at most max_per_day distinct keys
}
```

## Port: `Catalog` — what is sold, as data

```go
// Catalog resolves the realm's purchasable offers. Plans live in Postgres, not Go literals.
type Catalog interface {
	List(ctx context.Context, realm domain.Realm, currency string) ([]domain.Plan, error)
	// Resolve → plan + the provider price id for one account and currency.
	Resolve(ctx context.Context, realm domain.Realm, code, currency, account string) (domain.Plan, string, error)
	// PlanForPrice maps a VERIFIED provider price id back to a plan — how a webhook learns what was bought.
	PlanForPrice(ctx context.Context, account, providerPriceID string) (domain.Plan, bool, error)
}
```

## Port: `TaxStrategy` — named, not silently absent

```go
// The engine does not compute tax; it names WHO does, so the answer is never "nobody".
type TaxStrategy interface {
	Mode() domain.TaxMode
	Annotate(ctx context.Context, in *ports.CheckoutInput) error
}
```

| Mode | Who computes tax | Use when |
|---|---|---|
| `provider_managed` (default) | the provider, as merchant of record or via its tax product (Stripe Tax, Polar MoR) | almost always |
| `external_hook` | a host-supplied service, called before checkout | the host already owns tax for other revenue |
| `none` | nobody — prices are tax-inclusive or out of scope | internal chargeback, credits that are not a taxable sale |

There is no `compute_it_here` mode. Rate tables and filing obligations are a specialist product.

`provider_managed` and `none` add nothing to the checkout and never call the host. `external_hook` calls the
host's service once, before the checkout is created, with what is being sold; the service may annotate the
checkout (a tax code, a calculation reference) but may not change what is sold — the scope, mode, plan,
price, currency, customer, buyer, idempotency key and both redirects were resolved from the catalogue and the
customer records, and a changed one is refused as invalid. A hook that fails fails the checkout closed: an
unpriced tax is not a checkout. A deployment that names `external_hook` and supplies no service does not start.

---

## Stores

```go
type Customers interface {
	Get(ctx context.Context, s domain.Scope, account string) (domain.Customer, bool, error)
	Upsert(ctx context.Context, c domain.Customer) error
	// ResolveScope maps a VERIFIED provider customer id to a Scope — and so to a realm, which must
	// be one the account serves. The only sanctioned way a webhook learns whom it is about.
	ResolveScope(ctx context.Context, account, providerCustomerID string) (domain.Scope, bool, error)
}

// Accounts serves provider accounts, which are configuration data. SecretResolver turns the reference an
// account holds (`env:STRIPE_SECRET_KEY`) into the secret at the moment of use; a secret is never stored,
// logged or cached beyond a call (invariant 10). Providers resolves an adapter by name.
type Accounts interface {
	Account(ctx context.Context, id string) (domain.ProviderAccount, bool, error)
	ForRealm(ctx context.Context, realm domain.Realm) ([]domain.ProviderAccount, error) // active accounts that serve it, by id
}
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}
type Providers map[domain.Provider]PaymentProvider

// Inbox is the durable webhook queue (payment_events).
type Inbox interface {
	// Persist stores a verified, normalized event. An existing (account, event id) is a no-op
	// that still reports success: the delivery is durable either way, so the ingress answers 200.
	Persist(ctx context.Context, e domain.Event) (inserted bool, err error)
	// Park stores a delivery whose verification is UNAVAILABLE (not failed), raw body included,
	// until verification can be retried. The body is not parsed, so eventID is a stable name for
	// the delivery ("parked:" + the body's SHA-256): a redelivery of the same bytes is one row.
	Park(ctx context.Context, provider domain.Provider, account, eventID string, raw []byte, headers http.Header) error

	// Lease claims up to n events due for processing (received, failed past backoff, or a
	// processing lease that expired) for this worker, for `ttl`. A lease is a conditional UPDATE:
	// two processors never hold the same event, and an event is not leased while an earlier one of
	// the same customer is still being worked — a customer's events are handled in the provider's order.
	Lease(ctx context.Context, n int, ttl time.Duration) ([]LeasedEvent, error)
	MarkProcessed(ctx context.Context, id string, outcome string) error // processed | ignored
	// MarkFailed records a failed attempt and schedules the next; past the attempt cap the event
	// is dead, and the caller pages. It reports whether the event is now dead.
	MarkFailed(ctx context.Context, id string, cause error, retryAt time.Time) (dead bool, err error)

	// The parked half. A parked delivery is retried by payment-worker's webhook loop: Promote when it now
	// verifies (the body is deleted that moment), RetryParked while verification is still
	// unavailable, Reject when it FAILS to verify — a security event; never trusted, body dropped.
	// A ParkedDelivery carries ReceivedAt, when it FIRST arrived (a redelivery of the same bytes does not move it).
	LeaseParked(ctx context.Context, n int, ttl time.Duration) ([]ParkedDelivery, error)
	Promote(ctx context.Context, id string, e domain.Event) (inserted bool, err error)
	RetryParked(ctx context.Context, id string, retryAt time.Time) error
	Reject(ctx context.Context, id string, reason string) error

	// Oldest is the receipt time of the oldest verified event not yet processed
	// (billing_webhook_inbox_age_seconds).
	Oldest(ctx context.Context) (receivedAt time.Time, ok bool, err error)
	// Purge drops processed rows past retention, which MUST exceed the longest provider replay window.
	Purge(ctx context.Context, olderThan time.Time) (int, error)
}

type Payments interface {
	Get(ctx context.Context, account, providerPaymentID string) (domain.Payment, bool, error)
	Upsert(ctx context.Context, p domain.Payment) error
	// Update runs fn on the payment under an exclusive lock on (account, provider payment id) and
	// stores what it returns. Two refund reports for one payment, handled by two processors, must
	// not both read the same cumulative figures — the lock is what makes "revoke exactly" hold
	// under concurrency. fn may call Meter.Grant: every call it makes is idempotent, so a failed
	// commit is a retry, not a second effect.
	Update(ctx context.Context, account, providerPaymentID string, fn func(cur domain.Payment, found bool) (domain.Payment, error)) (domain.Payment, error)
	List(ctx context.Context, s domain.Scope, cursor string, limit int) ([]domain.Payment, string, error)
}

type Subs interface {
	Get(ctx context.Context, s domain.Scope) (domain.Sub, bool, error)
	// ApplyIfNewer writes the state only when it is newer than the stored one, by the total order of
	// domain.Sub.Apply (invariant 15). The store holds an exclusive lock on the subscription for the read, the
	// decision and the write, so concurrent appliers converge on the newest state; the order is the domain's,
	// not a second implementation in SQL. A second live subscription for a scope is ErrAlreadyExists (the event
	// is retried until the first one ends); a plan the realm does not sell is ErrUnknownPrice.
	ApplyIfNewer(ctx context.Context, sub domain.Sub, at time.Time) (applied bool, err error)
	// DueForGraceExpiry lists past_due subscriptions whose grace window has ended and whose end has
	// not been announced; MarkGraceLapsed records the announcement, only while it is still the grace
	// that ended (a newer state clears it).
	DueForGraceExpiry(ctx context.Context, now time.Time) ([]domain.Sub, error)
	MarkGraceLapsed(ctx context.Context, sub domain.Sub, at time.Time) (bool, error)
}

// AutoRecharges holds each scope's standing instruction to top up and bounds it. Claim reserves the next
// charge slot atomically: at most MaxPerDay distinct slots per UTC day however many times a trigger fires,
// and while one is in flight another claim RESUMES it, so a duplicated trigger names the same idempotency
// key and the provider charges once. Settle ends the slot: a good charge clears the failure count, a failed
// one adds one, and the third in a row disables the instruction with the reason recorded. The rules are the
// domain's (domain.AutoRecharge.Claim and Settle are pure); a store only makes them atomic.
type AutoRecharges interface {
	Get(ctx context.Context, s domain.Scope) (domain.AutoRecharge, bool, error)
	// Put stores the configuration (never the ledger, so an edit cannot hand out more charges today). Written
	// enabled it clears the failures — its owner has fixed the card; written disabled it records the reason given.
	// An invalid instruction is md.ErrInvalid and is not stored. On PostgreSQL the instruction keeps a balance watch
	// of key auto_recharge in the same transaction — metering's hot function flags a crossing of a watch and its
	// writer publishes billing.balance.low — and a watch of any other key is the host's and is never touched.
	Put(ctx context.Context, a domain.AutoRecharge) error
	// Claim and Settle are the domain's rules under a lock. A scope with no instruction claims nothing (disabled);
	// settling one is md.ErrNotFound.
	Claim(ctx context.Context, s domain.Scope, now time.Time, staleAfter time.Duration) (RechargeClaim, RechargeRefusal, error)
	Settle(ctx context.Context, s domain.Scope, slot RechargeClaim, ok bool, reason string, now time.Time) (disabled bool, err error)
}

// The driving ports behind the host-facing routes. Checkout has no Meter: it cannot grant. Subscriptions never
// writes status: it calls the provider and waits for the webhook (invariant 12). PlanChangeGuard is the
// entitlement layer's answer to "would this take a quota below what the scope uses now" (FR-029).
type Checkout interface {
	Start(ctx context.Context, req CheckoutRequest) (redirectURL string, err error)
	Portal(ctx context.Context, scope domain.Scope, account, returnURL string) (portalURL string, err error)
	Fulfilment(ctx context.Context, scope domain.Scope) (Fulfilment, error) // none | processing | granted | failed
}
type Subscriptions interface {
	Change(ctx context.Context, scope domain.Scope, planCode string) error
	CancelAtPeriodEnd(ctx context.Context, scope domain.Scope) error
}
type PlanChangeGuard interface {
	CheckChange(ctx context.Context, scope domain.Scope, from, to domain.Plan) error // domain.ErrBelowUsage when it cannot be made now
}

// The two driving ports of the webhook flow. Handle answers one provider delivery with a status; Tick leases the events
// that are due, processes them, and retries parked deliveries — safe to run from many processors at once.
type WebhookIngress interface {
	Handle(ctx context.Context, provider, account string, body []byte, header http.Header) IngressResult
}
type WebhookProcessor interface {
	Tick(ctx context.Context) error
}
```

---

## Checkout

`Checkout.Start` sells what the catalogue has, at the catalogue's price, and nothing else: the request names a
plan code and a currency, never an amount. It resolves the provider price for one account and currency (and
verifies that what came back is the plan asked for, live, in the realm), maps the customer to its scope
**before** the buyer reaches the provider — `PaymentProvider.EnsureCustomer` creates the provider's customer
once, idempotent on `customer:<account>:<scope tag>`, `Customers.Upsert` keeps the first writer's id, and the
checkout reads the winner back — runs the `TaxStrategy` (whose failure fails the checkout closed), and only then
asks the provider for a hosted page. The webhook for the payment that follows can therefore name a customer we
know. Return URLs must be `https` pages on a configured host (host as the URL spells it, so a port is part
of it); credentials in a URL and look-alike hosts are refused. The mode follows the plan (`payment` for a
pack, `subscription` for a recurring plan, `setup` for saving a method and selling nothing). `Checkout` is
given no `Meter`: it cannot grant.

`Checkout.Fulfilment` is the server's report on the scope's latest payment — **none** (no recent payment),
**processing** (pending), **failed** (with the provider's reason), **granted** (what the books were given) —
read from the record the processor writes, within a window (30 minutes) of the payment beginning. It is the only
sanctioned way for a UI to learn that credits arrived.

### Plan changes and cancellation

`Subscriptions.Change` moves a scope's live subscription to another recurring plan under the deployment's
`PlanChangePolicy` (D11): `at_period_end` (default) asks the provider for the change when the paid period
ends; `immediate_prorate` and `immediate_no_prorate` ask for it now, with that policy — proration is the
provider's math, since it owns the invoice. A change that takes effect **now** is first put to the entitlement
layer's `PlanChangeGuard`: one that would take a quota below what the scope uses is refused with
`ErrBelowUsage`, and so is one the guard cannot answer (an outage denies; it never permits). One deferred to
the period's end is allowed whatever the usage, because metering's ceilings answer for the new plan when it
begins. The currency is the one of the subscription's latest invoice. A change to the plan the scope is on, to a
one-time pack, to a plan nobody sells, or for a scope with no live subscription is refused before the provider is
asked. Neither `Change` nor `CancelAtPeriodEnd` writes subscription state: the local status moves when the
resulting webhook is processed (invariant 12).

### Persistence

The stores are PostgreSQL (`billing/adapters/driven/postgres`), and each is held to a conformance suite run against
an in-memory twin as well, so the fakes the processor suites stand on cannot be kinder than the database.
The guarantees are the database's: the `UNIQUE (provider_account, provider_event_id)` on `payment_events` is the
replay guard; `CHECK`s keep a payment from refunding more than it cost, revoking or clawing back more than it
granted; a unique partial index allows one live subscription per scope; and a transaction-scoped advisory lock on
a payment's or a subscription's name is what makes `Payments.Update` and `Subs.ApplyIfNewer` exclusive — including
the call that creates the row. `Inbox.Lease` takes one short transaction under a lock shared by every processor, so
"this customer is free" and "this event is due" are decided one at a time. A provider customer maps to one scope
and never moves (`Customers.Upsert` keeps the first writer's id and moves only the email). The catalogue is read
on every call, with no cache: a price written a moment ago is sold now; a plan since retired is no longer sold,
but `PlanForPrice` still recognizes it, so a customer who paid a minute before it was withdrawn gets what they
bought. `rejected` is the status of a parked delivery that failed verification or turned out to be a duplicate.

## Webhook processing

### Stage 1 — ingress (`paymentd`, synchronous, provider-facing)

```text
provider → POST /webhooks/{provider}/{account}
  1. Read the RAW body. Body-size limited; no JSON middleware may touch it first.
  2. Look up the account: unknown or inactive → 404. The route names the account, so the secret
     and the realms it may touch are known before anything is parsed.
  3. PaymentProvider.VerifyWebhook(account, raw, headers)
        invalid → 400 invalid_signature, billing_webhook_verify_failed_total{provider}, SECURITY
                  event, STOP. Never 2xx, never partial processing.
        unavailable (PayPal's API down) → Inbox.Park(raw) → 202. Retried by payment-worker's webhook loop.
  4. Inbox.Persist(normalized event) — an existing (account, event id) is a no-op.
  5. 200. Publish billing.webhook.received as a latency hint.
```

A body over the limit (1 MiB) is `413` before anything else. A route that names no registered provider,
or an account that does not exist, is inactive or belongs to another provider, is `404 unknown_account`
and verifies nothing. A delivery is parked under a name made of the SHA-256 of its bytes
(`parked:<hash>`) — nothing in it may be parsed yet — so the provider's redelivery of the same bytes is
one row. `payment-worker`'s webhook loop (every `PAYMENT_WEBHOOK_TICK_INTERVAL`, one run at a time, at once on start) calls `Ingress.Retry` and `Processor.Tick`: a parked delivery that now verifies becomes an
ordinary event and loses its raw body; one that fails is rejected (a security event, body dropped, never
processed); one that still cannot be verified backs off (30 s doubling, one hour at most). A parked delivery is
verified again as of when it ARRIVED (`ports.WithReceivedAt`), so waiting out an outage longer than the signature
tolerance does not turn a genuine delivery — which the provider was told `202` for and will not resend — into a replay. An event whose
provider or account differs from the route's is an adapter defect, answered `500`, never filed. Metrics
are counted only after the route has resolved to a registered provider and a live account, so no
label is ever keyed on what an unauthenticated caller typed.

Nothing in stage 1 touches money, so it can run on a single transaction's latency and answer every
duplicate `200` without a retry storm. **The only way a verified delivery is lost is a failure
before step 4 commits — and then the provider has not received `200` and retries it.**

### The door and the processes that run it

**The door** (`billing/adapters/driving/webhookhttp`) is mounted by the REST handler as a *webhook* route
(`resthandler.Config.Extra`), which is why it carries no authentication and no CORS and why the registry refuses to start with
either on it. It reads at most `MaxBody`+1 bytes of the raw body — over the limit is `413 payload_too_large`, a body that stopped
short is `400 invalid_request` and is never forwarded — and hands the ingress the body, every header value and the two names the
route carries. Its answer is the ingress's status: a success carries `{}`; anything else is the REST error envelope
`{"error":{"code","message"}}` with the ingress's code; an answer that is not an HTTP status is a `500`, so a defect can never
be read as an acknowledgement. Nothing of a delivery is logged, and a `5xx` names only the provider and the account.

**Secrets.** An account holds *references*: `env:NAME` (read from the environment of the process that needs it — `paymentd`
verifies, `payment-worker` calls the provider and retries parked deliveries) and `file:/absolute/path` (a regular file under
64 KiB; one trailing line ending is dropped). Both are resolved at the moment of use and never cached. Any other scheme, an unset
or empty value, a relative or unclean path and a non-regular file are refused with an error that carries no secret material.

**The worker's loops.** `payment-worker` runs `Processor.Tick`, `Ingress.Retry` and the inbox's retention purge, each at once on
start and then every `PAYMENT_WEBHOOK_TICK_INTERVAL` (retention: hourly) after the last run ended, so a stage never overlaps
itself; a failed run is logged with its stage and tried again and stops no other stage. It also joins `<prefix>.balance.low.>` as
the group `payment-autorecharge` and hands each message to the auto top-up handler; a handler error is not acknowledged, so the
bus delivers it again. The group's name is part of the deployment. A worker that cannot join exits `1` naming the subject. On a
stop the context is cancelled and `Run` returns only when the stage in hand has finished.

**Subjects** follow `PAYMENT_SUBJECT_PREFIX` (default `billing`) throughout: `<prefix>.payment.<tag>`, `<prefix>.entitlement.<tag>`,
`<prefix>.webhook.received` and the `<prefix>.balance.low.<tag>` that auto top-up accepts.

### Stage 2 — processor (`payment-worker`, from the inbox)

```text
Inbox.Lease(n) → for each event, in provider CreatedAt order per customer:
  a. Customers.ResolveScope(account, object.ProviderCustomerID)
        unknown → MarkProcessed(ignored). NOT an error: a provider account can carry customers this
                  deployment never created (another environment, a manual test).
        a realm the account does not serve → MarkProcessed(ignored) + security metric.
  b. Dispatch on event.Type:
       payment_succeeded   → plan := Catalog.PlanForPrice(object.ProviderPriceID)
                             Payments.Upsert(succeeded, credits_granted = plan.CreditAllotment, pool)
                             Meter.Grant{pool: plan.Pool, amount: plan.CreditAllotment,
                                         idem_key: provider_payment_id (or invoice id),
                                         reason: purchase | subscription_grant | auto_recharge}
       payment_failed      → Payments.Upsert(failed); for a subscription: FetchSubscription, then
                             Subs.ApplyIfNewer(past_due, GraceUntil = now + DunningGraceDays);
                             an auto top-up failure increments consecutive_failures (§ Auto top-up)
       payment_refunded    → revoke := clawback(payment, object.AmountRefunded)     (§ Partial refunds)
                             Meter.Grant{pool: payment.Pool, amount: −revoke, source_key: payment.IdemKey,
                                         idem_key: "refund:" + provider_refund_id, reason: refund}
                             Payments.Upsert(refunded_minor, credits_revoked, status)
       subscription_*      → sub, at := FetchSubscription(id)    ← the provider's CURRENT state
                             Subs.ApplyIfNewer(sub, at); if applied → billing.entitlement.<tag>
       dispute_*           → § Dispute lifecycle
       ignored             → MarkProcessed(ignored)
  c. MarkProcessed(processed). Publish billing.payment.<tag>.
  d. On error: MarkFailed with exponential backoff; past MaxAttempts → status dead, page.
```

A failed attempt keeps its reason on the row and is retried on a backoff (15 s doubling, 30 minutes at
most); past the attempt cap (5) the event is **dead**, `billing_webhook_dead_total` pages, and nothing
touches it again until a human does. An event the deployment does not own is **ignored**, never failed —
an unsubscribed type, a customer no `Customers` row names (a provider account can carry customers this
deployment never created), a price the catalogue does not sell (never a default plan) — each with its
reason on `billing_webhook_ignored_total`; a customer in a realm the account does not serve is ignored
and counted on `billing_webhook_foreign_realm_total`, which is a security signal. A price whose plan lives
in a realm other than the customer's fails loudly: money was taken for something the customer's realm does
not sell. An event that names a payment and not a customer (a dispute) takes its scope from that payment's
own record. `Lease` hands a customer's events out one at a time, oldest provider timestamp first, so two
processors never work one customer's money at once.

**Every step is idempotent**, which is what makes the lease-and-retry model safe: `Meter.Grant` is
keyed on a provider id (unique per realm and op — one payment can mint once, for one scope),
`Payments.Upsert` on `(account, provider_payment_id)`, `Subs.ApplyIfNewer` on the provider
timestamp. A processor that crashes mid-event leaves a lease that expires; the next lease re-runs
the event and every completed step is a no-op.

**Subscriptions.** A subscription event says THAT something changed; the processor fetches the
provider's current object (`FetchSubscription`) and writes it only if it is newer than the one held
(`Subs.ApplyIfNewer`), so no delivery order decides state. The adapter has already mapped the provider's
price to a plan code through the catalogue; an object with no plan code is ignored (`unknown_price`),
never guessed. When the provider says `past_due` the grace window opens at **the provider's timestamp**
of that object plus the dunning grace — not at our clock, so a retry computes the same instant — and a
newer notice of the same trouble carries the window forward instead of moving it; any other status
clears it. Entitlements lapse by the clock (`Sub.HoldsEntitlements`); every tick also lists the windows
that have ended and announces each once on `billing.entitlement.<tag>` (`reason: grace_expired`) so
caches evict at that instant, recording the announcement (`grace_lapsed_at`) after making it. A change
that was applied is announced on `billing.entitlement.<tag>` (`reason: subscription`); a cancellation also
on `billing.payment.<tag>` (`event: canceled`). A failed payment is recorded and moves no credits; one that
follows a success does not undo it; one paid at last under the same payment id is granted then.

### Partial refunds

A payment of `amount_minor` that granted `credits_granted` has, after a refund event reporting the
**cumulative** `refunded_minor`:

```text
revoke_total = min(credits_granted, ceil(credits_granted × refunded_minor / amount_minor))
this_refund  = revoke_total − credits_revoked          (never negative)
```

Computing the *cumulative* figure and subtracting what was already revoked means three partial
refunds revoke exactly what one refund of the same total would — no rounding drift, never more than
was granted, and a replayed refund event revokes nothing further. The product is computed in 128 bits
(`billing/domain.RevokeTotal`, held to `math/big` by a fuzz target), so it cannot wrap. Providers do not
order deliveries, so a report **older** than one already applied lowers nothing: the stored
`refunded_minor` is `max(report, stored)` and the report revokes nothing. A refund that fiat cannot
explain — more than was paid, in another currency, negative — is `ErrInvalid`, which the inbox retries
into a dead event and a page; it is never a number. `refunded_minor ≤ amount_minor`
and `credits_revoked ≤ credits_granted` are database constraints.

**What expiry already took (invariant 20).** Under `CreditExpiry=lots_fefo` the payment's credits are a
lot, and the lot may have lapsed unspent. Revoking those credits again would take them from the customer
twice — once by expiry, once by the refund — out of whatever other purchase still holds credits. So before
it computes `this_refund` the processor asks `LotReader.LotsOf(scope, payment.Pool, payment.IdemKey)` and
caps the cumulative figure at what expiry has not taken:

```text
revoke_total = min(revoke_total, credits_granted − Σ lot.Expired)
```

Credits the customer *spent* are still revoked: what a refund past the spend does is
`NegativeBalancePolicy`'s decision (metering invariant 12), not the processor's. The clawback names the
payment it reverses (`source_key`), so the books empty that payment's own lot before any other. With expiry
off there are no lots, `LotsOf` is empty and the cap is a no-op.

A refund or a dispute names a payment, and the payment is the authority on whose it is: an event whose
customer is not the payment's owner fails loudly rather than moving another scope's credits. Both run
under the payment's lock (`Payments.Update`), read `LotReader.LotsOf` for what expiry has taken — an
outage of the reader is an error, never a zero — and are announced on `billing.payment.<tag>`
(`refunded`, `disputed`, `charged_back`, `succeeded`) only when they changed something, **before** the
event is marked processed: a bus that blinked retries the event, every step of which is idempotent, so a
notification is never lost. A clawback whose payment commit failed after the grant was made is recomputed
identically on the retry and replays under the same key.

### Dispute lifecycle

| Event | Payment | Credits |
|---|---|---|
| `dispute_opened` | `disputed`, `dispute_status = open` | none: funds have not moved. A host that wants to freeze usage uses `block_and_flag` via an admin action |
| `dispute_funds_withdrawn` | `charged_back` | `Meter.Grant{amount: −(credits_granted − credits_revoked − Σ lot.Expired), source_key: payment.IdemKey, idem_key: "dispute:" + id + ":withdrawn", reason: chargeback}` — subject to `NegativeBalancePolicy`. What it took is kept in `dispute_clawed` |
| `dispute_funds_reinstated` / `dispute_closed{won}` | the status the refunds alone give (`succeeded`, `partially_refunded`, `refunded`), `dispute_status = won` | `Meter.Grant{amount: +dispute_clawed, idem_key: "dispute:" + id + ":reinstated", reason: chargeback_reversal}` — only if a withdrawal was applied |
| `dispute_closed{lost}` | `dispute_status = lost` | none further |

A chargeback is reversible because the provider says so; a system that only knows how to take
credits back turns every won dispute into a support ticket. Deliveries are unordered, so the two
phases converge in either order: a reinstatement that arrives first records the verdict and grants
nothing, and a withdrawal that then arrives takes nothing; a replay of either changes nothing; an
opening that arrives after the money moved does not un-charge the payment back. The chargeback is
capped by expiry exactly as a refund is (invariant 20): credits that lapsed unspent are not taken
a second time.

### Auto top-up

When a scope's pool crosses below its `auto_recharge.threshold`, the hot function flags the
crossing intent, the writer publishes `billing.balance.low.<tag>` (metering owns the mechanism,
billing owns what it triggers), and the recharge handler:

1. checks the row is enabled, under `max_per_day`, and not disabled by failures;
2. calls `ChargeSaved` with `idem_key = recharge:<realm>/<scope>:<day>:<n>` — so a duplicated
   trigger cannot charge twice, and a day cannot exceed `max_per_day` distinct charges;
3. does **nothing else**: credits arrive when the `payment_succeeded` webhook is processed, like any
   purchase, with `kind = auto_recharge`.

The bound on step 1 is a pure rule, `domain.AutoRecharge.Claim`, which every store applies and a fuzz target holds to
its promises. A day is the UTC day, and it never goes back: a clock that steps back across midnight keeps counting the
later day, so it cannot hand out a day's charges twice. A day hands out at most `max_per_day` slots, numbered 1, 2, 3…, and
the key of a slot (`domain.RechargeKey`, read from the right so it is unambiguous however many colons a scope id has) is
what the provider de-duplicates on. While a slot is in flight and younger than the staleness window, a claim **resumes**
it — the same slot, so the same key, and the provider charges once however many replicas see the trigger — and resuming
costs nothing, so it is allowed at the maximum. A slot claimed and never settled is replaced once it is stale, but it was
spent and still counts, so abandoning slots cannot get round the maximum. A new UTC day starts from slot 1 and drops
whatever was in flight on the day before. **A settlement names its slot** and releases the slot in flight only if it is that
one: webhooks are late and replayed, and slot one's arriving after slot two was claimed must not release slot two, or a
duplicate trigger would claim a third slot under a new key and charge the customer twice for one low balance. The outcome
counts either way: a good charge clears the count of failures, a failed one adds one, and **the third in a row switches the instruction off** with the reason recorded (a default if the provider
gave none, so the customer can always be told) and reports that this settling did it, so the news is made once. Nothing
switches an instruction back on but its owner writing a new one.

**The handler** (`billing/app.AutoRecharge`) takes `billing.balance.low.<tag>` as a **hint** and checks it. The message —
`{realm, scope, watch, seq}` — is read only if its subject, its realm and its scope tag agree, and a message that cannot be read
is an error that takes no slot. The instruction must exist, be enabled, and be for the pool whose watch was crossed (a host's own
watch on another pool is ignored); and the balance is **read** and must be below the threshold, so a duplicate delivered after a
recharge already restored it charges nothing. Only then is a slot claimed. The price is the catalogue's today for the plan and
currency the instruction names — never an amount the instruction holds (invariant 4) — and the plan must be a credit pack. The
charge is `ChargeSaved` under the slot's key, and **the handler grants nothing**. A charge the provider **refused**
(`domain.ErrChargeDeclined`), a plan nobody sells, an account that is off, a customer the account has never seen, or a provider
that cannot charge a saved method fails the slot — acknowledged, not retried, since resending would only fail again — and counts
toward switching the instruction off. Any other error leaves the outcome **unknown**: the message is not acknowledged, the slot
stays held, and the redelivery resumes it under the same key. An accepted charge leaves a **pending payment** of kind
`auto_recharge` keyed on the slot (its idempotency key is the slot's key), waiting for its webhook; if the webhook already
arrived, the payment the processor made is left as it is and the slot is settled at once.

**The processor** fulfils that payment like any purchase — the grant is keyed on the slot, with reason `auto_recharge` — and its
outcome settles **its** slot, once: the payment's key names the slot, and only the first outcome of a payment reaches the
settlement, so a replayed failure counts once and a success that arrives after its slot went stale does not release the slot that
replaced it. That is deliberately at-most-once: an undercount only delays switching an instruction off, and an overcount would
switch a good card off, so a settlement that cannot be made is logged and does not fail the event. Every outcome of a trigger is
counted in `billing_autorecharge_total{result}`.

Three consecutive failures disable the row (`disabled_reason`) and emit `billing.payment.<tag>`
with `failed`, so the host can ask the customer to update their card. The saved method is set up
through `CreateCheckout` in `setup` mode or the provider portal; this service stores only the
provider's reference.

### The Stripe adapter

`billing/adapters/driven/stripe` speaks Stripe's HTTP API directly (a few form-encoded calls) at API version
2024-06-20, to which a webhook endpoint must be pinned. **A payment is keyed on its payment intent**, the one
id every event about it carries — the paid invoice (`invoice.paid`, `invoice.payment_succeeded`; the invoice's own
id when there is no intent), the refunded charge (`charge.refunded`: `amount_refunded` is already cumulative, and
the newest refund's id names the clawback), the four dispute events (which carry no customer: the processor takes
the scope from the payment) and an off-session payment intent. A one-time checkout is created with
`invoice_creation`, so it too is fulfilled by `invoice.paid`; a payment intent that belongs to an invoice is
ignored because the invoice fulfils it once, and **`checkout.session.completed` is ignored, always**: a checkout return
never fulfils. The provider's current subscription is dated by the `Date` of Stripe's own response, and a price the
catalogue does not sell comes back with no plan code. A plan change now is a subscription update with Stripe's
proration (or none); a change at the period's end is a schedule that keeps the running phase, adds the new price
after it and releases itself. A secret is read from its reference at each call and is scrubbed from every error.
The event bodies the tests use are built from the documentation, not captured: a sandbox rehearsal is still owed.

### The Polar adapter

`billing/adapters/driven/polar` speaks Polar's HTTP API directly (a few JSON calls); an account in `live` mode is
served by `api.polar.sh` and one in `test` mode by `sandbox-api.polar.sh`, and any other mode is refused before a call
is made. **The provider price id the engine stores is the Polar product id.** A webhook carries Standard Webhooks
headers, `webhook-id`, `webhook-timestamp` and `webhook-signature` (`v1,<base64>` entries, space-separated), and the
signature is HMAC-SHA256 over `id.timestamp.body` — so the message id and the time are authenticated along with the
body, and a captured delivery can be replayed neither as a new event nor as a fresh one. A secret made before
8 September 2026 is keyed with the UTF-8 bytes of the whole `whsec_…` string and a later one with the base64 after the
prefix, decoded; the dashboard does not say which, so both are tried, and a key that is empty is never one. **The
event id is the signed `webhook-id`** (the body has none), and the event is dated by its own `timestamp`, or by the
signed header when it carries none.

**A payment is keyed on the order.** `order.paid` is a payment; `order.created` is one only if the order is already
paid, and an `order.paid` for an order that is not paid is an error, never a grant. They name one order, so they are one
grant. The amount is `total_amount`, what the customer was charged with the sales tax in it, and `tax_amount` is that
tax. **A refund is the order's cumulative total**: `refunded_amount` (the money, before tax) plus `refunded_tax_amount`
(its tax), because the customer got back both and the total they paid includes both; it arrives as `order.refunded` or
as an `order.updated` that carries a refund, and a refund event that refunded nothing is ignored. A subscription's
events (`created`, `active`, `updated`, `canceled`, `uncanceled`, `past_due`, `paused`, `resumed`, `cycled`) are all
updates and `subscription.revoked` is its end; the state applied is `FetchSubscription`'s, dated by the `Date` of
Polar's response. Checkouts, refund objects, benefit grants and customers are ignored — a checkout never fulfils, and
the order a refund belongs to carries the cumulative total. Polar is the merchant of record: it remits the tax and
settles chargebacks itself, so it sends no dispute events, and a failing renewal is `subscription.past_due`, not a
failed-payment event.

**What Polar cannot do, it says with `domain.ErrUnsupported` and does not call Polar:** a checkout that only saves a
payment method (mode `setup`), `ChargeSaved` (so auto top-up is unavailable on a Polar account), and a plan change
"now" without proration — Polar's only such option restarts the billing cycle and invoices the full price of the new
plan, which is not that policy. A plan change at the period's end is `proration_behavior: next_period` and a prorated
one `prorate`. **Two Polar limits the host should know:** an email address is unique within a Polar organization, so a
second scope that bills to an address already used on the account gets an error from `EnsureCustomer` (never the
first scope's customer); and a checkout cannot be made idempotent, so a double-clicked buy button makes two checkout
pages, which fulfil nothing until an order is paid. The checkout tells Polar the catalogue's price and currency as well
as the product: Polar honours them for a product whose price is set by the caller and ignores them for a fixed one. An
unresolvable secret is a misconfiguration (never an invalid signature), and an access token is scrubbed from every
error. The event bodies the tests use are built from the documentation, not captured: a sandbox rehearsal is still owed.

### The PayPal adapter

`billing/adapters/driven/paypal` speaks PayPal's REST API directly (a few JSON calls); an account in `live` mode is
served by `api-m.paypal.com` and one in `test` mode by `api-m.sandbox.paypal.com`, and any other mode is refused before a
call is made. An account's `SecretRef` resolves to `client_id:client_secret` and its `WebhookSecretRef` to the **id of the
webhook** as configured in PayPal's developer portal, which PayPal needs to verify a delivery. The access token is fetched
when needed, kept until a minute before it expires, replaced once (never in a loop) if PayPal withdraws it, and never
appears in an error, nor does the client secret.

**Verification is PayPal's, by a call** (`POST /v1/notifications/verify-webhook-signature`): PayPal signs with a certificate
the adapter never sees, so the five `PAYPAL-*` headers, the webhook id and the event are sent and PayPal answers
`SUCCESS` or not. Nothing is read out of the body until it does. **The event reaches PayPal byte for byte** — PayPal checks
the signature of exactly what it sent — spliced into the request rather than decoded and encoded again; and so it is sent
**only if it is exactly one JSON value**, because anything else could carry fields of its own into the request (a
`webhook_id` of the sender's choosing, so that PayPal vouches for an event it signed for the sender's own webhook). A
delivery with a header missing, or a transmission time that is not a time or is outside the tolerance **measured from when
the delivery arrived** (`ports.ReceivedAt`: a delivery parked through an outage is verified as of its arrival), is refused
without a call, not even for a token. A verdict other than `SUCCESS`, and a request PayPal rejects (400, 422), are
`ErrInvalidSignature`. **Everything else — PayPal down, slow or rate-limiting, a token it refuses, a cancelled context — is
`ErrVerificationUnavailable`**: the delivery is neither rejected (the provider was told `202` and will not resend it) nor
trusted, and the ingress parks it with its raw body. A reference that does not resolve, or credentials that are not
`client_id:client_secret`, are our misconfiguration and neither.

**A capture carries neither the customer nor the product**, so the order or subscription is made with a `custom_id` of
`<customer>~<product>` (the customer being what `EnsureCustomer` made: PayPal has no customer object, so it is an opaque id
made from the key, the same for the same key and carrying nothing of the scope) and a payment whose `custom_id` is not of
that form — a sale of the PayPal account's that this service did not make — is **ignored and grants nothing**. A payment is
keyed on the **capture** (`PAYMENT.CAPTURE.COMPLETED`; `DENIED` and `DECLINED` are failures); a subscription's cycle is a
`PAYMENT.SALE.COMPLETED`, which names only its subscription, so the subscription is read from PayPal for its customer and its
plan — and if PayPal cannot be asked, the delivery is parked, not guessed at. **A refund is the total refunded from the
capture to date** (`seller_payable_breakdown.total_refunded_amount`), against the capture its `up` link names. **Every phase of
a dispute shares one id per transaction** (`dp:<transaction>`), because PayPal's own dispute id is on only some of the events
of one dispute: `CUSTOMER.DISPUTE.CREATED` opens it, `PAYMENT.CAPTURE.REVERSED` is the funds withdrawn, and
`CUSTOMER.DISPUTE.RESOLVED` closes it won or lost by PayPal's `outcome_code` — an outcome the adapter has not been told how to
settle is an error, never a guess. Subscription events update or end it (`CANCELLED`, `EXPIRED`); `PAYMENT.FAILED` is a
failed payment of the subscription. An approved order is ignored: a checkout return never fulfils. Amounts are decimal
strings, parsed without floats in the currency's own exponent (`HUF`, `JPY` and `TWD` have none) and held by two fuzz targets
to arbitrary-precision arithmetic.

**A payment checkout is an order with `intent: CAPTURE` and `processing_instruction: ORDER_COMPLETE_ON_PAYMENT_APPROVAL`**,
so PayPal captures when the buyer approves and no call of ours sits between approval and payment; without it a buyer could
approve and never be charged. A subscription checkout is a billing plan the buyer approves. Both are idempotent on
`PayPal-Request-Id`. **What PayPal cannot do for a server, it says with `domain.ErrUnsupported` and does not call PayPal:** a
checkout that only saves a payment method (mode `setup`) and `ChargeSaved` (so auto top-up is unavailable on a PayPal
account), a customer portal (PayPal has none), cancelling at the period's end (PayPal cancels at once), and changing a plan
(PayPal's revision needs the buyer's approval at a link the port has no way to hand back). A PayPal subscription is
cancelled by the buyer at PayPal, which arrives as `BILLING.SUBSCRIPTION.CANCELLED`.

**Known limits.** A refund of a subscription's payment arrives as `PAYMENT.SALE.REFUNDED`, which carries no cumulative
total; it is ignored, not guessed, and an operator reverses those credits. A dispute names a transaction and not a
customer, so a dispute over a sale this service did not make is retried until it is dead. **What the tests cannot show:** the
fake PayPal signs with a private scheme, so they show that the adapter forwards a delivery faithfully and obeys the verdict,
not that PayPal's cryptography holds; and the event bodies are built from the documentation, not captured — the shape of
`PAYMENT.CAPTURE.REVERSED`, the presence of `total_refunded_amount` on every refund, and whether a wallet order is captured
on approval are exactly what a sandbox rehearsal before the first real order must confirm. If a wallet order is not
captured on approval, nothing is charged and nothing is granted: the failure is safe, and visible as orders approved and never
paid.

### Per-provider notes

| Provider | Verification | Notable events | Gotcha |
|---|---|---|---|
| **Stripe** | `Stripe-Signature`: HMAC-SHA256 over `timestamp.payload`, constant-time, with a tolerance window | `invoice.paid`, `checkout.session.completed`, `invoice.payment_failed`, `customer.subscription.updated\|deleted`, `charge.refunded` (`amount_refunded` is cumulative), `charge.dispute.created\|funds_withdrawn\|funds_reinstated\|closed` | `invoice.paid` **and** `checkout.session.completed` can both arrive for one purchase: key the grant on the **payment/invoice id**, never the session. Deliveries are unordered — hence `FetchSubscription` |
| **Polar** | Standard Webhooks: HMAC-SHA256 over `webhook-id.webhook-timestamp.body` (either of the secret's two keys), constant-time, with a tolerance window | `order.paid`, `order.created`, `order.refunded`, `order.updated`, `subscription.*` | the same order is named by `order.created`, `order.paid` and `order.updated` — key the grant on the **order id**; `refund.created` and `benefit_grant.*` are ignored, and `refunded_amount` is before tax |
| **PayPal** | **not a local HMAC** — a call to `verify-webhook-signature` with the transmission headers and `webhook_id`; the event is sent byte for byte and only if it is one JSON value | `PAYMENT.CAPTURE.COMPLETED\|REFUNDED\|REVERSED`, `PAYMENT.SALE.COMPLETED`, `BILLING.SUBSCRIPTION.*`, `CUSTOMER.DISPUTE.*` | verification does I/O: a PayPal outage means deliveries can be neither verified nor rejected, so they are **parked** with their raw body and retried — never processed optimistically, never dropped |

---

## Invariants every implementation MUST uphold

1. **Verify before anything.** No parse, no DB read, no log of the payload, no metric keyed on its contents — until the signature checks out on the raw body, in constant time, within a timestamp tolerance.
2. **Fulfilment is webhook-driven.** No code path grants credits on a checkout return, a client callback, a polling success or an off-session charge's synchronous response.
3. **Credits are created only through `Meter.Grant`.** The fiat layer never inserts a ledger row, never touches a balance key, never computes a new balance.
4. **Never trust client or metadata authority.** Scope, amount, currency and credit quantity come from the **verified provider object** plus this deployment's own `Customers` and `Catalog`. Metadata is correlation, never authority — an attacker who can buy something from you can produce a validly signed event naming someone else's scope.
5. **Persist before acknowledging.** A verified delivery is durable in the inbox before `200` is returned; processing happens from the inbox with leases, retries and a dead state that pages. A crash after the acknowledgement can delay a grant; it can never lose one.
6. **A replay answers `200`.** An already-persisted event returns success so the provider stops retrying.
7. **Inbox retention exceeds the provider's replay window.** Default 90 days. Past it, the realm-scoped `credit_idem (realm, 'grant', key)` guard still prevents a second grant — which is exactly why that guard is permanent.
8. **Refunds append, never mutate.** A refund writes a negative grant and updates the payment's cumulative refund fields and status; the original payment and grant rows are otherwise immutable.
9. **A provider SDK lives in exactly one package.** Its own adapter. Anywhere else is a lint failure.
10. **Secrets are server-side only, and referenced, not stored.** Provider secrets come from the environment or a secret manager, named by `provider_accounts.*_ref`; never a client bundle, never a log line, never a database value.
11. **Webhook routes have no CORS and no auth middleware.** They are server-to-server, authenticated by signature.
12. **Subscription status has one writer: the processor** (and the drift sweep, which uses the same `ApplyIfNewer`). A local cancel calls the provider and waits for the resulting event.
13. **A failed payment does not instantly revoke.** `GraceUntil` holds entitlements for the configured grace period, then suspends.
14. **Unverifiable deliveries are parked, never dropped and never trusted.** Their raw body is kept only until verification succeeds.
15. **Delivery order never decides state.** Subscription state is the provider's current object, applied only if it is newer than the stored state. "Newer" is a **total order**: the provider's instant, then — because providers stamp events to the second and `updated` and `deleted` can share one — how final the status is (`canceled` above `paused` above `past_due` above `active`…), then every remaining field in a fixed order (`billing/domain.Sub.Apply`). Any permutation of any set of events therefore converges to one state, and a redelivery of the state already held is not applied, so it is not announced twice.
16. **Partial refunds revoke exactly.** Cumulative arithmetic, rounded up, capped at what was granted; several partial refunds equal one refund of the same total.
17. **A chargeback is reversible.** Funds withdrawn claws back; funds reinstated grants back exactly what was clawed; each is keyed on the dispute id and phase.
18. **Auto top-up is bounded and still webhook-fulfilled.** At most `max_per_day` distinct idempotent charges per scope per day; repeated failure disables it and tells the host; credits arrive only on the verified payment.
19. **An account touches only its realms.** A verified event whose customer resolves to a realm outside the account's `realms` is ignored and counted as a security event.
20. **A refund never claws back what expiry already took.** The clawback names the payment it reverses (`source_key`) and is capped at `credits_granted − Σ lot.Expired`, read from `LotReader.LotsOf`; credits that were spent are still revoked and fall to `NegativeBalancePolicy`. A chargeback is the same clawback by another name and is capped the same way.

---

## Contract-test skeleton

`ProviderContract` runs against **every** adapter; `IngressContract` and `ProcessorContract` run against the app over a fake provider and real stores.

```go
func ProviderContract(t *testing.T, p ports.PaymentProvider, fx Fixtures) {
	ctx, acct := context.Background(), fx.Account()
	t.Run("rejects a tampered body", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		tampered := bytes.Replace(body, []byte(`"amount":1000`), []byte(`"amount":100000`), 1)
		if _, err := p.VerifyWebhook(ctx, acct, tampered, hdr); err == nil { t.Fatal("a tampered body must never verify") }
	})
	t.Run("rejects a missing signature and a stale one", func(t *testing.T) {
		body, _ := fx.SignedPaymentSucceeded()
		if _, err := p.VerifyWebhook(ctx, acct, body, http.Header{}); err == nil { t.Fatal("unsigned must not verify") }
		old, hdr := fx.SignedAt(time.Now().Add(-48 * time.Hour))
		if _, err := p.VerifyWebhook(ctx, acct, old, hdr); err == nil { t.Fatal("a captured payload must not verify forever") }
	})
	t.Run("rejects a body signed for another account", func(t *testing.T) {
		body, hdr := fx.SignedWithSecretOf(fx.OtherAccount())
		if _, err := p.VerifyWebhook(ctx, acct, body, hdr); err == nil { t.Fatal("secrets are per account") }
	})
	t.Run("verifies and normalizes, with the provider's timestamp", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		e, err := p.VerifyWebhook(ctx, acct, body, hdr); mustNoErr(t, err)
		if e.Type != domain.PaymentSucceeded || e.ProviderEventID == "" || e.Object.Amount == nil || e.CreatedAt.IsZero() {
			t.Fatalf("adapter must normalize the provider shape: %+v", e)
		}
		if e.RawHash == "" { t.Fatal("must hash the verified body; the body itself is not stored") }
	})
	t.Run("refund amounts are normalized as CUMULATIVE", func(t *testing.T) {
		e := mustVerify(t, p, acct, fx.SignedSecondPartialRefund(300, 200)) // two refunds: 300 then 200
		if e.Object.AmountRefunded.MinorUnits != 500 { t.Fatalf("want cumulative 500, got %d", e.Object.AmountRefunded.MinorUnits) }
	})
	t.Run("unsubscribed types are Ignored; checkout never grants", func(t *testing.T) {
		if e := mustVerify(t, p, acct, fx.SignedUnknownType()); e.Type != domain.Ignored { t.Fatalf("want Ignored, got %q", e.Type) }
		_, err := p.CreateCheckout(ctx, acct, fx.Checkout()); mustNoErr(t, err)
		if fx.Meter.GrantCalls() != 0 { t.Fatal("CreateCheckout granted credits") }
	})
}

func IngressContract(t *testing.T, h ports.WebhookIngress, fx Fixtures) {
	t.Run("a duplicate delivery is persisted once and answered 200 twice", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		mustStatus(t, h.Handle(ctx, "fake", "acct", body, hdr).Status, 200)
		mustStatus(t, h.Handle(ctx, "fake", "acct", body, hdr).Status, 200)
		if n := fx.Inbox.Count(); n != 1 { t.Fatalf("persisted %d rows", n) }
	})
	t.Run("ingress never touches money", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		mustStatus(t, h.Handle(ctx, "fake", "acct", body, hdr).Status, 200)
		if fx.Meter.GrantCalls() != 0 { t.Fatal("stage 1 must only persist") }
	})
	t.Run("an unknown account is 404; unavailable verification parks and answers 202", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		mustStatus(t, h.Handle(ctx, "fake", "nope", body, hdr).Status, 404)
		fx.Provider.VerificationDown()
		mustStatus(t, h.Handle(ctx, "fake", "acct", body, hdr).Status, 202)
		if !fx.Inbox.HasParked() { t.Fatal("an unverifiable delivery must be parked, not dropped") }
	})
}

func ProcessorContract(t *testing.T, p ports.WebhookProcessor, fx Fixtures) {
	t.Run("a crash after acknowledgement still grants, exactly once", func(t *testing.T) {
		fx.Persisted(fx.PaymentSucceededEvent("pay-1"))
		fx.Meter.FailNextGrant()                       // the first attempt dies mid-dispatch
		_ = p.Tick(ctx); fx.Clock.Advance(time.Minute); _ = p.Tick(ctx)
		if fx.Meter.Grants("pay-1") != 1 { t.Fatal("the inbox must re-drive the event to exactly one grant") }
	})
	t.Run("concurrent processors grant once", func(t *testing.T) {
		fx.Persisted(fx.PaymentSucceededEvent("pay-2"))
		parallel(8, func() { _ = p.Tick(ctx) })
		if fx.Meter.Grants("pay-2") != 1 { t.Fatal("leases must keep two processors off one event") }
	})
	t.Run("metadata cannot redirect the grant to another scope", func(t *testing.T) {
		fx.Persisted(fx.PaymentSucceededWithMetadata("pay-3", map[string]string{"scope_id": "victim"}))
		_ = p.Tick(ctx)
		if fx.Meter.LastGrant().Scope.ID == "victim" { t.Fatal("scope came from metadata (invariant 4)") }
	})
	t.Run("partial refunds revoke exactly, cumulatively", func(t *testing.T) {
		fx.GivenSucceededPayment("pay-4", 1000 /* cents */, 10_000_000 /* credits */)
		fx.Persisted(fx.Refunded("pay-4", 333)); _ = p.Tick(ctx)
		fx.Persisted(fx.Refunded("pay-4", 666)); _ = p.Tick(ctx)   // cumulative
		fx.Persisted(fx.Refunded("pay-4", 666)); _ = p.Tick(ctx)   // a replayed report
		if got := fx.Payments.Row("pay-4").CreditsRevoked; got != 6_660_000 { t.Fatalf("revoked %d, want 6,660,000", got) }
		if fx.Payments.Row("pay-4").Amount.MinorUnits != 1000 { t.Fatal("the original amount was mutated") }
	})
	t.Run("a refund does not claw back credits that expiry already took", func(t *testing.T) {
		fx.GivenSucceededPayment("pay-7", 1000, 10_000_000)
		fx.GivenLapsed("pay-7", 10_000_000)                          // its lot expired unspent: Σ lot.Expired = granted
		fx.Persisted(fx.Refunded("pay-7", 1000)); _ = p.Tick(ctx)
		if got := fx.Payments.Row("pay-7").CreditsRevoked; got != 0 { t.Fatalf("revoked %d credits the customer had already lost (invariant 20)", got) }
		if fx.Meter.Grants("refund:pay-7") != 0 && fx.Meter.LastGrant().SourceKey != "pay-7" { t.Fatal("a clawback must name the payment it reverses") }
	})
	t.Run("a won dispute reverses its chargeback", func(t *testing.T) {
		fx.GivenSucceededPayment("pay-5", 1000, 10_000_000)
		fx.Persisted(fx.Dispute("pay-5", "dp_1", "funds_withdrawn")); _ = p.Tick(ctx)
		fx.Persisted(fx.Dispute("pay-5", "dp_1", "funds_reinstated")); _ = p.Tick(ctx)
		if net := fx.Meter.NetGranted("pay-5"); net != 10_000_000 { t.Fatalf("net after a won dispute = %d", net) }
	})
	t.Run("an older subscription event never overwrites a newer state", func(t *testing.T) {
		fx.Persisted(fx.SubscriptionEventAt("sub-1", "canceled", t0.Add(time.Hour)))
		fx.Persisted(fx.SubscriptionEventAt("sub-1", "active", t0))     // delivered late
		_ = p.Tick(ctx)
		if fx.Subs.Row("sub-1").Status != domain.Canceled { t.Fatal("delivery order decided state (invariant 15)") }
	})
	t.Run("a failed renewal sets a grace window, not a revocation", func(t *testing.T) {
		fx.Persisted(fx.PaymentFailedForSubscription("sub-2")); _ = p.Tick(ctx)
		if s := fx.Subs.Row("sub-2"); s.Status != domain.PastDue || s.GraceUntil == nil { t.Fatal("invariant 13") }
	})
	t.Run("an event for a realm the account does not serve is ignored", func(t *testing.T) {
		fx.Persisted(fx.PaymentSucceededForCustomerInRealm("pay-6", "other-realm")); _ = p.Tick(ctx)
		if fx.Meter.GrantCalls("pay-6") != 0 { t.Fatal("invariant 19") }
	})
}
```

---

## Security checklist

- [ ] Webhook signature verified on the raw body, in constant time, with a timestamp tolerance, **before any side effect**, with the **route's account** secret.
- [ ] Webhook route: body-size limit, no CORS, no JWT/session middleware, no body-rewrite before verification; unknown accounts `404`.
- [ ] Provider secrets in the environment or a secret manager, referenced by `*_ref`; never client-exposed, logged or stored.
- [ ] `checkout` / `portal` / `cancel` / plan-change / auto top-up settings are authorization-gated by the host.
- [ ] Scope, amount and credits resolved from the verified object — never the request body, a query parameter, or `metadata`.
- [ ] No card data anywhere; `payload_hash` instead of bodies, except a parked, unverifiable body until it verifies.
- [ ] `Grant` callable only by the processor and audited admin tooling (metering invariant 13).
- [ ] Each provider account's `realms` is as narrow as possible; cross-realm resolutions are refused.

## Adoption checklist (per host product)

- [ ] **Provider account row(s)** — `provider_accounts` with secret *references*, the realms each serves, live vs test.
- [ ] **Plans defined as data** — `plans` + `plan_prices` + `plan_provider_prices` per account and currency; each plan's `pool`.
- [ ] **Webhook URL registered** — `/webhooks/{provider}/{account}` per account, and its delivery log checked after the first live payment.
- [ ] **Inbox alerts wired** — `billing_webhook_inbox_age_seconds` and dead events page a human.
- [ ] **Tax strategy chosen** — `provider_managed` unless the host already owns tax.
- [ ] **Plan-change, dunning and negative-balance policies chosen.**
- [ ] **Auto top-up decided** — off, or enabled per scope with a threshold, a pack and a `max_per_day`.
- [ ] **Sandbox rehearsal done** — purchase, renewal, failed payment, partial refunds, a dispute won and lost, a replayed webhook and a processor killed mid-event, all in test mode, before a real card.

## Non-goals (stays outside this layer, by design)

- **Tax computation, filing, and merchant-of-record duties** — a provider or a specialist service, named via `TaxStrategy`.
- **Invoice rendering and dunning emails** — the provider's hosted invoices and the host's notification system; this layer emits facts through [outbound-events.md](./outbound-events.md).
- **Credit consumption** — [metering-ports.md](./metering-ports.md).
- **Feature entitlement enforcement** — [entitlement-ports.md](./entitlement-ports.md).
- **Card data, PCI scope** — hosted checkout and hosted portal, always.
