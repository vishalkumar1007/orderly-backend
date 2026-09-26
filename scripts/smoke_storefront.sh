#!/usr/bin/env bash
# End-to-end smoke test of the customer storefront API.
#
#   make -C .. run                 # API on :8080
#   ./scripts/smoke_storefront.sh  # against momo-magic.localhost
#
# Override with API, HOST. This exercises the public storefront, guest checkout,
# phone login, the payment state machine, order tracking and tenant isolation
# against a real database.
#
# Every JSON body is built into a variable first. Interpolating a double-quoted
# string inside a "$( … )" is a quoting trap in bash — the escaped quotes stop
# protecting the argument and the shell brace-expands the payload — so bodies
# are assembled here and passed as a single already-quoted word.
set -uo pipefail

API="${API:-http://127.0.0.1:8080}"
HOST="${HOST:-momo-magic.localhost}"
BASE="$API/api/v1"

PASS=0
FAIL=0

section() { printf '\n== %s\n' "$1"; }
pass()    { printf '  ok   %s (%s)\n' "$1" "$2"; PASS=$((PASS + 1)); }
fail()    { printf '  FAIL %s: expected %s got %s\n' "$1" "$2" "$3"; FAIL=$((FAIL + 1)); }
check()   { if [ "$2" = "$3" ]; then pass "$1" "$3"; else fail "$1" "$2" "$3"; fi; }
checky()  { if [ "$2" = "yes" ]; then pass "$1" yes; else fail "$1" yes "$2"; fi; }

# jget <json> <dotted.path> — reads one value out of a JSON response.
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
    if isinstance(doc, list):
        try:
            doc = doc[int(key)]
            continue
        except (ValueError, IndexError):
            print(""); raise SystemExit
    if isinstance(doc, dict):
        doc = doc.get(key)
    else:
        print(""); raise SystemExit
    if doc is None:
        print(""); raise SystemExit
if isinstance(doc, bool):
    print("true" if doc else "false")
elif isinstance(doc, (dict, list)):
    print(json.dumps(doc))
else:
    print(doc)
' "$2" 2>/dev/null
}

# req <method> <path> [body] [host] [auth] — prints the response body.
req() {
  local method="$1" path="$2" body="${3:-}" host="${4:-$HOST}" auth="${5:-}"
  if [ -n "$body" ] && [ -n "$auth" ]; then
    curl -s -m 20 -X "$method" -H "Host: $host" -H "Authorization: Bearer $auth" \
      -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  elif [ -n "$body" ]; then
    curl -s -m 20 -X "$method" -H "Host: $host" -H 'Content-Type: application/json' \
      -d "$body" "$BASE$path"
  elif [ -n "$auth" ]; then
    curl -s -m 20 -X "$method" -H "Host: $host" -H "Authorization: Bearer $auth" "$BASE$path"
  else
    curl -s -m 20 -X "$method" -H "Host: $host" "$BASE$path"
  fi
}

# status <method> <path> [body] [host] [auth] — prints the HTTP status only.
status() {
  local method="$1" path="$2" body="${3:-}" host="${4:-$HOST}" auth="${5:-}"
  if [ -n "$body" ] && [ -n "$auth" ]; then
    curl -s -m 20 -o /dev/null -w '%{http_code}' -X "$method" -H "Host: $host" \
      -H "Authorization: Bearer $auth" -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  elif [ -n "$body" ]; then
    curl -s -m 20 -o /dev/null -w '%{http_code}' -X "$method" -H "Host: $host" \
      -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  elif [ -n "$auth" ]; then
    curl -s -m 20 -o /dev/null -w '%{http_code}' -X "$method" -H "Host: $host" \
      -H "Authorization: Bearer $auth" "$BASE$path"
  else
    curl -s -m 20 -o /dev/null -w '%{http_code}' -X "$method" -H "Host: $host" "$BASE$path"
  fi
}

if ! curl -sf -m 5 "$API/health" >/dev/null; then
  echo "API is not reachable at $API — start it with 'make -C .. run'." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
section "public store configuration"
STORE=$(req GET /public/store)
checky "store name is present" "$([ -n "$(jget "$STORE" store.name)" ] && echo yes || echo no)"
checky "theme ships resolved CSS tokens" "$([ -n "$(jget "$STORE" theme.vars.--sf-primary)" ] && echo yes || echo no)"
checky "homepage sections are present" "$([ -n "$(jget "$STORE" homepage.sections)" ] && echo yes || echo no)"
checky "payment methods are listed" "$([ -n "$(jget "$STORE" payments.methods)" ] && echo yes || echo no)"
checky "ordering state is reported" "$([ -n "$(jget "$STORE" ordering.enabled)" ] && echo yes || echo no)"

section "public menu and products"
MENU=$(req GET /public/menu)
PRODUCT_ID=$(jget "$MENU" products.0.id)
PRODUCT_NAME=$(jget "$MENU" products.0.name)
PRODUCT_PRICE=$(jget "$MENU" products.0.price)
checky "products are listed" "$([ -n "$PRODUCT_ID" ] && echo yes || echo no)"
echo "     using $PRODUCT_NAME at $PRODUCT_PRICE"
EXPECTED_TOTAL=$(python3 -c "print(round($PRODUCT_PRICE * 2, 2))")

