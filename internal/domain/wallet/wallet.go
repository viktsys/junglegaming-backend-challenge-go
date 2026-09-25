// Package wallet implements the Wallet aggregate root: identity, player,
// currency, balance, version and timestamps. Balance changes are only possible
// through Debit and Credit, which preserve the non-negative invariant.
package wallet

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

const (
	// CodeInvalidWallet is returned for invalid constructor input.
	CodeInvalidWallet = "INVALID_WALLET"
	// CodeInsufficientFunds is returned when a debit would make the balance negative.
	CodeInsufficientFunds = "INSUFFICIENT_FUNDS"
	// CodeCurrencyMismatch is returned when an operation uses another currency.
	CodeCurrencyMismatch = "WALLET_CURRENCY_MISMATCH"
	// CodeVersionConflict is returned when an optimistic update loses a race.
	CodeVersionConflict = "WALLET_VERSION_CONFLICT"
	// CodeNonPositiveAmount is returned when a movement amount is not positive.
	CodeNonPositiveAmount = "WALLET_NON_POSITIVE_AMOUNT"
	// CodeInvalidTransition is returned for invalid lifecycle operations.
	CodeInvalidTransition = "INVALID_WALLET_TRANSITION"
)

// Wallet is the financial aggregate root. Fields are unexported so that the
// balance can only change through validated operations.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// BalanceChange describes the before/after balances of a successful movement.
type BalanceChange struct {
	Before money.Money
	After  money.Money
}

// Open creates a new wallet with an optional positive opening balance.
// The initial version is always 1; the opening credit is part of creation and
// does not bump the version.
func Open(playerID uuid.UUID, initialBalance money.Money, now time.Time) (*Wallet, error) {
	if playerID == uuid.Nil {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "playerId is required")
	}
	if !initialBalance.IsValid() {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "initial balance is required")
	}
	if initialBalance.IsNegative() {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "initial balance must not be negative")
	}
	if now.IsZero() {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "creation instant is required")
	}
	return &Wallet{
		id:        mustUUIDv7(),
		playerID:  playerID,
		currency:  initialBalance.Currency(),
		balance:   initialBalance,
		version:   1,
		createdAt: now.UTC(),
		updatedAt: now.UTC(),
	}, nil
}

// Rehydrate rebuilds a wallet from persisted state without applying movements.
func Rehydrate(
	id uuid.UUID,
	playerID uuid.UUID,
	currency money.Currency,
	balanceMinor int64,
	version int64,
	createdAt time.Time,
	updatedAt time.Time,
) (*Wallet, error) {
	if id == uuid.Nil || playerID == uuid.Nil {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "wallet id and playerId are required")
	}
	if currency.IsZero() {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "currency is required")
	}
	if balanceMinor < 0 {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "persisted balance is negative")
	}
	if version < 1 {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "persisted version must be >= 1")
	}
	if createdAt.IsZero() || updatedAt.IsZero() || updatedAt.Before(createdAt) {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidWallet, "invalid persisted timestamps")
	}
	balance, err := money.FromMinorUnits(balanceMinor, currency)
	if err != nil {
		return nil, err
	}
	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  currency,
		balance:   balance,
		version:   version,
		createdAt: createdAt.UTC(),
		updatedAt: updatedAt.UTC(),
	}, nil
}

// ID returns the wallet identity.
func (w *Wallet) ID() uuid.UUID { return w.id }

// PlayerID returns the owning player.
func (w *Wallet) PlayerID() uuid.UUID { return w.playerID }

// Currency returns the wallet currency.
func (w *Wallet) Currency() money.Currency { return w.currency }

// Balance returns the current balance.
func (w *Wallet) Balance() money.Money { return w.balance }

// Version returns the optimistic concurrency version.
func (w *Wallet) Version() int64 { return w.version }

// CreatedAt returns the creation instant (UTC).
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }

// UpdatedAt returns the last balance change instant (UTC).
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// Debit removes amount from the balance. The result may not be negative.
func (w *Wallet) Debit(amount money.Money, now time.Time) (BalanceChange, error) {
	if err := w.assertMovement(amount, now); err != nil {
		return BalanceChange{}, err
	}
	after, err := w.balance.Sub(amount)
	if err != nil {
		return BalanceChange{}, err
	}
	if after.IsNegative() {
		return BalanceChange{}, apperr.New(apperr.KindRejected, CodeInsufficientFunds,
			fmt.Sprintf("balance %s is not enough for debit of %s", w.balance.AmountString(), amount.AmountString()))
	}
	change := BalanceChange{Before: w.balance, After: after}
	w.apply(after, now)
	return change, nil
}

// Credit adds amount to the balance.
func (w *Wallet) Credit(amount money.Money, now time.Time) (BalanceChange, error) {
	if err := w.assertMovement(amount, now); err != nil {
		return BalanceChange{}, err
	}
	after, err := w.balance.Add(amount)
	if err != nil {
		return BalanceChange{}, err
	}
	change := BalanceChange{Before: w.balance, After: after}
	w.apply(after, now)
	return change, nil
}

func (w *Wallet) apply(balance money.Money, now time.Time) {
	w.balance = balance
	w.version++
	w.updatedAt = now.UTC()
}

func (w *Wallet) assertMovement(amount money.Money, now time.Time) error {
	if !amount.IsValid() {
		return apperr.New(apperr.KindInvalid, CodeInvalidWallet, "movement amount is required")
	}
	if !amount.IsPositive() {
		return apperr.New(apperr.KindInvalid, CodeNonPositiveAmount, "movement amount must be positive")
	}
	if amount.Currency() != w.currency {
		return apperr.New(apperr.KindInvalid, CodeCurrencyMismatch,
			fmt.Sprintf("wallet currency is %s but movement uses %s", w.currency, amount.Currency()))
	}
	if now.IsZero() {
		return apperr.New(apperr.KindInvalid, CodeInvalidWallet, "movement instant is required")
	}
	return nil
}

func mustUUIDv7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("wallet: cannot generate UUIDv7: %v", err))
	}
	return id
}
