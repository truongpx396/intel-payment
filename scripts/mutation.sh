#!/usr/bin/env bash
# Mutation testing: change the code in a small way — flip a `<`, drop a `+` — and require a test to
# fail. A mutant no test notices is behaviour nothing pins down; line coverage cannot see it, because
# the line WAS executed. This is what turns "we wrote tests" into "the tests can fail".
#
#   scripts/mutation.sh                     # every target below (minutes — nightly, and before a release)
#   scripts/mutation.sh --diff origin/main  # judge only the lines changed since that ref — what a PR is held to
#   scripts/mutation.sh ./metering/domain   # one package
#
# Two numbers gate each target (thresholds live in TARGETS; raise them, never lower one to go green):
#   efficacy  killed ÷ (killed + lived)   of the mutants a test reaches, the share a test catches
#   mcover    (killed + lived) ÷ all      the share of mutants any test reaches at all
#
# A TIMED OUT mutant is NOT counted as killed. gremlins sizes the timeout from one cached coverage run,
# so a low `timeout-coefficient` turns nearly every mutant into a "timeout" and leaves a tiny, flattering
# denominator. If more than 5% time out the run FAILS: fix the timeout, do not read the score.
#
# Some survivors are EQUIVALENT mutants — `a > b` → `a >= b` where a == b cannot change the result — and
# no test can kill them. Each is listed, with a reason, in .mutation-equivalents (see report() below for
# how exactly an entry must match); listed mutants are set aside from the efficacy figure and counted.
# A survivor that is NOT equivalent is a missing test: write it first (docs/testing.md).
#
# Two different tolerances. A whole-package run keeps the percentage floor in TARGETS. A --diff run has
# none: every survivor on a changed line is killed or excused, however high the percentage around it.
#
# --diff does NOT use gremlins' own `--diff`: in v0.6.0 it names files relative to the package directory
# while git names them relative to the repository, so given a sub-package it matches nothing and every
# run would pass vacuously. Here a touched package is mutated whole and each mutant is then judged only
# if it sits on a line `git diff --merge-base <ref>` reports as changed (committed or not).
set -euo pipefail
cd "$(dirname "$0")/.."

GREMLINS="${GREMLINS:-$(command -v gremlins || echo "$HOME/go/bin/gremlins")}"
[ -x "$GREMLINS" ] || { echo "gremlins not found; install: go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0"; exit 2; }
command -v python3 >/dev/null || { echo "python3 is required to read gremlins' report"; exit 2; }

# package | min efficacy % | min mutant coverage %
TARGETS="
./metering/domain|98|81
./metering/adapters/driven/pricing/table|90|90
./metering/app|97|30
"
# metering/app's mutant coverage is low on purpose: booking, reconcile, recover and the writer are verified
# at the INTEGRATION tier (real Redis + Postgres), which a unit-tier mutation run cannot see. They are
# NOT measured yet — a mutation run over that tier is task T135 — and are not claimed here. Mutants on a `case`
# expression are also reported "not covered" whatever the tests do: Go's coverage profile has no block
# for a case's condition, so gremlins cannot tell they ran.

diff=""
if [ "${1:-}" = "--diff" ]; then diff="${2:?--diff needs a ref, e.g. origin/main}"; shift 2; fi

selected=()
if [ "$#" -gt 0 ]; then
  selected=("$@")
else
  while IFS='|' read -r pkg _ _; do [ -z "$pkg" ] || selected+=("$pkg"); done <<< "$TARGETS"
fi

