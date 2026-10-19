#!/usr/bin/env bash
# End-to-end smoke test of the payment path, through the real API.
#
# Its main job is the last gate before going live: one real payment with the
# gateway's TEST keys (or a Rs 1 payment with live keys), then a refund.
#
#   OWNER_TOKEN=... ADMIN_TOKEN=... PRODUCT_ID=... ./scripts/payments_smoke.sh
#
# Needed:
#   API           API base, e.g. https://api.example.com        (default http://127.0.0.1:8080)
#   HOST          the business's own host, e.g. shop.example.com (default momo-magic.localhost)
#   OWNER_TOKEN   an owner's access token for that business
#   PRODUCT_ID    a product to order. For a live-keys test make it a Rs 1 product.
# Optional:
#   ADMIN_HOST    host for Super Admin calls                     (default api.localhost)
#   ADMIN_TOKEN   a Super Admin token: adds the payment-health check
#   PHONE         customer phone for the test order              (default 9000000001)
#   WAIT_SECS     how long to wait for you to pay                (default 300)
#   ALLOW_SANDBOX=1  also accept a business in dummy mode: the script then
#                    "pays" by itself, so the script can be tried with no gateway.
#
# With a live-mode business the script stops and asks a human to pay in the
# browser (gateway test card or UPI), because that is the one step a script
# cannot do honestly. Everything else is checked automatically, including that
# the order really became paid through the gateway's callback and was then
# refunded.
set -uo pipefail

API="${API:-http://127.0.0.1:8080}"
HOST="${HOST:-momo-magic.localhost}"
ADMIN_HOST="${ADMIN_HOST:-api.localhost}"
PHONE="${PHONE:-9000000001}"
WAIT_SECS="${WAIT_SECS:-300}"
BASE="$API/api/v1"

PASS=0
FAIL=0

section() { printf '\n== %s\n' "$1"; }
pass()    { printf '  ok   %s (%s)\n' "$1" "$2"; PASS=$((PASS + 1)); }
fail()    { printf '  FAIL %s: expected %s got %s\n' "$1" "$2" "$3"; FAIL=$((FAIL + 1)); }
check()   { if [ "$2" = "$3" ]; then pass "$1" "$3"; else fail "$1" "$2" "$3"; fi; }
note()    { printf '       %s\n' "$1"; }

for tool in curl python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "needs $tool"; exit 2; }
done
[ -n "${OWNER_TOKEN:-}" ] || { echo "OWNER_TOKEN is required"; exit 2; }
[ -n "${PRODUCT_ID:-}" ]  || { echo "PRODUCT_ID is required";  exit 2; }

# jget <json> <dotted.path>: one value out of a JSON response ("" when absent).
jget() {
  printf '%s' "$1" | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit
for key in sys.argv[1].split("."):
    if key == "":
        continue
    if isinstance(doc, dict) and key in doc:
        doc = doc[key]
    else:
        print(""); raise SystemExit
if isinstance(doc, bool):
    print("true" if doc else "false")
elif doc is None:
    print("")
else:
    print(doc)
' "$2"
}

# call <host> <method> <path> [token] [body] : prints the response body.
call() {
  local host="$1" method="$2" path="$3" token="${4:-}" body="${5:-}"
  local args=(-sS -X "$method" -H "Host: $host" -H "Content-Type: application/json")
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  [ -n "$body" ]  && args+=(--data "$body")
  curl "${args[@]}" "$BASE$path"
}
shop()  { call "$HOST" "$@"; }
admin() { call "$ADMIN_HOST" "$@"; }

section "The business is ready for the payment you are about to make"
MODE=$(shop GET /tenant/payments/mode "$OWNER_TOKEN")
mode=$(jget "$MODE" mode)
ready=$(jget "$MODE" online_ready)
if [ "$mode" = "LIVE" ]; then
  pass "business is in real-payment mode" LIVE
  check "online payment is ready" true "$ready"
elif [ "${ALLOW_SANDBOX:-}" = "1" ]; then
  pass "business is in dummy mode (ALLOW_SANDBOX=1)" "$mode"
else
  fail "business is in real-payment mode" LIVE "$mode"
  echo "Switch the business to Real payments first (Super Admin -> Payments), or set ALLOW_SANDBOX=1."
  exit 1
fi

