#!/usr/bin/env bash
#
# Black-box security failure tests for the wager service.
#
# Prerequisites: the stack is running (docker compose up -d), plus curl and
# python3. The script obtains real tokens from Keycloak and exercises the HTTP
# API trying to bypass authentication and authorization. It exits non-zero when
# any check fails.
#
# Usage:
#   ./scripts/security-test.sh
#   API_URL=http://localhost:8081 KEYCLOAK_URL=http://localhost:8090 ./scripts/security-test.sh
#
# Environment:
#   API_URL              API base URL (auto-detected from Compose when omitted)
#   KEYCLOAK_URL         Keycloak base URL (default http://localhost:8090)
#   REALM                Keycloak realm (default wager)
#   ADMIN_USER/ADMIN_PASSWORD  Keycloak admin credentials for the expired-token
#                              check (default admin/admin; set SKIP_EXPIRED=true
#                              to skip it)
#   SKIP_EXPIRED         set to true to skip the expired-token check
set -uo pipefail

REALM="${REALM:-wager}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8090}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin}"
SKIP_EXPIRED="${SKIP_EXPIRED:-false}"

if [ -z "${API_URL:-}" ]; then
  if command -v docker >/dev/null 2>&1; then
    detected_port=$(docker compose port api 8080 2>/dev/null | head -1 | sed 's/.*://')
  fi
  API_URL="http://localhost:${detected_port:-8080}"
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required" >&2
  exit 2
fi

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT
BODY_FILE="$WORK_DIR/body"

TOTAL=0
PASSED=0
FAILED=0
SKIPPED=0

if [ -t 1 ]; then
  green() { printf '\033[32m%s\033[0m' "$1"; }
  red()   { printf '\033[31m%s\033[0m' "$1"; }
  yellow(){ printf '\033[33m%s\033[0m' "$1"; }
else
  green() { printf '%s' "$1"; }
  red()   { printf '%s' "$1"; }
  yellow(){ printf '%s' "$1"; }
fi

pass() { TOTAL=$((TOTAL + 1)); PASSED=$((PASSED + 1)); printf '  [%s] %s\n' "$(green PASS)" "$1"; }
fail() {
  TOTAL=$((TOTAL + 1)); FAILED=$((FAILED + 1))
  printf '  [%s] %s\n' "$(red FAIL)" "$1"
  [ -n "${2:-}" ] && printf '         %s\n' "$2"
}
skip() { SKIPPED=$((SKIPPED + 1)); printf '  [%s] %s\n' "$(yellow SKIP)" "$1"; }

section() { printf '\n== %s\n' "$1"; }

json_get() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

# check_status <expected> <description> <curl args...>
check_status() {
  local expected="$1"; shift
  local description="$1"; shift
  local status
  status=$(curl -s -o "$BODY_FILE" -w '%{http_code}' "$@")
  if [ "$status" = "$expected" ]; then
    pass "$description"
  else
    fail "$description" "expected HTTP $expected, got $status: $(head -c 200 "$BODY_FILE")"
  fi
}

# check_body <expected> <substring> <description> <curl args...>
check_body_contains() {
  local expected="$1"; shift
  local needle="$1"; shift
  local description="$1"; shift
  local status
  status=$(curl -s -o "$BODY_FILE" -w '%{http_code}' "$@")
  if [ "$status" = "$expected" ] && grep -q "$needle" "$BODY_FILE"; then
    pass "$description"
  else
    fail "$description" "expected HTTP $expected with '$needle', got $status: $(head -c 200 "$BODY_FILE")"
  fi
}

fetch_token() {
  curl -s -X POST "$KEYCLOAK_URL/realms/$REALM/protocol/openid-connect/token" \
    -d grant_type=client_credentials -d "client_id=$1" -d "client_secret=$2" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])' 2>/dev/null
}

wait_ready() {
  echo "== waiting for $API_URL/health/ready"
  for _ in $(seq 1 60); do
    if curl -fsS "$API_URL/health/ready" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "service did not become ready at $API_URL" >&2
  exit 2
}

wait_ready

section "obtaining tokens from Keycloak ($KEYCLOAK_URL)"
INTERNAL_TOKEN=$(fetch_token internal-service internal-service-secret)
PROVIDER_A_TOKEN=$(fetch_token provider-a provider-a-secret)
PROVIDER_B_TOKEN=$(fetch_token provider-b provider-b-secret)
if [ -z "$INTERNAL_TOKEN" ] || [ -z "$PROVIDER_A_TOKEN" ] || [ -z "$PROVIDER_B_TOKEN" ]; then
  echo "could not obtain tokens; is Keycloak running and the realm imported?" >&2
  exit 2
