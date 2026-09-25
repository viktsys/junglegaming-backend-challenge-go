//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/app/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
)

// TestSameBetSubmittedFiftyTimesInParallel proves application-level
// deduplication: exactly one debit regardless of how many processes race.
func TestSameBetSubmittedFiftyTimesInParallel(t *testing.T) {
	resetDatabase(t)
	instances := []*instance{newInstance(t), newInstance(t), newInstance(t)}
	walletID, playerID := openWallet(t, instances[0], "100.00")

	bet := command(playerID, walletID, wagering.KindBet, "80.00", "race-bet")
	const submissions = 50

	var (
		waitGroup sync.WaitGroup
		mu        sync.Mutex
		processed int
		replays   int
		failures  []error
	)
	for i := 0; i < submissions; i++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			inst := instances[index%len(instances)]
			result, err := inst.wagerSvc.Process(context.Background(), bet)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			if result.IdempotentReplay {
				replays++
			} else {
				processed++
			}
		}(i)
	}
	waitGroup.Wait()

	require.Empty(t, failures)
	assert.Equal(t, 1, processed, "exactly one submission applies the operation")
	assert.Equal(t, submissions-1, replays)
	assertBalance(t, instances[0], walletID, "20.00")

	// Opening credit plus a single bet debit.
	assertLedgerCount(t, instances[0], walletID, 2)
	assertReconciled(t, instances[0], walletID)

	// Resends cannot change the outcome.
	for i := 0; i < 10; i++ {
		replay := mustProcess(t, instances[i%len(instances)], bet)
		assert.True(t, replay.IdempotentReplay)
		assert.Equal(t, "20.00", replay.Balance.AmountString())
	}
	assertBalance(t, instances[0], walletID, "20.00")
	assertLedgerCount(t, instances[0], walletID, 2)
}

// TestTwoEightyBetsOverOneHundred is the mandatory race: one bet must win,
// the other must be rejected for insufficient funds, with a single debit.
func TestTwoEightyBetsOverOneHundred(t *testing.T) {
	resetDatabase(t)
	instances := []*instance{newInstance(t), newInstance(t), newInstance(t)}
	walletID, playerID := openWallet(t, instances[0], "100.00")

	first := command(playerID, walletID, wagering.KindBet, "80.00", "bet-a")
	second := command(playerID, walletID, wagering.KindBet, "80.00", "bet-b")

	type outcome struct {
		externalID string
		result     wager.Result
		err        error
	}
	results := make(chan outcome, 3)
	run := func(inst *instance, cmd wager.ProcessCommand) {
		result, err := inst.wagerSvc.Process(context.Background(), cmd)
		results <- outcome{externalID: cmd.ExternalTransactionID, result: result, err: err}
	}
	// Three independent instances: both distinct bets plus a concurrent
	// duplicate of the first one submitted through the third process.
	go run(instances[0], first)
	go run(instances[1], second)
	go run(instances[2], first)

	var processed, rejected, replays int
	for i := 0; i < 3; i++ {
		got := <-results
		require.NoError(t, got.err)
		if got.result.IdempotentReplay {
			replays++
			assert.Equal(t, first.ExternalTransactionID, got.externalID)
			continue
		}
		switch got.result.Status {
		case wagering.StatusProcessed:
			processed++
		case wagering.StatusRejected:
			rejected++
			assert.Equal(t, wagering.FailureInsufficientFunds, got.result.FailureCode)
		default:
			t.Fatalf("unexpected status %s", got.result.Status)
		}
	}
	assert.Equal(t, 1, processed)
	assert.Equal(t, 1, rejected)
	assert.Equal(t, 1, replays)
	assertBalance(t, instances[0], walletID, "20.00")

	// Opening credit plus a single bet debit.
	assertLedgerCount(t, instances[0], walletID, 2)

	totals := ledgerTotals(t, instances[0], walletID)
	assert.Equal(t, int64(10000), totals.CreditsMinor)
	assert.Equal(t, int64(8000), totals.DebitsMinor)
	assertReconciled(t, instances[0], walletID)

	// Resending both operations cannot change the result.
	for i := 0; i < 10; i++ {
		mustProcess(t, instances[i%len(instances)], first)
		mustProcess(t, instances[i%len(instances)], second)
	}
	assertBalance(t, instances[0], walletID, "20.00")
	assertLedgerCount(t, instances[0], walletID, 2)
}

// TestDistinctWalletsAdvanceInParallel proves there is no global lock.
func TestDistinctWalletsAdvanceInParallel(t *testing.T) {
	resetDatabase(t)
	instances := []*instance{newInstance(t), newInstance(t), newInstance(t)}

	const walletCount = 9
	type walletFixture struct {
		walletID uuid.UUID
		playerID uuid.UUID
	}
	fixtures := make([]walletFixture, 0, walletCount)
	for i := 0; i < walletCount; i++ {
		walletID, playerID := openWallet(t, instances[0], "100.00")
		fixtures = append(fixtures, walletFixture{walletID: walletID, playerID: playerID})
	}

	var waitGroup sync.WaitGroup
	errors := make(chan error, walletCount)
	for i, fixture := range fixtures {
		waitGroup.Add(1)
		go func(index int, fixture walletFixture) {
			defer waitGroup.Done()
			inst := instances[index%len(instances)]
			cmd := command(fixture.playerID, fixture.walletID, wagering.KindBet, "10.00",
				"parallel-"+fixture.walletID.String())
			if _, err := inst.wagerSvc.Process(context.Background(), cmd); err != nil {
				errors <- err
			}
		}(i, fixture)
	}
	waitGroup.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}

	for _, fixture := range fixtures {
		assertBalance(t, instances[0], fixture.walletID, "90.00")
		assertReconciled(t, instances[0], fixture.walletID)
	}
}
