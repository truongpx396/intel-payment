# Migrations

Applied by `cmd/payment-migrate` (`make migrate`), numbered `NNNN_name.sql`, applied in order.
`/readyz` fails while the schema is behind head, so a rollout cannot serve money against a
half-migrated database.

## Forward-only, from the first release

**There are no down migrations, and that is deliberate.** Every table here holds an account of
record: `credit_ledger` is the authoritative balance, the idempotency guards are what make
exactly-once true, `payments` is the fiat history, `audit_log` is the record of who did what. A down
migration for any of them is a statement that dropping the ledger is a supported operation. It is not.

A rollback that destroys the ledger is worse than the bad migration it is undoing, because the bad
migration is fixable and the ledger is not reconstructible.

**The path for a bad migration is forward**: write `NNNN+1` that corrects it. Two conventions make
that reliable:

1. **Forward migrations are additive.** New tables, new nullable columns, new indexes, widened
   checks. Never a drop, never a re-key, never a type narrowing on a money table.
2. **A deploy is reversible even when the schema is not.** Because migrations are additive, the
   previous binary still runs against the new schema. Roll back the *code*, leave the schema.

### Before the first release

Until the first release tag, **no database anywhere runs this schema** — the implementation has not
started — so `0001`–`0005` are a *baseline* that is corrected in place, and CI rebuilds it from
scratch on every push. Stacking fix-up migrations onto a schema nothing has run would add history
without protecting anyone ([D38](../specs/001-metering-billing-core/design-decisions.md)). From the
first tag, the rule above applies without exception.

### A feature that is not built ships no schema

The Phase 2 schema lives with its spec, in
[`specs/002-postpaid-invoicing/draft-migrations/`](../specs/002-postpaid-invoicing/draft-migrations/).
CI applies it on top of this baseline so it stays constructible, but `make migrate` never runs it:
under a forward-only policy, a table shipped before its feature exists could never be removed.

## For local development

Throw the database away instead:

```bash
make down-volumes && make up     # fresh volumes, migrations re-applied from 0001
```

## CI

[`scripts/verify-schema.sh`](../scripts/verify-schema.sh) applies every migration to a clean
PostgreSQL 16, seeds a realm, and asserts **behaviour**, not just existence: the idempotency guards'
keys, insert-only rate cards (an edit is refused), database-written audit rows (including for a
direct SQL write), the constraints that encode invariants, and partitions created ahead of need. It
then applies the Phase 2 draft and asserts its immutability triggers.
