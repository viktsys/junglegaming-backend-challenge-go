// Package ports defines the interfaces the application layer depends on. The
// domain and application packages never import infrastructure packages.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/ledger"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

// Sentinel errors returned by repositories for constraint conflicts.
var (
	ErrDuplicateExternalTransaction = errors.New("repository: duplicate provider/external transaction")
	ErrDuplicateIdempotencyKey      = errors.New("repository: idempotency key already used")
	ErrDuplicateOpening             = errors.New("repository: wallet already has an opening credit")
	ErrDuplicateReversal            = errors.New("repository: reference already has a successful reversal")
	ErrDuplicateLedgerEntry         = errors.New("repository: ledger entry already exists")
)

// Clock returns the current instant. Injected so tests can control time.
type Clock func() time.Time

// TxManager runs a function inside a database transaction. Nested calls join a
// savepoint of the outer transaction, which lets callers compose atomic units.
type TxManager interface {
	WithinTransaction(ctx context.Context, fn func(ctx context.Context) error) error
}

// WalletRepository persists wallets with optimistic concurrency control.
type WalletRepository interface {
	Create(ctx context.Context, w *wallet.Wallet) error
	GetByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	GetByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency money.Currency) (*wallet.Wallet, error)
	// UpdateBalance applies the wallet balance and increments its version only
	// when the persisted version still equals expectedVersion.
	UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error
}

// TransactionRepository persists wager transactions and supports worker claims.
type TransactionRepository interface {
	Insert(ctx context.Context, t *wagering.Transaction) error
	Update(ctx context.Context, t *wagering.Transaction) error
	GetByID(ctx context.Context, id uuid.UUID) (*wagering.Transaction, error)
	GetByProviderExternal(ctx context.Context, providerID, externalID string) (*wagering.Transaction, error)
	GetByProviderIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.Transaction, error)
	// ClaimOne locks and returns the oldest due PENDING/PENDING_REFERENCE row.
	ClaimOne(ctx context.Context, now time.Time) (*wagering.Transaction, error)
	// CountPending returns how many transactions are PENDING or PENDING_REFERENCE.
	CountPending(ctx context.Context) (int64, error)
	// FindReversal returns a PROCESSED reversal of the given kind for a reference.
	FindReversal(ctx context.Context, referenceID uuid.UUID, kind wagering.Kind) (*wagering.Transaction, error)
	// WakeDependents schedules immediate retry for operations waiting on the
	// given external transaction.
	WakeDependents(ctx context.Context, providerID, externalID string, now time.Time) (int64, error)
}

// LedgerCursor is the keyset position of a ledger page.
type LedgerCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// LedgerPage is one page of ledger entries ordered from newest to oldest.
type LedgerPage struct {
	Entries []*ledger.Entry
	HasMore bool
}

// LedgerRepository persists append-only entries and supports reconciliation.
type LedgerRepository interface {
	Insert(ctx context.Context, entry *ledger.Entry) error
	ListByWallet(ctx context.Context, walletID uuid.UUID, cursor *LedgerCursor, limit int) (LedgerPage, error)
	// TotalsByWallet returns the aggregate credits and debits of a wallet.
	TotalsByWallet(ctx context.Context, walletID uuid.UUID) (LedgerTotals, error)
}

// LedgerTotals is a consistent snapshot of a wallet and its ledger aggregate.
// Both values come from a single SQL statement so reconciliation cannot observe
// a movement between reading the wallet and summing the ledger.
type LedgerTotals struct {
	Currency           money.Currency
	StoredBalanceMinor int64
	CreditsMinor       int64
	DebitsMinor        int64
	Entries            int64
}

// InboxRecord is the persisted deduplication record of a consumed message.
type InboxRecord struct {
	MessageID   string
	PayloadHash string
	Completed   bool
	ReceivedAt  time.Time
}

// InboxRepository implements consumer-side deduplication.
type InboxRepository interface {
	// Claim inserts a RECEIVED record; claimed is false when the message was
	// already seen. It never overwrites an existing record.
	Claim(ctx context.Context, consumerName, messageID, payloadHash string, now time.Time) (claimed bool, existing *InboxRecord, err error)
	Complete(ctx context.Context, consumerName, messageID string, now time.Time) error
}

// OutboxEvent is a pending integration event stored transactionally.
type OutboxEvent struct {
	EventID       uuid.UUID
	AggregateID   uuid.UUID
	EventType     string
	Payload       []byte
	OccurredAt    time.Time
	Attempts      int
	NextAttemptAt time.Time
}

// OutboxRepository stores and claims outbox events for publication.
type OutboxRepository interface {
	Insert(ctx context.Context, event OutboxEvent) error
	// Claim locks up to limit publishable events, respecting per-aggregate
	// order, and marks them as owned by publisherID.
	Claim(ctx context.Context, publisherID string, now time.Time, limit int, lockTTL time.Duration) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID, now time.Time) error
	MarkFailed(ctx context.Context, eventID uuid.UUID, nextAttempt time.Time, lastError string, now time.Time) error
	// PendingCount returns how many events are still unpublished.
	PendingCount(ctx context.Context) (int64, error)
}

// EventPublisher delivers an outbox event to the broker.
type EventPublisher interface {
	Publish(ctx context.Context, event OutboxEvent) error
}

// ReceivedMessage is a broker message handed to the application handler.
type ReceivedMessage struct {
	MessageID     string
	ReceiptHandle string
	Body          string
	ReceiveCount  int
}

// MessageHandler processes one broker message. Returning nil acknowledges the
// message; a PermanentError routes it to the dead-letter queue; any other
// error leaves it for redelivery.
type MessageHandler interface {
	Handle(ctx context.Context, message ReceivedMessage) error
}

// PermanentError marks a message as unprocessable.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying cause.
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps an error as unprocessable.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the message should be sent to the DLQ.
func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

// QueueHealth reports whether the broker is reachable.
type QueueHealth interface {
	Ping(ctx context.Context) error
}

// DatabaseHealth reports whether PostgreSQL is reachable.
type DatabaseHealth interface {
	Ping(ctx context.Context) error
}

// Metrics records application-level counters and gauges.
type Metrics interface {
	RecordWagerTransaction(status, kind, failureCode string)
	RecordIdempotentReplay()
	RecordWalletVersionConflict()
	RecordWorkerRetry(worker string)
	RecordDLQMessage(queue string)
	RecordReconciliationRun(consistent bool)
	RecordOutboxPublished(attempts int, latency time.Duration)
	SetOutboxPending(count int64)
	SetPendingReferences(count int64)
}
