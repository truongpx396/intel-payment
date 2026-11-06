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

-- A message that used its delivery budget, parked whole and never dropped (FR-043). The bus delivers a dead-letter envelope at least
-- once, so (subject, source_id) — the subject it failed on and the bus's id for that delivery — is the letter's identity: parking
-- the same envelope twice is one letter, and the page fires once per parking. The payload is BYTEA, not JSONB: a message nobody
-- could read is the one an operator most needs to see, and it is kept as it arrived.
CREATE TABLE dead_letters (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm                TEXT        NOT NULL DEFAULT 'default',
    subject              TEXT        NOT NULL,
    source_id            TEXT        NOT NULL,
    payload              BYTEA       NOT NULL,
    attempts             INT         NOT NULL DEFAULT 0,
    last_error           TEXT,
    parked_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A replay takes the letter for a lease, publishes, then sets replayed_at. A holder killed between the two lets the lease
    -- lapse, and the next replay completes the work.
    replay_claimed_until TIMESTAMPTZ,
    replayed_at          TIMESTAMPTZ,
    UNIQUE (subject, source_id)
);
CREATE INDEX dead_letters_open_idx ON dead_letters (realm) WHERE replayed_at IS NULL;

-- Parked, never dropped: what a letter says about the failure is written once, and only a replay moves it, forward.
CREATE FUNCTION dead_letters_keep() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dead letters are never deleted: a parked message is kept (FR-043)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF ROW(NEW.id, NEW.realm, NEW.subject, NEW.source_id, NEW.payload, NEW.attempts, NEW.last_error, NEW.parked_at)
       IS DISTINCT FROM ROW(OLD.id, OLD.realm, OLD.subject, OLD.source_id, OLD.payload, OLD.attempts, OLD.last_error, OLD.parked_at) THEN
        RAISE EXCEPTION 'a dead letter is kept as it was parked; only its replay changes (FR-043)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.replayed_at IS NOT NULL AND NEW.replayed_at IS DISTINCT FROM OLD.replayed_at THEN
        RAISE EXCEPTION 'a replayed dead letter stays replayed (FR-043)' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER dead_letters_keep BEFORE UPDATE OR DELETE ON dead_letters
    FOR EACH ROW EXECUTE FUNCTION dead_letters_keep();

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

-- ------------------------------------------------- operator requests (R112) --
-- What an operator asks of the engine — reconcile a shard, recover it, rebuild an account, replay a dead letter, sweep a provider account —
-- is recorded here before anything is done, with the person and the reason, and worked afterwards by the payment-worker that holds the sole
-- durable writer. paymentd has no writer, so it cannot do these itself; and a record outlives both the process that asked and the worker that
-- dies halfway through. (realm, key) is the Idempotency-Key: a retry is the same row, and a key reused for a different request is refused by
-- the application. seq orders requests made in the same instant; ids are random.
CREATE TABLE admin_operations (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    seq         BIGINT      GENERATED ALWAYS AS IDENTITY,
    realm       TEXT        NOT NULL,
    kind        TEXT        NOT NULL CHECK (kind IN ('reconcile','recover','rehydrate','replay','subdrift','unblock','adjust')),
    params      JSONB       NOT NULL,
    actor       TEXT        NOT NULL CHECK (btrim(actor) <> ''),
    reason      TEXT        NOT NULL CHECK (btrim(reason) <> ''),
    key         TEXT        NOT NULL CHECK (btrim(key) <> ''),
    via         TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','succeeded','failed')),
    result      JSONB,
    error       TEXT,
    attempts    INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_until TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (realm, key)
);
CREATE INDEX admin_operations_queue_idx ON admin_operations (created_at, seq) WHERE status IN ('pending', 'running');

-- What was asked, by whom and why is the record of a privileged action: the database refuses to let it change, whoever holds the
-- connection, and refuses to forget it. An outcome is final; a claim moves a request from pending to running and may be taken over.
CREATE FUNCTION admin_operations_keep() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an operator request is never deleted: it is the record of who asked for what, and why'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.id <> OLD.id OR NEW.seq <> OLD.seq OR NEW.realm <> OLD.realm OR NEW.kind <> OLD.kind OR NEW.params <> OLD.params
       OR NEW.actor <> OLD.actor OR NEW.reason <> OLD.reason OR NEW.key <> OLD.key OR NEW.via <> OLD.via OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'what an operator asked, who asked and why never change'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('succeeded', 'failed') THEN
        RAISE EXCEPTION 'an outcome is final'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'pending' AND OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'a request that has been claimed is never made pending again'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER admin_operations_keep BEFORE UPDATE OR DELETE ON admin_operations
    FOR EACH ROW EXECUTE FUNCTION admin_operations_keep();

-- Audited by the database, in the transaction that made the change (D37): a request when it is made, an outcome when it is recorded. A
-- claim is bookkeeping and is not an action. The actor is the person the request names, on whose behalf the worker acted; a row written
-- in SQL, without the API, is audited the same way.
CREATE FUNCTION admin_operations_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO audit_log (realm, actor, action, subject_kind, subject_id, after, reason)
        VALUES (NEW.realm, NEW.actor, 'admin.' || NEW.kind || '.requested', 'admin_operations', NEW.id::text,
                jsonb_build_object('kind', NEW.kind, 'params', NEW.params, 'key', NEW.key, 'via', NEW.via), NEW.reason);
    ELSIF NEW.status IN ('succeeded', 'failed') AND OLD.status NOT IN ('succeeded', 'failed') THEN
        INSERT INTO audit_log (realm, actor, action, subject_kind, subject_id, after, reason)
        VALUES (NEW.realm, NEW.actor, 'admin.' || NEW.kind || '.' || NEW.status, 'admin_operations', NEW.id::text,
                jsonb_build_object('status', NEW.status, 'result', NEW.result, 'error', NEW.error, 'attempts', NEW.attempts), NEW.reason);
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER admin_operations_audit AFTER INSERT OR UPDATE ON admin_operations
    FOR EACH ROW EXECUTE FUNCTION admin_operations_audit();

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

-- The id is a UUID v7 the application issues UNDER A LOCK (events.Store.Append), strictly after the newest id in the log, so ids sort in
-- the order events became visible and a feed's cursor never skips one. `source` is the bus message (or the fact) the event came from:
-- recording it twice is one event, which is what makes the recorder idempotent under an at-least-once bus. An event never changes.
CREATE TABLE outbound_events (
    id          UUID        PRIMARY KEY,
    realm       TEXT        NOT NULL DEFAULT 'default',
    type        TEXT        NOT NULL,          -- e.g. credits.low, payment.succeeded
    scope_kind  TEXT,
    scope_id    TEXT,
    payload     JSONB       NOT NULL,
    source      TEXT        NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX outbound_events_feed_idx ON outbound_events (realm, id);

CREATE FUNCTION outbound_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'outbound events never change: a host that has the event must be able to trust it (% on %)', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END $$;
CREATE TRIGGER outbound_events_immutable BEFORE UPDATE ON outbound_events
    FOR EACH ROW EXECUTE FUNCTION outbound_events_immutable();

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
