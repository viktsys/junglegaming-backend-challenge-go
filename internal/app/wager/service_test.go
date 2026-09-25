package wager

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/ledger"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// --- in-memory fakes -------------------------------------------------------

type fakeTxManager struct{}

func (fakeTxManager) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

type fakeWallets struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]*wallet.Wallet
	byOwner map[string]*wallet.Wallet
}

func newFakeWallets() *fakeWallets {
	return &fakeWallets{byID: map[uuid.UUID]*wallet.Wallet{}, byOwner: map[string]*wallet.Wallet{}}
}

func (f *fakeWallets) Create(_ context.Context, w *wallet.Wallet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := w.PlayerID().String() + "|" + w.Currency().String()
	if _, exists := f.byOwner[key]; exists {
		return apperr.New(apperr.KindConflict, "WALLET_ALREADY_EXISTS", "duplicate wallet")
	}
	f.byID[w.ID()] = cloneWallet(w)
	f.byOwner[key] = cloneWallet(w)
	return nil
}

func (f *fakeWallets) GetByID(_ context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.byID[id]; ok {
		return cloneWallet(w), nil
	}
	return nil, apperr.New(apperr.KindNotFound, "WALLET_NOT_FOUND", "missing wallet")
}

func (f *fakeWallets) GetByPlayerAndCurrency(_ context.Context, playerID uuid.UUID, currency money.Currency) (*wallet.Wallet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.byOwner[playerID.String()+"|"+currency.String()]; ok {
		return cloneWallet(w), nil
	}
	return nil, apperr.New(apperr.KindNotFound, "WALLET_NOT_FOUND", "missing wallet")
}

func (f *fakeWallets) UpdateBalance(_ context.Context, w *wallet.Wallet, expectedVersion int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored, ok := f.byID[w.ID()]
	if !ok {
		return apperr.New(apperr.KindNotFound, "WALLET_NOT_FOUND", "missing wallet")
	}
	if stored.Version() != expectedVersion {
		return apperr.New(apperr.KindConflict, wallet.CodeVersionConflict, "version conflict")
	}
	f.byID[w.ID()] = cloneWallet(w)
	return nil
}

func cloneWallet(w *wallet.Wallet) *wallet.Wallet {
	clone, err := wallet.Rehydrate(w.ID(), w.PlayerID(), w.Currency(), w.Balance().MinorUnits(),
		w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		panic(err)
	}
	return clone
}

type fakeTransactions struct {
	mu   sync.Mutex
	byID map[uuid.UUID]*wagering.Transaction
}

func newFakeTransactions() *fakeTransactions {
	return &fakeTransactions{byID: map[uuid.UUID]*wagering.Transaction{}}
}

func (f *fakeTransactions) Insert(_ context.Context, t *wagering.Transaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if existing.Origin() != wagering.OriginExternal || t.Origin() != wagering.OriginExternal {
			continue
		}
		if existing.ProviderID() != t.ProviderID() {
			continue
		}
		if existing.ExternalTransactionID() == t.ExternalTransactionID() {
			return ports.ErrDuplicateExternalTransaction
		}
		if existing.IdempotencyKey() == t.IdempotencyKey() {
			return ports.ErrDuplicateIdempotencyKey
		}
	}
	f.byID[t.ID()] = cloneTransaction(t)
	return nil
}

func (f *fakeTransactions) Update(_ context.Context, t *wagering.Transaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[t.ID()] = cloneTransaction(t)
	return nil
}

func (f *fakeTransactions) GetByID(_ context.Context, id uuid.UUID) (*wagering.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.byID[id]; ok {
		return cloneTransaction(t), nil
	}
	return nil, apperr.New(apperr.KindNotFound, "TRANSACTION_NOT_FOUND", "missing transaction")
}

func (f *fakeTransactions) GetByProviderExternal(_ context.Context, providerID, externalID string) (*wagering.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byID {
		if t.Origin() == wagering.OriginExternal && t.ProviderID() == providerID && t.ExternalTransactionID() == externalID {
			return cloneTransaction(t), nil
		}
	}
	return nil, apperr.New(apperr.KindNotFound, "TRANSACTION_NOT_FOUND", "missing transaction")
}

