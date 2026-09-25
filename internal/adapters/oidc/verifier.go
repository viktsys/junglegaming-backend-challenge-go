// Package oidc validates OAuth 2.0 / OIDC bearer tokens against an external
// identity provider (Keycloak) using discovery and JWKS.
package oidc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/junglegaming/backend-challenge-go/internal/config"
	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
)

// Realm roles recognized by the service.
const (
	RoleProvider = "provider"
	RoleInternal = "internal"
)

// Principal is the authenticated identity extracted from a token.
type Principal struct {
	Subject    string
	ProviderID string
	Roles      []string
}

// HasRole reports whether the principal holds a role.
func (p *Principal) HasRole(role string) bool {
	for _, candidate := range p.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

// Verifier validates tokens and extracts authorization claims.
type Verifier struct {
	verifier      *oidc.IDTokenVerifier
	rolesClaim    string
	providerClaim string
}

// NewVerifier performs OIDC discovery and returns a token verifier. Discovery
// is retried so the service can start while the IdP is still booting.
func NewVerifier(ctx context.Context, cfg config.OIDCConfig) (*Verifier, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	discoveryCtx := oidc.ClientContext(ctx, client)

	var (
		provider *oidc.Provider
		err      error
	)
	deadline := time.Now().Add(60 * time.Second)
	for {
		provider, err = oidc.NewProvider(discoveryCtx, cfg.IssuerURL)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("oidc: discovery for %s failed: %w", cfg.IssuerURL, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return &Verifier{
		verifier: provider.Verifier(&oidc.Config{
			ClientID: cfg.Audience,
		}),
		rolesClaim:    cfg.RolesClaim,
		providerClaim: cfg.ProviderClaim,
	}, nil
}

// Verify validates the raw JWT and returns the principal.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Principal, error) {
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, apperr.Wrap(apperr.KindUnauthorized, "INVALID_TOKEN",
			"bearer token is missing, invalid or expired", err)
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return nil, apperr.Wrap(apperr.KindUnauthorized, "INVALID_TOKEN_CLAIMS",
			"token claims cannot be decoded", err)
	}

	principal := &Principal{
		Subject: token.Subject,
		Roles:   extractRoles(claims, v.rolesClaim),
	}
	if providerID, ok := lookupClaim(claims, v.providerClaim).(string); ok {
		principal.ProviderID = providerID
	}
	return principal, nil
}

// extractRoles reads a role list from a claim path such as
// "realm_access.roles" or "resource_access.wager-api.roles".
func extractRoles(claims map[string]any, path string) []string {
	value := lookupClaim(claims, path)
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		roles := make([]string, 0, len(typed))
		for _, item := range typed {
			if role, ok := item.(string); ok {
				roles = append(roles, role)
			}
		}
		return roles
	default:
		return nil
	}
}

func lookupClaim(claims map[string]any, path string) any {
	if path == "" {
		return nil
	}
	var current any = claims
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[segment]
		if !ok {
			return nil
		}
	}
	return current
}
