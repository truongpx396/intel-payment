-- 0002_payments: the fiat boundary.
BEGIN;

-- A provider ACCOUNT, not a provider: one deployment may sell through several Stripe accounts (one
-- per product, or test and live), and one account may serve several realms. Webhooks arrive at
-- /webhooks/{provider}/{account}, so the account — and its secrets — is known before anything is
-- parsed. Secrets are never stored: *_ref names an environment variable or a secret-manager path.
-- Adding a provider is an adapter plus a row here; no migration (D36).
CREATE TABLE provider_accounts (
    id                 TEXT        PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    provider           TEXT        NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]*$'),
    realms             TEXT[]      NOT NULL CHECK (cardinality(realms) >= 1),
    mode               TEXT        NOT NULL DEFAULT 'live' CHECK (mode IN ('live','test')),
    secret_ref         TEXT        NOT NULL,
    webhook_secret_ref TEXT        NOT NULL,
    active             BOOLEAN     NOT NULL DEFAULT true,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE plans (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm            TEXT        NOT NULL DEFAULT 'default',
    code             TEXT        NOT NULL,   -- the host's slug; the ONLY plan id a UI ever sees
    name             TEXT        NOT NULL,
    description      TEXT,
    kind             TEXT        NOT NULL CHECK (kind IN ('one_time','subscription')),
    -- The ONLY coupling between fiat and credits. NULL = custom/negotiated.
    credit_allotment BIGINT      CHECK (credit_allotment IS NULL OR credit_allotment >= 0),
    -- The credit pool the allotment is granted into (e.g. `promo` for a trial pack).
    pool             TEXT        NOT NULL DEFAULT 'general',
    billing_interval TEXT        CHECK (billing_interval IN ('month','year')),
    active           BOOLEAN     NOT NULL DEFAULT true,
    sort_order       INT         NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, code),
    CHECK (kind <> 'subscription' OR billing_interval IS NOT NULL)
);

-- Per-currency pricing (D12).
CREATE TABLE plan_prices (
    plan_id     UUID    NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    currency    CHAR(3) NOT NULL,
    minor_units BIGINT  NOT NULL CHECK (minor_units >= 0),
    PRIMARY KEY (plan_id, currency)
);

CREATE TABLE plan_provider_prices (
    id                UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id           UUID    NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    provider_account  TEXT    NOT NULL REFERENCES provider_accounts (id),
    currency          CHAR(3) NOT NULL,
    provider_price_id TEXT    NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_account, provider_price_id),
    UNIQUE (plan_id, provider_account, currency)
);

-- The ONLY sanctioned path from a verified webhook to a Scope — and therefore to a realm
-- (payments invariant 4).
CREATE TABLE billing_customers (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm                TEXT        NOT NULL DEFAULT 'default',
    scope_kind           TEXT        NOT NULL,
    scope_id             TEXT        NOT NULL,
    provider_account     TEXT        NOT NULL REFERENCES provider_accounts (id),
    provider_customer_id TEXT        NOT NULL,
    billing_email        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, scope_kind, scope_id, provider_account),
    UNIQUE (provider_account, provider_customer_id)
);

CREATE TABLE subscriptions (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm                    TEXT        NOT NULL DEFAULT 'default',
    scope_kind               TEXT        NOT NULL,
    scope_id                 TEXT        NOT NULL,
    plan_id                  UUID        NOT NULL REFERENCES plans (id),
    provider_account         TEXT        NOT NULL REFERENCES provider_accounts (id),
    provider_subscription_id TEXT        NOT NULL,
    -- Status has EXACTLY ONE WRITER: the webhook processor (payments invariant 12).
    status                   TEXT        NOT NULL CHECK (status IN
                               ('trialing','active','past_due','paused','canceled','incomplete')),
    current_period_start     TIMESTAMPTZ,
    current_period_end       TIMESTAMPTZ,
    cancel_at_period_end     BOOLEAN     NOT NULL DEFAULT false,
    grace_until              TIMESTAMPTZ,   -- dunning window (D10)
    -- The provider timestamp of the newest state applied. Providers do not order deliveries, so an
    -- older event can never overwrite a newer state (payments invariant 15).
    last_event_at            TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    canceled_at              TIMESTAMPTZ,
    UNIQUE (provider_account, provider_subscription_id)
);

CREATE UNIQUE INDEX subscriptions_one_live_per_scope
    ON subscriptions (realm, scope_kind, scope_id)
    WHERE status IN ('trialing','active','past_due','paused');

