#!/usr/bin/env bash
# Applies every shipped migration to a clean PostgreSQL, seeds a realm, asserts the schema's
# behaviour, then applies the Phase 2 DRAFT on top and asserts that too. A schema is verified by
# applying it (constitution XI), not by reading it.
#
#   PSQL='psql postgres://payment:payment@localhost:5432/payment' scripts/verify-schema.sh
#   PSQL='docker exec -i <container> psql -U payment -d payment' scripts/verify-schema.sh
#
# The database MUST be empty: this script creates the whole schema.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
PSQL="${PSQL:-psql ${PAYMENT_LEDGER_DSN:-postgres://payment:payment@localhost:5432/payment}}"
run() { $PSQL -X -q -v ON_ERROR_STOP=1 "$@"; }

echo "== baseline migrations"
for f in "$root"/migrations/*.sql; do
  echo "   $(basename "$f")"
  run < "$f"
done

echo "== seed (realm 'verify')"
run -v realm=verify < "$root/scripts/seed.sql" >/dev/null

echo "== baseline assertions"
run < "$root/scripts/schema-assertions.sql" 2>&1 | sed -e 's/^psql:[^:]*:[0-9]*: NOTICE:  /   /' -e 's/^NOTICE:  /   /'

echo "== phase 2 draft (verified, not shipped)"
for f in "$root"/specs/002-postpaid-invoicing/draft-migrations/*.sql; do
  echo "   $(basename "$f")"
  run < "$f"
done
run < "$root/scripts/schema-assertions-002.sql" 2>&1 | sed -e 's/^psql:[^:]*:[0-9]*: NOTICE:  /   /' -e 's/^NOTICE:  /   /'

echo "schema verified"