check "an unknown product 404s" "404" \
  "$(status GET /public/products/00000000-0000-0000-0000-000000000000)"
check "a product detail loads" "200" "$(status GET "/public/products/$PRODUCT_ID")"

section "guest quote (server-side pricing)"
QUOTE_BODY='{"items":[{"product_id":"'"$PRODUCT_ID"'","quantity":2}]}'
QUOTE=$(req POST /public/quote "$QUOTE_BODY")
check "quote total is priced by the server" "$EXPECTED_TOTAL" "$(jget "$QUOTE" totals.total)"

section "checkout validation"
MISSING_NAME='{"customer_phone":"9876543210","items":[{"product_id":"'"$PRODUCT_ID"'","quantity":1}]}'
MISSING_PHONE='{"customer_name":"Asha Rao","items":[{"product_id":"'"$PRODUCT_ID"'","quantity":1}]}'
EMPTY_CART='{"customer_name":"Asha Rao","customer_phone":"9876543210","items":[]}'
UNKNOWN_ITEM='{"customer_name":"Asha Rao","customer_phone":"9876543210","items":[{"product_id":"00000000-0000-0000-0000-000000000000","quantity":1}]}'
TAMPERED_PRICE='{"customer_name":"Asha Rao","customer_phone":"9876543210","items":[{"product_id":"'"$PRODUCT_ID"'","quantity":1,"price":1}]}'
check "a missing name is rejected"     "400" "$(status POST /public/orders "$MISSING_NAME")"
check "a missing phone is rejected"    "400" "$(status POST /public/orders "$MISSING_PHONE")"
check "an empty cart is rejected"      "400" "$(status POST /public/orders "$EMPTY_CART")"
check "an unknown product is rejected" "400" "$(status POST /public/orders "$UNKNOWN_ITEM")"
check "a client price is refused"      "400" "$(status POST /public/orders "$TAMPERED_PRICE")"

section "place a guest order"
ORDER_BODY='{"customer_name":"Asha Rao","customer_phone":"9876543210","payment_method":"CASH","client_token":"smoke-'"$(date +%s)"'-'"$RANDOM"'","items":[{"product_id":"'"$PRODUCT_ID"'","quantity":2}]}'
ORDER=$(req POST /public/orders "$ORDER_BODY")
ORDER_NO=$(jget "$ORDER" order_number)
checky "the order is created" "$([ -n "$ORDER_NO" ] && echo yes || echo no)"
echo "     order #$ORDER_NO ($(jget "$ORDER" reference)) total $(jget "$ORDER" totals.total)"
check "the backend prices the order"  "$EXPECTED_TOTAL" "$(jget "$ORDER" totals.total)"
check "cash is recorded as pending"   "PENDING"       "$(jget "$ORDER" payment.status)"
check "cash skips the payment step"   "confirmation"  "$(jget "$ORDER" next_step)"
check "the customer number is masked" "+91••••3210"   "$(jget "$ORDER" phone_masked)"

section "duplicate order prevention"
DUP=$(req POST /public/orders "$ORDER_BODY")
check "a resubmit returns the same order" "$ORDER_NO" "$(jget "$DUP" order_number)"
check "a resubmit is flagged as a duplicate" "true" "$(jget "$DUP" duplicate)"

section "order tracking authorisation"
check "tracking without a phone is refused" "400" "$(status GET "/public/orders/$ORDER_NO")"
check "tracking with the wrong phone 404s"  "404" "$(status GET "/public/orders/$ORDER_NO?phone=9999999999")"
check "tracking with the right phone works" "200" "$(status GET "/public/orders/$ORDER_NO?phone=9876543210")"
TRACK=$(req GET "/public/orders/$ORDER_NO?phone=9876543210")
checky "a tracking timeline is returned" "$([ -n "$(jget "$TRACK" timeline.0.label)" ] && echo yes || echo no)"
check "the timeline starts at the first step" "PENDING" "$(jget "$TRACK" timeline.0.key)"

section "guest order lookup by phone"
LOOKUP=$(req GET "/public/orders/lookup?phone=9876543210")
check "lookup finds the order" "$ORDER_NO" "$(jget "$LOOKUP" orders.0.order_number)"

section "tenant isolation"
check "another tenant's host cannot read the order" "404" \
  "$(status GET "/public/orders/$ORDER_NO?phone=9876543210" "" "no-such-shop.localhost")"
check "an unknown tenant host is rejected" "404" \
  "$(status GET /public/store "" "no-such-shop.localhost")"
check "the api host is not a storefront host" "400" \
  "$(status GET /public/store "" "api.localhost")"

