# Shared by backup.sh and restore-test.sh (sourced, not run).
#
# Two ways to reach Postgres:
#   PG_CONTAINER=<name>   run pg_dump/psql/pg_restore INSIDE that container, so the
#                         tool version always matches the server (use this in
#                         production, e.g. PG_CONTAINER=fs-A1-d3e4-p5, and locally
#                         with orderly-postgres). PGUSER / PGDATABASE pick the role
#                         and database (default orderly / orderly).
#   DATABASE_URL=<url>    use pg_dump/psql/pg_restore on this machine. The client
#                         must be the same major version as the server, or newer
#                         for dumps but NOT for restores of its own dumps.

if [[ -n "${PG_CONTAINER:-}" ]]; then
	PGUSER="${PGUSER:-orderly}"
	PGDATABASE="${PGDATABASE:-orderly}"
	SOURCE_DB="$PGDATABASE"
	# $1 = database to connect to
	psql_db()    { docker exec -i "$PG_CONTAINER" psql -X -q -A -t -v ON_ERROR_STOP=1 -U "$PGUSER" -d "$1"; }
	dump_db()    { docker exec -i "$PG_CONTAINER" pg_dump -Fc --no-owner -U "$PGUSER" -d "$1"; }
	restore_to() { docker exec -i "$PG_CONTAINER" pg_restore --no-owner --exit-on-error -U "$PGUSER" -d "$1"; }   # dump on stdin
	list_dump()  { docker exec -i "$PG_CONTAINER" pg_restore --list; }                                            # dump on stdin
elif [[ -n "${DATABASE_URL:-}" ]]; then
	SOURCE_DB="$(printf '%s' "$DATABASE_URL" | sed -E 's#^[a-z]+://[^/]+/([^?]*).*#\1#')"
	url_for()    { printf '%s' "$DATABASE_URL" | sed -E "s#^([a-z]+://[^/]+)/[^?]*#\\1/$1#"; }
	psql_db()    { psql -X -q -A -t -v ON_ERROR_STOP=1 "$(url_for "$1")"; }
	dump_db()    { pg_dump -Fc --no-owner "$(url_for "$1")"; }
	restore_to() { pg_restore --no-owner --exit-on-error -d "$(url_for "$1")"; }
	list_dump()  { pg_restore --list; }
else
	echo "set PG_CONTAINER (docker) or DATABASE_URL (host tools)" >&2
	exit 2
fi

# "table=rowcount" for every public table, one per line, sorted. Exact counts
# (not planner estimates), in plain SQL so it needs nothing but psql. A table
# dropped between being listed and being counted (a scratch table, a migration)
# is simply left out rather than failing the whole run.
table_counts() {
	psql_db "$1" <<'SQL'
CREATE FUNCTION pg_temp.safe_count(tbl text) RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE n bigint;
BEGIN
	EXECUTE format('SELECT count(*) FROM public.%I', tbl) INTO n;
	RETURN n;
EXCEPTION WHEN undefined_table THEN
	RETURN NULL;
END $f$;
SELECT table_name || '=' || c
FROM (SELECT table_name, pg_temp.safe_count(table_name) AS c
      FROM information_schema.tables
      WHERE table_schema = 'public' AND table_type = 'BASE TABLE') t
WHERE c IS NOT NULL
ORDER BY 1;
SQL
}

# Just the table names (cheap), for "which tables existed before the dump started".
table_names() {
	psql_db "$1" <<'SQL'
SELECT table_name FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1;
SQL
}

# Latest applied goose migration, or empty.
schema_version() {
	psql_db "$1" <<'SQL' 2>/dev/null || true
SELECT max(version_id) FROM goose_db_version WHERE is_applied;
SQL
}

# Host-tools mode only: a dump made by a NEWER pg_dump than the server cannot be
# restored onto that server (it emits settings the server does not know), so such
# a "backup" would fail exactly when it is needed. Refuse it up front.
check_client_version() {
	[[ -n "${PG_CONTAINER:-}" ]] && return 0
	local client server
	client="$(pg_restore --version | grep -oE '[0-9]+' | head -1)"
	server="$(echo 'SHOW server_version_num;' | psql_db "$SOURCE_DB" | tr -d ' ' | cut -c1-2)"
	if [[ -n "$client" && -n "$server" && "$client" -gt "$server" ]]; then
		echo "FAILED: this machine's PostgreSQL client is v$client but the server is v$server." >&2
		echo "        A dump from a newer client cannot be restored onto an older server." >&2
		echo "        Use PG_CONTAINER=<postgres container> (tools match the server), or install the v$server client." >&2
		return 1
	fi
}
