#!/usr/bin/env python3
"""A host product that is NOT written in Go, doing what docs/integration-guide.md tells a host to do, with nothing but
the Python standard library: it holds a bearer credential and speaks REST. Everything product-specific it brings is a
rate card (`standard`: an `enrichment` at 2000 credits each, an `api_call` at 150000 credits per million) and the name
of one scope it bills.

    adoption_host.py BASE_URL TOKEN SCOPE_ID

It prints one `STEP ok|FAIL <what> <detail>` line per check and exits non-zero if any failed. It is run by
integration/adoption_test.go, which starts the real paymentd and payment-worker; it is not a unit test of anything.
"""
import datetime
import json
import sys
import time
import urllib.error
import urllib.request

base, token, scope_id = sys.argv[1].rstrip("/"), sys.argv[2], sys.argv[3]
SCOPE = {"kind": "workspace", "id": scope_id}
failures = []


def call(method, path, body=None, key=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, method=method, data=data)
    req.add_header("Authorization", "Bearer " + token)
    req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Idempotency-Key", key)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {"raw": raw.decode(errors="replace")}


def step(what, ok, detail=""):
    print("STEP", "ok  " if ok else "FAIL", what, detail, flush=True)
    if not ok:
        failures.append(what)


def code_of(body):
    return (body or {}).get("error", {}).get("code") if isinstance((body or {}).get("error"), dict) else (body or {}).get("code")


# 1. An empty scope is refused BEFORE any work: 402 with a code a caller can act on (guide section 7).
st, body = call("POST", "/v1/admit", {"scope": SCOPE, "resource": "crm.enrich", "max_cost": 1000})
step("an unfunded scope is refused before the work", st == 402, f"{st} {body}")

# 2. Fund it. The worker opens the scope's shard, so a first grant may have to wait for it.
for _ in range(60):
    st, body = call("POST", "/v1/grant", {"scope": SCOPE, "amount": 1_000_000, "reason": "purchase"}, key="walkthrough-grant-1")
    if st == 200:
        break
    time.sleep(0.5)
step("a grant is accepted", st == 200, f"{st} {body}")

# 3. Admit with MaxCost, then do the work, then Record what was ACTUALLY produced, with a stable key and a stable time.
st, body = call("POST", "/v1/admit", {"scope": SCOPE, "resource": "crm.enrich", "max_cost": 10_000})
step("a funded scope is admitted", st == 200 and body.get("allowed") is True, f"{st} {body}")

started = datetime.datetime.now(datetime.timezone.utc).isoformat()
event = {
    "scope": SCOPE, "resource": "crm.enrich", "rate_key": "standard", "occurred_at": started,
    "quantities": [{"unit": "enrichment", "amount": 3}, {"unit": "api_call", "amount": 2000}],
}
# 3 enrichments at 2000 each, and 2000 calls at 150000 per million: 6000 + 300.
st, first = call("POST", "/v1/record", event, key="req-0001")
step("a record is priced exactly and applied", st == 200 and first.get("applied") is True and first.get("delta") == -6300, f"{st} {first}")

# 4. The SAME charge replayed (a retry): the same answer, and the balance moves once.
st, again = call("POST", "/v1/record", event, key="req-0001")
step("the same charge replayed is not applied twice", st == 200 and again.get("applied") is False, f"{st} {again}")
st, bal = call("GET", f"/v1/balance?scope_kind=workspace&scope_id={scope_id}")
step("the balance moved once", st == 200 and bal.get("total") == 1_000_000 - 6_300, f"{st} {bal}")

# 5. The same key with a DIFFERENT request is a bug in how keys are derived, never a silent replay.
other = dict(event, quantities=[{"unit": "enrichment", "amount": 4}])
st, body = call("POST", "/v1/record", other, key="req-0001")
step("the same key with a different request is refused", st == 409, f"{st} {body}")

# 6. A unit the card does not price is refused, not priced at zero.
st, body = call("POST", "/v1/record", dict(event, quantities=[{"unit": "unheard_of", "amount": 1}]), key="req-0002")
step("an unpriced unit is refused (fail closed)", st >= 400 and st < 500, f"{st} {body}")

# 7. A request that could not be afforded is refused up front.
st, body = call("POST", "/v1/admit", {"scope": SCOPE, "resource": "crm.enrich", "max_cost": 50_000_000})
step("an unaffordable MaxCost is refused", st == 402, f"{st} {body}")

sys.exit(1 if failures else 0)
