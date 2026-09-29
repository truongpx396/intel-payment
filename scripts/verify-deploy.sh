#!/usr/bin/env bash
# Verifies what deploy/ promises, by running it: the image builds distroless and non-root, the
# migrator brings an empty Postgres to head (and `check` agrees), and the Redis ACL gives a host
# nothing while giving the service tier exactly the hot path. Needs Docker; starts its own project
# and removes it, volumes included.
#
#   scripts/verify-deploy.sh
set -uo pipefail
cd "$(dirname "$0")/.."

export COMPOSE_PROJECT_NAME=ip-verify-deploy
# Unusual host ports: a developer's own Postgres/Redis may already hold 5432/6379.
export PAYMENT_POSTGRES_PORT=${PAYMENT_POSTGRES_PORT:-25432} PAYMENT_REDIS_PORT=${PAYMENT_REDIS_PORT:-26379}
dc() { docker compose -f deploy/docker-compose.yml "$@"; }
trap 'dc down -v --remove-orphans >/dev/null 2>&1' EXIT

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && echo "       $2"; }
# assert <label> <expected-substring> <output>
has() { case "$3" in *"$2"*) ok "$1" ;; *) bad "$1" "want substring '$2', got: $3" ;; esac; }
hasnt() { case "$3" in *"$2"*) bad "$1" "must not contain '$2', got: $3" ;; *) ok "$1" ;; esac; }
rcli() { dc exec -T redis redis-cli --no-auth-warning "$@" 2>&1; }

echo "== compose file is valid"
dc config -q && ok "docker compose config" || bad "docker compose config"

echo "== image"
dc build migrate >/dev/null 2>&1 && ok "image builds" || { bad "image builds"; exit 1; }
img="${COMPOSE_PROJECT_NAME}-migrate"
user="$(docker image inspect "$img" --format '{{.Config.User}}' 2>/dev/null)"
has "runs as non-root" "nonroot" "$user"
shell="$(docker run --rm --entrypoint /bin/sh "$img" -c true 2>&1)"
has "distroless: no shell in the image" "no such file or directory" "$shell"

echo "== stack: postgres + redis + migrate"
dc up -d --wait postgres redis >/dev/null 2>&1 && ok "postgres and redis healthy" || bad "postgres and redis healthy"

mig() { dc run --rm --no-deps -T --entrypoint /usr/local/bin/payment-migrate migrate "$@" 2>&1; }
before="$(mig check)"
has "an empty database is reported behind" "schema is behind head" "$before"
up="$(mig up)"
has "migrate up reaches head" "schema at head" "$up"
after="$(mig check)"
has "check passes at head" "current=0005 pending=[]" "$after"
hasnt "check reports no error at head" "behind" "$after"
again="$(mig up)"
has "second migrate up applies nothing" "0 applied this run" "$again"

echo "== redis: noeviction + AOF"
# No principal may CONFIG GET (that is the point of the ACL), so read the flags off the process.
argv="$(docker inspect --format '{{json .Config.Cmd}}' "$(dc ps -q redis)")"
has "maxmemory-policy noeviction" "noeviction" "$argv"
has "AOF on" "appendonly" "$argv"

echo "== redis ACL: a host has nothing; the service tier has only the hot path"
hasnt "no credential, no access" "PONG" "$(rcli PING)"
has   "default user is off (NOAUTH)" "NOAUTH" "$(rcli GET x)"
has   "wrong password refused" "WRONGPASS" "$(rcli --user payment --pass nope PING)"
has   "payment can PING" "PONG" "$(rcli --user payment --pass dev-payment PING)"
has   "payment cannot FLUSHALL" "NOPERM" "$(rcli --user payment --pass dev-payment FLUSHALL)"
has   "payment cannot CONFIG SET" "NOPERM" "$(rcli --user payment --pass dev-payment CONFIG SET maxmemory-policy allkeys-lru)"
has   "payment cannot ACL" "NOPERM" "$(rcli --user payment --pass dev-payment ACL LIST)"
has   "payment cannot KEYS" "NOPERM" "$(rcli --user payment --pass dev-payment KEYS '*')"
has   "payment cannot FUNCTION LOAD" "NOPERM" "$(rcli --user payment --pass dev-payment FUNCTION LOAD '#!lua name=x
redis.register_function("f", function() return 1 end)')"
lib='#!lua name=acltest
redis.register_function("acl_ping", function(keys, args) return "ran" end)'
has   "deploy can FUNCTION LOAD" "acltest" "$(rcli --user deploy --pass dev-deploy FUNCTION LOAD "$lib")"
has   "payment can FCALL a loaded function" "ran" "$(rcli --user payment --pass dev-payment FCALL acl_ping 0)"
has   "deploy cannot read data" "NOPERM" "$(rcli --user deploy --pass dev-deploy GET x)"
has   "healthcheck can PING" "PONG" "$(rcli --user healthcheck --pass dev-health PING)"
has   "healthcheck cannot read data" "NOPERM" "$(rcli --user healthcheck --pass dev-health GET x)"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
