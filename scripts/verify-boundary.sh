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
expect_reject "arch: billing's app reaches metering through its ports (billing/app -> metering/adapters/driven/redis)" arch \
  billing/app/zz_violation.go billing/app \
  "package app
import _ \"$mod/metering/adapters/driven/redis\""
expect_reject "arch: billing ports never reach the app (billing/ports -> billing/app)" arch \
  billing/ports/zz_violation.go billing/ports \
  "package ports
import _ \"$mod/billing/app\""
expect_reject "depguard 7: no infra in the fiat core (billing/app -> go-redis)" lint \
  billing/app/zz_violation.go billing/app \
  "package app
import _ \"github.com/redis/go-redis/v9\""

expect_reject "arch: a billing adapter does not reach the app (billing/adapters/driven/postgres -> billing/app)" arch \
  billing/adapters/driven/postgres/zz_violation.go billing/adapters/driven/postgres \
  "package postgres
import _ \"$mod/billing/app\""
expect_reject "arch: a billing adapter reaches metering through its ports (postgres -> metering/adapters/driven/redis)" arch \
  billing/adapters/driven/postgres/zz_violation.go billing/adapters/driven/postgres \
  "package postgres
import _ \"$mod/metering/adapters/driven/redis\""

expect_reject "depguard 9: a provider adapter imports no other (stripe -> postgres)" lint \
  billing/adapters/driven/stripe/zz_violation.go billing/adapters/driven/stripe \
  "package stripe
import \"$mod/billing/adapters/driven/postgres\"
var _ = postgres.NewInbox"
expect_reject "depguard 9: a provider adapter imports no other (polar -> paypal)" lint \
  billing/adapters/driven/polar/zz_violation.go billing/adapters/driven/polar \
  "package polar
import \"$mod/billing/adapters/driven/paypal\"
var _ = paypal.New"
expect_reject "depguard 9: a provider adapter imports no other (paypal -> polar)" lint \
  billing/adapters/driven/paypal/zz_violation.go billing/adapters/driven/paypal \
  "package paypal
import \"$mod/billing/adapters/driven/polar\"
var _ = polar.New"
expect_reject "depguard 9: an adapter implements ports, it does not call the use cases (polar -> billing/app)" lint \
  billing/adapters/driven/polar/zz_violation.go billing/adapters/driven/polar \
  "package polar
import \"$mod/billing/app\"
var _ = app.NewIngress"
expect_reject "depguard 9: an adapter implements ports, it does not call the use cases (paypal -> billing/app)" lint \
  billing/adapters/driven/paypal/zz_violation.go billing/adapters/driven/paypal \
  "package paypal
import \"$mod/billing/app\"
var _ = app.NewIngress"
expect_reject "arch: a billing adapter does not reach the app (paypal -> billing/app)" arch \
  billing/adapters/driven/paypal/zz_violation.go billing/adapters/driven/paypal \
  "package paypal
import _ \"$mod/billing/app\""
expect_reject "arch: the webhook door speaks for a port and calls no use case (billing/adapters/driving/webhookhttp -> billing/app)" arch \
  billing/adapters/driving/webhookhttp/zz_violation.go billing/adapters/driving/webhookhttp \
  "package webhookhttp
import _ \"$mod/billing/app\""
expect_reject "arch: the webhook door does not reach the books (billing/adapters/driving/webhookhttp -> billing/adapters/driven/postgres)" arch \
  billing/adapters/driving/webhookhttp/zz_violation.go billing/adapters/driving/webhookhttp \
  "package webhookhttp
import _ \"$mod/billing/adapters/driven/postgres\""
expect_reject "arch: the webhook door does not reach metering's transports (billing/adapters/driving/webhookhttp -> metering/adapters/driving/resthandler)" arch \
  billing/adapters/driving/webhookhttp/zz_violation.go billing/adapters/driving/webhookhttp \
  "package webhookhttp
import _ \"$mod/metering/adapters/driving/resthandler\""
expect_reject "depguard 9: the books know no provider (postgres -> stripe)" lint \
  billing/adapters/driven/postgres/zz_violation.go billing/adapters/driven/postgres \
  "package postgres
import \"$mod/billing/adapters/driven/stripe\"
var _ = stripe.New"

[ "$fail" -eq 0 ] && echo "boundary verified" || { echo "boundary NOT verified"; exit 1; }
