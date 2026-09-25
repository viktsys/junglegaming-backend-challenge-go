//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/app/wallets"
	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
)

func TestOpeningCreatesTransactionLedgerAndEvents(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "1000.00")

	found, err := inst.walletSvc.Get(context.Background(), walletID)
	require.NoError(t, err)
	assert.Equal(t, "1000.00", found.Balance().AmountString())
	assert.Equal(t, int64(1), found.Version())
	assert.Equal(t, playerID, found.PlayerID())

	totals := ledgerTotals(t, inst, walletID)
	assert.Equal(t, int64(1), totals.Entries)
	assert.Equal(t, int64(100000), totals.CreditsMinor)

	var kind, status string
	err = inst.pool.QueryRow(context.Background(),
		`SELECT kind, status FROM wager_transactions WHERE wallet_id = $1`, walletID).Scan(&kind, &status)
	require.NoError(t, err)
	assert.Equal(t, "OPENING", kind)
	assert.Equal(t, "PROCESSED", status)

	pending, err := inst.outboxRepo.PendingCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), pending, "opening emits WalletBalanceChanged and WagerTransactionProcessed")
}

func TestZeroOpeningCreatesNoOpeningArtifacts(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, _ := openWallet(t, inst, "0.00")

	assertBalance(t, inst, walletID, "0.00")
	assertLedgerCount(t, inst, walletID, 0)

	var count int64
	require.NoError(t, inst.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id = $1`, walletID).Scan(&count))
	assert.Zero(t, count)

	pending, err := inst.outboxRepo.PendingCount(context.Background())
	require.NoError(t, err)
	assert.Zero(t, pending)
}

func TestDuplicateWalletConflict(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "10.00")

	balance := money.MustParse("10.00", "BRL")
	_, err := inst.walletSvc.Open(context.Background(), walletsOpenCommand(playerID, balance))
	require.Error(t, err)
	assert.Equal(t, apperr.KindConflict, apperr.KindOf(err))
	assert.NotEqual(t, uuid.Nil, walletID)
}

func TestFiveExternalOperationTypes(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")
	ctx := context.Background()

	bet := mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "25.00", "bet-1"))
	require.Equal(t, wagering.StatusProcessed, bet.Status)
	assertBalance(t, inst, walletID, "75.00")

	win := mustProcess(t, inst, command(playerID, walletID, wagering.KindWin, "10.00", "win-1"))
	require.Equal(t, wagering.StatusProcessed, win.Status)
	assertBalance(t, inst, walletID, "85.00")

	loss := mustProcess(t, inst, command(playerID, walletID, wagering.KindLoss, "0.00", "loss-1"))
	require.Equal(t, wagering.StatusProcessed, loss.Status)
	assertBalance(t, inst, walletID, "85.00")

	refund := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRefund, "25.00", "refund-1"), "bet-1"))
	require.Equal(t, wagering.StatusProcessed, refund.Status)
	assertBalance(t, inst, walletID, "110.00")

	rollback := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRollback, "10.00", "rollback-1"), "win-1"))
	require.Equal(t, wagering.StatusProcessed, rollback.Status)
	assertBalance(t, inst, walletID, "100.00")

	// Opening + BET + WIN + REFUND + ROLLBACK = 5 ledger entries; LOSS none.
	assertLedgerCount(t, inst, walletID, 5)
	assertReconciled(t, inst, walletID)

	// The wallet version moved on each balance change but not for LOSS.
	found, err := inst.walletsRepo.GetByID(ctx, walletID)
	require.NoError(t, err)
	assert.Equal(t, int64(5), found.Version())
}

func TestLossDoesNotCreateLedgerOrVersionBump(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "10.00")

	mustProcess(t, inst, command(playerID, walletID, wagering.KindLoss, "0.00", "loss-1"))

	found, err := inst.walletsRepo.GetByID(context.Background(), walletID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), found.Version())
	assertLedgerCount(t, inst, walletID, 1)
	assertReconciled(t, inst, walletID)
}

func TestInsufficientFundsRejectionIsPersisted(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "50.00")

	result := mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "50.01", "bet-1"))
	require.Equal(t, wagering.StatusRejected, result.Status)
	assert.Equal(t, wagering.FailureInsufficientFunds, result.FailureCode)
	assertBalance(t, inst, walletID, "50.00")
	assertLedgerCount(t, inst, walletID, 1)

	// The rejection is auditable and replayed with the same code.
	replay := mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "50.01", "bet-1"))
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, wagering.FailureInsufficientFunds, replay.FailureCode)
}

func TestReversalInsufficientFundsHasDistinctCode(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	mustProcess(t, inst, command(playerID, walletID, wagering.KindWin, "50.00", "win-1"))
	mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "100.00", "bet-1"))
	mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "40.00", "bet-2"))
	assertBalance(t, inst, walletID, "10.00")

	result := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRollback, "50.00", "rollback-1"), "win-1"))
	require.Equal(t, wagering.StatusRejected, result.Status)
	assert.Equal(t, wagering.FailureReversalInsufficientFunds, result.FailureCode)
	assert.NotEqual(t, wagering.FailureInsufficientFunds, result.FailureCode)
	assertBalance(t, inst, walletID, "10.00")
	assertReconciled(t, inst, walletID)
}

func TestReversalConflicts(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "30.00", "bet-1"))

	refund := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRefund, "30.00", "refund-1"), "bet-1"))
	require.Equal(t, wagering.StatusProcessed, refund.Status)
	assertBalance(t, inst, walletID, "100.00")

	// A second successful refund of the same bet is impossible.
	secondRefund := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRefund, "30.00", "refund-2"), "bet-1"))
	require.Equal(t, wagering.StatusRejected, secondRefund.Status)
	assert.Equal(t, wagering.FailureReversalConflict, secondRefund.FailureCode)

	// A rollback of the already refunded bet is also impossible.
	rollbackBet := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRollback, "30.00", "rollback-1"), "bet-1"))
	require.Equal(t, wagering.StatusRejected, rollbackBet.Status)
	assert.Equal(t, wagering.FailureReversalConflict, rollbackBet.FailureCode)

	// Rolling back the refund itself is valid and debits the credited amount.
	rollbackRefund := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRollback, "30.00", "rollback-2"), "refund-1"))
	require.Equal(t, wagering.StatusProcessed, rollbackRefund.Status)
	assertBalance(t, inst, walletID, "70.00")

	// ... but only once.
	again := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRollback, "30.00", "rollback-3"), "refund-1"))
	require.Equal(t, wagering.StatusRejected, again.Status)
	assert.Equal(t, wagering.FailureReversalConflict, again.FailureCode)
	assertReconciled(t, inst, walletID)
}

func TestReferenceValidationFailures(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "20.00", "bet-1"))
	mustProcess(t, inst, command(playerID, walletID, wagering.KindWin, "5.00", "win-1"))

	// Amount must match the reference.
	amountMismatch := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRefund, "21.00", "refund-1"), "bet-1"))
	require.Equal(t, wagering.StatusRejected, amountMismatch.Status)
	assert.Equal(t, wagering.FailureReferenceAmountMismatch, amountMismatch.FailureCode)

	// REFUND can only reference a BET.
	wrongKind := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRefund, "5.00", "refund-2"), "win-1"))
	require.Equal(t, wagering.StatusRejected, wrongKind.Status)
	assert.Equal(t, wagering.FailureReferenceMismatch, wrongKind.FailureCode)

	// The reference must belong to the same wallet.
	otherWallet, otherPlayer := openWallet(t, inst, "100.00")
	otherWalletBet := mustProcess(t, inst, command(otherPlayer, otherWallet, wagering.KindBet, "20.00", "bet-other"))
	require.Equal(t, wagering.StatusProcessed, otherWalletBet.Status)
	crossWallet := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRefund, "20.00", "refund-3"), "bet-other"))
	require.Equal(t, wagering.StatusRejected, crossWallet.Status)
	assert.Equal(t, wagering.FailureReferenceMismatch, crossWallet.FailureCode)

	// A rejected reference cannot be reversed.
	rejectedBet := mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "1000.00", "bet-rejected"))
	require.Equal(t, wagering.StatusRejected, rejectedBet.Status)
	refundRejected := mustProcess(t, inst, withReference(
		command(playerID, walletID, wagering.KindRefund, "1000.00", "refund-4"), "bet-rejected"))
	require.Equal(t, wagering.StatusRejected, refundRejected.Status)
	assert.Equal(t, wagering.FailureReferenceNotProcessed, refundRejected.FailureCode)

	assertReconciled(t, inst, walletID)
}

func TestIdempotencyReplayAndConflicts(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")
	ctx := context.Background()

	bet := command(playerID, walletID, wagering.KindBet, "25.00", "bet-1")
	first := mustProcess(t, inst, bet)
	require.Equal(t, wagering.StatusProcessed, first.Status)

	// Same key, same payload: replay of the persisted result.
	replay := mustProcess(t, inst, bet)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, first.Transaction.ID(), replay.Transaction.ID())
	assert.Equal(t, "75.00", replay.Balance.AmountString())
	assert.Equal(t, wagering.StatusProcessed, replay.Status)

	// The wallet moves on; replay still returns the original observed balance.
	mustProcess(t, inst, command(playerID, walletID, wagering.KindWin, "5.00", "win-1"))
	replayAgain := mustProcess(t, inst, bet)
	assert.Equal(t, "75.00", replayAgain.Balance.AmountString())

	// Same external transaction with a different payload is a conflict.
	changed := bet
	changed.Money = money.MustParse("26.00", "BRL")
	_, err := inst.wagerSvc.Process(ctx, changed)
	require.Error(t, err)
	assert.Equal(t, "IDEMPOTENCY_CONFLICT", apperr.CodeOf(err))
	assert.Equal(t, apperr.KindConflict, apperr.KindOf(err))

	// Same external transaction with another key cannot reapply the operation.
	anotherKey := bet
	anotherKey.IdempotencyKey = "provider-a:other-key"
	replayByExternal := mustProcess(t, inst, anotherKey)
	assert.True(t, replayByExternal.IdempotentReplay)

	// A key reused for a different external transaction is a conflict.
	keyReuse := command(playerID, walletID, wagering.KindBet, "1.00", "bet-2")
	keyReuse.IdempotencyKey = bet.IdempotencyKey
	_, err = inst.wagerSvc.Process(ctx, keyReuse)
	require.Error(t, err)
	assert.Equal(t, "IDEMPOTENCY_CONFLICT", apperr.CodeOf(err))

	assertBalance(t, inst, walletID, "80.00")
	assertLedgerCount(t, inst, walletID, 3)
	assertReconciled(t, inst, walletID)
}

func TestLedgerIsAppendOnly(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, _ := openWallet(t, inst, "10.00")
	ctx := context.Background()

	var entryID uuid.UUID
	require.NoError(t, inst.pool.QueryRow(ctx,
		`SELECT id FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&entryID))

	_, err := inst.pool.Exec(ctx,
		`UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE id = $1`, entryID)
	require.Error(t, err, "updates must be rejected by the database")

	_, err = inst.pool.Exec(ctx, `DELETE FROM wallet_ledger_entries WHERE id = $1`, entryID)
	require.Error(t, err, "deletes must be rejected by the database")

	var count int64
	require.NoError(t, inst.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&count))
	assert.Equal(t, int64(1), count)
}

