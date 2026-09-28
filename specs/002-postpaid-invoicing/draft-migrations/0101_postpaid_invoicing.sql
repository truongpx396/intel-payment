-- DRAFT — 0101_postpaid_invoicing: the post-paid settlement model (spec 002).
--
-- NOT SHIPPED. This file lives with the Phase 2 spec, not in migrations/, because Phase 2 is not
-- built and migrations are forward-only from the first release: a table shipped before its feature
-- exists can never be removed again (D38). CI applies it on top of the shipped baseline so the
-- draft stays constructible; it moves into migrations/ with the Phase 2 code, renumbered then.
--
-- Additive to the baseline: new tables and nullable columns, nothing re-keyed and no money table
-- touched. The one non-additive statement is WIDENING two CHECK constraints on `limits` (a new
-- window and a new deny code), which rejects no existing row.
BEGIN;

-- ------------------------------------------------------- settlement model ---
CREATE TABLE scope_settlement (
    realm           TEXT        NOT NULL DEFAULT 'default',
    scope_kind      TEXT        NOT NULL,
    scope_id        TEXT        NOT NULL,
    model           TEXT        NOT NULL CHECK (model IN ('prepaid_credits','postpaid_invoice','hybrid')),
    -- Post-paid gates on a period-to-date spend cap rather than a positive balance.
    -- NULL = uncapped, which should be a decision rather than an omission.
    spend_cap_minor BIGINT,
    currency        CHAR(3),
    period_anchor   TEXT        NOT NULL DEFAULT 'calendar_month'
                                CHECK (period_anchor IN ('calendar_month','anniversary')),
    anchor_day      SMALLINT    CHECK (anchor_day BETWEEN 1 AND 28),
    effective_from  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id)
);

-- ---------------------------------------------------------------- periods ---
CREATE TABLE billing_periods (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm              TEXT        NOT NULL DEFAULT 'default',
    scope_kind         TEXT        NOT NULL,
    scope_id           TEXT        NOT NULL,
    period_start       TIMESTAMPTZ NOT NULL,
    period_end         TIMESTAMPTZ NOT NULL,
    status             TEXT        NOT NULL CHECK (status IN ('open','closing','closed','invoiced')),
    -- Stamped at open: a customer's terms never change under them mid-cycle.
    model              TEXT        NOT NULL CHECK (model IN ('prepaid_credits','postpaid_invoice','hybrid')),
    closed_at          TIMESTAMPTZ,
    closing_claimed_by TEXT,
    closing_claimed_at TIMESTAMPTZ,
    UNIQUE (realm, scope_kind, scope_id, period_start),
    CHECK (period_end > period_start)
);

-- One open period per scope. The `open -> closing` conditional UPDATE against this is the
-- idempotent claim that makes period close single-owner (FR-105): two schedulers, one invoice.
CREATE UNIQUE INDEX billing_periods_one_open_per_scope
    ON billing_periods (realm, scope_kind, scope_id) WHERE status = 'open';

-- -------------------------------------------------------- rate schedules ----
-- Insert-only and versioned, exactly like metering rate cards, so a closed period re-rates
-- identically forever (FR-107).
CREATE TABLE rate_schedules (
    realm        TEXT        NOT NULL DEFAULT 'default',
    version      TEXT        NOT NULL,
    product      TEXT        NOT NULL,
    unit         TEXT        NOT NULL,
    -- STORED, never inferred. Graduated and volume differ materially on identical input; a
    -- system that guesses which was meant mis-bills every customer (invariant 6).
    kind         TEXT        NOT NULL CHECK (kind IN ('flat','graduated','volume','package')),
    package_size BIGINT      CHECK (package_size IS NULL OR package_size > 0),
    currency     CHAR(3)     NOT NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at   TIMESTAMPTZ,
    PRIMARY KEY (realm, version, product, unit),
    CHECK (kind <> 'package' OR package_size IS NOT NULL)
);