fi
echo "  tokens acquired"

SUFFIX=$(python3 -c 'import uuid;print(uuid.uuid4().hex[:10])')
ISSUER=$(printf '%s' "$PROVIDER_A_TOKEN" | cut -d. -f2 | python3 -c '
import sys,base64,json
p=sys.stdin.read().strip(); p += "="*(-len(p)%4)
print(json.loads(base64.urlsafe_b64decode(p))["iss"])')

section "setup: wallet and transactions"
PLAYER_ID=$(python3 -c 'import uuid;print(uuid.uuid4())')
WALLET_RESPONSE=$(curl -s -X POST "$API_URL/wallets" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER_ID\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}")
WALLET_ID=$(printf '%s' "$WALLET_RESPONSE" | json_get "['id']" 2>/dev/null)
if [ -z "$WALLET_ID" ]; then
  echo "setup failed: could not open a wallet ($WALLET_RESPONSE)" >&2
  exit 2
fi
echo "  wallet $WALLET_ID"

submit_tx() {
  local token="$1" provider="$2" external_id="$3"
  curl -s -X POST "$API_URL/wagering/transactions" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: $provider:$external_id" \
    -d "{\"providerId\":\"$provider\",\"externalTransactionId\":\"$external_id\",\"playerId\":\"$PLAYER_ID\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-sec\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"10.00\",\"currency\":\"BRL\"}}"
}

TX_A_RESPONSE=$(submit_tx "$PROVIDER_A_TOKEN" provider-a "sec-script-a-$SUFFIX")
TX_A=$(printf '%s' "$TX_A_RESPONSE" | json_get "['transactionId']" 2>/dev/null)
TX_B_RESPONSE=$(submit_tx "$PROVIDER_B_TOKEN" provider-b "sec-script-b-$SUFFIX")
TX_B=$(printf '%s' "$TX_B_RESPONSE" | json_get "['transactionId']" 2>/dev/null)
if [ -z "$TX_A" ] || [ -z "$TX_B" ]; then
  echo "setup failed: could not create transactions" >&2
  exit 2
fi
echo "  transactions $TX_A (provider-a) and $TX_B (provider-b)"

section "missing and malformed credentials (expect 401)"
check_body_contains 401 "MISSING_TOKEN" "no Authorization header" \
  "$API_URL/wallets/$WALLET_ID"
check_body_contains 401 "MISSING_TOKEN" "Basic scheme" \
  -H 'Authorization: Basic aW50ZXJuYWw6c2VjcmV0' "$API_URL/wallets/$WALLET_ID"
check_body_contains 401 "MISSING_TOKEN" "raw token without scheme" \
  -H "Authorization: $INTERNAL_TOKEN" "$API_URL/wallets/$WALLET_ID"
check_body_contains 401 "MISSING_TOKEN" "unknown scheme" \
  -H "Authorization: Token $INTERNAL_TOKEN" "$API_URL/wallets/$WALLET_ID"
check_body_contains 401 "INVALID_TOKEN" "garbage token" \
  -H 'Authorization: Bearer garbage.token.value' "$API_URL/wallets/$WALLET_ID"
check_status 401 "token in query string is not accepted" \
  "$API_URL/wallets/$WALLET_ID?access_token=$INTERNAL_TOKEN"
check_status 200 "auth scheme is case-insensitive (bearer)" \
  -H "Authorization: bearer $INTERNAL_TOKEN" "$API_URL/wallets/$WALLET_ID"

section "forged tokens (expect 401)"
ALG_NONE_TOKEN=$(python3 - "$ISSUER" <<'PY'
import base64, json, sys, time

def b64(value):
    return base64.urlsafe_b64encode(json.dumps(value, separators=(',', ':')).encode()).rstrip(b'=').decode()

now = int(time.time())
header = b64({"alg": "none", "typ": "JWT"})
payload = b64({
    "iss": sys.argv[1], "aud": "wager-api", "sub": "attacker",
    "iat": now, "exp": now + 3600,
    "realm_access": {"roles": ["internal", "provider"]},
    "provider_id": "provider-a",
})
print(header + "." + payload + ".")
PY
)
check_body_contains 401 "INVALID_TOKEN" "alg=none token" \
  -H "Authorization: Bearer $ALG_NONE_TOKEN" "$API_URL/wallets/$WALLET_ID"

TAMPERED_TOKEN=$(python3 - "$PROVIDER_A_TOKEN" <<'PY'
import base64, json, sys

header, payload, signature = sys.argv[1].split(".")
padded = payload + "=" * (-len(payload) % 4)
claims = json.loads(base64.urlsafe_b64decode(padded))
claims["provider_id"] = "provider-b"
claims["realm_access"] = {"roles": ["internal"]}
forged = base64.urlsafe_b64encode(json.dumps(claims, separators=(',', ':')).encode()).rstrip(b'=').decode()
print(header + "." + forged + "." + signature)
PY
)
check_body_contains 401 "INVALID_TOKEN" "tampered payload with original signature" \
  -H "Authorization: Bearer $TAMPERED_TOKEN" "$API_URL/wallets/$WALLET_ID"

BROKEN_SIGNATURE_TOKEN=$(python3 - "$INTERNAL_TOKEN" <<'PY'
import sys
header, payload, signature = sys.argv[1].split(".")
flipped = ("A" if signature[0] != "A" else "B") + signature[1:]
print(header + "." + payload + "." + flipped)
PY
)
check_body_contains 401 "INVALID_TOKEN" "corrupted signature" \
  -H "Authorization: Bearer $BROKEN_SIGNATURE_TOKEN" "$API_URL/wallets/$WALLET_ID"

MASTER_TOKEN=$(curl -s -X POST "$KEYCLOAK_URL/realms/master/protocol/openid-connect/token" \
  -d grant_type=password -d client_id=admin-cli -d "username=$ADMIN_USER" -d "password=$ADMIN_PASSWORD" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null)
if [ -n "$MASTER_TOKEN" ]; then
  check_body_contains 401 "INVALID_TOKEN" "token from another realm (wrong issuer)" \
    -H "Authorization: Bearer $MASTER_TOKEN" "$API_URL/wallets/$WALLET_ID"
else
  skip "token from another realm (admin credentials unavailable)"
fi

section "expired token (expect 401)"
if [ "$SKIP_EXPIRED" = "true" ]; then
  skip "expired token check disabled (SKIP_EXPIRED=true)"
else
  ADMIN_TOKEN=$(curl -s -X POST "$KEYCLOAK_URL/realms/master/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=admin-cli -d "username=$ADMIN_USER" -d "password=$ADMIN_PASSWORD" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null)
  if [ -z "$ADMIN_TOKEN" ]; then
    skip "expired token check (could not obtain an admin token)"
  else
    CLIENT_ID="expired-check-$SUFFIX"
    CLIENT_SECRET="expired-check-secret-$SUFFIX"
    CREATE_BODY=$(python3 - "$CLIENT_ID" "$CLIENT_SECRET" <<'PY'
import json, sys
client_id, secret = sys.argv[1], sys.argv[2]
print(json.dumps({
    "clientId": client_id,
    "enabled": True,
    "protocol": "openid-connect",
    "publicClient": False,
    "clientAuthenticatorType": "client-secret",
    "secret": secret,
    "serviceAccountsEnabled": True,
    "standardFlowEnabled": False,
    "directAccessGrantsEnabled": False,
    "attributes": {"access.token.lifespan": "1"},
    "protocolMappers": [
        {"name": "provider-id", "protocol": "openid-connect",
         "protocolMapper": "oidc-hardcoded-claim-mapper",
         "config": {"claim.name": "provider_id", "claim.value": "provider-a",
                    "jsonType.label": "String", "access.token.claim": "true",
                    "id.token.claim": "false", "userinfo.token.claim": "false"}},
        {"name": "audience", "protocol": "openid-connect",
         "protocolMapper": "oidc-audience-mapper",
         "config": {"included.custom.audience": "wager-api",
                    "access.token.claim": "true", "id.token.claim": "false"}},
    ],
}))
PY
)
    CLIENT_LOCATION=$(curl -s -D - -o /dev/null -X POST "$KEYCLOAK_URL/admin/realms/$REALM/clients" \
      -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
      -d "$CREATE_BODY" | tr -d '\r' | awk -F': ' 'tolower($1)=="location"{print $2}')
    CLIENT_UUID=$(basename "$CLIENT_LOCATION" 2>/dev/null)
    if [ -z "$CLIENT_UUID" ] || [ "$CLIENT_UUID" = "." ]; then
      skip "expired token check (could not create a temporary client)"
    else
      EXPIRING_TOKEN=$(fetch_token "$CLIENT_ID" "$CLIENT_SECRET")
      sleep 3
      if [ -z "$EXPIRING_TOKEN" ]; then
        fail "expired token check setup" "temporary client did not issue a token"
      else
        check_body_contains 401 "INVALID_TOKEN" "expired token" \
          -H "Authorization: Bearer $EXPIRING_TOKEN" "$API_URL/wallets/$WALLET_ID"
      fi
      curl -s -o /dev/null -X DELETE "$KEYCLOAK_URL/admin/realms/$REALM/clients/$CLIENT_UUID" \
        -H "Authorization: Bearer $ADMIN_TOKEN"
    fi
  fi
fi

section "authorization matrix (expect 401/403)"
check_body_contains 403 "FORBIDDEN" "provider cannot open a wallet" \
  -X POST -H "Authorization: Bearer $PROVIDER_A_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER_ID\",\"initialBalance\":{\"amount\":\"1.00\",\"currency\":\"BRL\"}}" \
  "$API_URL/wallets"
check_body_contains 403 "FORBIDDEN" "provider cannot read a wallet" \
  -H "Authorization: Bearer $PROVIDER_A_TOKEN" "$API_URL/wallets/$WALLET_ID"
check_body_contains 403 "FORBIDDEN" "provider cannot read the ledger" \
  -H "Authorization: Bearer $PROVIDER_B_TOKEN" "$API_URL/wallets/$WALLET_ID/ledger"
check_body_contains 403 "FORBIDDEN" "provider cannot reconcile" \
  -X POST -H "Authorization: Bearer $PROVIDER_A_TOKEN" "$API_URL/wallets/$WALLET_ID/reconciliation"
check_body_contains 403 "FORBIDDEN" "provider A cannot read provider B transaction by id" \
  -H "Authorization: Bearer $PROVIDER_A_TOKEN" "$API_URL/wagering/transactions/$TX_B"
check_body_contains 403 "FORBIDDEN" "provider A cannot read provider B by external path" \
  -H "Authorization: Bearer $PROVIDER_A_TOKEN" "$API_URL/providers/provider-b/wagering/transactions/sec-script-b-$SUFFIX"
check_body_contains 403 "FORBIDDEN" "provider B cannot read provider A by external path" \
  -H "Authorization: Bearer $PROVIDER_B_TOKEN" "$API_URL/providers/provider-a/wagering/transactions/sec-script-a-$SUFFIX"
check_body_contains 403 "FORBIDDEN" "provider B cannot submit for provider A" \
  -X POST -H "Authorization: Bearer $PROVIDER_B_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: provider-a:forged-$SUFFIX" \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"forged-$SUFFIX\",\"playerId\":\"$PLAYER_ID\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-sec\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"10.00\",\"currency\":\"BRL\"}}" \
  "$API_URL/wagering/transactions"
check_status 200 "provider A reads its own transaction" \
  -H "Authorization: Bearer $PROVIDER_A_TOKEN" "$API_URL/providers/provider-a/wagering/transactions/sec-script-a-$SUFFIX"
check_status 200 "internal service reads any transaction" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" "$API_URL/wagering/transactions/$TX_A"

section "unauthorized attempts have no side effects"
BALANCE_BEFORE=$(curl -s "$API_URL/wallets/$WALLET_ID" -H "Authorization: Bearer $INTERNAL_TOKEN" | json_get "['balance']['amount']")
LEDGER_BEFORE=$(curl -s "$API_URL/wallets/$WALLET_ID/ledger" -H "Authorization: Bearer $INTERNAL_TOKEN" \
  | python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin),sort_keys=True))')

check_status 404 "forged transaction was not created" \
  -H "Authorization: Bearer $PROVIDER_A_TOKEN" "$API_URL/providers/provider-a/wagering/transactions/forged-$SUFFIX"

BALANCE_AFTER=$(curl -s "$API_URL/wallets/$WALLET_ID" -H "Authorization: Bearer $INTERNAL_TOKEN" | json_get "['balance']['amount']")
LEDGER_AFTER=$(curl -s "$API_URL/wallets/$WALLET_ID/ledger" -H "Authorization: Bearer $INTERNAL_TOKEN" \
  | python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin),sort_keys=True))')

if [ "$BALANCE_BEFORE" = "$BALANCE_AFTER" ]; then
  pass "wallet balance unchanged ($BALANCE_BEFORE)"
else
  fail "wallet balance unchanged" "before=$BALANCE_BEFORE after=$BALANCE_AFTER"
fi
if [ "$LEDGER_BEFORE" = "$LEDGER_AFTER" ]; then
  pass "ledger unchanged"
else
  fail "ledger unchanged" "the ledger changed after unauthorized attempts"
fi

section "public endpoints remain public (expect 200)"
for path in /health/live /health/ready /metrics; do
  check_status 200 "GET $path" "$API_URL$path"
done

printf '\n== summary: %d checks, %d passed, %d failed, %d skipped\n' "$TOTAL" "$PASSED" "$FAILED" "$SKIPPED"
if [ "$FAILED" -gt 0 ]; then
  printf '%s\n' "$(red 'SECURITY FAILURES DETECTED')"
  exit 1
fi
printf '%s\n' "$(green 'all security checks passed')"
