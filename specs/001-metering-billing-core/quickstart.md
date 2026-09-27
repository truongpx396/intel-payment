# Quickstart

## Run it as a container

```bash
git clone https://github.com/truongpx396/intel-payment && cd intel-payment
cp .env.example .env          # the defaults work for local development as-is
make up                       # postgres + redis + nats + migrations + paymentd + worker
curl -s localhost:8080/readyz  # {"status":"ready","migrations":"head"}
```

`make up` brings up the dependencies, runs migrations to head, then starts `paymentd`
(gRPC `:9090`, REST `:8080`) and `payment-worker` (the sole durable writer).

## Seed a realm

Prices, ceilings and plans are **configuration data**, not code — so this is SQL, and changing
any of it later needs no redeploy.

```bash
make seed REALM=my-product   # a rate card, three limits, two plans, a free tier
```

Or by hand, which is worth reading once because it is the whole configuration model:

```sql
-- 1. A rate card. Immutable and versioned: a price change INSERTS a new version.
INSERT INTO rate_cards (realm, version, micros_per_credit)
VALUES ('my-product', '2026-01-01', 1000);            -- 1 credit = $0.001

INSERT INTO rate_card_entries (realm, version, rate_key, unit, micros_per_unit) VALUES
  ('my-product','2026-01-01','gpt-4o','llm_input_token',   2),
  ('my-product','2026-01-01','gpt-4o','llm_output_token', 10);

-- 2. Ceilings. The COUNT is yours — one, three or five all render and enforce correctly.
INSERT INTO limits (realm, name, scope_kind, unit, max, window, warn_at, deny_code) VALUES
  ('my-product','scope_balance','organization','credit',      0,'balance',0.80,'payment_required'),
  ('my-product','user_daily',   'user',        'credit', 10000,'daily',  0.80,'limit_reached');

-- 3. A plan, its price, its allotment, and what it unlocks beyond credits.
INSERT INTO plans (realm, code, name, kind, credit_allotment, billing_interval, active)
VALUES ('my-product','pro_monthly','Pro','subscription',100000,'month',true);

INSERT INTO plan_prices (plan_id, currency, minor_units)
SELECT id,'USD',4900 FROM plans WHERE realm='my-product' AND code='pro_monthly';

INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_bool, value_int)
SELECT id,'sso','flag',true,NULL     FROM plans WHERE realm='my-product' AND code='pro_monthly'
UNION ALL
SELECT id,'seats','quota',NULL,25    FROM plans WHERE realm='my-product' AND code='pro_monthly';

-- 4. The free tier.
INSERT INTO realm_defaults (realm, feature_key, value_kind, value_bool, value_int) VALUES
  ('my-product','sso',  'flag', false, NULL),
  ('my-product','seats','quota',NULL,  3);
```

## Meter something

```bash
# Gate BEFORE the work. max_cost is what bounds overshoot — pass it.
curl -s localhost:8080/v1/admit -H 'Authorization: Bearer dev-token' \
  -d '{"scope":{"realm":"my-product","kind":"organization","id":"org_1"},"max_cost":500}'
# {"allowed":false,"exceeded":{"name":"scope_balance","deny_code":"payment_required"},"headroom":0}

# Grant some credits (privileged — in production only the webhook path calls this).
curl -s localhost:8080/v1/grant -H 'Authorization: Bearer dev-token' \
  -H 'Idempotency-Key: seed-1' \
  -d '{"scope":{"realm":"my-product","kind":"organization","id":"org_1"},
       "amount":100000,"idem_key":"seed-1","reason":"signup_grant"}'

# Settle real usage — the quantities ACTUALLY produced, not a ceiling.
curl -s localhost:8080/v1/record -H 'Authorization: Bearer dev-token' \
  -H 'Idempotency-Key: call-abc' \
  -d '{"scope":{"realm":"my-product","kind":"organization","id":"org_1"},
       "resource":"llm.chat","rate_key":"gpt-4o",
       "quantities":[{"unit":"llm_input_token","amount":1200},
                     {"unit":"llm_output_token","amount":800}],
       "idem_key":"call-abc"}'
# {"idem_key":"call-abc","applied":true,"delta":-11,"balance":99989,"rate_card_version":"2026-01-01"}

# Replay it. Nothing happens — that is the whole point.
curl -s localhost:8080/v1/record -H 'Authorization: Bearer dev-token' \
  -H 'Idempotency-Key: call-abc' -d '{…same body…}'
# {"idem_key":"call-abc","applied":false,"delta":0,"balance":99989}
```

## Use it as a library instead

```go
import (
    "github.com/truongpx396/intel-payment/metering"
    "github.com/truongpx396/intel-payment/metering/app"
    "github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/llmtoken"
)

meter, err := app.New(metering.Config{
    Realm:           "my-product",
    BalanceRedisURL: os.Getenv("REDIS_URL"),
    LedgerDSN:       os.Getenv("DATABASE_URL"),
    Settlement:      metering.SettlementOutbox,
    AdmitFail:       metering.FailClosed,
    NegativeBalance: metering.ClampToZero,
}, app.Deps{
    Pricer:    pricer,      // yours, or one of the three reference pricers
    Balance:   redisstore.New(rdb),
    Ledger:    pgstore.New(db),
    Limits:    pgstore.NewLimitStore(db),
    RateCards: pgstore.NewRateCardStore(db),
    Bus:       natsbus.New(nc),
})

// Your handlers depend on ports.Meter. Swap in grpcclient.New(conn) later and nothing
// downstream changes — the linters guarantee nothing depended on more than the interface.
```

## Take a payment

```bash
# 1. Configure a provider
echo 'STRIPE_SECRET_KEY=sk_test_…'      >> .env
echo 'STRIPE_WEBHOOK_SECRET=whsec_…'    >> .env
echo 'PAYMENT_PROVIDERS_ENABLED=stripe' >> .env

# 2. Map the plan to the provider's price
psql "$PAYMENT_LEDGER_DSN" -c "
  INSERT INTO plan_provider_prices (plan_id, provider, currency, provider_price_id)
  SELECT id,'stripe','USD','price_123' FROM plans
  WHERE realm='my-product' AND code='pro_monthly';"

# 3. Point the provider at the webhook (locally, via the Stripe CLI)
stripe listen --forward-to localhost:8080/webhooks/stripe

# 4. Start a checkout
curl -s localhost:8080/v1/checkout -H 'Authorization: Bearer dev-token' \
  -d '{"scope":{"realm":"my-product","kind":"organization","id":"org_1"},
       "plan_code":"pro_monthly","currency":"USD",
       "success_url":"https://app.example/done","cancel_url":"https://app.example/plans"}'
# {"redirect_url":"https://checkout.stripe.com/…"}
```

Pay in the provider's test mode. Credits appear **when the verified webhook lands**, not on the
redirect. Poll `/v1/fulfilment` to see `processing` → `granted`, then replay the webhook from the
provider's dashboard and confirm the balance does not move.

## Verify the guarantees

```bash
make test              # units + every conformance suite
make test-integration  # Testcontainers: real Redis + Postgres
make lint              # go-arch-lint + golangci-lint — the portability boundary
make verify-portability # metering/ builds with no dependency on billing/ or entitlement/
```

## Next

- [docs/integration-guide.md](../../docs/integration-guide.md) — adopting it in a host, end to end
- [docs/configuration.md](../../docs/configuration.md) — every knob and its failure mode
- [docs/security.md](../../docs/security.md) — caller auth, realm binding, webhook posture
- [docs/operations.md](../../docs/operations.md) — the runbook: drift, outbox, rehydrate, dunning
