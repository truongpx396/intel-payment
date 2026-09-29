# Redis ACL (local development)

`users.acl` is mounted into the compose Redis. **A Redis ACL file cannot carry comments**, so the
reasoning lives here. The passwords are public dev values; production supplies its own file
([docs/security.md](../../docs/security.md), "The hot store is a money store"). Write access to this
Redis is the ability to mint credits, so the *shape* matters more than the secrets:

| User | Can | Cannot |
|---|---|---|
| `default` | nothing — **OFF**. A host has no credential at all | — |
| `payment` (`paymentd`, `payment-worker`) | `FCALL`/`FCALL_RO` the hot-path functions; read/write the outbox streams; read | `FUNCTION LOAD`, `CONFIG`, `FLUSH*`, `ACL`, `KEYS`, anything `@dangerous` |
| `deploy` | `FUNCTION LOAD` | touch any data |
| `healthcheck` | `PING` | anything else |

Passwords are stored as sha256: `dev-payment`, `dev-deploy`, `dev-health`.

[`scripts/verify-deploy.sh`](../../scripts/verify-deploy.sh) proves every cell of that table by
attempting it.
