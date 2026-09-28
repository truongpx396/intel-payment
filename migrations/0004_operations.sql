-- 0004_operations: rollups, reconcile history, dead letters, audit, outbound events.
BEGIN;

-- Derived, never a billing input. Backs the burn rate, the series and the breakdown.
CREATE TABLE usage_daily (
    realm       TEXT   NOT NULL DEFAULT 'default',
    scope_kind  TEXT   NOT NULL,
    scope_id    TEXT   NOT NULL,
    day         DATE   NOT NULL,
    dimension   TEXT   NOT NULL,     -- the host's breakdown member (resource, feature, model…)
    actor_id    TEXT   NOT NULL DEFAULT '',  -- '' = aggregate; a member view filters to self (D20)
    credits     BIGINT NOT NULL DEFAULT 0,
    cost_micros BIGINT NOT NULL DEFAULT 0,
    events      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (realm, scope_kind, scope_id, day, dimension, actor_id)
);

-- One row per reconcile run per shard, whether or not anything drifted, so drift has a trend.
-- Outcomes are COUNTED, never netted: a +100 on one scope and a -100 on another are two findings,
-- not zero.
CREATE TABLE reconcile_runs (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    shard          INT         NOT NULL,
    kind           TEXT        NOT NULL CHECK (kind IN ('incremental','full')),
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ,
    scopes_checked INT         NOT NULL DEFAULT 0,
    in_sync        INT         NOT NULL DEFAULT 0,
    deferred       INT         NOT NULL DEFAULT 0,   -- intents in flight: not drift
    cold           INT         NOT NULL DEFAULT 0,
    regressions    INT         NOT NULL DEFAULT 0,   -- froze the shard
    drifted        INT         NOT NULL DEFAULT 0,
    abs_drift      BIGINT      NOT NULL DEFAULT 0    -- Σ |drift|, the figure that pages
);
CREATE INDEX reconcile_runs_shard_time_idx ON reconcile_runs (shard, started_at DESC);

