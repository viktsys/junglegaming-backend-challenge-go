// Package outbox implements the transactional outbox publisher. A separate
// worker claims unpublished events, publishes them to the broker and only then
// marks them as published. Abandoned locks are reclaimed after a TTL, so an
// interrupted publisher cannot strand events.
package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// Config tunes the publisher.
type Config struct {
	BatchSize   int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	LockTTL     time.Duration
	PublisherID string
}

// Publisher publishes pending outbox events.
type Publisher struct {
	tx      ports.TxManager
	repo    ports.OutboxRepository
	events  ports.EventPublisher
	clock   ports.Clock
	metrics ports.Metrics
	logger  *slog.Logger
	config  Config
}

// NewPublisher builds the publisher.
func NewPublisher(
	tx ports.TxManager,
	repo ports.OutboxRepository,
	events ports.EventPublisher,
	clock ports.Clock,
	metrics ports.Metrics,
	logger *slog.Logger,
	config Config,
) *Publisher {
	if config.BatchSize < 1 {
		config.BatchSize = 20
	}
	if config.LockTTL <= 0 {
		config.LockTTL = 30 * time.Second
	}
	if config.BaseBackoff <= 0 {
		config.BaseBackoff = 2 * time.Second
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = 5 * time.Minute
	}
	return &Publisher{
		tx:      tx,
		repo:    repo,
		events:  events,
		clock:   clock,
		metrics: metrics,
		logger:  logger,
		config:  config,
	}
}

// PublishDue claims and publishes one batch, returning how many events were
// confirmed. A failed publication is rescheduled with exponential backoff.
func (p *Publisher) PublishDue(ctx context.Context) (int, error) {
	var batch []ports.OutboxEvent
	err := p.tx.WithinTransaction(ctx, func(ctx context.Context) error {
		var claimErr error
		batch, claimErr = p.repo.Claim(ctx, p.config.PublisherID, p.clock(), p.config.BatchSize, p.config.LockTTL)
		return claimErr
	})
	if err != nil {
		return 0, err
	}

	published := 0
	for _, event := range batch {
		startedAt := time.Now()
		if err := p.events.Publish(ctx, event); err != nil {
			nextAttempt := p.clock().Add(p.backoff(event.Attempts + 1))
			p.metrics.RecordWorkerRetry("outbox")
			if markErr := p.markFailed(ctx, event, nextAttempt, err); markErr != nil {
				p.logger.Error("failed to reschedule outbox event",
					slog.String("eventId", event.EventID.String()),
					slog.String("error", markErr.Error()))
			}
			p.logger.Warn("outbox publication failed",
				slog.String("eventId", event.EventID.String()),
				slog.String("eventType", event.EventType),
				slog.Int("attempt", event.Attempts+1),
				slog.String("error", err.Error()))
			continue
		}

		if err := p.tx.WithinTransaction(ctx, func(ctx context.Context) error {
			return p.repo.MarkPublished(ctx, event.EventID, p.clock())
		}); err != nil {
			// The event may have been published without the confirmation being
			// stored; the stable eventId makes the republication idempotent.
			p.logger.Error("failed to confirm outbox publication",
				slog.String("eventId", event.EventID.String()),
				slog.String("error", err.Error()))
			continue
		}
		published++
		p.metrics.RecordOutboxPublished(event.Attempts+1, time.Since(startedAt))
	}

	if pending, err := p.repo.PendingCount(ctx); err == nil {
		p.metrics.SetOutboxPending(pending)
	}
	return published, nil
}

func (p *Publisher) markFailed(ctx context.Context, event ports.OutboxEvent, next time.Time, publishErr error) error {
	return p.tx.WithinTransaction(ctx, func(ctx context.Context) error {
		return p.repo.MarkFailed(ctx, event.EventID, next, publishErr.Error(), p.clock())
	})
}

func (p *Publisher) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := p.config.BaseBackoff
	for i := 1; i < attempt; i++ {
		backoff *= 2
		if backoff >= p.config.MaxBackoff {
			return p.config.MaxBackoff
		}
	}
	if backoff > p.config.MaxBackoff {
		return p.config.MaxBackoff
	}
	return backoff
}
