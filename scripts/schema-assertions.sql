-- Behavioural assertions over the shipped baseline (migrations/) plus scripts/seed.sql for realm
-- 'verify'. Run by scripts/verify-schema.sh. Any failed assertion raises and stops the run.
\set ON_ERROR_STOP 1
SET client_min_messages = notice;

CREATE OR REPLACE FUNCTION pg_temp.ok(label text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN RAISE NOTICE 'ok   %', label; END $$;

-- ------------------------------------------------ the guards the system rests on
DO $$ BEGIN
  PERFORM 1 FROM pg_constraint WHERE conrelid = 'credit_idem'::regclass AND contype = 'p'
     AND pg_get_constraintdef(oid) = 'PRIMARY KEY (realm, op, idem_key)';
  IF NOT FOUND THEN RAISE EXCEPTION 'FAIL: credit_idem PRIMARY KEY (realm, op, idem_key) missing'; END IF;
  PERFORM pg_temp.ok('credit_idem is keyed (realm, op, idem_key) — minting is unique per realm and op');

  IF (SELECT relkind FROM pg_class WHERE oid = 'credit_idem'::regclass) <> 'r' THEN
    RAISE EXCEPTION 'FAIL: credit_idem must stay non-partitioned (D21)'; END IF;
  PERFORM pg_temp.ok('credit_idem is not partitioned, so it outlives ledger partitions');

  IF (SELECT relkind FROM pg_class WHERE oid = 'usage_idem'::regclass) <> 'p' THEN
    RAISE EXCEPTION 'FAIL: usage_idem must be partitioned (window-bounded)'; END IF;
  PERFORM pg_temp.ok('usage_idem is partitioned, so its window is bounded by dropping partitions');

  PERFORM 1 FROM pg_constraint WHERE conrelid = 'payment_events'::regclass AND contype = 'u'
     AND pg_get_constraintdef(oid) = 'UNIQUE (provider_account, provider_event_id)';
  IF NOT FOUND THEN RAISE EXCEPTION 'FAIL: webhook replay guard missing'; END IF;
  PERFORM pg_temp.ok('payment_events carries the replay guard per provider account');

  PERFORM 1 FROM pg_indexes WHERE indexname = 'rate_cards_one_active_per_realm';
  IF NOT FOUND THEN RAISE EXCEPTION 'FAIL: one-active-card index missing'; END IF;
  PERFORM pg_temp.ok('exactly one active rate card per realm is enforced');
END $$;

-- ------------------------------------------------ partitions exist ahead of need
DO $$
DECLARE m date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
        d date := (now() AT TIME ZONE 'UTC')::date;
BEGIN
  FOR i IN 0..3 LOOP
    IF to_regclass(format('credit_ledger_y%sm%s', to_char(m, 'YYYY'), to_char(m, 'MM'))) IS NULL THEN
      RAISE EXCEPTION 'FAIL: ledger partition for % missing', m; END IF;
    m := (m + interval '1 month')::date;
  END LOOP;
  PERFORM pg_temp.ok('ledger partitions exist for this month and the next three');
  FOR i IN 0..4 LOOP
    IF to_regclass(format('usage_idem_d%s', to_char(d + i, 'YYYYMMDD'))) IS NULL THEN
      RAISE EXCEPTION 'FAIL: usage_idem partition for % missing', d + i; END IF;
  END LOOP;
  PERFORM pg_temp.ok('usage_idem daily partitions exist for today and the next four days');
  IF ensure_monthly_partitions('credit_ledger', 3) <> 0 THEN
    RAISE EXCEPTION 'FAIL: ensure_monthly_partitions is not idempotent'; END IF;
  PERFORM pg_temp.ok('partition creation is idempotent (safe for a duplicated tick)');
END $$;

-- ------------------------------------------------ rate cards are insert-only
DO $$ BEGIN
  BEGIN
    UPDATE rate_card_entries SET credits_per_block = credits_per_block + 1 WHERE realm = 'verify';
    RAISE EXCEPTION 'FAIL: a rate card entry was edited in place';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('editing a price in place is refused by the database');
  END;
  BEGIN
    DELETE FROM rate_cards WHERE realm = 'verify';
    RAISE EXCEPTION 'FAIL: a rate card was deleted';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('deleting a rate card is refused');
  END;
  BEGIN
    UPDATE rate_cards SET pricer = 'other' WHERE realm = 'verify';
    RAISE EXCEPTION 'FAIL: a rate card was edited in place';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('changing a published card''s pricer is refused');
  END;
END $$;

DO $$ BEGIN
  BEGIN
    INSERT INTO rate_cards (realm, version) VALUES ('verify', '2026-07-01');
    RAISE EXCEPTION 'FAIL: two active cards in one realm';
  EXCEPTION WHEN unique_violation THEN
    PERFORM pg_temp.ok('publishing a second active card without retiring the first is refused');
  END;
END $$;
BEGIN;
UPDATE rate_cards SET retired_at = now() WHERE realm = 'verify' AND retired_at IS NULL;
INSERT INTO rate_cards (realm, version) VALUES ('verify', '2026-07-01');
DO $$ BEGIN PERFORM pg_temp.ok('repricing = retire the active card + publish a new version'); END $$;
ROLLBACK;

-- ------------------------------------------------ configuration is audited by the database
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM audit_log
   WHERE realm = 'verify' AND actor = 'seed-script' AND reason = 'initial realm configuration';
  IF n < 10 THEN RAISE EXCEPTION 'FAIL: expected seed config writes to be audited, found % rows', n; END IF;
  PERFORM pg_temp.ok(format('seeding wrote %s audit rows, attributed and explained', n));
END $$;

BEGIN;
UPDATE limits SET max = 20000000 WHERE realm = 'verify' AND name = 'user_daily';
DO $$
DECLARE a record;
BEGIN
  SELECT * INTO a FROM audit_log WHERE subject_kind = 'limits' ORDER BY created_at DESC, id DESC LIMIT 1;
  IF a.actor NOT LIKE 'db:%' OR a.action <> 'config.update'
     OR (a.before->>'max')::bigint <> 10000000 OR (a.after->>'max')::bigint <> 20000000 THEN
    RAISE EXCEPTION 'FAIL: a direct SQL limit change was not audited correctly: %', row_to_json(a);
  END IF;
  PERFORM pg_temp.ok('a direct SQL write is still audited, attributed to the database role, with before/after');
END $$;
ROLLBACK;

-- ------------------------------------------------ constraints that encode invariants
DO $$ BEGIN
  BEGIN
    INSERT INTO limits (realm, name, subject_kind, unit, max, window_kind, deny_code)
    VALUES ('verify', 'bad_balance', 'user', 'credit', 0, 'balance', 'payment_required');
    RAISE EXCEPTION 'FAIL: a balance limit was attached to a subject';
  EXCEPTION WHEN check_violation THEN
    PERFORM pg_temp.ok('a balance limit belongs to the charged scope, never a subject');
  END;
  BEGIN
    INSERT INTO limits (realm, name, subject_kind, unit, max, window_kind, dur_seconds, deny_code)
    VALUES ('verify', 'bad_job', 'user', 'credit', 500, 'job', 3600, 'limit_reached');
    RAISE EXCEPTION 'FAIL: a job window without the job subject';
  EXCEPTION WHEN check_violation THEN
    PERFORM pg_temp.ok('a job budget is keyed by the job subject');
  END;
  BEGIN
    INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size)
    VALUES ('verify', '2026-01-01', 'x', 'y', 1, 0);
    RAISE EXCEPTION 'FAIL: a zero block size';
  EXCEPTION WHEN check_violation THEN
    PERFORM pg_temp.ok('a price rational cannot have a zero denominator');
  END;
  BEGIN
    INSERT INTO credit_transfers (realm, idem_key, from_kind, from_id, to_kind, to_id, amount)
    VALUES ('verify', 't-self', 'org', 'o1', 'org', 'o1', 10);
    RAISE EXCEPTION 'FAIL: a transfer to itself';
  EXCEPTION WHEN check_violation THEN
    PERFORM pg_temp.ok('a transfer cannot target its own source pool');
  END;
  BEGIN
    INSERT INTO credit_pools (realm, name, priority) VALUES ('verify', 'general', 1);
    RAISE EXCEPTION 'FAIL: general pool redefined';
  EXCEPTION WHEN check_violation THEN
    PERFORM pg_temp.ok('the implicit general pool cannot be redefined');
  END;
  BEGIN
    INSERT INTO payment_events (provider_account, provider, provider_event_id, event_type,
                                payload_hash, status, parked_body)
    VALUES ('stripe-test', 'stripe', 'evt_1', 'x', 'h', 'processed', '\x00');
    RAISE EXCEPTION 'FAIL: a raw body stored for a verified event';
  EXCEPTION WHEN check_violation THEN
    PERFORM pg_temp.ok('a raw webhook body is kept only while verification is unavailable');
  END;
  BEGIN
    INSERT INTO event_endpoints (realm, url, secret_ref, event_types)
    VALUES ('verify', 'http://example.com/hook', 'env:X', ARRAY['credits.low']);
    RAISE EXCEPTION 'FAIL: a plaintext outbound endpoint';
  EXCEPTION WHEN check_violation THEN
    PERFORM pg_temp.ok('outbound webhooks require https (localhost excepted)');
  END;
