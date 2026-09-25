package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// TransactionRepository is the pgx implementation of ports.TransactionRepository.
type TransactionRepository struct {
	pool *pgxpool.Pool
}

// NewTransactionRepository builds the repository.
func NewTransactionRepository(pool *pgxpool.Pool) *TransactionRepository {
	return &TransactionRepository{pool: pool}
}

const transactionColumns = `id, origin, provider_id, external_transaction_id, idempotency_key,
	payload_hash, wallet_id, player_id, round_id, game_id, kind, amount_minor, currency,
	reference_external_transaction_id, reference_transaction_id, status, failure_code,
	result_balance_minor, attempts, next_attempt_at, created_at, updated_at`

// Insert stores a new transaction, classifying unique violations so the
// application can distinguish idempotent replays from conflicts.
func (r *TransactionRepository) Insert(ctx context.Context, t *wagering.Transaction) error {
	referenceID := nullableUUID(t)
	var resultBalance any
	if balance, ok := t.ResultBalance(); ok {
		resultBalance = balance.MinorUnits()
	}
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO wager_transactions (
			id, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, amount_minor, currency,
			reference_external_transaction_id, reference_transaction_id, status, failure_code,
			result_balance_minor, attempts, next_attempt_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22
		)`,
		t.ID(), string(t.Origin()), nullableString(t.ProviderID()), nullableString(t.ExternalTransactionID()),
		nullableString(t.IdempotencyKey()), nullableString(t.PayloadHash()), t.WalletID(), t.PlayerID(),
		nullableString(t.RoundID()), nullableString(t.GameID()), string(t.Kind()), t.Money().MinorUnits(),
		t.Money().Currency().String(), nullableString(t.ReferenceExternalTransactionID()), referenceID,
		string(t.Status()), nullableString(t.FailureCode()), resultBalance, t.Attempts(),
		nullableTime(t.NextAttemptAt()), t.CreatedAt(), t.UpdatedAt())
	if err != nil {
		switch constraintName(err) {
		case constraintProviderExternal:
			return ports.ErrDuplicateExternalTransaction
		case constraintProviderIdemKey:
			return ports.ErrDuplicateIdempotencyKey
		case constraintOpeningUnique:
			return ports.ErrDuplicateOpening
		case constraintReversalUnique:
			return ports.ErrDuplicateReversal
		}
		return fmt.Errorf("postgres: insert wager transaction: %w", err)
	}
	return nil
}

// Update persists state transitions. Terminal rows are never overwritten.
func (r *TransactionRepository) Update(ctx context.Context, t *wagering.Transaction) error {
	referenceID := nullableUUID(t)
	var resultBalance any
	if balance, ok := t.ResultBalance(); ok {
		resultBalance = balance.MinorUnits()
	}
	tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE wager_transactions SET
			reference_transaction_id = $2,
			status = $3,
			failure_code = $4,
			result_balance_minor = $5,
			attempts = $6,
			next_attempt_at = $7,
			updated_at = $8
		WHERE id = $1 AND status IN ('PENDING', 'PENDING_REFERENCE')`,
		t.ID(), referenceID, string(t.Status()), nullableString(t.FailureCode()), resultBalance,
		t.Attempts(), nullableTime(t.NextAttemptAt()), t.UpdatedAt())
	if err != nil {
		if isUniqueViolation(err, constraintReversalUnique) {
			return ports.ErrDuplicateReversal
		}
		return fmt.Errorf("postgres: update wager transaction: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.New(apperr.KindConflict, wagering.CodeTerminalState,
			"transaction is terminal or was already finalized")
	}
	return nil
}

// GetByID loads a transaction by internal identity.
func (r *TransactionRepository) GetByID(ctx context.Context, id uuid.UUID) (*wagering.Transaction, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1`, id)
	return scanTransaction(row)
}

// GetByProviderExternal loads a transaction by (providerId, externalTransactionId).
func (r *TransactionRepository) GetByProviderExternal(ctx context.Context, providerID, externalID string) (*wagering.Transaction, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions
		 WHERE provider_id = $1 AND external_transaction_id = $2 AND origin = 'EXTERNAL'`,
		providerID, externalID)
	return scanTransaction(row)
}

