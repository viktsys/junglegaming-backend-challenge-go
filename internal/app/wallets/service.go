// Package wallets implements wallet lifecycle, ledger pagination and
// reconciliation use cases.
package wallets

import (
	"context"
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

// ErrWalletExists is returned when opening a duplicate (playerId, currency) wallet.
var ErrWalletExists = apperr.New(apperr.KindConflict, "WALLET_ALREADY_EXISTS",
	"a wallet already exists for this player and currency")

// Service exposes wallet use cases.
type Service struct {
	tx           ports.TxManager
	wallets      ports.WalletRepository
	transactions ports.TransactionRepository
	ledgers      ports.LedgerRepository
	outbox       ports.OutboxRepository
	clock        ports.Clock
	metrics      ports.Metrics
	logger       *slog.Logger
}

// NewService builds the wallet service.
func NewService(
	tx ports.TxManager,
	wallets ports.WalletRepository,
	transactions ports.TransactionRepository,
	ledgers ports.LedgerRepository,
	outbox ports.OutboxRepository,
	clock ports.Clock,
	metrics ports.Metrics,
	logger *slog.Logger,
) *Service {
	return &Service{
		tx:           tx,
		wallets:      wallets,
		transactions: transactions,
		ledgers:      ledgers,
		outbox:       outbox,
		clock:        clock,
		metrics:      metrics,
		logger:       logger,
	}
}

// OpenCommand describes a wallet opening request.
type OpenCommand struct {
	PlayerID       uuid.UUID
	InitialBalance money.Money
	CorrelationID  string
}

// Open creates a wallet. A positive initial balance also creates the OPENING
// transaction, its ledger credit and the outbox events in the same commit.
func (s *Service) Open(ctx context.Context, cmd OpenCommand) (*wallet.Wallet, error) {
	if cmd.PlayerID == uuid.Nil {
		return nil, apperr.New(apperr.KindInvalid, "INVALID_WALLET", "playerId is required")
	}
	if !cmd.InitialBalance.IsValid() {
		return nil, apperr.New(apperr.KindInvalid, money.CodeInvalidAmount, "initialBalance is required")
	}
	if cmd.InitialBalance.IsNegative() {
		return nil, apperr.New(apperr.KindInvalid, money.CodeNegativeAmount, "initialBalance must not be negative")
	}

	var opened *wallet.Wallet
	err := s.tx.WithinTransaction(ctx, func(ctx context.Context) error {
		now := s.clock()
		existing, err := s.wallets.GetByPlayerAndCurrency(ctx, cmd.PlayerID, cmd.InitialBalance.Currency())
		if err == nil && existing != nil {
			return ErrWalletExists
		}
		if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
			return err
		}

		w, err := wallet.Open(cmd.PlayerID, cmd.InitialBalance, now)
		if err != nil {
			return err
		}
		if err := s.wallets.Create(ctx, w); err != nil {
			return err
		}
		opened = w

		if !cmd.InitialBalance.IsPositive() {
			return nil
		}

		correlationID := cmd.CorrelationID
		if correlationID == "" {
			correlationID = w.ID().String()
		}

		opening, err := wagering.NewOpening(wagering.OpeningParams{
			WalletID: w.ID(),
			PlayerID: w.PlayerID(),
			Money:    cmd.InitialBalance,
		}, now)
		if err != nil {
			return err
		}
		if err := s.transactions.Insert(ctx, opening); err != nil {
			return err
		}

		zero, err := money.Zero(w.Currency())
		if err != nil {
			return err
		}
		entry, err := ledger.New(w.ID(), opening.ID(), ledger.DirectionCredit,
			cmd.InitialBalance, zero, cmd.InitialBalance, now)
		if err != nil {
			return err
		}
		if err := s.ledgers.Insert(ctx, entry); err != nil {
			return err
		}

		balanceEnvelope, err := events.NewWalletBalanceChanged(w.ID(), correlationID, opening.ID().String(), now,
			events.WalletBalanceChangedData{
				WalletID:      w.ID().String(),
				TransactionID: opening.ID().String(),
				Direction:     ledger.DirectionCredit,
				Money:         cmd.InitialBalance,
				BalanceBefore: zero,
				BalanceAfter:  cmd.InitialBalance,
				WalletVersion: w.Version(),
			})
		if err != nil {
			return err
		}
		if err := s.insertEvent(ctx, balanceEnvelope.EventID, w.ID(), balanceEnvelope.EventType, balanceEnvelope.OccurredAt, balanceEnvelope); err != nil {
			return err
		}

		processedEnvelope, err := events.NewWagerTransactionProcessed(opening.ID(), correlationID, opening.ID().String(), now,
			events.WagerTransactionProcessedData{
				TransactionID: opening.ID().String(),
				WalletID:      w.ID().String(),
				PlayerID:      w.PlayerID().String(),
				Kind:          string(wagering.KindOpening),
				Status:        string(wagering.StatusProcessed),
				Money:         cmd.InitialBalance,
				Balance:       cmd.InitialBalance,
			})
		if err != nil {
			return err
		}
		return s.insertEvent(ctx, processedEnvelope.EventID, opening.ID(), processedEnvelope.EventType, processedEnvelope.OccurredAt, processedEnvelope)
	})
	if err != nil {
		return nil, err
	}
	s.metrics.RecordWagerTransaction(string(wagering.StatusProcessed), string(wagering.KindOpening), "")
	return opened, nil
}

