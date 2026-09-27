-- 0003_entitlements: what a plan unlocks besides credits (D8).
--
-- value_int = -1 means UNLIMITED, which is deliberately distinct from NULL ("unset").
-- The distinction is load-bearing: the two must behave differently and usually do not.
BEGIN;

CREATE TABLE plan_entitlements (
    id          UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id     UUID    NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    feature_key TEXT    NOT NULL,
    value_kind  TEXT    NOT NULL CHECK (value_kind IN ('flag','quota','enum')),
    value_bool  BOOLEAN,
    value_int   BIGINT,
    value_text  TEXT,
    UNIQUE (plan_id, feature_key),
    CHECK (
        (value_kind = 'flag'  AND value_bool IS NOT NULL) OR
        (value_kind = 'quota' AND value_int  IS NOT NULL) OR
        (value_kind = 'enum'  AND value_text IS NOT NULL)
    )
);

-- The free tier: what a scope with no subscription gets.
CREATE TABLE realm_defaults (
    realm       TEXT    NOT NULL DEFAULT 'default',
    feature_key TEXT    NOT NULL,
    value_kind  TEXT    NOT NULL CHECK (value_kind IN ('flag','quota','enum')),
    value_bool  BOOLEAN,
    value_int   BIGINT,
    value_text  TEXT,
    PRIMARY KEY (realm, feature_key)
);

-- Deliberate per-scope exceptions. `reason` and `actor` are NOT NULL at the schema level:
-- an unexplained override is indistinguishable from a bug, and is usually discovered in the
-- middle of a revenue investigation.
CREATE TABLE entitlement_overrides (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    feature_key TEXT        NOT NULL,
    value_kind  TEXT        NOT NULL CHECK (value_kind IN ('flag','quota','enum')),
    value_bool  BOOLEAN,
    value_int   BIGINT,
    value_text  TEXT,
    expires_at  TIMESTAMPTZ,
    reason      TEXT        NOT NULL,
    actor       TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, scope_kind, scope_id, feature_key)
);

COMMIT;
