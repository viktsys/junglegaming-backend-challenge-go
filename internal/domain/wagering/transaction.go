// Package wagering implements the WagerTransaction aggregate: external betting
// operations (BET, WIN, LOSS, REFUND, ROLLBACK) and the internal OPENING that
// credits a wallet at creation. The aggregate owns the state machine and the
// per-kind validation rules.
package wagering

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

// Kind is the external operation type.
type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// ParseKind validates an operation kind.
func ParseKind(raw string) (Kind, error) {
	switch Kind(raw) {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return Kind(raw), nil
	default:
		return "", apperr.New(apperr.KindInvalid, CodeInvalidKind,
			fmt.Sprintf("kind %q is not supported", raw))
	}
}

// IsExternal reports whether the kind may arrive through HTTP or SQS.
func (k Kind) IsExternal() bool { return k != KindOpening }

// RequiresReference reports whether the kind must carry a reference.
func (k Kind) RequiresReference() bool { return k == KindRefund || k == KindRollback }

// IsReversal reports whether the kind undoes a previous transaction.
func (k Kind) IsReversal() bool { return k == KindRefund || k == KindRollback }

// Status is the lifecycle state of a transaction.
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// ParseStatus validates a persisted status.
func ParseStatus(raw string) (Status, error) {
	switch Status(raw) {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return Status(raw), nil
	default:
		return "", apperr.New(apperr.KindInvalid, CodeInvalidStatus,
			fmt.Sprintf("status %q is not supported", raw))
	}
}

// IsTerminal reports whether no further transitions are allowed.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// Origin distinguishes internal (OPENING) from external operations.
type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

// Stable failure codes returned to providers and persisted for audit.
const (
	FailureInsufficientFunds         = "INSUFFICIENT_FUNDS"
	FailureReversalInsufficientFunds = "REVERSAL_INSUFFICIENT_FUNDS"
	FailureReferenceNotFound         = "REFERENCE_NOT_FOUND"
	FailureReferenceNotProcessed     = "REFERENCE_NOT_PROCESSED"
	FailureReferenceMismatch         = "REFERENCE_MISMATCH"
	FailureReferenceAmountMismatch   = "REFERENCE_AMOUNT_MISMATCH"
	FailureReversalConflict          = "REVERSAL_CONFLICT"
	FailureWalletNotFound            = "WALLET_NOT_FOUND"
	FailureWalletPlayerMismatch      = "WALLET_PLAYER_MISMATCH"
	FailureCurrencyMismatch          = "CURRENCY_MISMATCH"
	FailureInvalidAmount             = "INVALID_AMOUNT"
	FailureUnsupportedKind           = "UNSUPPORTED_KIND"
	FailureReferencePending          = "REFERENCE_PENDING"
)

// Domain error codes.
const (
	CodeInvalidTransaction = "INVALID_TRANSACTION"
	CodeInvalidKind        = "INVALID_TRANSACTION_KIND"
	CodeInvalidStatus      = "INVALID_TRANSACTION_STATUS"
	CodeInvalidTransition  = "INVALID_TRANSACTION_TRANSITION"
	CodeTerminalState      = "TRANSACTION_TERMINAL"
)

// IsCorrectableFailure reports whether a failure code may be resolved by a new,
// corrected request (as opposed to a definitive result for that payload).
func IsCorrectableFailure(code string) bool {
	switch code {
	case FailureInsufficientFunds, FailureReversalInsufficientFunds, FailureReferenceNotFound,
		FailureReferenceNotProcessed, FailureReferencePending:
		return true
	default:
		return false
	}
}

// ExternalParams are the fields required to create an external transaction.
type ExternalParams struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
}

// OpeningParams are the fields required to create an internal wallet opening.
type OpeningParams struct {
	WalletID uuid.UUID
	PlayerID uuid.UUID
	Money    money.Money
}

// Transaction is the wager transaction aggregate. Fields are unexported and
// transitions are validated.
type Transaction struct {
	id                             uuid.UUID
	origin                         Origin
	providerID                     string
	externalTransactionID          string
	idempotencyKey                 string
	payloadHash                    string
	walletID                       uuid.UUID
	playerID                       uuid.UUID
	roundID                        string
	gameID                         string
	kind                           Kind
	amount                         money.Money
	referenceExternalTransactionID string
	referenceTransactionID         *uuid.UUID
	status                         Status
	failureCode                    string
	resultBalance                  *money.Money
	attempts                       int
	nextAttemptAt                  *time.Time
	createdAt                      time.Time
	updatedAt                      time.Time
}

