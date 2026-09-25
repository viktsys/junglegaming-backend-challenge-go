package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// InboxRepository is the pgx implementation of ports.InboxRepository.
type InboxRepository struct {
	pool *pgxpool.Pool
}

// NewInboxRepository builds the repository.
func NewInboxRepository(pool *pgxpool.Pool) *InboxRepository {
	return &InboxRepository{pool: pool}
}

// Claim records a message as received unless it was seen before. The unique
// index on (consumer_name, message_id) is the durable deduplication guard.
func (r *InboxRepository) Claim(ctx context.Context, consumerName, messageID, payloadHash string, now time.Time) (bool, *ports.InboxRecord, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return false, nil, fmt.Errorf("postgres: generate inbox id: %w", err)
	}
	q := querierFrom(ctx, r.pool)

	var inserted uuid.UUID
	err = q.QueryRow(ctx, `
		INSERT INTO inbox_messages (id, consumer_name, message_id, payload_hash, status, received_at)
		VALUES ($1, $2, $3, $4, 'RECEIVED', $5)
		ON CONFLICT (consumer_name, message_id) DO NOTHING
		RETURNING id`, id, consumerName, messageID, payloadHash, now).Scan(&inserted)
	if err == nil {
		return true, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, nil, fmt.Errorf("postgres: claim inbox message: %w", err)
	}

	existing, err := r.get(ctx, consumerName, messageID)
	if err != nil {
		return false, nil, err
	}
	return false, existing, nil
}

// Complete marks a claimed message as durably processed.
func (r *InboxRepository) Complete(ctx context.Context, consumerName, messageID string, now time.Time) error {
	tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE inbox_messages
		SET status = 'COMPLETED', completed_at = $3
		WHERE consumer_name = $1 AND message_id = $2`,
		consumerName, messageID, now)
	if err != nil {
		return fmt.Errorf("postgres: complete inbox message: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: inbox message %s/%s does not exist", consumerName, messageID)
	}
	return nil
}

func (r *InboxRepository) get(ctx context.Context, consumerName, messageID string) (*ports.InboxRecord, error) {
	var (
		record      ports.InboxRecord
		status      string
		completedAt *time.Time
	)
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT message_id, payload_hash, status, received_at, completed_at
		FROM inbox_messages
		WHERE consumer_name = $1 AND message_id = $2`, consumerName, messageID).
		Scan(&record.MessageID, &record.PayloadHash, &status, &record.ReceivedAt, &completedAt)
	if err != nil {
		return nil, fmt.Errorf("postgres: load inbox message: %w", err)
	}
	record.Completed = status == "COMPLETED"
	return &record, nil
}
