-- 0001_metering_core: the engine's schema.
--
-- Every money table is keyed on the OPAQUE scope triple (realm, scope_kind, scope_id) — never on a
-- host's tenant column. Re-anchoring the billing subject is a change in how the host constructs a
-- Scope, not a migration across every money table.
--
-- How these tables stay consistent with the Redis hot tier (the sequence watermark, suspense,
-- window-bounded usage dedup, two-phase transfers) is specified in
-- specs/001-metering-billing-core/contracts/hot-path-consistency.md.
BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ------------------------------------------------------ partition helpers --
-- Future partitions are created ahead of need by billing.partitions.tick, and by this migration for
-- the current period. A DEFAULT partition catches a gap so a late tick can never reject a charge,
-- and `metering_default_partition_rows` pages if it ever holds a row: PostgreSQL cannot create a
-- partition whose range overlaps rows already sitting in DEFAULT, so a gap must be fixed promptly.
CREATE OR REPLACE FUNCTION ensure_monthly_partitions(parent regclass, months_ahead int)
RETURNS int LANGUAGE plpgsql AS $$
DECLARE
    m       date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
    created int  := 0;
    name    text;
BEGIN
    FOR i IN 0..months_ahead LOOP
        name := format('%s_y%sm%s', parent::text, to_char(m, 'YYYY'), to_char(m, 'MM'));
        IF to_regclass(name) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
                           name, parent, m::timestamptz, (m + interval '1 month')::timestamptz);
            created := created + 1;
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN created;
END $$;

CREATE OR REPLACE FUNCTION ensure_daily_partitions(parent regclass, days_ahead int)
RETURNS int LANGUAGE plpgsql AS $$
DECLARE
    d       date := (now() AT TIME ZONE 'UTC')::date;
    created int  := 0;
    name    text;
BEGIN
    FOR i IN 0..days_ahead LOOP
        name := format('%s_d%s', parent::text, to_char(d, 'YYYYMMDD'));
        IF to_regclass(name) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',
                           name, parent, d::timestamptz, (d + 1)::timestamptz);
            created := created + 1;
        END IF;
        d := d + 1;
    END LOOP;
    RETURN created;
END $$;

-- ------------------------------------------------------------- hot tier ---
-- Deployment-wide infrastructure state, deliberately NOT realm-scoped: every realm's scopes hash
-- into the same shards. `shards` is recorded once; a process configured with a different count
-- refuses to start, because two processes hashing scopes differently keep two sets of books.
CREATE TABLE hot_config (
    id         BOOLEAN     PRIMARY KEY DEFAULT true CHECK (id),   -- a single row
    shards     INT         NOT NULL CHECK (shards BETWEEN 1 AND 16384),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Authoritative shard generations. `meta:{s<n>}` in Redis mirrors this row. Bumping `gen` (after a
-- freeze and a drain) makes every hot account in the shard stale at once, so each rebuilds lazily
-- from the books — the one recovery procedure for loss, rollback and resharding.
CREATE TABLE hot_shards (
    shard         INT         PRIMARY KEY CHECK (shard >= 0),
    gen           BIGINT      NOT NULL DEFAULT 1 CHECK (gen >= 1),
    frozen        BOOLEAN     NOT NULL DEFAULT false,
    frozen_reason TEXT,
    node_replid   TEXT,                       -- the Redis replication id last seen serving it
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------------ pools --
-- Credit pools let one scope hold credits with different rules: promotional credits consumed
-- before paid ones, credits usable only on some resources. The pool `general` is implicit — it
-- always exists, applies to every resource and is drawn LAST, so it absorbs any overshoot.
CREATE TABLE credit_pools (
    realm        TEXT        NOT NULL DEFAULT 'default',
    name         TEXT        NOT NULL CHECK (name ~ '^[a-z][a-z0-9_]{0,31}$' AND name <> 'general'),
    priority     INT         NOT NULL,          -- lower is drawn first
    applies_to   TEXT[],                        -- resources; NULL = every resource
    description  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, name)
);

-- ---------------------------------------------------------------- balances --
-- BOOKED balances: the ledger's running sum per (scope, pool), maintained by the sole durable
-- writer in the same transaction as every ledger row. The hot tier equals booked + open suspense at
-- every sequence number; reconcile checks exactly that.
CREATE TABLE account_credits (
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    pool        TEXT        NOT NULL DEFAULT 'general',
    balance     BIGINT      NOT NULL DEFAULT 0,   -- signed: overshoot and allow_debt go negative
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id, pool)
);

