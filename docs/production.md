# Production profile

What a deployment has to decide and set that the defaults do not. The compose stack in [deploy/](../deploy/docker-compose.yml)
is a development and verification stack; **no production manifest ships here**. This page is the list of what a production
manifest must say, and, for each line, whether it was checked and how. Reference material is elsewhere: every variable is in
[configuration.md](configuration.md), the threat model and its checklist in [security.md](security.md), the alerts' runbook in
[operations.md](operations.md), and what was measured in [hardening.md](hardening.md).

A line marked **tried** was run against the real binaries or the real library. A line marked **not tried** is reasoning, and is
here because leaving it out would be worse.

## Before the first deploy

- **Cut a tag.** No release tag exists yet, so migrations `0001`–`0005` are still a baseline that is corrected in place
  ([migrations/README.md](../migrations/README.md)). After the first tag they are additive only. Deploy from a tag, never from a
  branch, so that "what is running" has an answer.
- **Pin what the image is built from.** The Dockerfile names its two base images by tag, not by digest, and the CI actions are
  versioned, not pinned to a commit. Both follow a moving target between two builds of the same commit. Not done here.
- **Replace every public development value.** The Redis ACL file in [deploy/redis/](../deploy/redis/README.md) carries passwords
  that are published in this repository (`dev-payment`, `dev-deploy`, `dev-health`), and `.env.example` carries `CHANGE-ME` and
  `payment:payment` placeholders. Production supplies its own ACL file and its own credentials; keep the *shape* of the file (the
  `default` user off, `payment` limited to the hot-path commands, `deploy` able to load functions and nothing else).