section "phone login"
OTP=$(req POST /auth/customer/send-otp '{"phone":"9876543210"}')
CODE=$(jget "$OTP" otp.dev_code)
checky "a code is issued" "$([ -n "$CODE" ] && echo yes || echo no)"
check "the number is masked in the response" "+91••••3210" "$(jget "$OTP" phone)"
check "a malformed number is rejected" "400" "$(status POST /auth/customer/send-otp '{"phone":"abc"}')"
check "an immediate resend is rate limited" "429" "$(status POST /auth/customer/send-otp '{"phone":"9876543210"}')"
check "a wrong code is rejected" "400" \
  "$(status POST /auth/customer/verify-otp '{"phone":"9876543210","code":"000000"}')"

SESSION=$(req POST /auth/customer/verify-otp '{"phone":"9876543210","code":"'"$CODE"'"}')
ACCESS=$(jget "$SESSION" tokens.access_token)
checky "a session is issued" "$([ -n "$ACCESS" ] && echo yes || echo no)"
check "the session masks the number" "+91••••3210" "$(jget "$SESSION" customer.phone)"
check "a used code cannot be replayed" "400" \
  "$(status POST /auth/customer/verify-otp '{"phone":"9876543210","code":"'"$CODE"'"}')"
check "the profile is readable with the token" "200" \
  "$(status GET /customer/profile "" "$HOST" "$ACCESS")"
PROFILE=$(req GET /customer/profile "" "$HOST" "$ACCESS")
check "the profile masks the number" "+91••••3210" "$(jget "$PROFILE" customer.phone)"

section "order ownership isolation"
check "a customer cannot read another order" "404" \
  "$(status GET "/public/orders/$ORDER_NO" "" "$HOST" "$ACCESS")"
check "a customer cannot pay another order" "404" \
  "$(status POST "/public/orders/$ORDER_NO/pay" '{"method":"ONLINE"}' "$HOST" "$ACCESS")"

section "online payment flow"
ONLINE_BODY='{"customer_name":"Ravi Kumar","customer_phone":"9811122233","payment_method":"ONLINE","client_token":"smoke-pay-'"$(date +%s)"'-'"$RANDOM"'","items":[{"product_id":"'"$PRODUCT_ID"'","quantity":1}]}'
ONLINE=$(req POST /public/orders "$ONLINE_BODY")
ONLINE_NO=$(jget "$ONLINE" order_number)
checky "an online order is created" "$([ -n "$ONLINE_NO" ] && echo yes || echo no)"
check "the customer is sent to pay" "pay" "$(jget "$ONLINE" next_step)"

START_BODY='{"method":"ONLINE","phone":"9811122233"}'
START=$(req POST "/public/orders/$ONLINE_NO/pay" "$START_BODY")
PAY_TOKEN=$(jget "$START" payment_token)
checky "a payment session starts" "$([ -n "$PAY_TOKEN" ] && echo yes || echo no)"

NO_TOKEN='{"outcome":"SUCCESS","phone":"9811122233"}'
check "confirming without a token is refused" "409" \
  "$(status POST "/public/orders/$ONLINE_NO/pay/confirm" "$NO_TOKEN")"

FAIL_BODY='{"outcome":"FAILURE","reason":"Insufficient funds","token":"'"$PAY_TOKEN"'","phone":"9811122233"}'
FAILED=$(req POST "/public/orders/$ONLINE_NO/pay/confirm" "$FAIL_BODY")
check "the failure is recorded" "FAILED" "$(jget "$FAILED" payment_status)"
check "a retry is offered" "true" "$(jget "$FAILED" can_retry)"
check "the order survives the failure" "$ONLINE_NO" "$(jget "$FAILED" order_number)"

RESTART=$(req POST "/public/orders/$ONLINE_NO/pay" "$START_BODY")
PAY_TOKEN2=$(jget "$RESTART" payment_token)
checky "a retry mints a new session" "$([ -n "$PAY_TOKEN2" ] && [ "$PAY_TOKEN2" != "$PAY_TOKEN" ] && echo yes || echo no)"

STALE_BODY='{"outcome":"SUCCESS","token":"'"$PAY_TOKEN"'","phone":"9811122233"}'
check "the superseded token is dead" "409" \
  "$(status POST "/public/orders/$ONLINE_NO/pay/confirm" "$STALE_BODY")"

SUCCESS_BODY='{"outcome":"SUCCESS","token":"'"$PAY_TOKEN2"'","phone":"9811122233"}'
PAID=$(req POST "/public/orders/$ONLINE_NO/pay/confirm" "$SUCCESS_BODY")
check "the payment is captured" "PAID" "$(jget "$PAID" payment_status)"

LATE_FAILURE='{"outcome":"FAILURE","token":"'"$PAY_TOKEN2"'","phone":"9811122233"}'
check "a captured payment survives a late failure callback" "200" \
  "$(status POST "/public/orders/$ONLINE_NO/pay/confirm" "$LATE_FAILURE")"
FINAL=$(req GET "/public/orders/$ONLINE_NO/pay?phone=9811122233")
check "it is still paid afterwards" "PAID" "$(jget "$FINAL" payment_status)"

section "order tracking reflects the order"
check "the guest order is still pending" "PENDING" \
  "$(jget "$(req GET "/public/orders/lookup?phone=9876543210")" orders.0.status)"

# ---------------------------------------------------------------------------
printf '\n----------------------------------------\n'
printf 'passed: %s   failed: %s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
