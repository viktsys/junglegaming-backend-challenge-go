// Package bootstrap composes the application with Uber Fx. Each layer is an
// fx.Module; lifecycle hooks own the HTTP server, the SQS consumer and the
// background workers. Dependencies are closed after the components that use
// them stop (Fx runs OnStop hooks in reverse order of registration).
package bootstrap

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/junglegaming/backend-challenge-go/internal/adapters/httpapi"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/oidc"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/postgres"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/sqs"
	"github.com/junglegaming/backend-challenge-go/internal/app/consumer"
	"github.com/junglegaming/backend-challenge-go/internal/app/outbox"
	"github.com/junglegaming/backend-challenge-go/internal/app/references"
	"github.com/junglegaming/backend-challenge-go/internal/app/wager"
	"github.com/junglegaming/backend-challenge-go/internal/app/wallets"
	"github.com/junglegaming/backend-challenge-go/internal/config"
	"github.com/junglegaming/backend-challenge-go/internal/platform/migrate"
	"github.com/junglegaming/backend-challenge-go/internal/platform/observability"
	"github.com/junglegaming/backend-challenge-go/internal/platform/worker"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
	"github.com/junglegaming/backend-challenge-go/migrations"
)

// NewApp builds the Fx application.
func NewApp() *fx.App {
	return fx.New(
		fx.StopTimeout(30*time.Second),
		fx.WithLogger(newFxLogger),
		configModule,
		observabilityModule,
		postgresModule,
		messagingModule,
		authModule,
		appModule,
		httpModule,
		workerModule,
	)
}

var configModule = fx.Module("config",
	fx.Provide(config.Load),
	fx.Provide(func() ports.Clock {
		return func() time.Time { return time.Now().UTC() }
	}),
)

var observabilityModule = fx.Module("observability",
	fx.Provide(func(cfg *config.Config) *slog.Logger {
		return observability.NewLogger(cfg.LogLevel)
	}),
	fx.Provide(observability.NewMetrics),
	fx.Provide(func(metrics *observability.Metrics) ports.Metrics { return metrics }),
)

var postgresModule = fx.Module("postgres",
	fx.Provide(fx.Annotate(
		func(cfg *config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.Database.ConnectTimeout)
			defer cancel()
			return postgres.NewPool(ctx, cfg.Database, logger)
		},
		fx.OnStop(func(_ context.Context, pool *pgxpool.Pool) error {
			pool.Close()
			return nil
		}),
	)),
	fx.Provide(postgres.NewTxManager),
	fx.Provide(func(manager *postgres.TxManager) ports.TxManager { return manager }),
	fx.Provide(postgres.NewWalletRepository),
	fx.Provide(func(repo *postgres.WalletRepository) ports.WalletRepository { return repo }),
	fx.Provide(postgres.NewTransactionRepository),
	fx.Provide(func(repo *postgres.TransactionRepository) ports.TransactionRepository { return repo }),
	fx.Provide(postgres.NewLedgerRepository),
	fx.Provide(func(repo *postgres.LedgerRepository) ports.LedgerRepository { return repo }),
	fx.Provide(postgres.NewInboxRepository),
	fx.Provide(func(repo *postgres.InboxRepository) ports.InboxRepository { return repo }),
	fx.Provide(postgres.NewOutboxRepository),
	fx.Provide(func(repo *postgres.OutboxRepository) ports.OutboxRepository { return repo }),
	fx.Provide(postgres.NewHealth),
	fx.Provide(func(health *postgres.Health) ports.DatabaseHealth { return health }),
	fx.Invoke(runMigrations),
)

func runMigrations(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) error {
	if !cfg.Migrations.Enabled {
		logger.Info("database migrations disabled")
		return nil
	}
	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	applied, err := migrate.Up(ctx, pool, loaded)
	if err != nil {
		return err
	}
	if applied > 0 {
		logger.Info("database migrations applied", slog.Int("count", applied))
	}
	return nil
}

var messagingModule = fx.Module("messaging",
	fx.Provide(func(cfg *config.Config, logger *slog.Logger) (*sqs.Client, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return sqs.NewClient(ctx, cfg.SQS, logger)
	}),
	fx.Provide(func(client *sqs.Client) ports.QueueHealth { return client }),
	fx.Provide(func(client *sqs.Client) ports.EventPublisher { return client }),
	fx.Provide(consumer.NewWagerHandler),
	fx.Provide(func(handler *consumer.WagerHandler) ports.MessageHandler { return handler }),
	fx.Provide(func(
		client *sqs.Client,
		handler ports.MessageHandler,
		metrics ports.Metrics,
		logger *slog.Logger,
		cfg *config.Config,
	) *sqs.Consumer {
		return sqs.NewConsumer(client, handler, metrics, logger, cfg.HTTP.ShutdownTimeout)
	}),
)

var authModule = fx.Module("auth",
	fx.Provide(func(cfg *config.Config) (*oidc.Verifier, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return oidc.NewVerifier(ctx, cfg.OIDC)
	}),
	fx.Provide(httpapi.NewAuth),
)

