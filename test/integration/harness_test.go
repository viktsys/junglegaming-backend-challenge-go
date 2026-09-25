//go:build integration

// Package integration exercises the service against real PostgreSQL, Keycloak
// and LocalStack instances. It is excluded from the default build and runs
// with:
//
//	go test -race -tags=integration ./test/integration/...
//
// Environment variables (defaults target the Docker Compose network):
//
//	TEST_DATABASE_URL     postgres://wager:wager@postgres:5432/wager?sslmode=disable
//	TEST_SQS_ENDPOINT     http://localstack:4566
//	TEST_OIDC_ISSUER_URL  http://keycloak:8080/realms/wager
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/junglegaming/backend-challenge-go/internal/adapters/postgres"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/sqs"
	"github.com/junglegaming/backend-challenge-go/internal/app/outbox"
	"github.com/junglegaming/backend-challenge-go/internal/app/wager"
	"github.com/junglegaming/backend-challenge-go/internal/app/wallets"
	"github.com/junglegaming/backend-challenge-go/internal/config"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/platform/migrate"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
	"github.com/junglegaming/backend-challenge-go/migrations"
)

var (
	testSchema string
	baseDSN    string
)

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func TestMain(m *testing.M) {
	baseDSN = envOr("TEST_DATABASE_URL", "postgres://wager:wager@postgres:5432/wager?sslmode=disable")
	testSchema = "it_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot connect to database: %v\n", err)
		os.Exit(1)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+testSchema); err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot create schema: %v\n", err)
		admin.Close()
		os.Exit(1)
	}

	pool, err := newTestPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot open schema pool: %v\n", err)
		admin.Close()
		os.Exit(1)
	}
	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot load migrations: %v\n", err)
		os.Exit(1)
	}
	if _, err := migrate.Up(ctx, pool, loaded); err != nil {
		fmt.Fprintf(os.Stderr, "integration: migrations failed: %v\n", err)
		os.Exit(1)
	}
	pool.Close()

	code := m.Run()

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
	defer cleanupCancel()
	_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+testSchema+" CASCADE")
	admin.Close()
	os.Exit(code)
}

func newTestPool(ctx context.Context) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(baseDSN)
	if err != nil {
		return nil, err
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = testSchema
	poolConfig.MaxConns = 20
	return pgxpool.NewWithConfig(ctx, poolConfig)
}

// instance is one independent "process": its own pool, connections and memory.
type instance struct {
	pool        *pgxpool.Pool
	tx          *postgres.TxManager
	walletsRepo *postgres.WalletRepository
	txRepo      *postgres.TransactionRepository
	ledgerRepo  *postgres.LedgerRepository
	inboxRepo   *postgres.InboxRepository
	outboxRepo  *postgres.OutboxRepository
	walletSvc   *wallets.Service
	wagerSvc    *wager.Service
	metrics     *testMetrics
	publisher   *outbox.Publisher
	eventSink   *fakePublisher
}

type instanceOptions struct {
	referenceMaxAttempts int
	referenceBaseBackoff time.Duration
	referenceMaxBackoff  time.Duration
	maxWalletRetries     int
}

func defaultInstanceOptions() instanceOptions {
	return instanceOptions{
		referenceMaxAttempts: 5,
		referenceBaseBackoff: time.Millisecond,
		referenceMaxBackoff:  10 * time.Millisecond,
		maxWalletRetries:     5,
	}
}

