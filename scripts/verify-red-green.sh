#!/usr/bin/env bash
# Proves a change was DRIVEN by a test, not merely accompanied by one.
#
# For a branch that changes production code there must be a RED commit: it comes before the first
# production change, it touches only tests, and the tests it adds FAIL when it is checked out. The
# branch tip must then pass those same tests (GREEN). A test written after the code cannot satisfy
# this, because at the commit where it appears the code is already right and the test passes.
#
# A branch may hold several cycles (a stack of task pairs R### → T###), and EVERY production change after
# the first needs a `test(red):` commit of its own since the previous one — "a failing test-only commit
# ahead of every production change" (constitution, quality gates). Each such red is checked out and must
# fail, and the tip must pass them all. A `refactor:` commit changes no behaviour, so it needs no red —
# and it does not buy the next change one. A `test(mutation):` commit after green is test-after by
# definition and is never taken for a red.
#
#   scripts/verify-red-green.sh [base-ref]        # default origin/main; run from a branch
#
# Commit shape (docs/testing.md):
#   test(red): <the invariant>        tests only; fails, for the reason the message states
#   feat(green): / fix(green): …      the smallest production change that makes it pass
#   refactor: …                       optional; the tests are green before and after
#
# A change that genuinely cannot be test-driven (a rename, a dependency bump, a formatting pass, a doc
# comment) says so: a line `TDD-Exempt: <reason>` in any commit message or in the PR description
# (PR_BODY). The reason is required and is reviewed like any other line.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

base="${1:-origin/main}"
git rev-parse --verify --quiet "$base" >/dev/null || { echo "verify-red-green: unknown base ref '$base'"; exit 2; }

# Test support code counts as tests: conformance suites, fakes, fixtures and harnesses are not what ships.
is_test_path() {
  case "$1" in
    *_test.go | */testdata/* | e2e/* | */contracts/* | */pricingtest/* | */redistest/* | */natstest/* | internal/pgtest/* | internal/chaos/*) return 0 ;;
  esac
  return 1
}
# Production code: Go that ships, SQL migrations, Lua functions. A doc.go is prose, not behaviour.
is_production_path() {
  is_test_path "$1" && return 1
  case "$1" in
    */doc.go | doc.go) return 1 ;;
    *.go | migrations/*.sql | *.lua) return 0 ;;
  esac
  return 1
}

commits=$(git rev-list --reverse --no-merges "$base"..HEAD)
[ -n "$commits" ] || { echo "verify-red-green: no commits beyond $base"; exit 0; }

first_prod=""
declare -a red_candidates=() unredded=()
have_red=0   # a red has been committed since the last production change that needed one
prev_prod="" # the last such production change
i=0
for c in $commits; do
  prod=0; tests=0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    if is_production_path "$f"; then prod=1; elif is_test_path "$f"; then tests=1; fi
  done < <(git diff-tree --no-commit-id --name-only -r "$c")
  subject=$(git log -1 --format=%s "$c")
  if [ "$prod" -eq 1 ]; then
    if [ -z "$first_prod" ]; then
      first_prod="$i:$c"
    elif [[ ! "$subject" =~ ^refactor ]] && [ "$have_red" -eq 0 ]; then
      unredded+=("$c|$prev_prod")
    fi
    if [[ ! "$subject" =~ ^refactor ]] || [ "$prev_prod" = "" ]; then have_red=0; prev_prod="$c"; fi
  elif [ "$tests" -eq 1 ]; then
    # Before the first production change any tests-only commit is a candidate, as it always was; after
    # it, only a commit that calls itself a red — `test(mutation):` and the like are test-after.
    if [ -z "$first_prod" ] || [[ "$subject" =~ ^test\(red\) ]]; then red_candidates+=("$c"); have_red=1; fi
  fi
  i=$((i + 1))
done

if [ -z "$first_prod" ]; then
  echo "verify-red-green: no production code changed since $base — nothing to drive"
  exit 0
fi

# An explicit, reasoned exemption.
exempt=$( { git log --format=%B "$base"..HEAD; printf '%s\n' "${PR_BODY:-}"; } | grep -iE '^TDD-Exempt:' | head -1 || true)
if [ -n "$exempt" ]; then
  reason=$(echo "$exempt" | sed -E 's/^[^:]*:[[:space:]]*//')
  if [ -z "$reason" ]; then echo "FAIL: 'TDD-Exempt:' needs a reason"; exit 1; fi
  echo "verify-red-green: exempt — $reason"
  exit 0
fi

