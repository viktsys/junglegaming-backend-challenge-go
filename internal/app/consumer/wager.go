// Package consumer implements the application handler for wager messages
// consumed from SQS. The inbox record, the domain changes and the inbox
// completion share one database transaction.
package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/app/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

const (
	// ConsumerName identifies this consumer in the inbox table.
	ConsumerName = "wager-transactions-consumer"
	// RequestEventType is the only accepted inbound event type.
	RequestEventType = "WagerTransactionRequested"
)

// WagerHandler turns SQS messages into wager operations with durable dedup.
type WagerHandler struct {
	tx      ports.TxManager
	inbox   ports.InboxRepository
	wager   *wager.Service
	clock   ports.Clock
	logger  *slog.Logger
	metrics ports.Metrics
}

// NewWagerHandler builds the handler.
func NewWagerHandler(
	tx ports.TxManager,
	inbox ports.InboxRepository,
	service *wager.Service,
	clock ports.Clock,
	metrics ports.Metrics,
	logger *slog.Logger,
) *WagerHandler {
	return &WagerHandler{
		tx:      tx,
		inbox:   inbox,
		wager:   service,
		clock:   clock,
		logger:  logger,
		metrics: metrics,
	}
}

type requestEnvelope struct {
	MessageID  string      `json:"messageId"`
	Type       string      `json:"type"`
	OccurredAt time.Time   `json:"occurredAt"`
	Data       requestData `json:"data"`
}

type requestData struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	IdempotencyKey                 string      `json:"idempotencyKey"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
}

// Handle processes one message. It returns nil when the outcome is durably
// recorded (including business rejections), a PermanentError for unprocessable
// messages and any other error for transient failures.
func (h *WagerHandler) Handle(ctx context.Context, message ports.ReceivedMessage) error {
	envelope, cmd, err := parseRequest(message)
	if err != nil {
		return ports.Permanent(err)
	}
	if err := cmd.Validate(); err != nil {
		return ports.Permanent(err)
	}

	payloadHash := hashBody(message.Body)
	now := h.clock()

	err = h.tx.WithinTransaction(ctx, func(ctx context.Context) error {
		claimed, existing, err := h.inbox.Claim(ctx, ConsumerName, envelope.MessageID, payloadHash, now)
		if err != nil {
			return err
		}
		if !claimed {
			if existing.PayloadHash != payloadHash {
				return ports.Permanent(apperr.New(apperr.KindConflict, "MESSAGE_HASH_MISMATCH",
					"message id was redelivered with a different body"))
			}
			h.logger.Info("duplicate message ignored",
				slog.String("messageId", envelope.MessageID),
				slog.String("providerId", cmd.ProviderID),
				slog.String("externalTransactionId", cmd.ExternalTransactionID))
			return nil
		}

		result, err := h.wager.Process(ctx, cmd)
		if err != nil {
			return classify(err)
		}
		if err := h.inbox.Complete(ctx, ConsumerName, envelope.MessageID, h.clock()); err != nil {
			return err
		}

		h.logger.Info("sqs operation processed",
			slog.String("messageId", envelope.MessageID),
			slog.String("providerId", cmd.ProviderID),
			slog.String("externalTransactionId", cmd.ExternalTransactionID),
			slog.String("transactionId", result.Transaction.ID().String()),
			slog.String("walletId", cmd.WalletID.String()),
			slog.String("status", string(result.Status)))
		return nil
	})
	return err
}

func parseRequest(message ports.ReceivedMessage) (requestEnvelope, wager.ProcessCommand, error) {
	var envelope requestEnvelope
	if err := json.Unmarshal([]byte(message.Body), &envelope); err != nil {
		return envelope, wager.ProcessCommand{}, apperr.Wrap(apperr.KindInvalid, "INVALID_MESSAGE",
			"message body is not valid JSON", err)
	}
	if envelope.Type != RequestEventType {
		return envelope, wager.ProcessCommand{}, apperr.New(apperr.KindInvalid, "UNSUPPORTED_EVENT_TYPE",
			fmt.Sprintf("event type %q is not supported", envelope.Type))
	}
	if envelope.MessageID == "" {
		envelope.MessageID = message.MessageID
	}
	if envelope.MessageID == "" {
		return envelope, wager.ProcessCommand{}, apperr.New(apperr.KindInvalid, "INVALID_MESSAGE",
			"messageId is required")
	}

	data := envelope.Data
	playerID, err := uuid.Parse(data.PlayerID)
	if err != nil {
		return envelope, wager.ProcessCommand{}, apperr.Wrap(apperr.KindInvalid, "INVALID_PLAYER_ID",
			"playerId must be a UUID", err)
	}
	walletID, err := uuid.Parse(data.WalletID)
	if err != nil {
		return envelope, wager.ProcessCommand{}, apperr.Wrap(apperr.KindInvalid, "INVALID_WALLET_ID",
			"walletId must be a UUID", err)
	}
	kind, err := wagering.ParseKind(data.Kind)
	if err != nil {
		return envelope, wager.ProcessCommand{}, err
	}

	return envelope, wager.ProcessCommand{
		ProviderID:                     data.ProviderID,
		ExternalTransactionID:          data.ExternalTransactionID,
		IdempotencyKey:                 data.IdempotencyKey,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        data.RoundID,
		GameID:                         data.GameID,
		Kind:                           kind,
		Money:                          data.Money,
		ReferenceExternalTransactionID: data.ReferenceExternalTransactionID,
		CorrelationID:                  envelope.MessageID,
	}, nil
}

// classify maps classified application errors to permanent or transient
// failures. Invalid input and idempotency conflicts go to the DLQ; everything
// else is retried.
func classify(err error) error {
	if ports.IsPermanent(err) {
		return err
	}
	switch apperr.KindOf(err) {
	case apperr.KindInvalid, apperr.KindForbidden, apperr.KindUnauthorized:
		return ports.Permanent(err)
	case apperr.KindRejected:
		// A rejection that could not be persisted (for example an unknown
		// wallet) is unprocessable as-is.
		return ports.Permanent(err)
	}
	if apperr.CodeOf(err) == "IDEMPOTENCY_CONFLICT" {
		return ports.Permanent(err)
	}
	return err
}

func hashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
