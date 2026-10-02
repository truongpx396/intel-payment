---
paths:
  - "metering/adapters/driven/redis/**"
  - "metering/adapters/driven/redisstreams/**"
  - "specs/001-metering-billing-core/contracts/reference/**"
  - "deploy/redis/**"
---

# The Redis hot path

Sub-millisecond enforcement is one Redis Function per operation. The layout below is why it runs on
Redis Cluster, which refuses a script that touches two hash slots.

- **One call, one slot.** Every key a mutation touches carries the scope's shard tag —
  `acct:{s<n>}:<tag>`, `billing:outbox:{s<n>}` — so they hash to one slot
  (`specs/001-metering-billing-core/contracts/hot-path-consistency.md` §1). A key outside the tag works
  on standalone Redis and fails in production; test against cluster mode (`redistest.Cluster`).
- **The Lua is a contract.** `metering/adapters/driven/redis/lua/hot_path.lua` is loaded unchanged and a
  test fails if it differs from `specs/001-metering-billing-core/contracts/reference/hot_path.lua`.
  Change both together, in the same commit.
- **The outbox is a stream** (`XADD`/`XREADGROUP`/`XLEN`), never a list (D25).
  `scripts/check-spec-drift.sh` fails any document that describes the retired list mechanism as current.
- **The ACL is closed.** `deploy/redis/users.acl` gives the `payment` user `-@all` plus an explicit
  command list. A new Redis command in the adapter or the Lua needs an entry there, or production
  refuses it. Verify with `make verify-deploy`, which attempts what the ACL forbids.
- **Hot-tier outcomes are typed domain errors** (`ErrColdScope`, `ErrShardFrozen`, `ErrBackpressure`,
  `ErrHotStoreUnavailable`); the adapter never leaks go-redis types to the core.
- Verify with `make verify-hot-path` (Redis 7, cluster mode; Docker) and `make test-integration`, and
  paste the output.
