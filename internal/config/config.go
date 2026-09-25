// Package config loads and validates all runtime configuration from environment
// variables. The application refuses to start with invalid configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env        string
	LogLevel   string
	HTTP       HTTPConfig
	Database   DatabaseConfig
	SQS        SQSConfig
	OIDC       OIDCConfig
	Workers    WorkerConfig
	Migrations MigrationsConfig
}

type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

type DatabaseConfig struct {
	URL            string
	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
}

type SQSConfig struct {
	Region            string
	Endpoint          string
	AccessKeyID       string
	SecretAccessKey   string
	QueueName         string
	DLQName           string
	OutboxQueueName   string
	OutboxDLQName     string
	VisibilityTimeout time.Duration
	WaitTime          time.Duration
	MaxMessages       int32
	MaxReceiveCount   int32
}

type OIDCConfig struct {
	IssuerURL     string
	Audience      string
	RolesClaim    string
	ProviderClaim string
	HTTPTimeout   time.Duration
}

type WorkerConfig struct {
	PollInterval         time.Duration
	ReferenceMaxAttempts int
	ReferenceBaseBackoff time.Duration
	ReferenceMaxBackoff  time.Duration
	ReferenceBatchSize   int
	OutboxPollInterval   time.Duration
	OutboxBatchSize      int
	OutboxMaxAttempts    int
	OutboxBaseBackoff    time.Duration
	OutboxMaxBackoff     time.Duration
	OutboxLockTTL        time.Duration
	PublisherID          string
}

type MigrationsConfig struct {
	Enabled bool
}

// Load reads configuration from the process environment, applying defaults
// suitable for local development, and validates mandatory fields.
func Load() (*Config, error) {
	cfg := &Config{
		Env:      getString("APP_ENV", "development"),
		LogLevel: getString("LOG_LEVEL", "info"),
		HTTP: HTTPConfig{
			Addr:            getString("HTTP_ADDR", ":8080"),
			ReadTimeout:     getDuration("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:    getDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: getDuration("HTTP_SHUTDOWN_TIMEOUT", 25*time.Second),
		},
		Database: DatabaseConfig{
			URL:            getString("DATABASE_URL", ""),
			MaxConns:       int32(getInt("DB_MAX_CONNS", 20)),
			MinConns:       int32(getInt("DB_MIN_CONNS", 2)),
			ConnectTimeout: getDuration("DB_CONNECT_TIMEOUT", 5*time.Second),
		},
		SQS: SQSConfig{
			Region:            getString("AWS_REGION", "us-east-1"),
			Endpoint:          getString("SQS_ENDPOINT", ""),
			AccessKeyID:       getString("AWS_ACCESS_KEY_ID", ""),
			SecretAccessKey:   getString("AWS_SECRET_ACCESS_KEY", ""),
			QueueName:         getString("SQS_QUEUE_NAME", "wager-transactions.fifo"),
			DLQName:           getString("SQS_DLQ_NAME", "wager-transactions-dlq.fifo"),
			OutboxQueueName:   getString("SQS_OUTBOX_QUEUE_NAME", "wager-events.fifo"),
			OutboxDLQName:     getString("SQS_OUTBOX_DLQ_NAME", "wager-events-dlq.fifo"),
			VisibilityTimeout: getDuration("SQS_VISIBILITY_TIMEOUT", 30*time.Second),
			WaitTime:          getDuration("SQS_WAIT_TIME", 20*time.Second),
			MaxMessages:       int32(getInt("SQS_MAX_MESSAGES", 10)),
			MaxReceiveCount:   int32(getInt("SQS_MAX_RECEIVE_COUNT", 5)),
		},
		OIDC: OIDCConfig{
			IssuerURL:     getString("OIDC_ISSUER_URL", ""),
			Audience:      getString("OIDC_AUDIENCE", ""),
			RolesClaim:    getString("OIDC_ROLES_CLAIM", "realm_access.roles"),
			ProviderClaim: getString("OIDC_PROVIDER_CLAIM", "provider_id"),
			HTTPTimeout:   getDuration("OIDC_HTTP_TIMEOUT", 10*time.Second),
		},
		Workers: WorkerConfig{
			PollInterval:         getDuration("WORKER_POLL_INTERVAL", time.Second),
			ReferenceMaxAttempts: getInt("REFERENCE_MAX_ATTEMPTS", 10),
			ReferenceBaseBackoff: getDuration("REFERENCE_BASE_BACKOFF", 2*time.Second),
			ReferenceMaxBackoff:  getDuration("REFERENCE_MAX_BACKOFF", 5*time.Minute),
			ReferenceBatchSize:   getInt("REFERENCE_BATCH_SIZE", 20),
			OutboxPollInterval:   getDuration("OUTBOX_POLL_INTERVAL", time.Second),
			OutboxBatchSize:      getInt("OUTBOX_BATCH_SIZE", 20),
			OutboxMaxAttempts:    getInt("OUTBOX_MAX_ATTEMPTS", 10),
			OutboxBaseBackoff:    getDuration("OUTBOX_BASE_BACKOFF", 2*time.Second),
			OutboxMaxBackoff:     getDuration("OUTBOX_MAX_BACKOFF", 5*time.Minute),
			OutboxLockTTL:        getDuration("OUTBOX_LOCK_TTL", 30*time.Second),
			PublisherID:          getString("PUBLISHER_ID", defaultPublisherID()),
		},
		Migrations: MigrationsConfig{
			Enabled: getBool("MIGRATIONS_ENABLED", true),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Database.URL == "" {
		return fmt.Errorf("config: DATABASE_URL is required")
	}
	if c.OIDC.IssuerURL == "" {
		return fmt.Errorf("config: OIDC_ISSUER_URL is required")
	}
	if c.OIDC.Audience == "" {
		return fmt.Errorf("config: OIDC_AUDIENCE is required")
	}
	if !strings.HasSuffix(c.SQS.QueueName, ".fifo") || !strings.HasSuffix(c.SQS.DLQName, ".fifo") {
		return fmt.Errorf("config: SQS_QUEUE_NAME and SQS_DLQ_NAME must end with .fifo")
	}
	if !strings.HasSuffix(c.SQS.OutboxQueueName, ".fifo") || !strings.HasSuffix(c.SQS.OutboxDLQName, ".fifo") {
		return fmt.Errorf("config: SQS_OUTBOX_QUEUE_NAME and SQS_OUTBOX_DLQ_NAME must end with .fifo")
	}
	if c.Database.MaxConns < 1 || c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		return fmt.Errorf("config: invalid database connection bounds")
	}
	if c.Workers.ReferenceMaxAttempts < 1 || c.Workers.OutboxMaxAttempts < 1 {
		return fmt.Errorf("config: worker max attempts must be >= 1")
	}
	if c.Workers.ReferenceBatchSize < 1 || c.Workers.OutboxBatchSize < 1 {
		return fmt.Errorf("config: worker batch sizes must be >= 1")
	}
	return nil
}

// IsProduction reports whether the app runs in production mode.
func (c *Config) IsProduction() bool { return c.Env == "production" }

func defaultPublisherID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "publisher-unknown"
	}
	return fmt.Sprintf("publisher-%s-%d", host, os.Getpid())
}

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func getInt(key string, def int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return v
}

func getBool(key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return v
}

func getDuration(key string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return v
}