// Get loads a wallet by identity.
func (s *Service) Get(ctx context.Context, walletID uuid.UUID) (*wallet.Wallet, error) {
	return s.wallets.GetByID(ctx, walletID)
}

// Ledger returns one keyset-paginated page of the wallet ledger.
func (s *Service) Ledger(ctx context.Context, walletID uuid.UUID, cursor *ports.LedgerCursor, limit int) (ports.LedgerPage, error) {
	if _, err := s.wallets.GetByID(ctx, walletID); err != nil {
		return ports.LedgerPage{}, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return s.ledgers.ListByWallet(ctx, walletID, cursor, limit)
}

// Reconciliation is the result of comparing the stored balance with the ledger.
type Reconciliation struct {
	WalletID          uuid.UUID
	StoredBalance     money.Money
	CalculatedBalance money.Money
	Difference        money.Money
	Consistent        bool
	CheckedEntries    int64
}

// Reconcile rebuilds the balance from the ledger and compares it with the
// stored value without changing either.
func (s *Service) Reconcile(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	totals, err := s.ledgers.TotalsByWallet(ctx, walletID)
	if err != nil {
		return Reconciliation{}, err
	}

	stored, err := money.FromMinorUnits(totals.StoredBalanceMinor, totals.Currency)
	if err != nil {
		return Reconciliation{}, err
	}
	calculated, err := money.FromMinorUnits(totals.CreditsMinor, totals.Currency)
	if err != nil {
		return Reconciliation{}, err
	}
	debits, err := money.FromMinorUnits(totals.DebitsMinor, totals.Currency)
	if err != nil {
		return Reconciliation{}, err
	}
	calculated, err = calculated.Sub(debits)
	if err != nil {
		return Reconciliation{}, err
	}
	difference, err := stored.Sub(calculated)
	if err != nil {
		return Reconciliation{}, err
	}
	consistent := difference.IsZero()

	if !consistent {
		s.logger.Error("wallet reconciliation divergence",
			slog.String("walletId", walletID.String()),
			slog.String("storedBalance", stored.AmountString()),
			slog.String("calculatedBalance", calculated.AmountString()),
			slog.String("difference", difference.AmountString()),
			slog.Int64("checkedEntries", totals.Entries))
	}
	s.metrics.RecordReconciliationRun(consistent)

	return Reconciliation{
		WalletID:          walletID,
		StoredBalance:     stored,
		CalculatedBalance: calculated,
		Difference:        difference,
		Consistent:        consistent,
		CheckedEntries:    totals.Entries,
	}, nil
}

func (s *Service) insertEvent(ctx context.Context, eventID string, aggregateID uuid.UUID, eventType string, occurredAt time.Time, envelope interface{ Marshal() ([]byte, error) }) error {
	payload, err := envelope.Marshal()
	if err != nil {
		return err
	}
	parsedID, err := uuid.Parse(eventID)
	if err != nil {
		return fmt.Errorf("wallets: invalid event id %q: %w", eventID, err)
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
