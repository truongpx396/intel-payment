# Contract: gRPC Surface

**Spec**: [../spec.md](../spec.md) | **Status**: Normative — the service-mode transport for the hot path.

The authoritative `metering.v1` definition, the wrapping rules and the reasoning live in
[metering-ports.md § Service surface](./metering-ports.md#service-surface-meteringservice-grpc-contract-locked)
— this file exists so the wire contract is findable by name, and to record what gRPC adds beyond the
message shapes.

**Why gRPC and not REST for the hot path:** `Admit` is on every host request. It needs a tight
deadline, a strongly-typed contract, connection reuse and a generated client that *satisfies the
`Meter` interface* — so business code depends on `ports.Meter` and cannot tell an in-process value
from a remote one. REST ([rest-api.md](./rest-api.md)) covers non-Go hosts, browser-facing
snapshots, configuration and operations.

## Services

| Service | RPCs | Notes |
|---|---|---|
| `metering.v1.Metering` | `Admit`, `Record`, `GetBalance`, `Grant`, `Transfer` | `Grant` and `Transfer` are **privileged** and separated at the RPC level, so a compromised spend producer can debit but never mint or move credits |
| `billing.v1.Billing` | `ListPlans`, `CreateCheckout`, `GetSubscription`, `ChangePlan`, `Cancel`, `GetPortal`, `GetAutoRecharge`, `PutAutoRecharge`, `ListPayments`, `GetFulfilment` | No webhook RPC: providers speak HTTP, and the raw body must survive verification untouched |
| `entitlement.v1.Entitlement` | `Check`, `Resolve` | `Check` is batched — a page render asks about several capabilities at once |
| `events.v1.Events` | `List` | The event feed ([outbound-events.md](./outbound-events.md)) for gRPC-native hosts |

Configuration and operations are REST-only (`/v1/admin/*`): they are low-frequency, human-driven
and benefit more from curl-ability than from a generated client.

## Rules

- **One atomic operation per RPC.** `Admit` is one round trip that evaluates every limit server-side.
- **No streaming RPC for partial settlement.** A cancelled unit of work settles with one terminal `Record` carrying the quantities actually produced.
- **Money crosses as `int64`.** Credits signed, fiat as minor units + ISO-4217. No floats, no decimal strings.
- **`idem_key` required on every mutating RPC**, and `occurred_at` on `Record`. A missing one is `INVALID_ARGUMENT`, never a generated default — a server-generated key makes every retry a new charge.
- **The realm is authenticated, never asserted.** A `Scope.realm` outside the credential's realm set is `PERMISSION_DENIED`.
- **Deadlines are the caller's, and so is its fail policy.** The server's `AdmitFailPolicy` covers only its own hot-store outage; `Record` defers to the journal rather than failing.
- **Status mapping is fixed:**

| Condition | gRPC status | Detail (`ErrorInfo.reason`) |
|---|---|---|
| balance or windowed limit exhausted | `RESOURCE_EXHAUSTED` | `payment_required` · `limit_reached` |
| account held by `block_and_flag` | `PERMISSION_DENIED` | `account_blocked` |
| idempotency key reused for a different request · transfer would overdraw | `ALREADY_EXISTS` · `FAILED_PRECONDITION` | `idempotency_conflict` · `insufficient_balance` |
| unpriceable · stale event · amount out of range · missing subject | `FAILED_PRECONDITION` · `INVALID_ARGUMENT` | `unpriceable` · `stale_event` · `amount_out_of_range` · `missing_subject` |
| privilege or realm violation | `PERMISSION_DENIED` | `forbidden` · `realm_mismatch` |
| `Admit` with the shard unavailable under `fail_closed` | `UNAVAILABLE` | `hot_store_unavailable` |

A deferred `Record` is `OK` with `Receipt.deferred = true` — it succeeded; it has not been applied yet.

- **The generated client is wrapped to satisfy `ports.Meter`.** That wrapper is what makes library↔service a wiring change rather than a refactor, and it is not optional.
- **Versioning** follows [rest-api.md § Versioning and deprecation](./rest-api.md#versioning-and-deprecation): additive within `v1`; a breaking change is a new package.
