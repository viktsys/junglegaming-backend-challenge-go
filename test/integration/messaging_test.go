//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"strings"

	"github.com/junglegaming/backend-challenge-go/internal/adapters/sqs"
	"github.com/junglegaming/backend-challenge-go/internal/app/consumer"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

func newRawSQS(t *testing.T) *awssqs.Client {
	t.Helper()
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(envOr("TEST_AWS_REGION", "us-east-1")),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			envOr("TEST_AWS_ACCESS_KEY_ID", "test"), envOr("TEST_AWS_SECRET_ACCESS_KEY", "test"), "")),
	)
	require.NoError(t, err)
	return awssqs.NewFromConfig(awsCfg, func(options *awssqs.Options) {
		options.BaseEndpoint = aws.String(envOr("TEST_SQS_ENDPOINT", "http://localstack:4566"))
	})
}

func sendMessage(t *testing.T, api *awssqs.Client, queueURL, body, groupID, dedupID string) {
	t.Helper()
	_, err := api.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String(groupID),
		MessageDeduplicationId: aws.String(dedupID),
	})
	require.NoError(t, err)
}

func receiveFrom(t *testing.T, api *awssqs.Client, queueURL string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := api.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl:            aws.String(queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		require.NoError(t, err)
		if len(out.Messages) == 0 {
			continue
		}
		bodies := make([]string, 0, len(out.Messages))
		for _, message := range out.Messages {
			bodies = append(bodies, aws.ToString(message.Body))
			_, err := api.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: message.ReceiptHandle,
			})
			require.NoError(t, err)
		}
		return bodies
	}
	return nil
}

// crashAfterCommitHandler commits the message durably and then reports a
// transient failure, simulating a consumer crash after the commit and before
// the DeleteMessage call.
type crashAfterCommitHandler struct {
	inner ports.MessageHandler
	mu    sync.Mutex
	calls int
}

func (h *crashAfterCommitHandler) Handle(ctx context.Context, message ports.ReceivedMessage) error {
	h.mu.Lock()
	h.calls++
	firstCall := h.calls == 1
	h.mu.Unlock()

	if err := h.inner.Handle(ctx, message); err != nil {
		return err
	}
	if firstCall {
		return errors.New("simulated crash after commit before delete")
	}
	return nil
}

