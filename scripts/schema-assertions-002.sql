-- Assertions over the Phase 2 DRAFT schema, applied on top of the shipped baseline.
-- Run by scripts/verify-schema.sh. The draft is verified, not shipped (D38).
\set ON_ERROR_STOP 1

CREATE OR REPLACE FUNCTION pg_temp.ok(label text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN RAISE NOTICE 'ok   %', label; END $$;

BEGIN;
INSERT INTO billing_periods (id, realm, scope_kind, scope_id, period_start, period_end, status, model)
VALUES ('00000000-0000-0000-0000-000000000001', 'verify', 'org', 'o1',
        '2026-08-01', '2026-09-01', 'closing', 'postpaid_invoice');
INSERT INTO invoices (id, realm, scope_kind, scope_id, period_id, number, subtotal_minor, total_minor,
                      currency, status)
VALUES ('00000000-0000-0000-0000-0000000000a1', 'verify', 'org', 'o1',
        '00000000-0000-0000-0000-000000000001', 'DRAFT-1', 1000, 1000, 'USD', 'draft');
INSERT INTO invoice_lines (invoice_id, line_index, kind, description, amount_minor, currency)
VALUES ('00000000-0000-0000-0000-0000000000a1', 0, 'usage', 'API requests', 1000, 'USD');
UPDATE invoices SET status = 'finalized', number = 'INV-verify-000001', finalized_at = now()
 WHERE id = '00000000-0000-0000-0000-0000000000a1';

DO $$ BEGIN
  PERFORM pg_temp.ok('a draft invoice is freely editable and finalizes');
  BEGIN
    UPDATE invoices SET total_minor = 1 WHERE id = '00000000-0000-0000-0000-0000000000a1';
    RAISE EXCEPTION 'FAIL: a finalized invoice total was edited';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('a finalized invoice cannot be edited');
  END;
  BEGIN
    UPDATE invoices SET status = 'draft' WHERE id = '00000000-0000-0000-0000-0000000000a1';
    RAISE EXCEPTION 'FAIL: a finalized invoice returned to draft';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('a finalized invoice never returns to draft');
  END;
  BEGIN
    INSERT INTO invoice_lines (invoice_id, line_index, kind, description, amount_minor, currency)
    VALUES ('00000000-0000-0000-0000-0000000000a1', 1, 'usage', 'sneaked in', 1, 'USD');
    RAISE EXCEPTION 'FAIL: a line was added to a finalized invoice';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('lines of a finalized invoice cannot be written');
  END;
  BEGIN
    DELETE FROM invoices WHERE id = '00000000-0000-0000-0000-0000000000a1';
    RAISE EXCEPTION 'FAIL: a finalized invoice was deleted';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('a finalized invoice cannot be deleted');
  END;
  UPDATE invoices SET status = 'paid', provider_invoice_id = 'in_1'
   WHERE id = '00000000-0000-0000-0000-0000000000a1';
  PERFORM pg_temp.ok('status and the provider id still move after finalize');
END $$;

INSERT INTO credit_notes (id, realm, scope_kind, scope_id, number, against_invoice_id, total_minor,
                          currency, reason, actor)
VALUES ('00000000-0000-0000-0000-0000000000c1', 'verify', 'org', 'o1', 'CN-verify-000001',
        '00000000-0000-0000-0000-0000000000a1', -100, 'USD', 'goodwill', 'ops@example');
DO $$ BEGIN
  BEGIN
    UPDATE credit_notes SET total_minor = -1 WHERE id = '00000000-0000-0000-0000-0000000000c1';
    RAISE EXCEPTION 'FAIL: a credit note was edited';
  EXCEPTION WHEN integrity_constraint_violation THEN
    PERFORM pg_temp.ok('credit notes are immutable from the moment they exist');
  END;
END $$;
ROLLBACK;

DO $$ BEGIN RAISE NOTICE 'phase 2 draft assertions passed'; END $$;
