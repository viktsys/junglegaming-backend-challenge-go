package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// HealthHandler implements liveness and readiness probes.
type HealthHandler struct {
	database ports.DatabaseHealth
	queue    ports.QueueHealth
}

// NewHealthHandler builds the health handler.
func NewHealthHandler(database ports.DatabaseHealth, queue ports.QueueHealth) *HealthHandler {
	return &HealthHandler{database: database, queue: queue}
}

// Live reports that the process is running.
func (h *HealthHandler) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// Ready reports PostgreSQL and SQS reachability.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	response := map[string]string{"status": "ready", "postgres": "ok", "sqs": "ok"}
	ready := true
	if err := h.database.Ping(ctx); err != nil {
		response["postgres"] = "unavailable"
		ready = false
	}
	if err := h.queue.Ping(ctx); err != nil {
		response["sqs"] = "unavailable"
		ready = false
	}
	if !ready {
		response["status"] = "not_ready"
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}