func newInstance(t *testing.T, opts ...func(*instanceOptions)) *instance {
	t.Helper()
	options := defaultInstanceOptions()
	for _, apply := range opts {
		apply(&options)
	}

	ctx := context.Background()
	pool, err := newTestPool(ctx)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	metrics := &testMetrics{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clock := func() time.Time { return time.Now().UTC() }

	inst := &instance{
		pool:        pool,
		tx:          postgres.NewTxManager(pool),
		walletsRepo: postgres.NewWalletRepository(pool),
		txRepo:      postgres.NewTransactionRepository(pool),
		ledgerRepo:  postgres.NewLedgerRepository(pool),
		inboxRepo:   postgres.NewInboxRepository(pool),
		outboxRepo:  postgres.NewOutboxRepository(pool),
		metrics:     metrics,
		eventSink:   newFakePublisher(),
	}

	inst.walletSvc = wallets.NewService(
		inst.tx, inst.walletsRepo, inst.txRepo, inst.ledgerRepo, inst.outboxRepo, clock, metrics, logger)
	inst.wagerSvc = wager.NewService(
		inst.tx, inst.walletsRepo, inst.txRepo, inst.ledgerRepo, inst.outboxRepo, clock, metrics, logger,
		wager.Config{
			MaxWalletRetries:     options.maxWalletRetries,
			ReferenceMaxAttempts: options.referenceMaxAttempts,
			ReferenceBaseBackoff: options.referenceBaseBackoff,
			ReferenceMaxBackoff:  options.referenceMaxBackoff,
		})
	inst.publisher = outbox.NewPublisher(inst.tx, inst.outboxRepo, inst.eventSink, clock, metrics, logger,
		outbox.Config{
			BatchSize:   50,
			BaseBackoff: time.Millisecond,
			MaxBackoff:  10 * time.Millisecond,
			LockTTL:     200 * time.Millisecond,
			PublisherID: "test-publisher-" + uuid.NewString(),
		})
	return inst
}

func resetDatabase(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool, err := newTestPool(ctx)
	require.NoError(t, err)
	defer pool.Close()

	// The ledger is protected against TRUNCATE by a trigger; disable triggers
	// for this administrative cleanup on a single session.
	connection, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer connection.Release()

	_, err = connection.Exec(ctx, `SET session_replication_role = replica`)
	require.NoError(t, err)
	_, err = connection.Exec(ctx,
		`TRUNCATE wallet_ledger_entries, outbox_events, inbox_messages, wager_transactions, wallets CASCADE`)
	require.NoError(t, err)
	_, err = connection.Exec(ctx, `SET session_replication_role = DEFAULT`)
	require.NoError(t, err)
}

func randomUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)
	return id
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func mustParseUUID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(raw)
	require.NoError(t, err)
	return id
}

// openWallet creates a wallet through the wallet service.
func openWallet(t *testing.T, inst *instance, initial string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	playerID := randomUUID(t)
	balance, err := money.Parse(initial, "BRL")
	require.NoError(t, err)
	opened, err := inst.walletSvc.Open(context.Background(), wallets.OpenCommand{
		PlayerID:       playerID,
		InitialBalance: balance,
		CorrelationID:  "test",
	})
	require.NoError(t, err)
	return opened.ID(), playerID
}

func command(playerID, walletID uuid.UUID, kind wagering.Kind, amount, externalID string) wager.ProcessCommand {
	parsed := money.MustParse(amount, "BRL")
	return wager.ProcessCommand{
		ProviderID:            "provider-a",
		ExternalTransactionID: externalID,
		IdempotencyKey:        "provider-a:" + externalID,
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               "round-1",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Money:                 parsed,
		CorrelationID:         "test-" + externalID,
	}
}

func withReference(cmd wager.ProcessCommand, referenceExternalID string) wager.ProcessCommand {
	cmd.ReferenceExternalTransactionID = referenceExternalID
	return cmd
}

func mustProcess(t *testing.T, inst *instance, cmd wager.ProcessCommand) wager.Result {
	t.Helper()
	result, err := inst.wagerSvc.Process(context.Background(), cmd)
	require.NoError(t, err)
	return result
}

func assertBalance(t *testing.T, inst *instance, walletID uuid.UUID, expected string) {
	t.Helper()
	found, err := inst.walletSvc.Get(context.Background(), walletID)
	require.NoError(t, err)
	require.Equal(t, expected, found.Balance().AmountString())
}

func assertLedgerCount(t *testing.T, inst *instance, walletID uuid.UUID, expected int64) {
	t.Helper()
	totals, err := inst.ledgerRepo.TotalsByWallet(context.Background(), walletID)
	require.NoError(t, err)
	require.Equal(t, expected, totals.Entries)
}

func assertReconciled(t *testing.T, inst *instance, walletID uuid.UUID) {
	t.Helper()
	result, err := inst.walletSvc.Reconcile(context.Background(), walletID)
	require.NoError(t, err)
	require.True(t, result.Consistent, "stored %s vs calculated %s",
		result.StoredBalance.AmountString(), result.CalculatedBalance.AmountString())
}