func (f *fakeTransactions) GetByProviderIdempotencyKey(_ context.Context, providerID, key string) (*wagering.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byID {
		if t.Origin() == wagering.OriginExternal && t.ProviderID() == providerID && t.IdempotencyKey() == key {
			return cloneTransaction(t), nil
		}
	}
	return nil, apperr.New(apperr.KindNotFound, "TRANSACTION_NOT_FOUND", "missing transaction")
}

func cloneTransaction(tx *wagering.Transaction) *wagering.Transaction {
	var referenceID *uuid.UUID
	if id, ok := tx.ReferenceTransactionID(); ok {
		referenceID = &id
	}
	var resultBalance *money.Money
	if balance, ok := tx.ResultBalance(); ok {
		resultBalance = &balance
	}
	var nextAttempt *time.Time
	if at, ok := tx.NextAttemptAt(); ok {
		nextAttempt = &at
	}
	clone, err := wagering.Rehydrate(wagering.RehydrateParams{
		ID:                             tx.ID(),
		Origin:                         tx.Origin(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalTransactionID(),
		IdempotencyKey:                 tx.IdempotencyKey(),
		PayloadHash:                    tx.PayloadHash(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		Kind:                           tx.Kind(),
		Money:                          tx.Money(),
		ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
		ReferenceTransactionID:         referenceID,
		Status:                         tx.Status(),
		FailureCode:                    tx.FailureCode(),
		ResultBalance:                  resultBalance,
		Attempts:                       tx.Attempts(),
		NextAttemptAt:                  nextAttempt,
		CreatedAt:                      tx.CreatedAt(),
		UpdatedAt:                      tx.UpdatedAt(),
	})
	if err != nil {
		panic(err)
	}
	return clone
}

func (f *fakeTransactions) ClaimOne(context.Context, time.Time) (*wagering.Transaction, error) {
	return nil, nil
}

func (f *fakeTransactions) CountPending(context.Context) (int64, error) { return 0, nil }

func (f *fakeTransactions) FindReversal(_ context.Context, referenceID uuid.UUID, kind wagering.Kind) (*wagering.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byID {
		resolved, ok := t.ReferenceTransactionID()
		if ok && resolved == referenceID && t.Kind() == kind && t.Status() == wagering.StatusProcessed {
			return t, nil
		}
	}
	return nil, nil
}

func (f *fakeTransactions) WakeDependents(context.Context, string, string, time.Time) (int64, error) {
	return 0, nil
}

type fakeLedger struct {
	mu      sync.Mutex
	entries []*ledger.Entry
}

func (f *fakeLedger) Insert(_ context.Context, entry *ledger.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.entries {
		if existing.WalletID() == entry.WalletID() && existing.TransactionID() == entry.TransactionID() {
			return ports.ErrDuplicateLedgerEntry
		}
	}
	f.entries = append(f.entries, entry)
	return nil
}

func (f *fakeLedger) ListByWallet(context.Context, uuid.UUID, *ports.LedgerCursor, int) (ports.LedgerPage, error) {
	return ports.LedgerPage{}, nil
}

func (f *fakeLedger) TotalsByWallet(_ context.Context, walletID uuid.UUID) (ports.LedgerTotals, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	totals := ports.LedgerTotals{Currency: "BRL"}
	for _, entry := range f.entries {
		if entry.WalletID() != walletID {
			continue
		}
		totals.Entries++
		if entry.Direction() == ledger.DirectionCredit {
			totals.CreditsMinor += entry.Amount().MinorUnits()
		} else {
			totals.DebitsMinor += entry.Amount().MinorUnits()
		}
	}
	return totals, nil
}

type fakeOutbox struct {
	mu     sync.Mutex
	events []ports.OutboxEvent
}

func (f *fakeOutbox) Insert(_ context.Context, event ports.OutboxEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakeOutbox) Claim(context.Context, string, time.Time, int, time.Duration) ([]ports.OutboxEvent, error) {
	return nil, nil
}

func (f *fakeOutbox) MarkPublished(context.Context, uuid.UUID, time.Time) error { return nil }

func (f *fakeOutbox) MarkFailed(context.Context, uuid.UUID, time.Time, string, time.Time) error {
	return nil
}

func (f *fakeOutbox) PendingCount(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.events)), nil
}

