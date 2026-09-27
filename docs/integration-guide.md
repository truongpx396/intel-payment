# Integration Guide

How a host product adopts `intel-payment`. The whole adoption is **three bindings and some
configuration rows** — if you find yourself editing anything under `metering/`, something has
gone wrong and this guide will say what.

---

## 1. Decide: library or service

| | Embedded library | Container service |
|---|---|---|
| Host language | Go only | any |
| `Admit` latency | in-process Redis, sub-ms | +1 network hop — **co-locate** (same node/AZ, or a sidecar over a Unix socket) |
| Who owns Redis/Postgres | the host | the service |
| Multiple products on one credit pool | no | **yes** — this is the reason the realm axis exists |
| Switching later | a wiring change in `cmd/`, no data migration | same, in reverse |

**Pick the library** if you are a Go monolith with one product. **Pick the service** otherwise.
Either way the durable writer is its own process, so you are running two things regardless.

---

## 2. Choose your three bindings

### Realm — which product this is

A stable slug. One realm per *product*, never per tenant — tenants are `Scope`s.

```go
const Realm = "acme-crm"
```

A single-product deployment can leave it at `default` and never think about it again. It costs
one field. It buys the ability to add a second product later without a key-space migration.

### Scope — who gets billed

Construct it from your request context. The engine never parses it, so the question is purely
"who does an invoice belong to?"

```go
func scopeOf(ctx context.Context) domain.Scope {
    return domain.Scope{Realm: Realm, Kind: "organization", ID: auth.OrgID(ctx)}
}
```

Pick the level where **money** lives, which is often not the level where **content** lives. A
workspace may be your isolation boundary while an organization holds the contract. If you later
need to move the billing subject, you change this function and nothing else — that is the
coupling this whole design exists to remove, and it was worth removing because the originating
product had to do it as a four-table migration.

### Pricer — what things cost

```go
type SeatPricer struct{ card RateCard }

func (p *SeatPricer) Price(_ context.Context, e domain.Event) (domain.Price, error) {
    rate, ok := p.card.Rates[e.RateKey]
    if !ok {
        // FAIL CLOSED. Pricing unknown usage at zero is how a product gives itself away.
        return domain.Price{}, fmt.Errorf("no rate for %q in card %q", e.RateKey, p.card.Version)
    }
    var micros int64
    for _, q := range e.Quantities {
        if q.Unit != UnitSeat {
            return domain.Price{}, fmt.Errorf("unpriceable unit %q", q.Unit)
        }
        if q.Amount < 0 {
            return domain.Price{}, fmt.Errorf("negative quantity")
        }
        micros += q.Amount * rate.MicrosPerSeat
    }
    // Round UP: rounding must never favour the customer over the books by accident.
    credits := (micros + p.card.MicrosPerCredit - 1) / p.card.MicrosPerCredit
    return domain.Price{
        Credits:         domain.Credits(credits),
        CostMicros:      micros,
        RateCardVersion: p.card.Version, // travels onto the ledger row — required
    }, nil
}
```

Four rules, each of which is tested by `PricerContract`:

1. **Pure.** No I/O, no clock, no globals. Reconciliation and dispute audits re-run this against
   an old event and must get the old number.
2. **Fail closed.** An unknown rate key or unit is an error, never a zero price.
3. **Round up.** Integer math only; rounding never under-bills.
4. **Report the card version.** Without it, a replay months later silently uses today's rates.

Reference implementations to copy: `llmtoken`, `seat`, `storagebyte`.

---

## 3. Configure your realm (SQL, not code)

Rate cards, ceilings and plans are configuration data — see
[configuration.md](./configuration.md) and the
[quickstart](../specs/001-metering-billing-core/quickstart.md) for the exact statements. The
point of them being data is that launching a price or a limit needs no deploy of this service,
which matters most when you do not control its release cycle.

---

## 4. Wire it

### Library

```go
meter, err := app.New(metering.Config{
    Realm: Realm, BalanceRedisURL: …, LedgerDSN: …,
    Settlement: metering.SettlementOutbox,
    AdmitFail:  metering.FailClosed,
}, app.Deps{Pricer: pricer, Balance: …, Ledger: …, Limits: …, RateCards: …, Bus: …})
```

Then depend on `ports.Meter` everywhere. Nothing in your handlers should name `app`.

### Service

```go
conn, _ := grpc.NewClient(os.Getenv("PAYMENT_GRPC_ADDR"), creds, grpc.WithDefaultCallOptions(...))
meter := grpcclient.New(conn)   // also a ports.Meter — your code cannot tell
```