CREATE UNIQUE INDEX rate_schedules_one_active
    ON rate_schedules (realm, product, unit) WHERE retired_at IS NULL;

CREATE TABLE rate_schedule_tiers (
    realm            TEXT   NOT NULL DEFAULT 'default',
    version          TEXT   NOT NULL,
    product          TEXT   NOT NULL,
    unit             TEXT   NOT NULL,
    tier_index       INT    NOT NULL,
    up_to            BIGINT,                 -- NULL = unbounded (the last tier)
    unit_price_micros BIGINT NOT NULL DEFAULT 0,
    flat_micros      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (realm, version, product, unit, tier_index),
    FOREIGN KEY (realm, version, product, unit)
        REFERENCES rate_schedules (realm, version, product, unit) ON DELETE CASCADE
);

-- ------------------------------------------------------------ commitments ---
CREATE TABLE commitments (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm           TEXT        NOT NULL DEFAULT 'default',
    scope_kind      TEXT        NOT NULL,
    scope_id        TEXT        NOT NULL,
    -- minimum: spend at least X; a shortfall becomes its OWN line and never inflates a usage
    --          line, because an invoice may not lie about what happened (invariant 7).
    -- prepaid: X paid up front, drawn down by usage.
    kind            TEXT        NOT NULL CHECK (kind IN ('minimum','prepaid')),
    amount_minor    BIGINT      NOT NULL CHECK (amount_minor > 0),
    remaining_minor BIGINT      CHECK (remaining_minor IS NULL OR remaining_minor >= 0),
    currency        CHAR(3)     NOT NULL,
    starts_at       TIMESTAMPTZ NOT NULL,
    ends_at         TIMESTAMPTZ NOT NULL,
    rollover_unused BOOLEAN     NOT NULL DEFAULT false,
    CHECK (ends_at > starts_at),
    CHECK (kind <> 'prepaid' OR remaining_minor IS NOT NULL)
);

-- -------------------------------------------------------------- discounts ---
CREATE TABLE discounts (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm           TEXT        NOT NULL DEFAULT 'default',
    code            TEXT        NOT NULL,
    kind            TEXT        NOT NULL CHECK (kind IN ('percentage','fixed')),
    percent         NUMERIC(5,2) CHECK (percent IS NULL OR (percent > 0 AND percent <= 100)),
    amount_minor    BIGINT      CHECK (amount_minor IS NULL OR amount_minor > 0),
    currency        CHAR(3),
    applies_to      TEXT        NOT NULL CHECK (applies_to IN ('invoice','product','line')),
    product         TEXT,
    valid_from      TIMESTAMPTZ,
    valid_until     TIMESTAMPTZ,
    max_redemptions INT,
    redemptions     INT         NOT NULL DEFAULT 0,
    UNIQUE (realm, code),
    CHECK (kind <> 'percentage' OR percent IS NOT NULL),
    CHECK (kind <> 'fixed'      OR (amount_minor IS NOT NULL AND currency IS NOT NULL)),
    CHECK (applies_to <> 'product' OR product IS NOT NULL)
);

CREATE TABLE scope_discounts (
    realm         TEXT        NOT NULL DEFAULT 'default',
    scope_kind    TEXT        NOT NULL,
    scope_id      TEXT        NOT NULL,
    discount_id   UUID        NOT NULL REFERENCES discounts (id) ON DELETE CASCADE,
    applied_count INT         NOT NULL DEFAULT 0,
    attached_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id, discount_id)
);

-- --------------------------------------------------- invoice numbering -----
-- NOT a Postgres SEQUENCE: sequences intentionally gap on rollback, and a gap in an invoice
-- series is an audit finding in most jurisdictions (invariant 8). Allocated under
-- SELECT ... FOR UPDATE inside the finalize transaction, so a rolled-back finalize consumes
-- no number.
CREATE TABLE invoice_numbers (
    realm      TEXT   NOT NULL DEFAULT 'default',
    kind       TEXT   NOT NULL CHECK (kind IN ('invoice','credit_note')),
    next_value BIGINT NOT NULL DEFAULT 1 CHECK (next_value > 0),
    format     TEXT   NOT NULL DEFAULT 'INV-{realm}-{seq:06d}',
    PRIMARY KEY (realm, kind)
);

