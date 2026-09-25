// Package postgres contains the pgx-based persistence adapters. Every
// repository reads the current transaction from the context, so application
// code composes atomic units through ports.TxManager without importing pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/junglegaming/backend-challenge-go/internal/config"
)

// Constraint names used to classify unique violations.
const (
	constraintProviderExternal = "wager_tx_provider_external_unique"
	constraintProviderIdemKey  = "wager_tx_provider_idempotency_unique"
	constraintOpeningUnique    = "wager_tx_opening_unique"
	constraintReversalUnique   = "wager_tx_reference_reversal_unique"
	constraintLedgerUnique     = "ledger_wallet_transaction_unique"
)

type txContextKey struct{}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// querierFrom returns the transaction stored in ctx, or the pool.
func querierFrom(ctx context.Context, pool *pgxpool.Pool) querier {
	if tx, ok := ctx.Value(txContextKey{}).(pgx.Tx); ok {
		return tx
	}
	return pool
}

// TxFromContext exposes the transaction for tests and adapters.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txContextKey{}).(pgx.Tx)
	return tx, ok
}

// TxManager implements ports.TxManager over a pgx pool. Nested calls create
// savepoints so that inner failures can be retried without discarding the
// outer transaction.
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager builds a transaction manager.
func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
}

// WithinTransaction runs fn inside a transaction (or savepoint when nested).
func (m *TxManager) WithinTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if parent, ok := ctx.Value(txContextKey{}).(pgx.Tx); ok {
		nested, err := parent.Begin(ctx)
		if err != nil {
			return fmt.Errorf("postgres: begin savepoint: %w", err)
		}
		if err := fn(context.WithValue(ctx, txContextKey{}, nested)); err != nil {
			rollbackErr := nested.Rollback(ctx)
			if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				return errors.Join(err, fmt.Errorf("postgres: rollback savepoint: %w", rollbackErr))
			}
			return err
		}
		if err := nested.Commit(ctx); err != nil {
			return fmt.Errorf("postgres: release savepoint: %w", err)
		}
		return nil
	}

	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(context.WithValue(ctx, txContextKey{}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit transaction: %w", err)
	}
	return nil
}

// NewPool opens and verifies a connection pool.
func NewPool(ctx context.Context, cfg config.DatabaseConfig, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	logger.Info("postgres pool ready", slog.Int("max_conns", int(cfg.MaxConns)))
	return pool, nil
}

// Health checks database readiness.
type Health struct {
	pool *pgxpool.Pool
}

// NewHealth builds the readiness probe.
func NewHealth(pool *pgxpool.Pool) *Health { return &Health{pool: pool} }

// Ping verifies the database answers within the context deadline.
func (h *Health) Ping(ctx context.Context) error {
	if err := h.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
