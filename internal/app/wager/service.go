package wager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/events"
	"github.com/junglegaming/backend-challenge-go/internal/domain/ledger"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// ErrIdempotencyConflict is returned when a key or external transaction is
// reused with a different payload.
var ErrIdempotencyConflict = apperr.New(apperr.KindConflict, "IDEMPOTENCY_CONFLICT",
	"idempotency key or external transaction id was already used with a different payload")

// errReferencePending is an internal sentinel meaning "try again later".
var errReferencePending = errors.New("reference is not available yet")

// Config carries the service tuning knobs.
type Config struct {
	MaxWalletRetries     int
	ReferenceMaxAttempts int
	ReferenceBaseBackoff time.Duration
	ReferenceMaxBackoff  time.Duration
}

// Service processes wager operations.
type Service struct {
	tx           ports.TxManager
	wallets      ports.WalletRepository
	transactions ports.TransactionRepository
	ledgers      ports.LedgerRepository
	outbox       ports.OutboxRepository
	clock        ports.Clock
	metrics      ports.Metrics
	logger       *slog.Logger
	config       Config
}

// NewService builds the wager service.
func NewService(
	tx ports.TxManager,
	wallets ports.WalletRepository,
	transactions ports.TransactionRepository,
	ledgers ports.LedgerRepository,
	outbox ports.OutboxRepository,
	clock ports.Clock,
	metrics ports.Metrics,
	logger *slog.Logger,
	config Config,
) *Service {
	if config.MaxWalletRetries < 1 {
		config.MaxWalletRetries = 5
	}
	if config.ReferenceMaxAttempts < 1 {
		config.ReferenceMaxAttempts = 10
	}
	return &Service{
		tx:           tx,
		wallets:      wallets,
		transactions: transactions,
		ledgers:      ledgers,
		outbox:       outbox,
		clock:        clock,
		metrics:      metrics,
		logger:       logger,
		config:       config,
	}
}

// ProcessCommand is a normalized external operation request.
type ProcessCommand struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           wagering.Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
	CorrelationID                  string
	CausationID                    string
}

// Result is the outcome of processing or replaying an operation.
type Result struct {
	Transaction      *wagering.Transaction
	Status           wagering.Status
	Balance          money.Money
	HasBalance       bool
	FailureCode      string
	IdempotentReplay bool
}

// Validate checks the command fields that are independent of persistence.
func (cmd ProcessCommand) Validate() error {
	if cmd.ProviderID == "" {
		return apperr.New(apperr.KindInvalid, wagering.CodeInvalidTransaction, "providerId is required")
	}
	if cmd.ExternalTransactionID == "" {
		return apperr.New(apperr.KindInvalid, wagering.CodeInvalidTransaction, "externalTransactionId is required")
	}
	if cmd.IdempotencyKey == "" {
		return apperr.New(apperr.KindInvalid, wagering.CodeInvalidTransaction, "idempotency key is required")
	}
	if cmd.PlayerID == uuid.Nil || cmd.WalletID == uuid.Nil {
		return apperr.New(apperr.KindInvalid, wagering.CodeInvalidTransaction, "playerId and walletId are required")
	}
	if cmd.RoundID == "" || cmd.GameID == "" {
		return apperr.New(apperr.KindInvalid, wagering.CodeInvalidTransaction, "roundId and gameId are required")
	}
	if !cmd.Money.IsValid() {
		return apperr.New(apperr.KindInvalid, money.CodeInvalidAmount, "money is required")
	}
	if !cmd.Kind.IsExternal() {
		return apperr.New(apperr.KindInvalid, wagering.CodeInvalidKind,
			"OPENING is reserved for internal wallet creation")
	}
	if cmd.Kind.RequiresReference() && cmd.ReferenceExternalTransactionID == "" {
		return apperr.New(apperr.KindInvalid, wagering.CodeInvalidTransaction,
			fmt.Sprintf("%s requires referenceExternalTransactionId", cmd.Kind))
	}
	if cmd.Money.IsNegative() {
		return apperr.New(apperr.KindInvalid, money.CodeNegativeAmount, "amount must not be negative")
	}
	if cmd.Kind == wagering.KindLoss {
		if !cmd.Money.IsZero() {
			return apperr.New(apperr.KindInvalid, wagering.FailureInvalidAmount, "LOSS requires money.amount equal to 0.00")
		}
		return nil
	}
	if !cmd.Money.IsPositive() {
		return apperr.New(apperr.KindInvalid, wagering.FailureInvalidAmount,
			fmt.Sprintf("%s requires a positive amount", cmd.Kind))
	}
	return nil
}