// NewExternal creates a PENDING external transaction with all validations for
// its kind applied.
func NewExternal(params ExternalParams, now time.Time) (*Transaction, error) {
	if params.ProviderID == "" {
		return nil, invalid("providerId is required")
	}
	if params.ExternalTransactionID == "" {
		return nil, invalid("externalTransactionId is required")
	}
	if params.IdempotencyKey == "" {
		return nil, invalid("idempotency key is required")
	}
	if params.PayloadHash == "" {
		return nil, invalid("payload hash is required")
	}
	if params.WalletID == uuid.Nil || params.PlayerID == uuid.Nil {
		return nil, invalid("walletId and playerId are required")
	}
	if params.RoundID == "" {
		return nil, invalid("roundId is required")
	}
	if params.GameID == "" {
		return nil, invalid("gameId is required")
	}
	if !params.Kind.IsExternal() {
		return nil, apperr.New(apperr.KindInvalid, CodeInvalidKind,
			"OPENING is reserved for internal wallet creation and cannot be sent by a provider")
	}
	if err := validateAmountForKind(params.Kind, params.Money); err != nil {
		return nil, err
	}
	if params.Kind.RequiresReference() && params.ReferenceExternalTransactionID == "" {
		return nil, invalid(fmt.Sprintf("%s requires referenceExternalTransactionId", params.Kind))
	}
	if !params.Kind.IsReversal() && !params.Kind.requiresOptionalReference() && params.ReferenceExternalTransactionID != "" {
		return nil, invalid(fmt.Sprintf("%s does not accept referenceExternalTransactionId", params.Kind))
	}
	if now.IsZero() {
		return nil, invalid("creation instant is required")
	}

	return &Transaction{
		id:                             mustUUIDv7(),
		origin:                         OriginExternal,
		providerID:                     params.ProviderID,
		externalTransactionID:          params.ExternalTransactionID,
		idempotencyKey:                 params.IdempotencyKey,
		payloadHash:                    params.PayloadHash,
		walletID:                       params.WalletID,
		playerID:                       params.PlayerID,
		roundID:                        params.RoundID,
		gameID:                         params.GameID,
		kind:                           params.Kind,
		amount:                         params.Money,
		referenceExternalTransactionID: params.ReferenceExternalTransactionID,
		status:                         StatusPending,
		createdAt:                      now.UTC(),
		updatedAt:                      now.UTC(),
	}, nil
}

// NewOpening creates the internal OPENING transaction for a wallet with a
// positive opening balance. It is immediately PROCESSED.
func NewOpening(params OpeningParams, now time.Time) (*Transaction, error) {
	if params.WalletID == uuid.Nil || params.PlayerID == uuid.Nil {
		return nil, invalid("walletId and playerId are required")
	}
	if !params.Money.IsValid() || !params.Money.IsPositive() {
		return nil, invalid("opening amount must be positive")
	}
	if now.IsZero() {
		return nil, invalid("creation instant is required")
	}
	result := params.Money
	return &Transaction{
		id:            mustUUIDv7(),
		origin:        OriginInternal,
		walletID:      params.WalletID,
		playerID:      params.PlayerID,
		kind:          KindOpening,
		amount:        params.Money,
		status:        StatusProcessed,
		resultBalance: &result,
		createdAt:     now.UTC(),
		updatedAt:     now.UTC(),
	}, nil
}

