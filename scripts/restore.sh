#!/usr/bin/env bash
# Restores a ProxUI database dump.
#
# TimescaleDB is why this is a script and not a pg_restore one-liner. A plain
# restore into an empty database produces a portal that looks healthy - it logs
# in, the inventory is all there, the audit trail is intact - and has silently
# lost every metric, because the hypertables came back as ordinary tables with
# broken triggers. The restore drill found exactly that, with 109 ignored
# errors and charts that returned 500.
#
# The order below is what TimescaleDB requires:
#   1. an empty target - pg_restore --clean would drop the extension partway
#      through, which defeats the wrapper below and is how the first attempt
#      at this script failed
#   2. create the extension first, at the same version
#   3. timescaledb_pre_restore()  - stop the background workers and let the
#      catalogue be written directly
#   4. pg_restore
#   5. timescaledb_post_restore() - put it back together
set -euo pipefail

DUMP="${1:-}"
DATABASE_URL="${PROXUI_DATABASE_URL:-${DATABASE_URL:-}}"

if [[ -z "${DUMP}" || -z "${DATABASE_URL}" ]]; then
  echo "usage: PROXUI_DATABASE_URL=postgres://... scripts/restore.sh <dump>" >&2
  exit 1
fi
if [[ ! -f "${DUMP}" ]]; then
  echo "error: ${DUMP} does not exist" >&2
  exit 1
fi

if [[ -f "${DUMP}.sha256" ]]; then
  echo "==> verifying checksum"
  sha256sum --check "${DUMP}.sha256"
else
  echo "warning: no checksum beside ${DUMP}; restoring unverified" >&2
fi

EXISTING="$(psql "${DATABASE_URL}" -tAc \
  "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" 2>/dev/null || echo 0)"

if [[ "${EXISTING}" -gt 0 && "${FORCE:-0}" != "1" ]]; then
  cat >&2 <<EOF
error: the target database already has ${EXISTING} tables.

Restoring over live data is not something to do by accident. If this is
really what you want:

  FORCE=1 scripts/restore.sh ${DUMP}
EOF
  exit 1
fi

if [[ "${EXISTING}" -gt 0 ]]; then
  # Emptying the target rather than restoring over it: pg_restore --clean
  # drops the TimescaleDB extension in the middle of the restore, and
  # everything after that lands in a database that no longer knows what a
  # hypertable is.
  echo "==> emptying the target (FORCE=1)"
  psql "${DATABASE_URL}" -q <<'SQL'
DROP EXTENSION IF EXISTS timescaledb CASCADE;
DROP SCHEMA IF EXISTS public CASCADE;
CREATE SCHEMA public;
SQL
fi

echo "==> preparing TimescaleDB in the target"
psql "${DATABASE_URL}" -q -c "CREATE EXTENSION IF NOT EXISTS timescaledb;"
psql "${DATABASE_URL}" -q -c "SELECT timescaledb_pre_restore();"

# post_restore has to run even if pg_restore fails part-way; leaving a database
# in pre-restore mode means background workers stay stopped and nothing tells
# you why compression and retention quietly never run again.
finish() {
  echo "==> finalizing TimescaleDB"
  psql "${DATABASE_URL}" -q -c "SELECT timescaledb_post_restore();" || true
}
trap finish EXIT

echo "==> restoring ${DUMP}"
pg_restore --no-owner --no-privileges --exit-on-error \
  --dbname="${DATABASE_URL}" "${DUMP}"

trap - EXIT
finish

echo "==> verifying"
FAILED=0
for table in users platforms vms audit_logs; do
  COUNT="$(psql "${DATABASE_URL}" -tAc "SELECT count(*) FROM ${table}" 2>/dev/null || echo "?")"
  printf '    %-14s %s rows\n' "${table}" "${COUNT}"
done

# The check that actually matters, and the one a plain restore fails silently.
HYPERTABLES="$(psql "${DATABASE_URL}" -tAc \
  "SELECT count(*) FROM timescaledb_information.hypertables" 2>/dev/null || echo 0)"
printf '    %-14s %s\n' "hypertables" "${HYPERTABLES}"
if [[ "${HYPERTABLES}" -lt 2 ]]; then
  echo "    ERROR: metrics hypertables are missing; performance history did not survive" >&2
  FAILED=1
fi

SAMPLES="$(psql "${DATABASE_URL}" -tAc "SELECT count(*) FROM metrics_vm" 2>/dev/null || echo 0)"
printf '    %-14s %s rows\n' "metrics_vm" "${SAMPLES}"

AGGREGATES="$(psql "${DATABASE_URL}" -tAc \
  "SELECT count(*) FROM timescaledb_information.continuous_aggregates" 2>/dev/null || echo 0)"
printf '    %-14s %s\n' "rollups" "${AGGREGATES}"
if [[ "${AGGREGATES}" -lt 4 ]]; then
  echo "    ERROR: continuous aggregates are missing; the 24h and longer charts will fail" >&2
  FAILED=1
fi

if [[ "${FAILED}" -ne 0 ]]; then
  echo >&2
  echo "Restore completed with errors. Do not put this database into service." >&2
  exit 1
fi

cat <<EOF

Restore complete and verified.

Before starting the portal, confirm PROXUI_MASTER_KEY is the same key that was
in use when the dump was taken. A mismatched key leaves every platform
credential undecryptable, which shows up as authentication failures against
platforms whose settings look correct.
EOF
