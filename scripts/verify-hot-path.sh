#!/usr/bin/env bash
# Verifies the reference hot path (specs/001-metering-billing-core/contracts/reference/hot_path.lua)
# against Redis 7 running in CLUSTER mode, where a script touching two hash slots is refused.
#
#   scripts/verify-hot-path.sh                    # starts and removes its own container
#   IP_REDIS_CONTAINER=name scripts/verify-hot-path.sh   # reuse a running cluster-mode Redis
#
# The contract it checks is hot-path-consistency.md. Exits non-zero on any failure.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
lib="$root/specs/001-metering-billing-core/contracts/reference/hot_path.lua"
c="${IP_REDIS_CONTAINER:-}"

if [ -z "$c" ]; then
  c="ip-hotpath-verify-$$"
  docker run -d --rm --name "$c" redis:7-alpine redis-server \
    --cluster-enabled yes --appendonly yes --maxmemory-policy noeviction >/dev/null
  trap 'docker rm -f "$c" >/dev/null 2>&1 || true' EXIT
  for _ in $(seq 1 50); do docker exec "$c" redis-cli ping >/dev/null 2>&1 && break; sleep 0.2; done
  docker exec "$c" redis-cli CLUSTER ADDSLOTSRANGE 0 16383 >/dev/null
fi
for _ in $(seq 1 50); do
  docker exec "$c" redis-cli CLUSTER INFO | grep -q 'cluster_state:ok' && break; sleep 0.2
done

cli()  { docker exec -i "$c" redis-cli "$@" 2>&1; }
flat() { tr '\n' ' ' | sed -e 's/ *$//'; }
pass=0; fail=0; intents=0
check() {
  if [ "$2" = "$3" ]; then pass=$((pass + 1)); printf 'ok   %s\n' "$1"
  else fail=$((fail + 1)); printf 'FAIL %s\n       want: %s\n       got:  %s\n' "$1" "$2" "$3"; fi
}
contains() {
  case "$3" in *"$2"*) pass=$((pass + 1)); printf 'ok   %s\n' "$1" ;;
    *) fail=$((fail + 1)); printf 'FAIL %s\n       want substring: %s\n       got: %s\n' "$1" "$2" "$3" ;; esac
}
applied() { case "$1" in APPLIED*) intents=$((intents + 1)) ;; esac; }

cli FLUSHALL >/dev/null
cli FUNCTION FLUSH >/dev/null
docker exec -i "$c" redis-cli -x FUNCTION LOAD REPLACE < "$lib" >/dev/null

S='{s7}'; META="meta:$S"; STREAM="billing:outbox:$S"; TTL=86400000; MAXLEN=1000000
acct() { echo "acct:$S:t1/org:$1"; }
idem() { echo "idem:$S:t1/org:$1:$2:$3"; }

