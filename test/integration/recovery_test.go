//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/app/consumer"
	"github.com/junglegaming/backend-challenge-go/internal/app/outbox"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

func TestReferenceArrivingAfterReversal(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	refund := withReference(command(playerID, walletID, wagering.KindRefund, "30.00", "refund-1"), "bet-late")
	parked := mustProcess(t, inst, refund)
	require.Equal(t, wagering.StatusPendingReference, parked.Status)

	// The reference arrives and wakes the pending refund.
	bet := mustProcess(t, inst, command(playerID, walletID, wagering.KindBet, "30.00", "bet-late"))
	require.Equal(t, wagering.StatusProcessed, bet.Status)
	assertBalance(t, inst, walletID, "70.00")

	processed := resumeUntilTerminal(t, inst, parked.Transaction.ID())
	require.Equal(t, wagering.StatusProcessed, processed.Status())
	assertBalance(t, inst, walletID, "100.00")
	assertReconciled(t, inst, walletID)
}

func TestReferenceExpiryRejectsWithStableCode(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t, func(options *instanceOptions) {
		options.referenceMaxAttempts = 3
		options.referenceBaseBackoff = time.Millisecond
		options.referenceMaxBackoff = 2 * time.Millisecond
	})
	walletID, playerID := openWallet(t, inst, "100.00")

	refund := withReference(command(playerID, walletID, wagering.KindRefund, "30.00", "refund-1"), "never-arrives")
	parked := mustProcess(t, inst, refund)
	require.Equal(t, wagering.StatusPendingReference, parked.Status)

	rejected := resumeUntilTerminal(t, inst, parked.Transaction.ID())
	require.Equal(t, wagering.StatusRejected, rejected.Status())
	assert.Equal(t, wagering.FailureReferenceNotFound, rejected.FailureCode())
	assert.True(t, wagering.IsCorrectableFailure(rejected.FailureCode()))
	assertBalance(t, inst, walletID, "100.00")
	assertReconciled(t, inst, walletID)

	// The rejection emitted WagerTransactionRejected to the outbox.
	var eventTypes []string
	rows, err := inst.pool.Query(context.Background(),
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY occurred_at`, rejected.ID())
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var eventType string
		require.NoError(t, rows.Scan(&eventType))
		eventTypes = append(eventTypes, eventType)
	}
	assert.Contains(t, eventTypes, "WagerTransactionPendingReference")
	assert.Contains(t, eventTypes, "WagerTransactionRejected")
}

func TestRestartPreservesIdempotencyAndPendings(t *testing.T) {
	resetDatabase(t)
	before := newInstance(t)
	walletID, playerID := openWallet(t, before, "100.00")

	bet := command(playerID, walletID, wagering.KindBet, "25.00", "bet-1")
	first := mustProcess(t, before, bet)
	require.Equal(t, wagering.StatusProcessed, first.Status)

	refund := withReference(command(playerID, walletID, wagering.KindRefund, "25.00", "refund-1"), "bet-1")
	// Park a reversal by referencing a transaction that does not exist yet.
	otherBet := withReference(command(playerID, walletID, wagering.KindRefund, "1.00", "refund-2"), "bet-missing")
	parked := mustProcess(t, before, otherBet)
	require.Equal(t, wagering.StatusPendingReference, parked.Status)

	// "Restart": brand new instances with new pools and in-memory state.
	after := newInstance(t)
	replay := mustProcess(t, after, bet)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, first.Transaction.ID(), replay.Transaction.ID())
	assert.Equal(t, "75.00", replay.Balance.AmountString())

	processedRefund := mustProcess(t, after, refund)
	assert.Equal(t, wagering.StatusProcessed, processedRefund.Status)
	assertBalance(t, after, walletID, "100.00")

	// The pending reversal is still retried by the new process.
	stillPending := mustProcess(t, after, otherBet)
	assert.True(t, stillPending.IdempotentReplay)
	assert.Equal(t, wagering.StatusPendingReference, stillPending.Status)
	assertReconciled(t, after, walletID)
}

func TestInboxDeduplicatesRedeliveries(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	body := mustJSON(t, map[string]any{
		"messageId":  "msg-inbox-1",
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": "inbox-tx-1",
			"idempotencyKey":        "provider-a:inbox-tx-1",
			"playerId":              playerID.String(),
			"walletId":              walletID.String(),
			"roundId":               "round-inbox",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "10.00", "currency": "BRL"},
		},
	})

	handler := consumer.NewWagerHandler(inst.tx, inst.inboxRepo, inst.wagerSvc, func() time.Time {
		return time.Now().UTC()
	}, inst.metrics, testLogger())

	message := ports.ReceivedMessage{MessageID: "msg-inbox-1", ReceiptHandle: "receipt-1", Body: body, ReceiveCount: 1}
	require.NoError(t, handler.Handle(context.Background(), message))
	require.NoError(t, handler.Handle(context.Background(), message), "redelivery must be ignored")

	assertBalance(t, inst, walletID, "90.00")
	assertLedgerCount(t, inst, walletID, 2)

	var completed int64
	require.NoError(t, inst.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM inbox_messages WHERE consumer_name = $1 AND status = 'COMPLETED'`,
		consumer.ConsumerName).Scan(&completed))
	assert.Equal(t, int64(1), completed)

	// A message id reused with a different body is unprocessable.
	tampered := ports.ReceivedMessage{MessageID: "msg-inbox-1", ReceiptHandle: "receipt-2", Body: body + " ", ReceiveCount: 1}
	err := handler.Handle(context.Background(), tampered)
	require.Error(t, err)
	assert.True(t, ports.IsPermanent(err))
}

