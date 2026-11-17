#!/usr/bin/env bash
# Fails when the credits-ui package names a unit or a colour it must be handed.
#
# The package renders balances over an OPAQUE unit (credits-ui-ports.md, invariant 6): "credits", "seats" and "GB-months" are
# the host's UnitLabels, never the package's words — a screen that says "credit" in its own copy lies about a product that
# meters seats. And its stylesheet reads design tokens only (adoption checklist): a hex colour, a colour function, a palette
# utility or a literal radius is a design decision the host did not get to make.
#
# 1. VOCABULARY — any token containing "credit" (any case) in ui/credits-ui/src/** is refused, except the identifiers the
#    contract itself defines and the package's own name. A token is a run of letters, digits, `_` and `-`, so a string, a
#    comment, JSX text, a CSS custom property and an identifier are all caught, and `loadCreditsSnapshot` is not
#    `CreditsSnapshot`.
# 2. TOKENS — no hex colour, no rgb()/hsl()/oklch()-style function, no palette utility class, no literal border radius.
#    color-mix() over var(--token) is fine: it is derived from what the host supplied.
#
# The names the contract defines (credits-ui-ports.md) are the only exemption, and scripts/verify-ui-boundary.sh shows the
# scan accepting exactly them and rejecting a neighbour.
#
#   scripts/check-credits-ui.sh            # every .ts, .tsx and .css under ui/credits-ui/src
#   scripts/check-credits-ui.sh FILE…      # only these (the boundary gate plants a violation this way)
set -euo pipefail
cd "$(dirname "$0")/.."

allowed=" CreditsPanel CreditsPanelProps CreditsSource CreditsSnapshot creditAllotment credits-ui "

if [ "$#" -gt 0 ]; then files=("$@"); else
  files=(); [ ! -d ui/credits-ui/src ] || while IFS= read -r f; do files+=("$f"); done < <(find ui/credits-ui/src -type f \( -name '*.ts' -o -name '*.tsx' -o -name '*.css' \) | sort)
fi

fail=0
report() { printf '%s:%s: %s\n' "$1" "$2" "$3"; fail=1; }

for f in "${files[@]+"${files[@]}"}"; do
  [ -f "$f" ] || continue

  while IFS=: read -r line tok; do
    [ -n "$tok" ] || continue
    case "$allowed" in *" $tok "*) continue ;; esac
    report "$f" "$line" "the package names a unit: \"$tok\" contains \"credit\" — the unit is the host's (UnitLabels), never the package's"
  done < <(grep -noiE '[A-Za-z0-9_-]*credit[A-Za-z0-9_-]*' "$f" || true)

  while IFS=: read -r line text; do
    report "$f" "$line" "a hex colour — the package reads design tokens only: $text"
  done < <(grep -nE '(^|[^A-Za-z0-9_&])#([0-9A-Fa-f]{8}|[0-9A-Fa-f]{6}|[0-9A-Fa-f]{3,4})([^A-Za-z0-9_-]|$)' "$f" || true)

  while IFS=: read -r line text; do
    report "$f" "$line" "a colour function — the package reads design tokens only: $text"
  done < <(grep -nE '(^|[^A-Za-z0-9_-])(rgb|rgba|hsl|hsla|hwb|lab|lch|oklab|oklch)\(' "$f" || true)

  while IFS=: read -r line text; do
    report "$f" "$line" "a palette utility — the package reads design tokens only: $text"
  done < <(grep -nE '(^|[^A-Za-z0-9_-])(bg|text|border|ring|fill|stroke|from|via|to|outline|divide|shadow|accent|caret|decoration)-(slate|gray|grey|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-[0-9]{2,3}([^A-Za-z0-9_-]|$)' "$f" || true)

  while IFS=: read -r line text; do
    report "$f" "$line" "a literal radius — the package reads design tokens only: $text"
  done < <(grep -nE "(border-radius|borderRadius)[\"']?[[:space:]]*:[[:space:]]*[\"']?-?[0-9.]" "$f" || true)
done

if [ "$fail" -ne 0 ]; then
  cat <<'MSG'

FAIL: credits-ui names a unit or a colour it must be handed (credits-ui-ports.md, invariant 6 and the adoption checklist).
      A label is UnitLabels' to supply, and a colour is a design token's: take it as a prop, or as var(--cu-…).
MSG
  exit 1
fi
echo "credits-ui names no unit and no colour of its own"