-- Every scope and pool that disagreed at the SAME sequence number — a defect by construction.
CREATE TABLE reconcile_findings (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           UUID        NOT NULL REFERENCES reconcile_runs (id),
    realm            TEXT        NOT NULL,
    scope_kind       TEXT        NOT NULL,
    scope_id         TEXT        NOT NULL,
    pool             TEXT        NOT NULL,
    outcome          TEXT        NOT NULL CHECK (outcome IN ('drift','regression')),
    hot_seq          BIGINT      NOT NULL,
    applied_seq      BIGINT      NOT NULL,
    hot_balance      BIGINT,
    expected_balance BIGINT,                          -- booked + open suspense
    drift            BIGINT,
    healed           BOOLEAN     NOT NULL DEFAULT false,
    alarmed          BOOLEAN     NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX reconcile_findings_scope_idx ON reconcile_findings (realm, scope_kind, scope_id, created_at DESC);

CREATE TABLE dead_letters (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm       TEXT        NOT NULL DEFAULT 'default',
    subject     TEXT        NOT NULL,
    payload     JSONB       NOT NULL,
    attempts    INT         NOT NULL DEFAULT 0,
    last_error  TEXT,
    parked_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    replayed_at TIMESTAMPTZ
);

-- Every privileged action: a grant not from a verified webhook, an override, a manual reconcile or
-- rehydrate, a freeze, a rate-card publication, a plan or limit change.
CREATE TABLE audit_log (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm        TEXT        NOT NULL DEFAULT 'default',
    actor        TEXT        NOT NULL,
    action       TEXT        NOT NULL,
    subject_kind TEXT,
    subject_id   TEXT,
    before       JSONB,
    after        JSONB,
    reason       TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_realm_time_idx ON audit_log (realm, created_at DESC);

-- ------------------------------------------------ configuration is audited --
-- Configuration is data (D13), and data can be written by anything that holds a connection. So the
-- audit row and the cache-invalidation signal are written BY THE DATABASE, in the same transaction
-- as the change: the admin API sets intelpay.actor / intelpay.reason for the transaction, and a
-- direct SQL write is still recorded, attributed to the database role that made it (D37).
CREATE OR REPLACE FUNCTION audit_config_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    old_row jsonb := CASE WHEN TG_OP <> 'INSERT' THEN to_jsonb(OLD) END;
    new_row jsonb := CASE WHEN TG_OP <> 'DELETE' THEN to_jsonb(NEW) END;
    r       text  := coalesce(new_row->>'realm', old_row->>'realm');
BEGIN
    -- Plan children (prices, provider prices, entitlements) carry their realm through the plan.
    IF r IS NULL AND coalesce(new_row, old_row) ? 'plan_id' THEN
        SELECT realm INTO r FROM plans WHERE id = (coalesce(new_row, old_row)->>'plan_id')::uuid;
    END IF;
    r := coalesce(r, '(global)');   -- deployment-wide rows: hot_config, provider_accounts
    INSERT INTO audit_log (realm, actor, action, subject_kind, subject_id, before, after, reason)
    VALUES (r,
            coalesce(nullif(current_setting('intelpay.actor', true), ''), 'db:' || session_user),
            'config.' || lower(TG_OP),
            TG_TABLE_NAME,
            coalesce(new_row->>'id', old_row->>'id', new_row->>'code', old_row->>'code',
                     new_row->>'version', old_row->>'version', new_row->>'name', old_row->>'name',
                     new_row->>'key', old_row->>'key', new_row->>'feature_key', old_row->>'feature_key',
                     new_row->>'plan_id', old_row->>'plan_id'),
            old_row, new_row,
            nullif(current_setting('intelpay.reason', true), ''));
    -- Every process caching configuration LISTENs here, so a change needs no deploy and no TTL wait.
    PERFORM pg_notify('intelpay_config', json_build_object('table', TG_TABLE_NAME, 'realm', r)::text);
    RETURN NULL;
END $$;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'hot_config', 'credit_pools', 'rate_cards', 'rate_card_entries', 'limits', 'balance_watches',
        'provider_accounts', 'plans', 'plan_prices', 'plan_provider_prices', 'auto_recharge',
        'entitlement_keys', 'plan_entitlements', 'realm_defaults', 'entitlement_overrides']
    LOOP
        EXECUTE format('CREATE TRIGGER %I AFTER INSERT OR UPDATE OR DELETE ON %I
                        FOR EACH ROW EXECUTE FUNCTION audit_config_change()', t || '_audit', t);
    END LOOP;
END $$;

-- ------------------------------------------------------- outbound events ---
-- How a host hears about what happened, without ever touching this service's Redis (D35). Events
-- are appended here by the worker; each subscribed endpoint gets a signed, retried delivery; and
-- GET /v1/events serves the same log as a cursor feed for hosts that cannot receive webhooks.
CREATE TABLE event_endpoints (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm       TEXT        NOT NULL DEFAULT 'default',
    url         TEXT        NOT NULL CHECK (url ~ '^https://' OR url ~ '^http://(localhost|127\.0\.0\.1)(:|/|$)'),
    secret_ref  TEXT        NOT NULL,          -- the HMAC signing secret, by reference
    event_types TEXT[]      NOT NULL CHECK (cardinality(event_types) >= 1),
    active      BOOLEAN     NOT NULL DEFAULT true,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER event_endpoints_audit AFTER INSERT OR UPDATE OR DELETE ON event_endpoints
    FOR EACH ROW EXECUTE FUNCTION audit_config_change();

CREATE TABLE outbound_events (
    id          UUID        PRIMARY KEY,       -- UUID v7 from the application: time-ordered cursor
    realm       TEXT        NOT NULL DEFAULT 'default',
    type        TEXT        NOT NULL,          -- e.g. credits.low, payment.succeeded
    scope_kind  TEXT,
    scope_id    TEXT,
    payload     JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX outbound_events_feed_idx ON outbound_events (realm, id);

CREATE TABLE event_deliveries (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        UUID        NOT NULL REFERENCES outbound_events (id) ON DELETE CASCADE,
    endpoint_id     UUID        NOT NULL REFERENCES event_endpoints (id) ON DELETE CASCADE,
    status          TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivered','dead')),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status     INT,
    last_error      TEXT,
    delivered_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);
CREATE INDEX event_deliveries_pending_idx ON event_deliveries (next_attempt_at) WHERE status = 'pending';

COMMIT;
