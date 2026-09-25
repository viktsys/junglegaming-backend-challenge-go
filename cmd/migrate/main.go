// Command migrate applies or reverts the embedded SQL migrations.
//
// Usage:
//
//	go run ./cmd/migrate -command up
//	go run ./cmd/migrate -command down -steps 1
//	go run ./cmd/migrate -command down -steps 0   # revert everything
//	go run ./cmd/migrate -command version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/junglegaming/backend-challenge-go/internal/platform/migrate"
	"github.com/junglegaming/backend-challenge-go/migrations"
)

func main() {
	command := flag.String("command", "up", "up, down or version")
	steps := flag.Int("steps", 1, "number of migrations to revert for down (0 = all)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://wager:wager@localhost:5432/wager?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		logger.Error("cannot create pool", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		logger.Error("cannot reach database", slog.String("error", err.Error()))
		os.Exit(1)
	}

	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		logger.Error("cannot load migrations", slog.String("error", err.Error()))
		os.Exit(1)
	}

	switch *command {
	case "up":
		applied, err := migrate.Up(ctx, pool, loaded)
		if err != nil {
			logger.Error("migration up failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		fmt.Printf("applied %d migration(s)\n", applied)
	case "down":
		reverted, err := migrate.Down(ctx, pool, loaded, *steps)
		if err != nil {
			logger.Error("migration down failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		fmt.Printf("reverted %d migration(s)\n", reverted)
	case "version":
		version, err := migrate.CurrentVersion(ctx, pool)
		if err != nil {
			logger.Error("cannot read version", slog.String("error", err.Error()))
			os.Exit(1)
		}
		fmt.Printf("current schema version: %d\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (expected up, down or version)\n", *command)
		os.Exit(2)
	}
}