// Process applies an external operation with persistent idempotency. Wallet
// version conflicts are retried a bounded number of times.
func (s *Service) Process(ctx context.Context, cmd ProcessCommand) (Result, error) {
	if err := cmd.Validate(); err != nil {
		return Result{}, err
	}
	hash := ComputePayloadHash(cmd)

	var (
		result Result
		last   error
	)
	for attempt := 0; attempt <= s.config.MaxWalletRetries; attempt++ {
		err := s.tx.WithinTransaction(ctx, func(ctx context.Context) error {
			var err error
			result, err = s.processOnce(ctx, cmd, hash)
			return err
		})
		if err == nil {
			return result, nil
		}
		if isRetryable(err) {
			last = err
			if apperr.CodeOf(err) == wallet.CodeVersionConflict {
				s.metrics.RecordWalletVersionConflict()
			}
			continue
		}
		return Result{}, err
	}
	return Result{}, apperr.Wrap(apperr.KindConflict, wallet.CodeVersionConflict,
		"wallet kept changing during the operation", last)
}

// isRetryable reports whether the attempt should be replayed on fresh state.
// Unique violations abort the PostgreSQL transaction, so the retry always runs
// in a new transaction and re-reads the committed winner.
func isRetryable(err error) bool {
	if apperr.CodeOf(err) == wallet.CodeVersionConflict {
		return true
	}
	return errors.Is(err, ports.ErrDuplicateExternalTransaction) ||
		errors.Is(err, ports.ErrDuplicateIdempotencyKey) ||
		errors.Is(err, ports.ErrDuplicateReversal)
}

func (s *Service) processOnce(ctx context.Context, cmd ProcessCommand, hash string) (Result, error) {
	now := s.clock()

	existing, err := s.transactions.GetByProviderExternal(ctx, cmd.ProviderID, cmd.ExternalTransactionID)
	if err == nil {
		return s.replay(existing, hash)
	}
	if apperr.KindOf(err) != apperr.KindNotFound {
		return Result{}, err
	}

	byKey, err := s.transactions.GetByProviderIdempotencyKey(ctx, cmd.ProviderID, cmd.IdempotencyKey)
	if err == nil {
		if byKey.ExternalTransactionID() == cmd.ExternalTransactionID && byKey.PayloadHash() == hash {
			return s.replay(byKey, hash)
		}
		return Result{}, ErrIdempotencyConflict
	}
	if apperr.KindOf(err) != apperr.KindNotFound {
		return Result{}, err
	}

	t, err := wagering.NewExternal(wagering.ExternalParams{
		ProviderID:                     cmd.ProviderID,
		ExternalTransactionID:          cmd.ExternalTransactionID,
		IdempotencyKey:                 cmd.IdempotencyKey,
		PayloadHash:                    hash,
		WalletID:                       cmd.WalletID,
		PlayerID:                       cmd.PlayerID,
		RoundID:                        cmd.RoundID,
		GameID:                         cmd.GameID,
		Kind:                           cmd.Kind,
		Money:                          cmd.Money,
		ReferenceExternalTransactionID: cmd.ReferenceExternalTransactionID,
	}, now)
	if err != nil {
		return Result{}, err
	}

	if err := s.transactions.Insert(ctx, t); err != nil {
		// A unique violation aborts the transaction, so the caller retries on
		// fresh state; the pre-checks then resolve the replay or the conflict.
		return Result{}, err
	}
	return s.applyOperation(ctx, t, now, correlationOf(cmd.CorrelationID, t.ID()))
}

func (s *Service) replay(existing *wagering.Transaction, hash string) (Result, error) {
	if existing.PayloadHash() != hash {
		return Result{}, ErrIdempotencyConflict
	}
	s.metrics.RecordIdempotentReplay()
	result := Result{
		Transaction:      existing,
		Status:           existing.Status(),
		FailureCode:      existing.FailureCode(),
		IdempotentReplay: true,
	}
	if balance, ok := existing.ResultBalance(); ok {
		result.Balance = balance
		result.HasBalance = true
	}
	return result, nil
}

