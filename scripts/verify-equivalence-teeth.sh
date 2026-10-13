#!/usr/bin/env bash
# The Meter equivalence suite (metering/contracts.MeterEquivalence) is a conformance suite, so it is held to
# the rule it enforces: shown to FAIL against a deliberately broken implementation. Each variant in
# integration/equivalence_teeth_test.go is a faithful gRPC client with ONE defect of a kind a wire is prone to
# — an error flattened to its text, a missing time encoded as the epoch, a field dropped, a call sent twice, a
# refusal reported as a failure. The suite must reject every one, and name the subtest that caught it.
#
# Needs Docker (the suite runs on real Redis and Postgres). Run by CI beside the integration tests.
#
#   scripts/verify-equivalence-teeth.sh
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0
# <variant> <a substring the failing subtest's name must contain>
check() {
  local variant=$1 caught=$2 out rc=0
  out=$(EQUIVALENCE_BROKEN="$variant" go test -tags=integration -count=1 -run '^TestMeterEquivalenceBroken$' ./integration/ 2>&1) || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "  FAIL  $variant — the suite PASSED a broken transport"; fail=1
  elif ! grep -qE -- "--- FAIL: TestMeterEquivalenceBroken/.*${caught}" <<<"$out"; then
    echo "  FAIL  $variant — rejected, but not by a subtest named like /$caught/:"; grep -E -- "--- FAIL" <<<"$out" | head -5 | sed 's/^/          /'; fail=1
  else
    echo "  ok    $variant — rejected by: $(grep -E -- "--- FAIL: TestMeterEquivalenceBroken/.*${caught}" <<<"$out" | head -1 | sed -E 's/.*Broken\///; s/ \(.*//')"
  fi
}

echo "== the control: the real client passes (the suite is not just failing everything)"
if go test -tags=integration -count=1 -run '^TestMeterEquivalenceOverTheWire$' ./integration/ >/dev/null 2>&1; then echo "  ok    over the wire"; else echo "  FAIL  the real client does not pass"; fail=1; fi

echo "== each broken transport is rejected"
check flatten-errors  "conflict|invalid|unpriceable|stale|insufficient"
check default-time    "names_no_key|no_key,_no_time|never_given_a_default"
check drop-writeoff   "floored"
check retry           "idempotent|charged_once|prices_the_event"
check refusal-as-error "exhausted_balance|MaxCost|blocked|job_budget|ceiling"

[ "$fail" -eq 0 ] && echo "the equivalence suite refused what it should" || { echo "the equivalence suite has NO TEETH against some defect"; exit 1; }