var appModule = fx.Module("app",
	fx.Provide(func(cfg *config.Config) wager.Config {
		return wager.Config{
			MaxWalletRetries:     5,
			ReferenceMaxAttempts: cfg.Workers.ReferenceMaxAttempts,
			ReferenceBaseBackoff: cfg.Workers.ReferenceBaseBackoff,
			ReferenceMaxBackoff:  cfg.Workers.ReferenceMaxBackoff,
		}
	}),
	fx.Provide(func(cfg *config.Config) outbox.Config {
		return outbox.Config{
			BatchSize:   cfg.Workers.OutboxBatchSize,
			BaseBackoff: cfg.Workers.OutboxBaseBackoff,
			MaxBackoff:  cfg.Workers.OutboxMaxBackoff,
			LockTTL:     cfg.Workers.OutboxLockTTL,
			PublisherID: cfg.Workers.PublisherID,
		}
	}),
	fx.Provide(wager.NewService),
	fx.Provide(wallets.NewService),
)

var httpModule = fx.Module("http",
	fx.Provide(httpapi.NewHealthHandler),
	fx.Provide(httpapi.NewRouter),
	fx.Provide(httpapi.NewServer),
	fx.Invoke(func(lifecycle fx.Lifecycle, server *httpapi.Server, cfg *config.Config, logger *slog.Logger) {
		lifecycle.Append(fx.Hook{
			OnStart: func(context.Context) error {
				server.Start()
				return nil
			},
			OnStop: func(ctx context.Context) error {
				shutdownCtx, cancel := context.WithTimeout(ctx, cfg.HTTP.ShutdownTimeout)
				defer cancel()
				logger.Info("stopping http server")
				return server.Shutdown(shutdownCtx)
			},
		})
	}),
)

var workerModule = fx.Module("workers",
	fx.Provide(outbox.NewPublisher),
	fx.Provide(func(cfg *config.Config, metrics ports.Metrics, service *wager.Service, logger *slog.Logger) *references.Resolver {
		return references.NewResolver(service, metrics, logger, cfg.Workers.PollInterval, cfg.Workers.ReferenceBatchSize)
	}),
	fx.Invoke(startReferenceResolver),
	fx.Invoke(startOutboxPublisher),
	fx.Invoke(startSQSConsumer),
)

func startReferenceResolver(lifecycle fx.Lifecycle, resolver *references.Resolver, logger *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				resolver.Run(ctx)
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			logger.Info("stopping reference resolver")
			cancel()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return stopCtx.Err()
			}
		},
	})
}

func startOutboxPublisher(lifecycle fx.Lifecycle, publisher *outbox.Publisher, cfg *config.Config, logger *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				worker.Run(ctx, logger, "outbox-publisher", cfg.Workers.OutboxPollInterval, func(ctx context.Context) error {
					_, err := publisher.PublishDue(ctx)
					return err
				})
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			logger.Info("stopping outbox publisher")
			cancel()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return stopCtx.Err()
			}
		},
	})
}

func startSQSConsumer(lifecycle fx.Lifecycle, consumer *sqs.Consumer, cfg *config.Config, logger *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer waitGroup.Done()
				consumer.Run(ctx)
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			logger.Info("stopping sqs consumer")
			cancel()
			done := make(chan struct{})
			go func() {
				waitGroup.Wait()
				close(done)
			}()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return stopCtx.Err()
			}
		},
	})
}

type fxSlogLogger struct {
	log *slog.Logger
}

func newFxLogger(log *slog.Logger) fxevent.Logger {
	return &fxSlogLogger{log: log}
}

func (l *fxSlogLogger) LogEvent(event fxevent.Event) {
	switch e := event.(type) {
	case *fxevent.OnStartExecuting:
		l.log.Debug("fx onstart", slog.String("caller", e.CallerName))
	case *fxevent.OnStartExecuted:
		if e.Err != nil {
			l.log.Error("fx onstart failed", slog.String("caller", e.CallerName), slog.String("error", e.Err.Error()))
		}
	case *fxevent.OnStopExecuting:
		l.log.Debug("fx onstop", slog.String("caller", e.CallerName))
	case *fxevent.OnStopExecuted:
		if e.Err != nil {
			l.log.Error("fx onstop failed", slog.String("caller", e.CallerName), slog.String("error", e.Err.Error()))
		}
	case *fxevent.Started:
		if e.Err != nil {
			l.log.Error("fx start failed", slog.String("error", e.Err.Error()))
		} else {
			l.log.Info("application started")
		}
	case *fxevent.Stopped:
		if e.Err != nil {
			l.log.Error("fx stop failed", slog.String("error", e.Err.Error()))
		} else {
			l.log.Info("application stopped")
		}
	case *fxevent.RollingBack:
		l.log.Warn("fx rolling back", slog.String("error", e.StartErr.Error()))
	case *fxevent.RolledBack:
		if e.Err != nil {
			l.log.Error("fx rollback failed", slog.String("error", e.Err.Error()))
		}
	case *fxevent.Supplied:
		if e.Err != nil {
			l.log.Error("fx supply failed", slog.String("type", e.TypeName), slog.String("error", e.Err.Error()))
		}
	case *fxevent.Provided:
		if e.Err != nil {
			l.log.Error("fx provide failed", slog.String("constructor", e.ConstructorName), slog.String("error", e.Err.Error()))
		}
	case *fxevent.Invoking:
		l.log.Debug("fx invoking", slog.String("function", e.FunctionName))
	case *fxevent.Invoked:
		if e.Err != nil {
			l.log.Error("fx invoke failed", slog.String("function", e.FunctionName), slog.String("error", e.Err.Error()))
		}
	case *fxevent.Run:
		if e.Err != nil {
			l.log.Error("fx run failed", slog.String("name", e.Name), slog.String("error", e.Err.Error()))
		}
	}
}
