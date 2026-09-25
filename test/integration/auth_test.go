//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/adapters/httpapi"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/oidc"
	"github.com/junglegaming/backend-challenge-go/internal/config"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
	"github.com/junglegaming/backend-challenge-go/internal/platform/observability"
)

type healthyPinger struct{}

func (healthyPinger) Ping(context.Context) error { return nil }

func TestHTTPAuthenticationAndProviderIsolation(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)

	issuer := envOr("TEST_OIDC_ISSUER_URL", "http://keycloak:8080/realms/wager")
	audience := envOr("TEST_OIDC_AUDIENCE", "wager-api")
	verifier, err := oidc.NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL:     issuer,
		Audience:      audience,
		RolesClaim:    "realm_access.roles",
		ProviderClaim: "provider_id",
		HTTPTimeout:   10 * time.Second,
	})
	require.NoError(t, err)

	router := httpapi.NewRouter(
		testLogger(),
		observability.NewMetrics(),
		httpapi.NewAuth(verifier),
		inst.walletSvc,
		inst.wagerSvc,
		httpapi.NewHealthHandler(healthyPinger{}, healthyPinger{}),
	)
	server := httptest.NewServer(router)
	defer server.Close()

	internalToken := fetchClientCredentialsToken(t, issuer, "internal-service", "internal-service-secret")
	providerAToken := fetchClientCredentialsToken(t, issuer, "provider-a", "provider-a-secret")
	providerBToken := fetchClientCredentialsToken(t, issuer, "provider-b", "provider-b-secret")

	// Public health checks.
	status, _ := doJSON(t, http.MethodGet, server.URL+"/health/live", "", nil)
	assert.Equal(t, http.StatusOK, status)

	// Missing or invalid credentials are rejected.
	status, body := doJSON(t, http.MethodGet, server.URL+"/wallets/"+randomUUID(t).String(), "", nil)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Contains(t, body, "MISSING_TOKEN")

	status, _ = doJSON(t, http.MethodGet, server.URL+"/wallets/"+randomUUID(t).String(), "not-a-token", nil)
	assert.Equal(t, http.StatusUnauthorized, status)

	status, _ = doJSON(t, http.MethodGet, server.URL+"/wallets/"+randomUUID(t).String(), "Bearer garbage.token.value", nil)
	assert.Equal(t, http.StatusUnauthorized, status)

	// Only the internal service may open wallets.
	playerID := randomUUID(t)
	openWalletBody := map[string]any{
		"playerId":       playerID.String(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	}
	status, _ = doJSON(t, http.MethodPost, server.URL+"/wallets", providerAToken, openWalletBody)
	assert.Equal(t, http.StatusForbidden, status)

	status, body = doJSON(t, http.MethodPost, server.URL+"/wallets", internalToken, openWalletBody)
	require.Equal(t, http.StatusCreated, status)
	var wallet struct {
		ID       string `json:"id"`
		PlayerID string `json:"playerId"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &wallet))
	require.NotEmpty(t, wallet.ID)

	// A provider cannot submit on behalf of another provider.
	bet := map[string]any{
		"providerId":            "provider-b",
		"externalTransactionId": "auth-tx-1",
		"playerId":              playerID.String(),
		"walletId":              wallet.ID,
		"roundId":               "round-auth",
		"gameId":                "fortune-chimp",
		"kind":                  "BET",
		"money":                 map[string]string{"amount": "10.00", "currency": "BRL"},
	}
	status, _ = doJSONWithHeaders(t, http.MethodPost, server.URL+"/wagering/transactions", providerAToken,
		map[string]string{"Idempotency-Key": "provider-b:auth-tx-1"}, bet)
	assert.Equal(t, http.StatusForbidden, status)

	// Provider A submits its own operation.
	bet["providerId"] = "provider-a"
	status, body = doJSONWithHeaders(t, http.MethodPost, server.URL+"/wagering/transactions", providerAToken,
		map[string]string{"Idempotency-Key": "provider-a:auth-tx-1"}, bet)
	require.Equal(t, http.StatusCreated, status, body)
	var submitted struct {
		TransactionID string `json:"transactionId"`
		Status        string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &submitted))
	assert.Equal(t, string(wagering.StatusProcessed), submitted.Status)

	// Missing Idempotency-Key is a bad request.
	status, _ = doJSON(t, http.MethodPost, server.URL+"/wagering/transactions", providerAToken, bet)
	assert.Equal(t, http.StatusBadRequest, status)

	// Provider B cannot read provider A's transaction, by internal or external id.
	status, _ = doJSON(t, http.MethodGet, server.URL+"/wagering/transactions/"+submitted.TransactionID, providerBToken, nil)
	assert.Equal(t, http.StatusForbidden, status)

	status, _ = doJSON(t, http.MethodGet, server.URL+"/providers/provider-a/wagering/transactions/auth-tx-1", providerBToken, nil)
	assert.Equal(t, http.StatusForbidden, status)

	// Provider A can read its own transaction.
	status, _ = doJSON(t, http.MethodGet, server.URL+"/providers/provider-a/wagering/transactions/auth-tx-1", providerAToken, nil)
	assert.Equal(t, http.StatusOK, status)

	// The internal service can read any transaction.
	status, _ = doJSON(t, http.MethodGet, server.URL+"/wagering/transactions/"+submitted.TransactionID, internalToken, nil)
	assert.Equal(t, http.StatusOK, status)

	// No financial effects happened for the unauthorized attempts.
	assertBalance(t, inst, mustParseUUID(t, wallet.ID), "90.00")
	assertLedgerCount(t, inst, mustParseUUID(t, wallet.ID), 2)
}

// TestExpiredTokenIsRejected lowers the provider-b token lifespan through the
// Keycloak admin API, obtains a token, waits for it to expire and verifies the
// API rejects it.
func TestExpiredTokenIsRejected(t *testing.T) {
	resetDatabase(t)
	inst := newInstance(t)

	issuer := envOr("TEST_OIDC_ISSUER_URL", "http://keycloak:8080/realms/wager")
	verifier, err := oidc.NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL:     issuer,
		Audience:      envOr("TEST_OIDC_AUDIENCE", "wager-api"),
		RolesClaim:    "realm_access.roles",
		ProviderClaim: "provider_id",
		HTTPTimeout:   10 * time.Second,
	})
	require.NoError(t, err)

	router := httpapi.NewRouter(
		testLogger(),
		observability.NewMetrics(),
		httpapi.NewAuth(verifier),
		inst.walletSvc,
		inst.wagerSvc,
		httpapi.NewHealthHandler(healthyPinger{}, healthyPinger{}),
	)
	server := httptest.NewServer(router)
	defer server.Close()

	host := strings.TrimSuffix(issuer, "/realms/wager")
	adminToken := fetchPasswordToken(t, host, "admin", "admin")
	adminClientsURL := host + "/admin/realms/wager/clients"

	clientUUID := lookupClientUUID(t, adminClientsURL, adminToken, "provider-b")
	representation := getClientRepresentation(t, adminClientsURL+"/"+clientUUID, adminToken)
	originalLifespan := clientAttribute(representation, "access.token.lifespan")
	defer func() {
		setClientLifespan(t, adminClientsURL+"/"+clientUUID, adminToken, representation, originalLifespan)
	}()

	setClientLifespan(t, adminClientsURL+"/"+clientUUID, adminToken, representation, "1")

	expiringToken := fetchClientCredentialsToken(t, issuer, "provider-b", "provider-b-secret")
	time.Sleep(2500 * time.Millisecond)

	status, body := doJSON(t, http.MethodGet, server.URL+"/providers/provider-b/wagering/transactions/whatever", expiringToken, nil)
	require.Equal(t, http.StatusUnauthorized, status, body)
	assert.Contains(t, body, "INVALID_TOKEN")
}

func fetchPasswordToken(t *testing.T, host, username, password string) string {
	t.Helper()
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {"admin-cli"},
		"username":   {username},
		"password":   {password},
	}
	response, err := http.Post(host+"/realms/master/protocol/openid-connect/token",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NotEmpty(t, payload.AccessToken)
	return payload.AccessToken
}

func lookupClientUUID(t *testing.T, clientsURL, adminToken, clientID string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, clientsURL+"?clientId="+url.QueryEscape(clientID), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))

	var clients []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &clients))
	require.NotEmpty(t, clients)
	return clients[0].ID
}

func getClientRepresentation(t *testing.T, clientURL, adminToken string) map[string]any {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, clientURL, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))

	var representation map[string]any
	require.NoError(t, json.Unmarshal(raw, &representation))
	return representation
}

func clientAttribute(representation map[string]any, name string) string {
	attributes, _ := representation["attributes"].(map[string]any)
	if attributes == nil {
		return ""
	}
	value, _ := attributes[name].(string)
	return value
}

func setClientLifespan(t *testing.T, clientURL, adminToken string, representation map[string]any, lifespan string) {
	t.Helper()
	attributes, _ := representation["attributes"].(map[string]any)
	if attributes == nil {
		attributes = map[string]any{}
	}
	if lifespan == "" {
		delete(attributes, "access.token.lifespan")
	} else {
		attributes["access.token.lifespan"] = lifespan
	}
	representation["attributes"] = attributes

	encoded, err := json.Marshal(representation)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPut, clientURL, bytes.NewReader(encoded))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	require.Less(t, response.StatusCode, 300, string(raw))
}

func fetchClientCredentialsToken(t *testing.T, issuer, clientID, secret string) string {
	t.Helper()
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	response, err := http.Post(issuer+"/protocol/openid-connect/token",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NotEmpty(t, payload.AccessToken)
	return payload.AccessToken
}

func doJSON(t *testing.T, method, target, token string, payload any) (int, string) {
	t.Helper()
	return doJSONWithHeaders(t, method, target, token, nil, payload)
}

func doJSONWithHeaders(t *testing.T, method, target, token string, headers map[string]string, payload any) (int, string) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, target, body)
	require.NoError(t, err)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		if strings.HasPrefix(token, "Bearer ") || token == "not-a-token" {
			request.Header.Set("Authorization", token)
		} else {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(raw)
}