-- The per-scope watermark: the highest CONTIGUOUSLY applied hot sequence number. Intents are
-- booked in sequence order; a redelivery, a gap and a regression are told apart by this row plus
-- the idempotency row, never by timing.
CREATE TABLE account_watermarks (
    realm        TEXT        NOT NULL DEFAULT 'default',
    scope_kind   TEXT        NOT NULL,
    scope_id     TEXT        NOT NULL,
    gen          BIGINT      NOT NULL DEFAULT 1,
    applied_seq  BIGINT      NOT NULL DEFAULT 0 CHECK (applied_seq >= 0),
    blocked      BOOLEAN     NOT NULL DEFAULT false,   -- block_and_flag, until an operator clears it
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id)
);
-- Reconcile checks scopes booked since its last run, so its cost follows activity, not history.
CREATE INDEX account_watermarks_updated_idx ON account_watermarks (updated_at);

-- ------------------------------------------------------------------ ledger --
-- The ACCOUNT OF RECORD. Append-only, partitioned by month. Old partitions are detached and
-- archived only after ledger_checkpoints covers them — never dropped while needed to prove a balance.
CREATE TABLE credit_ledger (
    id                 UUID        NOT NULL DEFAULT gen_random_uuid(),
    realm              TEXT        NOT NULL DEFAULT 'default',
    scope_kind         TEXT        NOT NULL,
    scope_id           TEXT        NOT NULL,
    pool               TEXT        NOT NULL DEFAULT 'general',

    -- SIGNED delta (D1): negative = consumption, positive = grant.
    delta              BIGINT      NOT NULL,

    -- Open vocabulary, deliberately NOT an enum. Engine-reserved values: writeoff, expiry,
    -- allocation_out, allocation_in, admin_adjustment. There is no `reconcile` type: reconcile
    -- heals the HOT side only and never books a row (hot-path-consistency.md §8).
    operation_type     TEXT        NOT NULL,

    -- Set on both rows of a Transfer: the counterparty. allocation_out and allocation_in share
    -- idem_key, and Σ ledger + Σ in-transit (credit_transfers) is invariant (metering invariant 14).
    counter_scope_kind TEXT,
    counter_scope_id   TEXT,

    -- Lookup only. Uniqueness lives in credit_idem / usage_idem (D21, D32).
    idem_key           TEXT        NOT NULL,

    -- The hot sequence number(s) this row books. `event` granularity: seq_from = seq_to.
    -- `rollup` granularity: one row per (scope, pool, resource, rate key, card version) per drain
    -- batch, covering seq_from..seq_to and event_count events archived in usage_events.
    gen                BIGINT      NOT NULL,
    seq_from           BIGINT      NOT NULL,
    seq_to             BIGINT      NOT NULL,
    event_count        INT         NOT NULL DEFAULT 1 CHECK (event_count >= 1),

    rate_card_version  TEXT,                     -- which card priced it (D6); NULL for non-priced rows
    cost_micros        BIGINT,                   -- informational upstream cost; never a billing input
    resource           TEXT,
    rate_key           TEXT,
    quantities         JSONB,                    -- the immutable event (summed, in rollup rows)
    subjects           JSONB,                    -- attribution dimensions (user, api_key, project…)
    actor_id           TEXT,
    ref                JSONB,                    -- trace/call/payment ids; audit only
    occurred_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at),
    CHECK (seq_to >= seq_from)
) PARTITION BY RANGE (created_at);

CREATE INDEX credit_ledger_scope_time_idx ON credit_ledger (realm, scope_kind, scope_id, created_at DESC);
CREATE INDEX credit_ledger_realm_time_idx ON credit_ledger (realm, created_at);
CREATE INDEX credit_ledger_idem_idx       ON credit_ledger (realm, idem_key);
CREATE INDEX credit_ledger_seq_idx        ON credit_ledger (realm, scope_kind, scope_id, gen, seq_from);

CREATE TABLE credit_ledger_default PARTITION OF credit_ledger DEFAULT;
DO $$ BEGIN PERFORM ensure_monthly_partitions('credit_ledger', 3); END $$;

-- Opening balances for the deep audit (billing.audit.tick): booked == balance + Σ ledger rows
-- created after through_created_at. Checkpoints are what make detaching an old partition safe.
CREATE TABLE ledger_checkpoints (
    realm              TEXT        NOT NULL DEFAULT 'default',
    scope_kind         TEXT        NOT NULL,
    scope_id           TEXT        NOT NULL,
    pool               TEXT        NOT NULL DEFAULT 'general',
    through_created_at TIMESTAMPTZ NOT NULL,
    balance            BIGINT      NOT NULL,
    rows_counted       BIGINT      NOT NULL,
    checked_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id, pool)
);

