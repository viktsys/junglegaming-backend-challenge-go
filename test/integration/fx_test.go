//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/bootstrap"
)

// TestFxCompositionStartsAndStops boots the real Fx application (config,
// database pool, migrations flag, SQS client and consumer, HTTP server,
// outbox publisher and reference resolver) against the running infrastructure
// and then shuts it down, verifying the listener is released and all lifecycle
// hooks completed without error.
func TestFxCompositionStartsAndStops(t *testing.T) {
	dsn := baseDSN
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	t.Setenv("DATABASE_URL", dsn+separator+"search_path="+testSchema)
	t.Setenv("MIGRATIONS_ENABLED", "false")
	t.Setenv("HTTP_ADDR", "127.0.0.1:18099")
	t.Setenv("OIDC_ISSUER_URL", envOr("TEST_OIDC_ISSUER_URL", "http://keycloak:8080/realms/wager"))
	t.Setenv("OIDC_AUDIENCE", envOr("TEST_OIDC_AUDIENCE", "wager-api"))
	t.Setenv("SQS_ENDPOINT", envOr("TEST_SQS_ENDPOINT", "http://localstack:4566"))
	t.Setenv("AWS_ACCESS_KEY_ID", envOr("TEST_AWS_ACCESS_KEY_ID", "test"))
	t.Setenv("AWS_SECRET_ACCESS_KEY", envOr("TEST_AWS_SECRET_ACCESS_KEY", "test"))
	t.Setenv("AWS_REGION", envOr("TEST_AWS_REGION", "us-east-1"))
	t.Setenv("SQS_QUEUE_NAME", "wager-transactions-fx.fifo")
	t.Setenv("SQS_DLQ_NAME", "wager-transactions-fx-dlq.fifo")
	t.Setenv("SQS_OUTBOX_QUEUE_NAME", "wager-events-fx.fifo")
	t.Setenv("SQS_OUTBOX_DLQ_NAME", "wager-events-fx-dlq.fifo")
	t.Setenv("SQS_WAIT_TIME", "2s")
	t.Setenv("SQS_VISIBILITY_TIMEOUT", "5s")
	t.Setenv("WORKER_POLL_INTERVAL", "100ms")
	t.Setenv("OUTBOX_POLL_INTERVAL", "100ms")

	app := bootstrap.NewApp()
	require.NoError(t, app.Err(), "the Fx dependency graph must be valid")

	startCtx, cancelStart := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStart()
	require.NoError(t, app.Start(startCtx), "all Fx lifecycle hooks must start")

	require.Eventually(t, func() bool {
		response, err := http.Get("http://127.0.0.1:18099/health/live")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 10*time.Second, 100*time.Millisecond, "the HTTP server must answer while the app runs")

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStop()
	require.NoError(t, app.Stop(stopCtx), "all Fx lifecycle hooks must stop cleanly")

	_, err := http.Get("http://127.0.0.1:18099/health/live")
	require.Error(t, err, "the HTTP listener must be released after shutdown")
}
