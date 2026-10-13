#!/usr/bin/env bash
# Orderly database backup: one consistent pg_dump (custom format), verified
# readable, checksummed, with a manifest the restore test checks against.
#
#   PG_CONTAINER=fs-A1-d3e4-p5 BACKUP_DIR=/var/backups/orderly scripts/backup.sh
#   DATABASE_URL=postgres://... scripts/backup.sh
#
# Env: BACKUP_DIR (default ./backups)   KEEP_LAST (newest backups to keep, default 48)
# Files per backup:  orderly-<UTC>.dump  .sha256  .manifest
# Exit 0 only if a complete, readable backup now exists; the new backup is written
# under a temporary name and moved into place last, and old ones are pruned only
# after that, so a failed run never costs you an existing backup.
set -euo pipefail
# A backup is the whole customer database in one file. Everything this script
# creates — the directory (if it makes it), the dump, checksum and manifest — is
# private to the user running it.
umask 077
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=pg-env.sh
source "$here/pg-env.sh"

BACKUP_DIR="${BACKUP_DIR:-./backups}"
KEEP_LAST="${KEEP_LAST:-48}"
mkdir -p "$BACKUP_DIR"
ts="$(date -u +%Y%m%dT%H%M%SZ)"
name="orderly-$ts"
part="$BACKUP_DIR/.$name.dump.part"
trap 'rm -f "$part" "$BACKUP_DIR/.$name.manifest.part" "$BACKUP_DIR/.$name.tables.part"' EXIT

check_client_version || exit 1
echo "backing up database '$SOURCE_DB' …"
# Tables that exist BEFORE the dump starts. A table created or dropped while the
# dump runs (a migration, a scratch table) is left out of the manifest, so the
# manifest only ever lists tables the dump is guaranteed to contain.
before="$BACKUP_DIR/.$name.tables.part"
table_names "$SOURCE_DB" > "$before"
dump_db "$SOURCE_DB" > "$part"
[[ -s "$part" ]] || { echo "FAILED: dump is empty" >&2; exit 1; }
# A dump that pg_restore cannot even list is not a backup.
list_dump < "$part" > /dev/null || { echo "FAILED: dump is not a readable pg_restore archive" >&2; exit 1; }

{
	echo "dumped_at=$ts"
	echo "database=$SOURCE_DB"
	echo "schema_version=$(schema_version "$SOURCE_DB")"
	# Counts are taken right after the dump; rows written in between are why the
	# restore test allows a small tolerance.
	table_counts "$SOURCE_DB" | while IFS='=' read -r t c; do
		if grep -qxF "$t" "$before"; then echo "table:$t=$c"; fi
	done
} > "$BACKUP_DIR/.$name.manifest.part"

mv "$part" "$BACKUP_DIR/$name.dump"
( cd "$BACKUP_DIR" && shasum -a 256 "$name.dump" > "$name.sha256" )
mv "$BACKUP_DIR/.$name.manifest.part" "$BACKUP_DIR/$name.manifest"

# Prune: keep the newest KEEP_LAST complete backups.
# ponytail: count-based retention (hourly × 48 = 2 days). Add daily/weekly tiers if you need longer history.
ls -1t "$BACKUP_DIR"/orderly-*.dump 2>/dev/null | tail -n +"$((KEEP_LAST + 1))" | while read -r old; do
	base="${old%.dump}"
	rm -f "$old" "$base.sha256" "$base.manifest"
	echo "pruned $(basename "$old")"
done

size="$(du -h "$BACKUP_DIR/$name.dump" | cut -f1)"
echo "OK: $BACKUP_DIR/$name.dump ($size, $(grep -c '^table:' "$BACKUP_DIR/$name.manifest") tables)"