- **Run the migrator, then the worker, then the server** ([operations.md](operations.md#deploy)), and gate the rollout on
  `/readyz`.

## The network

- **TLS on every hop a credential crosses.** `paymentd` serves TLS itself (`PAYMENT_TLS_CERT_FILE` and `PAYMENT_TLS_KEY_FILE`),
  or sits behind a proxy that terminates it. Bearer tokens over plaintext on a reachable listener are accepted and **warned
  about at start** (`paymentd` logs the warning once and names the variables); that is a prompt, not a refusal, because a proxy
  that terminates TLS is the normal shape. Be sure the proxy is the only way in.
- **`PAYMENT_METRICS_ADDR` is its own listener and is never published.** `/metrics` carries no credential and names every realm
  and its traffic. `paymentd` refuses to start with the API on a reachable address and no `PAYMENT_METRICS_ADDR`; the metrics
  listener is plain HTTP, so keeping it off any public network is yours. In the compose stack it is `:9092` and is not in `ports:`
  (**tried**: an integration test starts `paymentd` and shows `/metrics` answered on that listener and refused on the API's).
  `payment-worker` serves its own `/healthz` and `/metrics` on `PAYMENT_WORKER_ADDR`; keep that one off a public network too.
- **The hot Redis is network-isolated from hosts.** Write access to it is the ability to mint credits
  ([security.md](security.md#the-hot-store-is-a-money-store)). It is `noeviction`, AOF, and not a cache's Redis.
- **Rate-limit the webhook route at the edge.** `POST /webhooks/{provider}/{account}` has no authentication in front of it by
  design: the signature is the authentication, and it is checked before any parse. It is body-size limited and its verification
  failures are counted (`billing_webhook_verify_failed_total`), but **nothing in the binary limits the rate** of requests to it.
  Put the limit in the proxy or the load balancer. Not tried: no proxy configuration is shipped.
- **Keep `/v1/admin` off the public network**, and give no host an `admin` or `grant` privilege it does not need
  ([security.md](security.md#privilege-separation)).

## Postgres

`PAYMENT_LEDGER_DSN` is parsed by pgx, which accepts connection settings and server run-time parameters in the DSN, in either the
`postgres://…?key=value` or the `key=value key=value` form.

| Setting | Why | State |
|---|---|---|
| `sslmode=verify-full` and `sslrootcert=/path/ca.pem` | `.env.example` ships `sslmode=disable`, which is for the compose network only. `verify-full` checks the certificate chain **and** that the certificate names the host | **Tried, in part:** the DSN parses to a configuration that verifies the server name, and an `sslrootcert` that does not exist is refused at start, so a typo is loud. **Not tried:** a connection to a Postgres that serves TLS |
| `pool_max_conns=N` | `payment-worker` sizes its pool to `PAYMENT_SHARDS` + 16 when the DSN does not name `pool_max_conns` (it holds one connection per shard it owns), and `paymentd` leaves pgx's default. A DSN that names it wins in both. `max_connections` on the server must cover every replica ([operations.md](operations.md#deploy)) | **Tried:** a named value is kept and an unnamed one is filled in, in both DSN forms |
| `statement_timeout=…` (ms) on **`paymentd` only** | A query that stalls cannot hold a pooled connection past the server's own deadline, which is `PAYMENT_REQUEST_TIMEOUT` (25s by default). Set it a little *below* that so Postgres ends the statement before the request is cancelled | **Tried:** it reaches the connection as a run-time parameter. **Do not set it for `payment-worker` or `payment-migrate`**: the worker's reconcile, rollup and recovery statements and a migration are long by design, and how long they run on a real ledger has not been measured |
| `idle_in_transaction_session_timeout=…` (ms) | A transaction left open by a crashed client holds its locks and its connection; this ends it | **Tried:** it reaches the connection as a run-time parameter. The value that is safe for the worker's transactions has not been measured |

The request deadline is independent of these: with `PAYMENT_REQUEST_TIMEOUT` set, a request that stalls on Postgres is answered
`504 deadline_exceeded` at the deadline and the connection is released (**tried**, against a real Postgres, with a table lock
held by the test).

**A connection pooler.** The worker owns a shard by a Postgres *session* advisory lock, and so does the migrator; a pooler in
transaction mode hands the connection to someone else between statements and the lock goes with it. If a pooler is used at all,
`payment-worker` and `payment-migrate` must connect directly or through a pooler in **session mode**. Reasoned from how the
locks are taken, not tried: no pooler has been put between this stack and Postgres.

**Backups.** A backup that has not been restored is not a backup ([operations.md](operations.md#backups-and-retention)).
Nothing in this repository detaches or archives a ledger partition, so the ledger grows (569 bytes a row measured) until an
operator does it.

## Redis and durability

Balances are rebuilt from the books, but the outbox holds intents that are **not** derived from anything: AOF is load-bearing.

- `PAYMENT_SETTLEMENT=outbox` (the default) can lose the last acknowledged intents on a Redis crash or failover: silent
  under-billing, never double-billing. `journal` settles to Postgres first and has no such window, at a latency cost.
- `PAYMENT_HOT_ACK_WAIT` narrows the outbox's window (`aof_local`, `aof_replica`) and its latency depends on `appendfsync`; read
  the row in [configuration.md](configuration.md) before choosing it.
- Say which of these the deployment chose, and why, in its own runbook. The defaults are the cheapest, not the safest.

## Settings to choose on purpose

The defaults are permissive where a refusal would break a deployment that has not yet chosen. Each below is a decision, not a
tuning knob.

| Setting | Default | What the default costs | Where it is loud |
|---|---|---|---|
| `PAYMENT_CHECKOUT_RETURN_HOSTS` | empty: any https host | A host that forwards a return URL it did not write offers its customers an open redirect after payment | `paymentd` logs a warning at start |
| `PAYMENT_UNDERPAYMENT_POLICY` | `hold_zero` | A verified payment of **nothing** for a paid plan is held for an operator; a discounted one is granted in full and counted. `hold` for a deployment that never runs a coupon; `grant` for one that uses a provider trial as free credits | `billing_payment_underpaid_total` and the `BillingPaymentHeldUnderpaid` alert ([operations.md](operations.md#alert-a-payment-of-nothing-was-held)) |
| `scope_balance` limit (a realm row, not a variable) | none | A realm with a rate card and **no** `scope_balance` limit admits an unfunded scope: nothing checks what it holds. The engine does not refuse this | `metering_admit_no_balance_limit_total`, which should be zero for a realm that sells credits ([integration-guide.md](integration-guide.md)) |
| `PAYMENT_ADMIT_FAIL_POLICY` | `fail_closed` | `fail_open` serves work while the hot tier is unavailable and meters it on recovery: a choice between an outage and unbilled work | every occurrence is counted |
| `PAYMENT_REQUIRE_MAX_COST` | `false` | An `Admit` with no cost bound is admitted | `metering_admit_uncapped_total`; turn this on once it is flat at zero |
| `PAYMENT_SERVICE_AUTH` | `bearer` | `none` is refused unless both listeners are loopback or a unix socket | refusal at start |
| `PAYMENT_EVENTS_ALLOW_PRIVATE_ENDPOINTS` | `false` | `true` lets an outbound webhook reach a private address. Development only | — |

## Alerts reach a human

[deploy/alerts.yml](../deploy/alerts.yml) holds the rules, each tied to a runbook entry by a test. What routes them (the
Alertmanager receivers, the on-call schedule) is the deployment's and is not shipped. At minimum: drift, a frozen shard, outbox
depth, webhook verification failures, the held-underpayment alert and dead letters.

## What has not been rehearsed

Stated so that nobody reads this page as a clearance.

- **Provider sandboxes met once** ([sandbox.md](sandbox.md)). Not tried: a buyer approving a PayPal order, a paid Polar order,
  PayPal subscriptions, Stripe test clocks, a live-mode account. Stripe's `amount_paid` includes tax, and the underpayment check
  compares it to the plan's price, so a deployment that charges tax on top of a plan's price needs the policy chosen with that in
  mind.
- **Load tested once, on a laptop** ([hardening.md](hardening.md)). Not shown: the 99th percentile at the target load, Redis
  Cluster, a production network.
- **The runbook was exercised in part** (Redis killed, a truncated AOF, drift, a poisoned intent, an operator's shard recovery).
  Not exercised: the Postgres half of a region failover.
- **Not built:** a GDPR erasure story, a suspension stage after dunning's grace, an admin API for entitlements and event
  endpoints, auto top-up on Polar and PayPal ([README](../README.md), Known gaps).

The honest summary is that this is ready for a staging pilot with a real provider's test mode and a few cooperative hosts, and
not yet for real money until the lines above that say **not tried** have each been tried.