END $$;

BEGIN;
INSERT INTO payments (realm, scope_kind, scope_id, provider_account, provider_payment_id, kind,
                      amount_minor, currency, credits_granted, status, idem_key)
VALUES ('verify', 'org', 'o1', 'stripe-test', 'pi_1', 'one_time', 1000, 'USD', 10000000, 'succeeded', 'pi_1');
DO $$ BEGIN
  BEGIN
    UPDATE payments SET refunded_minor = 1001 WHERE provider_payment_id = 'pi_1';
    RAISE EXCEPTION 'FAIL: refunded more than was paid';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    UPDATE payments SET credits_revoked = 10000001 WHERE provider_payment_id = 'pi_1';
    RAISE EXCEPTION 'FAIL: revoked more credits than were granted';
  EXCEPTION WHEN check_violation THEN NULL; END;
  PERFORM pg_temp.ok('refunds and clawbacks cannot exceed what was paid and granted');
END $$;
ROLLBACK;

-- ------------------------------------------------ idempotency guard behaviour
BEGIN;
INSERT INTO credit_idem (realm, op, idem_key, fingerprint, scope_kind, scope_id, gen, seq)
VALUES ('verify', 'grant', 'pi_1', 'fp', 'org', 'o1', 1, 1);
DO $$ BEGIN
  -- the same key under another op is a different operation
  INSERT INTO credit_idem (realm, op, idem_key, fingerprint, scope_kind, scope_id, gen, seq)
  VALUES ('verify', 'transfer', 'pi_1', 'fp', 'org', 'o1', 1, 2);
  -- the same payment granted to a second scope is refused
  BEGIN
    INSERT INTO credit_idem (realm, op, idem_key, fingerprint, scope_kind, scope_id, gen, seq)
    VALUES ('verify', 'grant', 'pi_1', 'fp', 'org', 'o2', 1, 1);
    RAISE EXCEPTION 'FAIL: one payment granted to two scopes';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  -- the same key in another realm is independent
  INSERT INTO credit_idem (realm, op, idem_key, fingerprint, scope_kind, scope_id, gen, seq)
  VALUES ('other', 'grant', 'pi_1', 'fp', 'org', 'o1', 1, 1);
  PERFORM pg_temp.ok('minting keys: unique per realm and op, independent across realms and ops');
END $$;
ROLLBACK;

DO $$ BEGIN RAISE NOTICE 'baseline assertions passed'; END $$;
