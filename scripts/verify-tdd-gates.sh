#!/usr/bin/env bash
# The TDD gates must FAIL when crossed. A gate that has never been seen to refuse anything is a hope,
# and these two judge every pull request, so each is exercised against the cases it exists for, in a
# scratch clone (the working tree is never touched) — the same idea as scripts/verify-boundary.sh.
#
#   red-green   a proper red→green branch passes; production code without a failing test-only commit
#               before it is refused; so are a "red" commit that already passes, test and code squeezed
#               into one commit, and a tip that never turns green; an exemption needs a reason;
#               a build failure counts as red but says so
#   mutation    a planted survivor (the boundary tests of the pricer deleted) is refused, by name
#
#   scripts/verify-tdd-gates.sh [--no-mutation]     # mutation needs gremlins and ~30 s
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
with_mutation=1
[ "${1:-}" = "--no-mutation" ] && with_mutation=0

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export GIT_AUTHOR_NAME=gate GIT_AUTHOR_EMAIL=gate@example.invalid GIT_COMMITTER_NAME=gate GIT_COMMITTER_EMAIL=gate@example.invalid

clone() { # <dir> — a scratch repo holding the working tree as it is now (committed or not) as `base`
  mkdir -p "$1"
  (cd "$root" && git ls-files -z -co --exclude-standard | tar --null -T - -cf - 2>/dev/null || true) | tar -xf - -C "$1"
  git -C "$1" init --quiet -b base
  git -C "$1" add -A
  git -C "$1" commit --quiet -m "base snapshot"
}

failures=0
expect() { # <name> <want-exit> <pattern-in-output> <command…>
  local name="$1" want="$2" pattern="$3"; shift 3
  local out rc=0
  out=$("$@" 2>&1) || rc=$?
  if [ "$rc" -ne "$want" ]; then
    echo "NOT OK  $name — exit $rc, wanted $want"; echo "$out" | sed 's/^/        /' | tail -12; failures=$((failures + 1)); return
  fi
  if ! echo "$out" | grep -qE "$pattern"; then
    echo "NOT OK  $name — output lacks /$pattern/"; echo "$out" | sed 's/^/        /' | tail -12; failures=$((failures + 1)); return
  fi
  echo "ok      $name"
}

# ---------------------------------------------------------------- red-green
repo="$tmp/repo"; clone "$repo"; cd "$repo"
vrg="$root/scripts/verify-red-green.sh"
branch() { git switch --quiet -f -C "$1" base; }
testfile=metering/domain/scratch_gate_test.go
write_test() { # <expected MaxOperationAmount expression>
  cat >"$testfile" <<GO
package domain_test

import (
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

func TestScratchGate(t *testing.T) {
	t.Parallel()
	if domain.MaxOperationAmount != $1 {
		t.Fatalf("got %d", domain.MaxOperationAmount)
	}
}
GO
}
bump() { perl -pi -e 's/MaxOperationAmount Credits = 1 << 50/MaxOperationAmount Credits = 1 << 51/' metering/domain/credits.go; }

branch a; write_test '1 << 51'; git add "$testfile"; git commit -qm "test(red): the ceiling is 2^51"; bump; git commit -qam "fix(green): raise the ceiling"
expect "a failing test-only commit, then the change, passes" 0 "red, then green" "$vrg" base

branch b; bump; git commit -qam "raise the ceiling"
expect "production code with no test-only commit is refused" 1 "no test-only commit" "$vrg" base

branch c; bump; git commit -qam "raise the ceiling" -m "TDD-Exempt: a constant pinned by another suite"
expect "an exemption with a reason passes, and says why" 0 "exempt — a constant pinned" "$vrg" base

branch d; bump; git commit -qam "raise the ceiling" -m "TDD-Exempt:"
expect "an exemption without a reason is refused" 1 "needs a reason" "$vrg" base

branch e; write_test '1 << 50'; git add "$testfile"; git commit -qm "test: the ceiling is 2^50"; bump; git commit -qam "raise the ceiling"
expect "a 'red' commit that already passes is refused" 1 "PASSES" "$vrg" base

branch f; write_test '1 << 51'; bump; git add "$testfile"; git commit -qam "raise the ceiling, with its test"
expect "test and code in one commit is refused" 1 "no test-only commit" "$vrg" base

branch g; echo "a note" >>README.md; git commit -qam "docs"
expect "a docs-only change has nothing to drive" 0 "nothing to drive" "$vrg" base

branch h
cat >"$testfile" <<'GO'
package domain_test

import (
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

func TestScratchGateNew(t *testing.T) {
	t.Parallel()
	if domain.ScratchGateCeiling() != 7 {
		t.Fatal("no")
	}
}
GO
git add "$testfile"; git commit -qm "test(red): a ceiling function"
printf 'package domain\n\n// ScratchGateCeiling is test scaffolding.\nfunc ScratchGateCeiling() int { return 7 }\n' >metering/domain/scratch_gate.go
git add metering/domain/scratch_gate.go; git commit -qm "feat(green): the ceiling function"
expect "red by build failure passes, and says to stub the symbol" 0 "build failed" "$vrg" base

branch i; write_test '1 << 51'; git add "$testfile"; git commit -qm "test(red): the ceiling is 2^51"; echo "// tweak" >>metering/domain/credits.go; git commit -qam "fix(green): forgot the change"
expect "a red commit whose tip never turns green is refused" 1 "does not pass the tests" "$vrg" base

# ---------------------------------------------------------------- mutation
if [ "$with_mutation" -eq 1 ]; then
  cd "$root"
  mrepo="$tmp/mutrepo"; clone "$mrepo"; cd "$mrepo"
  pricer=./metering/adapters/driven/pricing/table
  guard='metering/adapters/driven/pricing/table/table.go'

  expect "the pricer's gate passes with its boundary tests" 0 "mutation gate passed" scripts/mutation.sh "$pricer"

  git switch --quiet -c no-tests
  git rm --quiet metering/adapters/driven/pricing/table/fuzz_test.go
  expect "a planted survivor is refused, by name" 1 "LIVED .*table\.go" scripts/mutation.sh "$pricer"

  # The pull-request form judges only the lines a branch committed. The survivor is still in the
  # package, but a branch that did not touch that line is not answerable for it; one that did, is.
  git commit --quiet -m "drop the pricer's boundary tests"
  expect "diff mode leaves alone a package the branch did not touch" 0 "no changed lines since base" scripts/mutation.sh --diff base "$pricer"

  perl -pi -e 's/^(\t\tif q\.Amount < 0 \{)$/$1 \/\/ a negative count is refused/' "$guard"
  git commit --quiet -am "touch the guarded line"
  expect "diff mode holds a branch to the lines it touched" 1 "LIVED .*table\.go:36" scripts/mutation.sh --diff base "$pricer"
fi

echo
if [ "$failures" -eq 0 ]; then echo "every TDD gate refused what it should and passed what it should"; else echo "$failures gate check(s) failed"; fi
exit "$failures"
