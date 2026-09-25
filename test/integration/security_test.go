//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/adapters/httpapi"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/oidc"
	"github.com/junglegaming/backend-challenge-go/internal/config"
	"github.com/junglegaming/backend-challenge-go/internal/platform/observability"
)

type securityHarness struct {
	inst           *instance
	server         *httptest.Server
	issuer         string
	host           string
	internalToken  string
	providerAToken string
	providerBToken string
	walletID       string
	playerID       string
}

func newSecurityHarness(t *testing.T) *securityHarness {
	t.Helper()
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

	server := httptest.NewServer(httpapi.NewRouter(
		testLogger(),
		observability.NewMetrics(),
		httpapi.NewAuth(verifier),
		inst.walletSvc,
		inst.wagerSvc,
		httpapi.NewHealthHandler(healthyPinger{}, healthyPinger{}),
	))
	t.Cleanup(server.Close)

	harness := &securityHarness{
		inst:           inst,
		server:         server,
		issuer:         issuer,
		host:           strings.TrimSuffix(issuer, "/realms/wager"),
		internalToken:  fetchClientCredentialsToken(t, issuer, "internal-service", "internal-service-secret"),
		providerAToken: fetchClientCredentialsToken(t, issuer, "provider-a", "provider-a-secret"),
		providerBToken: fetchClientCredentialsToken(t, issuer, "provider-b", "provider-b-secret"),
	}

	status, body := doJSON(t, http.MethodPost, server.URL+"/wallets", harness.internalToken, map[string]any{
		"playerId":       randomUUID(t).String(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	})
	require.Equal(t, http.StatusCreated, status, body)
	var wallet struct {
		ID       string `json:"id"`
		PlayerID string `json:"playerId"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &wallet))
	harness.walletID = wallet.ID
	harness.playerID = wallet.PlayerID
	return harness
}

// submit is a helper that posts a wagering operation and returns status/body.
func (h *securityHarness) submit(t *testing.T, token, providerID, externalID string, amount string) (int, string) {
	t.Helper()
	return doJSONWithHeaders(t, http.MethodPost, h.server.URL+"/wagering/transactions", token,
		map[string]string{"Idempotency-Key": providerID + ":" + externalID},
		map[string]any{
			"providerId":            providerID,
			"externalTransactionId": externalID,
			"playerId":              h.playerID,
			"walletId":              h.walletID,
			"roundId":               "round-security",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": amount, "currency": "BRL"},
		})
}

func TestSecurityMissingOrMalformedCredentials(t *testing.T) {
	harness := newSecurityHarness(t)
	walletURL := harness.server.URL + "/wallets/" + harness.walletID

	cases := []struct {
		name   string
		header string
		omit   bool
	}{
		{"absent", "", true},
		{"empty bearer", "Bearer", false},
		{"bearer without token", "Bearer ", false},
		{"basic scheme", "Basic aW50ZXJuYWw6c2VjcmV0", false},
		{"raw token without scheme", harness.internalToken, false},
		{"unknown scheme", "Token " + harness.internalToken, false},
		{"digest scheme", "Digest username=provider-a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if !tc.omit {
				headers["Authorization"] = tc.header
			}
			status, body := doRawJSON(t, http.MethodGet, walletURL, headers, nil)
			assert.Equal(t, http.StatusUnauthorized, status, body)
			assert.Contains(t, body, "MISSING_TOKEN")
			assert.NotContains(t, body, harness.internalToken, "the response must not echo credentials")
		})
	}

	// A token in the query string must not authenticate the request.
	status, body := doJSON(t, http.MethodGet, walletURL+"?access_token="+harness.internalToken, "", nil)
	assert.Equal(t, http.StatusUnauthorized, status, body)

	// The auth scheme is case-insensitive (RFC 7235).
	status, body = doRawJSON(t, http.MethodGet, walletURL,
		map[string]string{"Authorization": "bearer " + harness.internalToken}, nil)
	assert.Equal(t, http.StatusOK, status, body)
}

func TestSecurityCraftedAndForgedTokens(t *testing.T) {
	harness := newSecurityHarness(t)
	walletURL := harness.server.URL + "/wallets/" + harness.walletID

	now := time.Now().Unix()

	t.Run("alg none", func(t *testing.T) {
		header := b64url(`{"alg":"none","typ":"JWT"}`)
		payload := b64url(fmt.Sprintf(
			`{"iss":%q,"aud":"wager-api","sub":"attacker","iat":%d,"exp":%d,"realm_access":{"roles":["internal","provider"]},"provider_id":"provider-a"}`,
			harness.issuer, now, now+3600))
		token := header + "." + payload + "."
		status, body := doJSONWithHeaders(t, http.MethodGet, walletURL, "Bearer "+token, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, status, body)
	})

	t.Run("tampered payload keeps signature", func(t *testing.T) {
		parts := strings.Split(harness.providerAToken, ".")
		require.Len(t, parts, 3)
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		require.NoError(t, err)
		var claims map[string]any
		require.NoError(t, json.Unmarshal(payload, &claims))
		claims["provider_id"] = "provider-b"
		claims["realm_access"] = map[string]any{"roles": []string{"internal"}}
		forgedPayload, err := json.Marshal(claims)
		require.NoError(t, err)
		token := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forgedPayload) + "." + parts[2]

		status, body := doJSONWithHeaders(t, http.MethodGet, walletURL, "Bearer "+token, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, status, body)
	})

	t.Run("wrong signing key", func(t *testing.T) {
		parts := strings.Split(harness.internalToken, ".")
		require.Len(t, parts, 3)
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		require.NoError(t, err)
		signature[0] ^= 0xFF
		token := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)

		status, body := doJSONWithHeaders(t, http.MethodGet, walletURL, "Bearer "+token, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, status, body)
	})

	t.Run("token from another realm (wrong issuer)", func(t *testing.T) {
		masterToken := fetchPasswordToken(t, harness.host, "admin", "admin")
		status, body := doJSONWithHeaders(t, http.MethodGet, walletURL, "Bearer "+masterToken, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, status, body)
		assert.Contains(t, body, "INVALID_TOKEN")
	})

	t.Run("valid signature but wrong audience", func(t *testing.T) {
		suffix := strings.ReplaceAll(randomUUID(t).String(), "-", "")[:8]
		clientID := "no-audience-" + suffix
		secret := "no-audience-secret-" + suffix
		adminToken := fetchPasswordToken(t, harness.host, "admin", "admin")
		clientUUID := createKeycloakClient(t, harness.host, adminToken, map[string]any{
			"clientId":                  clientID,
			"enabled":                   true,
			"protocol":                  "openid-connect",
			"publicClient":              false,
			"clientAuthenticatorType":   "client-secret",
			"secret":                    secret,
			"serviceAccountsEnabled":    true,
			"standardFlowEnabled":       false,
			"directAccessGrantsEnabled": false,
		})
		defer deleteKeycloakClient(t, harness.host, adminToken, clientUUID)

		token := fetchClientCredentialsToken(t, harness.issuer, clientID, secret)
		status, body := doJSONWithHeaders(t, http.MethodGet, walletURL, "Bearer "+token, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, status, body)
		assert.Contains(t, body, "INVALID_TOKEN")
	})
}

func TestSecurityAuthorizationMatrix(t *testing.T) {
	harness := newSecurityHarness(t)

	// A transaction owned by provider A and another owned by provider B.
	status, body := harness.submit(t, harness.providerAToken, "provider-a", "sec-a-1", "10.00")
	require.Equal(t, http.StatusCreated, status, body)
	var transactionA struct {
		TransactionID string `json:"transactionId"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &transactionA))

	status, body = harness.submit(t, harness.providerBToken, "provider-b", "sec-b-1", "5.00")
	require.Equal(t, http.StatusCreated, status, body)
	var transactionB struct {
		TransactionID string `json:"transactionId"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &transactionB))

	cases := []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		// Wallet operations are internal-only.
		{"open wallet as provider", http.MethodPost, "/wallets", harness.providerAToken, http.StatusForbidden},
		{"read wallet as provider", http.MethodGet, "/wallets/" + harness.walletID, harness.providerAToken, http.StatusForbidden},
		{"read ledger as provider", http.MethodGet, "/wallets/" + harness.walletID + "/ledger", harness.providerBToken, http.StatusForbidden},
		{"reconcile as provider", http.MethodPost, "/wallets/" + harness.walletID + "/reconciliation", harness.providerAToken, http.StatusForbidden},
		{"open wallet without token", http.MethodPost, "/wallets", "", http.StatusUnauthorized},
		{"read wallet without token", http.MethodGet, "/wallets/" + harness.walletID, "", http.StatusUnauthorized},

		// Providers are isolated from each other.
		{"provider A reads provider B transaction by id", http.MethodGet, "/wagering/transactions/" + transactionB.TransactionID, harness.providerAToken, http.StatusForbidden},
		{"provider B reads provider A transaction by id", http.MethodGet, "/wagering/transactions/" + transactionA.TransactionID, harness.providerBToken, http.StatusForbidden},
		{"provider A reads provider B by external path", http.MethodGet, "/providers/provider-b/wagering/transactions/sec-b-1", harness.providerAToken, http.StatusForbidden},
		{"provider B reads provider A by external path", http.MethodGet, "/providers/provider-a/wagering/transactions/sec-a-1", harness.providerBToken, http.StatusForbidden},
		{"provider A reads provider B by external path without token", http.MethodGet, "/providers/provider-b/wagering/transactions/sec-b-1", "", http.StatusUnauthorized},

		// Internal service can read any transaction but is not a provider.
		{"internal reads provider A transaction", http.MethodGet, "/wagering/transactions/" + transactionA.TransactionID, harness.internalToken, http.StatusOK},
		{"internal reads provider B transaction", http.MethodGet, "/providers/provider-b/wagering/transactions/sec-b-1", harness.internalToken, http.StatusOK},
		{"provider A reads own transaction", http.MethodGet, "/providers/provider-a/wagering/transactions/sec-a-1", harness.providerAToken, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doJSON(t, tc.method, harness.server.URL+tc.path, tc.token, nil)
			assert.Equal(t, tc.want, status, body)
		})
	}
}

func TestSecurityUnauthorizedRequestsHaveNoSideEffects(t *testing.T) {
	harness := newSecurityHarness(t)

	_, ledgerBefore := doJSON(t, http.MethodGet,
		harness.server.URL+"/wallets/"+harness.walletID+"/ledger", harness.internalToken, nil)

	// provider-b tries to submit an operation on behalf of provider-a.
	status, body := harness.submit(t, harness.providerBToken, "provider-a", "sec-forged-1", "10.00")
	require.Equal(t, http.StatusForbidden, status, body)

	// A provider tries to open a wallet (internal operation).
	status, _ = doJSON(t, http.MethodPost, harness.server.URL+"/wallets", harness.providerAToken, map[string]any{
		"playerId":       randomUUID(t).String(),
		"initialBalance": map[string]string{"amount": "999.00", "currency": "BRL"},
	})
	require.Equal(t, http.StatusForbidden, status)

	// No transaction was created and no money moved.
	status, body = doJSON(t, http.MethodGet,
		harness.server.URL+"/providers/provider-a/wagering/transactions/sec-forged-1", harness.providerAToken, nil)
	assert.Equal(t, http.StatusNotFound, status, body)

	status, walletBody := doJSON(t, http.MethodGet,
		harness.server.URL+"/wallets/"+harness.walletID, harness.internalToken, nil)
	require.Equal(t, http.StatusOK, status)
	var wallet struct {
		Balance struct {
			Amount string `json:"amount"`
		} `json:"balance"`
	}
	require.NoError(t, json.Unmarshal([]byte(walletBody), &wallet))
	assert.Equal(t, "100.00", wallet.Balance.Amount)

	_, ledgerAfter := doJSON(t, http.MethodGet,
		harness.server.URL+"/wallets/"+harness.walletID+"/ledger", harness.internalToken, nil)
	assert.JSONEq(t, ledgerBefore, ledgerAfter)
}

func TestSecurityPublicEndpointsRemainPublic(t *testing.T) {
	harness := newSecurityHarness(t)

	for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
		status, _ := doJSON(t, http.MethodGet, harness.server.URL+path, "", nil)
		assert.Equal(t, http.StatusOK, status, path)
	}
}

func b64url(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// doRawJSON sends a request with the exact headers provided (no scheme
// normalization) and returns the status code and body.
func doRawJSON(t *testing.T, method, target string, headers map[string]string, payload any) (int, string) {
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

func createKeycloakClient(t *testing.T, host, adminToken string, representation map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(representation)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, host+"/admin/realms/wager/clients", bytes.NewReader(encoded))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode, string(raw))

	location := response.Header.Get("Location")
	require.NotEmpty(t, location)
	parts := strings.Split(strings.TrimRight(location, "/"), "/")
	return parts[len(parts)-1]
}

func deleteKeycloakClient(t *testing.T, host, adminToken, clientUUID string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodDelete, host+"/admin/realms/wager/clients/"+clientUUID, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Less(t, response.StatusCode, 300)
}
