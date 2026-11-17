-- What the browser tests need of the stack's realm (`default`): a rate card to price an event, and the two ceilings the reference screen draws —
-- a balance and a daily cap. Applied by `make e2e-seed`; idempotent, so it can be run against a live stack more than once.
--
-- One ordinary event (1,200 tokens in, 800 out at `gpt`) costs 660 credits, so with a daily cap of 1,000 a test reaches the warning
-- (at 80%) with a second, smaller event and the cap with a third. Nothing here is a secret: it is configuration, and the stack is local.
INSERT INTO rate_cards (realm, version, pricer) VALUES ('default', 'v1', 'table') ON CONFLICT DO NOTHING;

INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size, cost_micros_per_block) VALUES
  ('default', 'v1', 'gpt', 'in',  150000, 1000000, 100000),
  ('default', 'v1', 'gpt', 'out', 600000, 1000000, 0)
ON CONFLICT DO NOTHING;

INSERT INTO limits (realm, name, unit, max, window_kind, warn_at, deny_code, sort_order) VALUES
  ('default', 'scope_balance', 'credit', 0,    'balance', 0.80, 'payment_required', 0),
  ('default', 'daily_cap',     'credit', 1000, 'daily',   0.80, 'limit_reached',    1)
ON CONFLICT (realm, name) DO UPDATE SET max = EXCLUDED.max, warn_at = EXCLUDED.warn_at;
