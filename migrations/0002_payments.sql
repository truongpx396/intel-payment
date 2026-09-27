-- 0002_payments: the fiat boundary.
BEGIN;

CREATE TABLE plans (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm            TEXT        NOT NULL DEFAULT 'default',
    code             TEXT        NOT NULL,   -- the host's slug; the ONLY plan id a UI ever sees
    name             TEXT        NOT NULL,
    description      TEXT,
    kind             TEXT        NOT NULL CHECK (kind IN ('one_time','subscription')),
    -- The ONLY coupling between fiat and credits. NULL = custom/negotiated, which the
    -- catalogue renders as a contact path rather than a disabled buy button.
    credit_allotment BIGINT,
    billing_interval TEXT        CHECK (billing_interval IN ('month','year')),
    active           BOOLEAN     NOT NULL DEFAULT true,
    sort_order       INT         NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, code),
    CHECK (kind <> 'subscription' OR billing_interval IS NOT NULL)
);

-- Per-currency pricing (D12). One price per plan would force a host selling in three
-- currencies to maintain three plans, which then drift.
CREATE TABLE plan_prices (
    plan_id     UUID   NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    currency    CHAR(3) NOT NULL,
    minor_units BIGINT NOT NULL CHECK (minor_units >= 0),
    PRIMARY KEY (plan_id, currency)
);

CREATE TABLE plan_provider_prices (
    id                UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id           UUID    NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    provider          TEXT    NOT NULL CHECK (provider IN ('stripe','polar','paypal')),
    currency          CHAR(3) NOT NULL,
    provider_price_id TEXT    NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_price_id),
    UNIQUE (plan_id, provider, currency)
);

-- The ONLY sanctioned path from a verified webhook to a Scope (payments invariant 4).
CREATE TABLE billing_customers (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm                TEXT        NOT NULL DEFAULT 'default',
    scope_kind           TEXT        NOT NULL,
    scope_id             TEXT        NOT NULL,
    provider             TEXT        NOT NULL CHECK (provider IN ('stripe','polar','paypal')),
    provider_customer_id TEXT        NOT NULL,
    billing_email        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm, scope_kind, scope_id, provider),
    UNIQUE (provider, provider_customer_id)
);

CREATE TABLE subscriptions (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm                    TEXT        NOT NULL DEFAULT 'default',
    scope_kind               TEXT        NOT NULL,
    scope_id                 TEXT        NOT NULL,
    plan_id                  UUID        NOT NULL REFERENCES plans (id),
    provider                 TEXT        NOT NULL CHECK (provider IN ('stripe','polar','paypal')),
    provider_subscription_id TEXT        NOT NULL,
    -- Status has EXACTLY ONE WRITER: the webhook path (payments invariant 12).
    status                   TEXT        NOT NULL CHECK (status IN
                               ('trialing','active','past_due','paused','canceled','incomplete')),
    current_period_start     TIMESTAMPTZ,
    current_period_end       TIMESTAMPTZ,
    cancel_at_period_end     BOOLEAN     NOT NULL DEFAULT false,
    -- Dunning window (D10): a transient card decline must not revoke access instantly.
    grace_until              TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    canceled_at              TIMESTAMPTZ,
    UNIQUE (provider, provider_subscription_id)
);

CREATE UNIQUE INDEX subscriptions_one_live_per_scope
    ON subscriptions (realm, scope_kind, scope_id)
    WHERE status IN ('trialing','active','past_due','paused');

CREATE TABLE payments (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm               TEXT        NOT NULL DEFAULT 'default',
    scope_kind          TEXT        NOT NULL,
    scope_id            TEXT        NOT NULL,
    provider            TEXT        NOT NULL CHECK (provider IN ('stripe','polar','paypal')),
    provider_payment_id TEXT        NOT NULL,
    plan_id             UUID        REFERENCES plans (id),
    kind                TEXT        NOT NULL CHECK (kind IN ('one_time','subscription_invoice')),
    amount_minor        BIGINT      NOT NULL,
    currency            CHAR(3)     NOT NULL,
    tax_minor           BIGINT,                 -- as REPORTED by the provider; never computed here
    credits_granted     BIGINT      NOT NULL DEFAULT 0,
    status              TEXT        NOT NULL CHECK (status IN
                          ('pending','succeeded','failed','refunded','partially_refunded','disputed')),
    receipt_url         TEXT,
    failure_reason      TEXT,
    -- The RECONCILIATION KEY between fiat and credits: equals the grant row's idem_key.
    idem_key            TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_payment_id)
);
CREATE INDEX payments_scope_time_idx ON payments (realm, scope_kind, scope_id, created_at DESC);

-- Verified webhook events. The UNIQUE below IS the replay guard (payments invariant 5).
CREATE TABLE payment_events (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm             TEXT        NOT NULL DEFAULT 'default',
    provider          TEXT        NOT NULL,
    provider_event_id TEXT        NOT NULL,
    event_type        TEXT        NOT NULL,
    -- SHA-256 of the verified raw body. The body itself is NOT stored: it carries PII and is
    -- not needed once the facts are extracted.
    payload_hash      TEXT        NOT NULL,
    status            TEXT        NOT NULL CHECK (status IN
                        ('received','processed','ignored','failed','unverifiable')),
    scope_kind        TEXT,                     -- resolved after parse
    scope_id          TEXT,
    error             TEXT,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    UNIQUE (realm, provider, provider_event_id)
);
CREATE INDEX payment_events_received_idx ON payment_events (received_at);

COMMIT;
