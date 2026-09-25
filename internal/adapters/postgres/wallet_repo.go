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
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

// WalletRepository is the pgx implementation of ports.WalletRepository.
type WalletRepository struct {
	pool *pgxpool.Pool
}

// NewWalletRepository builds the repository.
func NewWalletRepository(pool *pgxpool.Pool) *WalletRepository {
	return &WalletRepository{pool: pool}
}

const walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

// Create inserts a new wallet, rejecting duplicate (playerId, currency) pairs.
func (r *WalletRepository) Create(ctx context.Context, w *wallet.Wallet) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), w.Currency().String(), w.Balance().MinorUnits(),
		w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		if isUniqueViolation(err, "wallets_player_currency_unique") {
			return apperr.Wrap(apperr.KindConflict, "WALLET_ALREADY_EXISTS",
				"a wallet already exists for this player and currency", err)
		}
		return fmt.Errorf("postgres: insert wallet: %w", err)
	}
	return nil
}

// GetByID loads a wallet by identity.
func (r *WalletRepository) GetByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id)
	return scanWallet(row)
}

// GetByPlayerAndCurrency loads the single wallet of a player in a currency.
func (r *WalletRepository) GetByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency money.Currency) (*wallet.Wallet, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE player_id = $1 AND currency = $2`, playerID, currency.String())
	return scanWallet(row)
}

// UpdateBalance applies the balance and bumps the version only when the stored
// version still matches expectedVersion. Zero rows affected means another
// writer won the race.
func (r *WalletRepository) UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error {
	tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE wallets
		SET balance_minor = $2, version = version + 1, updated_at = $3
		WHERE id = $1 AND version = $4`,
		w.ID(), w.Balance().MinorUnits(), w.UpdatedAt(), expectedVersion)
	if err != nil {
		if isCheckViolation(err) {
			return apperr.Wrap(apperr.KindRejected, wallet.CodeInsufficientFunds,
				"database rejected a negative balance", err)
		}
		return fmt.Errorf("postgres: update wallet balance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.New(apperr.KindConflict, wallet.CodeVersionConflict,
			"wallet was modified concurrently")
	}
	return nil
}

func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		id, playerID uuid.UUID
		currency     string
		balanceMinor int64
		version      int64
		createdAt    time.Time
		updatedAt    time.Time
	)
	if err := row.Scan(&id, &playerID, &currency, &balanceMinor, &version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.New(apperr.KindNotFound, "WALLET_NOT_FOUND", "wallet was not found")
		}
		return nil, fmt.Errorf("postgres: scan wallet: %w", err)
	}
	cur, err := money.NewCurrency(currency)
	if err != nil {
		return nil, err
	}
	return wallet.Rehydrate(id, playerID, cur, balanceMinor, version, createdAt, updatedAt)
}
