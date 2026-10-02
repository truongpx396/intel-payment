---
paths:
  - "migrations/**"
  - "specs/*/draft-migrations/**"
  - "scripts/*.sql"
---

# Schema and migrations

Full policy: [migrations/README.md](../../migrations/README.md). Every table here is an account of
record (`credit_ledger`, the idempotency guards, `payments`, `audit_log`).

- **Forward-only. There are no down migrations**, deliberately. The path for a bad migration is
  `NNNN+1` that corrects it. Forward migrations are additive: new tables, nullable columns, indexes,
  widened checks — never a drop, a re-key, or a type narrowing on a money table.
- **Until the first release tag, `0001`–`0005` are a baseline corrected in place** (CI rebuilds it from
  scratch on every push). No release tag exists yet; once one does, the additive rule applies without
  exception. Check `git tag` before editing a shipped file in place.
- **A feature that is not built ships no schema.** The Phase 2 schema lives in
  `specs/002-postpaid-invoicing/draft-migrations/`; `make migrate` never runs it. Do not move a table
  into `migrations/` before its code exists — it could never be removed.
- **Verify by applying, not by reading** (constitution XI): `make verify-schema` on an *empty* PostgreSQL
  applies every migration, seeds a realm, and asserts behaviour — idempotency-guard keys, insert-only
  rate cards, database-written audit rows, constraints, partitions created ahead of need. An invariant a
  migration adds gets an assertion in `scripts/schema-assertions.sql` (or `-002.sql`), written first.
- A migration is production code for the red-green gate: its failing assertion precedes it.
- Local data is disposable: `make down-volumes && make up` rebuilds from `0001`.