// applyOperation executes the kind-specific rules for a PENDING transaction.
// It must run inside the transaction that inserted (or claimed) the row.
func (s *Service) applyOperation(ctx context.Context, t *wagering.Transaction, now time.Time, correlationID string) (Result, error) {
	w, err := s.wallets.GetByID(ctx, t.WalletID())
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return Result{}, apperr.Wrap(apperr.KindRejected, wagering.FailureWalletNotFound,
				"wallet does not exist", err)
		}
		return Result{}, err
	}
	if w.PlayerID() != t.PlayerID() {
		return s.reject(ctx, t, w, wagering.FailureWalletPlayerMismatch, now, correlationID)
	}
	if w.Currency() != t.Money().Currency() {
		return s.reject(ctx, t, w, wagering.FailureCurrencyMismatch, now, correlationID)
	}

	switch t.Kind() {
	case wagering.KindLoss:
		return s.markProcessed(ctx, t, w.Balance(), now, correlationID)

	case wagering.KindBet:
		return s.move(ctx, t, w, ledger.DirectionDebit, nil, now, correlationID)

	case wagering.KindWin:
		if t.ReferenceExternalTransactionID() == "" {
			return s.move(ctx, t, w, ledger.DirectionCredit, nil, now, correlationID)
		}
		ref, err := s.resolveReference(ctx, t)
		if err != nil {
			return s.handleReferenceError(ctx, t, w, err, now, correlationID)
		}
		return s.move(ctx, t, w, ledger.DirectionCredit, ref, now, correlationID)

	case wagering.KindRefund, wagering.KindRollback:
		ref, err := s.resolveReference(ctx, t)
		if err != nil {
			return s.handleReferenceError(ctx, t, w, err, now, correlationID)
		}
		return s.applyReversal(ctx, t, w, ref, now, correlationID)
	}

	return Result{}, apperr.New(apperr.KindInvalid, wagering.FailureUnsupportedKind,
		fmt.Sprintf("kind %s cannot be processed", t.Kind()))
}

// handleReferenceError either parks the transaction waiting for its reference
// or finalizes a definitive rejection.
func (s *Service) handleReferenceError(ctx context.Context, t *wagering.Transaction, w *wallet.Wallet, refErr error, now time.Time, correlationID string) (Result, error) {
	if errors.Is(refErr, errReferencePending) {
		return s.scheduleReferenceRetry(ctx, t, now, correlationID)
	}
	code := apperr.CodeOf(refErr)
	if code == "" {
		return Result{}, refErr
	}
	return s.reject(ctx, t, w, code, now, correlationID)
}

func (s *Service) scheduleReferenceRetry(ctx context.Context, t *wagering.Transaction, now time.Time, correlationID string) (Result, error) {
	if t.Attempts()+1 >= s.config.ReferenceMaxAttempts {
		s.logger.Warn("reference expired",
			slog.String("transactionId", t.ID().String()),
			slog.String("referenceExternalTransactionId", t.ReferenceExternalTransactionID()),
			slog.Int("attempts", t.Attempts()))
		return s.reject(ctx, t, nil, wagering.FailureReferenceNotFound, now, correlationID)
	}
	if t.Status() == wagering.StatusPending {
		if err := t.MarkPendingReference(now); err != nil {
			return Result{}, err
		}
	}
	next := now.Add(s.referenceBackoff(t.Attempts() + 1))
	if err := t.ScheduleRetry(next, now); err != nil {
		return Result{}, err
	}
	if err := s.transactions.Update(ctx, t); err != nil {
		return Result{}, err
	}

	envelope, err := events.NewWagerTransactionPendingReference(t.ID(), correlationID, t.ID().String(), now,
		events.WagerTransactionPendingReferenceData{
			TransactionID:                  t.ID().String(),
			ProviderID:                     t.ProviderID(),
			ExternalTransactionID:          t.ExternalTransactionID(),
			ReferenceExternalTransactionID: t.ReferenceExternalTransactionID(),
			Attempt:                        t.Attempts(),
			NextAttemptAt:                  next,
		})
	if err != nil {
		return Result{}, err
	}
	if err := s.enqueue(ctx, envelope.EventID, t.ID(), envelope.EventType, envelope.OccurredAt, envelope); err != nil {
		return Result{}, err
	}
	s.metrics.RecordWagerTransaction(string(wagering.StatusPendingReference), string(t.Kind()), "")
	return Result{
		Transaction: t,
		Status:      wagering.StatusPendingReference,
	}, nil
}

