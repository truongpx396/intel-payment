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
	// on the verified payment webhook (invariant 18).
	ChargeSaved(ctx context.Context, acct domain.ProviderAccount, in OffSessionInput) (providerPaymentID string, err error)
}

type CheckoutInput struct {
	Scope           domain.Scope
	Mode            string // "payment" | "subscription" | "setup"
	PlanCode        string
	Currency        string
	ProviderPriceID string
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

	// The parked half. A parked delivery is retried by billing.webhook.tick: Promote when it now
	// verifies (the body is deleted that moment), RetryParked while verification is still
	// unavailable, Reject when it FAILS to verify — a security event; never trusted, body dropped.
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
	// domain.Sub.Apply (invariant 15) — in one conditional statement, so concurrent appliers converge.
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
// one adds one, and the third in a row disables the instruction with the reason recorded.
type AutoRecharges interface {
	Get(ctx context.Context, s domain.Scope) (domain.AutoRecharge, bool, error)
	Put(ctx context.Context, a domain.AutoRecharge) error
	Claim(ctx context.Context, s domain.Scope, now time.Time, staleAfter time.Duration) (RechargeClaim, RechargeRefusal, error)
	Settle(ctx context.Context, s domain.Scope, ok bool, reason string, now time.Time) (disabled bool, err error)
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
        unavailable (PayPal's API down) → Inbox.Park(raw) → 202. Retried by billing.webhook.tick.
  4. Inbox.Persist(normalized event) — an existing (account, event id) is a no-op.
  5. 200. Publish billing.webhook.received as a latency hint.
```

A body over the limit (1 MiB) is `413` before anything else. A route that names no registered provider,
or an account that does not exist, is inactive or belongs to another provider, is `404 unknown_account`
and verifies nothing. A delivery is parked under a name made of the SHA-256 of its bytes
(`parked:<hash>`) — nothing in it may be parsed yet — so the provider's redelivery of the same bytes is
one row. `billing.webhook.tick` calls `Ingress.Retry`: a parked delivery that now verifies becomes an
ordinary event and loses its raw body; one that fails is rejected (a security event, body dropped, never
processed); one that still cannot be verified backs off (30 s doubling, one hour at most). An event whose
provider or account differs from the route's is an adapter defect, answered `500`, never filed. Metrics
are counted only after the route has resolved to a registered provider and a live account, so no
label is ever keyed on what an unauthenticated caller typed.

Nothing in stage 1 touches money, so it can run on a single transaction's latency and answer every
duplicate `200` without a retry storm. **The only way a verified delivery is lost is a failure
before step 4 commits — and then the provider has not received `200` and retries it.**

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

Three consecutive failures disable the row (`disabled_reason`) and emit `billing.payment.<tag>`
with `failed`, so the host can ask the customer to update their card. The saved method is set up
through `CreateCheckout` in `setup` mode or the provider portal; this service stores only the
provider's reference.

### Per-provider notes

| Provider | Verification | Notable events | Gotcha |
|---|---|---|---|
| **Stripe** | `Stripe-Signature`: HMAC-SHA256 over `timestamp.payload`, constant-time, with a tolerance window | `invoice.paid`, `checkout.session.completed`, `invoice.payment_failed`, `customer.subscription.updated\|deleted`, `charge.refunded` (`amount_refunded` is cumulative), `charge.dispute.created\|funds_withdrawn\|funds_reinstated\|closed` | `invoice.paid` **and** `checkout.session.completed` can both arrive for one purchase: key the grant on the **payment/invoice id**, never the session. Deliveries are unordered — hence `FetchSubscription` |
| **Polar** | webhook secret HMAC | `order.created`, `subscription.active\|updated\|canceled`, `refund.created`, `benefit_grant.*` | `benefit_grant` can double with `order.created` — same keying discipline |
| **PayPal** | **not a local HMAC** — a call to `verify-webhook-signature` with the transmission headers and `webhook_id` | `PAYMENT.CAPTURE.COMPLETED\|REFUNDED\|REVERSED`, `BILLING.SUBSCRIPTION.*`, `CUSTOMER.DISPUTE.*` | verification does I/O: a PayPal outage means deliveries can be neither verified nor rejected, so they are **parked** with their raw body and retried — never processed optimistically, never dropped |

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
