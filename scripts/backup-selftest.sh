#!/usr/bin/env bash
# Self-test for backup.sh / restore-test.sh. A restore test that cannot fail is
# worthless, so this takes a real backup, checks it restores, then damages copies
# of it in every way that matters and checks the restore test REFUSES each one.
# Also checks retention and that a failed backup never costs an existing one.
#
#   PG_CONTAINER=orderly-postgres scripts/backup-selftest.sh
# Touches only a temp directory and throwaway restore databases. Safe on a dev DB.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=pg-env.sh
source "$here/pg-env.sh"

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
export BACKUP_DIR="$work/good"
pass=0; fail=0
ok()   { echo "  PASS  $1"; pass=$((pass + 1)); }
bad()  { echo "  FAIL  $1"; fail=$((fail + 1)); }
expect_ok()   { local label="$1"; shift; if "$@" > "$work/out" 2>&1; then ok "$label"; else bad "$label"; sed 's/^/        /' "$work/out"; fi; }
expect_fail() { local label="$1"; shift; if "$@" > "$work/out" 2>&1; then bad "$label (the restore test ACCEPTED a bad backup)"; else ok "$label"; fi; }

echo "BACKUP"
expect_ok "takes a backup" "$here/backup.sh"
good="$(ls -1 "$BACKUP_DIR"/orderly-*.dump | head -1)"; base="${good%.dump}"
expect_ok "writes dump + checksum + manifest" bash -c "[[ -s '$good' && -s '$base.sha256' && -s '$base.manifest' ]]"
# Customer data: nobody but the owner may read it. ls -l's group/other columns must be empty.
private() { for f in "$@"; do [[ "$(ls -ld "$f" | cut -c5-10)" == "------" ]] || { echo "$f is readable by others: $(ls -ld "$f")"; return 1; }; done; }
expect_ok "backup files are private to their owner (no group/other access)" private "$good" "$base.sha256" "$base.manifest"
fresh="$work/created-by-backup"
expect_ok "a backup directory it creates is private too" bash -c "BACKUP_DIR='$fresh' '$here/backup.sh' > /dev/null && [[ \"\$(ls -ld '$fresh' | cut -c5-10)\" == '------' ]]"

echo "RESTORE TEST"
expect_ok "accepts the good backup" "$here/restore-test.sh" "$good"

mk() { # copy the good backup into a fresh dir: mk <name> → prints dump path
	mkdir -p "$work/$1"; cp "$base".* "$work/$1/"; echo "$work/$1/$(basename "$good")"
}
d="$(mk flipped)";   printf 'X' | dd of="$d" bs=1 seek=2000 conv=notrunc 2> /dev/null
expect_fail "rejects a dump changed after it was written (checksum)" "$here/restore-test.sh" "$d"

d="$(mk truncated)"; head -c 60000 "$good" > "$d"; ( cd "$work/truncated" && shasum -a 256 "$(basename "$d")" > "$(basename "$d" .dump).sha256" )
expect_fail "rejects a truncated dump even with a matching checksum" "$here/restore-test.sh" "$d"

d="$(mk garbage)";   head -c 5000 /dev/urandom > "$d"; ( cd "$work/garbage" && shasum -a 256 "$(basename "$d")" > "$(basename "$d" .dump).sha256" )
expect_fail "rejects a file that is not a dump" "$here/restore-test.sh" "$d"

d="$(mk counts)";    m="${d%.dump}.manifest"
sed -i.bak 's/^table:tenants=.*/table:tenants=99999/' "$m"
expect_fail "rejects data that disagrees with the manifest (row counts)" "$here/restore-test.sh" "$d"

d="$(mk missing)";   m="${d%.dump}.manifest"; echo "table:no_such_table=3" >> "$m"
expect_fail "rejects a table the backup should contain but does not" "$here/restore-test.sh" "$d"

d="$(mk schema)";    m="${d%.dump}.manifest"
sed -i.bak 's/^schema_version=.*/schema_version=19990101000000/' "$m"
expect_fail "rejects a schema version that disagrees with the manifest" "$here/restore-test.sh" "$d"

expect_fail "fails clearly when there is no backup" env BACKUP_DIR="$work/none" "$here/restore-test.sh"

left="$(echo "SELECT count(*) FROM pg_database WHERE datname LIKE 'orderly_restore_check%';" | psql_db postgres | tr -d ' ')"
[[ "$left" == "0" ]] && ok "every failed run dropped its scratch database" || bad "$left scratch database(s) left behind"

echo "RETENTION AND SAFETY"
export BACKUP_DIR="$work/retain"
for i in 1 2 3 4; do sleep 1.1; KEEP_LAST=2 "$here/backup.sh" > /dev/null 2>&1 || bad "backup $i failed"; done
n="$(ls -1 "$BACKUP_DIR"/orderly-*.dump | wc -l | tr -d ' ')"
[[ "$n" == "2" ]] && ok "keeps only the newest KEEP_LAST backups" || bad "kept $n backups, wanted 2"
[[ "$(ls -1 "$BACKUP_DIR"/orderly-*.sha256 | wc -l | tr -d ' ')" == "2" && "$(ls -1 "$BACKUP_DIR"/orderly-*.manifest | wc -l | tr -d ' ')" == "2" ]] \
	&& ok "prunes the checksum and manifest with the dump" || bad "orphaned checksum/manifest files"
before="$(ls -1 "$BACKUP_DIR" | sort | tr '\n' ' ')"
if [[ -n "${PG_CONTAINER:-}" ]]; then broken=(env PG_CONTAINER=no-such-container-xyz); else broken=(env DATABASE_URL=postgres://nobody@127.0.0.1:1/none); fi
expect_fail "a backup that cannot reach the database fails" "${broken[@]}" KEEP_LAST=1 "$here/backup.sh"
after="$(ls -1 "$BACKUP_DIR" | sort | tr '\n' ' ')"
[[ "$before" == "$after" ]] && ok "a failed backup leaves existing backups untouched (and no half-written file)" || bad "backup directory changed: [$before] → [$after]"

echo
if (( fail == 0 )); then echo "BACKUP SELF-TEST PASSED ($pass checks)"; else echo "BACKUP SELF-TEST FAILED ($fail of $((pass + fail)))"; exit 1; fi
