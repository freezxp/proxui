#!/usr/bin/env bash
# Backs up everything needed to rebuild a ProxUI installation.
#
# Three things matter and they are not interchangeable:
#   1. the database  — inventory, users, audit trail, encrypted credentials
#   2. the master key — without it the credentials in the dump are unreadable
#   3. nothing else  — the binary and config are rebuilt from the repository
#
# The key is deliberately NOT written into the backup directory. A dump and the
# key that decrypts it, sitting together, is one stolen archive away from every
# platform credential you own.
set -euo pipefail

BACKUP_DIR="${BACKUP_DIR:-./backups}"
DATABASE_URL="${PROXUI_DATABASE_URL:-${DATABASE_URL:-}}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
TARGET="${BACKUP_DIR}/proxui-${STAMP}.dump"

if [[ -z "${DATABASE_URL}" ]]; then
  echo "error: set PROXUI_DATABASE_URL (or DATABASE_URL) to the database to back up" >&2
  exit 1
fi

mkdir -p "${BACKUP_DIR}"

# A newer pg_dump writes SET statements an older server rejects on restore
# (PostgreSQL 17 emits transaction_timeout, which 16 does not understand). The
# dump succeeds and the restore fails, which is the worst place to find out.
CLIENT_MAJOR="$(pg_dump --version | grep -oE '[0-9]+' | head -1)"
SERVER_MAJOR="$(psql "${DATABASE_URL}" -tAc 'SHOW server_version' | cut -d. -f1)"
if [[ "${CLIENT_MAJOR}" != "${SERVER_MAJOR}" ]]; then
  cat >&2 <<EOF
error: pg_dump is version ${CLIENT_MAJOR} but the server is ${SERVER_MAJOR}.

Dump with a client matching the server, or the restore will fail on SET
statements the older server does not recognize. In the shipped compose
deployment:

  docker compose exec -T db pg_dump --format=custom --no-owner --no-privileges \\
    -U proxui proxui > backup.dump
EOF
  exit 1
fi

echo "==> dumping database to ${TARGET}"
# Custom format: compressed, and restorable table-by-table if a partial restore
# is ever needed.
pg_dump --format=custom --no-owner --no-privileges --file="${TARGET}" "${DATABASE_URL}"

SIZE="$(du -h "${TARGET}" | cut -f1)"
echo "==> wrote ${TARGET} (${SIZE})"

# A checksum turns "the restore failed" into "the backup was already corrupt",
# which is a much shorter conversation at 3am.
sha256sum "${TARGET}" > "${TARGET}.sha256"
echo "==> checksum ${TARGET}.sha256"

cat <<EOF

Backup complete.

Keep separately, and verify you still have it:
  PROXUI_MASTER_KEY   the 32-byte key that decrypts platform credentials.
                      Without it, a restored database has unusable platforms
                      and every one must be re-entered by hand.

Restore with:
  scripts/restore.sh ${TARGET}
EOF