section "Place an order and start paying"
STAMP="smoke-$(date +%s)-$RANDOM"
ORDER_BODY=$(printf '{"customer_name":"Payment Smoke","customer_phone":"%s","payment_method":"ONLINE","client_token":"%s","items":[{"product_id":"%s","quantity":1}]}' "$PHONE" "$STAMP" "$PRODUCT_ID")
ORDER=$(shop POST /public/orders "" "$ORDER_BODY")
NUM=$(jget "$ORDER" order_number)
[ -n "$NUM" ] || { fail "order placed" "an order number" "$ORDER"; exit 1; }
pass "order placed" "#$NUM"

START_BODY=$(printf '{"method":"ONLINE","phone":"%s"}' "$PHONE")
START=$(shop POST "/public/orders/$NUM/pay" "" "$START_BODY")
amount=$(jget "$START" amount)
pass "payment started" "amount $amount"
if [ "$mode" = "LIVE" ]; then
  check "payment runs in LIVE mode" LIVE "$(jget "$START" payment_mode)"
  gw_order=$(jget "$START" checkout.order_id)
  [ -n "$gw_order" ] && pass "gateway order created" "$gw_order" || fail "gateway order created" "an id" "none"
  check "browser got no token that could settle it" "" "$(jget "$START" payment_token)"
fi

section "The customer pays"
if [ "$mode" = "LIVE" ]; then
  echo "  Open the storefront and pay order #$NUM:"
  echo "    https://$HOST/payment?order=$NUM        (phone: $PHONE)"
  echo "  Use the gateway's test card or test UPI. Waiting up to ${WAIT_SECS}s…"
else
  TOKEN=$(jget "$START" payment_token)
  CONFIRM_BODY=$(printf '{"outcome":"SUCCESS","token":"%s","phone":"%s"}' "$TOKEN" "$PHONE")
  shop POST "/public/orders/$NUM/pay/confirm" "" "$CONFIRM_BODY" >/dev/null
fi
waited=0
status=""
while [ "$waited" -le "$WAIT_SECS" ]; do
  S=$(shop GET "/public/orders/$NUM/pay?phone=$PHONE")
  status=$(jget "$S" payment_status)
  [ "$status" = "PAID" ] && break
  sleep 3
  waited=$((waited + 3))
done
check "payment became PAID (confirmed by the gateway, not the browser)" PAID "$status"
[ "$status" = "PAID" ] || { echo "Not paid in time; stopping before the refund."; echo; echo "passed: $PASS  failed: $FAIL"; exit 1; }
check "order cannot be paid twice" false "$(jget "$S" payable)"

section "The shop sees it"
ORDERS=$(shop GET /tenant/orders "$OWNER_TOKEN")
PAYID=$(printf '%s' "$ORDERS" | python3 -c '
import json, sys
num = int(sys.argv[1])
doc = json.load(sys.stdin)
rows = doc if isinstance(doc, list) else doc.get("orders", doc.get("items", []))
for o in rows:
    if o.get("order_number") == num and o.get("payment"):
        print(o["payment"].get("id", "") + "|" + o["payment"].get("status", ""))
        break
' "$NUM")
payment_id="${PAYID%%|*}"
check "staff order list shows the payment as PAID" PAID "${PAYID##*|}"

section "Refund it"
REFUND=$(shop POST "/tenant/payments/$payment_id/refund" "$OWNER_TOKEN" '{"reason":"payment smoke test"}')
note "refund answered: done=$(jget "$REFUND" done) status=$(jget "$REFUND" payment_status)"
waited=0
final=""
while [ "$waited" -le 90 ]; do
  S=$(shop GET "/public/orders/$NUM/pay?phone=$PHONE")
  final=$(jget "$S" payment_status)
  [ "$final" = "REFUNDED" ] && break
  sleep 3
  waited=$((waited + 3))
done
check "payment ended REFUNDED" REFUNDED "$final"
AGAIN=$(shop POST "/tenant/payments/$payment_id/refund" "$OWNER_TOKEN" '{}')
check "a second refund is refused" refund_in_progress "$(jget "$AGAIN" error.code)"

if [ -n "${ADMIN_TOKEN:-}" ]; then
  section "Platform payment health"
  H=$(admin GET /admin/payments/health "$ADMIN_TOKEN")
  for k in overdue_pending stuck_refunds mismatches_24h duplicates_24h; do
    check "$k is zero" 0 "$(jget "$H" "$k")"
  done
fi

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
