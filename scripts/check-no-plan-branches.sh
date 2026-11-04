#!/usr/bin/env bash
# Fails on a branch on a plan's identity in production Go: `if plan == "pro"`, `switch planCode { case "team": … }`.
#
# Entitlements are DATA that travels with the plan, checked through one port (entitlement invariant 3): a new plan is rows, not a
# release. A plan-code branch is how a host ends up with the rule scattered across its codebase, the UI's idea of a plan drifting
# from the server's, and launching a plan meaning a code change in every one of them. It is checked in this repository so the
# engine never models the habit it exists to replace; a host should run the same scan over its own tree.
#
# What it looks for, in Go that ships (tests, conformance suites, fixtures and generated code are exempt — they NAME plans):
#   1. a plan identifier (plan, planCode, planID, planSlug, plan_code, or any .Code/.ID/.Slug of one) compared with == or !=
#      to a non-empty string literal, on either side;
#   2. a `switch` on such an identifier with a `case "literal"`;
#   3. a string-matching helper (Contains, EqualFold, HasPrefix, HasSuffix, Index) given such an identifier and a literal.
# Comparing a plan's code with the EMPTY string is not a branch on which plan it is — it asks whether there is one.
#
# A line that genuinely must (say, a migration of old rows) carries `// plan-branch-ok: <reason>`; the reason is required.
#
#   scripts/check-no-plan-branches.sh            # every tracked and untracked-but-not-ignored Go file
#   scripts/check-no-plan-branches.sh FILE…      # only these (the boundary gate plants a violation this way)
set -euo pipefail
cd "$(dirname "$0")/.."

# The same paths the red-green gate calls test support, plus generated code and the documents' examples.
exempt='(_test\.go$|/testdata/|^e2e/|/contracts/|/pricingtest/|/redistest/|/natstest/|^internal/pgtest/|^internal/chaos/|^integration/|^api/[a-z]+v[0-9]+/.*\.pb\.go$|\.pb\.go$)'

if [ "$#" -gt 0 ]; then files=("$@"); else
  files=(); while IFS= read -r f; do files+=("$f"); done < <(git ls-files -co --exclude-standard -- '*.go')
fi

# A plan's identity: the identifier ends in plan / planCode / planID / planSlug / plan_code, or reaches .Code/.ID/.Slug of a plan.
# PlanChangePolicy, planKind and the like are about a plan, not which plan it is: they do not end in one of these.
ident='([A-Za-z_][A-Za-z0-9_]*\.)*([Pp]lan([Cc]ode|[Ss]lug|[Ii][Dd])?|[Pp]lan_code)(\.(Code|Slug|ID|Id))?(\(\))?'
lit='"[^"]+"'
cmp="($ident)[[:space:]]*[!=]=[[:space:]]*$lit|$lit[[:space:]]*[!=]=[[:space:]]*($ident)"
helper="(Contains|EqualFold|HasPrefix|HasSuffix|Index)\\([^)]*(($ident)[^)]*$lit|$lit[^)]*($ident))"

# The patterns reach awk through the environment: `awk -v` would eat their backslashes.
export CMP="$cmp" HELPER="$helper"
export SWPAT="^[[:space:]]*switch[[:space:]]+([^{]*[^A-Za-z0-9_.])?$ident([^A-Za-z0-9_(]|\\(\\)|\$)"

fail=0
for f in "${files[@]}"; do
  [[ "$f" =~ $exempt ]] && continue
  [ -f "$f" ] || continue
  hits=$(awk '
    function ok(line) { return line ~ /plan-branch-ok:[[:space:]]*[^[:space:]]/ }
    BEGIN { cmp = ENVIRON["CMP"]; helper = ENVIRON["HELPER"]; swpat = ENVIRON["SWPAT"] }
    {
      line = $0
      if (insw && line ~ ("^" swindent "}")) insw = 0
      if (!ok(line)) {
        if (line ~ cmp || line ~ helper) { printf "%s:%d: a branch on a plan: %s\n", FILENAME, FNR, line }
        else if (insw && line ~ /^[[:space:]]*case[[:space:]].*"[^"]+"/) { printf "%s:%d: a case on a plan in the switch at line %d: %s\n", FILENAME, FNR, swline, line }
      }
      if (line ~ swpat) { insw = 1; swline = FNR; match(line, /^[[:space:]]*/); swindent = substr(line, 1, RLENGTH) }
    }' "$f") || true
  if [ -n "$hits" ]; then fail=1; printf '%s\n' "$hits"; fi
done

if [ "$fail" -ne 0 ]; then
  cat <<'MSG'

FAIL: production Go branches on a plan's identity (entitlement invariant 3).
      Give the plan the entitlement as DATA — a row in plan_entitlements for the capability — and ask the Entitler
      (Allowed / Quota / Choice / Consume). A new plan is rows, not a release.
      A line that truly must says `// plan-branch-ok: <reason>`.
MSG
  exit 1
fi
echo "no production Go branches on a plan's identity"