func TestTerminalTransactionsAreImmutable(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")
	ctx := context.Background()

	bet := mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "10.00", "immutable-1"))
	require.Equal(t, wagering.StatusProcessed, bet.Status)

	_, err := inst.pool.Exec(ctx,
		`UPDATE wager_transactions SET amount_minor = 999 WHERE id = $1`, bet.Transaction.ID())
	require.Error(t, err, "terminal transactions must be immutable in the database")

	rejected := mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "1000.00", "immutable-2"))
	require.Equal(t, wagering.StatusRejected, rejected.Status)
	_, err = inst.pool.Exec(ctx,
		`UPDATE wager_transactions SET failure_code = 'OTHER' WHERE id = $1`, rejected.Transaction.ID())
	require.Error(t, err, "rejected transactions must be immutable in the database")
}

func TestLedgerCannotBeTruncated(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	openWallet(t, inst, "10.00")

	_, err := inst.pool.Exec(context.Background(), `TRUNCATE wallet_ledger_entries`)
	require.Error(t, err, "the append-only trigger must also protect against TRUNCATE")
}

func TestDatabaseRejectsNegativeBalance(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, _ := openWallet(t, inst, "10.00")
	ctx := context.Background()

	_, err := inst.pool.Exec(ctx,
		`UPDATE wallets SET balance_minor = -1 WHERE id = $1`, walletID)
	require.Error(t, err, "the check constraint must reject negative balances")
}

func TestDatabaseEnforcesUniqueWalletPerPlayerAndCurrency(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	_, playerID := openWallet(t, inst, "10.00")
	ctx := context.Background()

	otherID := randomUUID(t)
	_, err := inst.pool.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, 'BRL', 0, 1, now(), now())`, otherID, playerID)
	require.Error(t, err)
}

func TestDatabaseEnforcesSingleOpeningPerWallet(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "10.00")
	ctx := context.Background()

	openingID := randomUUID(t)
	_, err := inst.pool.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, origin, wallet_id, player_id, kind, amount_minor, currency, status, result_balance_minor, created_at, updated_at)
		VALUES ($1, 'INTERNAL', $2, $3, 'OPENING', 1000, 'BRL', 'PROCESSED', 1000, now(), now())`,
		openingID, walletID, playerID)
	require.Error(t, err, "a wallet may have only one opening credit")
}

func walletsOpenCommand(playerID uuid.UUID, balance money.Money) wallets.OpenCommand {
	return wallets.OpenCommand{PlayerID: playerID, InitialBalance: balance, CorrelationID: "test"}
}
