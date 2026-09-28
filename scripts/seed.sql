-- Seed one realm: pools, a rate card, ceilings, plans, entitlements, a free tier, a test provider
-- account. Usage: make seed REALM=my-product
--
-- Every statement here is CONFIGURATION DATA. Changing any of it later needs no redeploy — and every
-- change is audited by the database and signalled to caches (0004's audit trigger), whether it
-- comes through the admin API or through psql.
\set realm :realm
BEGIN;
SELECT set_config('intelpay.actor', 'seed-script', true),
       set_config('intelpay.reason', 'initial realm configuration', true);

-- 1. Accounting unit: 1 credit = 1 µ$ (one millionth of a US dollar) of LIST price. Fine enough
--    that rounding up once per event costs < $0.000001; coarse enough that int64 holds $9 trillion.
--    How credits are DISPLAYED (dollars, "1,000-credit" units) is UnitLabels.scale in the UI.

-- 2. Pools. `general` is implicit and drawn last. Promotional credits are drawn first.
INSERT INTO credit_pools (realm, name, priority, applies_to, description) VALUES
  (:'realm', 'promo', 10, NULL, 'Promotional credits — consumed before paid ones')
ON CONFLICT DO NOTHING;

-- 3. A rate card. IMMUTABLE (a trigger refuses edits) and versioned. Prices are RATIONALS:
--    credits_per_block per block_size units — $0.15 per 1M tokens is exactly (150000, 1000000).
INSERT INTO rate_cards (realm, version, pricer)
VALUES (:'realm', '2026-01-01', 'table')
ON CONFLICT DO NOTHING;

INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size, cost_micros_per_block) VALUES
  (:'realm','2026-01-01','gpt-4o',      'llm_input_token',   2500000, 1000000,  2500000),
  (:'realm','2026-01-01','gpt-4o',      'llm_cached_token',  1250000, 1000000,  1250000),
  (:'realm','2026-01-01','gpt-4o',      'llm_output_token', 10000000, 1000000, 10000000),
  (:'realm','2026-01-01','gpt-4o-mini', 'llm_input_token',    150000, 1000000,   150000),
  (:'realm','2026-01-01','gpt-4o-mini', 'llm_cached_token',    75000, 1000000,    75000),
  (:'realm','2026-01-01','gpt-4o-mini', 'llm_output_token',   600000, 1000000,   600000)
ON CONFLICT DO NOTHING;

-- 4. Capabilities the realm declares. An undeclared key is denied.
INSERT INTO entitlement_keys (realm, key, kind, enum_order, description) VALUES
  (:'realm', 'sso',                'flag',  NULL, 'Single sign-on'),
  (:'realm', 'seats',              'quota', NULL, 'Members per organization'),
  (:'realm', 'support_tier',       'enum',  ARRAY['bronze','silver','gold'], 'Support level'),
  (:'realm', 'daily_user_credits', 'quota', NULL, 'Per-member daily spend ceiling, in credits')
ON CONFLICT DO NOTHING;

-- 5. Ceilings. The COUNT is yours. `user_daily` belongs to a SUBJECT (the member making the call),
--    and its size comes from the plan through `max_entitlement` — Free and Pro differ with no code.
INSERT INTO limits (realm, name, subject_kind, unit, max, max_entitlement, window_kind, dur_seconds, tz, warn_at, deny_code) VALUES
  (:'realm','scope_balance', NULL,   'credit',        0, NULL,                 'balance', NULL, 'UTC', 0.80, 'payment_required'),
  (:'realm','user_daily',    'user', 'credit', 10000000, 'daily_user_credits', 'daily',   NULL, 'UTC', 0.80, 'limit_reached'),
  (:'realm','burst_hourly',  'user', 'credit',  2000000, NULL,                 'rolling', 3600, 'UTC', 0.90, 'limit_reached')
ON CONFLICT (realm, name) DO NOTHING;

-- 6. Plans, prices per currency, and what each unlocks beyond credits.
INSERT INTO plans (realm, code, name, kind, credit_allotment, pool, billing_interval, active, sort_order) VALUES
  (:'realm','pack_10',    'Credit pack ($10)', 'one_time',     10000000, 'general', NULL,    true, 1),
  (:'realm','pro_monthly','Pro',               'subscription', 60000000, 'general', 'month', true, 2)
ON CONFLICT (realm, code) DO NOTHING;

INSERT INTO plan_prices (plan_id, currency, minor_units)
SELECT id, 'USD', CASE code WHEN 'pack_10' THEN 1000 ELSE 4900 END
FROM plans WHERE realm = :'realm'
ON CONFLICT DO NOTHING;

INSERT INTO plan_prices (plan_id, currency, minor_units)
SELECT id, 'EUR', CASE code WHEN 'pack_10' THEN 900 ELSE 4500 END
FROM plans WHERE realm = :'realm'
ON CONFLICT DO NOTHING;

INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_bool, value_int, value_text)
SELECT id,'sso','flag',true,NULL,NULL                     FROM plans WHERE realm=:'realm' AND code='pro_monthly'
UNION ALL
SELECT id,'seats','quota',NULL,25,NULL                    FROM plans WHERE realm=:'realm' AND code='pro_monthly'
UNION ALL
SELECT id,'support_tier','enum',NULL,NULL,'gold'          FROM plans WHERE realm=:'realm' AND code='pro_monthly'
UNION ALL
SELECT id,'daily_user_credits','quota',NULL,50000000,NULL FROM plans WHERE realm=:'realm' AND code='pro_monthly'
ON CONFLICT DO NOTHING;

-- 7. The free tier: what a scope with no subscription gets.
INSERT INTO realm_defaults (realm, feature_key, value_kind, value_bool, value_int, value_text) VALUES
  (:'realm','sso',                'flag', false, NULL,     NULL),
  (:'realm','seats',              'quota',NULL,  3,        NULL),
  (:'realm','support_tier',       'enum', NULL,  NULL,     'bronze'),
  (:'realm','daily_user_credits', 'quota',NULL,  10000000, NULL)
ON CONFLICT DO NOTHING;

-- 8. A TEST-mode Stripe account for this realm. Secrets are referenced, never stored.
INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref)
VALUES ('stripe-test', 'stripe', ARRAY[:'realm'], 'test', 'env:STRIPE_SECRET_KEY', 'env:STRIPE_WEBHOOK_SECRET')
ON CONFLICT (id) DO UPDATE
  SET realms = (SELECT array_agg(DISTINCT r) FROM unnest(provider_accounts.realms || EXCLUDED.realms) r);

COMMIT;

\echo 'Seeded realm:' :realm
\echo 'Map plans to provider prices once you have them: POST /v1/admin/plans/{code}/provider-prices'
