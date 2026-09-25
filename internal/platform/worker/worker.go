// Package worker provides a small lifecycle helper for periodic background
// workers. Each worker runs until its context is cancelled and reports errors
// without crashing the process.
package worker

import (
	"context"
	"log/slog"
	"time"
)

// Run executes fn immediately and then on every tick until ctx is cancelled.
func Run(ctx context.Context, logger *slog.Logger, name string, interval time.Duration, fn func(context.Context) error) {
	if interval <= 0 {
		interval = time.Second
	}
	logger.Info("worker started", slog.String("worker", name), slog.Duration("interval", interval))
	defer logger.Info("worker stopped", slog.String("worker", name))

	run := func() {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			logger.Error("worker iteration failed", slog.String("worker", name), slog.String("error", err.Error()))
		}
	}

	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
