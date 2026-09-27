-- 0001_metering_core: the engine's schema.
--
-- Every table is keyed on the OPAQUE scope triple (realm, scope_kind, scope_id) — never on a
-- host's tenant column. That single choice is what makes this schema reusable: re-anchoring the
-- billing subject becomes a change in how the host constructs a Scope, not a migration across
-- every money table.
BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------- balances --
-- The DURABLE MIRROR of the hot balance. The ledger is authoritative; this row exists so a
-- cold start has somewhere to rehydrate to, and so a report need not sum the ledger.
CREATE TABLE account_credits (
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    balance     BIGINT      NOT NULL DEFAULT 0,   -- signed; negative only under allow_debt
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id)
);

-- ------------------------------------------------------------------ ledger --
-- The ACCOUNT OF RECORD. Append-only, partitioned by month.
CREATE TABLE credit_ledger (
    id                UUID        NOT NULL DEFAULT gen_random_uuid(),
    realm             TEXT        NOT NULL DEFAULT 'default',
    scope_kind        TEXT        NOT NULL,
    scope_id          TEXT        NOT NULL,

    -- SIGNED delta (design decision D1): negative = consumption, positive = grant.
    -- One column, so purchase/refund/chargeback/reconcile/writeoff all land here with no
    -- schema change and balance = SUM(delta) needs no CASE.
    delta             BIGINT      NOT NULL,

    -- Open vocabulary, deliberately NOT a Postgres enum: adding a value to an enum is a
    -- migration, and a host will invent operation types this schema cannot predict.
    operation_type    TEXT        NOT NULL,

    -- The durable idempotency backstop. REALM-SCOPED (D4): two host products both minting
    -- 'invoice-1' must not silently collapse into one another's no-op.
    idem_key          TEXT        NOT NULL,

    -- Which rate card priced this row (D6). Without it, a dispute replay silently applies
    -- today's rates and disagrees with the charge it is meant to verify.
    rate_card_version TEXT,

    cost_micros       BIGINT,                    -- informational upstream cost; never a billing input
    resource          TEXT,
    rate_key          TEXT,
    quantities        JSONB,                     -- the immutable event, so price is re-derivable
    actor_id          TEXT,                      -- attribution; opaque to the engine
    ref               JSONB,                     -- trace/call/payment ids; audit only
    occurred_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX credit_ledger_scope_time_idx ON credit_ledger (realm, scope_kind, scope_id, created_at DESC);
CREATE INDEX credit_ledger_realm_time_idx  ON credit_ledger (realm, created_at);
CREATE INDEX credit_ledger_idem_idx        ON credit_ledger (realm, idem_key);  -- lookup, not uniqueness

-- ------------------------------------------------------ the idempotency guard --
-- THE constraint the system's exactly-once guarantee rests on (FR-003, SC-002).
--
-- It lives in its own NON-PARTITIONED table because Postgres requires a unique constraint on a
-- partitioned table to include the partition key — so `PARTITION BY RANGE (created_at)` and a
-- global `UNIQUE (realm, idem_key)` on the same table are mutually exclusive. The inherited
-- design specified both, and is not constructible as written (design decision D21).
--
-- Splitting them keeps BOTH properties, and is strictly better than either compromise: the
-- guard is a narrow, hot, fully-cached table, and it OUTLIVES ledger partitions — so dropping
-- an aged partition cannot resurrect the ability to double-charge an old idem_key. A
-- credit-affecting write inserts here first, in the same transaction; no row returned means
-- this is a replay.
CREATE TABLE credit_idem (
    realm      TEXT        NOT NULL DEFAULT 'default',
    idem_key   TEXT        NOT NULL,
    ledger_id  UUID        NOT NULL,
    delta      BIGINT      NOT NULL,   -- lets a replay report the original outcome with no partition scan
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, idem_key)
);

-- Bootstrap partitions. A scheduled job creates future ones; DEFAULT catches a gap so a
-- missing partition can never reject a charge.
CREATE TABLE credit_ledger_default PARTITION OF credit_ledger DEFAULT;

-- --------------------------------------------------------- outbox poison ---
-- Terminal parking for intents that exhausted their retry budget. Nothing is ever dropped:
-- a dropped money intent is silent under-billing (FR-043).
CREATE TABLE credit_outbox_dead (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    payload     JSONB       NOT NULL,
    idem_key    TEXT        NOT NULL,
    attempts    INT         NOT NULL DEFAULT 0,
    last_error  TEXT,
    parked_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    replayed_at TIMESTAMPTZ
);

-- ----------------------------------------------------------- spend journal --
-- Used ONLY under SettlementDurability=journal: the durable intent written synchronously
-- before returning, which removes the under-bill RPO window of outbox mode (invariant 7).
CREATE TABLE spend_journal (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm             TEXT        NOT NULL DEFAULT 'default',
    scope_kind        TEXT        NOT NULL,
    scope_id          TEXT        NOT NULL,
    delta             BIGINT      NOT NULL,
    operation_type    TEXT        NOT NULL,
    rate_card_version TEXT,
    idem_key          TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at        TIMESTAMPTZ,
    UNIQUE (realm, idem_key)
);

-- -------------------------------------------------------------- rate cards --
-- Prices as IMMUTABLE, VERSIONED configuration data (D13): repricing is an operator action,
-- not a redeploy — which matters most for a host that does not control this release cycle.
CREATE TABLE rate_cards (
    realm             TEXT        NOT NULL DEFAULT 'default',
    version           TEXT        NOT NULL,
    micros_per_credit BIGINT      NOT NULL CHECK (micros_per_credit > 0),
    published_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at        TIMESTAMPTZ,
    PRIMARY KEY (realm, version)
);

-- Exactly one active card per realm. Enforced, not documented.
CREATE UNIQUE INDEX rate_cards_one_active_per_realm
    ON rate_cards (realm) WHERE retired_at IS NULL;

CREATE TABLE rate_card_entries (
    realm           TEXT   NOT NULL DEFAULT 'default',
    version         TEXT   NOT NULL,
    rate_key        TEXT   NOT NULL,
    unit            TEXT   NOT NULL,
    micros_per_unit BIGINT NOT NULL CHECK (micros_per_unit >= 0),
    PRIMARY KEY (realm, version, rate_key, unit),
    FOREIGN KEY (realm, version) REFERENCES rate_cards (realm, version) ON DELETE CASCADE
);

-- ------------------------------------------------------------------ limits --
-- Ceilings as per-realm configuration data (D13). This is what makes "the ceiling count is
-- data, not N hardcoded cards" true on both sides of the wire.
CREATE TABLE limits (
    id          UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    realm       TEXT          NOT NULL DEFAULT 'default',
    name        TEXT          NOT NULL,
    scope_kind  TEXT          NOT NULL,          -- whose counter; MAY differ from the charged scope
    unit        TEXT          NOT NULL,
    max         BIGINT        NOT NULL,
    window_kind TEXT          NOT NULL CHECK (window_kind IN ('balance','daily','hourly','rolling')),
    dur_seconds INT,                             -- rolling only
    warn_at     NUMERIC(3,2)  NOT NULL DEFAULT 0.80 CHECK (warn_at >= 0 AND warn_at <= 1),
    deny_code   TEXT          NOT NULL CHECK (deny_code IN ('payment_required','limit_reached')),
    resource    TEXT,                            -- NULL = applies to every resource
    sort_order  INT           NOT NULL DEFAULT 0,
    active      BOOLEAN       NOT NULL DEFAULT true,
    UNIQUE (realm, name),
    CHECK (window_kind <> 'rolling' OR dur_seconds IS NOT NULL)
);

COMMIT;
