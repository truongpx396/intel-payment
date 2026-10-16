# Configuration

Two layers, and the split is deliberate:

- **Deployment config** (env → `Config` structs) — infrastructure and policy. Changing it needs a restart.
- **Realm config** (Postgres rows, written through the admin API) — rate cards, ceilings, pools, plans, entitlements, provider accounts, event endpoints. Changing it needs **nothing**: the database audits the change and signals every cache in the same transaction.

Anything a host might want to change without waiting on this service's release cycle is in the
second layer. That is what "highly configurable" means here: not more env vars, but the
commercially volatile things living in tables.

---

## Deployment configuration

Every variable maps 1:1 to a `Config` field. The last column is what happens if you get it wrong.

### Identity

| Variable | Default | Failure mode if wrong |
|---|---|---|
| `PAYMENT_REALM` | `default` | The realm a library-mode host acts in. Two products sharing a realm share plans, limits **and an idempotency key space** |

### Stores

| Variable | Default | Failure mode if wrong |
|---|---|---|
| `PAYMENT_BALANCE_REDIS_URL` | — (required) | a single primary (`redis://user:pass@host:6379/0`) or a cluster (`redis+cluster://user:pass@host:6379?addr=host2:6379&addr=host3:6379`); the key layout is cluster-safe either way. The service role's credential — it cannot `FUNCTION LOAD` |
| `PAYMENT_BALANCE_REDIS_DEPLOY_URL` | — | the **deploy role's** credential, used only by `payment-migrate` to install the hot-path functions (`FUNCTION LOAD REPLACE`). Never given to `paymentd` or `payment-worker` |
| `PAYMENT_LEDGER_DSN` | — (required) | — |

> **The balance Redis MUST be `noeviction` with AOF on, MUST NOT be shared with a cache, and MUST
> NOT be reachable by hosts.** An LRU policy silently discards balances and outbox intents; host
> access to it is the ability to mint credits. Hosts receive events through
> [outbound webhooks](../specs/001-metering-billing-core/contracts/outbound-events.md).

### Sharding

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_SHARDS` | `256` | The Redis placement unit: every key of a scope carries its shard's hash tag `{s<n>}`. **Recorded in `hot_config` on first start**; a process configured differently refuses to start. Changing it is a planned reshard (freeze → drain → bump → unfreeze), see [operations.md](./operations.md#resharding) |

### Bus

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_BUS` | `redis_streams` | `redis_streams` \| `nats_jetstream`. Money intents live in the Redis outbox either way |
| `PAYMENT_BUS_URL` | — | Required for JetStream |
| `PAYMENT_SUBJECT_PREFIX` | `billing` | Fit into a host's existing namespace |
| `PAYMENT_BUS_RETENTION` | `168h` | Acked entries kept this long, then trimmed. Streams only |
| `PAYMENT_BUS_MAX_LEN` | `1000000` | Per-stream cap. At the cap the hot function refuses with backpressure and `Record` defers to the journal — nothing is trimmed or dropped |
| `PAYMENT_BUS_MAX_ATTEMPTS` | `5` | Deliveries before an event goes to the DLQ (intents are parked into suspense instead) |
| `PAYMENT_BUS_ACK_WAIT` | `30s` | In-flight reclaim threshold (`XAUTOCLAIM` / ack-wait) |

### Settlement durability

