-- 0004_operations: rollups, reconcile history, dead letters, audit.
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

-- Every run writes a row whether or not it healed, so drift has a trend rather than only an
-- alert — which is how steady upward healing (silent under-billing) becomes visible.
CREATE TABLE reconcile_runs (
    id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm     TEXT        NOT NULL DEFAULT 'default',
    shard     TEXT        NOT NULL,
    bucket    TIMESTAMPTZ NOT NULL,
    expected  BIGINT      NOT NULL,
    observed  BIGINT      NOT NULL,
    drift     BIGINT      NOT NULL,
    tolerance BIGINT      NOT NULL,
    healed    BOOLEAN     NOT NULL DEFAULT false,
    alarmed   BOOLEAN     NOT NULL DEFAULT false,
    ran_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, shard, bucket)
);

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

-- Written for EVERY privileged action: a grant not from a verified webhook, an override, a
-- manual reconcile or rehydrate, a rate-card publication, a plan change.
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

COMMIT;
