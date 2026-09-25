#!/usr/bin/env bash
# End-to-end authenticated demo against a running stack.
#
#   docker compose up -d --build
#   ./scripts/demo.sh
#
# Override API_URL / KEYCLOAK_URL when the stack is not on localhost.
set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8090}"
REALM="${REALM:-wager}"

json() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

token() {
  curl -fsS -X POST "$KEYCLOAK_URL/realms/$REALM/protocol/openid-connect/token" \
    -d grant_type=client_credentials -d "client_id=$1" -d "client_secret=$2" | json "['access_token']"
}

wait_ready() {
  echo "== waiting for $API_URL/health/ready"
  for _ in $(seq 1 60); do
    if curl -fsS "$API_URL/health/ready" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "service did not become ready" >&2
  exit 1
}

wait_ready

INTERNAL_TOKEN=$(token internal-service internal-service-secret)
PROVIDER_A=$(token provider-a provider-a-secret)
PROVIDER_B=$(token provider-b provider-b-secret)

PLAYER_ID=$(python3 -c 'import uuid;print(uuid.uuid4())')
SUFFIX=$(python3 -c 'import uuid;print(uuid.uuid4().hex[:8])')
BET_ID="demo-bet-$SUFFIX"
WIN_ID="demo-win-$SUFFIX"
LOSS_ID="demo-loss-$SUFFIX"
REFUND_ID="demo-refund-$SUFFIX"
ROLLBACK_ID="demo-rollback-$SUFFIX"
echo "== opening wallet for player $PLAYER_ID"
WALLET=$(curl -fsS -X POST "$API_URL/wallets" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER_ID\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}")
WALLET_ID=$(echo "$WALLET" | json "['id']")
echo "$WALLET" | python3 -m json.tool

submit() {
  local token="$1" key="$2" body="$3"
  curl -fsS -X POST "$API_URL/wagering/transactions" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: $key" -d "$body"
}

bet_body() {
  cat <<EOF
{"providerId":"provider-a","externalTransactionId":"$1","playerId":"$PLAYER_ID","walletId":"$WALLET_ID","roundId":"round-demo","gameId":"fortune-chimp","kind":"$2","money":{"amount":"$3","currency":"BRL"}$4}
EOF
}

echo "== BET 25.00"
submit "$PROVIDER_A" "provider-a:$BET_ID" "$(bet_body $BET_ID BET 25.00 "")" | python3 -m json.tool

echo "== replay of the same BET (idempotent)"
submit "$PROVIDER_A" "provider-a:$BET_ID" "$(bet_body $BET_ID BET 25.00 "")" | python3 -m json.tool

echo "== WIN 10.00"
submit "$PROVIDER_A" "provider-a:$WIN_ID" "$(bet_body $WIN_ID WIN 10.00 "")" | python3 -m json.tool

echo "== LOSS 0.00 (no balance movement)"
submit "$PROVIDER_A" "provider-a:$LOSS_ID" "$(bet_body $LOSS_ID LOSS 0.00 "")" | python3 -m json.tool

echo "== REFUND of the BET"
submit "$PROVIDER_A" "provider-a:$REFUND_ID" \
  "$(bet_body $REFUND_ID REFUND 25.00 ",\"referenceExternalTransactionId\":\"$BET_ID\"")" | python3 -m json.tool

echo "== ROLLBACK of the WIN"
submit "$PROVIDER_A" "provider-a:$ROLLBACK_ID" \
  "$(bet_body $ROLLBACK_ID ROLLBACK 10.00 ",\"referenceExternalTransactionId\":\"$WIN_ID\"")" | python3 -m json.tool

echo "== provider-b cannot read provider-a transactions"
curl -sS -o /dev/null -w "HTTP %{http_code}\n" \
  "$API_URL/providers/provider-a/wagering/transactions/$BET_ID" \
  -H "Authorization: Bearer $PROVIDER_B"

echo "== ledger (first page)"
curl -fsS "$API_URL/wallets/$WALLET_ID/ledger?limit=10" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" | python3 -m json.tool

echo "== reconciliation"
curl -fsS -X POST "$API_URL/wallets/$WALLET_ID/reconciliation" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" | python3 -m json.tool

echo "== SQS integration events (requires docker compose; may be empty if already consumed)"
if command -v docker >/dev/null 2>&1; then
  docker exec junglegaming-wager-localstack-1 awslocal sqs receive-message \
    --queue-url http://localhost:4566/000000000000/wager-events.fifo \
    --max-number-of-messages 5 2>/dev/null \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);[print(json.loads(m["Body"])["eventType"], json.loads(m["Body"])["eventId"]) for m in d.get("Messages",[])]' || true
fi

echo "== done"
