-- 0005_credit_lots: optional credit expiry (D2).
--
-- Applied always, USED only when CreditExpiry=lots_fifo. Off by default because expiry is the
-- surprising behaviour: a system that silently destroys something a customer paid for should
-- have been asked for explicitly.
BEGIN;

CREATE TABLE credit_lots (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm           TEXT        NOT NULL DEFAULT 'default',
    scope_kind      TEXT        NOT NULL,
    scope_id        TEXT        NOT NULL,
    granted         BIGINT      NOT NULL CHECK (granted > 0),
    remaining       BIGINT      NOT NULL CHECK (remaining >= 0),
    expires_at      TIMESTAMPTZ,
    source_idem_key TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expired_at      TIMESTAMPTZ,
    CHECK (remaining <= granted)
);

-- FIFO consumption: the oldest unexpired lot with a remainder is drawn first.
CREATE INDEX credit_lots_fifo_idx
    ON credit_lots (realm, scope_kind, scope_id, created_at)
    WHERE remaining > 0 AND expired_at IS NULL;

CREATE INDEX credit_lots_expiry_idx
    ON credit_lots (expires_at)
    WHERE remaining > 0 AND expired_at IS NULL;

COMMIT;
