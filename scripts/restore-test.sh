#!/usr/bin/env bash
# Proves a backup can actually be restored. Restores the dump into a throwaway
# database on the same server (never touching the real one), checks it, and
# drops it again.
#
#   PG_CONTAINER=orderly-postgres scripts/restore-test.sh backups/orderly-<UTC>.dump
#   scripts/restore-test.sh            # newest backup in BACKUP_DIR
#
# Checks: checksum matches · pg_restore finishes with no errors (constraints and
# foreign keys included) · the migration version matches the manifest · every
# table in the manifest exists with a row count within tolerance (a live database
# keeps changing while it is dumped; default tolerance = max(5 rows, 2%)).
# Exit 0 = the backup is restorable. Anything else = do not trust it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=pg-env.sh
source "$here/pg-env.sh"

BACKUP_DIR="${BACKUP_DIR:-./backups}"
dump="${1:-$(ls -1t "$BACKUP_DIR"/orderly-*.dump 2>/dev/null | head -1 || true)}"
[[ -n "$dump" && -f "$dump" ]] || { echo "FAILED: no backup found (give a path, or set BACKUP_DIR)" >&2; exit 1; }
base="${dump%.dump}"

fail() { echo "FAILED: $*" >&2; exit 1; }
check_client_version || exit 1

echo "restore test: $dump"
if [[ -f "$base.sha256" ]]; then
	( cd "$(dirname "$dump")" && shasum -a 256 -c "$(basename "$base").sha256" > /dev/null ) || fail "checksum mismatch (the file changed or is corrupt)"
	echo "  ok  checksum"
else
	echo "  warn no .sha256 next to the backup; integrity not checked"
fi

scratch="orderly_restore_check_$(date -u +%Y%m%d%H%M%S)_$$"
[[ "$scratch" != "$SOURCE_DB" ]] || fail "refusing to restore over the source database"
cleanup() { echo "DROP DATABASE IF EXISTS \"$scratch\";" | psql_db postgres > /dev/null 2>&1 || echo "warn: could not drop $scratch — drop it by hand" >&2; }
trap cleanup EXIT

echo "CREATE DATABASE \"$scratch\";" | psql_db postgres > /dev/null || fail "cannot create the scratch database (needs CREATEDB)"
restore_to "$scratch" < "$dump" 2> /tmp/orderly-restore.$$.log || { cat /tmp/orderly-restore.$$.log >&2; rm -f /tmp/orderly-restore.$$.log; fail "pg_restore reported errors"; }
rm -f /tmp/orderly-restore.$$.log
echo "  ok  pg_restore (no errors)"

restored_schema="$(schema_version "$scratch")"
[[ -n "$restored_schema" ]] || fail "the restored database has no migration history (goose_db_version)"
restored="$(mktemp)"; trap 'rm -f "$restored"; cleanup' EXIT
table_counts "$scratch" > "$restored"
[[ -s "$restored" ]] || fail "the restored database has no tables"

if [[ -f "$base.manifest" ]]; then
	want_schema="$(sed -n 's/^schema_version=//p' "$base.manifest")"
	[[ "$restored_schema" == "$want_schema" ]] || fail "schema version $restored_schema, backup recorded $want_schema"
	echo "  ok  schema version $restored_schema"
	bad=0; rows=0; n=0
	while IFS='=' read -r tbl want; do
		tbl="${tbl#table:}"; n=$((n + 1))
		got="$(sed -n "s/^$tbl=//p" "$restored")"
		if [[ -z "$got" ]]; then echo "  FAIL table '$tbl' is missing after restore" >&2; bad=1; continue; fi
		rows=$((rows + got))
		tol=$(( want / 50 )); (( tol < 5 )) && tol=5
		diff=$(( got > want ? got - want : want - got ))
		if (( diff > tol )); then echo "  FAIL $tbl: restored $got rows, backup recorded $want (tolerance $tol)" >&2; bad=1; fi
	done < <(grep '^table:' "$base.manifest")
	(( bad == 0 )) || fail "restored data does not match the backup manifest"
	echo "  ok  $n tables, $rows rows match the manifest"
else
	echo "  warn no .manifest; checked only that the restore ran and tables exist ($(wc -l < "$restored" | tr -d ' ') tables)"
fi
echo "RESTORE TEST PASSED: $dump is restorable"
