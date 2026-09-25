package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/config"
)

// Server owns the HTTP listener lifecycle.
type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

// NewServer builds the HTTP server.
func NewServer(cfg *config.Config, handler http.Handler, logger *slog.Logger) *Server {
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.HTTP.Addr,
			Handler:           handler,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
			IdleTimeout:       90 * time.Second,
		},
		logger: logger,
	}
}

// Start begins listening without blocking.
func (s *Server) Start() {
	go func() {
		s.logger.Info("http server listening", slog.String("addr", s.httpServer.Addr))
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http server stopped unexpectedly", slog.String("error", err.Error()))
		}
	}()
}

// Shutdown stops accepting connections and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
