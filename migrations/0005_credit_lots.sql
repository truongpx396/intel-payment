-- 0005_credit_lots: optional credit expiry (D2).
--
-- Applied always, USED only when CreditExpiry=lots_fefo. Off by default because expiry is the
-- surprising behaviour: a system that silently destroys something a customer paid for should
-- have been asked for explicitly.
--
-- Lots are maintained by the sole durable writer as it books intents in sequence order, so a lot's
-- `remaining` is exact at the watermark. Expiry is pre-applied on the hot tier at that figure (an
-- upper bound) and trued up by the writer — see hot-path-consistency.md §3.
BEGIN;

CREATE TABLE credit_lots (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    realm           TEXT        NOT NULL DEFAULT 'default',
    scope_kind      TEXT        NOT NULL,
    scope_id        TEXT        NOT NULL,
    pool            TEXT        NOT NULL DEFAULT 'general',
    granted         BIGINT      NOT NULL CHECK (granted > 0),
    remaining       BIGINT      NOT NULL CHECK (remaining >= 0),
    -- What expiry took out of the lot (its remainder when it lapsed). Without it a lot that lapsed
    -- reads as "fully spent", and a refund cannot tell credits the customer used from credits they
    -- already lost: clawing back the second punishes them twice.
    expired_credits BIGINT      NOT NULL DEFAULT 0 CHECK (expired_credits >= 0),
    expires_at      TIMESTAMPTZ,
    source_idem_key TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expired_at      TIMESTAMPTZ,
    -- granted = still held + lapsed + consumed (spent or clawed back); the first two can never exceed it.
    CHECK (remaining + expired_credits <= granted)
);

-- Draw order within a pool: the lot that lapses soonest first, lots that never lapse (expires_at NULL)
-- last, ties in grant order. This is the scan every spend makes, so it is an index over LIVE lots
-- only; spending the lot that will not lapse while one that will sits untouched destroys credits the
-- customer could have used.
CREATE INDEX credit_lots_fefo_idx
    ON credit_lots (realm, scope_kind, scope_id, pool, expires_at, created_at, id)
    WHERE remaining > 0 AND expired_at IS NULL;

-- "Where do this grant's credits stand?" — the refund path. Lapsed lots are not in the live-lot index
-- above, so without this the question scans the scope's whole lot history.
CREATE INDEX credit_lots_source_idx
    ON credit_lots (realm, scope_kind, scope_id, pool, source_idem_key);

CREATE INDEX credit_lots_expiry_idx
    ON credit_lots (expires_at)
    WHERE remaining > 0 AND expired_at IS NULL;

COMMIT;