# debit <scope> <key> <amount> <fp> <pools> [watch] [counter-key incr ttl]...
debit() {
  local s="$1" k="$2" a="$3" fp="$4" pools="$5" watch="${6:-}"; shift 6 2>/dev/null || shift $#
  local keys=("$META" "$(acct "$s")" "$(idem "$s" u "$k")" "$STREAM") argv=()
  while [ $# -ge 3 ]; do keys+=("$1"); argv+=("$2" "$3"); shift 3; done
  cli FCALL ip_debit "${#keys[@]}" "${keys[@]}" "$a" "$fp" "$TTL" "$MAXLEN" "$pools" \
    t1 org "$s" "$k" '{"resource":"llm.chat"}' "$watch" ${argv[@]+"${argv[@]}"} | flat
}
grant() {  # grant <scope> <key> <pool> <amount> <policy>
  cli FCALL ip_grant 4 "$META" "$(acct "$1")" "$(idem "$1" g "$2")" "$STREAM" \
    "$3" "$4" "fp-$2" "$TTL" "$MAXLEN" "$5" t1 org "$1" "$2" '{}' | flat
}
rehydrate() {  # rehydrate <scope> <gen> <seq> <blocked> [pool bal]...
  local s="$1"; shift
  cli FCALL ip_rehydrate 2 "$META" "$(acct "$s")" "$@" | flat
}
admit() { cli FCALL_RO ip_admit 2 "$META" "$(acct "$1")" | flat; }

echo "== layout"
r=$(cli EVAL "redis.call('DECRBY', KEYS[1], ARGV[1]); redis.call('XADD', KEYS[2], '*', 'd', ARGV[1]); return 1" \
      2 'credit:{t1/org:o1}:balance' 'billing:outbox:{7}' 100 | flat)
contains "original layout (scope tag + shard tag) is refused by Redis Cluster" "CROSSSLOT" "$r"
r=$(cli FCALL ip_debit 4 "$META" "$(acct o1)" "$(idem o1 u x)" 'billing:outbox:{s8}' 1 fp 1 1 general t1 org o1 x '{}' '' | flat)
contains "a function given keys from two shards is refused too" "CROSSSLOT" "$r"

echo "== shard lifecycle and cold start"
check "no shard meta → COLD_SHARD" "COLD_SHARD" "$(debit o1 k1 10 fpA general '')"
check "open shard at gen 1" "OK 1" "$(cli FCALL ip_shard 1 "$META" open 1 | flat)"
check "open is idempotent and never lowers gen" "OK 1" "$(cli FCALL ip_shard 1 "$META" open 0 | flat)"
check "unknown account → COLD_SCOPE" "COLD_SCOPE" "$(debit o1 k1 10 fpA general '')"
check "rehydrate installs booked state" "OK" "$(rehydrate o1 1 0 0 general 1000 promo 100)"
check "a second rehydrate converges (ALREADY)" "ALREADY" "$(rehydrate o1 1 0 0 general 5 promo 5)"
check "rehydrate with a stale gen is refused" "STALE_GEN 1" "$(rehydrate o1 2 0 0 general 5)"

echo "== debit, idempotency, pools"
r=$(debit o1 k1 150 fpA promo,general ''); applied "$r"
check "debit draws promo first, then general" "APPLIED 1 950" "$r"
check "hot balances after the draw (promo, general)" "0 950" "$(cli HMGET "$(acct o1)" b:promo b:general | flat)"
contains "the intent records the draws per pool" "promo:-100;general:-50" "$(cli XREVRANGE "$STREAM" + - COUNT 1 | flat)"
check "replay (same key, same fingerprint) → REPLAY with the original seq" "REPLAY 1" "$(debit o1 k1 150 fpA promo,general '')"
check "reused key with a different fingerprint → IDEM_CONFLICT" "IDEM_CONFLICT" "$(debit o1 k1 999 fpB promo,general '')"
check "neither replay nor conflict moved the balance" "950" "$(cli HGET "$(acct o1)" b:general | flat)"
r=$(debit o1 k2 2000 fpC general ''); applied "$r"
check "the last eligible pool absorbs overshoot" "APPLIED 2 -1050" "$r"

echo "== counters and low-balance watch"
day="ctr:$S:t1/org:o1:user_daily:u1:20260928"
r=$(debit o1 k3 10 fpD general 'general:-1055' "$day" 10 60000); applied "$r"
check "seq increases by exactly one per mutation" "APPLIED 3 -1060" "$r"
check "window counter incremented" "10" "$(cli GET "$day" | flat)"
ttl=$(cli PTTL "$day" | flat); [ "$ttl" -gt 0 ] && [ "$ttl" -le 60000 ] && t=ok || t="$ttl"
check "window counter carries its expiry" "ok" "$t"
contains "crossing the watch threshold is flagged on the intent" "general:-1055" "$(cli XREVRANGE "$STREAM" + - COUNT 1 | flat)"
r=$(debit o1 k4 10 fpE general 'general:-1055'); applied "$r"
case "$(cli XREVRANGE "$STREAM" + - COUNT 1 | flat)" in *" lw "*) t=flagged ;; *) t=silent ;; esac
check "already below the threshold → not flagged again" "silent" "$t"
check "admit reads balances and counters in one call" "OK 4 0 general -1070 promo 0" "$(admit o1)"

echo "== grants and the negative-balance policy"
rehydrate o2 1 0 0 general 10 >/dev/null
r=$(grant o2 refund-1 general -100 clamp_to_zero); applied "$r"
check "clamp_to_zero floors at zero and reports the writeoff" "APPLIED 1 0 90" "$r"
contains "the writeoff travels on the intent" "wo 90" "$(cli XREVRANGE "$STREAM" + - COUNT 1 | flat)"
rehydrate o3 1 0 0 general 10 >/dev/null
r=$(grant o3 cb-1 general -100 block_and_flag); applied "$r"
check "block_and_flag floors and blocks" "1" "$(cli HGET "$(acct o3)" blocked | flat)"
check "admit reports the block" "OK 1 1 general 0" "$(admit o3)"
rehydrate o5 1 0 0 general -50 >/dev/null
r=$(grant o5 refund-2 general -100 clamp_to_zero); applied "$r"
check "an existing overshoot debt is never forgiven by a clamp" "APPLIED 1 -50 100" "$r"
r=$(grant o2 pay-1 general 500 clamp_to_zero); applied "$r"
check "a positive grant applies" "APPLIED 2 500 0" "$r"

