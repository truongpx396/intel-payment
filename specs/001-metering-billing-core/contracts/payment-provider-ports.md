# Contract: Payments & Providers (fiat boundary)

**Spec**: [../spec.md](../spec.md) | **Plan**: [../plan.md](../plan.md) | **Status**: Normative — the fiat layer of `intel-payment`.

This contract defines the layer that turns the credit engine into a monetized system. It is **strictly additive to consumption**: credits remain the single internal unit, and nothing here changes how they are spent. A payment provider converts fiat → credits (one-time top-up) or grants a recurring allotment (subscription), then appends a positive `credit_ledger` row **through the same idempotent `Grant` path any host would use**. The consumption hot path is untouched.

> **Provenance.** Derived from the Phase 2 Billing & Payments design in [aisat-intel](https://github.com/truongpx396/aisat-intel) (`specs/draft-plan.md`, commits `e6b689d` → `cc02bc4`), elevated from a design sketch into a ports contract with the same rigor as [metering-ports.md](./metering-ports.md). See [PROVENANCE.md](../../../PROVENANCE.md) for what changed in the lift.

---

## Design principles

1. **Provider-agnostic core, thin adapters.** A `PaymentProvider` port exposes checkout, portal, webhook verification, event parsing and subscription fetch. Stripe, Polar and PayPal are interchangeable adapters behind it. **No code outside `billing/adapters/driven/<provider>/` may import a provider SDK** — enforced by a `depguard` rule, not by convention.
2. **Money is integer minor units.** All fiat is `BIGINT` minor units + an ISO-4217 `currency CHAR(3)`. Never floats, anywhere, including in a log line or a test fixture.
3. **Webhooks are the source of truth for fulfilment.** Credits are granted on a *verified* provider event, never optimistically on a checkout return. A checkout return only redirects the browser. This is not a preference: the user can close the tab, the redirect can be replayed, and the payment can fail after the redirect fires.
4. **Idempotent everywhere.** Provider event IDs dedup in `payment_events`; credit grants reuse `credit_idem (realm, idem_key)`. Replays are no-ops that return `200` so the provider stops retrying.
5. **Signature verification is mandatory and precedes everything.** Every webhook is signature-verified on the **raw body** before any parse, any lookup, any side effect. An unverified payload is rejected `400`, counted as a **security** event, and never processed. A webhook route with no verification is a critical defect, not a shortcut.
6. **The fiat layer never writes the ledger.** It calls `Grant` through the metering port, exactly as any other host would. There is still exactly one durable money-writer (metering invariant 1), and the payments layer is not it.
7. **Fiat and credits are separate records with one reconciliation key.** A refund reverses fiat *and* appends a negative ledger row — two records, joined by `idem_key`. Merging them into one table would make a currency conversion and a credit grant indistinguishable.

---

## Ports at a glance

```text
BROWSER                                  PROVIDER (Stripe · Polar · PayPal · yours)
   │ POST /billing/checkout                  │ POST /webhooks/{provider}   (unauthenticated,
   ▼                                         ▼                              signature-verified)
┌── Checkout (app) ──────────────┐   ┌── WebhookIngress (app) ─────────────────────────────┐
│  Catalog.Resolve(realm, code)  │   │  1. read RAW body (no pre-parse)                    │
│  Customers.Upsert(scope)       │   │  2. PaymentProvider.VerifyWebhook  ← CRITICAL, first │
│  PaymentProvider.CreateCheckout│   │  3. ParseEvent → {event_id, type, object}            │
└──────────────┬─────────────────┘   │  4. Events.Claim(event_id)  ON CONFLICT → 200 no-op │
               │ redirectUrl          │  5. resolve Scope from the VERIFIED customer id     │
               ▼                      │  6. dispatch by type ──────────────┐                │
        (user pays at provider)       └────────────────────────────────────┼────────────────┘
                                                                           ▼
                        ┌── the ONLY way credits are created ──────────────────────────┐
                        │  metering ports.Meter.Grant({scope, amount, idem_key, …})    │
                        │  idem_key = provider_payment_id / invoice_id                 │
                        └──────────────────────────────────────────────────────────────┘
```

Four seams a host swaps independently: the **`PaymentProvider`** (who takes the money), the **`Catalog`** (what is sold), the **`TaxStrategy`** (who computes tax), and the stores.

---

## Domain types

```go
package billing // billing/domain

import (
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Money is re-exported from metering: one money type across the whole system.
type Money = domain.Money

type Provider string

const (
	Stripe Provider = "stripe"
	Polar  Provider = "polar"
	PayPal Provider = "paypal"
)

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

// Plan is a purchasable thing, defined per REALM. `Code` is the host's stable slug and the
// only identifier that ever crosses to a UI — a provider price id leaking outward is a defect.
type Plan struct {
	Realm domain.Realm
	Code  string // "pro_monthly" | "pack_10k" — stable, host-owned
	Name  string
	Kind  PlanKind

	// CreditAllotment is the ONLY coupling between fiat and credits: how many credits one
	// purchase (or one billing period) grants. Nil means custom/negotiated — the catalogue
	// renders a contact path rather than a checkout.
	//
	// Because pricing lives here and consumption lives in the ledger, changing a price can
	// never retroactively alter credits already granted. That property is structural.
	CreditAllotment *domain.Credits

	Interval *Interval // nil for OneTime
	Active   bool
	SortOrder int

	// Prices is per-currency, not one price. A reusable billing service that hardcodes one
	// currency per plan forces a host selling in three currencies to maintain three plans.
	Prices map[string]Money // ISO-4217 → amount

	// Entitlements are the non-credit benefits (seats, feature flags, quotas). See
	// entitlement-ports.md — the catalogue renders them, the Entitler enforces them.
	Entitlements []Entitlement
}

// Customer links a metering Scope to one provider's customer record.
type Customer struct {
	Scope              domain.Scope
	Provider           Provider
	ProviderCustomerID string
	BillingEmail       string // receipts/invoices; defaults to the host-supplied owner email
}

type SubStatus string

const (
	Trialing SubStatus = "trialing"
	Active   SubStatus = "active"
	PastDue  SubStatus = "past_due"
	Paused   SubStatus = "paused"
	Canceled SubStatus = "canceled"
	Incomplete SubStatus = "incomplete"
)

type Sub struct {
	Scope                  domain.Scope
	PlanCode               string
	Provider               Provider
	ProviderSubscriptionID string
	Status                 SubStatus // driven EXCLUSIVELY by webhooks, never by a local action
	CurrentPeriodStart     time.Time
	CurrentPeriodEnd       time.Time
	CancelAtPeriodEnd      bool
	// GraceUntil is set when a payment fails: the subscription keeps its entitlements until
	// this instant, then suspends. Without it, one failed card on a Friday cuts off a paying
	// customer before anyone can retry it.
	GraceUntil *time.Time
}

type PaymentStatus string

const (
	Pending           PaymentStatus = "pending"
	Succeeded         PaymentStatus = "succeeded"
	Failed            PaymentStatus = "failed"
	Refunded          PaymentStatus = "refunded"
	PartiallyRefunded PaymentStatus = "partially_refunded"
	Disputed          PaymentStatus = "disputed"
)

// Payment is a fiat transaction, kept for accounting, receipts, refunds and provider
// reconciliation. It is NOT a ledger row; `IdemKey` is what joins the two.
type Payment struct {
	Scope             domain.Scope
	Provider          Provider
	ProviderPaymentID string // PaymentIntent / order / invoice id
	PlanCode          string // empty for ad-hoc
	Amount            Money
	TaxAmount         *Money // whatever the provider reported; never computed here
	CreditsGranted    domain.Credits
	Status            PaymentStatus
	ReceiptURL        string
	FailureReason     string
	IdemKey           string // == the credit_ledger grant row's idem_key. The reconciliation key.
	CreatedAt         time.Time
}

// Event is a PARSED, ALREADY-VERIFIED provider event. Constructing one outside a successful
// VerifyWebhook is a defect: the type's existence is the proof that verification happened.
type Event struct {
	Provider        Provider
	ProviderEventID string
	Type            EventType
	// Object carries the provider-neutral facts the dispatcher needs. Adapters normalize into
	// it; the dispatcher never sees a provider-shaped payload, which is what keeps the
	// dispatch logic identical across providers.
	Object EventObject
	RawHash string // SHA-256 of the verified body — stored instead of the body (PII)
}

type EventType string

const (
	PaymentSucceeded    EventType = "payment_succeeded"
	PaymentFailed       EventType = "payment_failed"
	SubscriptionUpdated EventType = "subscription_updated"
	SubscriptionDeleted EventType = "subscription_deleted"
	Refunded            EventType = "refunded"
	DisputeOpened       EventType = "dispute_opened"
	// Ignored is a FIRST-CLASS outcome, not an error. A provider sends dozens of event types
	// nobody subscribed to; an adapter that errors on them turns normal traffic into alert noise.
	Ignored EventType = "ignored"
)

type EventObject struct {
	ProviderCustomerID     string
	ProviderPaymentID      string
	ProviderSubscriptionID string
	ProviderPriceID        string
	Amount                 *Money
	TaxAmount              *Money
	Status                 string
	PeriodStart, PeriodEnd *time.Time
	CancelAtPeriodEnd      bool
	ReceiptURL             string
	FailureReason          string
}
```

---

## Port: `PaymentProvider` — the one provider-specific seam

```go
package ports // billing/ports

type PaymentProvider interface {
	Name() domain.Provider

	// CreateCheckout starts a hosted checkout and returns a URL to hand off to.
	// It MUST NOT grant anything: fulfilment arrives later on a verified webhook (principle 3).
	CreateCheckout(ctx context.Context, in CheckoutInput) (redirectURL string, err error)

	// CreatePortalSession returns a provider-hosted management portal URL (payment method,
	// invoices, cancellation). Delegating this is deliberate: re-implementing card management
	// means handling card data, and the best card-data posture is never to touch it.
	CreatePortalSession(ctx context.Context, customerID string, returnURL string) (portalURL string, err error)

	// VerifyWebhook authenticates the RAW body and returns the parsed event.
	//
	// CRITICAL. It MUST:
	//   - operate on the raw, unmodified body (no JSON round-trip before it)
	//   - compare signatures in CONSTANT TIME
	//   - enforce a timestamp tolerance, so a captured payload cannot be replayed forever
	//   - return an error, never a partially-populated Event, on any failure
	// It MAY perform I/O (PayPal verifies via an API call, not a local HMAC) — so callers
	// MUST give it a deadline.
	VerifyWebhook(ctx context.Context, rawBody []byte, headers http.Header) (domain.Event, error)

	// FetchSubscription reconciles local state against the provider's. Used by the drift
	// sweep, and after any window where webhooks may have been missed.
	FetchSubscription(ctx context.Context, providerSubID string) (domain.Sub, error)

	// ChangePlan applies an upgrade/downgrade under the deployment's PlanChangePolicy.
	// Proration math belongs to the provider, which already owns the invoice.
	ChangePlan(ctx context.Context, providerSubID, providerPriceID string, policy domain.PlanChangePolicy) error

	// CancelAtPeriodEnd sets the flag at the provider. Local status is NOT updated here —
	// it changes when the resulting webhook arrives (principle 3 applied to cancellation too,
	// so there is exactly one path that writes subscription state).
	CancelAtPeriodEnd(ctx context.Context, providerSubID string) error
}

type CheckoutInput struct {
	Scope           domain.Scope
	PlanCode        string
	Currency        string
	ProviderPriceID string
	CustomerID      string // existing provider customer, or "" to create
	BillingEmail    string
	SuccessURL      string
	CancelURL       string
	// IdemKey guards against a double-clicked buy button creating two checkouts,
	// and is forwarded to providers that support an idempotency key natively.
	IdemKey string
	// Metadata is echoed back on the webhook. It MUST NOT be trusted as authority for
	// scope or amount — those are resolved from the verified object (see invariant 4).
	Metadata map[string]string
}
```

## Port: `Catalog` — what is sold, as data

```go
// Catalog resolves the realm's purchasable offers. Plans live in Postgres, not in Go
// literals: a host must be able to launch a plan without a redeploy of the billing service.
type Catalog interface {
	List(ctx context.Context, realm domain.Realm, currency string) ([]domain.Plan, error)
	Resolve(ctx context.Context, realm domain.Realm, code, currency string, p domain.Provider) (domain.Plan, string, error) // → plan, provider_price_id
}
```

## Port: `TaxStrategy` — named, not silently absent

```go
// Tax is the difference between a demo and a system that can take money in the EU.
// The engine does not compute tax; it names WHO does, so the answer is never "nobody".
type TaxStrategy interface {
	// Mode reports how tax is handled so the checkout flow and the receipt can say so.
	Mode() domain.TaxMode
	// Annotate enriches a checkout with whatever the mode needs (a tax-behavior flag for a
	// provider-managed mode, a computed line from an external service, nothing for "none").
	Annotate(ctx context.Context, in *ports.CheckoutInput) error
}
```

`TaxMode` is config (`PAYMENT_TAX_STRATEGY`):

| Mode | Who computes tax | Use when |
|---|---|---|
| `provider_managed` (default) | The provider, as merchant of record or via its tax product (Stripe Tax, Polar MoR) | Almost always. The provider owns the rate tables and the filings |
| `external_hook` | A host-supplied service, called before checkout | The host already owns tax for other revenue and must stay consistent |
| `none` | Nobody — prices are tax-inclusive or the host is out of scope | Internal chargeback, single-jurisdiction, or credits that are not a taxable sale |

There is no `compute_it_here` mode. Rate tables and filing obligations are a specialist product, and a half-implemented one is a liability rather than a feature.

---

## Stores

```go
type Customers interface {
	Get(ctx context.Context, s domain.Scope, p domain.Provider) (domain.Customer, bool, error)
	Upsert(ctx context.Context, c domain.Customer) error
	// ResolveScope maps a VERIFIED provider customer id back to a Scope. This is the only
	// sanctioned way a webhook learns whom it is about (invariant 4).
	ResolveScope(ctx context.Context, p domain.Provider, providerCustomerID string) (domain.Scope, bool, error)
}

type Events interface {
	// Claim inserts the event and reports whether THIS call won the race. `false` means
	// already-seen: the caller returns 200 and does nothing. Atomic insert-or-lose is what
	// makes concurrent duplicate deliveries safe — a read-then-write check is not.
	Claim(ctx context.Context, e domain.Event, realm domain.Realm) (won bool, err error)
	MarkProcessed(ctx context.Context, p domain.Provider, eventID string, outcome string) error
	// Purge drops rows past the retention window, which MUST exceed the longest provider
	// replay window (see invariant 7).
	Purge(ctx context.Context, olderThan time.Time) (int, error)
}

type Payments interface {
	Upsert(ctx context.Context, p domain.Payment) error
	List(ctx context.Context, s domain.Scope, cursor string, limit int) ([]domain.Payment, string, error)
}

type Subs interface {
	Get(ctx context.Context, s domain.Scope) (domain.Sub, bool, error)
	Upsert(ctx context.Context, sub domain.Sub) error
	// DueForGraceExpiry drives the dunning sweep.
	DueForGraceExpiry(ctx context.Context, now time.Time) ([]domain.Sub, error)
}
```

---

## Webhook processing flow

Identical for every provider, because the adapter normalizes before the dispatcher runs.

```text
provider → POST /webhooks/{provider}
  1. Read the RAW request body. Body-size limited; no JSON middleware may touch it first.
  2. PaymentProvider.VerifyWebhook(raw, headers)
        fail → 400 invalid_signature, increment billing_webhook_verify_failed_total{provider},
               log a SECURITY event, STOP. Never 2xx, never partial processing.
  3. Event is now trustworthy. Resolve the realm from the route/credential.
  4. Events.Claim(event) — atomic insert
        lost the race → 200 (idempotent ack; the provider stops retrying)
  5. Customers.ResolveScope(provider, event.Object.ProviderCustomerID)
        unknown customer → 200 + record `ignored`. NOT an error: a provider account can
        carry customers this deployment never created (another environment, a manual test).
  6. Dispatch on event.Type:
       payment_succeeded → Payments.Upsert(succeeded)
                           Meter.Grant{amount: plan.CreditAllotment,
                                       idem_key: provider_payment_id,
                                       reason: "purchase"|"subscription_grant"}
       payment_failed    → Payments.Upsert(failed); Subs: status=past_due,
                           GraceUntil = now + DunningGraceDays; notify the host
       subscription_*    → Subs.Upsert(status, period, cancel_at_period_end)
       refunded / dispute→ Payments.Upsert(refunded|disputed)
                           Meter.Grant{amount: NEGATIVE, idem_key: refund_id,
                                       reason: "refund"|"chargeback"}
                           ← subject to NegativeBalancePolicy (metering invariant 12)
       ignored           → record and return 200
  7. Events.MarkProcessed(...); return 200.
```

**Step 6 is the whole design.** Every money-creating branch goes through `Meter.Grant` with a provider-supplied `idem_key`. There is no second path, no direct ledger write, and no local balance arithmetic. That is why a webhook storm, a replay, an out-of-order delivery and a double-click all converge on the same balance.

### Per-provider verification notes

| Provider | Verification | Notable events | Gotcha |
|---|---|---|---|
| **Stripe** | `Stripe-Signature`: HMAC-SHA256 over `timestamp.payload`, constant-time, with a tolerance window | `checkout.session.completed`, `invoice.paid`, `invoice.payment_failed`, `customer.subscription.updated\|deleted`, `charge.refunded`, `charge.dispute.created` | `invoice.paid` **and** `checkout.session.completed` can both arrive for one purchase. Key the grant on the **payment/invoice id**, not the session, or the first subscription period is granted twice |
| **Polar** | Webhook secret HMAC | `order.created`, `subscription.active\|updated\|canceled`, `benefit_grant.*` | Closest to the credit-grant model; `benefit_grant` can double with `order.created` — same keying discipline |
| **PayPal** | **Not a local HMAC** — a call back to PayPal's `verify-webhook-signature` with the transmission headers + `webhook_id` | `PAYMENT.CAPTURE.COMPLETED`, `BILLING.SUBSCRIPTION.ACTIVATED\|CANCELLED`, `PAYMENT.CAPTURE.REFUNDED` | Verification does network I/O, so it needs a deadline and a retry budget, and a PayPal outage means webhooks cannot be verified *or* rejected. Park unverifiable deliveries for replay; never fail them open |

---

## Invariants every implementation MUST uphold

1. **Verify before anything.** No parse, no DB read, no log of the payload contents, no metric keyed on its contents — until the signature checks out on the raw body, in constant time, within a timestamp tolerance.
2. **Fulfilment is webhook-driven.** No code path grants credits on a checkout return, a client callback, or a polling success. The client's honest state after redirect is *processing*.
3. **Credits are created only through `Meter.Grant`.** The fiat layer never inserts a ledger row, never touches a balance key, never computes a new balance.
4. **Never trust client or metadata authority.** Scope, amount, currency and credit quantity are resolved from the **verified provider object** plus this deployment's own `Customers` and `Catalog`. A field in `metadata` may be used for correlation and never for authority — otherwise a forged (but validly signed, e.g. self-purchased) event can name someone else's scope.
5. **Atomic event claim.** Dedup is an atomic insert whose conflict is the duplicate signal, never a read-then-write. Two concurrent deliveries of one event must produce one grant.
6. **A replay answers `200`.** An already-processed event returns success so the provider stops retrying. Returning an error to a duplicate manufactures a retry storm.
7. **`payment_events` retention exceeds the provider's replay window.** If a row is purged while the provider can still redeliver, the dedup guard silently disappears and the grant can be applied twice. The `credit_idem (realm, idem_key)` backstop still catches it — which is exactly why the backstop exists — but the row must outlive the window regardless. Default 90 days.
8. **Refunds append, never mutate.** A refund or chargeback writes a new negative ledger row and updates the payment's status; the original payment row and the original grant row are immutable. An accounting record you can edit is not a record.
9. **A provider SDK lives in exactly one package.** Its own adapter. Anywhere else is a lint failure.
10. **Secrets are server-side only.** Provider secret keys and webhook secrets come from the environment, never a client bundle, never a `PUBLIC_`-prefixed variable, never a log line. Only publishable keys may reach a browser.
11. **Webhook routes have no CORS and no auth middleware.** They are server-to-server, authenticated by signature. A JWT check on a webhook route rejects the provider; a CORS policy on it is meaningless.
12. **Subscription status has one writer: the webhook path.** A local cancel calls the provider and waits for the resulting event. Two writers produce a state that disagrees with the invoice.
13. **A failed payment does not instantly revoke.** `GraceUntil` holds entitlements for the configured grace period, then suspends. Silent instant revocation on a transient card decline is an outage the customer cannot diagnose.
14. **Unverifiable deliveries are parked, never dropped and never trusted.** When verification itself is unavailable (PayPal's API down), the delivery is stored unprocessed for replay. It is not processed optimistically, and it is not discarded.

---

## Contract-test skeleton

`ProviderContract` runs against **every** adapter, so a new provider is held to the same guarantees rather than reviewed by eye.

```go
// billing/ports/contract_test.go — runs against every PaymentProvider adapter.
func ProviderContract(t *testing.T, p ports.PaymentProvider, fx Fixtures) {
	ctx := context.Background()

	t.Run("rejects a tampered body", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		tampered := bytes.Replace(body, []byte(`"amount":1000`), []byte(`"amount":100000`), 1)
		if _, err := p.VerifyWebhook(ctx, tampered, hdr); err == nil {
			t.Fatal("a tampered body must never verify")
		}
	})
	t.Run("rejects a missing signature", func(t *testing.T) {
		body, _ := fx.SignedPaymentSucceeded()
		if _, err := p.VerifyWebhook(ctx, body, http.Header{}); err == nil {
			t.Fatal("an unsigned body must never verify")
		}
	})
	t.Run("rejects a stale but validly signed body (replay window)", func(t *testing.T) {
		body, hdr := fx.SignedAt(time.Now().Add(-48 * time.Hour))
		if _, err := p.VerifyWebhook(ctx, body, hdr); err == nil {
			t.Fatal("a captured payload must not verify forever")
		}
	})
	t.Run("verifies a good body and normalizes it", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		e, err := p.VerifyWebhook(ctx, body, hdr)
		mustNoErr(t, err)
		if e.Type != domain.PaymentSucceeded { t.Fatalf("type=%q", e.Type) }
		// Normalization is what lets ONE dispatcher serve every provider.
		if e.ProviderEventID == "" || e.Object.ProviderPaymentID == "" || e.Object.Amount == nil {
			t.Fatalf("adapter must normalize the provider shape: %+v", e)
		}
		if e.RawHash == "" { t.Fatal("must hash the verified body; the body itself is not stored") }
	})
	t.Run("unsubscribed event types are Ignored, not errors", func(t *testing.T) {
		body, hdr := fx.SignedUnknownType()
		e, err := p.VerifyWebhook(ctx, body, hdr)
		mustNoErr(t, err) // signature is fine; we simply do not care about the type
		if e.Type != domain.Ignored { t.Fatalf("want Ignored, got %q", e.Type) }
	})
	t.Run("checkout never grants", func(t *testing.T) {
		url, err := p.CreateCheckout(ctx, fx.Checkout())
		mustNoErr(t, err)
		if url == "" { t.Fatal("must return a redirect URL") }
		if fx.Meter.GrantCalls() != 0 { t.Fatal("CreateCheckout granted credits — principle 3 violated") }
	})
}

// IngressContract runs against the app-level handler over a fake provider + real stores.
func IngressContract(t *testing.T, h ports.WebhookIngress, fx Fixtures) {
	t.Run("duplicate delivery grants once", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		mustStatus(t, h.Handle(ctx, "fake", body, hdr), 200)
		mustStatus(t, h.Handle(ctx, "fake", body, hdr), 200) // replay
		if n := fx.Meter.GrantCalls(); n != 1 { t.Fatalf("granted %d times, want 1", n) }
	})
	t.Run("concurrent delivery grants once", func(t *testing.T) {
		body, hdr := fx.SignedPaymentSucceeded()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ { wg.Add(1); go func(){ defer wg.Done(); _ = h.Handle(ctx, "fake", body, hdr) }() }
		wg.Wait()
		if n := fx.Meter.GrantCalls(); n != 1 { t.Fatalf("granted %d times under concurrency, want 1", n) }
	})
	t.Run("metadata cannot redirect the grant to another scope", func(t *testing.T) {
		body, hdr := fx.SignedWithMetadata(map[string]string{"scope_id": "victim"})
		mustStatus(t, h.Handle(ctx, "fake", body, hdr), 200)
		if got := fx.Meter.LastGrant().Scope.ID; got == "victim" {
			t.Fatal("scope came from metadata, not from the verified customer mapping (invariant 4)")
		}
	})
	t.Run("unknown customer is ignored, not failed", func(t *testing.T) {
		body, hdr := fx.SignedForUnknownCustomer()
		mustStatus(t, h.Handle(ctx, "fake", body, hdr), 200)
		if fx.Meter.GrantCalls() != 0 { t.Fatal("must not grant for an unmapped customer") }
	})
	t.Run("refund appends a negative grant, never mutates the original", func(t *testing.T) {
		fx.GivenSucceededPayment("pay-1", 500)
		body, hdr := fx.SignedRefund("pay-1")
		mustStatus(t, h.Handle(ctx, "fake", body, hdr), 200)
		if d := fx.Meter.LastGrant().Amount; d != -500 { t.Fatalf("refund delta=%d want -500", d) }
		if fx.Payments.Row("pay-1").Amount.MinorUnits != 500 { t.Fatal("original payment amount was mutated") }
	})
	t.Run("failed payment sets a grace window, does not revoke", func(t *testing.T) {
		body, hdr := fx.SignedPaymentFailed()
		mustStatus(t, h.Handle(ctx, "fake", body, hdr), 200)
		sub := fx.Subs.Row(fx.Scope)
		if sub.Status != domain.PastDue { t.Fatalf("status=%q want past_due", sub.Status) }
		if sub.GraceUntil == nil { t.Fatal("a transient decline must not revoke instantly (invariant 13)") }
	})
}
```

---

## Security checklist

- [ ] Webhook signature verified on the raw body, in constant time, with a timestamp tolerance, **before any side effect**.
- [ ] Webhook route: body-size limit, no CORS, no JWT/session middleware, no JSON body-rewrite before verification.
- [ ] Provider secrets from the environment only; never client-exposed, never logged. Only publishable keys reach a browser.
- [ ] `checkout` / `portal` / `cancel` / plan-change are authorization-gated by the host and re-authenticated for destructive changes.
- [ ] The handler resolves scope, amount and credits from the verified object — never from the request body, a query parameter, or `metadata`.
- [ ] Grants idempotent via `credit_idem (realm, idem_key)` + `payment_events` atomic claim; replays are no-ops that return `200`.
- [ ] No card data anywhere. No full provider payload with PII in logs or in the database — store `payload_hash`.
- [ ] `Grant` is callable only by the webhook path and audited admin tooling (metering invariant 13).
- [ ] Cross-realm rejection: a webhook route bound to realm A cannot resolve a scope in realm B.

---

## Adoption checklist (per host product)

- [ ] **Plans defined as data** — rows in `plans` + `plan_prices` + `plan_provider_prices` for the realm, not Go literals.
- [ ] **Provider(s) enabled** — `PAYMENT_PROVIDERS_ENABLED`, with secrets in the environment and webhooks registered at the provider.
- [ ] **Webhook URL registered and reachable** — and its delivery log checked after the first live payment, not assumed.
- [ ] **`CreditAllotment` set per plan** — the single fiat↔credit coupling.
- [ ] **Tax strategy chosen** — `provider_managed` unless the host already owns tax.
- [ ] **Plan-change policy chosen** — `at_period_end` unless proration is a product requirement.
- [ ] **Dunning window chosen** — grace days and suspend-after days, and what the host shows during grace.
- [ ] **Negative-balance policy chosen** — see metering invariant 12; a refund past spend is normal, not exceptional.
- [ ] **Receipts path decided** — the provider's hosted receipt (default) or the host's own from `Payments.List`.
- [ ] **Sandbox rehearsal done** — a full purchase, a renewal, a failed payment, a refund and a replayed webhook, all in the provider's test mode, before a real card.

---

## Non-goals (stays outside this layer, by design)

- **Tax computation, filing, and merchant-of-record duties** — a provider or a specialist service. Named explicitly via `TaxStrategy` so the answer is never accidentally "nobody".
- **Invoice rendering and dunning emails** — the provider's hosted invoices and the host's notification system. This layer emits the facts; it does not own the templates.
- **Credit consumption** — [metering-ports.md](./metering-ports.md). Nothing here touches the hot path.
- **Feature entitlement enforcement** — [entitlement-ports.md](./entitlement-ports.md). This layer records *what was bought*; that one answers *what is allowed*.
- **Card data, PCI scope** — hosted checkout and hosted portal, always. The safest way to handle a card number is never to receive one.
