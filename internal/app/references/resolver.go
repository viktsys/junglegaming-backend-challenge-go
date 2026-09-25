// Package references implements the worker that resumes transactions waiting
// for a reference. It retries with exponential backoff and rejects operations
// whose reference never became available.
package references

import (
	"context"
	"log/slog"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/app/wager"
	"github.com/junglegaming/backend-challenge-go/internal/platform/worker"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// Resolver periodically resumes pending transactions.
type Resolver struct {
	wager    *wager.Service
	metrics  ports.Metrics
	logger   *slog.Logger
	interval time.Duration
	batch    int
}

// NewResolver builds the resolver worker.
func NewResolver(service *wager.Service, metrics ports.Metrics, logger *slog.Logger, interval time.Duration, batch int) *Resolver {
	if batch < 1 {
		batch = 20
	}
	return &Resolver{
		wager:    service,
		metrics:  metrics,
		logger:   logger,
		interval: interval,
		batch:    batch,
	}
}

// Run blocks until the context is cancelled.
func (r *Resolver) Run(ctx context.Context) {
	worker.Run(ctx, r.logger, "reference-resolver", r.interval, func(ctx context.Context) error {
		resumed, err := r.wager.ResumeDue(ctx, r.batch)
		if err != nil {
			return err
		}
		if resumed > 0 {
			r.logger.Info("resumed pending transactions", slog.Int("count", resumed))
		}
		if pending, err := r.wager.CountPending(ctx); err == nil {
			r.metrics.SetPendingReferences(pending)
		}
		return nil
	})
}