func (s *Service) applyReversal(ctx context.Context, t *wagering.Transaction, w *wallet.Wallet, ref *wagering.Transaction, now time.Time, correlationID string) (Result, error) {
	var direction ledger.Direction
	switch ref.Kind() {
	case wagering.KindBet:
		direction = ledger.DirectionCredit
	case wagering.KindWin, wagering.KindRefund:
		direction = ledger.DirectionDebit
	default:
		return s.reject(ctx, t, w, wagering.FailureReferenceMismatch, now, correlationID)
	}
	return s.move(ctx, t, w, direction, ref, now, correlationID)
}

// move applies a wallet movement, appends the ledger entry and emits the
// balance-changed plus processed events. The wallet update is the only step
// that can race; it runs before the aggregate is mutated so a retry starts
// from clean state.
func (s *Service) move(ctx context.Context, t *wagering.Transaction, w *wallet.Wallet, direction ledger.Direction, ref *wagering.Transaction, now time.Time, correlationID string) (Result, error) {
	expectedVersion := w.Version()

	var (
		change wallet.BalanceChange
		err    error
	)
	if direction == ledger.DirectionDebit {
		change, err = w.Debit(t.Money(), now)
	} else {
		change, err = w.Credit(t.Money(), now)
	}
	if err != nil {
		if apperr.CodeOf(err) == wallet.CodeInsufficientFunds {
			code := wagering.FailureInsufficientFunds
			if t.Kind() == wagering.KindRollback {
				code = wagering.FailureReversalInsufficientFunds
			}
			return s.reject(ctx, t, w, code, now, correlationID)
		}
		return Result{}, err
	}

	if err := s.wallets.UpdateBalance(ctx, w, expectedVersion); err != nil {
		return Result{}, err
	}
	if ref != nil {
		t.SetReferenceResolution(ref.ID(), now)
	}

	entry, err := ledger.New(w.ID(), t.ID(), direction, t.Money(), change.Before, change.After, now)
	if err != nil {
		return Result{}, err
	}
	if err := s.ledgers.Insert(ctx, entry); err != nil {
		return Result{}, err
	}

	balanceEnvelope, err := events.NewWalletBalanceChanged(w.ID(), correlationID, t.ID().String(), now,
		events.WalletBalanceChangedData{
			WalletID:      w.ID().String(),
			TransactionID: t.ID().String(),
			Direction:     direction,
			Money:         t.Money(),
			BalanceBefore: change.Before,
			BalanceAfter:  change.After,
			WalletVersion: w.Version(),
		})
	if err != nil {
		return Result{}, err
	}
	if err := s.enqueue(ctx, balanceEnvelope.EventID, w.ID(), balanceEnvelope.EventType, balanceEnvelope.OccurredAt, balanceEnvelope); err != nil {
		return Result{}, err
	}

	return s.markProcessed(ctx, t, change.After, now, correlationID)
}

func (s *Service) markProcessed(ctx context.Context, t *wagering.Transaction, balance money.Money, now time.Time, correlationID string) (Result, error) {
	if err := t.MarkProcessed(balance, now); err != nil {
		return Result{}, err
	}
	if err := s.transactions.Update(ctx, t); err != nil {
		return Result{}, err
	}

	envelope, err := events.NewWagerTransactionProcessed(t.ID(), correlationID, t.ID().String(), now,
		events.WagerTransactionProcessedData{
			TransactionID:         t.ID().String(),
			ProviderID:            t.ProviderID(),
			ExternalTransactionID: t.ExternalTransactionID(),
			WalletID:              t.WalletID().String(),
			PlayerID:              t.PlayerID().String(),
			RoundID:               t.RoundID(),
			GameID:                t.GameID(),
			Kind:                  string(t.Kind()),
			Status:                string(t.Status()),
			Money:                 t.Money(),
			Balance:               balance,
		})
	if err != nil {
		return Result{}, err
	}
	if err := s.enqueue(ctx, envelope.EventID, t.ID(), envelope.EventType, envelope.OccurredAt, envelope); err != nil {
		return Result{}, err
	}

	if t.Origin() == wagering.OriginExternal {
		if _, err := s.transactions.WakeDependents(ctx, t.ProviderID(), t.ExternalTransactionID(), now); err != nil {
			return Result{}, err
		}
	}

	s.metrics.RecordWagerTransaction(string(wagering.StatusProcessed), string(t.Kind()), "")
	return Result{
		Transaction: t,
		Status:      wagering.StatusProcessed,
		Balance:     balance,
		HasBalance:  true,
	}, nil
}

