-- Seed one realm: a rate card, ceilings, plans, entitlements, a free tier.
-- Usage: make seed REALM=my-product
--
-- Every statement here is CONFIGURATION DATA. Changing any of it later needs no redeploy —
-- which is the point of it living in tables rather than in Go.
\set realm :realm
BEGIN;

-- 1. A rate card. IMMUTABLE and versioned: repricing INSERTS a new version and retires the old
--    one, so past ledger rows keep pricing identically and every dispute replay stays exact.
INSERT INTO rate_cards (realm, version, micros_per_credit)
VALUES (:'realm', '2026-01-01', 1000)                 -- 1 credit = $0.001
ON CONFLICT DO NOTHING;

INSERT INTO rate_card_entries (realm, version, rate_key, unit, micros_per_unit) VALUES
  (:'realm','2026-01-01','gpt-4o',     'llm_input_token',   2),
  (:'realm','2026-01-01','gpt-4o',     'llm_cached_token',  1),
  (:'realm','2026-01-01','gpt-4o',     'llm_output_token', 10),
  (:'realm','2026-01-01','gpt-4o-mini','llm_input_token',   1),
  (:'realm','2026-01-01','gpt-4o-mini','llm_output_token',  1)
ON CONFLICT DO NOTHING;

-- 2. Ceilings. The COUNT is yours: one, three or five all enforce and render correctly, because
--    the engine evaluates []Limit and the UI renders LimitView[].
INSERT INTO limits (realm, name, scope_kind, unit, max, window_kind, dur_seconds, warn_at, deny_code) VALUES
  (:'realm','scope_balance','organization','credit',      0,'balance', NULL, 0.80,'payment_required'),
  (:'realm','user_daily',   'user',        'credit',  10000,'daily',   NULL, 0.80,'limit_reached'),
  (:'realm','burst_hourly', 'user',        'credit',   2000,'rolling', 3600, 0.90,'limit_reached')
ON CONFLICT (realm, name) DO NOTHING;

-- 3. Plans, prices per currency, and what each unlocks beyond credits.
INSERT INTO plans (realm, code, name, kind, credit_allotment, billing_interval, active, sort_order) VALUES
  (:'realm','pack_10k',   'Credit pack (10k)','one_time',     10000, NULL,   true, 1),
  (:'realm','pro_monthly','Pro',              'subscription',100000,'month', true, 2)
ON CONFLICT (realm, code) DO NOTHING;

INSERT INTO plan_prices (plan_id, currency, minor_units)
SELECT id, 'USD', CASE code WHEN 'pack_10k' THEN 1000 ELSE 4900 END
FROM plans WHERE realm = :'realm'
ON CONFLICT DO NOTHING;

INSERT INTO plan_prices (plan_id, currency, minor_units)
SELECT id, 'EUR', CASE code WHEN 'pack_10k' THEN 900 ELSE 4500 END
FROM plans WHERE realm = :'realm'
ON CONFLICT DO NOTHING;

INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_bool, value_int, value_text)
SELECT id,'sso','flag',true,NULL,NULL          FROM plans WHERE realm=:'realm' AND code='pro_monthly'
UNION ALL
SELECT id,'seats','quota',NULL,25,NULL         FROM plans WHERE realm=:'realm' AND code='pro_monthly'
UNION ALL
SELECT id,'support_tier','enum',NULL,NULL,'gold' FROM plans WHERE realm=:'realm' AND code='pro_monthly'
ON CONFLICT DO NOTHING;

-- 4. The free tier: what a scope with no subscription gets.
INSERT INTO realm_defaults (realm, feature_key, value_kind, value_bool, value_int, value_text) VALUES
  (:'realm','sso',         'flag', false, NULL, NULL),
  (:'realm','seats',       'quota',NULL,  3,    NULL),
  (:'realm','support_tier','enum', NULL,  NULL, 'bronze')
ON CONFLICT DO NOTHING;

COMMIT;

\echo 'Seeded realm:' :realm
\echo 'Provider price mapping is NOT seeded — add plan_provider_prices once you have a provider.'
