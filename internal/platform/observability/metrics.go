package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics implements ports.Metrics with Prometheus instruments.
type Metrics struct {
	registry *prometheus.Registry

	wagerTransactions      *prometheus.CounterVec
	idempotentReplays      prometheus.Counter
	walletVersionConflicts prometheus.Counter
	workerRetries          *prometheus.CounterVec
	dlqMessages            *prometheus.CounterVec
	reconciliationRuns     *prometheus.CounterVec
	outboxPublished        prometheus.Counter
	outboxPublishLatency   prometheus.Histogram
	outboxPending          prometheus.Gauge
	pendingReferences      prometheus.Gauge

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// NewMetrics registers all instruments.
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	m := &Metrics{
		registry: registry,
		wagerTransactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_transactions_total",
			Help: "Wager transaction outcomes by status, kind and failure code.",
		}, []string{"status", "kind", "failure_code"}),
		idempotentReplays: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_idempotent_replays_total",
			Help: "Number of operations answered from the persisted result.",
		}),
		walletVersionConflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wallet_version_conflicts_total",
			Help: "Number of optimistic wallet update conflicts.",
		}),
		workerRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "worker_retries_total",
			Help: "Retries scheduled by background workers.",
		}, []string{"worker"}),
		dlqMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqs_dlq_messages_total",
			Help: "Messages routed to a dead-letter queue by this process.",
		}, []string{"queue"}),
		reconciliationRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reconciliation_runs_total",
			Help: "Reconciliation executions by outcome.",
		}, []string{"consistent"}),
		outboxPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_published_total",
			Help: "Outbox events confirmed as published.",
		}),
		outboxPublishLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "outbox_publish_latency_seconds",
			Help:    "Latency of a confirmed outbox publication.",
			Buckets: prometheus.DefBuckets,
		}),
		outboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_pending_events",
			Help: "Unpublished outbox events (backlog).",
		}),
		pendingReferences: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wager_pending_transactions",
			Help: "Transactions waiting for processing or a reference.",
		}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by route, method and status.",
		}, []string{"route", "method", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
	}

	registry.MustRegister(
		m.wagerTransactions,
		m.idempotentReplays,
		m.walletVersionConflicts,
		m.workerRetries,
		m.dlqMessages,
		m.reconciliationRuns,
		m.outboxPublished,
		m.outboxPublishLatency,
		m.outboxPending,
		m.pendingReferences,
		m.httpRequests,
		m.httpDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler exposes the Prometheus scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// RecordWagerTransaction counts an outcome.
func (m *Metrics) RecordWagerTransaction(status, kind, failureCode string) {
	m.wagerTransactions.WithLabelValues(status, kind, failureCode).Inc()
}

// RecordIdempotentReplay counts a replay answered from persisted state.
func (m *Metrics) RecordIdempotentReplay() { m.idempotentReplays.Inc() }

// RecordWalletVersionConflict counts an optimistic conflict.
func (m *Metrics) RecordWalletVersionConflict() { m.walletVersionConflicts.Inc() }

// RecordWorkerRetry counts a scheduled retry.
func (m *Metrics) RecordWorkerRetry(worker string) { m.workerRetries.WithLabelValues(worker).Inc() }

// RecordDLQMessage counts a message sent to a dead-letter queue.
func (m *Metrics) RecordDLQMessage(queue string) { m.dlqMessages.WithLabelValues(queue).Inc() }

// RecordReconciliationRun counts a reconciliation outcome.
func (m *Metrics) RecordReconciliationRun(consistent bool) {
	label := "false"
	if consistent {
		label = "true"
	}
	m.reconciliationRuns.WithLabelValues(label).Inc()
}

// RecordOutboxPublished counts a confirmed publication.
func (m *Metrics) RecordOutboxPublished(attempts int, latency time.Duration) {
	m.outboxPublished.Inc()
	m.outboxPublishLatency.Observe(latency.Seconds())
}

// SetOutboxPending records the current outbox backlog.
func (m *Metrics) SetOutboxPending(count int64) { m.outboxPending.Set(float64(count)) }

// SetPendingReferences records pending transactions waiting for a reference.
func (m *Metrics) SetPendingReferences(count int64) { m.pendingReferences.Set(float64(count)) }

// ObserveHTTP records an HTTP request.
func (m *Metrics) ObserveHTTP(route, method, status string, duration time.Duration) {
	m.httpRequests.WithLabelValues(route, method, status).Inc()
	m.httpDuration.WithLabelValues(route, method).Observe(duration.Seconds())
}
