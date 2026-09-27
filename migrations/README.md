# Migrations

Applied by `cmd/payment-migrate` (`make migrate`), numbered `NNNN_name.sql`, applied in order.
`/readyz` fails while the schema is behind head, so a rollout cannot serve money against a
half-migrated database.

## Forward-only, by policy

**There are no down migrations, and that is deliberate.** Every table here holds an account of
record: `credit_ledger` is the authoritative balance, `credit_idem` is the guard that makes
exactly-once true, `payments` is the fiat history, `audit_log` is the record of who did what. A down
migration for any of them is a statement that dropping the ledger is a supported operation. It is not.

A rollback that destroys the ledger is worse than the bad migration it is undoing, because the bad
migration is fixable and the ledger is not reconstructible — a backup restores a *copy*, not the
rows a live system wrote since.

**The path for a bad migration is forward**: write `NNNN+1` that corrects it. Two conventions make
that reliable:

1. **Forward migrations are additive.** New tables, new nullable columns, new indexes. Never a drop,
   never a re-key, never a type narrowing on a money table.
   [002's data model](../specs/002-postpaid-invoicing/data-model.md) follows this — it adds tables and
   nullable columns to 001 and drops nothing, so a 001 deployment migrates forward without touching a
   money table.
2. **A deploy is reversible even when the schema is not.** Because migrations are additive, the
   previous binary still runs against the new schema. Roll back the *code*, leave the schema, and fix
   forward.

## For local development

Throw the database away instead:

```bash
make down-volumes && make up     # fresh volumes, migrations re-applied from 0001
```

That is what a down migration would have been used for, and it is both faster and honest about the
fact that it destroys everything.

## CI

CI applies every migration from `0001` against a clean PostgreSQL 16 on each push, then asserts the
two constraints the system's guarantees rest on still exist: the realm-scoped idempotency guard on
`credit_idem`, and the webhook replay guard on `payment_events`. A migration that silently drops
either fails the build.
