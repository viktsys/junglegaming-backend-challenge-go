// Package ledger implements the append-only WalletLedgerEntry. Each entry
// records one side of a balance movement and validates that balanceAfter
// matches balanceBefore adjusted by the direction and amount.
package ledger

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

// Direction is the sign of a ledger movement.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// ParseDirection validates a persisted direction.
func ParseDirection(raw string) (Direction, error) {
	switch Direction(raw) {
	case DirectionDebit, DirectionCredit:
		return Direction(raw), nil
	default:
		return "", apperr.New(apperr.KindInvalid, CodeInvalidEntry,
			fmt.Sprintf("direction %q is not supported", raw))
	}
}

// Domain error codes.
const (
	CodeInvalidEntry = "INVALID_LEDGER_ENTRY"
	CodeMathMismatch = "LEDGER_BALANCE_MISMATCH"
)

// Entry is an immutable ledger record.
type Entry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// New builds a ledger entry and enforces balanceAfter = balanceBefore ± amount.
func New(
	walletID uuid.UUID,
	transactionID uuid.UUID,
	direction Direction,
	amount money.Money,
	balanceBefore money.Money,
	balanceAfter money.Money,
	now time.Time,
) (*Entry, error) {
	entry := &Entry{
		id:            mustUUIDv7(),
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		amount:        amount,
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		createdAt:     now.UTC(),
	}
	if err := entry.validate(); err != nil {
		return nil, err
	}
	return entry, nil
}

// Rehydrate rebuilds an entry from persisted state.
func Rehydrate(
	id uuid.UUID,
	walletID uuid.UUID,
	transactionID uuid.UUID,
	direction Direction,
	amount money.Money,
	balanceBefore money.Money,
	balanceAfter money.Money,
	createdAt time.Time,
) (*Entry, error) {
	entry := &Entry{
		id:            id,
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		amount:        amount,
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		createdAt:     createdAt.UTC(),
	}
	if err := entry.validate(); err != nil {
		return nil, err
	}
	return entry, nil
}

func (e *Entry) validate() error {
	if e.id == uuid.Nil || e.walletID == uuid.Nil || e.transactionID == uuid.Nil {
		return apperr.New(apperr.KindInvalid, CodeInvalidEntry, "ledger entry identifiers are required")
	}
	if e.direction != DirectionDebit && e.direction != DirectionCredit {
		return apperr.New(apperr.KindInvalid, CodeInvalidEntry, "ledger direction is required")
	}
	if !e.amount.IsValid() || !e.amount.IsPositive() {
		return apperr.New(apperr.KindInvalid, CodeInvalidEntry, "ledger amount must be positive")
	}
	if !e.balanceBefore.IsValid() || !e.balanceAfter.IsValid() {
		return apperr.New(apperr.KindInvalid, CodeInvalidEntry, "ledger balances are required")
	}
	if e.balanceBefore.IsNegative() || e.balanceAfter.IsNegative() {
		return apperr.New(apperr.KindInvalid, CodeInvalidEntry, "ledger balances must not be negative")
	}
	if e.createdAt.IsZero() {
		return apperr.New(apperr.KindInvalid, CodeInvalidEntry, "ledger creation instant is required")
	}

	var expected money.Money
	var err error
	switch e.direction {
	case DirectionDebit:
		expected, err = e.balanceBefore.Sub(e.amount)
	case DirectionCredit:
		expected, err = e.balanceBefore.Add(e.amount)
	}
	if err != nil {
		return err
	}
	equal, err := expected.Equal(e.balanceAfter)
	if err != nil {
		return err
	}
	if !equal {
		return apperr.New(apperr.KindInvalid, CodeMathMismatch,
			fmt.Sprintf("balanceAfter %s does not match balanceBefore %s %s %s",
				e.balanceAfter.AmountString(), e.balanceBefore.AmountString(), e.direction, e.amount.AmountString()))
	}
	return nil
}

// Accessors.

// ID returns the entry identity.
func (e *Entry) ID() uuid.UUID { return e.id }

// WalletID returns the affected wallet.
func (e *Entry) WalletID() uuid.UUID { return e.walletID }

// TransactionID returns the originating transaction.
func (e *Entry) TransactionID() uuid.UUID { return e.transactionID }

// Direction returns DEBIT or CREDIT.
func (e *Entry) Direction() Direction { return e.direction }

// Amount returns the movement amount.
func (e *Entry) Amount() money.Money { return e.amount }

// BalanceBefore returns the balance before the movement.
func (e *Entry) BalanceBefore() money.Money { return e.balanceBefore }

// BalanceAfter returns the balance after the movement.
func (e *Entry) BalanceAfter() money.Money { return e.balanceAfter }

// CreatedAt returns the entry instant (UTC).
func (e *Entry) CreatedAt() time.Time { return e.createdAt }

func mustUUIDv7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("ledger: cannot generate UUIDv7: %v", err))
	}
	return id
}