-- ---------------------------------------------------------------- invoices --
CREATE TABLE invoices (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm               TEXT        NOT NULL DEFAULT 'default',
    scope_kind          TEXT        NOT NULL,
    scope_id            TEXT        NOT NULL,
    period_id           UUID        NOT NULL REFERENCES billing_periods (id),
    number              TEXT        NOT NULL,
    subtotal_minor      BIGINT      NOT NULL,
    tax_minor           BIGINT      NOT NULL DEFAULT 0,
    total_minor         BIGINT      NOT NULL,
    currency            CHAR(3)     NOT NULL,
    -- `draft` is the ONLY mutable status. Past `finalized` the document is immutable and the
    -- triggers at the end of this file enforce it (invariant 3) — immutability is the artifact's
    -- only real property, and application code is not where you defend it.
    status              TEXT        NOT NULL CHECK (status IN
                          ('draft','finalized','sent','paid','void','uncollectible')),
    issued_at           TIMESTAMPTZ,
    due_at              TIMESTAMPTZ,
    finalized_at        TIMESTAMPTZ,
    provider_invoice_id TEXT,
    -- Discounts and commitments that did NOT apply, with reasons. "What happened to my
    -- discount?" deserves an answer from the record.
    not_applied         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, number),
    UNIQUE (period_id)                         -- one invoice per period: the durable half of FR-105
);
CREATE INDEX invoices_scope_idx ON invoices (realm, scope_kind, scope_id, created_at DESC);

CREATE TABLE invoice_lines (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id          UUID        NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    line_index          INT         NOT NULL,
    kind                TEXT        NOT NULL CHECK (kind IN
                          ('usage','subscription','minimum_shortfall','commitment_drawdown',
                           'discount','credit','tax','residual')),
    description         TEXT        NOT NULL,
    product             TEXT,
    unit                TEXT,
    quantity            BIGINT,
    schedule_kind       TEXT,                  -- named on the line; see rate_schedules.kind
    schedule_version    TEXT,
    rate_card_versions  TEXT[],                -- every metering card that priced the events behind it
    amount_minor        BIGINT      NOT NULL,  -- SIGNED: negative for discounts/drawdowns/credits
    currency            CHAR(3)     NOT NULL,
    -- Traceability (invariant 4). A line without a source is not finalizable, because
    -- "trust the total" is not an answer a finance team accepts.
    source_period_start TIMESTAMPTZ,
    source_period_end   TIMESTAMPTZ,
    source_resource     TEXT,
    source_ledger_query TEXT,
    source_event_count  BIGINT,
    UNIQUE (invoice_id, line_index)
);

-- ------------------------------------------------------------ credit notes --
CREATE TABLE credit_notes (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm              TEXT        NOT NULL DEFAULT 'default',
    scope_kind         TEXT        NOT NULL,
    scope_id           TEXT        NOT NULL,
    number             TEXT        NOT NULL,
    against_invoice_id UUID        NOT NULL REFERENCES invoices (id),
    total_minor        BIGINT      NOT NULL,
    currency           CHAR(3)     NOT NULL,
    -- NOT NULL at the schema level: an unexplained credit note is indistinguishable from
    -- fraud, and is exactly what an auditor looks for.
    reason             TEXT        NOT NULL,
    actor              TEXT        NOT NULL,
    issued_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, number)
);

