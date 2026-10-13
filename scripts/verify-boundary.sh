#!/usr/bin/env bash
# Proves the boundary gates FAIL when the boundary is crossed. A lint rule nobody has seen fire is a
# rule nobody knows is wired: this plants one violation per rule, expects the tool to reject it, and
# always removes it again. Needs go-arch-lint and golangci-lint on PATH.
#
#   scripts/verify-boundary.sh
set -uo pipefail
cd "$(dirname "$0")/.."

mod=github.com/truongpx396/intel-payment
planted=()
cleanup() { for f in "${planted[@]:-}"; do [ -n "$f" ] && rm -f "$f"; done; }
trap cleanup EXIT

fail=0
# expect_reject <label> <tool: arch|lint> <file> <pkg dir> <go source>
expect_reject() {
  local label=$1 tool=$2 file=$3 pkg=$4 src=$5
  printf '%s\n' "$src" > "$file"; planted+=("$file")
  if [ "$tool" = arch ]; then
    go-arch-lint check --project-path . >/dev/null 2>&1; rc=$?
  else
    golangci-lint run "./$pkg/..." >/dev/null 2>&1; rc=$?
  fi
  rm -f "$file"
  if [ "$rc" -eq 0 ]; then echo "  FAIL  $label — the violation was ACCEPTED"; fail=1
  else echo "  ok    $label"; fi
}

echo "== the clean tree passes"
go-arch-lint check --project-path . >/dev/null 2>&1 && echo "  ok    go-arch-lint" || { echo "  FAIL  go-arch-lint on the clean tree"; fail=1; }
golangci-lint run ./... >/dev/null 2>&1 && echo "  ok    golangci-lint" || { echo "  FAIL  golangci-lint on the clean tree"; fail=1; }

echo "== each rule rejects its violation"
expect_reject "arch: domain imports nothing (domain -> ports)" arch \
  metering/domain/zz_violation.go metering/domain \
  "package domain
import _ \"$mod/metering/ports\""
expect_reject "arch: metering never imports billing" arch \
  metering/app/zz_violation.go metering/app \
  "package app
import _ \"$mod/billing\""
expect_reject "arch: billing never reaches metering/app" arch \
  billing/zz_violation.go billing \
  "package billing
import _ \"$mod/metering/app\""
expect_reject "depguard 1: no net/http in the core" lint \
  metering/domain/zz_violation.go metering/domain \
  "package domain
import _ \"net/http\""
expect_reject "depguard 2: engine stands alone (metering -> events)" lint \
  metering/zz_violation.go metering \
  "package metering
import _ \"$mod/events\""
expect_reject "depguard 3: layers use ports only (entitlement -> metering/app)" lint \
  entitlement/zz_violation.go entitlement \
  "package entitlement
import _ \"$mod/metering/app\""

# A used import, not a blank one: golangci-lint reports one issue per line, so a blank import would be
# rejected by revive and say nothing about whether depguard fired.
expect_reject "depguard 5: api is a leaf (api -> metering/adapters/driving/auth)" lint \
  api/zz_violation.go api \
  "package api
import \"$mod/metering/adapters/driving/auth\"
var _ = auth.Record"

expect_reject "depguard 6: a transport does not import another (grpcclient -> grpcserver)" lint \
  metering/adapters/driving/grpcclient/zz_violation.go metering/adapters/driving/grpcclient \
  "package grpcclient
import \"$mod/metering/adapters/driving/grpcserver\"
var _ = grpcserver.New"

expect_reject "arch: billing domain imports nothing of billing (billing/domain -> billing/ports)" arch \
  billing/domain/zz_violation.go billing/domain \
  "package domain
import _ \"$mod/billing/ports\""
expect_reject "arch: billing ports reach metering through its ports (billing/ports -> metering/app)" arch \
  billing/ports/zz_violation.go billing/ports \
  "package ports
import _ \"$mod/metering/app\""
expect_reject "depguard 7: no infra in the fiat core (billing/ports -> pgx)" lint \
  billing/ports/zz_violation.go billing/ports \
  "package ports
import _ \"github.com/jackc/pgx/v5\""
expect_reject "depguard 8: the billing domain imports only metering's domain (billing/domain -> metering/ports)" lint \
  billing/domain/zz_violation.go billing/domain \
  "package domain
import _ \"$mod/metering/ports\""

[ "$fail" -eq 0 ] && echo "boundary verified" || { echo "boundary NOT verified"; exit 1; }
