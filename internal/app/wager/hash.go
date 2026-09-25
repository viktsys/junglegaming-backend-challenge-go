// Package wager implements the use case that processes wager operations with
// persistent idempotency, optimistic wallet concurrency control and
// transactional outbox events.
package wager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ComputePayloadHash returns the SHA-256 of the canonical JSON representation
// of the business fields of a command.
//
// Canonicalization rules (identical for HTTP and SQS):
//   - keys are sorted lexicographically (Go map marshaling);
//   - amounts are normalized to the fixed-scale decimal string and the
//     currency to its ISO 4217 code;
//   - UUIDs are lower-case canonical strings;
//   - referenceExternalTransactionId is always present (empty when absent);
//   - the idempotency key and all transport metadata (headers, timestamps,
//     message ids, correlation ids) are excluded.
func ComputePayloadHash(cmd ProcessCommand) string {
	canonical := map[string]any{
		"providerId":            cmd.ProviderID,
		"externalTransactionId": cmd.ExternalTransactionID,
		"playerId":              cmd.PlayerID.String(),
		"walletId":              cmd.WalletID.String(),
		"roundId":               cmd.RoundID,
		"gameId":                cmd.GameID,
		"kind":                  string(cmd.Kind),
		"money": map[string]string{
			"amount":   cmd.Money.AmountString(),
			"currency": cmd.Money.Currency().String(),
		},
		"referenceExternalTransactionId": cmd.ReferenceExternalTransactionID,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		// The map only contains strings, so marshaling cannot fail.
		panic("wager: canonical payload cannot be encoded: " + err.Error())
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
