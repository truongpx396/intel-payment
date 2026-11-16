#!/usr/bin/env bash
# Proves the credits-ui boundary guards FAIL when the boundary is crossed (credits-ui-ports.md, invariant 6 and the
# adoption checklist). A guard nobody has seen fire is a guard nobody knows is wired: this plants one violation per rule,
# expects the tool to reject it FOR THAT RULE, and always removes it again. Needs `npm ci` in ui/.
#
# Two ways a guard test lies, and what this does about each:
#   * an absent or broken guard "rejects" everything — so the clean tree must pass first, and a rejection only counts when
#     the output names the rule (ESLint) or the planted file and its reason (the scan);
#   * a carve-out that is too wide accepts what it should not — so the scan is also shown ACCEPTING exactly the names the
#     contract defines, and nothing next to them.
#
#   scripts/verify-ui-boundary.sh
set -uo pipefail
cd "$(dirname "$0")/.."

pkg=ui/credits-ui/src
ex=ui/examples/llm-credits/src
eslint=node_modules/.bin/eslint
planted=()
cleanup() { for f in "${planted[@]:-}"; do [ -n "$f" ] && rm -f "$f"; done; }
trap cleanup EXIT

fail=0
plant() { mkdir -p "$(dirname "$1")"; printf '%s\n' "$2" > "$1"; planted+=("$1"); }

# lint_rejects <label> <rule id> <file> — ESLint must reject the file, and say it is THIS rule
lint_rejects() {
  local label=$1 rule=$2 file=$3 out rc
  out=$(cd ui && "$eslint" "${file#ui/}" 2>&1); rc=$?
  if [ "$rc" -eq 0 ]; then echo "  FAIL  $label — the violation was ACCEPTED"; fail=1
  elif ! grep -q -- "$rule" <<<"$out"; then echo "  FAIL  $label — rejected, but not by $rule:"; sed 's/^/          /' <<<"$out" | head -6; fail=1
  else echo "  ok    $label"; fi
}

# scan_rejects <label> <file> <what the report must say>
scan_rejects() {
  local label=$1 file=$2 want=$3 out rc
  out=$(scripts/check-credits-ui.sh "$file" 2>&1); rc=$?
  if [ "$rc" -eq 0 ]; then echo "  FAIL  $label — the violation was ACCEPTED"; fail=1
  elif ! grep -q -- "$file" <<<"$out" || ! grep -qi -- "$want" <<<"$out"; then
    echo "  FAIL  $label — rejected, but not for '$want' in $file:"; sed 's/^/          /' <<<"$out" | head -6; fail=1
  else echo "  ok    $label"; fi
}

scan_accepts() {
  local label=$1 file=$2 out
  if out=$(scripts/check-credits-ui.sh "$file" 2>&1); then echo "  ok    $label"
  else echo "  FAIL  $label — refused what the contract itself names:"; sed 's/^/          /' <<<"$out" | head -6; fail=1; fi
}

echo "== the clean tree passes"
(cd ui && "$eslint" . >/dev/null 2>&1) && echo "  ok    eslint (no-restricted-paths and the framework-free rules)" || { echo "  FAIL  eslint on the clean tree"; fail=1; }
scripts/check-credits-ui.sh >/dev/null 2>&1 && echo "  ok    the vocabulary and token scan" || { echo "  FAIL  the vocabulary and token scan on the clean tree"; fail=1; }

echo "== ESLint rejects each crossing"
# The import must RESOLVE or import-x says nothing, so the target is planted too.
plant "$ex/zz_target.ts" "export const zzLabels = { unit: 'tokens' };"
plant "$pkg/zz_violation.ts" "import { zzLabels } from '../../examples/llm-credits/src/zz_target.js';
export const zzUnit = zzLabels.unit;"
lint_rejects "the package imports the reference binding (credits-ui -> examples)" "import-x/no-restricted-paths" "$pkg/zz_violation.ts"

plant "$pkg/zz_internal.ts" "export const zzInternal = 1;"
plant "$ex/zz_violation.ts" "import { zzInternal } from '../../../credits-ui/src/zz_internal.js';
export const zzValue = zzInternal;"
lint_rejects "the binding reaches into the package's source (examples -> credits-ui/src)" "import-x/no-restricted-paths" "$ex/zz_violation.ts"

plant "$pkg/zz_react.ts" "import { useState } from 'react';
export const zzHook = useState;"
lint_rejects "a .ts file imports React (ports and the model are framework-free)" "no-restricted-imports" "$pkg/zz_react.ts"

plant "$pkg/zz_fetch.ts" "export const zzLoad = () => fetch('/credits');"
lint_rejects "a .ts file reaches for fetch (the model has no transport)" "no-restricted-globals" "$pkg/zz_fetch.ts"

echo "== the scan rejects each crossing"
plant "$pkg/zz_unit.ts" "export const zzDefaultUnit = 'credits';"
scan_rejects "a unit baked into a string" "$pkg/zz_unit.ts" "credit"
plant "$pkg/zz_copy.tsx" "export const ZzHeading = () => <h2>Credit balance</h2>;"
scan_rejects "a unit baked into copy (capitalised, in JSX)" "$pkg/zz_copy.tsx" "credit"
plant "$pkg/zz_ident.ts" "export const zzCreditsRemaining = 0;"
scan_rejects "an identifier that merely contains the word" "$pkg/zz_ident.ts" "credit"
plant "$pkg/zz_hex.css" ".zz { color: #1a2b3c; }"
scan_rejects "a hex colour (tokens only)" "$pkg/zz_hex.css" "hex"
plant "$pkg/zz_fn.css" ".zz { background: rgb(10 20 30); }"
scan_rejects "a colour function (tokens only)" "$pkg/zz_fn.css" "colour function"
plant "$pkg/zz_palette.tsx" "export const ZzBox = () => <div className=\"bg-blue-500\" />;"
scan_rejects "a palette utility (tokens only)" "$pkg/zz_palette.tsx" "palette"
plant "$pkg/zz_radius.css" ".zz { border-radius: 4px; }"
scan_rejects "a literal radius (tokens only)" "$pkg/zz_radius.css" "radius"

echo "== the scan accepts exactly what the contract names"
plant "$pkg/zz_names.ts" "export interface CreditsPanelProps { readonly zz: number }
export type CreditsSource = { load(): void };
export type CreditsSnapshot = { readonly zz: number };
export const creditAllotment = 0;
// published as @intel-payment/credits-ui
export const zzTokens = 'var(--cu-status-ok)';"
scan_accepts "the identifiers credits-ui-ports.md defines, and the package's own name" "$pkg/zz_names.ts"
plant "$pkg/zz_token.css" ".zz { border-radius: var(--cu-radius); color: var(--cu-text); background: color-mix(in srgb, var(--cu-surface) 80%, transparent); }"
scan_accepts "tokens, and a colour derived from tokens" "$pkg/zz_token.css"

cleanup
planted=()
if git status --porcelain -- ui | grep -q 'zz_'; then echo "  FAIL  a planted file was left behind"; fail=1; fi

[ "$fail" -eq 0 ] && echo "ui boundary verified" || { echo "ui boundary NOT verified"; exit 1; }
