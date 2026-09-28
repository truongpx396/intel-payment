# Quickstart

> **Intended interface.** Everything below the "Verify the design today" section needs the Phase 1
> implementation; that section runs now.

## Verify the design today

The parts of the design that can be executed without Go already are, and CI runs them:

```bash
scripts/verify-hot-path.sh          # reference Redis Functions on Redis 7 in CLUSTER mode (needs Docker)
make verify-schema                  # every migration + seed + assertions + the Phase 2 draft, on an EMPTY Postgres
scripts/check-spec-drift.sh         # no document describes a retired mechanism as current
```

## Run it as a container

```bash
git clone https://github.com/truongpx396/intel-payment && cd intel-payment
cp .env.example .env          # the defaults work for local development as-is
make up                       # postgres + redis + migrations + paymentd + worker (no broker)
curl -s localhost:8080/readyz  # {"status":"ready","migrations":"head","shards":256}
```

`make up` brings up the dependencies, runs migrations to head, then starts `paymentd`
(gRPC `:9090`, REST `:8080`) and `payment-worker` (the sole durable writer).

Each caller authenticates with a service credential carrying its realms and privileges
(`PAYMENT_SERVICE_TOKENS` in `.env`). The examples below read three of them from the environment —
one per privilege, because a spend producer must never hold the credential that mints credits:

```bash
export PAYMENT_TOKEN=…         # privilege: record
export PAYMENT_GRANT_TOKEN=…   # privilege: grant
export PAYMENT_ADMIN_TOKEN=…   # privilege: admin
```

## Seed a realm

Prices, ceilings, pools and plans are **configuration data**, not code:

```bash
make seed REALM=my-product   # a promo pool, a rate card, three limits, two plans, a free tier, a test Stripe account
```

[scripts/seed.sql](../../scripts/seed.sql) is worth reading once — it is the whole configuration
model. The essentials:

- **1 credit = 1 µ$ of list price.** Fine enough that rounding up once per event costs under a
  millionth of a dollar; the UI decides how to display it.
- **Prices are rationals.** gpt-4o-mini input at $0.15 per million tokens is
  `credits_per_block = 150000, block_size = 1000000` — exact.
- **`user_daily` belongs to a subject** (the member making the call), and its size comes from the
  plan: 10 000 000 credits ($10) on the free tier, 50 000 000 on Pro.

The same through the admin API, which audits every change with your name:

```bash
curl -s localhost:8080/v1/admin/limits/user_daily -X PUT -H "Authorization: Bearer $PAYMENT_ADMIN_TOKEN" -d '{
  "subject_kind":"user","unit":"credit","max":10000000,"max_entitlement":"daily_user_credits",
  "window":"daily","tz":"UTC","warn_at":0.8,"deny_code":"limit_reached"}'
```

## Meter something

```bash
# Gate BEFORE the work. max_cost bounds overshoot; subjects key the per-member ceiling.
curl -s localhost:8080/v1/admit -H "Authorization: Bearer $PAYMENT_TOKEN" \
  -d '{"scope":{"kind":"organization","id":"org_1"},"resource":"llm.chat",
       "subjects":{"user":"u_1"},"max_cost":50000}'
# {"allowed":false,"exceeded":{"name":"scope_balance","deny_code":"payment_required"},"headroom":0}

# Grant signup credits into the promo pool (privileged — in production only the webhook processor mints).
curl -s localhost:8080/v1/grant -H "Authorization: Bearer $PAYMENT_GRANT_TOKEN" -H 'Idempotency-Key: signup-org_1' \
  -d '{"scope":{"kind":"organization","id":"org_1"},"pool":"promo","amount":5000000,"reason":"signup_grant"}'

# Settle real usage — the quantities ACTUALLY produced, with a stable occurred_at.
curl -s localhost:8080/v1/record -H "Authorization: Bearer $PAYMENT_TOKEN" -H 'Idempotency-Key: call-abc' \
  -d '{"scope":{"kind":"organization","id":"org_1"},"resource":"llm.chat","rate_key":"gpt-4o",
       "subjects":{"user":"u_1"},"occurred_at":"2026-09-28T10:00:00Z",
       "quantities":[{"unit":"llm_input_token","amount":1200},{"unit":"llm_output_token","amount":800}]}'
# {"idem_key":"call-abc","applied":true,"delta":-11000,"balance":4989000,"seq":2,"rate_card_version":"2026-01-01"}
#   1,200 × 2.5 µ$ + 800 × 10 µ$ = 11,000 credits, exactly — drawn from the promo pool first.

# Replay it. Nothing happens — that is the whole point.
curl -s localhost:8080/v1/record … -H 'Idempotency-Key: call-abc' -d '{…same body…}'
# {"idem_key":"call-abc","applied":false,"delta":0,"balance":4989000,"seq":2}

# Reuse the key for a DIFFERENT request. Refused, never silently swallowed.
curl -s localhost:8080/v1/record … -H 'Idempotency-Key: call-abc' -d '{…different quantities…}'
# 409 {"error":{"code":"idempotency_conflict", …}}
```

