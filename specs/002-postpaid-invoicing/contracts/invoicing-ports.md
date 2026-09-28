# Contract: Rating & Invoicing (post-paid)

**Spec**: [../spec.md](../spec.md) | **Status**: Designed, not started. **Additive to [001](../../001-metering-billing-core/contracts/metering-ports.md); changes none of it.**

## Why `Rater` is a second port and not a smarter `Pricer`

[`Pricer`](../../001-metering-billing-core/contracts/metering-ports.md) is pure over **one event**. That purity is load-bearing: it is what lets the same computation run at the call site, during reconciliation and in a dispute replay and yield the same number.

Post-paid pricing is not a function of one event. `Rater` is therefore pure over **the set** — the period's complete, immutable usage — rather than over one event. It runs once at close, never on a hot path, and is replayable for the same reason `Pricer` is: it reads only recorded usage and recorded schedule versions.

It does not read that usage **event by event**. Every schedule kind depends only on how much of a unit fell under each schedule version, in the order the versions took effect — so the period is aggregated in SQL to one row per (unit, schedule version) before the `Rater` sees it. A period of a billion events rates in the memory of a few rows, and the result is identical to rating the events one by one (proven by `RaterContract`'s equivalence test).

```text
HOT PATH (unchanged from 001)                 PERIOD CLOSE (new, off the hot path)
  Pricer.Price(card, event) → credits  ┌──▶  Rater.Rate(period, usage aggregates, schedules)
  Ledger.Debit        → ledger row     │         │  graduated · volume · package · flat
  Admit               → enforcement    │         │  commitments · minimums · drawdown
        │                               │         ▼
        └── immutable credit_ledger ────┘     []InvoiceLine
                                                  │  DiscountEngine.Apply (documented order)
                                                  ▼
                                              Invoice (draft → finalized, IMMUTABLE)
                                                  │  Collector.Collect
                                                  ▼
                                              provider · payment · credit notes
```

---

## Domain types

```go
package invoicing // invoicing/domain

import (
	"time"
	"github.com/truongpx396/intel-payment/metering/domain"
)

type Money = domain.Money

// SettlementModel is per scope, and is fixed for a period once that period opens —
// a customer's terms cannot change under them mid-cycle.
type SettlementModel string

const (
	PrepaidCredits  SettlementModel = "prepaid_credits"  // 001's model; the default
	PostpaidInvoice SettlementModel = "postpaid_invoice" // meter now, invoice at close
	Hybrid          SettlementModel = "hybrid"           // credits first, overage invoiced
)

// ScheduleKind is how a metered dimension becomes money over a period. The four cover
// essentially all usage pricing in the wild; a fifth belongs here only with a real customer.
type ScheduleKind string

const (
	// Flat: amount = qty × unit_price. The only one a per-event Pricer could have done.
	Flat ScheduleKind = "flat"
	// Graduated: each tier charges its OWN rate for the volume inside it.
	//   1–1k @ $0, 1k–10k @ $0.40/k  ⇒ 5k units = 0×1k + 0.40×4 = $1.60
	Graduated ScheduleKind = "graduated"
	// Volume: the FINAL total selects one rate, applied retroactively to everything.
	//   5k units reaching the 1k–10k band ⇒ 0.40 × 5 = $2.00
	// Graduated and Volume differ by ~25% on the same schedule and the same usage. A system
	// that conflates them under-bills or over-bills every customer, so the kind is explicit
	// on every schedule and named on every line.
	Volume ScheduleKind = "volume"
	// Package: charged in whole blocks, rounded UP. 5,001 units of a 1k package = 6 packages.
	Package ScheduleKind = "package"
)

type Tier struct {
	UpTo          *int64 // nil = unbounded (the last tier)
	UnitPriceMicros int64
	FlatMicros      int64 // an optional per-tier fixed charge
}

// RateSchedule prices ONE metered dimension for one realm. Versioned and immutable, exactly
// like a metering rate card: a price change publishes a new version, so closed periods
// re-rate identically forever.
type RateSchedule struct {
	Realm     domain.Realm
	Version   string
	Product   string      // the line grouping a human recognizes: "API requests", "Storage"
	Unit      domain.Unit // the metering Unit it rates
	Kind      ScheduleKind
	Tiers     []Tier
	PackageSize int64     // Package only
	Currency  string
}

// CommitmentKind distinguishes the two things people both call "commitment".
type CommitmentKind string

const (
	// Minimum: spend at least X per period. Falling short adds a SHORTFALL line — the usage
	// lines stay truthful. Inflating a usage figure to reach a minimum makes the invoice lie
	// about what happened, which is the one thing an invoice may not do.
	Minimum CommitmentKind = "minimum"
	// Prepaid: X paid up front, drawn down by usage. Rating shows a credit line and reports
	// what remains.
	Prepaid CommitmentKind = "prepaid"
)

type Commitment struct {
	Scope     domain.Scope
	Kind      CommitmentKind
	Amount    Money
	Remaining Money      // Prepaid only
	StartsAt  time.Time
	EndsAt    time.Time
	// RolloverUnused: does an unspent minimum carry forward? Contracts differ, so it is data.
	RolloverUnused bool
}

type PeriodStatus string

const (
	PeriodOpen     PeriodStatus = "open"
	PeriodClosing  PeriodStatus = "closing"  // resumable; close is idempotent
	PeriodClosed   PeriodStatus = "closed"
	PeriodInvoiced PeriodStatus = "invoiced"
)

type BillingPeriod struct {
	Scope     domain.Scope
	Start     time.Time
	End       time.Time
	Status    PeriodStatus
	Model     SettlementModel // FIXED at open — terms cannot change under a customer mid-cycle
}

type LineKind string

const (
	UsageLine        LineKind = "usage"
	SubscriptionLine LineKind = "subscription"
	ShortfallLine    LineKind = "minimum_shortfall"
	DrawdownLine     LineKind = "commitment_drawdown" // negative
	DiscountLine     LineKind = "discount"            // negative
	CreditLine       LineKind = "credit"              // negative
	TaxLine          LineKind = "tax"
	ResidualLine     LineKind = "residual"            // see invariant 2
)

// InvoiceLine is one component. Every field that produced the amount is recorded on it,
// because "why is this number what it is?" is asked months later, by someone with only
// the invoice in front of them.
type InvoiceLine struct {
	Kind        LineKind
	Description string
	Product     string
	Unit        domain.Unit
	Quantity    int64
	Schedule    ScheduleKind // named on the line: graduated and volume are not interchangeable
	ScheduleVersion string
	RateCardVersions []string // every metering card version that priced the underlying events
	AmountMinor int64        // signed; negative for discounts, drawdowns and credits
	Currency    string
	// SourceRef resolves to the ledger rows behind this line. Without it a disputed line is
	// unanswerable, and "trust the total" is not an answer a finance team accepts.
	SourceRef   LineSource
}

type LineSource struct {
	PeriodStart, PeriodEnd time.Time
	Resource               string
	LedgerQuery            string // the reproducible predicate over credit_ledger
	EventCount             int64
}

type InvoiceStatus string

const (
	Draft         InvoiceStatus = "draft"      // mutable; the ONLY mutable state
	Finalized     InvoiceStatus = "finalized"  // immutable forever
	Sent          InvoiceStatus = "sent"
	Paid          InvoiceStatus = "paid"
	Void          InvoiceStatus = "void"       // pre-send withdrawal
	Uncollectible InvoiceStatus = "uncollectible"
)

type Invoice struct {
	Realm       domain.Realm
	Scope       domain.Scope
	Number      string // human-facing, gapless per realm (see invariant 8)
	Period      BillingPeriod
	Lines       []InvoiceLine
	SubtotalMinor int64
	TaxMinor      int64
	TotalMinor    int64
	Currency    string
	Status      InvoiceStatus
	IssuedAt    time.Time
	DueAt       time.Time
	ProviderInvoiceID string
	// NotApplied records discounts and commitments that did NOT apply, with the reason.
	// A customer asking "what happened to my discount?" deserves an answer from the record.
	NotApplied []Skipped
}

type Skipped struct{ Code, Reason string }

// CreditNote is the ONLY sanctioned correction to a finalized invoice.
type CreditNote struct {
	Realm     domain.Realm
	Scope     domain.Scope
	Number    string
	AgainstInvoice string
	Lines     []InvoiceLine // negative
	TotalMinor int64
	Reason    string        // REQUIRED
	Actor     string        // REQUIRED
	IssuedAt  time.Time
}
```

---

## Ports

```go
package ports // invoicing/ports

// Rater converts a period's usage into invoice lines.
//
// PURE over the set: it reads only `usage` (aggregated from immutable ledger rows), the schedules and
// commitments handed to it, and nothing else — no clock, no I/O, no ambient config. That is
// what makes SC-101 (re-rating reproduces the invoice exactly) achievable rather than aspirational.
//
// It MUST price each event under the rate card in force WHEN IT OCCURRED, using the
// rate_card_version recorded on the ledger row — never the card current at close.
type Rater interface {
	Rate(ctx context.Context, in RateInput) ([]domain.InvoiceLine, error)
}

type RateInput struct {
	Period      domain.BillingPeriod
	// Usage is the period's consumption aggregated per (unit, schedule version), ordered by the
	// version's effective time — never the raw event list, which can be billions of rows.
	Usage       []UsageAggregate
	Schedules   []domain.RateSchedule
	Commitments []domain.Commitment
	Currency    string
	TrialActive bool // rate normally, then zero the amounts and keep the would-be cost visible
}

// UsageAggregate is one unit's consumption under one schedule version, with the ledger range
// behind it so every line stays traceable (invariant 4).
type UsageAggregate struct {
	Unit            domain.Unit
	ScheduleVersion string
	EffectiveFrom   time.Time // orders tiers across a mid-period version change
	Quantity        int64
	EventCount      int64
	Source          domain.LineSource
}

// Across a mid-period schedule change, GRADUATED tiers are consumed cumulatively in version order
// (the 1,000,001st unit is in the second tier whichever version priced it); VOLUME selects each
// version's rate from the PERIOD total under that version's tier table. Both are deterministic,
// both are named on the line, and both are in RaterContract.

// DiscountEngine applies discounts in ONE documented order. The order is part of the contract,
// not an implementation detail: percentage-then-fixed and fixed-then-percentage give different
// totals on the same inputs, and a customer who recomputes their invoice must land on our number.
//
// The order is: (1) line-scoped, (2) product-scoped, (3) invoice-scoped; and within each,
// (a) fixed-amount before (b) percentage. Ties break by the discount's stable code, ascending.
type DiscountEngine interface {
	Apply(ctx context.Context, lines []domain.InvoiceLine, ds []domain.Discount) ([]domain.InvoiceLine, []domain.Skipped, error)
}

// Periods owns the lifecycle. Close is an idempotent claim, so two schedulers or a retried
// tick produce exactly one invoice.
type Periods interface {
	Current(ctx context.Context, s domain.Scope) (domain.BillingPeriod, error)
	// ClaimClose transitions open → closing and returns won=false if someone else holds it.
	ClaimClose(ctx context.Context, s domain.Scope, period time.Time) (won bool, err error)
	Close(ctx context.Context, s domain.Scope, period time.Time) error
	// Open starts the next period, fixing its SettlementModel at that moment.
	Open(ctx context.Context, s domain.Scope, start time.Time, m domain.SettlementModel) error
}

type Invoices interface {
	Draft(ctx context.Context, inv domain.Invoice) (id string, err error)
	// Finalize assigns the gapless number and freezes the document. After this it is
	// immutable — an implementation that offers an Update is in violation of invariant 3.
	Finalize(ctx context.Context, id string) (domain.Invoice, error)
	Get(ctx context.Context, id string) (domain.Invoice, error)
	List(ctx context.Context, s domain.Scope, cursor string, limit int) ([]domain.Invoice, string, error)
	IssueCreditNote(ctx context.Context, cn domain.CreditNote) (string, error)
}

// Collector hands a finalized invoice to a provider. Status is PROVIDER-REPORTED, arriving
// by webhook exactly as in 001 — never inferred from a successful hand-off.
type Collector interface {
	Collect(ctx context.Context, inv domain.Invoice) (providerInvoiceID string, err error)
	Void(ctx context.Context, providerInvoiceID string) error
}
```

---

## Invariants

1. **Rating is pure over the period's immutable usage.** No clock, no I/O, no ambient config. It consumes per-(unit, schedule version) aggregates, which is exactly equivalent to rating the events one by one. Re-rating a closed period reproduces its invoice byte for byte (SC-101).
2. **Lines sum to the subtotal, or a residual line is shown.** Never a silent rounding gap. A document whose whole purpose is exact accounting may not present arithmetic that does not close — the same rule the credits UI holds to, for the same reason.
3. **A finalized invoice is immutable.** Corrections are credit notes. There is no update path, and an implementation that provides one has broken the artifact's only real property.
4. **Every line is traceable.** `SourceRef` resolves to the ledger rows behind it. "Trust the total" is not an answer.
5. **Historical pricing wins.** An event is priced under the rate card in force when it occurred, per its recorded `rate_card_version`. A card published today never re-prices last quarter.
6. **Schedule kind is explicit and named on the line.** Graduated and volume differ by a large margin on identical inputs; inferring which was meant is how a system silently mis-bills every customer.
7. **Minimums add a shortfall line; they never inflate usage.** The usage lines stay true to what happened.
8. **Invoice numbers are gapless per realm.** A gap is an audit finding in most jurisdictions. A voided invoice keeps its number and its `void` status; it is not reissued and the number is never reused.
9. **Close is idempotent and single-owner.** Under a per-(scope, period) claim, so two schedulers produce one invoice. A `closing` period is resumable; a partial invoice is never `finalized`.
10. **Late events never alter a finalized invoice.** `LateEventPolicy` decides, and the default carries them into the open period with their original `occurred_at` preserved.
11. **Integer money, end to end.** Rounding happens once, at the line, to the currency's minor unit. Intermediate results carry micros. No float touches any amount.
12. **Post-paid negative balances are normal.** `NegativeBalancePolicy` applies only to prepaid scopes; applying it to post-paid would refuse work the customer is contractually entitled to.
13. **Discount order is specified and tested**, and a discount that did not apply is recorded with its reason.
14. **Tax is carried, never computed** — its own line, sourced from the provider or a hook.
15. **Collection status is provider-reported.** Handing an invoice to a collector does not make it paid.

---

## Contract-test skeleton

```go
// RaterContract runs against ANY Rater — pure, no infra.
func RaterContract(t *testing.T, r ports.Rater) {
	t.Run("graduated and volume are not the same number", func(t *testing.T) {
		// 5,000 units; tiers: 0–1k @ $0.00, 1k–10k @ $0.40/unit
		grad := rate(t, r, schedule(domain.Graduated, tiers), qty(5000))
		vol  := rate(t, r, schedule(domain.Volume,    tiers), qty(5000))
		if grad.Total() == vol.Total() {
			t.Fatal("graduated must charge 4,000 units at the second rate; volume all 5,000")
		}
		mustEqual(t, grad.Total(), 1600_00) // 0×1000 + 0.40×4000
		mustEqual(t, vol.Total(),  2000_00) // 0.40×5000
	})
	t.Run("package rounds up to whole blocks", func(t *testing.T) {
		got := rate(t, r, schedule(domain.Package, pkg(1000, 5_00)), qty(5001))
		mustEqual(t, got.Total(), 30_00) // 6 packages, not 5
	})
	t.Run("a mid-period rate change prices each event under its own card", func(t *testing.T) {
		got := rate(t, r, twoCardPeriod(t)) // half the events under v1, half under v2
		if got.Total() != handCalculated(t) {
			t.Fatal("close-time card was applied retroactively (invariant 5)")
		}
		if len(got.RateCardVersions()) != 2 { t.Fatal("the invoice must name both versions") }
	})
	t.Run("rating aggregates equals rating the events one by one", func(t *testing.T) {
		if !reflect.DeepEqual(rate(t, r, fixture), rateEventByEvent(t, r, fixture)) {
			t.Fatal("aggregation changed the result — the Rater may not depend on event order within a version")
		}
	})
	t.Run("re-rating is byte-identical", func(t *testing.T) {
		a := rate(t, r, fixture); b := rate(t, r, fixture)
		if !reflect.DeepEqual(a, b) { t.Fatal("rating is not replayable (invariant 1, SC-101)") }
	})
	t.Run("a minimum adds a shortfall line and leaves usage truthful", func(t *testing.T) {
		got := rate(t, r, withMinimum(fixtureUsing(500_00), 2000_00))
		if usageTotal(got) != 500_00 { t.Fatal("usage lines were inflated to reach the minimum") }
		if !hasLine(got, domain.ShortfallLine, 1500_00) { t.Fatal("missing shortfall line") }
	})
	t.Run("lines sum to the subtotal", func(t *testing.T) {
		got := rate(t, r, fixture)
		if sum(got.Lines) != got.SubtotalMinor && !hasLine(got, domain.ResidualLine, 0) {
			t.Fatal("arithmetic does not close and no residual is shown (invariant 2)")
		}
	})
	t.Run("a trial rates to zero but keeps the would-be cost visible", func(t *testing.T) {
		got := rate(t, r, trial(fixture))
		if got.Total() != 0 { t.Fatal("a trial must not charge") }
		if wouldBeCost(got) == 0 { t.Fatal("the customer should see what it will cost") }
	})
}

// InvoiceContract runs against a real store.  //go:build integration
func InvoiceContract(t *testing.T, inv ports.Invoices, pr ports.Periods) {
	t.Run("a finalized invoice cannot be mutated", func(t *testing.T) { /* no path exists; assert the API */ })
	t.Run("a correction is a credit note referencing the invoice", func(t *testing.T) { /* … */ })
	t.Run("concurrent period close produces exactly one invoice", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ { wg.Add(1); go func(){ defer wg.Done(); _ = closePeriod(pr, scope, p) }() }
		wg.Wait()
		if n := countInvoices(t, scope, p); n != 1 { t.Fatalf("%d invoices for one period", n) }
	})
	t.Run("invoice numbers are gapless and a void keeps its number", func(t *testing.T) { /* … */ })
	t.Run("a late event does not alter a finalized invoice", func(t *testing.T) { /* … */ })
}
```

A fixture suite derived from **published vendor pricing pages** (SC-106) is the honest test of a rating engine: if it cannot reproduce a real, public price table by hand-checked arithmetic, it cannot be trusted with a customer's.

---

## Adoption checklist (per host adopting post-paid)

- [ ] **Settlement model chosen per scope** — and understood to be fixed for a period once it opens.
- [ ] **Schedules defined as data** — with `ScheduleKind` explicit; nobody guesses graduated vs volume.
- [ ] **Spend caps set** for post-paid scopes, with the deny code mapped distinctly from balance exhaustion.
- [ ] **Period boundary chosen** — calendar month, anniversary, or custom — and the close tick scheduled.
- [ ] **`LateEventPolicy` chosen** and its consequence understood.
- [ ] **Invoice numbering format agreed** with whoever answers to an auditor.
- [ ] **Collector wired** and collection status flowing back by webhook, never inferred.
- [ ] **Tax source decided** — provider or hook. This system still computes none.
- [ ] **A real period rated and hand-checked** before the first customer invoice. Not a unit test — an actual period, checked by someone who would notice being overcharged.

## Non-goals

- **Tax computation, invoice PDFs, delivery, dunning workflows** — provider or host.
- **Revenue recognition, deferred revenue, GL export** — [Phase 3](../../../ROADMAP.md).
- **CPQ, quotes, contract lifecycle** — a different product.
- **Credit scoring and collections** — the provider's tooling and the host's policy.
