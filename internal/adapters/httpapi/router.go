package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/junglegaming/backend-challenge-go/internal/adapters/oidc"
	"github.com/junglegaming/backend-challenge-go/internal/app/wager"
	"github.com/junglegaming/backend-challenge-go/internal/app/wallets"
	"github.com/junglegaming/backend-challenge-go/internal/platform/observability"
)

// NewRouter builds the HTTP handler with authentication and authorization.
func NewRouter(
	logger *slog.Logger,
	metrics *observability.Metrics,
	auth *Auth,
	walletsService *wallets.Service,
	wagerService *wager.Service,
	health *HealthHandler,
) http.Handler {
	router := chi.NewRouter()
	router.Use(RequestID)
	router.Use(Recoverer(logger))
	router.Use(Logging(logger))
	router.Use(MetricsMiddleware(metrics))

	router.Get("/health/live", health.Live)
	router.Get("/health/ready", health.Ready)
	router.Handle("/metrics", metrics.Handler())

	walletHandler := NewWalletHandler(walletsService)
	wageringHandler := NewWageringHandler(wagerService)

	// Wallet operations are internal only.
	router.Route("/wallets", func(r chi.Router) {
		r.Use(auth.Authenticate)
		r.Use(RequireRoles(oidc.RoleInternal))
		r.Post("/", walletHandler.Open)
		r.Get("/{walletId}", walletHandler.Get)
		r.Get("/{walletId}/ledger", walletHandler.Ledger)
		r.Post("/{walletId}/reconciliation", walletHandler.Reconcile)
	})

	// Wager operations are available to providers (scoped to their own data)
	// and to the internal service.
	router.Route("/wagering/transactions", func(r chi.Router) {
		r.Use(auth.Authenticate)
		r.Use(RequireRoles(oidc.RoleProvider, oidc.RoleInternal))
		r.Post("/", wageringHandler.Submit)
		r.Get("/{transactionId}", wageringHandler.Get)
	})

	router.Route("/providers/{providerId}/wagering/transactions", func(r chi.Router) {
		r.Use(auth.Authenticate)
		r.Use(RequireRoles(oidc.RoleProvider, oidc.RoleInternal))
		r.Get("/{externalTransactionId}", wageringHandler.GetByExternal)
	})

	return router
}
