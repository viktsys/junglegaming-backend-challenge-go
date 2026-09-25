package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/adapters/oidc"
	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/platform/observability"
)

type contextKey string

const (
	principalContextKey contextKey = "principal"
	requestIDContextKey contextKey = "requestId"
)

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) *oidc.Principal {
	principal, _ := ctx.Value(principalContextKey).(*oidc.Principal)
	return principal
}

// RequestIDFrom returns the request correlation identifier.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey).(string)
	return id
}

// RequestID assigns or propagates a correlation id.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = r.Header.Get("X-Correlation-Id")
		}
		if id == "" {
			generated, err := uuid.NewV7()
			if err != nil {
				id = uuid.NewString()
			} else {
				id = generated.String()
			}
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(payload []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(payload)
}

// Recoverer converts panics into 500 responses without leaking details.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("panic recovered",
						slog.Any("panic", recovered),
						slog.String("path", r.URL.Path),
						slog.String("requestId", RequestIDFrom(r.Context())))
					writeJSON(w, http.StatusInternalServerError, errorResponse{
						Code:    "INTERNAL_ERROR",
						Message: "internal error",
						Kind:    string(apperr.KindInternal),
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logging emits one structured log line per request with the identifiers
// available at the transport boundary.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startedAt := time.Now()
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)
			if recorder.status == 0 {
				recorder.status = http.StatusOK
			}
			logger.Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Duration("duration", time.Since(startedAt)),
				slog.String("requestId", RequestIDFrom(r.Context())),
				slog.String("providerId", providerIDOf(r.Context())))
		})
	}
}

// MetricsMiddleware records request counters and latency by route pattern.
func MetricsMiddleware(metrics *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startedAt := time.Now()
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)

			route := "unmatched"
			if routeContext := chi.RouteContext(r.Context()); routeContext != nil && routeContext.RoutePattern() != "" {
				route = routeContext.RoutePattern()
			}
			if recorder.status == 0 {
				recorder.status = http.StatusOK
			}
			metrics.ObserveHTTP(route, r.Method, strconv.Itoa(recorder.status), time.Since(startedAt))
		})
	}
}

// Auth validates bearer tokens and stores the principal in the context.
type Auth struct {
	verifier *oidc.Verifier
}

// NewAuth builds the authentication middleware.
func NewAuth(verifier *oidc.Verifier) *Auth {
	return &Auth{verifier: verifier}
}

// Authenticate rejects requests without a valid bearer token. The auth scheme
// is compared case-insensitively as required by RFC 7235.
func (a *Auth) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, credentials, found := strings.Cut(r.Header.Get("Authorization"), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") {
			writeError(w, apperr.New(apperr.KindUnauthorized, "MISSING_TOKEN", "Authorization bearer token is required"))
			return
		}
		rawToken := strings.TrimSpace(credentials)
		if rawToken == "" {
			writeError(w, apperr.New(apperr.KindUnauthorized, "MISSING_TOKEN", "Authorization bearer token is required"))
			return
		}
		principal, err := a.verifier.Verify(r.Context(), rawToken)
		if err != nil {
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey, principal)))
	})
}

// RequireRoles authorizes requests holding at least one of the roles.
func RequireRoles(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal := PrincipalFrom(r.Context())
			if principal == nil {
				writeError(w, apperr.New(apperr.KindUnauthorized, "MISSING_TOKEN", "authentication is required"))
				return
			}
			for _, role := range roles {
				if principal.HasRole(role) {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, apperr.New(apperr.KindForbidden, "FORBIDDEN",
				"the authenticated identity is not allowed to perform this operation"))
		})
	}
}

// authorizeProvider ensures a provider-scoped identity only accesses its own data.
func authorizeProvider(w http.ResponseWriter, r *http.Request, providerID string) bool {
	principal := PrincipalFrom(r.Context())
	if principal == nil {
		writeError(w, apperr.New(apperr.KindUnauthorized, "MISSING_TOKEN", "authentication is required"))
		return false
	}
	if principal.HasRole(oidc.RoleInternal) {
		return true
	}
	if principal.ProviderID != "" && principal.ProviderID == providerID {
		return true
	}
	writeError(w, apperr.New(apperr.KindForbidden, "FORBIDDEN",
		"the authenticated identity cannot access another provider's transactions"))
	return false
}

func providerIDOf(ctx context.Context) string {
	if principal := PrincipalFrom(ctx); principal != nil {
		return principal.ProviderID
	}
	return ""
}