func (h *crashAfterCommitHandler) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// TestSQSRedeliveryAfterCommitBeforeDelete proves the message is redelivered
// when the consumer dies in the commit/delete window and that the redelivery
// is deduplicated by the inbox.
func TestSQSRedeliveryAfterCommitBeforeDelete(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	client := newSQSClientWith(t, 2*time.Second)
	inner := consumer.NewWagerHandler(inst.tx, inst.inboxRepo, inst.wagerSvc,
		func() time.Time { return time.Now().UTC() }, inst.metrics, testLogger())
	handler := &crashAfterCommitHandler{inner: inner}
	consumerRunner := sqs.NewConsumer(client, handler, inst.metrics, testLogger(), 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		consumerRunner.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("consumer did not stop in time")
		}
	}()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	externalID := "redelivery-tx-" + suffix
	body := mustJSON(t, map[string]any{
		"messageId":  "msg-redelivery-" + suffix,
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": externalID,
			"idempotencyKey":        "provider-a:" + externalID,
			"playerId":              playerID.String(),
			"walletId":              walletID.String(),
			"roundId":               "round-redelivery",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "10.00", "currency": "BRL"},
		},
	})
	sendMessage(t, newRawSQS(t), client.TransactionsQueueURL(), body, walletID.String(), "dedup-"+suffix)

	// The message is delivered at least twice: the first delivery commits but
	// is not acknowledged, so the visibility timeout expires and it is redelivered.
	require.Eventually(t, func() bool { return handler.callCount() >= 2 }, 20*time.Second, 200*time.Millisecond,
		"message must be redelivered after the simulated crash")

	// The redelivery is deduplicated: one movement, one inbox record.
	assertBalance(t, inst, walletID, "90.00")
	assertLedgerCount(t, inst, walletID, 2)

	var completed int64
	require.NoError(t, inst.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2 AND status = 'COMPLETED'`,
		consumer.ConsumerName, "msg-redelivery-"+suffix).Scan(&completed))
	assert.Equal(t, int64(1), completed)
}

// TestCrossPathIdempotency sends the same operation first through the HTTP use
// case and then through the SQS handler: the second path must replay the
// persisted result without moving money again.
func TestCrossPathIdempotency(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	cmd := command(playerID, walletID, wagering.KindBet, "40.00", "cross-path-1")
	httpResult := mustProcess(t, inst, cmd)
	require.Equal(t, wagering.StatusProcessed, httpResult.Status)

	body := mustJSON(t, map[string]any{
		"messageId":  "msg-cross-path-1",
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"providerId":            cmd.ProviderID,
			"externalTransactionId": cmd.ExternalTransactionID,
			"idempotencyKey":        cmd.IdempotencyKey,
			"playerId":              playerID.String(),
			"walletId":              walletID.String(),
			"roundId":               cmd.RoundID,
			"gameId":                cmd.GameID,
			"kind":                  string(cmd.Kind),
			"money":                 map[string]string{"amount": "40.00", "currency": "BRL"},
		},
	})
	handler := consumer.NewWagerHandler(inst.tx, inst.inboxRepo, inst.wagerSvc,
		func() time.Time { return time.Now().UTC() }, inst.metrics, testLogger())
	require.NoError(t, handler.Handle(context.Background(),
		ports.ReceivedMessage{MessageID: "msg-cross-path-1", ReceiptHandle: "r", Body: body, ReceiveCount: 1}))

	assertBalance(t, inst, walletID, "60.00")
	assertLedgerCount(t, inst, walletID, 2)
	assert.Equal(t, 1, inst.metrics.replays)
	assertReconciled(t, inst, walletID)
}

func TestSQSConsumerProcessesAndDeduplicates(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)
	walletID, playerID := openWallet(t, inst, "100.00")

	client := newSQSClient(t)
	handler := consumer.NewWagerHandler(inst.tx, inst.inboxRepo, inst.wagerSvc,
		func() time.Time { return time.Now().UTC() }, inst.metrics, testLogger())
	consumerRunner := sqs.NewConsumer(client, handler, inst.metrics, testLogger(), 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		consumerRunner.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("consumer did not stop in time")
		}
	}()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	externalID := "sqs-tx-" + suffix
	messageID := "msg-sqs-" + suffix

	api := newRawSQS(t)
	body := mustJSON(t, map[string]any{
		"messageId":  messageID,
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": externalID,
			"idempotencyKey":        "provider-a:" + externalID,
			"playerId":              playerID.String(),
			"walletId":              walletID.String(),
			"roundId":               "round-sqs",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "10.00", "currency": "BRL"},
		},
	})
	sendMessage(t, api, client.TransactionsQueueURL(), body, walletID.String(), "dedup-"+suffix)

	require.Eventually(t, func() bool {
		transaction, err := inst.wagerSvc.GetByProviderExternal(context.Background(), "provider-a", externalID)
		return err == nil && transaction.Status() == wagering.StatusProcessed
	}, 15*time.Second, 100*time.Millisecond, "message must be processed")

	// Redelivery with the same envelope messageId (new SQS dedup id) must not
	// apply the operation twice.
	sendMessage(t, api, client.TransactionsQueueURL(), body, walletID.String(), "dedup-redelivery-"+suffix)
	time.Sleep(2 * time.Second)
	assertBalance(t, inst, walletID, "90.00")
	assertLedgerCount(t, inst, walletID, 2)

	// An unprocessable message is routed to the DLQ and removed from the main queue.
	invalidBody := `{"messageId":"msg-invalid-` + suffix + `","type":"WagerTransactionRequested","data":{"kind":"OPENING"}}`
	sendMessage(t, api, client.TransactionsQueueURL(), invalidBody, "invalid-group", "invalid-"+uuid.NewString())

	dlqMessages := receiveFrom(t, api, client.TransactionsDLQURL(), 15*time.Second)
	require.NotEmpty(t, dlqMessages, "invalid message must reach the DLQ")
	assert.Contains(t, dlqMessages[0], "msg-invalid-"+suffix)
}
