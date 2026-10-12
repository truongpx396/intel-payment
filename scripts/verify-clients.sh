#!/usr/bin/env bash
# The REST clients are GENERATED from api/openapi/v1.yaml, so the check is that what is generated compiles and
# talks to a real handler: a TypeScript client (openapi-typescript + openapi-fetch, type-checked by tsc) and a
# Python client (openapi-python-client) each send one Admit that is allowed and one that is refused, and read
# the answers back through their generated types. A document the generators cannot make a working client from
# fails here, not in a customer's build.
#
#   make verify-clients                      # starts the handler itself (integration/clients)
#   BASE_URL=http://host:8080 TOKEN=… scripts/verify-clients.sh   # the same round trip against any server
#
# The server must answer POST /v1/admit for scope workspace/w1: resource "llm.chat" allowed (headroom 7),
# resource "spent" refused 402 payment_required naming the ceiling "scope_balance".
# Needs node >= 22 (npm), python >= 3.11 and network access to npm and PyPI; versions are pinned below.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${BASE_URL:?BASE_URL is the server the clients talk to}"
: "${TOKEN:?TOKEN is a service credential with the record privilege}"

OPENAPI_TYPESCRIPT=7.13.0   # needs typescript ^5 to generate; it is installed beside, never into, the checker
OPENAPI_FETCH=0.17.0
TS_GENERATOR_TYPESCRIPT=5.9.3
TS_CHECKER_TYPESCRIPT=7.0.2
TYPES_NODE=26.6.3
OPENAPI_PYTHON_CLIENT=0.29.1
SPEC="$PWD/api/openapi/v1.yaml"

command -v node >/dev/null || { echo "verify-clients: node >= 22 is required"; exit 2; }
command -v npm >/dev/null || { echo "verify-clients: npm is required"; exit 2; }
command -v python3 >/dev/null || { echo "verify-clients: python >= 3.11 is required"; exit 2; }
node -e 'process.exit(+process.versions.node.split(".")[0] >= 22 ? 0 : 1)' || { echo "verify-clients: node $(node --version) is too old, need >= 22"; exit 2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "==> TypeScript: generate from the document, type-check, round-trip"
mkdir -p "$work/ts-gen" "$work/ts"
(cd "$work/ts-gen" && npm init -y >/dev/null && npm install --silent --no-audit --no-fund "openapi-typescript@$OPENAPI_TYPESCRIPT" "typescript@$TS_GENERATOR_TYPESCRIPT")
(cd "$work/ts-gen" && npx --no-install openapi-typescript "$SPEC" -o "$work/ts/schema.d.ts")
cat >"$work/ts/package.json" <<JSON
{ "name": "generated-client-check", "private": true, "type": "module" }
JSON
cat >"$work/ts/tsconfig.json" <<'JSON'
{ "compilerOptions": { "target": "es2022", "module": "nodenext", "moduleResolution": "nodenext", "strict": true,
  "noUncheckedIndexedAccess": true, "skipLibCheck": false, "types": ["node"], "outDir": "out" }, "include": ["*.ts"] }
JSON
cat >"$work/ts/roundtrip.ts" <<'TS'
import createClient from "openapi-fetch";
import type { paths } from "./schema.d.ts";

function must(ok: boolean, what: string): void {
  if (!ok) {
    console.error(`FAIL (typescript): ${what}`);
    process.exit(1);
  }
}

const client = createClient<paths>({
  baseUrl: process.env.BASE_URL!,
  headers: { Authorization: `Bearer ${process.env.TOKEN}` },
});
const scope = { kind: "workspace", id: "w1" };

const allowed = await client.POST("/v1/admit", { body: { scope, resource: "llm.chat", max_cost: 10 } });
must(allowed.response.status === 200, `an allowed admit answers 200, got ${allowed.response.status}`);
must(allowed.data?.allowed === true && allowed.data.headroom === 7, `the Admission is read back: ${JSON.stringify(allowed.data)}`);

const refused = await client.POST("/v1/admit", { body: { scope, resource: "spent" } });
must(refused.response.status === 402, `a spent balance answers 402, got ${refused.response.status}`);
must(refused.error?.error.code === "payment_required", `the error code is read back: ${JSON.stringify(refused.error)}`);
must(
  refused.error !== undefined && "exceeded" in refused.error && refused.error.exceeded?.name === "scope_balance",
  `the refusal names the ceiling: ${JSON.stringify(refused.error)}`,
);
console.log("typescript: an allowed Admit and a refused one round-trip");
TS
(cd "$work/ts" && npm install --silent --no-audit --no-fund "openapi-fetch@$OPENAPI_FETCH" "typescript@$TS_CHECKER_TYPESCRIPT" "@types/node@$TYPES_NODE")
(cd "$work/ts" && npx --no-install tsc -p . && BASE_URL="$BASE_URL" TOKEN="$TOKEN" node out/roundtrip.js)

echo "==> Python: generate from the document, round-trip"
py="$work/py"
mkdir -p "$py"
if command -v uv >/dev/null; then
  uv venv -q --python ">=3.11" "$py/venv"
  uv pip install -q --python "$py/venv/bin/python" "openapi-python-client==$OPENAPI_PYTHON_CLIENT" httpx attrs python-dateutil
else
  python3 -m venv "$py/venv"
  "$py/venv/bin/pip" install -q "openapi-python-client==$OPENAPI_PYTHON_CLIENT" httpx attrs python-dateutil
fi
echo "post_hooks: []" >"$py/config.yml"   # no formatter needed: nobody reads this tree
(cd "$py" && venv/bin/openapi-python-client generate --path "$SPEC" --config config.yml --output-path metering_client --meta none)
cat >"$py/roundtrip.py" <<'PY'
import os
import sys

from metering_client import AuthenticatedClient
from metering_client.api.metering import admit
from metering_client.models import Admission, AdmissionRefusal, AdmitRequest, Scope


def must(ok: bool, what: str) -> None:
    if not ok:
        print(f"FAIL (python): {what}", file=sys.stderr)
        sys.exit(1)


client = AuthenticatedClient(base_url=os.environ["BASE_URL"], token=os.environ["TOKEN"])
scope = Scope(kind="workspace", id="w1")

allowed = admit.sync_detailed(client=client, body=AdmitRequest(scope=scope, resource="llm.chat", max_cost=10))
must(allowed.status_code == 200, f"an allowed admit answers 200, got {allowed.status_code}")
must(isinstance(allowed.parsed, Admission), f"the body is an Admission, got {type(allowed.parsed)}")
must(allowed.parsed.allowed is True and allowed.parsed.headroom == 7, f"the Admission is read back: {allowed.parsed}")

refused = admit.sync_detailed(client=client, body=AdmitRequest(scope=scope, resource="spent"))
must(refused.status_code == 402, f"a spent balance answers 402, got {refused.status_code}")
must(isinstance(refused.parsed, AdmissionRefusal), f"the body is an AdmissionRefusal, got {type(refused.parsed)}")
must(refused.parsed.error.code == "payment_required", f"the error code is read back: {refused.parsed}")
must(refused.parsed.exceeded is not None and refused.parsed.exceeded.name == "scope_balance", f"the refusal names the ceiling: {refused.parsed}")
print("python: an allowed Admit and a refused one round-trip")
PY
(cd "$py" && BASE_URL="$BASE_URL" TOKEN="$TOKEN" venv/bin/python roundtrip.py)

echo "OK — the clients generated from api/openapi/v1.yaml compile and round-trip an Admit"
