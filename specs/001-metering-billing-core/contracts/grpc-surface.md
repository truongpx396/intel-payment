# Contract: gRPC Surface

**Spec**: [../spec.md](../spec.md) | **Status**: Normative — the service-mode transport for the hot path.

The authoritative `metering.v1` definition, the wrapping rules and the reasoning live in
[metering-ports.md § Service surface](./metering-ports.md) — this file exists so the wire
contract is findable by name, and to record what gRPC adds beyond the message shapes.

**Why gRPC and not REST for the hot path:** `Admit` is on every host request. It needs a
tight deadline, a strongly-typed contract, connection reuse and a generated client that
*satisfies the `Meter` interface* — so business code depends on `ports.Meter` and cannot
tell an in-process value from a remote one. REST ([rest-api.md](./rest-api.md)) covers
non-Go hosts, browser-facing snapshots and operations.

## Services

| Service | RPCs | Notes |
|---|---|---|
| `metering.v1.Metering` | `Admit`, `Record`, `Grant`, `GetBalance` | `Grant` is **privileged** and separated at the RPC level, so a compromised spend producer can debit but never mint |
| `billing.v1.Billing` | `ListPlans`, `CreateCheckout`, `GetSubscription`, `ChangePlan`, `Cancel`, `GetPortal`, `ListPayments`, `GetFulfilment` | No webhook RPC: providers speak HTTP, and the raw body must survive verification untouched |
| `entitlement.v1.Entitlement` | `Check`, `Resolve`, `PutOverride` | `Check` is batched — a page render asks about several capabilities at once |

## Rules

- **One atomic operation per RPC.** The API stays coarse: `Admit` is one round trip that
  evaluates every limit server-side. A chatty metering API taxes every host request.
- **No streaming RPC for partial settlement.** A cancelled unit of work settles with one
  terminal `Record` carrying the quantities actually produced. Settlement is a single call.
- **Money crosses as `int64`.** Credits signed, fiat as minor units + ISO-4217. No floats,
  no decimal strings.
- **`idem_key` required on every mutating RPC.** A missing one is `INVALID_ARGUMENT`, never
  a generated default — a server-generated key makes every retry a new charge.
- **The realm is authenticated, never asserted.** A caller's credential binds it to a realm
  set; a `Scope.realm` outside it is `PERMISSION_DENIED`. See [security.md](../../../docs/security.md).
- **Deadlines are the caller's, and so is the fail policy.** The server is stateless
  per-RPC. A caller sets a tight `Admit` deadline and decides fail-open vs fail-closed
  deliberately; the server's `AdmitFailPolicy` covers only its *own* hot-store outage.
- **Status mapping is fixed:** `RESOURCE_EXHAUSTED` for an exhausted balance or limit (with
  the deny code in the detail), `FAILED_PRECONDITION` for an unpriceable event,
  `PERMISSION_DENIED` for privilege and realm violations, `UNAVAILABLE` for a hot-store
  outage under `fail_closed`.
- **The generated client is wrapped to satisfy `ports.Meter`.** That wrapper is what makes
  library↔service a wiring change rather than a refactor, and it is not optional.
