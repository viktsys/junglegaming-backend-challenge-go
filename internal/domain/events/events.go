// Package events defines the integration event contracts published through the
// transactional outbox. Envelope type and version are fixed by constructors so
// that a payload snapshot is immutable once stored.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/ledger"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

// Event type names.
const (
	TypeWagerTransactionProcessed        = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         = "WagerTransactionRejected"
	TypeWalletBalanceChanged             = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// SchemaVersion is the version of every event contract defined in this package.
const SchemaVersion = 1

// CodeInvalidEvent is returned when an event cannot be constructed.
const CodeInvalidEvent = "INVALID_EVENT"

// Envelope is the common integration event wrapper.
type Envelope[T any] struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          T         `json:"data"`
}

// WagerTransactionProcessedData describes a successful operation.
type WagerTransactionProcessedData struct {
	TransactionID         string      `json:"transactionId"`
	ProviderID            string      `json:"providerId,omitempty"`
	ExternalTransactionID string      `json:"externalTransactionId,omitempty"`
	WalletID              string      `json:"walletId"`
	PlayerID              string      `json:"playerId"`
	RoundID               string      `json:"roundId,omitempty"`
	GameID                string      `json:"gameId,omitempty"`
	Kind                  string      `json:"kind"`
	Status                string      `json:"status"`
	Money                 money.Money `json:"money"`
	Balance               money.Money `json:"balance"`
}

// WagerTransactionRejectedData describes a definitive business rejection.
type WagerTransactionRejectedData struct {
	TransactionID         string      `json:"transactionId"`
	ProviderID            string      `json:"providerId,omitempty"`
	ExternalTransactionID string      `json:"externalTransactionId,omitempty"`
	WalletID              string      `json:"walletId"`
	PlayerID              string      `json:"playerId"`
	Kind                  string      `json:"kind"`
	Status                string      `json:"status"`
	FailureCode           string      `json:"failureCode"`
	Money                 money.Money `json:"money"`
}

// WalletBalanceChangedData describes an effective balance movement.
type WalletBalanceChangedData struct {
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     ledger.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

// WagerTransactionPendingReferenceData describes a transaction waiting for a reference.
type WagerTransactionPendingReferenceData struct {
	TransactionID                  string    `json:"transactionId"`
	ProviderID                     string    `json:"providerId,omitempty"`
	ExternalTransactionID          string    `json:"externalTransactionId,omitempty"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId"`
	Attempt                        int       `json:"attempt"`
	NextAttemptAt                  time.Time `json:"nextAttemptAt"`
}

// NewWagerTransactionProcessed builds the event emitted when an operation
// completes successfully, including LOSS.
func NewWagerTransactionProcessed(
	aggregateID uuid.UUID,
	correlationID, causationID string,
	occurredAt time.Time,
	data WagerTransactionProcessedData,
) (Envelope[WagerTransactionProcessedData], error) {
	if data.TransactionID == "" || data.WalletID == "" || data.PlayerID == "" || data.Kind == "" {
		return Envelope[WagerTransactionProcessedData]{}, invalid("processed event requires transaction, wallet, player and kind")
	}
	if !data.Money.IsValid() || !data.Balance.IsValid() {
		return Envelope[WagerTransactionProcessedData]{}, invalid("processed event requires money and balance")
	}
	return newEnvelope(TypeWagerTransactionProcessed, aggregateID, correlationID, causationID, occurredAt, data)
}

// NewWagerTransactionRejected builds the event emitted on definitive rejection.
func NewWagerTransactionRejected(
	aggregateID uuid.UUID,
	correlationID, causationID string,
	occurredAt time.Time,
	data WagerTransactionRejectedData,
) (Envelope[WagerTransactionRejectedData], error) {
	if data.TransactionID == "" || data.WalletID == "" || data.FailureCode == "" {
		return Envelope[WagerTransactionRejectedData]{}, invalid("rejected event requires transaction, wallet and failure code")
	}
	if !data.Money.IsValid() {
		return Envelope[WagerTransactionRejectedData]{}, invalid("rejected event requires money")
	}
	return newEnvelope(TypeWagerTransactionRejected, aggregateID, correlationID, causationID, occurredAt, data)
}

// NewWalletBalanceChanged builds the event emitted on an effective balance change.
func NewWalletBalanceChanged(
	aggregateID uuid.UUID,
	correlationID, causationID string,
	occurredAt time.Time,
	data WalletBalanceChangedData,
) (Envelope[WalletBalanceChangedData], error) {
	if data.WalletID == "" || data.TransactionID == "" {
		return Envelope[WalletBalanceChangedData]{}, invalid("balance changed event requires wallet and transaction")
	}
	if data.Direction != ledger.DirectionDebit && data.Direction != ledger.DirectionCredit {
		return Envelope[WalletBalanceChangedData]{}, invalid("balance changed event requires a direction")
	}
	if !data.Money.IsValid() || !data.BalanceBefore.IsValid() || !data.BalanceAfter.IsValid() {
		return Envelope[WalletBalanceChangedData]{}, invalid("balance changed event requires all amounts")
	}
	if data.WalletVersion < 1 {
		return Envelope[WalletBalanceChangedData]{}, invalid("balance changed event requires wallet version >= 1")
	}
	return newEnvelope(TypeWalletBalanceChanged, aggregateID, correlationID, causationID, occurredAt, data)
}

// NewWagerTransactionPendingReference builds the event emitted when a
// transaction starts waiting for its reference.
func NewWagerTransactionPendingReference(
	aggregateID uuid.UUID,
	correlationID, causationID string,
	occurredAt time.Time,
	data WagerTransactionPendingReferenceData,
) (Envelope[WagerTransactionPendingReferenceData], error) {
	if data.TransactionID == "" || data.ReferenceExternalTransactionID == "" {
		return Envelope[WagerTransactionPendingReferenceData]{}, invalid("pending reference event requires transaction and reference")
	}
	return newEnvelope(TypeWagerTransactionPendingReference, aggregateID, correlationID, causationID, occurredAt, data)
}

func newEnvelope[T any](
	eventType string,
	aggregateID uuid.UUID,
	correlationID, causationID string,
	occurredAt time.Time,
	data T,
) (Envelope[T], error) {
	if aggregateID == uuid.Nil {
		return Envelope[T]{}, invalid("event aggregateId is required")
	}
	if correlationID == "" {
		return Envelope[T]{}, invalid("event correlationId is required")
	}
	if occurredAt.IsZero() {
		return Envelope[T]{}, invalid("event occurredAt is required")
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return Envelope[T]{}, fmt.Errorf("events: cannot generate event id: %w", err)
	}
	return Envelope[T]{
		EventID:       eventID.String(),
		EventType:     eventType,
		AggregateID:   aggregateID.String(),
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    occurredAt.UTC(),
		Version:       SchemaVersion,
		Data:          data,
	}, nil
}

// Marshal serializes the envelope with RFC 3339 timestamps and decimal strings.
func (e Envelope[T]) Marshal() ([]byte, error) {
	return json.Marshal(e)
}

// ParseEventID extracts the event identity from a stored payload.
func ParseEventID(payload []byte) (uuid.UUID, error) {
	var header struct {
		EventID string `json:"eventId"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(header.EventID)
}

func invalid(message string) error {
	return apperr.New(apperr.KindInvalid, CodeInvalidEvent, message)
}