// GetByProviderIdempotencyKey loads a transaction by (providerId, idempotencyKey).
func (r *TransactionRepository) GetByProviderIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.Transaction, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions
		 WHERE provider_id = $1 AND idempotency_key = $2 AND origin = 'EXTERNAL'`,
		providerID, key)
	return scanTransaction(row)
}

// ClaimOne locks and returns the oldest due pending transaction.
func (r *TransactionRepository) ClaimOne(ctx context.Context, now time.Time) (*wagering.Transaction, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT `+transactionColumns+` FROM wager_transactions
		WHERE status IN ('PENDING', 'PENDING_REFERENCE')
		  AND next_attempt_at IS NOT NULL
		  AND next_attempt_at <= $1
		ORDER BY next_attempt_at ASC, created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, now)
	t, err := scanTransaction(row)
	if err != nil {
		if apperr.CodeOf(err) == "TRANSACTION_NOT_FOUND" {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// CountPending returns how many transactions are waiting for processing.
func (r *TransactionRepository) CountPending(ctx context.Context) (int64, error) {
	var count int64
	err := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT COUNT(*) FROM wager_transactions WHERE status IN ('PENDING', 'PENDING_REFERENCE')`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("postgres: count pending transactions: %w", err)
	}
	return count, nil
}

// FindReversal returns a PROCESSED reversal of the given kind, or nil.
func (r *TransactionRepository) FindReversal(ctx context.Context, referenceID uuid.UUID, kind wagering.Kind) (*wagering.Transaction, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions
		 WHERE reference_transaction_id = $1 AND kind = $2 AND status = 'PROCESSED'
		 LIMIT 1`, referenceID, string(kind))
	t, err := scanTransaction(row)
	if err != nil {
		if apperr.CodeOf(err) == "TRANSACTION_NOT_FOUND" {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// WakeDependents makes pending references immediately eligible for retry.
func (r *TransactionRepository) WakeDependents(ctx context.Context, providerID, externalID string, now time.Time) (int64, error) {
	tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE wager_transactions
		SET next_attempt_at = $3, updated_at = $3
		WHERE provider_id = $1 AND reference_external_transaction_id = $2
		  AND status = 'PENDING_REFERENCE'`,
		providerID, externalID, now)
	if err != nil {
		return 0, fmt.Errorf("postgres: wake dependents: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanTransaction(row pgx.Row) (*wagering.Transaction, error) {
	var (
		id                  uuid.UUID
		origin              string
		providerID          *string
		externalID          *string
		idempotencyKey      *string
		payloadHash         *string
		walletID            uuid.UUID
		playerID            uuid.UUID
		roundID             *string
		gameID              *string
		kind                string
		amountMinor         int64
		currency            string
		referenceExternalID *string
		referenceID         *uuid.UUID
		status              string
		failureCode         *string
		resultBalanceMinor  *int64
		attempts            int
		nextAttemptAt       *time.Time
		createdAt           time.Time
		updatedAt           time.Time
	)
	if err := row.Scan(
		&id, &origin, &providerID, &externalID, &idempotencyKey, &payloadHash,
		&walletID, &playerID, &roundID, &gameID, &kind, &amountMinor, &currency,
		&referenceExternalID, &referenceID, &status, &failureCode,
		&resultBalanceMinor, &attempts, &nextAttemptAt, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.New(apperr.KindNotFound, "TRANSACTION_NOT_FOUND", "transaction was not found")
		}
		return nil, fmt.Errorf("postgres: scan wager transaction: %w", err)
	}

	cur, err := money.NewCurrency(currency)
	if err != nil {
		return nil, err
	}
	amount, err := money.FromMinorUnits(amountMinor, cur)
	if err != nil {
		return nil, err
	}
	var resultBalance *money.Money
	if resultBalanceMinor != nil {
		balance, err := money.FromMinorUnits(*resultBalanceMinor, cur)
		if err != nil {
			return nil, err
		}
		resultBalance = &balance
	}
	parsedKind, err := wagering.ParseKind(kind)
	if err != nil {
		return nil, err
	}
	parsedStatus, err := wagering.ParseStatus(status)
	if err != nil {
		return nil, err
	}

	return wagering.Rehydrate(wagering.RehydrateParams{
		ID:                             id,
		Origin:                         wagering.Origin(origin),
		ProviderID:                     derefString(providerID),
		ExternalTransactionID:          derefString(externalID),
		IdempotencyKey:                 derefString(idempotencyKey),
		PayloadHash:                    derefString(payloadHash),
		WalletID:                       walletID,
		PlayerID:                       playerID,
		RoundID:                        derefString(roundID),
		GameID:                         derefString(gameID),
		Kind:                           parsedKind,
		Money:                          amount,
		ReferenceExternalTransactionID: derefString(referenceExternalID),
		ReferenceTransactionID:         referenceID,
		Status:                         parsedStatus,
		FailureCode:                    derefString(failureCode),
		ResultBalance:                  resultBalance,
		Attempts:                       attempts,
		NextAttemptAt:                  nextAttemptAt,
		CreatedAt:                      createdAt,
		UpdatedAt:                      updatedAt,
	})
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableUUID(t *wagering.Transaction) any {
	if id, ok := t.ReferenceTransactionID(); ok {
		return id
	}
	return nil
}

func nullableTime(value time.Time, ok bool) any {
	if !ok || value.IsZero() {
		return nil
	}
	return value
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