type noopMetrics struct{}

func (noopMetrics) RecordWagerTransaction(string, string, string) {}
func (noopMetrics) RecordIdempotentReplay()                       {}
func (noopMetrics) RecordWalletVersionConflict()                  {}
func (noopMetrics) RecordWorkerRetry(string)                      {}
func (noopMetrics) RecordDLQMessage(string)                       {}
func (noopMetrics) RecordReconciliationRun(bool)                  {}
func (noopMetrics) RecordOutboxPublished(int, time.Duration)      {}
func (noopMetrics) SetOutboxPending(int64)                        {}
func (noopMetrics) SetPendingReferences(int64)                    {}

// --- fixtures --------------------------------------------------------------

type unitHarness struct {
	service      *Service
	wallets      *fakeWallets
	transactions *fakeTransactions
	ledger       *fakeLedger
	outbox       *fakeOutbox
}

func newUnitHarness(t *testing.T) *unitHarness {
	t.Helper()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	walletsFake := newFakeWallets()
	transactionsFake := newFakeTransactions()
	ledgerFake := &fakeLedger{}
	outboxFake := &fakeOutbox{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	service := NewService(
		fakeTxManager{},
		walletsFake,
		transactionsFake,
		ledgerFake,
		outboxFake,
		func() time.Time { return now },
		noopMetrics{},
		logger,
		Config{MaxWalletRetries: 5, ReferenceMaxAttempts: 3,
			ReferenceBaseBackoff: time.Millisecond, ReferenceMaxBackoff: 2 * time.Millisecond},
	)
	return &unitHarness{
		service:      service,
		wallets:      walletsFake,
		transactions: transactionsFake,
		ledger:       ledgerFake,
		outbox:       outboxFake,
	}
}

func (h *unitHarness) openWallet(t *testing.T, initial string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	playerID := uuid.Must(uuid.NewV7())
	balance := money.MustParse(initial, "BRL")
	opened, err := wallet.Open(playerID, balance, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NoError(t, h.wallets.Create(context.Background(), opened))
	return opened.ID(), playerID
}

func (h *unitHarness) command(playerID, walletID uuid.UUID, amount string) ProcessCommand {
	return ProcessCommand{
		ProviderID:            "provider-a",
		ExternalTransactionID: "tx-1",
		IdempotencyKey:        "provider-a:tx-1",
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               "round-1",
		GameID:                "fortune-chimp",
		Kind:                  wagering.KindBet,
		Money:                 money.MustParse(amount, "BRL"),
	}
}

// --- tests -----------------------------------------------------------------

func TestProcessPersistsAndReplays(t *testing.T) {
	harness := newUnitHarness(t)
	walletID, playerID := harness.openWallet(t, "100.00")
	cmd := harness.command(playerID, walletID, "25.00")

	first, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)
	assert.Equal(t, wagering.StatusProcessed, first.Status)
	assert.False(t, first.IdempotentReplay)
	assert.Equal(t, "75.00", first.Balance.AmountString())

	replay, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, first.Transaction.ID(), replay.Transaction.ID())
	assert.Equal(t, "75.00", replay.Balance.AmountString())

	// The wallet moved on; replay still returns the original observed balance.
	win := cmd
	win.ExternalTransactionID = "tx-2"
	win.IdempotencyKey = "provider-a:tx-2"
	win.Kind = wagering.KindWin
	_, err = harness.service.Process(context.Background(), win)
	require.NoError(t, err)

	replayAgain, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)
	assert.Equal(t, "75.00", replayAgain.Balance.AmountString())
}