CREATE TABLE credit_note_lines (
    id             UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    credit_note_id UUID    NOT NULL REFERENCES credit_notes (id) ON DELETE CASCADE,
    line_index     INT     NOT NULL,
    kind           TEXT    NOT NULL,
    description    TEXT    NOT NULL,
    amount_minor   BIGINT  NOT NULL CHECK (amount_minor <= 0),   -- credits are negative
    currency       CHAR(3) NOT NULL,
    UNIQUE (credit_note_id, line_index)
);

-- ------------------------------------------------------------- late events --
-- Every event arriving after its period closed, with the policy that handled it — so an
-- invoice that LOOKS short can be explained (invariant 10).
CREATE TABLE late_events (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm                TEXT        NOT NULL DEFAULT 'default',
    scope_kind           TEXT        NOT NULL,
    scope_id             TEXT        NOT NULL,
    ledger_idem_key      TEXT        NOT NULL,
    original_occurred_at TIMESTAMPTZ NOT NULL,
    closed_period_id     UUID        REFERENCES billing_periods (id),
    applied_period_id    UUID        REFERENCES billing_periods (id),
    policy_applied       TEXT        NOT NULL CHECK (policy_applied IN ('next_period','reopen','reject')),
    recorded_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------ immutability, enforced ---
-- Past `draft`, only the status, the provider's invoice id and updated_at may change, and a status
-- never returns to draft. Lines of a non-draft invoice cannot be written at all; credit notes are
-- the only correction, and they are immutable from the moment they exist.
CREATE OR REPLACE FUNCTION invoices_immutable_after_draft() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'draft' THEN
            RAISE EXCEPTION 'invoice % is % and cannot be deleted; void it or issue a credit note', OLD.id, OLD.status
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.status = 'draft' THEN
        RETURN NEW;
    END IF;
    IF NEW.status = 'draft'
       OR (to_jsonb(NEW) - ARRAY['status','provider_invoice_id','updated_at'])
          <> (to_jsonb(OLD) - ARRAY['status','provider_invoice_id','updated_at']) THEN
        RAISE EXCEPTION 'invoice % is % and immutable; corrections are credit notes', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER invoices_immutable BEFORE UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_immutable_after_draft();

CREATE OR REPLACE FUNCTION invoice_lines_draft_only() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE st text;
BEGIN
    SELECT status INTO st FROM invoices
     WHERE id = CASE WHEN TG_OP = 'DELETE' THEN OLD.invoice_id ELSE NEW.invoice_id END;
    IF st IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'lines of a % invoice cannot be written', coalesce(st, 'missing')
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END $$;

CREATE TRIGGER invoice_lines_draft_only BEFORE INSERT OR UPDATE OR DELETE ON invoice_lines
    FOR EACH ROW EXECUTE FUNCTION invoice_lines_draft_only();

CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable (% refused)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE TRIGGER credit_notes_immutable BEFORE UPDATE OR DELETE ON credit_notes
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER credit_note_lines_immutable BEFORE UPDATE OR DELETE ON credit_note_lines
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ------------------------------------------- additive changes to the baseline ---
-- The post-paid spend cap is a new window; its deny code is DISTINCT from balance exhaustion,
-- so a client can tell "over your credit limit" from "out of credits" (FR-102).
ALTER TABLE limits DROP CONSTRAINT IF EXISTS limits_window_kind_check;
ALTER TABLE limits ADD CONSTRAINT limits_window_kind_check
    CHECK (window_kind IN ('balance','daily','hourly','rolling','job','period_to_date'));
ALTER TABLE limits DROP CONSTRAINT IF EXISTS limits_deny_code_check;
ALTER TABLE limits ADD CONSTRAINT limits_deny_code_check
    CHECK (deny_code IN ('payment_required','limit_reached','credit_limit_reached'));

ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS quantity  INT NOT NULL DEFAULT 1 CHECK (quantity > 0);
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS trial_end TIMESTAMPTZ;
ALTER TABLE plans         ADD COLUMN IF NOT EXISTS trial_days INT CHECK (trial_days IS NULL OR trial_days >= 0);

COMMIT;
