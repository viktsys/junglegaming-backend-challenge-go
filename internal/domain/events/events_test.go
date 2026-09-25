package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/ledger"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

func TestWagerTransactionProcessedEnvelope(t *testing.T) {
	aggregateID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()

	envelope, err := NewWagerTransactionProcessed(aggregateID, "corr-1", "cause-1", now,
		WagerTransactionProcessedData{
			TransactionID: aggregateID.String(),
			WalletID:      uuid.Must(uuid.NewV7()).String(),
			PlayerID:      uuid.Must(uuid.NewV7()).String(),
			Kind:          "BET",
			Status:        "PROCESSED",
			Money:         money.MustParse("25.00", "BRL"),
			Balance:       money.MustParse("75.00", "BRL"),
		})
	require.NoError(t, err)
	assert.Equal(t, TypeWagerTransactionProcessed, envelope.EventType)
	assert.Equal(t, SchemaVersion, envelope.Version)
	assert.NotEmpty(t, envelope.EventID)
	assert.Equal(t, aggregateID.String(), envelope.AggregateID)
	assert.Equal(t, "corr-1", envelope.CorrelationID)
	assert.Equal(t, "cause-1", envelope.CausationID)

	parsedID, err := ParseEventID(mustMarshal(t, envelope))
	require.NoError(t, err)
	assert.Equal(t, envelope.EventID, parsedID.String())
}

func TestWalletBalanceChangedPayload(t *testing.T) {
	walletID := uuid.Must(uuid.NewV7())
	transactionID := uuid.Must(uuid.NewV7())
	envelope, err := NewWalletBalanceChanged(walletID, "corr-1", transactionID.String(), time.Now().UTC(),
		WalletBalanceChangedData{
			WalletID:      walletID.String(),
			TransactionID: transactionID.String(),
			Direction:     ledger.DirectionDebit,
			Money:         money.MustParse("80.00", "BRL"),
			BalanceBefore: money.MustParse("100.00", "BRL"),
			BalanceAfter:  money.MustParse("20.00", "BRL"),
			WalletVersion: 2,
		})
	require.NoError(t, err)
	assert.Equal(t, TypeWalletBalanceChanged, envelope.EventType)

	raw := mustMarshal(t, envelope)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	data := decoded["data"].(map[string]any)
	assert.Equal(t, "DEBIT", data["direction"])
	assert.Equal(t, "80.00", data["money"].(map[string]any)["amount"])
	assert.Equal(t, "BRL", data["money"].(map[string]any)["currency"])
	assert.Equal(t, float64(2), data["walletVersion"])
}

func TestEventConstructorsValidate(t *testing.T) {
	now := time.Now().UTC()
	aggregateID := uuid.Must(uuid.NewV7())

	_, err := NewWagerTransactionProcessed(uuid.Nil, "corr", "", now, WagerTransactionProcessedData{
		TransactionID: "tx",
		WalletID:      "wallet",
		PlayerID:      "player",
		Kind:          "BET",
		Money:         money.MustParse("1.00", "BRL"),
		Balance:       money.MustParse("1.00", "BRL"),
	})
	require.Error(t, err)
	assert.Equal(t, CodeInvalidEvent, apperr.CodeOf(err))

	_, err = NewWagerTransactionProcessed(aggregateID, "", "", now, WagerTransactionProcessedData{
		TransactionID: "tx",
		WalletID:      "wallet",
		PlayerID:      "player",
		Kind:          "BET",
		Money:         money.MustParse("1.00", "BRL"),
		Balance:       money.MustParse("1.00", "BRL"),
	})
	require.Error(t, err)

	_, err = NewWalletBalanceChanged(aggregateID, "corr", "", now, WalletBalanceChangedData{
		WalletID:      "wallet",
		TransactionID: "tx",
		Direction:     "SIDEWAYS",
		Money:         money.MustParse("1.00", "BRL"),
		BalanceBefore: money.MustParse("1.00", "BRL"),
		BalanceAfter:  money.MustParse("2.00", "BRL"),
		WalletVersion: 2,
	})
	require.Error(t, err)

	_, err = NewWagerTransactionRejected(aggregateID, "corr", "", now, WagerTransactionRejectedData{
		TransactionID: "tx",
		WalletID:      "wallet",
		FailureCode:   "",
		Money:         money.MustParse("1.00", "BRL"),
	})
	require.Error(t, err)

	_, err = NewWagerTransactionPendingReference(aggregateID, "corr", "", now, WagerTransactionPendingReferenceData{
		TransactionID: "tx",
	})
	require.Error(t, err)
}

func mustMarshal[T any](t *testing.T, envelope Envelope[T]) []byte {
	t.Helper()
	raw, err := envelope.Marshal()
	require.NoError(t, err)
	return raw
}
