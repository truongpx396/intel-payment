// Package postgres is the durable-side adapter: the books (ledger, balances, watermarks, guards,
// suspense, transfers, lots), the spend journal, and the per-realm configuration data (limits,
// pools, rate cards) with a cache that a Postgres LISTEN keeps current.
//
// It implements the driven ports; it never imports the app. The single durable writer's rules — the
// watermark cases, the op table — live in the app, and this package supplies the transactions and
// queries they run on. Every statement here is parameterised; no value is ever formatted into SQL.
package postgres