## Use it as a library instead

```go
meter, err := app.New(metering.Config{
    Realm:           "my-product",
    BalanceRedisURL: os.Getenv("REDIS_URL"),
    LedgerDSN:       os.Getenv("DATABASE_URL"),
}, app.Deps{
    Pricers:   pricing.Registry{"table": table.Pricer{}},
    Balance:   redisstore.New(rdb),
    Books:     pgstore.New(db),
    Journal:   pgstore.NewJournal(db),
    Limits:    pgstore.NewLimitStore(db),
    Pools:     pgstore.NewPoolStore(db),
    RateCards: pgstore.NewRateCardStore(db),
    Bus:       redisstreams.New(rdb),
})
// Your handlers depend on ports.Meter. Swap in grpcclient.New(conn) later and nothing changes.
```

## Take a payment

```bash
# 1. The seed created provider account `stripe-test`, whose secrets are REFERENCES to these variables:
echo 'STRIPE_SECRET_KEY=sk_test_…'    >> .env
echo 'STRIPE_WEBHOOK_SECRET=whsec_…'  >> .env

# 2. Map the plan to the provider's price, for that account and currency
curl -s localhost:8080/v1/admin/plans/pro_monthly/provider-prices -H "Authorization: Bearer $PAYMENT_ADMIN_TOKEN" \
  -d '{"provider_account":"stripe-test","currency":"USD","provider_price_id":"price_123"}'

# 3. Point the provider at the account's webhook route (locally, via the Stripe CLI)
stripe listen --forward-to localhost:8080/webhooks/stripe/stripe-test

# 4. Start a checkout
curl -s localhost:8080/v1/checkout -H "Authorization: Bearer $PAYMENT_TOKEN" \
  -d '{"scope":{"kind":"organization","id":"org_1"},"plan_code":"pro_monthly","currency":"USD",
       "success_url":"https://app.example/done","cancel_url":"https://app.example/plans"}'
# {"redirect_url":"https://checkout.stripe.com/…"}
```

Pay in test mode. Credits appear **when the verified webhook is processed**, not on the redirect.
Poll `/v1/fulfilment` to see `processing` → `granted`, then replay the webhook from the provider's
dashboard and confirm the balance does not move.

## Verify the guarantees

```bash
make test              # units + every conformance suite
make test-integration  # Testcontainers: real Redis (cluster mode) + Postgres, including a forced failover
make lint              # go-arch-lint + golangci-lint — the portability boundary
make ci                # everything CI runs
```

## Next

- [docs/integration-guide.md](../../docs/integration-guide.md) — adopting it in a host, end to end
- [docs/configuration.md](../../docs/configuration.md) — every knob and its failure mode
- [docs/security.md](../../docs/security.md) — caller auth, realm binding, the money store, webhook posture
- [docs/operations.md](../../docs/operations.md) — the runbook