column() { # <pkg> <2|3> — a package outside TARGETS is held to the strictest defaults
  local v
  v=$(echo "$TARGETS" | awk -F'|' -v p="$1" -v c="$2" '$1==p {print $c}')
  echo "${v:-90}"
}
num() { echo "$1" | sed -nE "s/.*$2: ([0-9]+).*/\1/p"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# "path:line" for every added or modified line of a non-test Go file since <ref>.
changed_lines() {
  git diff --merge-base "$1" -U0 --no-color -- '*.go' ':!*_test.go' | python3 -c '
import re, sys
path = None
for line in sys.stdin:
    if line.startswith("+++ "):
        name = line[4:].strip()
        path = name[2:] if name.startswith("b/") else None      # /dev/null: the file was deleted
    elif line.startswith("@@") and path:
        m = re.match(r"@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@", line)
        if m:
            start = int(m.group(1)); n = int(m.group(2)) if m.group(2) is not None else 1
            for i in range(start, start + n):
                print(f"{path}:{i}")
'
}

# Equivalent mutants (see the header): validated once, up front, so a malformed entry stops the run.
EQUIVALENTS="${EQUIVALENTS:-.mutation-equivalents}"
if [ -f "$EQUIVALENTS" ]; then
  python3 - "$EQUIVALENTS" <<'PY' || exit 2
import sys, tomllib
try:
    entries = tomllib.load(open(sys.argv[1], "rb")).get("equivalent", [])
except tomllib.TOMLDecodeError as e:
    sys.exit(f"mutation: {sys.argv[1]} is not valid TOML: {e}")
bad = []
for i, e in enumerate(entries, 1):
    label = f"entry {i} ({e.get('file', '?')} {e.get('mutator', '?')})"
    if not all(isinstance(e.get(k), str) and e[k].strip() for k in ("file", "mutator", "line")) or not isinstance(e.get("col"), int):
        bad.append(f"{label} needs file, mutator, col (an integer) and line")
    elif not str(e.get("why", "")).strip():
        bad.append(f"{label} needs a reason: say why no test could tell the mutant from the code")
if bad:
    sys.exit("mutation: " + sys.argv[1] + "\n    " + "\n    ".join(bad))
PY
  export EQUIVALENTS
fi

# <gremlins json> <package dir> [<changed-lines file>] → "killed lived timedout uncovered excused", then
# the survivors. A survivor is EXCUSED only by an entry that names its file, operator, column (counted from
# the line's first non-blank character) and the exact text of its line: edit the line and the excuse lapses,
# and an excuse for one operator never covers another's on the same line. Without a scope (a whole-package
# run) an entry that matches nothing is reported as STALE — the code moved on and the excuse did not.
report() {
  python3 - "$@" <<'PY'
import json, os, sys, tomllib
js, pkgdir = sys.argv[1], sys.argv[2].removeprefix("./").rstrip("/")
scope = None
if len(sys.argv) > 3:
    scope = {l.strip() for l in open(sys.argv[3]) if l.strip()}
equiv = {}
ef = os.environ.get("EQUIVALENTS")
if ef and os.path.exists(ef):
    for e in tomllib.load(open(ef, "rb")).get("equivalent", []):
        equiv[(e["file"], e["mutator"], e["col"], e["line"].strip())] = False
counts = {"KILLED": 0, "LIVED": 0, "TIMED OUT": 0, "NOT COVERED": 0}
excused = 0
survivors = []
for f in json.load(open(js))["files"]:
    path = f"{pkgdir}/{f['file_name']}"
    src = None
    for m in f["mutations"]:
        if scope is not None and f"{path}:{m['line']}" not in scope:
            continue
        if m["status"] == "LIVED":
            if src is None:
                src = open(path).read().split("\n")
            text = src[m["line"] - 1]
            key = (path, m["type"], m["column"] - (len(text) - len(text.lstrip())), text.strip())
            if key in equiv:
                equiv[key] = True
                excused += 1
                continue
            survivors.append(f"LIVED {m['type']} at {path}:{m['line']}:{m['column']}")
        if m["status"] in counts:
            counts[m["status"]] += 1
print(counts["KILLED"], counts["LIVED"], counts["TIMED OUT"], counts["NOT COVERED"], excused)
for s in sorted(survivors):
    print(s)
if scope is None:
    for (path, kind, col, _), used in sorted(equiv.items()):
        if not used and path.startswith(pkgdir + "/"):
            print(f"STALE {kind} at {path} col {col}: no surviving mutant matches this excuse")
PY
}

if [ -n "$diff" ]; then
  git rev-parse --verify --quiet "$diff" >/dev/null || { echo "mutation: unknown ref '$diff'"; exit 2; }
  changed_lines "$diff" >"$tmp/changed.txt"
fi

fail=0
for pkg in "${selected[@]}"; do
  dir="${pkg#./}"
  if [ -n "$diff" ] && ! grep -q "^${dir}/" "$tmp/changed.txt"; then
    echo "==> $pkg — no changed lines since $diff"; continue
  fi

  json="$tmp/$(echo "$dir" | tr '/' '_').json"; out="$tmp/run.out"
  echo "==> mutating $pkg${diff:+ (judging the lines changed since $diff)}"
  "$GREMLINS" unleash "$pkg" -o "$json" >"$out" 2>&1 || true
  line1=$(grep -E '^Killed:' "$out" || true); line2=$(grep -E '^Timed out:' "$out" || true)
  if [ -z "$line1" ] || [ -z "$line2" ] || [ ! -s "$json" ]; then
    echo "FAIL: $pkg — gremlins did not finish:"; tail -15 "$out" | sed 's/^/    /'; fail=1; continue
  fi

  # The timeout check is on the whole run, whatever the scope: it is about the tool, not the change.
  run_timed=$(num "$line2" 'Timed out')
  run_considered=$(( $(num "$line1" Killed) + $(num "$line1" Lived) + run_timed ))
  if [ "$run_considered" -gt 0 ] && [ $((run_timed * 100)) -gt $((run_considered * 5)) ]; then
    echo "FAIL: $pkg — $run_timed of $run_considered mutants timed out; the timeout is mis-set (see .gremlins.yaml), so a score would mean nothing"
    fail=1; continue
  fi

  if [ -n "$diff" ]; then res=$(report "$json" "$pkg" "$tmp/changed.txt"); else res=$(report "$json" "$pkg"); fi
  read -r killed lived timed uncovered excused <<<"$(echo "$res" | head -1)"
  excuse_note=""; [ "$excused" -eq 0 ] || excuse_note=" · excused $excused as equivalent ($EQUIVALENTS)"
  echo "    killed $killed · lived $lived · timed out $timed · not covered $uncovered${excuse_note}${diff:+  (changed lines only)}"
  echo "$res" | grep -E '^STALE' | sed 's/^/    note: /' || true

  if [ $((killed + lived + excused)) -eq 0 ]; then
    if [ "$uncovered" -gt 0 ] && [ -n "$diff" ]; then
      echo "    note: $uncovered mutant(s) on changed lines are reached by no unit test here"
    fi
    echo "    no reachable mutants in scope"; continue
  fi

  # An excused mutant was reached by a test and is set aside from the efficacy figure: it is not a
  # behaviour a test could pin down, so it is neither a kill nor a survivor.
  total=$((killed + lived + timed + uncovered + excused))
  eff=$(awk -v k="$killed" -v l="$lived" 'BEGIN {printf "%.1f", (k+l == 0) ? 100 : 100*k/(k+l)}')
  mcov=$(awk -v k="$killed" -v l="$lived" -v e="$excused" -v t="$total" 'BEGIN {printf "%.1f", 100*(k+l+e)/t}')
  min_eff=$(column "$pkg" 2); min_mcov=$(column "$pkg" 3)
  # A pull request is held to every line it changed with no tolerance: each survivor there is killed by a
  # test or excused, with a reason, in $EQUIVALENTS. (Whole-package runs keep the percentage, which leaves
  # room for the equivalents nobody has listed yet.)
  if [ -n "$diff" ]; then min_eff=100; fi
  # A handful of changed lines is too few mutants for a reach percentage to mean anything.
  if [ -n "$diff" ] && [ "$total" -lt 5 ]; then min_mcov=0; fi
  echo "    efficacy ${eff}% (min ${min_eff}%) · mutant coverage ${mcov}% (min ${min_mcov}%)"

  if awk -v v="$eff" -v m="$min_eff" 'BEGIN {exit !(v < m)}'; then
    echo "FAIL: $pkg — survivors; each is behaviour no test pins down (equivalent mutants excepted):"
    echo "$res" | grep -E '^LIVED' | sed 's/^/    /'
    fail=1
  fi
  if awk -v v="$mcov" -v m="$min_mcov" 'BEGIN {exit !(v < m)}'; then
    echo "FAIL: $pkg — only ${mcov}% of mutants are reached by any test (min ${min_mcov}%)"
    fail=1
  fi
done

[ "$fail" -eq 0 ] && echo "mutation gate passed"
exit "$fail"
