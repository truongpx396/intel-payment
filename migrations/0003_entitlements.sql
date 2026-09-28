-- 0003_entitlements: what a plan unlocks besides credits (D8) — and, through
-- limits.max_entitlement, how large each metering ceiling is for this scope's plan (D30).
--
-- value_int = -1 means UNLIMITED, which is deliberately distinct from NULL ("unset").
BEGIN;

-- The declared capabilities of a realm. A key not declared here resolves to DENIED (entitlement
-- invariant 1), and an enum's `enum_order` is the ranking "highest wins" resolves by.
CREATE TABLE entitlement_keys (
    realm       TEXT        NOT NULL DEFAULT 'default',
    key         TEXT        NOT NULL,
    kind        TEXT        NOT NULL CHECK (kind IN ('flag','quota','enum')),
    enum_order  TEXT[],                         -- lowest to highest; required for enums
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, key),
    CHECK ((kind = 'enum') = (enum_order IS NOT NULL))
);

CREATE TABLE plan_entitlements (
    id          UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id     UUID    NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    feature_key TEXT    NOT NULL,
    value_kind  TEXT    NOT NULL CHECK (value_kind IN ('flag','quota','enum')),
    value_bool  BOOLEAN,
    value_int   BIGINT  CHECK (value_int IS NULL OR value_int >= -1),
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
    value_int   BIGINT  CHECK (value_int IS NULL OR value_int >= -1),
    value_text  TEXT,
    PRIMARY KEY (realm, feature_key)
);

-- Deliberate per-scope exceptions — including a per-customer metering ceiling, since a limit with
-- max_entitlement resolves through here first. `reason` and `actor` are NOT NULL, and the audit
-- trigger in 0004 records every write in the same transaction.
CREATE TABLE entitlement_overrides (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    feature_key TEXT        NOT NULL,
    value_kind  TEXT        NOT NULL CHECK (value_kind IN ('flag','quota','enum')),
    value_bool  BOOLEAN,
    value_int   BIGINT      CHECK (value_int IS NULL OR value_int >= -1),
    value_text  TEXT,
    expires_at  TIMESTAMPTZ,
    reason      TEXT        NOT NULL,
    actor       TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, scope_kind, scope_id, feature_key)
);

COMMIT;