func TestOutboxConcurrentPublishersAndAbandonedLockRecovery(t *testing.T) {
	resetDatabase(t)
	first := newInstance(t)
	second := newInstance(t)
	walletID, _ := openWallet(t, first, "10.00")

	// Opening produced two pending events.
	pending, err := first.outboxRepo.PendingCount(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), pending)

	var waitGroup sync.WaitGroup
	for _, inst := range []*instance{first, second} {
		waitGroup.Add(1)
		go func(inst *instance) {
			defer waitGroup.Done()
			for i := 0; i < 10; i++ {
				if _, err := inst.publisher.PublishDue(context.Background()); err != nil {
					t.Errorf("publish due: %v", err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(inst)
	}
	waitGroup.Wait()

	pending, err = first.outboxRepo.PendingCount(context.Background())
	require.NoError(t, err)
	assert.Zero(t, pending, "all events must be published")

	// Both publishers may publish the same event (at-least-once), but the
	// stable eventId means consumers can deduplicate.
	total := first.eventSink.publishedCount() + second.eventSink.publishedCount()
	assert.GreaterOrEqual(t, total, 2)

	// Simulate a publisher crashing after claiming: the lock expires and the
	// event is republished by another instance.
	insertTestEvent(t, first, walletID, "abandoned-event")
	claimed, err := first.outboxRepo.Claim(context.Background(), "crashed-publisher", time.Now().UTC(), 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	_, err = first.pool.Exec(context.Background(),
		`UPDATE outbox_events SET locked_at = now() - interval '10 minutes' WHERE event_id = $1`, claimed[0].EventID)
	require.NoError(t, err)

	// A different publisher with a shorter TTL reclaims and publishes it.
	published, err := second.publisher.PublishDue(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, published, 1)
	assert.Equal(t, 1, second.eventSink.attemptCount(claimed[0].EventID))
}

// markPublishedFailingRepo simulates a database failure when confirming a
// publication that already happened at the broker.
type markPublishedFailingRepo struct {
	ports.OutboxRepository
	fail bool
}

func (r *markPublishedFailingRepo) MarkPublished(ctx context.Context, eventID uuid.UUID, now time.Time) error {
	if r.fail {
		return errors.New("simulated failure confirming publication")
	}
	return r.OutboxRepository.MarkPublished(ctx, eventID, now)
}

// TestOutboxRepublicationPreservesEventID covers the crash window between a
// successful broker publication and the outbox confirmation: another publisher
// republishes the same eventId.
func TestOutboxRepublicationPreservesEventID(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	openWallet(t, inst, "10.00")
	ctx := context.Background()

	failingRepo := &markPublishedFailingRepo{OutboxRepository: inst.outboxRepo, fail: true}
	firstPublisher := outbox.NewPublisher(inst.tx, failingRepo, inst.eventSink,
		func() time.Time { return time.Now().UTC() }, inst.metrics, testLogger(),
		outbox.Config{BatchSize: 50, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond,
			LockTTL: time.Millisecond, PublisherID: "publisher-a"})

	published, err := firstPublisher.PublishDue(ctx)
	require.NoError(t, err)
	assert.Zero(t, published, "the publication happened but was not confirmed")
	assert.Equal(t, 2, inst.eventSink.publishedCount())

	pending, err := inst.outboxRepo.PendingCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), pending, "events remain pending until confirmation")

	time.Sleep(20 * time.Millisecond)
	secondPublisher := outbox.NewPublisher(inst.tx, inst.outboxRepo, inst.eventSink,
		func() time.Time { return time.Now().UTC() }, inst.metrics, testLogger(),
		outbox.Config{BatchSize: 50, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond,
			LockTTL: time.Millisecond, PublisherID: "publisher-b"})

	published, err = secondPublisher.PublishDue(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, published)

	pending, err = inst.outboxRepo.PendingCount(ctx)
	require.NoError(t, err)
	assert.Zero(t, pending)

	// Every event was published twice with the same stable eventId.
	inst.eventSink.mu.Lock()
	defer inst.eventSink.mu.Unlock()
	assert.Len(t, inst.eventSink.events, 2)
	for eventID, attempts := range inst.eventSink.attempts {
		assert.Equal(t, 2, attempts, "event %s must be republished with the same id", eventID)
	}
}

func TestOutboxRetriesAfterBrokerFailure(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	openWallet(t, inst, "10.00")

	inst.eventSink.failNext(2)
	published, err := inst.publisher.PublishDue(context.Background())
	require.NoError(t, err)
	assert.Zero(t, published)

	pending, err := inst.outboxRepo.PendingCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), pending)

	time.Sleep(20 * time.Millisecond)
	published, err = inst.publisher.PublishDue(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, published)
	pending, err = inst.outboxRepo.PendingCount(context.Background())
	require.NoError(t, err)
	assert.Zero(t, pending)
}

func resumeUntilTerminal(t *testing.T, inst *instance, transactionID uuid.UUID) *wagering.Transaction {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := inst.wagerSvc.ResumeDue(context.Background(), 10); err != nil {
			require.NoError(t, err)
		}
		transaction, err := inst.wagerSvc.GetTransaction(context.Background(), transactionID)
		require.NoError(t, err)
		if transaction.IsTerminal() {
			return transaction
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("transaction did not reach a terminal state in time")
	return nil
}

func insertTestEvent(t *testing.T, inst *instance, aggregateID uuid.UUID, eventType string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"eventId": randomUUID(t).String(), "eventType": eventType})
	require.NoError(t, err)
	require.NoError(t, inst.outboxRepo.Insert(context.Background(), ports.OutboxEvent{
		EventID:       randomUUID(t),
		AggregateID:   aggregateID,
		EventType:     eventType,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
		NextAttemptAt: time.Now().UTC(),
	}))
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}