| Variable | Values | Default | Decides |
|---|---|---|---|
| `PAYMENT_SETTLEMENT` | `outbox` · `journal` | `outbox` | Whether an intent's first durable home is Redis (sub-ms) or Postgres (+1 synchronous insert) |
| `PAYMENT_HOT_ACK_WAIT` | `none` · `aof_local` · `aof_replica` | `none` | Outbox only: wait for `WAITAOF` before acknowledging. **Latency depends on `appendfsync`**: under `always` it is one group-committed fsync; under `everysec` a call waits for the next fsync tick — **up to ~1 s, measured** — so `PAYMENT_RECORD_TIMEOUT` must exceed it or every acknowledgement is refused (and the charge journaled). Prefer `appendfsync always` with `aof_local`/`aof_replica`, or `Settlement=journal` |
| `PAYMENT_LEDGER_GRANULARITY` | `event` · `rollup` | `event` | One ledger row per event, or per batch with per-event detail archived — see the [scale envelope](../specs/001-metering-billing-core/plan.md#scale-envelope) |
| `PAYMENT_DRAIN_BATCH` | | `500` | Intents booked per writer transaction |

**What `outbox` can lose, stated exactly.** A Redis crash or asynchronous failover can lose the most
recent *acknowledged* intents **together with their balance change** — Redis persists a function's
effects as one unit — so no reconcile can see it. That is silent **under-billing**, never
double-billing, bounded by the AOF fsync interval (≤ 1 s with `everysec`) or the replication lag.
`aof_local` removes the crash case, `aof_replica` the failover case, `journal` all of it. The opposite
case — the books ahead of a rolled-back Redis — loses nothing: it is detected and the shard is rebuilt.

### Idempotency

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_HOT_IDEM_TTL` | `24h` | Hot guard lifetime. MUST NOT exceed the window below. A replay after it is still caught, by the writer, and reversed through suspense (`metering_late_replay_total`) |
| `PAYMENT_USAGE_IDEM_WINDOW` | `72h` | The durable usage dedup window. **MUST exceed every producer's retry horizon** — usage older than it is refused as `stale_event`, never charged twice |
| `PAYMENT_IDEM_SAFETY_MARGIN` | `1h` | `Record` refuses `occurred_at` older than window − margin |
| `PAYMENT_MAX_CLOCK_SKEW` | `5m` | `Record` refuses `occurred_at` this far in the future |

Grants, refunds, transfers and adjustments are guarded **permanently** — they are rare, and they mint
or move money.

### Reconciliation & recovery

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_RECONCILE_INTERVAL` | `5m` | Incremental: scopes booked since the last run. Cost follows activity, not history |
| `PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL` | `24h` | Every scope |
| `PAYMENT_RECONCILE_TOLERANCE` | `0` | `\|drift\|` at **equal sequence numbers** above this pages. In-flight intents are never drift, so `0` is the right value, not a noisy one |
| `PAYMENT_GAP_TIMEOUT` | `2m` | A sequence gap older than this pages |
| `PAYMENT_TRANSFER_REDRIVE` | `30s` | A transfer in transit longer than this is re-driven (`payment-worker`) |

### Worker (`payment-worker`)

The worker reads every engine variable above, and these. It does not read the transport or auth variables.

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_DRAIN_INTERVAL` | `250ms` | How long a shard with nothing to book waits before it looks again. A shard with work does not wait. Must be positive. Idle load is roughly `PAYMENT_SHARDS` shards × a few round trips per interval |
| `PAYMENT_OWNERSHIP_RETRY` | `2s` | How often shards it does not own are tried, and the locks it holds are checked. A stopped or dead worker's shards are taken over within about this long. Must be positive |
| `PAYMENT_JOURNAL_REPLAY_INTERVAL` | `5s` | How often usage journalled during a hot-tier outage is replayed. Must be positive |
| `PAYMENT_SHUTDOWN_TIMEOUT` | `30s` | How long a stop waits for the batch in hand. Must be positive |
| `PAYMENT_WORKER_ADDR` | `:9091` | `/healthz` and `/metrics` for the worker process — the outbox, suspense and transfer gauges are exported here, not by `paymentd` |

`PAYMENT_BUS=nats_jetstream` is refused by the worker: it drains the Redis outbox. **A worker holds one PostgreSQL
connection for every shard it owns** (a session advisory lock lives on its connection), so `PAYMENT_SHARDS` bounds
its connection use — the pool is sized `PAYMENT_SHARDS` + 16 — and the server's `max_connections` must allow it (the
compose stack sets 400 for the default 256). Replicas divide the shards, so each needs only its share.
| `PAYMENT_TRANSFER_TRANSIT_ALARM` | `5m` | …and longer than this pages |

### Hot-path safety

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_ADMIT_TIMEOUT` | `250ms` | Generous. Co-located, set ~50 ms |
| `PAYMENT_RECORD_TIMEOUT` | `500ms` | Settlement is off the user's critical path |
| `PAYMENT_ADMIT_FAIL_POLICY` | `fail_closed` | `fail_open` serves work while the hot tier is unavailable; its usage is journaled and metered on recovery, and every occurrence is counted |
| `PAYMENT_REQUIRE_MAX_COST` | `false` | `true` rejects an `Admit` with no cost bound. Turn it on once `metering_admit_uncapped_total` is flat at zero |
| `PAYMENT_MAX_OPERATION_AMOUNT` | `1125899906842624` (2^50) | The largest single operation; keeps hot-tier arithmetic exact |

### Money policies

| Variable | Values | Default | What it decides |
|---|---|---|---|
| `PAYMENT_NEGATIVE_BALANCE_POLICY` | `allow_debt` · `clamp_to_zero` · `block_and_flag` | `clamp_to_zero` | What a refund past consumed credits does, per pool. A floor always books a `writeoff` row and never forgives debt that already existed |
| `PAYMENT_CREDIT_EXPIRY` | `off` · `lots_fefo` | `off` | Whether grants expire. With `lots_fefo` each grant is a lot and the one that lapses soonest is spent first (lots that never lapse last). `lots_fifo`, the name this mode had before it drew by expiry, is refused at start-up |
| `PAYMENT_PLAN_CHANGE_POLICY` | `at_period_end` · `immediate_prorate` · `immediate_no_prorate` | `at_period_end` | Mid-period upgrade/downgrade. Proration is the provider's math |
| `PAYMENT_TAX_STRATEGY` | `provider_managed` · `external_hook` · `none` | `provider_managed` | **Who computes tax.** There is no compute-it-here mode |

### Payments

Read by `paymentd` (the webhook door) and `payment-worker` (everything after it); both check the whole of it at start and name every variable that is wrong.

| Variable | Default | Failure mode if wrong |
|---|---|---|
| `PAYMENT_WEBHOOK_TOLERANCE` | `5m` | How far a signature's timestamp may be from **when the delivery arrived**. Positive, at most `1h`: nothing rejects every delivery, and a wider window makes a captured delivery replayable for that long. A parked delivery is judged as of its arrival, so an outage longer than this does not turn it into a replay |
| `PAYMENT_WEBHOOK_TICK_INTERVAL` | `5s` | `payment-worker`: how often the webhook inbox is worked and parked deliveries are verified again. A customer who has paid waits at most this long for their credits. Positive, at most `1m` |

### Dunning

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_DUNNING_GRACE_DAYS` | `7` | `payment-worker`: entitlements hold this long after a failed payment. A whole number of days, 1–365; `0` would silently become the default, so it is refused |
| `PAYMENT_DUNNING_SUSPEND_AFTER_DAYS` | `14` | Suspension after grace. **Not read yet** — entitlements are Phase 4 |

### Retention

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_PAYMENT_EVENT_RETENTION` | `90d` | Processed webhook inbox rows, `payment-worker` deletes the older ones hourly. Days (`90d`) or a Go duration (`2160h`); **at least `7d`** — it MUST exceed the longest provider replay window, and past it the permanent grant guard still prevents a double grant. A row not yet processed, parked or dead is never deleted |
| `PAYMENT_EVENT_RETENTION` | `30d` | Outbound events in the feed |
| `PAYMENT_LEDGER_ONLINE_RETENTION` | `unlimited` | After this, a ledger partition fully covered by checkpoints is detached and archived — never dropped while needed to prove a balance |
| `PAYMENT_USAGE_ARCHIVE_RETENTION` | `400d` | Per-event detail under `rollup` granularity |

### Transports & auth

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_GRPC_ADDR` | `:9090` | Hot path |
| `PAYMENT_HTTP_ADDR` | `:8080` | REST + webhooks + `/healthz` + `/readyz` + `/metrics` |
| `PAYMENT_SERVICE_AUTH` | `bearer` | `none` \| `mtls` \| `bearer`. **`none` is permitted only on loopback or a Unix socket** |
| `PAYMENT_SERVICE_TOKENS` | — | bearer mode: `<caller-id>:<sha256-hex>:<realms>:<privileges>,…` — realms and privileges (`record`, `grant`, `admin`) each joined with `+`. The **digest** of the token, never the token: a plaintext token, a short or non-hex digest, a caller or a token listed twice, a wildcard realm or an unknown privilege stops the service at start-up. Empty accepts no caller |
| `PAYMENT_SERVICE_CERTS` | — | mTLS mode: `<caller-id>:<certificate common name>:<realms>:<privileges>,…` — same rules. The listener verifies the chain against the private CA; this maps the verified leaf's subject to a caller |
| `PAYMENT_TLS_CERT_FILE` / `PAYMENT_TLS_KEY_FILE` | — | PEM files. With both, REST and gRPC serve TLS; one without the other stops the service at start-up. Required by `mtls` |
| `PAYMENT_TLS_CLIENT_CA_FILE` | — | `mtls` only: the private CA a caller's certificate must chain to. A certificate is requested of every caller and verified when offered, so a probe with none can still reach `/healthz` and `/readyz`; `/v1/*` then refuses the call as `401` |
| `PAYMENT_LEGACY_429_FOR_LIMITS` | `false` | Restores `429` instead of `409` for spent windows, for a host mid-migration |
| `PAYMENT_SHUTDOWN_TIMEOUT` | `30s` | How long a stop waits for the calls in flight. On `SIGTERM` both listeners stop taking new calls at once, then what was taken is answered (a `Record` settles or is journalled); past this bound the rest is cut, the exit code is 1, and the log says calls were in flight — their callers retry with the same `Idempotency-Key`, which is booked once. Must be positive. Set it above the longest deploy-time call (`PAYMENT_RECORD_TIMEOUT` is the usual bound) and below the orchestrator's kill grace period |
| `PAYMENT_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. Logs are JSON on stderr; `listening` reports the address each transport bound |

An address is `host:port`, or `unix:/path` for a unix socket. `paymentd` reads all of this at start, checks the
whole of it before it dials anything, and stops with exit code 2 naming **every** variable that is wrong (never
echoing a credential). It then refuses to start against a schema that is not at head (`payment-migrate up` first)
or a `PAYMENT_SHARDS` that disagrees with the books. `paymentd healthcheck` exits 0 only while `/readyz` answers
200 — the container healthcheck, since the image has no `curl`.

### Provider secrets

Provider credentials are **realm configuration** (`provider_accounts`), and a row holds only
*references* — never the secret. Two schemes are resolved, at the moment of use and never cached, so a rotated secret takes
effect on the next delivery: `env:NAME` (the variable of that name, in the process that needs it — `paymentd` verifies,
`payment-worker` calls the provider) and `file:/absolute/path` (a mounted secret; it must be a regular file under 64 KiB, and
one trailing line ending is dropped). A reference that cannot be resolved — an unset variable, an unreadable file, any other
scheme — fails closed: the delivery is answered `500` and the provider retries. The referenced
variables live wherever your platform keeps secrets:

| Referenced variable (example) | Notes |
|---|---|
| `STRIPE_SECRET_KEY` / `STRIPE_WEBHOOK_SECRET` | Local HMAC verification |
| `POLAR_ACCESS_TOKEN` / `POLAR_WEBHOOK_SECRET` | Local HMAC verification |
| `PAYPAL_CREDENTIALS` / `PAYPAL_WEBHOOK_ID` | `PAYPAL_CREDENTIALS` is `<client id>:<client secret>` in one value (an account row references one value); `PAYPAL_WEBHOOK_ID` is the webhook's id. **Remote** verification — an API call per webhook; an outage parks deliveries |

---

## Realm configuration (data, no deploy)

Written through the admin API ([rest-api.md § Configuration](../specs/001-metering-billing-core/contracts/rest-api.md#configuration-admin-privilege)).
A direct SQL write also works and is still audited — attributed to the database role — but skips
validation.

| Table | What you change | How often |
|---|---|---|
| `rate_cards` / `rate_card_entries` | rational prices (`credits_per_block` per `block_size` units) and the card's pricer | on repricing — **publish a new version**; the database refuses edits |
| `limits` | the ceilings, **including how many there are**, their subject, window, time zone, and whether the plan sizes them (`max_entitlement`) | on policy change |
| `credit_pools` | pools beyond `general`, their priority and eligible resources | rarely |
| `plans` / `plan_prices` / `plan_provider_prices` | the catalogue, per currency, per provider account; the pool each plan grants into | on a launch |
| `entitlement_keys` / `plan_entitlements` / `realm_defaults` | declared capabilities; what plans and the free tier unlock — including ceiling sizes | on a packaging change |
| `entitlement_overrides` | per-scope exceptions, including a per-customer ceiling | per deal — `reason` and `actor` are `NOT NULL` |
| `provider_accounts` | which provider accounts serve which realms, by secret reference | per account |
| `event_endpoints` | where hosts receive signed events | per integration |
| `auto_recharge` / `balance_watches` | per-scope top-up and low-balance notices | per customer |

Three rules make this safe:

1. **Rate cards are insert-only**, by trigger. Editing a price would re-price history and break every dispute replay.
2. **Every change is audited and propagated by the database.** An audit row with before/after, and a `NOTIFY` that every cache listens to — no TTL wait, no deploy.
3. **The ceiling count is data on both sides.** The engine evaluates `[]Limit` and the UI renders `LimitView[]`.

---

## Common shapes

**Free internal metering (no money).** No provider accounts, one `Balance` limit, `TaxStrategy=none`. Enforcement and a ledger, no fiat.

**Prepaid credit packs.** `plans.kind=one_time`, `CreditExpiry=lots_fefo`, a `promo` pool for signup credits, auto top-up offered per customer.

**Seat-based SaaS.** A `seat_day` card for the `table` pricer, `PlanChangePolicy=immediate_prorate`, entitlements carrying the seat quota. Most gating is entitlements, not credits.

**Usage-based API product.** Subject `api_key` on `hourly` + `rolling` limits sized by the plan, `RequireMaxCost=true`, `Settlement=journal` if revenue-loss RPO must be zero.

**Multi-product platform.** One deployment, one realm per product, per-realm cards/limits/plans, provider accounts scoped to their realms, per-realm caller credentials. Nothing crosses.