func (s *Service) reject(ctx context.Context, t *wagering.Transaction, w *wallet.Wallet, code string, now time.Time, correlationID string) (Result, error) {
	var (
		balance    *money.Money
		result     money.Money
		hasBalance bool
	)
	if w != nil {
		observed := w.Balance()
		balance = &observed
		result = observed
		hasBalance = true
	}
	if err := t.MarkRejected(code, balance, now); err != nil {
		return Result{}, err
	}
	if err := s.transactions.Update(ctx, t); err != nil {
		return Result{}, err
	}

	envelope, err := events.NewWagerTransactionRejected(t.ID(), correlationID, t.ID().String(), now,
		events.WagerTransactionRejectedData{
			TransactionID:         t.ID().String(),
			ProviderID:            t.ProviderID(),
			ExternalTransactionID: t.ExternalTransactionID(),
			WalletID:              t.WalletID().String(),
			PlayerID:              t.PlayerID().String(),
			Kind:                  string(t.Kind()),
			Status:                string(t.Status()),
			FailureCode:           code,
			Money:                 t.Money(),
		})
	if err != nil {
		return Result{}, err
	}
	if err := s.enqueue(ctx, envelope.EventID, t.ID(), envelope.EventType, envelope.OccurredAt, envelope); err != nil {
		return Result{}, err
	}

	s.metrics.RecordWagerTransaction(string(wagering.StatusRejected), string(t.Kind()), code)
	return Result{
		Transaction: t,
		Status:      wagering.StatusRejected,
		FailureCode: code,
		Balance:     result,
		HasBalance:  hasBalance,
	}, nil
}

// resolveReference validates the referenced external transaction, returning
// errReferencePending while it is unavailable or still in flight.
func (s *Service) resolveReference(ctx context.Context, t *wagering.Transaction) (*wagering.Transaction, error) {
	ref, err := s.transactions.GetByProviderExternal(ctx, t.ProviderID(), t.ReferenceExternalTransactionID())
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return nil, errReferencePending
		}
		return nil, err
	}
	switch ref.Status() {
	case wagering.StatusPending, wagering.StatusPendingReference:
		return nil, errReferencePending
	case wagering.StatusProcessed:
	default:
		return nil, apperr.New(apperr.KindRejected, wagering.FailureReferenceNotProcessed,
			fmt.Sprintf("reference %s finished with status %s", ref.ExternalTransactionID(), ref.Status()))
	}

	switch t.Kind() {
	case wagering.KindRefund:
		if ref.Kind() != wagering.KindBet {
			return nil, mismatchError(t, "REFUND can only reference a BET")
		}
	case wagering.KindRollback:
		if ref.Kind() != wagering.KindBet && ref.Kind() != wagering.KindWin && ref.Kind() != wagering.KindRefund {
			return nil, mismatchError(t, "ROLLBACK can only reference a BET, WIN or REFUND")
		}
	case wagering.KindWin:
		if ref.Kind() != wagering.KindBet {
			return nil, mismatchError(t, "WIN can only reference a BET")
		}
	}

	if ref.PlayerID() != t.PlayerID() || ref.WalletID() != t.WalletID() ||
		ref.Money().Currency() != t.Money().Currency() || ref.RoundID() != t.RoundID() {
		return nil, mismatchError(t, "reference does not agree on player, wallet, currency or round")
	}

	if t.Kind().IsReversal() {
		equal, err := ref.Money().Equal(t.Money())
		if err != nil {
			return nil, err
		}
		if !equal {
			return nil, apperr.New(apperr.KindRejected, wagering.FailureReferenceAmountMismatch,
				"reversal amount must equal the referenced amount")
		}
	}

	existing, err := s.transactions.FindReversal(ctx, ref.ID(), t.Kind())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, apperr.New(apperr.KindRejected, wagering.FailureReversalConflict,
			fmt.Sprintf("reference already has a successful %s", t.Kind()))
	}

	if ref.Kind() == wagering.KindBet {
		crossKind := wagering.KindRollback
		if t.Kind() == wagering.KindRollback {
			crossKind = wagering.KindRefund
		}
		cross, err := s.transactions.FindReversal(ctx, ref.ID(), crossKind)
		if err != nil {
			return nil, err
		}
		if cross != nil {
			return nil, apperr.New(apperr.KindRejected, wagering.FailureReversalConflict,
				fmt.Sprintf("bet already has a successful %s; it cannot be reversed again", crossKind))
		}
	}

	return ref, nil
}

