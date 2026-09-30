#!/usr/bin/env bash
# Fails when a document still describes a RETIRED mechanism as current. The contracts are
# normative; a contract that contradicts another is a defect even before any code exists.
#
# design-decisions.md and PROVENANCE.md are exempt: recording what was retired, and why, is their job.
# To retire another term, add a line below: <extended regex> | <what replaced it>.
set -euo pipefail
cd "$(dirname "$0")/.."

retired=$(cat <<'EOF'
\bLPUSH\b|\bLPOP\b|\bLLEN\b|Redis lists|the outbox is a stream (D25); XADD / XREADGROUP / XLEN
outbox:\{shard\}|credit:\{[a-z]|keys are shard-tagged {s<n>}: acct:{s<n>}:<tag>, billing:outbox:{s<n>} (D29)
micros_per_credit|micros_per_unit|MicrosPerCredit|\bLLMTokenPricer\b|prices are rationals: credits_per_block / block_size, table pricer (D31)
natsbus|the default bus is redisstreams; JetStream sits behind a relay (bus-subjects.md)
IdemCacheTTL|IDEMPOTENCY_CACHE_TTL|HotIdemTTL + UsageIdemWindow (D32)
\.down\.sql|migrate-down|migration-up-down|migrations are forward-only (D27)
ReconcileReport|operation_type='reconcile'|compensating row to heal|heals? the balance upward|reconcile heals the hot side only, at equal sequence numbers, and never books a row (D39)
billing\.allocate|transfers complete through transfer_out / transfer_in intents (D33)
webhooks/\{provider\}([^/]|$)|webhooks arrive at /webhooks/{provider}/{account} (D36)
Charge\.Job|job budgets are keyed by Subjects["job"] (D30)
[Ii]mplementation not started|no Go code yet|\b0 lines of Go|the build status is the task count in ROADMAP.md, which this script checks against tasks.md
EOF
)

fail=0
while IFS='|' read -r -a parts; do
  n=${#parts[@]}
  why="${parts[$((n - 1))]}"
  pattern="$(IFS='|'; echo "${parts[*]:0:$((n - 1))}")"
  # Tracked AND untracked-but-not-ignored files, so a local run catches drift before `git add`.
  hits=$(git ls-files -co --exclude-standard -- '*.md' '*.sql' '*.yml' '*.yaml' '*.lua' '*.go' '*.proto' 'Makefile' '.env.example' \
           ':!specs/001-metering-billing-core/design-decisions.md' ':!PROVENANCE.md' \
         | xargs grep -nE "$pattern" 2>/dev/null || true)
  if [ -n "$hits" ]; then
    fail=1
    echo "RETIRED: /$pattern/  →  $why"
    echo "$hits" | sed 's/^/    /'
  fi
done <<< "$retired"

[ "$fail" -eq 0 ] && echo "no retired mechanism is described as current"

# A status claim that nothing checks goes stale: ROADMAP.md said "implementation not started" while
# forty tasks were done and verified. So the claim is DERIVED — ROADMAP.md must state each spec's task
# count exactly as its tasks.md has it, and a change to one without the other fails here. A task line
# is "- [ ] **R…/T…" (done: "[x]").
status_fail=0
for tasks in specs/*/tasks.md; do
  spec=$(basename "$(dirname "$tasks")")
  total=$(grep -cE '^- \[[ x]\] \*\*[RT][0-9]' "$tasks" || true)
  done_=$(grep -cE '^- \[x\] \*\*[RT][0-9]' "$tasks" || true)
  want="Tasks: ${done_} of ${total} done (specs/${spec}/tasks.md)"
  if ! grep -qF "$want" ROADMAP.md; then
    status_fail=1
    echo "STALE STATUS: ROADMAP.md does not state this spec's task count as tasks.md has it."
    echo "    expected the line: $want"
    have=$(grep -F "(specs/${spec}/tasks.md)" ROADMAP.md | grep -F 'Tasks:' || true)
    [ -n "$have" ] && echo "    found:             $have"
  fi
done
[ "$status_fail" -eq 0 ] && echo "ROADMAP.md states every spec's task count as its tasks.md has it"
[ "$status_fail" -eq 0 ] || fail=1
exit "$fail"