echo "== transfer (source shard only) and writer-issued deltas"
rehydrate o4 1 0 0 general 500 >/dev/null
check "a transfer refuses rather than overdrawing" "INSUFFICIENT 500" \
  "$(cli FCALL ip_transfer_out 4 "$META" "$(acct o4)" "$(idem o4 t alloc-0)" "$STREAM" general 501 fp-t0 "$TTL" "$MAXLEN" t1 org o4 alloc-0 '{}' | flat)"
r=$(cli FCALL ip_transfer_out 4 "$META" "$(acct o4)" "$(idem o4 t alloc-1)" "$STREAM" general 200 fp-t1 "$TTL" "$MAXLEN" t1 org o4 alloc-1 '{"to":"t1/org:w1"}' | flat); applied "$r"
check "a transfer debits the source" "APPLIED 1 300" "$r"
r=$(cli FCALL ip_apply_delta 4 "$META" "$(acct o4)" "$(idem o4 x lot-9)" "$STREAM" expiry general -1000 fp-x "$TTL" "$MAXLEN" t1 org o4 lot-9 '{}' 1 | flat); applied "$r"
check "expiry pre-application is capped at the available balance" "APPLIED 2 0 -300" "$r"
r=$(cli FCALL ip_apply_delta 4 "$META" "$(acct o4)" "$(idem o4 c s-1)" "$STREAM" correction general 120 fp-c "$TTL" "$MAXLEN" t1 org o4 s-1 '{}' 0 | flat); applied "$r"
check "a correction applies its delta" "APPLIED 3 120 120" "$r"
contains "unknown writer ops are rejected" "ERR op" \
  "$(cli FCALL ip_apply_delta 4 "$META" "$(acct o4)" "$(idem o4 c s-2)" "$STREAM" usage general 1 fp "$TTL" "$MAXLEN" t1 org o4 s-2 '{}' 0 | flat)"

echo "== freeze, generation bump, heal"
cli FCALL ip_shard 1 "$META" freeze rollback >/dev/null
check "a frozen shard refuses mutations" "FROZEN" "$(debit o1 k5 1 fpF general '')"
check "a frozen shard refuses admission reads" "FROZEN" "$(admit o1)"
check "bump raises the generation" "OK 2" "$(cli FCALL ip_shard 1 "$META" bump 2 | flat)"
cli FCALL ip_shard 1 "$META" unfreeze >/dev/null
check "after a bump every account is stale" "COLD_SCOPE" "$(debit o1 k5 1 fpF general '')"
check "rehydrate at the new generation" "OK" "$(rehydrate o1 2 4 0 general -1070 promo 0)"
check "seq resumes at applied_seq (no number reissued)" "OK 4 0 general -1070 promo 0" "$(admit o1)"
check "heal refuses when seq moved since the measurement" "RACE" "$(cli FCALL ip_heal 2 "$META" "$(acct o1)" 3 general 0 | flat)"
check "heal applies at the measured seq" "OK" "$(cli FCALL ip_heal 2 "$META" "$(acct o1)" 4 general -1000 | flat)"
check "heal is hot-side only and does not advance seq" "OK 4 0 general -1000 promo 0" "$(admit o1)"

echo "== backpressure"
cli FCALL ip_shard 1 'meta:{s9}' open 1 >/dev/null
cli FCALL ip_rehydrate 2 'meta:{s9}' 'acct:{s9}:t1/org:o9' 1 0 0 general 100 >/dev/null
cli XADD 'billing:outbox:{s9}' '*' filler 1 >/dev/null
check "a full stream refuses the mutation instead of dropping or trimming" "BACKPRESSURE" \
  "$(cli FCALL ip_debit 4 'meta:{s9}' 'acct:{s9}:t1/org:o9' 'idem:{s9}:t1/org:o9:u:k' 'billing:outbox:{s9}' 1 fp 1000 1 general t1 org o9 k '{}' '' | flat)"
check "and changes nothing" "100" "$(cli HGET 'acct:{s9}:t1/org:o9' b:general | flat)"

echo "== one intent per mutation"
check "stream length equals the number of applied mutations" "$intents" "$(cli XLEN "$STREAM" | flat)"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