if [ "${#red_candidates[@]}" -eq 0 ]; then
  echo "FAIL: production code changed (first in $(git log -1 --format='%h %s' "${first_prod#*:}")) with no test-only commit before it."
  echo "      Write the failing test first and commit it alone as 'test(red): …', then the change."
  echo "      If this change cannot be test-driven, say why with a 'TDD-Exempt: <reason>' line."
  exit 1
fi

if [ "${#unredded[@]}" -gt 0 ]; then
  for u in "${unredded[@]}"; do
    echo "FAIL: production code changed in $(git log -1 --format='%h %s' "${u%%|*}") with no test(red) commit since the previous"
    echo "      production change ($(git log -1 --format='%h %s' "${u##*|}"))."
  done
  echo "      A branch that holds several changes needs a red for each: commit the failing test alone as"
  echo "      'test(red): …' before the change. A restructuring that changes no behaviour is 'refactor: …'."
  echo "      If a change cannot be test-driven, say why with a 'TDD-Exempt: <reason>' line."
  exit 1
fi

tmp=$(mktemp -d)
trap 'git worktree prune; rm -rf "$tmp"' EXIT

# Tests added by a commit, as a -run pattern; empty when it only edited existing tests.
added_tests() { git diff-tree -p --no-commit-id "$1" | sed -nE 's/^\+func ((Test|Fuzz)[A-Za-z0-9_]*)\(.*/\1/p' | paste -sd'|' -; }
test_packages() {
  git diff-tree --no-commit-id --name-only -r "$1" | grep -E '_test\.go$' | xargs -n1 dirname 2>/dev/null | sort -u | sed 's|^|./|' | tr '\n' ' '
}
needs_integration_tag() {
  git diff-tree --no-commit-id --name-only -r "$1" | grep -E '_test\.go$' | while read -r f; do
    git show "$1:$f" | head -5 | grep -q '^//go:build.*integration' && echo yes && break
  done
}

status=0
verified_names=""; verified_pkgs=""; verified_tags=()  # the tags are the union over every red: one that needs
                                                           # a tag is built with it at the tip, whatever tier the last red was in
for c in "${red_candidates[@]}"; do
  short=$(git log -1 --format='%h %s' "$c")
  names=$(added_tests "$c"); pkgs=$(test_packages "$c")
  [ -n "$pkgs" ] || pkgs="./..."
  read -ra pkg_args <<<"$pkgs"
  tag_args=(); [ -z "$(needs_integration_tag "$c")" ] || tag_args=(-tags=integration)
  run=(); [ -z "$names" ] || run=(-run "^($names)\$")

  git worktree add --quiet --detach "$tmp/wt" "$c"
  out="$tmp/red.out"
  set +e
  (cd "$tmp/wt" && go test -count=1 "${tag_args[@]+"${tag_args[@]}"}" "${run[@]+"${run[@]}"}" "${pkg_args[@]}" >"$out" 2>&1)
  rc=$?
  set -e
  git worktree remove --force "$tmp/wt"

  if [ "$rc" -eq 0 ]; then
    echo "FAIL: $short"
    echo "      this test-only commit PASSES — the code it tests already behaves this way, so the test"
    echo "      was not driving anything. A red test fails first, for the reason the commit states."
    status=1
  else
    kind="an assertion failed"
    if grep -qE 'build failed|undefined:|cannot use|declared and not used' "$out"; then
      kind="the build failed (stub the new symbol so the test fails on its assertion, not on a compile error)"
    fi
    echo "RED   $short — $kind"
    grep -E '^(--- FAIL|FAIL|\s+[a-z_]+_test\.go:[0-9]+:)' "$out" | head -4 | sed 's/^/        /'
    verified_names="${verified_names:+$verified_names|}$names"
    verified_pkgs="$verified_pkgs $pkgs"; [ "${#tag_args[@]}" -eq 0 ] || verified_tags=("${tag_args[@]}")
  fi
done
[ "$status" -eq 0 ] || exit 1

# GREEN: at the tip, the tests the red commits added pass.
run=(); [ -z "$verified_names" ] || run=(-run "^($verified_names)\$")
read -ra green_pkgs <<<"$(echo "$verified_pkgs" | tr ' ' '\n' | sort -u | tr '\n' ' ')"
if go test -count=1 "${verified_tags[@]+"${verified_tags[@]}"}" "${run[@]+"${run[@]}"}" "${green_pkgs[@]}" >"$tmp/green.out" 2>&1; then
  echo "GREEN tip $(git rev-parse --short HEAD) — the tests the red commit added now pass"
else
  echo "FAIL: the branch tip does not pass the tests its red commit added:"; tail -15 "$tmp/green.out" | sed 's/^/    /'
  exit 1
fi
echo "verify-red-green: red, then green"
