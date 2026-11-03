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
-- max_entitlement resolves through here first. `reason` and `actor` are NOT NULL and not blank, and the
-- audit trigger in 0004 records every write in the same transaction.
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
    UNIQUE (realm, scope_kind, scope_id, feature_key),
    -- An unexplained override is indistinguishable from a bug (entitlement invariant 5). The API refuses one; so does the table,
    -- or the write that bypasses the API is the one nobody can explain. NOT NULL alone admits '' and '   ', and btrim trims only
    -- spaces: "has a character that is not whitespace" is what the API's TrimSpace means.
    CONSTRAINT entitlement_overrides_reason_not_blank CHECK (reason ~ '\S'),
    CONSTRAINT entitlement_overrides_actor_not_blank  CHECK (actor ~ '\S')
);

-- A limit whose size comes from an entitlement is sized by a NUMBER (D30). Naming a flag or an enum would be discovered by the
-- first customer's Admit, as a 503 quota_unavailable — or, under fail_open, as a scope with no ceiling at all. So it is refused
-- when it is configured, here, where it holds for the admin API and for a write that bypasses it. A key that is not declared yet is
-- accepted: keys can be declared in any order, and until then nothing grants it and the limit's own max applies.
CREATE FUNCTION limits_max_entitlement_is_quota() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM entitlement_keys k
               WHERE k.realm = NEW.realm AND k.key = NEW.max_entitlement AND k.kind <> 'quota') THEN
        RAISE EXCEPTION 'limit % is sized by entitlement %, which is not a quota', NEW.name, NEW.max_entitlement
            USING ERRCODE = 'check_violation', CONSTRAINT = 'limits_max_entitlement_is_quota';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER limits_max_entitlement_is_quota
    BEFORE INSERT OR UPDATE OF max_entitlement, realm ON limits
    FOR EACH ROW WHEN (NEW.max_entitlement IS NOT NULL)
    EXECUTE FUNCTION limits_max_entitlement_is_quota();

-- … and the key a limit is sized by cannot be turned into something that is not a number underneath it.
CREATE FUNCTION entitlement_keys_quota_in_use() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM limits l WHERE l.realm = NEW.realm AND l.max_entitlement = NEW.key) THEN
        RAISE EXCEPTION 'entitlement key % sizes a limit, so it stays a quota', NEW.key
            USING ERRCODE = 'check_violation', CONSTRAINT = 'entitlement_keys_quota_in_use';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER entitlement_keys_quota_in_use
    BEFORE UPDATE OF kind ON entitlement_keys
    FOR EACH ROW WHEN (OLD.kind = 'quota' AND NEW.kind <> 'quota')
    EXECUTE FUNCTION entitlement_keys_quota_in_use();

COMMIT;