func TestProcessRejectsPayloadConflictForSameKey(t *testing.T) {
	harness := newUnitHarness(t)
	walletID, playerID := harness.openWallet(t, "100.00")
	cmd := harness.command(playerID, walletID, "25.00")

	_, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)

	conflicting := cmd
	conflicting.Money = money.MustParse("26.00", "BRL")
	_, err = harness.service.Process(context.Background(), conflicting)
	require.Error(t, err)
	assert.Equal(t, "IDEMPOTENCY_CONFLICT", apperr.CodeOf(err))
	assert.Equal(t, apperr.KindConflict, apperr.KindOf(err))

	// No extra movement or ledger entry was created (only the first bet).
	opened, err := harness.wallets.GetByID(context.Background(), walletID)
	require.NoError(t, err)
	assert.Equal(t, "75.00", opened.Balance().AmountString())
	assert.Len(t, harness.ledger.entries, 1)
}

func TestProcessRejectsKeyReuseForAnotherTransaction(t *testing.T) {
	harness := newUnitHarness(t)
	walletID, playerID := harness.openWallet(t, "100.00")
	cmd := harness.command(playerID, walletID, "25.00")

	_, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)

	other := cmd
	other.ExternalTransactionID = "tx-2"
	// same idempotency key, different external transaction
	_, err = harness.service.Process(context.Background(), other)
	require.Error(t, err)
	assert.Equal(t, "IDEMPOTENCY_CONFLICT", apperr.CodeOf(err))
}

func TestProcessAllowsOtherKeyForSameExternalTransaction(t *testing.T) {
	harness := newUnitHarness(t)
	walletID, playerID := harness.openWallet(t, "100.00")
	cmd := harness.command(playerID, walletID, "25.00")

	first, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)

	otherKey := cmd
	otherKey.IdempotencyKey = "provider-a:another-key"
	replay, err := harness.service.Process(context.Background(), otherKey)
	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, first.Transaction.ID(), replay.Transaction.ID())
}

func TestProcessValidatesCommandBeforePersisting(t *testing.T) {
	harness := newUnitHarness(t)
	walletID, playerID := harness.openWallet(t, "100.00")

	cases := map[string]func(*ProcessCommand){
		"opening":  func(c *ProcessCommand) { c.Kind = wagering.KindOpening },
		"loss":     func(c *ProcessCommand) { c.Kind = wagering.KindLoss; c.Money = money.MustParse("1.00", "BRL") },
		"negative": func(c *ProcessCommand) { c.Money = money.MustParse("-1.00", "BRL") },
		"no-round": func(c *ProcessCommand) { c.RoundID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := harness.command(playerID, walletID, "10.00")
			mutate(&cmd)
			_, err := harness.service.Process(context.Background(), cmd)
			require.Error(t, err)
			assert.Equal(t, apperr.KindInvalid, apperr.KindOf(err))
		})
	}
}

func TestProcessInsufficientFundsIsRejectedAndAudited(t *testing.T) {
	harness := newUnitHarness(t)
	walletID, playerID := harness.openWallet(t, "10.00")
	cmd := harness.command(playerID, walletID, "10.01")

	result, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)
	assert.Equal(t, wagering.StatusRejected, result.Status)
	assert.Equal(t, wagering.FailureInsufficientFunds, result.FailureCode)

	replay, err := harness.service.Process(context.Background(), cmd)
	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, wagering.StatusRejected, replay.Status)
	assert.Equal(t, wagering.FailureInsufficientFunds, replay.FailureCode)
	assert.Len(t, harness.ledger.entries, 0)
}

func TestProcessUnknownWalletIsRejectedWithoutPersistence(t *testing.T) {
	harness := newUnitHarness(t)
	missingWallet := uuid.Must(uuid.NewV7())
	cmd := harness.command(uuid.Must(uuid.NewV7()), missingWallet, "10.00")

	_, err := harness.service.Process(context.Background(), cmd)
	require.Error(t, err)
	assert.Equal(t, wagering.FailureWalletNotFound, apperr.CodeOf(err))
	assert.Equal(t, apperr.KindRejected, apperr.KindOf(err))
}