// ResumeDue claims and resumes up to limit pending transactions. It returns
// how many rows were claimed.
func (s *Service) ResumeDue(ctx context.Context, limit int) (int, error) {
	resumed := 0
	for i := 0; i < limit; i++ {
		claimed, err := s.resumeOne(ctx)
		if err != nil {
			return resumed, err
		}
		if !claimed {
			break
		}
		resumed++
	}
	return resumed, nil
}

func (s *Service) resumeOne(ctx context.Context) (bool, error) {
	claimed := false
	err := s.tx.WithinTransaction(ctx, func(ctx context.Context) error {
		t, err := s.transactions.ClaimOne(ctx, s.clock())
		if err != nil {
			return err
		}
		if t == nil {
			return nil
		}
		claimed = true

		var last error
		for attempt := 0; attempt <= s.config.MaxWalletRetries; attempt++ {
			if attempt > 0 {
				// The previous attempt was rolled back to its savepoint; reload
				// the aggregate so retries start from clean state.
				reloaded, reloadErr := s.transactions.GetByID(ctx, t.ID())
				if reloadErr != nil {
					return reloadErr
				}
				t = reloaded
			}
			attemptErr := s.tx.WithinTransaction(ctx, func(ctx context.Context) error {
				_, err := s.applyOperation(ctx, t, s.clock(), correlationOf("", t.ID()))
				return err
			})
			if attemptErr == nil {
				return nil
			}
			if isRetryable(attemptErr) {
				last = attemptErr
				if apperr.CodeOf(attemptErr) == wallet.CodeVersionConflict {
					s.metrics.RecordWalletVersionConflict()
				}
				continue
			}
			return attemptErr
		}
		return apperr.Wrap(apperr.KindConflict, wallet.CodeVersionConflict,
			"wallet kept changing during resume", last)
	})
	if err != nil {
		return claimed, err
	}
	return claimed, nil
}

func (s *Service) enqueue(ctx context.Context, eventID string, aggregateID uuid.UUID, eventType string, occurredAt time.Time, envelope interface{ Marshal() ([]byte, error) }) error {
	payload, err := envelope.Marshal()
	if err != nil {
		return err
	}
	parsedID, err := uuid.Parse(eventID)
	if err != nil {
		return fmt.Errorf("wager: invalid event id %q: %w", eventID, err)
	}
	return s.outbox.Insert(ctx, ports.OutboxEvent{
		EventID:       parsedID,
		AggregateID:   aggregateID,
		EventType:     eventType,
		Payload:       payload,
		OccurredAt:    occurredAt,
		NextAttemptAt: occurredAt,
	})
}

func (s *Service) referenceBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := s.config.ReferenceBaseBackoff
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff >= s.config.ReferenceMaxBackoff {
			return s.config.ReferenceMaxBackoff
		}
	}
	if backoff > s.config.ReferenceMaxBackoff {
		return s.config.ReferenceMaxBackoff
	}
	return backoff
}

func mismatchError(t *wagering.Transaction, message string) error {
	return apperr.New(apperr.KindRejected, wagering.FailureReferenceMismatch,
		fmt.Sprintf("reference %s is invalid for transaction %s: %s", t.ReferenceExternalTransactionID(), t.ID(), message))
}

func correlationOf(correlationID string, transactionID uuid.UUID) string {
	if correlationID != "" {
		return correlationID
	}
	return transactionID.String()
}
