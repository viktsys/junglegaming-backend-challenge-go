package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// OutboxRepository is the pgx implementation of ports.OutboxRepository.
type OutboxRepository struct {
	pool *pgxpool.Pool
}

// NewOutboxRepository builds the repository.
func NewOutboxRepository(pool *pgxpool.Pool) *OutboxRepository {
	return &OutboxRepository{pool: pool}
}

// Insert stores an event snapshot in the same transaction as the state change.
func (r *OutboxRepository) Insert(ctx context.Context, event ports.OutboxEvent) error {
	nextAttempt := event.NextAttemptAt
	if nextAttempt.IsZero() {
		nextAttempt = event.OccurredAt
	}
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO outbox_events (
			event_id, aggregate_id, event_type, payload, occurred_at,
			attempts, next_attempt_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $5)`,
		event.EventID, event.AggregateID, event.EventType, event.Payload, event.OccurredAt,
		event.Attempts, nextAttempt)
	if err != nil {
		return fmt.Errorf("postgres: insert outbox event: %w", err)
	}
	return nil
}

// Claim locks a batch of publishable events. Per-aggregate ordering is
// preserved by only ever claiming the earliest unpublished event of each
// aggregate, even when another publisher holds its lock.
func (r *OutboxRepository) Claim(ctx context.Context, publisherID string, now time.Time, limit int, lockTTL time.Duration) ([]ports.OutboxEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	lockCutoff := now.Add(-lockTTL)
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		WITH candidates AS (
			SELECT o.event_id
			FROM outbox_events o
			WHERE o.published_at IS NULL
			  AND o.next_attempt_at <= $2
			  AND (o.locked_at IS NULL OR o.locked_at < $3)
			  AND NOT EXISTS (
				  SELECT 1 FROM outbox_events e
				  WHERE e.aggregate_id = o.aggregate_id
				    AND e.published_at IS NULL
				    AND (e.occurred_at, e.event_id) < (o.occurred_at, o.event_id)
			  )
			ORDER BY o.occurred_at, o.event_id
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		)
		UPDATE outbox_events SET locked_by = $1, locked_at = $2
		WHERE event_id IN (SELECT event_id FROM candidates)
		RETURNING event_id, aggregate_id, event_type, payload, occurred_at, attempts, next_attempt_at`,
		publisherID, now, lockCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: claim outbox events: %w", err)
	}
	defer rows.Close()

	events := make([]ports.OutboxEvent, 0, limit)
	for rows.Next() {
		var event ports.OutboxEvent
		if err := rows.Scan(&event.EventID, &event.AggregateID, &event.EventType, &event.Payload,
			&event.OccurredAt, &event.Attempts, &event.NextAttemptAt); err != nil {
			return nil, fmt.Errorf("postgres: scan outbox event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate outbox events: %w", err)
	}
	return events, nil
}

// MarkPublished records a confirmed publication.
func (r *OutboxRepository) MarkPublished(ctx context.Context, eventID uuid.UUID, now time.Time) error {
	tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE outbox_events
		SET published_at = $2, locked_by = NULL, locked_at = NULL, last_error = NULL
		WHERE event_id = $1 AND published_at IS NULL`, eventID, now)
	if err != nil {
		return fmt.Errorf("postgres: mark outbox published: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: outbox event %s is already published or missing", eventID)
	}
	return nil
}

// MarkFailed releases the lock and schedules a retry with backoff.
func (r *OutboxRepository) MarkFailed(ctx context.Context, eventID uuid.UUID, nextAttempt time.Time, lastError string, now time.Time) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE outbox_events
		SET attempts = attempts + 1,
		    next_attempt_at = $2,
		    locked_by = NULL,
		    locked_at = NULL,
		    last_error = $3
		WHERE event_id = $1 AND published_at IS NULL`, eventID, nextAttempt, lastError)
	if err != nil {
		return fmt.Errorf("postgres: mark outbox failed: %w", err)
	}
	return nil
}

// PendingCount returns the number of unpublished events.
func (r *OutboxRepository) PendingCount(ctx context.Context) (int64, error) {
	var count int64
	if err := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: count outbox events: %w", err)
	}
	return count, nil
}