// RehydrateParams is the complete persisted state of a transaction.
type RehydrateParams struct {
	ID                             uuid.UUID
	Origin                         Origin
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
	ReferenceTransactionID         *uuid.UUID
	Status                         Status
	FailureCode                    string
	ResultBalance                  *money.Money
	Attempts                       int
	NextAttemptAt                  *time.Time
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

// Rehydrate rebuilds a transaction from persisted state without re-applying
// movements or emitting events.
func Rehydrate(params RehydrateParams) (*Transaction, error) {
	if params.ID == uuid.Nil {
		return nil, invalid("transaction id is required")
	}
	if params.WalletID == uuid.Nil || params.PlayerID == uuid.Nil {
		return nil, invalid("walletId and playerId are required")
	}
	if !params.Money.IsValid() {
		return nil, invalid("money is required")
	}
	if params.Attempts < 0 {
		return nil, invalid("attempts must not be negative")
	}
	if params.Status.IsTerminal() && params.ResultBalance == nil && params.Status == StatusProcessed && params.Kind != KindLoss {
		// PROCESSED movements always persist the resulting balance; LOSS is the
		// only kind that succeeds without a balance.
		return nil, invalid("processed transaction is missing its result balance")
	}
	if params.Status == StatusRejected || params.Status == StatusFailed {
		if params.FailureCode == "" {
			return nil, invalid("rejected or failed transaction is missing a failure code")
		}
	}
	if params.CreatedAt.IsZero() || params.UpdatedAt.IsZero() {
		return nil, invalid("timestamps are required")
	}
	tx := &Transaction{
		id:                             params.ID,
		origin:                         params.Origin,
		providerID:                     params.ProviderID,
		externalTransactionID:          params.ExternalTransactionID,
		idempotencyKey:                 params.IdempotencyKey,
		payloadHash:                    params.PayloadHash,
		walletID:                       params.WalletID,
		playerID:                       params.PlayerID,
		roundID:                        params.RoundID,
		gameID:                         params.GameID,
		kind:                           params.Kind,
		amount:                         params.Money,
		referenceExternalTransactionID: params.ReferenceExternalTransactionID,
		referenceTransactionID:         params.ReferenceTransactionID,
		status:                         params.Status,
		failureCode:                    params.FailureCode,
		resultBalance:                  params.ResultBalance,
		attempts:                       params.Attempts,
		nextAttemptAt:                  params.NextAttemptAt,
		createdAt:                      params.CreatedAt.UTC(),
		updatedAt:                      params.UpdatedAt.UTC(),
	}
	return tx, nil
}

// MarkPendingReference records that the operation waits for its reference.
func (t *Transaction) MarkPendingReference(now time.Time) error {
	if err := t.assertTransition(StatusPendingReference); err != nil {
		return err
	}
	t.status = StatusPendingReference
	t.updatedAt = now.UTC()
	return nil
}

// ScheduleRetry increments the attempt counter and sets the next attempt time.
func (t *Transaction) ScheduleRetry(next time.Time, now time.Time) error {
	if t.status != StatusPending && t.status != StatusPendingReference {
		return apperr.New(apperr.KindConflict, CodeTerminalState,
			fmt.Sprintf("cannot schedule retry for transaction in status %s", t.status))
	}
	if next.IsZero() {
		return invalid("next attempt instant is required")
	}
	t.attempts++
	nextUTC := next.UTC()
	t.nextAttemptAt = &nextUTC
	t.updatedAt = now.UTC()
	return nil
}

// MarkProcessed finalizes the transaction successfully with the resulting balance.
func (t *Transaction) MarkProcessed(balance money.Money, now time.Time) error {
	if err := t.assertTransition(StatusProcessed); err != nil {
		return err
	}
	if !balance.IsValid() {
		return invalid("result balance is required")
	}
	t.status = StatusProcessed
	t.resultBalance = &balance
	t.failureCode = ""
	t.nextAttemptAt = nil
	t.updatedAt = now.UTC()
	return nil
}

// MarkRejected finalizes the transaction as a business rejection.
func (t *Transaction) MarkRejected(code string, balance *money.Money, now time.Time) error {
	if err := t.assertTransition(StatusRejected); err != nil {
		return err
	}
	if code == "" {
		return invalid("failure code is required for a rejection")
	}
	t.status = StatusRejected
	t.failureCode = code
	t.resultBalance = balance
	t.nextAttemptAt = nil
	t.updatedAt = now.UTC()
	return nil
}

// MarkFailed finalizes the transaction with a permanent infrastructure failure.
func (t *Transaction) MarkFailed(code string, now time.Time) error {
	if err := t.assertTransition(StatusFailed); err != nil {
		return err
	}
	if code == "" {
		return invalid("failure code is required for a failure")
	}
	t.status = StatusFailed
	t.failureCode = code
	t.nextAttemptAt = nil
	t.updatedAt = now.UTC()
	return nil
}

// SetReferenceResolution stores the internal identity of the resolved reference.
func (t *Transaction) SetReferenceResolution(referenceID uuid.UUID, now time.Time) {
	t.referenceTransactionID = &referenceID
	t.updatedAt = now.UTC()
}

// Validate checks the full set of persisted invariants.
func (t *Transaction) Validate() error {
	if t.id == uuid.Nil {
		return invalid("transaction id is required")
	}
	if t.origin == OriginExternal {
		if t.providerID == "" || t.externalTransactionID == "" || t.idempotencyKey == "" || t.payloadHash == "" {
			return invalid("external transaction is missing provider metadata")
		}
		if t.roundID == "" || t.gameID == "" {
			return invalid("external transaction is missing roundId or gameId")
		}
		if t.kind == KindOpening {
			return invalid("external transaction cannot be an opening")
		}
	} else if t.kind != KindOpening {
		return invalid("internal transaction must be an opening")
	}
	return validateAmountForKind(t.kind, t.amount)
}

func (t *Transaction) assertTransition(to Status) error {
	if t.status.IsTerminal() {
		return apperr.New(apperr.KindConflict, CodeTerminalState,
			fmt.Sprintf("transaction %s is terminal in status %s", t.id, t.status))
	}
	allowed := map[Status]map[Status]bool{
		StatusPending: {
			StatusPendingReference: true,
			StatusProcessed:        true,
			StatusRejected:         true,
			StatusFailed:           true,
		},
		StatusPendingReference: {
			StatusProcessed: true,
			StatusRejected:  true,
			StatusFailed:    true,
		},
	}
	if !allowed[t.status][to] {
		return apperr.New(apperr.KindConflict, CodeInvalidTransition,
			fmt.Sprintf("cannot transition transaction from %s to %s", t.status, to))
	}
	return nil
}

func validateAmountForKind(kind Kind, amount money.Money) error {
	if !amount.IsValid() {
		return invalid("money is required")
	}
	if amount.IsNegative() {
		return apperr.New(apperr.KindInvalid, money.CodeNegativeAmount, "amount must not be negative")
	}
	if kind == KindLoss {
		if !amount.IsZero() {
			return apperr.New(apperr.KindInvalid, FailureInvalidAmount, "LOSS requires money.amount equal to 0.00")
		}
		return nil
	}
	if !amount.IsPositive() {
		return apperr.New(apperr.KindInvalid, FailureInvalidAmount,
			fmt.Sprintf("%s requires a positive amount", kind))
	}
	return nil
}

func (k Kind) requiresOptionalReference() bool { return k == KindWin }

// Accessors.

// ID returns the internal transaction identity.
func (t *Transaction) ID() uuid.UUID { return t.id }

// Origin returns INTERNAL or EXTERNAL.
func (t *Transaction) Origin() Origin { return t.origin }

// ProviderID returns the provider identity for external transactions.
func (t *Transaction) ProviderID() string { return t.providerID }

// ExternalTransactionID returns the provider transaction identity.
func (t *Transaction) ExternalTransactionID() string { return t.externalTransactionID }

// IdempotencyKey returns the key supplied by the provider.
func (t *Transaction) IdempotencyKey() string { return t.idempotencyKey }

// PayloadHash returns the deterministic hash of the business fields.
func (t *Transaction) PayloadHash() string { return t.payloadHash }

// WalletID returns the wallet affected by the operation.
func (t *Transaction) WalletID() uuid.UUID { return t.walletID }

// PlayerID returns the player that owns the operation.
func (t *Transaction) PlayerID() uuid.UUID { return t.playerID }

// RoundID returns the round identity.
func (t *Transaction) RoundID() string { return t.roundID }

// GameID returns the game identity.
func (t *Transaction) GameID() string { return t.gameID }

// Kind returns the operation kind.
func (t *Transaction) Kind() Kind { return t.kind }

// Money returns the operation amount.
func (t *Transaction) Money() money.Money { return t.amount }

// ReferenceExternalTransactionID returns the referenced provider transaction.
func (t *Transaction) ReferenceExternalTransactionID() string {
	return t.referenceExternalTransactionID
}

// ReferenceTransactionID returns the resolved internal reference, when known.
func (t *Transaction) ReferenceTransactionID() (uuid.UUID, bool) {
	if t.referenceTransactionID == nil {
		return uuid.Nil, false
	}
	return *t.referenceTransactionID, true
}

// Status returns the lifecycle state.
func (t *Transaction) Status() Status { return t.status }

// FailureCode returns the stable failure code for rejected or failed operations.
func (t *Transaction) FailureCode() string { return t.failureCode }

// ResultBalance returns the balance observed when the transaction was finalized.
func (t *Transaction) ResultBalance() (money.Money, bool) {
	if t.resultBalance == nil {
		return money.Money{}, false
	}
	return *t.resultBalance, true
}

// Attempts returns how many reference resolution attempts were made.
func (t *Transaction) Attempts() int { return t.attempts }

// NextAttemptAt returns the scheduled retry instant, when set.
func (t *Transaction) NextAttemptAt() (time.Time, bool) {
	if t.nextAttemptAt == nil {
		return time.Time{}, false
	}
	return *t.nextAttemptAt, true
}

// CreatedAt returns the creation instant (UTC).
func (t *Transaction) CreatedAt() time.Time { return t.createdAt }

// UpdatedAt returns the last transition instant (UTC).
func (t *Transaction) UpdatedAt() time.Time { return t.updatedAt }

// IsTerminal reports whether the transaction reached a terminal status.
func (t *Transaction) IsTerminal() bool { return t.status.IsTerminal() }

func invalid(message string) error {
	return apperr.New(apperr.KindInvalid, CodeInvalidTransaction, message)
}

func mustUUIDv7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("wagering: cannot generate UUIDv7: %v", err))
	}
	return id
}
