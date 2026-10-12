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
- **Status mapping is fixed**, one row per condition. `ErrorInfo.reason` (domain `metering.v1`) is how a client recovers the typed error: `errors.Is(err, domain.ErrIdemConflict)` after a round trip, never a string match. Each row is a test (`grpcserver`, `grpcclient`), and a test enumerates the exported errors of `domain/errors.go` and fails on any without a row, so a new error cannot ship unmapped.

| Condition | gRPC status | `ErrorInfo.reason` |
|---|---|---|
| no credential, or an invalid one | `UNAUTHENTICATED` | `unauthenticated` |
| the credential lacks the privilege (`record` calling `Grant`, …) | `PERMISSION_DENIED` | `forbidden` |
| a `Scope.realm` outside the credential's realms | `PERMISSION_DENIED` | `realm_mismatch` |
| malformed · a missing `idem_key`, `occurred_at` or scope · a bad timestamp or window | `INVALID_ARGUMENT` | `invalid_request` |
| a configured subject limit the call does not carry | `INVALID_ARGUMENT` | `missing_subject` |
| a caller-supplied window limit no configured limit backs | `INVALID_ARGUMENT` | `uncountable_limit` |
| an amount above `MaxOperationAmount`, or an overflow | `INVALID_ARGUMENT` | `amount_out_of_range` |
| no rate for the key or unit, or the card names an unregistered pricer | `FAILED_PRECONDITION` | `unpriceable` |
| `occurred_at` older than the dedup window | `FAILED_PRECONDITION` | `stale_event` |
| a transfer that would overdraw its source | `FAILED_PRECONDITION` | `insufficient_balance` |
| a transfer that would leave its destination above `max_dest_balance` | `FAILED_PRECONDITION` | `dest_balance_cap` |
| an idempotency key reused for a **different** request | `ALREADY_EXISTS` | `idempotency_conflict` |
| a transfer between two realms | `PERMISSION_DENIED` | `cross_realm` |
| the hot tier unreachable (`Admit` under `fail_closed`, any `Grant` or `Transfer`) | `UNAVAILABLE` | `hot_store_unavailable` |
| the same, by its specific cause: a cold shard · a frozen shard · the outbox at its cap · a cold account | `UNAVAILABLE` | `hot_shard_cold` · `hot_shard_frozen` · `outbox_backpressure` · `hot_account_cold` |
| a limit sized by an entitlement that could not be resolved | `UNAVAILABLE` | `quota_unavailable` |
| the configured shard count differs from the recorded one · a quota the engine should have absorbed | `INTERNAL` | `shard_count_mismatch` · `quota_not_granted` |
| the caller cancelled · the caller's deadline passed | `CANCELED` · `DEADLINE_EXCEEDED` | `cancelled` · `deadline_exceeded` |
| anything else | `INTERNAL` | `internal` |

  **A refused `Admit` is a decision, and it travels as a status** — `RESOURCE_EXHAUSTED` with `payment_required` (a `Balance` limit is exhausted) or `limit_reached` (a windowed limit is), `PERMISSION_DENIED` with `account_blocked` (held by `block_and_flag`) — **with the whole `metering.v1.Admission` as a status detail beside the `ErrorInfo`**. The client turns it back into the `(Admission{Allowed: false, …}, nil)` the in-process `Meter` returns, so a refusal is the same value on both sides of the wire (FR-044). A refusal the engine did not classify (no `Exceeded`, not blocked) is `INTERNAL`, never a guess. An allowed `Admit` is `OK` and may carry a `warning`.

  **What a caller may read.** The message of a row the caller can act on (invalid, unpriceable, stale, a conflict, a refused privilege) is the whole error — which field, how stale. The message of an `UNAVAILABLE` or `INTERNAL` row is the sentinel's own text and nothing else: the chain behind it names the topology (a dial address, a DSN), and it goes to the operator's log, whole.

  **Authorisation order.** Credential, then privilege, then the request's own validity, then realm: a stranger learns nothing about a request, and a caller without a privilege learns nothing about realms. An omitted `Scope.realm` is the credential's realm when it has exactly one and `INVALID_ARGUMENT` when it has several — never a guess about which product to charge. `Transfer` checks both ends.

A deferred `Record` is `OK` with `Receipt.deferred = true` — it succeeded; it has not been applied yet.

- **`GetBalance` reads what `Admit` reads**, so a cold account is rebuilt rather than reported (`ports.BalanceReader`); its `total` is the sum of the pools, and a sum that would wrap is `INTERNAL`.
- **The generated client is wrapped to satisfy `ports.Meter`.** That wrapper is what makes library↔service a wiring change rather than a refactor, and it is not optional.
- **Versioning** follows [rest-api.md § Versioning and deprecation](./rest-api.md#versioning-and-deprecation): additive within `v1`; a breaking change is a new package.
