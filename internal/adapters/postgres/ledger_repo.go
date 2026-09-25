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
	"github.com/junglegaming/backend-challenge-go/internal/domain/ledger"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// LedgerRepository is the pgx implementation of ports.LedgerRepository.
type LedgerRepository struct {
	pool *pgxpool.Pool
}

// NewLedgerRepository builds the repository.
func NewLedgerRepository(pool *pgxpool.Pool) *LedgerRepository {
	return &LedgerRepository{pool: pool}
}

const ledgerColumns = `id, wallet_id, transaction_id, direction, amount_minor, currency,
	balance_before_minor, balance_after_minor, created_at`

// Insert appends an entry. Updates and deletes are rejected by a trigger.
func (r *LedgerRepository) Insert(ctx context.Context, entry *ledger.Entry) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, direction, amount_minor, currency,
			balance_before_minor, balance_after_minor, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		entry.ID(), entry.WalletID(), entry.TransactionID(), string(entry.Direction()),
		entry.Amount().MinorUnits(), entry.Amount().Currency().String(),
		entry.BalanceBefore().MinorUnits(), entry.BalanceAfter().MinorUnits(), entry.CreatedAt())
	if err != nil {
		if isUniqueViolation(err, constraintLedgerUnique) {
			return ports.ErrDuplicateLedgerEntry
		}
		return fmt.Errorf("postgres: insert ledger entry: %w", err)
	}
	return nil
}

// ListByWallet returns a keyset page ordered from newest to oldest.
func (r *LedgerRepository) ListByWallet(ctx context.Context, walletID uuid.UUID, cursor *ports.LedgerCursor, limit int) (ports.LedgerPage, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := querierFrom(ctx, r.pool)

	var (
		rows pgx.Rows
		err  error
	)
	if cursor == nil {
		rows, err = q.Query(ctx, `
			SELECT `+ledgerColumns+` FROM wallet_ledger_entries
			WHERE wallet_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $2`, walletID, limit+1)
	} else {
		rows, err = q.Query(ctx, `
			SELECT `+ledgerColumns+` FROM wallet_ledger_entries
			WHERE wallet_id = $1 AND (created_at, id) < ($2, $3)
			ORDER BY created_at DESC, id DESC
			LIMIT $4`, walletID, cursor.CreatedAt, cursor.ID, limit+1)
	}
	if err != nil {
		return ports.LedgerPage{}, fmt.Errorf("postgres: list ledger: %w", err)
	}
	defer rows.Close()

	entries := make([]*ledger.Entry, 0, limit+1)
	for rows.Next() {
		entry, err := scanLedgerEntry(rows)
		if err != nil {
			return ports.LedgerPage{}, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return ports.LedgerPage{}, fmt.Errorf("postgres: iterate ledger: %w", err)
	}

	page := ports.LedgerPage{Entries: entries}
	if len(entries) > limit {
		page.HasMore = true
		page.Entries = entries[:limit]
	}
	return page, nil
}

// TotalsByWallet returns the stored balance and the ledger aggregate from a
// single statement, which guarantees a consistent reconciliation snapshot.
func (r *LedgerRepository) TotalsByWallet(ctx context.Context, walletID uuid.UUID) (ports.LedgerTotals, error) {
	var (
		totals   ports.LedgerTotals
		currency string
	)
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT
			w.currency,
			w.balance_minor,
			COALESCE(SUM(CASE WHEN l.direction = 'CREDIT' THEN l.amount_minor ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN l.direction = 'DEBIT' THEN l.amount_minor ELSE 0 END), 0),
			COUNT(l.id)
		FROM wallets w
		LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
		WHERE w.id = $1
		GROUP BY w.id, w.currency, w.balance_minor`, walletID).
		Scan(&currency, &totals.StoredBalanceMinor, &totals.CreditsMinor, &totals.DebitsMinor, &totals.Entries)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.LedgerTotals{}, apperr.New(apperr.KindNotFound, "WALLET_NOT_FOUND", "wallet was not found")
		}
		return ports.LedgerTotals{}, fmt.Errorf("postgres: ledger totals: %w", err)
	}
	cur, err := money.NewCurrency(currency)
	if err != nil {
		return ports.LedgerTotals{}, err
	}
	totals.Currency = cur
	return totals, nil
}

func scanLedgerEntry(row pgx.Row) (*ledger.Entry, error) {
	var (
		id, walletID, transactionID uuid.UUID
		direction                   string
		amountMinor                 int64
		currency                    string
		balanceBeforeMinor          int64
		balanceAfterMinor           int64
		createdAt                   time.Time
	)
	if err := row.Scan(&id, &walletID, &transactionID, &direction, &amountMinor, &currency,
		&balanceBeforeMinor, &balanceAfterMinor, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("postgres: scan ledger entry: %w", err)
	}
	cur, err := money.NewCurrency(currency)
	if err != nil {
		return nil, err
	}
	amount, err := money.FromMinorUnits(amountMinor, cur)
	if err != nil {
		return nil, err
	}
	before, err := money.FromMinorUnits(balanceBeforeMinor, cur)
	if err != nil {
		return nil, err
	}
	after, err := money.FromMinorUnits(balanceAfterMinor, cur)
	if err != nil {
		return nil, err
	}
	parsedDirection, err := ledger.ParseDirection(direction)
	if err != nil {
		return nil, err
	}
	return ledger.Rehydrate(id, walletID, transactionID, parsedDirection, amount, before, after, createdAt)
}
