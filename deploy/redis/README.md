# Redis ACL (local development)

`users.acl` is mounted into the compose Redis. **A Redis ACL file cannot carry comments**, so the
reasoning lives here. The passwords are public dev values; production supplies its own file
([docs/security.md](../../docs/security.md), "The hot store is a money store"). Write access to this
Redis is the ability to mint credits, so the *shape* matters more than the secrets:

| User | Can | Cannot |
|---|---|---|
| `default` | nothing — **OFF**. A host has no credential at all | — |
| `payment` (`paymentd`, `payment-worker`) | `FCALL`/`FCALL_RO`; **exactly the commands the hot-path functions call on its behalf** (an ACL applies *inside* a function to the caller: `HGET/HMGET/HGETALL/HSET/HINCRBY`, `GET/SET/INCRBY/PEXPIRE/DEL`, `XLEN/XADD`); the writer's stream commands (`XGROUP/XREADGROUP/XACK/XAUTOCLAIM/XPENDING/XRANGE/XINFO/XTRIM`); `WAITAOF`; `INFO` (the replication id, to notice a failover) | `FUNCTION LOAD`, `CONFIG`, `FLUSH*`, `ACL`, `KEYS`, `SCAN`, `EVAL`, and every command not listed |
| `deploy` | `FUNCTION LOAD` | touch any data |
| `healthcheck` | `PING` | anything else |

Passwords are stored as sha256: `dev-payment`, `dev-deploy`, `dev-health`.

[`scripts/verify-deploy.sh`](../../scripts/verify-deploy.sh) proves every cell of that table by
attempting it.