func ledgerTotals(t *testing.T, inst *instance, walletID uuid.UUID) ports.LedgerTotals {
	t.Helper()
	totals, err := inst.ledgerRepo.TotalsByWallet(context.Background(), walletID)
	require.NoError(t, err)
	return totals
}

// testMetrics is an in-memory ports.Metrics implementation.
type testMetrics struct {
	mu       sync.Mutex
	replays  int
	conflict int
	statuses map[string]int
}

func (m *testMetrics) RecordWagerTransaction(status, kind, failureCode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statuses == nil {
		m.statuses = map[string]int{}
	}
	m.statuses[status+"/"+kind+"/"+failureCode]++
}

func (m *testMetrics) RecordIdempotentReplay() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replays++
}

func (m *testMetrics) RecordWalletVersionConflict() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conflict++
}

func (m *testMetrics) RecordWorkerRetry(string)                 {}
func (m *testMetrics) RecordDLQMessage(string)                  {}
func (m *testMetrics) RecordReconciliationRun(bool)             {}
func (m *testMetrics) RecordOutboxPublished(int, time.Duration) {}
func (m *testMetrics) SetOutboxPending(int64)                   {}
func (m *testMetrics) SetPendingReferences(int64)               {}

// fakePublisher captures outbox events and can simulate broker outages.
type fakePublisher struct {
	mu       sync.Mutex
	events   map[string][]byte
	attempts map[string]int
	failures int
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{events: map[string][]byte{}, attempts: map[string]int{}}
}

func (p *fakePublisher) Publish(_ context.Context, event ports.OutboxEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures > 0 {
		p.failures--
		return fmt.Errorf("simulated broker outage")
	}
	p.events[event.EventID.String()] = append([]byte(nil), event.Payload...)
	p.attempts[event.EventID.String()]++
	return nil
}

func (p *fakePublisher) failNext(count int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = count
}

func (p *fakePublisher) publishedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

func (p *fakePublisher) attemptCount(eventID uuid.UUID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts[eventID.String()]
}

// newSQSClient builds a client against freshly recreated test queues.
func newSQSClient(t *testing.T) *sqs.Client {
	t.Helper()
	return newSQSClientWith(t, 5*time.Second)
}

// newSQSClientWith recreates the test queues (so attributes such as the
// visibility timeout are deterministic) and returns a client for them.
func newSQSClientWith(t *testing.T, visibility time.Duration) *sqs.Client {
	t.Helper()
	cfg := config.SQSConfig{
		Region:            envOr("TEST_AWS_REGION", "us-east-1"),
		Endpoint:          envOr("TEST_SQS_ENDPOINT", "http://localstack:4566"),
		AccessKeyID:       envOr("TEST_AWS_ACCESS_KEY_ID", "test"),
		SecretAccessKey:   envOr("TEST_AWS_SECRET_ACCESS_KEY", "test"),
		QueueName:         envOr("TEST_SQS_QUEUE_NAME", "wager-transactions-it.fifo"),
		DLQName:           envOr("TEST_SQS_DLQ_NAME", "wager-transactions-it-dlq.fifo"),
		OutboxQueueName:   envOr("TEST_SQS_OUTBOX_QUEUE_NAME", "wager-events-it.fifo"),
		OutboxDLQName:     envOr("TEST_SQS_OUTBOX_DLQ_NAME", "wager-events-it-dlq.fifo"),
		VisibilityTimeout: visibility,
		WaitTime:          2 * time.Second,
		MaxMessages:       10,
		MaxReceiveCount:   5,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	api := newRawSQS(t)
	for _, name := range []string{cfg.QueueName, cfg.DLQName, cfg.OutboxQueueName, cfg.OutboxDLQName} {
		out, err := api.GetQueueUrl(ctx, &awssqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			continue
		}
		_, _ = api.DeleteQueue(ctx, &awssqs.DeleteQueueInput{QueueUrl: out.QueueUrl})
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client, err := sqs.NewClient(ctx, cfg, logger)
	require.NoError(t, err)
	return client
}
