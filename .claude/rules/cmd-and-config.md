---
paths:
  - "cmd/**"
  - "deploy/**"
  - ".env.example"
  - "docs/configuration.md"
  - "docs/security.md"
---

# Binaries, configuration and secrets

- **`os.Getenv` lives in `cmd/` only.** Pass the lookup in as a function (`run(ctx, args, os.Getenv,
  stdout, stderr)` in `cmd/payment-migrate`) so `main` is testable without touching the process
  environment. Everything past `cmd/` receives a typed `metering.Config`.
- **A new knob is data or config, never a constant** (constitution VIII). Anything a host might change
  without a deploy — prices, ceilings, plans, entitlements — is a Postgres row behind the admin API;
  infrastructure and policy are `PAYMENT_*` environment variables. Add all of: the variable in
  `.env.example`, its row in `docs/configuration.md`, validation in `Config.Validate()` that rejects an
  unknown value, and a test for the rejection.
- **Policies fail closed.** An unrecognised policy value is a startup error, not a default: a new policy
  type gets a `Validate()` built on `oneOf` in `metering/domain/policy.go` and is listed in
  `Config.Validate()`.
- **Secrets are server-side only.** Provider keys and webhook secrets come from the environment or a
  secret manager, named by `provider_accounts.secret_ref`; never a log line, a database value, a client
  bundle, or a `PUBLIC_`/`NEXT_PUBLIC_` variable. `.env.example` holds names, never values.
  Webhook signatures are verified on the raw body, in constant time, inside a timestamp tolerance, before
  any parse or side effect (constitution XIII).
- **The image is distroless and non-root; Redis is `noeviction` + AOF** with an ACL that gives hosts
  nothing. Prove a change with `make verify-deploy` (Docker) and paste the output.
- `.env` and other real env files are denied to Claude by `.claude/settings.json`; `.env.example` is the
  reference and is editable.
