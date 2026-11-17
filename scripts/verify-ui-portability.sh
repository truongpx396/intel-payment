#!/usr/bin/env bash
# SC-012: the credits UI package is portable. `npm pack` it, install the tarball into an UNRELATED app — a different product, a
# different unit, its own bundler — and build that app. It compiles with zero edits to the package, or the package has a coupling.
#
# What it proves, and what would make it fail:
#   * the tarball holds the built package and nothing else (no source, no tests, no reference binding);
#   * the built JavaScript imports only itself and React — a dependency the package forgot to declare fails here, not in a host;
#   * a strict consumer type-checks it WITHOUT skipLibCheck, so a declaration file that does not compile is caught;
#   * the stylesheet subpath resolves and is bundled;
#   * the app builds with a unit that is not "credits".
#
# Needs Node >= 22.12 and the network (npm installs React, TypeScript and Vite into the throwaway app), like `make verify-clients`.
#
#   scripts/verify-ui-portability.sh
set -euo pipefail
cd "$(dirname "$0")/.."

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
root=$PWD
fail() { echo "FAIL: $*" >&2; exit 1; }
# The versions the workspace is tested with, resolved from the repo root: the throwaway app must not float to whatever is newest.
dev() { local v; v=$(node -p "require('$root/ui/package.json').devDependencies['$1'] || ''") || true; [ -n "$v" ] || fail "no version for $1 in ui/package.json"; printf '%s' "$v"; }
react=$(dev react); reactdom=$(dev react-dom); ts=$(dev typescript); vite=$(dev vite); treact=$(dev @types/react); treactdom=$(dev @types/react-dom)

echo "== build and pack the package"
out=$(cd ui && npm run build -w @intel-payment/credits-ui 2>&1) || { echo "$out"; fail "the package does not build"; }
(cd ui && npm pack -w @intel-payment/credits-ui --pack-destination "$tmp" --silent >/dev/null)
tarball=$(ls "$tmp"/*.tgz) || fail "npm pack produced no tarball"
echo "  $(basename "$tarball")"

echo "== the tarball holds the built package and nothing else"
files=$(tar -tzf "$tarball")
for want in package/package.json package/dist/index.js package/dist/index.d.ts package/dist/theme.css; do
  grep -qx "$want" <<<"$files" || fail "the tarball is missing $want"
done
extra=$(grep -vE '^package/(package\.json|README(\.md)?|LICENSE(\.md)?|dist/)' <<<"$files" || true)
[ -z "$extra" ] || fail "the tarball ships more than dist/:
$extra"
echo "  ok"

echo "== the built JavaScript imports only itself and React"
mkdir -p "$tmp/unpacked" && tar -xzf "$tarball" -C "$tmp/unpacked"
foreign=$(grep -rhoE "(from|import) +['\"][^'\"./][^'\"]*['\"]" "$tmp/unpacked/package/dist" --include='*.js' | grep -vE "['\"](react|react/jsx-runtime)['\"]" || true)
[ -z "$foreign" ] || fail "the package imports something it does not declare:
$foreign"
echo "  ok"

echo "== install it into an unrelated app and build"
app="$tmp/app"
mkdir -p "$app/src"
cat > "$app/package.json" <<'JSON'
{ "name": "unrelated-app", "private": true, "type": "module", "scripts": { "build": "tsc --noEmit && vite build" } }
JSON
cat > "$app/tsconfig.json" <<'JSON'
{
  "compilerOptions": {
    "target": "ES2022", "lib": ["ES2022", "DOM"], "module": "ESNext", "moduleResolution": "Bundler",
    "jsx": "react-jsx", "strict": true, "noEmit": true, "skipLibCheck": false, "types": ["vite/client"]
  },
  "include": ["src"]
}
JSON
cat > "$app/index.html" <<'HTML'
<!doctype html><html><body><div id="root"></div><script type="module" src="/src/main.tsx"></script></body></html>
HTML
# A different product — seats, not credits — with its own source. Nothing here is the package's, and the package knows none of it.
cat > "$app/src/main.tsx" <<'TSX'
import "@intel-payment/credits-ui/theme.css";
import { CreditsPanel, derivePanel, formatWithUnit, type CreditsSnapshot, type CreditsSource, type Int, type Quantity } from "@intel-payment/credits-ui";
import { createRoot } from "react-dom/client";

const q = (n: number) => n as Quantity;
const labels = { unit: "seats", one: "seat" };
const snapshot: CreditsSnapshot = {
  scope: { kind: "team", id: "t1" },
  balance: { remaining: q(60), granted: q(100), primaryLimitKey: "seats" },
  limits: [{ key: "seats", label: "Seats in use", used: q(40), cap: q(100), window: "period", warnAtPct: 80 as Int }],
  breakdown: [],
  ledger: [],
};
const source: CreditsSource = { load: async () => snapshot };

// the pure half is usable without React, from the same entry point
console.log(derivePanel(snapshot).hero.remaining, formatWithUnit(q(1), labels));

createRoot(document.getElementById("root")!).render(<CreditsPanel source={source} scope={snapshot.scope} labels={labels} />);
TSX
(cd "$app" && npm install --no-audit --no-fund --silent \
  "$tarball" "react@$react" "react-dom@$reactdom" "typescript@$ts" "vite@$vite" "@types/react@$treact" "@types/react-dom@$treactdom")
echo "  installed: $(cd "$app" && npm ls --depth=0 2>/dev/null | grep -E 'react@|typescript@|vite@|credits-ui@' | sed 's/^[^a-z@]*//' | tr '\n' ' ')"
(cd "$app" && npm run build) || fail "the unrelated app did not build against the packed package"
grep -rqE 'cu-meter' "$app"/dist/assets/*.css || fail "the stylesheet subpath was not bundled"
echo "  ok"

echo "portability verified (SC-012): packed, installed into an unrelated app, built with zero edits"
