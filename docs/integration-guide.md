# Integration Guide

How a host product adopts `intel-payment`. The whole adoption is **a scope binding, a rate card and
some configuration rows** — if you find yourself editing anything under `metering/`, something has
gone wrong and this guide will say what.

---

## 1. Decide: library or service

| | Embedded library | Container service |
|---|---|---|
| Host language | Go only | any |
| `Admit` latency | in-process Redis call, sub-ms | +1 network hop — **co-locate** (same node/AZ, or a sidecar over a Unix socket) |
| Custom pricing code | any `Pricer` you register | the `table` pricer from a card, or a pricer compiled into your build of `paymentd` |
| Who owns Redis/Postgres | the host | the service |
| Multiple products on one deployment | no | **yes** — this is the reason the realm axis exists |
| Switching later | a wiring change in `cmd/`, no data migration | same, in reverse |

**Pick the library** if you are a Go monolith with one product. **Pick the service** otherwise.
Either way the durable writer is its own process.

---

## 2. Choose your bindings

### Realm — which product this is

A stable slug. One realm per *product*, never per tenant — tenants are `Scope`s.

### Scope — who gets billed, and Subjects — who within it

```go
func scopeOf(ctx context.Context) domain.Scope {
    return domain.Scope{Realm: "acme-crm", Kind: "organization", ID: auth.OrgID(ctx)}
}

func subjectsOf(ctx context.Context) domain.Subjects {
    return domain.Subjects{"user": auth.UserID(ctx), "api_key": auth.KeyID(ctx)}
}
```

Pick the level where **money** lives, which is often not where **content** lives. Subjects are the
dimensions you want ceilings on and breakdowns by *within* that scope. They are never a price input.

### Pricing — what things cost

**Most products need no code.** If a price is "so much per unit" — per token, per call, per GB-day,
per seat-day, per second — publish a rate card for the built-in `table` pricer:

```bash
curl -X POST $PAYMENT/v1/admin/rate-cards -H "Authorization: Bearer $PAYMENT_ADMIN_TOKEN" -d '{
  "version": "2026-10-01", "retire_previous": true,
  "entries": [
    {"rate_key": "standard", "unit": "enrichment",  "credits_per_block": 2000,   "block_size": 1},
    {"rate_key": "standard", "unit": "api_call",    "credits_per_block": 150000, "block_size": 1000000}
  ]}'
```

A price is a **rational** — `credits_per_block` credits for every `block_size` units — so $0.15 per
million is exact. Pick a **fine accounting unit** (the reference is 1 credit = 1 µ$ of list price) and
let the UI's `UnitLabels.scale` decide how it is displayed.

Write a `Pricer` only for pricing that is not per-unit linear but still depends on the event alone:

```go
type EnrichmentPricer struct{} // registered in cmd/ as "enrichment"

func (EnrichmentPricer) Price(ctx context.Context, card domain.RateCard, e domain.Event) (domain.Price, error) {
    // validate your own unit vocabulary, then delegate the arithmetic to the table pricer
    for _, q := range e.Quantities {
        if q.Unit != "enrichment" && q.Unit != "api_call" {
            return domain.Price{}, fmt.Errorf("%w: unit %q", domain.ErrUnpriceable, q.Unit)
        }
    }
    return table.Pricer{}.Price(ctx, card, e)
}
```

Four rules, each tested by `PricerContract`: **pure** (no I/O, no clock), **fail closed**, **exact and
rounded up once per event**, **report the card version**. Anything that depends on period-to-date
volume — tiers, free allowances, minimums — is not a `Pricer`; it is the post-paid `Rater`
([feature 002](../specs/002-postpaid-invoicing/)).

---

## 3. Configure your realm (the admin API)

Pools, ceilings, plans, entitlements, provider accounts and event endpoints are configuration data —
see [configuration.md](./configuration.md) and [scripts/seed.sql](../scripts/seed.sql) for a complete
example. Through the admin API every change is validated, audited with your name, and live
immediately. The two decisions most teams get wrong:

- **Which ceilings belong to a subject** (a member's daily budget) and which to the scope (the balance).
- **Which ceilings depend on the plan** — set `max_entitlement` and give each plan and the free tier a
  quota, instead of one number for everyone.

---

## 4. Wire it

### Library

```go
meter, err := app.New(metering.Config{
    Realm: "acme-crm", BalanceRedisURL: …, LedgerDSN: …,
    Settlement: metering.SettlementOutbox, AdmitFail: metering.FailClosed,
}, app.Deps{
    Pricers:   pricing.Registry{"table": table.Pricer{}, "enrichment": EnrichmentPricer{}},
    Balance:   redisstore.New(rdb), Books: pgstore.New(db), Journal: pgstore.NewJournal(db),
    Limits:    pgstore.NewLimitStore(db), Pools: pgstore.NewPoolStore(db),
    RateCards: pgstore.NewRateCardStore(db), Bus: redisstreams.New(rdb),
    Quotas:    entitlementadapter.QuotaSource(entitler),
})
```

Then depend on `ports.Meter` everywhere. Nothing in your handlers should name `app`.

### Service

```go
conn, _ := grpc.NewClient(os.Getenv("PAYMENT_GRPC_ADDR"), creds)
meter := grpcclient.New(conn)   // also a ports.Meter — your code cannot tell
```

Non-Go hosts use the generated TypeScript or Python client, or the REST API directly. Set a tight
`Admit` deadline (~50 ms) and decide your fail policy deliberately.

---

## 5. Instrument your call sites

```go
// BEFORE the expensive work. Pass MaxCost — it is what bounds overshoot.
adm, err := meter.Admit(ctx, scopeOf(ctx), domain.AdmitRequest{
    Resource: "crm.enrich", Subjects: subjectsOf(ctx), MaxCost: estimate,
})
if err != nil {
    return failPolicy(err)          // your decision, made once, in one place
}
if !adm.Allowed {
    return deny(adm.Exceeded)       // 402 with an upgrade path, 409 with a reset time, 403 if blocked
}
if adm.Headroom < estimate {
    estimate = adm.Headroom         // resize rather than start work you cannot finish
}

start := time.Now()                 // when the work happened — captured ONCE
out, produced, err := doTheWork(ctx, estimate)

// AFTER, with what was ACTUALLY produced — including on cancellation or partial failure.
rcpt, err := meter.Record(ctx, domain.Event{
    Scope: scopeOf(ctx), Subjects: subjectsOf(ctx),
    Resource: "crm.enrich", RateKey: "standard", Quantities: produced,
    IdemKey:    requestID(ctx),     // stable across retries — the whole guarantee
    OccurredAt: start,              // ALSO stable across retries
})
switch {
case errors.Is(err, domain.ErrIdemConflict):
    // you reused a key for a different request: a bug in how you derive keys — fix it, do not retry
case err != nil:
    // retry later with the SAME IdemKey and OccurredAt, from a durable queue
case rcpt.Deferred:
    // accepted and durable; it will be applied when the hot store recovers — nothing to do
}
```

**`IdemKey` and `OccurredAt` are the two things you cannot get wrong.** Both must be stable across
retries of one logical operation and the key distinct between operations. A request id works; a
fresh UUID per attempt turns every retry into a new charge, and `time.Now()` at retry time turns a
late retry into a `stale_event` refusal. Retry within `PAYMENT_USAGE_IDEM_WINDOW` (72 h by default).

---

## 6. Wire the surfaces

- **Credits screen** — proxy `GET /v1/credits` behind your own session (never expose a `/v1/*`
  credential to a browser) and render with `@intel-payment/credits-ui`, supplying `UnitLabels`
  (with `scale`), your `LedgerColumn`s and your `BillingAnchor`. Or consume the JSON.
- **Checkout** — `POST /v1/checkout` → redirect. On return, show **processing** and poll
  `GET /v1/fulfilment`, or wait for the `fulfilment.granted` event.
- **Feature gates** — `POST /v1/entitlements/check`, batched per page render.
- **Events** — register an endpoint (`POST /v1/admin/event-endpoints`) for `credits.low`,
  `credits.blocked`, `payment.*`, `entitlements.changed`…; verify each delivery's signature, dedup on
  its `id`, answer `2xx` fast. Or poll `GET /v1/events?after=`. **Never connect to the service's Redis**:
  it is a money store, and access to it is the ability to mint credits.

---

## 7. Verify before you trust it

Run all of these in a provider sandbox before a real card touches it:

- [ ] A charge, then the **same charge replayed** — balance moves once.
- [ ] The **same key with a different request** — `409 idempotency_conflict`.
- [ ] A call missing a subject a ceiling needs — `400 missing_subject`, not a silent pass.
- [ ] An exhausted balance — `402` with an upgrade path; a spent daily ceiling — `409` with a reset time.
- [ ] An upgrade raises a plan-sized ceiling immediately.
- [ ] A **cancelled** operation — billed for what it produced.
- [ ] A purchase — credits appear only after the webhook; a **replayed** webhook changes nothing.
- [ ] **Kill `payment-worker` right after a webhook is acknowledged** — the grant still lands, once.
- [ ] Partial refunds — the credits revoked are exact; a dispute won gives them back.
- [ ] A **tampered** webhook — `400`, counted as a security event.
- [ ] A failed renewal — grace holds access, then suspends.
- [ ] **Kill Redis mid-traffic** — your fail policy holds, usage is deferred not lost, and shards rebuild from the books.
- [ ] Alerts actually page someone.

---

## Anti-patterns

| Don't | Why | Instead |
|---|---|---|
| Grant credits on a checkout return | The tab closes, the redirect replays, the payment fails afterwards | Poll `/v1/fulfilment` or wait for `fulfilment.granted` |
| Generate a fresh `IdemKey` or `OccurredAt` per attempt | Every retry becomes a new charge, or is refused as stale | Derive both from the logical operation, once |
| Treat `idempotency_conflict` as retryable | It is a key-derivation bug; retrying cannot fix it | Fix how keys are built |
| Omit `MaxCost` | The overshoot bound becomes unknown | Pass an estimate; watch `metering_admit_uncapped_total` |
| Report a ceiling instead of actuals | Over-bills your own users | Report what was produced |
| Price in a coarse unit to "keep numbers small" | Rounding up once per event then over-bills every small event | A fine unit in the books; `UnitLabels.scale` for display |
| `if plan == "pro"` — for features *or* ceiling sizes | Scatters the rule; launching a plan becomes a code change | `Entitler`, and `max_entitlement` on limits |
| Read the service's Redis or bus | It is a money store; read access tends to become write access | Signed webhooks or the event feed |
| Expose `/v1/*` to a browser | Anyone can spend | Proxy behind your session |
| Read the hot balance as authoritative for an invoice | It is a fast copy; the ledger is the record | Query the ledger |
| Edit a rate card | The database refuses; history would re-price | Publish a new version |
| Skip the realm | Two products silently share an idempotency key space | Set it once, in one constant |
