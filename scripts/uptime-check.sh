#!/usr/bin/env bash
# One-shot uptime probe for cron / any monitor that can run a command.
#
#   scripts/uptime-check.sh https://api.orderly.qd.je [max_ms]
#
# Set HEALTH_TOKEN to the server's HEALTH_DETAIL_TOKEN to also see version and schema.
# Exit 0  = API is ready (HTTP 200, database ok) and answered within max_ms (default 2000).
# Exit 1  = down, unhealthy, or too slow. Prints one line saying why, ready to mail or log.
set -uo pipefail
base="${1:-}"; max_ms="${2:-2000}"
[[ -n "$base" ]] || { echo "usage: $0 <api base url> [max_ms]" >&2; exit 2; }
base="${base%/}"

start=$(python3 -c 'import time; print(int(time.time()*1000))')
err="$(mktemp)"
out="$(curl -sS -m 5 ${HEALTH_TOKEN:+-H "X-Health-Token: $HEALTH_TOKEN"} -o /tmp/orderly-uptime.$$ -w '%{http_code}' "$base/health/ready" 2> "$err")"; rc=$?
why="$(tr '\n' ' ' < "$err")"; rm -f "$err"
end=$(python3 -c 'import time; print(int(time.time()*1000))')
ms=$((end - start)); body="$(cat /tmp/orderly-uptime.$$ 2>/dev/null)"; rm -f /tmp/orderly-uptime.$$

if (( rc != 0 )); then echo "DOWN  $base unreachable: $why"; exit 1; fi
if [[ "$out" != "200" ]]; then echo "DOWN  $base answered HTTP $out: $body"; exit 1; fi
if ! grep -q '"ok":true' <<< "$body"; then echo "DOWN  $base is up but the database check is not ok: $body"; exit 1; fi
if (( ms > max_ms )); then echo "SLOW  $base took ${ms}ms (limit ${max_ms}ms)"; exit 1; fi
echo "UP    $base ready in ${ms}ms $(grep -oE '"version":"[^"]*"' <<< "$body" | head -1)"