CREATE TABLE payments (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm               TEXT        NOT NULL DEFAULT 'default',
    scope_kind          TEXT        NOT NULL,
    scope_id            TEXT        NOT NULL,
    provider_account    TEXT        NOT NULL REFERENCES provider_accounts (id),
    provider_payment_id TEXT        NOT NULL,
    plan_id             UUID        REFERENCES plans (id),
    kind                TEXT        NOT NULL CHECK (kind IN ('one_time','subscription_invoice','auto_recharge')),
    amount_minor        BIGINT      NOT NULL CHECK (amount_minor >= 0),
    currency            CHAR(3)     NOT NULL,
    tax_minor           BIGINT,                 -- as REPORTED by the provider; never computed here
    credits_granted     BIGINT      NOT NULL DEFAULT 0 CHECK (credits_granted >= 0),
    pool                TEXT        NOT NULL DEFAULT 'general',
    -- Cumulative, so several partial refunds revoke exactly
    -- ceil(credits_granted × refunded_minor / amount_minor) in total — never more than granted,
    -- with no rounding drift across refunds (payments invariant 16).
    refunded_minor      BIGINT      NOT NULL DEFAULT 0,
    credits_revoked     BIGINT      NOT NULL DEFAULT 0,
    status              TEXT        NOT NULL CHECK (status IN
                          ('pending','succeeded','failed','refunded','partially_refunded',
                           'disputed','charged_back')),
    dispute_id          TEXT,
    dispute_status      TEXT        CHECK (dispute_status IN ('open','won','lost')),
    receipt_url         TEXT,
    failure_reason      TEXT,
    -- The RECONCILIATION KEY between fiat and credits: the grant's credit_idem key.
    idem_key            TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_account, provider_payment_id),
    CHECK (refunded_minor BETWEEN 0 AND amount_minor),
    CHECK (credits_revoked BETWEEN 0 AND credits_granted)
);
CREATE INDEX payments_scope_time_idx ON payments (realm, scope_kind, scope_id, created_at DESC);

-- The webhook INBOX (D34). A verified delivery is persisted here and acknowledged 200 in one step;
-- processing happens afterwards, from this table, with retries and a lease. A crash between
-- acknowledging and granting therefore loses nothing — the provider's retry is no longer the
-- reliability mechanism, this table is. The UNIQUE constraint is the replay guard.
CREATE TABLE payment_events (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_account  TEXT        NOT NULL REFERENCES provider_accounts (id),
    provider          TEXT        NOT NULL,
    provider_event_id TEXT        NOT NULL,
    event_type        TEXT        NOT NULL,
    event_created_at  TIMESTAMPTZ,              -- the provider's timestamp; orders subscription state
    payload_hash      TEXT        NOT NULL,     -- SHA-256 of the raw body
    -- The normalized, VERIFIED facts (EventObject): provider ids, amounts, status, periods. Not the
    -- raw body — that carries PII and is not needed once the facts are extracted.
    object            JSONB,
    -- Only while status = 'unverifiable': the raw delivery, kept so verification can be retried
    -- once the provider's verification API recovers, and deleted the moment it succeeds.
    parked_body       BYTEA,
    parked_headers    JSONB,
    realm             TEXT,                     -- resolved from the verified customer mapping
    scope_kind        TEXT,
    scope_id          TEXT,
    status            TEXT        NOT NULL CHECK (status IN
                        ('received','processing','processed','ignored','failed','dead','unverifiable')),
    attempts          INT         NOT NULL DEFAULT 0,
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until       TIMESTAMPTZ,
    error             TEXT,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    UNIQUE (provider_account, provider_event_id),
    CHECK (status = 'unverifiable' OR (parked_body IS NULL AND parked_headers IS NULL))
);
CREATE INDEX payment_events_work_idx ON payment_events (next_attempt_at)
    WHERE status IN ('received','failed','processing','unverifiable');
CREATE INDEX payment_events_received_idx ON payment_events (received_at);

-- Auto top-up: when a scope's balance crosses `threshold`, charge a saved payment method off-session
-- for `plan_code` (a one_time pack). Credits still arrive ONLY on the verified payment webhook.
-- The payment method is the provider's reference — never card data.
CREATE TABLE auto_recharge (
    realm                TEXT        NOT NULL DEFAULT 'default',
    scope_kind           TEXT        NOT NULL,
    scope_id             TEXT        NOT NULL,
    enabled              BOOLEAN     NOT NULL DEFAULT true,
    pool                 TEXT        NOT NULL DEFAULT 'general',
    threshold            BIGINT      NOT NULL,
    plan_code            TEXT        NOT NULL,
    currency             CHAR(3)     NOT NULL,
    provider_account     TEXT        NOT NULL REFERENCES provider_accounts (id),
    payment_method_ref   TEXT        NOT NULL,
    max_per_day          INT         NOT NULL DEFAULT 3 CHECK (max_per_day BETWEEN 1 AND 50),
    last_triggered_at    TIMESTAMPTZ,
    consecutive_failures INT         NOT NULL DEFAULT 0,
    disabled_reason      TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, scope_kind, scope_id)
);

COMMIT;
