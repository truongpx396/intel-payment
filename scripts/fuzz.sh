#!/usr/bin/env bash
# Runs every Go fuzz target for FUZZTIME (default 20s) each. `go test -fuzz` accepts ONE target in ONE
# package per invocation, so this discovers them all.
#
#   scripts/fuzz.sh                      # every target, 20s each
#   FUZZTIME=5m scripts/fuzz.sh          # a deeper nightly run
#   scripts/fuzz.sh FuzzWindowCounter    # only targets whose name matches
#
# A failing input is written to <package>/testdata/fuzz/<Target>/. Commit it with the fix: from then
# on it runs as an ordinary test on every `go test`, so the bug cannot return unseen.
set -euo pipefail
cd "$(dirname "$0")/.."

FUZZTIME="${FUZZTIME:-20s}"
filter="${1:-}"

pkgs=$(grep -rl --include='*_test.go' '^func Fuzz' . 2>/dev/null | xargs -n1 dirname | sort -u || true)
[ -n "$pkgs" ] || { echo "no fuzz targets found"; exit 1; }

n=0
fail=0
for pkg in $pkgs; do
  for target in $(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true); do
    [ -z "$filter" ] || [[ "$target" == *"$filter"* ]] || continue
    n=$((n + 1))
    echo "==> $pkg  $target  ($FUZZTIME)"
    go test -run='^$' -fuzz="^${target}\$" -fuzztime="$FUZZTIME" "$pkg" || fail=1
  done
done

echo "$n fuzz target(s) run"
[ "$n" -gt 0 ] || { echo "the filter matched nothing"; exit 1; }
exit "$fail"