-- -------------------------------------------------- idempotency guards ------
-- Two guards with two lifetimes (D32).
--
-- credit_idem: operations that MINT or MOVE money — grants, refunds, transfers, expiry,
-- corrections, admin adjustments. Low volume, so PERMANENT. Unique per (realm, op, key): one
-- provider payment can never be granted to two scopes, and a usage key never collides with a grant.
-- It is its own NON-PARTITIONED table because a partitioned ledger cannot carry a unique constraint
-- that omits the partition key (D21).
CREATE TABLE credit_idem (
    realm       TEXT        NOT NULL DEFAULT 'default',
    op          TEXT        NOT NULL CHECK (op IN
                  ('grant','transfer','transfer_in','expiry','correction','adjustment')),
    idem_key    TEXT        NOT NULL,
    fingerprint TEXT        NOT NULL,          -- SHA-256 of the canonical operation (hex)
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    gen         BIGINT      NOT NULL,
    seq         BIGINT      NOT NULL,          -- (gen, seq) identifies a redelivery exactly
    ledger_id   UUID,                          -- NULL for corrections (they book nothing)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, op, idem_key)
);

-- usage_idem: metered consumption. High volume, so WINDOW-BOUNDED (UsageIdemWindow, default 72h):
-- daily partitions, dropped once past the window. Record refuses an event older than the window
-- (`422 stale_event`), so a retry that outlives its guard fails loudly instead of charging twice.
--
-- Uniqueness across the window is enforced by the writer: under a per-scope
-- pg_advisory_xact_lock it probes the window's partitions (at most window-days + 1 index lookups)
-- before inserting. The PRIMARY KEY below is per-partition and is the backstop inside a day.
CREATE TABLE usage_idem (
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    idem_key    TEXT        NOT NULL,
    fingerprint TEXT        NOT NULL,
    gen         BIGINT      NOT NULL,
    seq         BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id, idem_key, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE usage_idem_default PARTITION OF usage_idem DEFAULT;
DO $$ BEGIN PERFORM ensure_daily_partitions('usage_idem', 4); END $$;

-- ------------------------------------------------------------- suspense ----
-- Hot-tier movements the books do not accept: a replay that outlived its hot guard, a poison
-- intent, the unused part of an expiry's upper bound. Recorded in the same transaction that
-- advances the watermark; closed when the writer's correction intent is booked. Reconcile compares
-- hot against booked + OPEN suspense, so these are never mistaken for drift.
CREATE TABLE credit_suspense (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm           TEXT        NOT NULL DEFAULT 'default',
    scope_kind      TEXT        NOT NULL,
    scope_id        TEXT        NOT NULL,
    pool            TEXT        NOT NULL DEFAULT 'general',
    delta           BIGINT      NOT NULL,         -- the hot movement not booked
    reason          TEXT        NOT NULL CHECK (reason IN
                      ('duplicate','idem_conflict','poison','expiry_trueup')),
    gen             BIGINT      NOT NULL,
    seq             BIGINT      NOT NULL,         -- the intent that opened it
    idem_key        TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ
);
CREATE INDEX credit_suspense_open_idx ON credit_suspense (realm, scope_kind, scope_id, pool)
    WHERE status = 'open';

-- ------------------------------------------------------------- transfers ---
-- A transfer is two hot mutations in two shards joined by this record (hot-path-consistency.md §4).
-- Σ credit_ledger + Σ amount WHERE status = 'in_transit' is invariant across every step.
CREATE TABLE credit_transfers (
    realm        TEXT        NOT NULL DEFAULT 'default',
    idem_key     TEXT        NOT NULL,
    from_kind    TEXT        NOT NULL,
    from_id      TEXT        NOT NULL,
    from_pool    TEXT        NOT NULL DEFAULT 'general',
    to_kind      TEXT        NOT NULL,
    to_id        TEXT        NOT NULL,
    to_pool      TEXT        NOT NULL DEFAULT 'general',
    amount       BIGINT      NOT NULL CHECK (amount > 0),
    status       TEXT        NOT NULL DEFAULT 'in_transit' CHECK (status IN ('in_transit','settled')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at   TIMESTAMPTZ,
    PRIMARY KEY (realm, idem_key),
    CHECK (NOT (from_kind = to_kind AND from_id = to_id AND from_pool = to_pool))
);
CREATE INDEX credit_transfers_in_transit_idx ON credit_transfers (created_at) WHERE status = 'in_transit';

-- --------------------------------------------------------- outbox poison ---
-- Intents that exhausted their retry budget. Parked, never dropped: a dropped money intent is
-- silent under-billing (FR-043). Parking opens a `poison` suspense entry so the watermark advances
-- and one bad intent cannot block its scope.
CREATE TABLE credit_outbox_dead (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    op          TEXT        NOT NULL,
    gen         BIGINT      NOT NULL,
    seq         BIGINT      NOT NULL,
    payload     JSONB       NOT NULL,
    idem_key    TEXT        NOT NULL,
    attempts    INT         NOT NULL DEFAULT 0,
    last_error  TEXT,
    parked_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    replayed_at TIMESTAMPTZ
);

-- ----------------------------------------------------------- spend journal --
-- The durable home of a usage intent that has not reached the hot tier yet: on EVERY call under
-- Settlement=journal, and during a hot-store outage in any mode (so `fail_open` means served before
-- metered, never served unmetered). billing.journal.tick replays pending rows through the hot
-- function; the writer marks a row settled when it books the resulting intent.
CREATE TABLE spend_journal (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm             TEXT        NOT NULL DEFAULT 'default',
    scope_kind        TEXT        NOT NULL,
    scope_id          TEXT        NOT NULL,
    idem_key          TEXT        NOT NULL,
    fingerprint       TEXT        NOT NULL,
    amount            BIGINT      NOT NULL CHECK (amount >= 0),  -- priced once, at Record
    rate_card_version TEXT        NOT NULL,
    payload           JSONB       NOT NULL,                      -- the event, for the hot replay
    status            TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','settled')),
    attempts          INT         NOT NULL DEFAULT 0,
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at        TIMESTAMPTZ,
    UNIQUE (realm, scope_kind, scope_id, idem_key)
);
CREATE INDEX spend_journal_pending_idx ON spend_journal (next_attempt_at) WHERE status = 'pending';

-- ------------------------------------------------------------ usage archive --
-- Per-event detail under LedgerGranularity=rollup, where the ledger books one row per batch. The
-- default UsageArchive adapter; a columnar store is the scale-out adapter behind the same port.
-- Under `event` granularity it is unused, because the ledger row IS the event.
CREATE TABLE usage_events (
    realm             TEXT        NOT NULL DEFAULT 'default',
    scope_kind        TEXT        NOT NULL,
    scope_id          TEXT        NOT NULL,
    gen               BIGINT      NOT NULL,
    seq               BIGINT      NOT NULL,
    idem_key          TEXT        NOT NULL,
    pool_draws        JSONB       NOT NULL,
    credits           BIGINT      NOT NULL,
    resource          TEXT,
    rate_key          TEXT,
    rate_card_version TEXT,
    quantities        JSONB,
    subjects          JSONB,
    cost_micros       BIGINT,
    occurred_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id, gen, seq, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE usage_events_default PARTITION OF usage_events DEFAULT;
DO $$ BEGIN PERFORM ensure_daily_partitions('usage_events', 4); END $$;

-- -------------------------------------------------------------- rate cards --
-- Prices as IMMUTABLE, VERSIONED data (D13). A card names the Pricer that interprets it; `table`
-- (the default) is the data-driven pricer every deployment has, so a non-Go host can price anything
-- linear without writing code. Compiled pricers are selected by name from the PricerRegistry.
CREATE TABLE rate_cards (
    realm        TEXT        NOT NULL DEFAULT 'default',
    version      TEXT        NOT NULL,
    pricer       TEXT        NOT NULL DEFAULT 'table' CHECK (pricer ~ '^[a-z][a-z0-9_]*$'),
    params       JSONB       NOT NULL DEFAULT '{}'::jsonb,   -- read only by a named pricer
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at   TIMESTAMPTZ,
    PRIMARY KEY (realm, version)
);

-- Exactly one active card per realm. Enforced, not documented.
CREATE UNIQUE INDEX rate_cards_one_active_per_realm ON rate_cards (realm) WHERE retired_at IS NULL;

-- A price is a RATIONAL: credits_per_block credits for every block_size units. $0.15 per million
-- tokens at 1 credit = 1 µ$ is (150000, 1000000) — exact, where an integer "per unit" column
-- cannot express any price below one credit per unit (D31). The table pricer sums the exact
-- rationals and rounds UP once per event.
CREATE TABLE rate_card_entries (
    realm                 TEXT   NOT NULL DEFAULT 'default',
    version               TEXT   NOT NULL,
    rate_key              TEXT   NOT NULL,
    unit                  TEXT   NOT NULL,
    credits_per_block     BIGINT NOT NULL CHECK (credits_per_block >= 0),
    block_size            BIGINT NOT NULL DEFAULT 1 CHECK (block_size > 0),
    cost_micros_per_block BIGINT CHECK (cost_micros_per_block IS NULL OR cost_micros_per_block >= 0),
    PRIMARY KEY (realm, version, rate_key, unit),
    FOREIGN KEY (realm, version) REFERENCES rate_cards (realm, version) ON DELETE CASCADE
);

-- Insert-only, enforced by the database: editing a price in place silently re-prices history and
-- breaks every dispute replay against it. The one permitted update is retiring an active card.
CREATE OR REPLACE FUNCTION forbid_rate_card_edits() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'rate_cards' AND TG_OP = 'UPDATE' THEN
        IF (to_jsonb(OLD)->>'retired_at') IS NULL AND (to_jsonb(NEW)->>'retired_at') IS NOT NULL
           AND (to_jsonb(NEW) - 'retired_at') = (to_jsonb(OLD) - 'retired_at') THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'rate cards are insert-only: publish a new version instead (% on %)', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE TRIGGER rate_cards_insert_only BEFORE UPDATE OR DELETE ON rate_cards
    FOR EACH ROW EXECUTE FUNCTION forbid_rate_card_edits();
CREATE TRIGGER rate_card_entries_insert_only BEFORE UPDATE OR DELETE ON rate_card_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_rate_card_edits();

-- ------------------------------------------------------------------ limits --
-- Ceilings as per-realm data (D13). A limit's counter belongs to the charged scope or to one
-- SUBJECT within it (a user, an API key, a project, a job) named by the call (D30).
CREATE TABLE limits (
    id              UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    realm           TEXT          NOT NULL DEFAULT 'default',
    name            TEXT          NOT NULL,
    -- NULL = the charged scope itself. Otherwise a subject kind whose id every call MUST supply
    -- in Subjects; a call that omits it is refused (400 missing_subject), never silently unlimited.
    subject_kind    TEXT,
    unit            TEXT          NOT NULL,   -- 'credit', or a metered Unit the counter sums
    -- The realm default. For window_kind='balance' it is the permitted OVERDRAFT (0 = none).
    max             BIGINT        NOT NULL CHECK (max >= 0),
    -- When set, the effective max is the scope's entitlement quota for this key (plan, override
    -- or free tier — with provenance and audit); -1 there means unlimited. `max` is the fallback
    -- when no grant applies.
    max_entitlement TEXT,
    window_kind     TEXT          NOT NULL CHECK (window_kind IN ('balance','daily','hourly','rolling','job')),
    dur_seconds     INT           CHECK (dur_seconds IS NULL OR dur_seconds > 0),
    tz              TEXT          NOT NULL DEFAULT 'UTC',   -- IANA zone for daily/hourly resets
    warn_at         NUMERIC(3,2)  NOT NULL DEFAULT 0.80 CHECK (warn_at >= 0 AND warn_at <= 1),
    deny_code       TEXT          NOT NULL CHECK (deny_code IN ('payment_required','limit_reached')),
    resource        TEXT,         -- NULL = every resource; otherwise only that resource counts and is gated
    sort_order      INT           NOT NULL DEFAULT 0,
    active          BOOLEAN       NOT NULL DEFAULT true,
    UNIQUE (realm, name),
    CHECK (window_kind NOT IN ('rolling','job') OR dur_seconds IS NOT NULL),
    CHECK (window_kind <> 'balance' OR (subject_kind IS NULL AND unit = 'credit')),
    CHECK (window_kind <> 'job' OR subject_kind = 'job')
);

-- ------------------------------------------------------- balance watches ---
-- A low-water mark on one pool of one scope. The hot function flags the intent that crosses it,
-- and the writer publishes billing.balance.low.<tag> — which drives auto top-up and the host's
-- low-balance notice. Metering owns the mechanism; billing and hosts own what it triggers.
CREATE TABLE balance_watches (
    realm       TEXT        NOT NULL DEFAULT 'default',
    scope_kind  TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    pool        TEXT        NOT NULL DEFAULT 'general',
    key         TEXT        NOT NULL,          -- 'auto_recharge' | a host-defined name
    threshold   BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id, pool, key)
);

COMMIT;