Set a tight `Admit` deadline (~50 ms) and decide your fail policy deliberately: serve unmetered
during an outage, or refuse. There is no correct default, only a correct decision.

---

## 5. Instrument your call sites

```go
// BEFORE the expensive work. Pass MaxCost — it is what bounds overshoot.
adm, err := meter.Admit(ctx, scopeOf(ctx), domain.AdmitRequest{MaxCost: estimate})
if err != nil {
    return failPolicy(err)          // your decision, made once, in one place
}
if !adm.Allowed {
    return deny(adm.Exceeded)       // 402 with an upgrade path, or 409 with a reset time
}
if adm.Headroom < estimate {
    estimate = adm.Headroom         // resize rather than start work you cannot finish
}

out, produced, err := doTheWork(ctx, estimate)

// AFTER, with what was ACTUALLY produced — including on cancellation or partial failure.
// Reporting zero makes "start then abort" free; reporting the ceiling over-bills.
_, _ = meter.Record(ctx, domain.Event{
    Scope: scopeOf(ctx), Resource: "crm.enrich", RateKey: "standard",
    Quantities: produced,
    IdemKey:    requestID(ctx),     // stable across retries. This is the whole guarantee.
})
```

**`IdemKey` is the one thing you cannot get wrong.** It must be stable across retries of the
same logical operation and distinct between different operations. A request id works; a
timestamp or a fresh UUID per attempt turns every retry into a new charge.

---

## 6. Wire the surfaces

- **Credits screen** — proxy `GET /v1/credits` behind your own session (never expose a `/v1/*`
  credential to a browser) and render with `@intel-payment/credits-ui`, supplying `UnitLabels`,
  your `LedgerColumn`s and your `BillingAnchor`. Or consume the JSON and render your own.
- **Checkout** — `POST /v1/checkout` → redirect. On return, show **processing** and poll
  `GET /v1/fulfilment`. Never tell the user credits arrived because they came back from a
  redirect; the payment can still fail, and the resulting double-payment is the worst failure
  this surface has.
- **Feature gates** — `POST /v1/entitlements/check`, batched per page render. Render
  `Decision.Reason` and `UpgradePath` on refusal.
- **Notifications** — subscribe to `billing.warn.*`, `billing.blocked.*` and `billing.payment.*`
  and route them into your own notification system. This service emits facts; it owns no
  templates and sends no email.

---

## 7. Verify before you trust it

Run all of these in a provider sandbox before a real card touches it:

- [ ] A charge, then the **same charge replayed** — balance moves once.
- [ ] An exhausted balance — refused with `402` and an upgrade path, not a `500`.
- [ ] A spent daily ceiling — refused with `409` and a reset time.
- [ ] A **cancelled** operation — billed for what it produced.
- [ ] A purchase — credits appear only after the webhook, and `/v1/fulfilment` shows the transition.
- [ ] The **webhook replayed** from the provider's dashboard — balance does not move.
- [ ] A **tampered** webhook body — rejected `400`, counted as a security event.
- [ ] A refund — negative row appended, original untouched, and your `NegativeBalancePolicy` behaves as you expect.
- [ ] A failed renewal — grace window holds access, then suspends.
- [ ] **Kill Redis mid-traffic** — your fail policy holds, and rehydration restores the balance from the ledger before serving resumes.
- [ ] Alerts actually page someone: drift, outbox age, fail-open, webhook verification failures.

The last two are the ones teams skip, and they are the two that matter at 3am.

---

## Anti-patterns

| Don't | Why | Instead |
|---|---|---|
| Grant credits on a checkout return | The tab closes, the redirect replays, the payment fails afterwards | Poll `/v1/fulfilment` |
| Generate a fresh `IdemKey` per attempt | Every retry becomes a new charge | Derive it from the logical operation |
| Omit `MaxCost` | The overshoot bound becomes unknown | Pass an estimate; watch `metering_admit_uncapped_total` |
| Report a ceiling instead of actuals | Over-bills your own users | Report what was produced |
| `if plan == "pro"` | Scatters the rule; launching a plan becomes a code change everywhere | `Entitler.Allowed` |
| Share the balance Redis with your LRU cache | An eviction turns a cache miss into a billing incident | A dedicated `noeviction` + AOF instance |
| Expose `/v1/*` to a browser | Anyone can spend | Proxy behind your session |
| Read the hot balance as authoritative for an invoice | It is a fast copy; the ledger is the record | Query the ledger |
| Edit a rate card row | Breaks replayability for every past charge | Insert a new version |
| Skip the realm | Two products silently share an idempotency key space | Set it once, in one constant |
