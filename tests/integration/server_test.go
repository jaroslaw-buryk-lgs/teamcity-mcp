//go:build integration

package integration

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Configuration comes from the environment so this suite matches whatever the
// server under test was actually started with. Previously it hardcoded a token
// that could never be a valid HMAC, so the auth assertions could not all pass
// at the same time.
var (
	serverURL    = envOrDefault("MCP_URL", "http://localhost:8123")
	serverSecret = os.Getenv("SERVER_SECRET")
	tcToken      = os.Getenv("TC_TOKEN")
)

// Header names must match internal/teamcity. Duplicated rather than imported so
// this suite exercises the wire contract a real client depends on.
const (
	headerTCToken = "X-TeamCity-Token"
	headerTCURL   = "X-TeamCity-Url"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// gateToken derives the outer HMAC gate's bearer token from SERVER_SECRET.
// Note this is the hex digest, not the raw secret.
func gateToken() string {
	if serverSecret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(serverSecret))
	mac.Write([]byte("teamcity-mcp"))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestServerHealth(t *testing.T) {
	// Test liveness
	resp, err := http.Get(serverURL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Test readiness. With no server-side TC_TOKEN the deep probe is skipped and
	// this is a 200; with one it may be 503 if TeamCity is unreachable.
	resp, err = http.Get(serverURL + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, resp.StatusCode)
}

// TestReadyzIgnoresTeamCityHeaders confirms /readyz cannot be steered at another
// host. It is unauthenticated, so honouring these headers would make it an SSRF
// probe with a success oracle.
func TestReadyzIgnoresTeamCityHeaders(t *testing.T) {
	plain, err := http.Get(serverURL + "/readyz")
	require.NoError(t, err)
	defer plain.Body.Close()

	req, err := http.NewRequest("GET", serverURL+"/readyz", nil)
	require.NoError(t, err)
	req.Header.Set(headerTCURL, "http://169.254.169.254")
	req.Header.Set(headerTCToken, "attacker-token")

	withHeaders, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer withHeaders.Body.Close()

	assert.Equal(t, plain.StatusCode, withHeaders.StatusCode,
		"/readyz must behave identically regardless of TeamCity headers")
}

// TestInitializeWithoutTeamCityCredentials is the contract that lets a client
// connect before it has been given a token.
func TestInitializeWithoutTeamCityCredentials(t *testing.T) {
	resp := post(t, nil, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo":      map[string]interface{}{"name": "test-client", "version": "1.0.0"},
		},
	})

	assert.Equal(t, "2.0", resp["jsonrpc"])
	assert.NotContains(t, resp, "error")

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "2024-11-05", result["protocolVersion"])

	serverInfo, ok := result["serverInfo"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "teamcity-mcp", serverInfo["name"])
}

func TestMCPToolsList(t *testing.T) {
	// Tool discovery must also work without credentials.
	resp := post(t, nil, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)

	tools, ok := result["tools"].([]interface{})
	require.True(t, ok)
	assert.Greater(t, len(tools), 0)

	var triggerBuildFound bool
	for _, tool := range tools {
		toolMap, ok := tool.(map[string]interface{})
		require.True(t, ok)
		if toolMap["name"] == "trigger_build" {
			triggerBuildFound = true
			assert.Contains(t, toolMap, "description")
			assert.Contains(t, toolMap, "inputSchema")
		}
	}
	assert.True(t, triggerBuildFound, "trigger_build tool not found")
}

// TestMissingTeamCityCredentials is the actionable-error contract: a 200 with a
// JSON-RPC error naming the header, not an opaque connection failure.
func TestMissingTeamCityCredentials(t *testing.T) {
	resp := post(t, nil, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "search_builds",
			"arguments": map[string]interface{}{},
		},
	})

	errorResp, ok := resp["error"].(map[string]interface{})
	require.True(t, ok, "expected a credential error, got %v", resp)
	assert.Equal(t, float64(-32001), errorResp["code"])
	assert.Contains(t, errorResp["message"], headerTCToken)
}

func TestGetCurrentTimeWithoutCredentials(t *testing.T) {
	resp := post(t, nil, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "get_current_time",
			"arguments": map[string]interface{}{},
		},
	})

	assert.NotContains(t, resp, "error", "get_current_time needs no TeamCity access")
}

func TestMCPResourcesList(t *testing.T) {
	if tcToken == "" {
		t.Skip("set TC_TOKEN to a real TeamCity API token to exercise TeamCity-backed resources")
	}

	resp := post(t, http.Header{headerTCToken: {tcToken}}, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "resources/list",
		"params":  map[string]interface{}{"uri": "teamcity://projects"},
	})

	require.NotContains(t, resp, "error")
	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)

	resources, ok := result["resources"].([]interface{})
	require.True(t, ok)
	assert.GreaterOrEqual(t, len(resources), 0)
}

// TestInvalidTeamCityToken confirms a bad token is reported as a token problem,
// without echoing the token back.
func TestInvalidTeamCityToken(t *testing.T) {
	resp := post(t, http.Header{headerTCToken: {"definitely-not-a-valid-token"}}, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "resources/list",
		"params":  map[string]interface{}{"uri": "teamcity://projects"},
	})

	errorResp, ok := resp["error"].(map[string]interface{})
	require.True(t, ok, "an invalid token must fail, got %v", resp)

	body, err := json.Marshal(errorResp)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "definitely-not-a-valid-token",
		"the error must not echo the supplied token")
}

func TestDisallowedTeamCityURL(t *testing.T) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/list",
	})
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", serverURL+"/mcp", bytes.NewBuffer(reqBody))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(headerTCToken, "some-token")
	httpReq.Header.Set(headerTCURL, "http://169.254.169.254/latest/meta-data")
	if token := gateToken(); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"an unpermitted TeamCity URL must be refused before reaching the handler")
}

func TestInvalidMethod(t *testing.T) {
	resp := post(t, nil, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "invalid/method",
	})

	assert.Equal(t, "2.0", resp["jsonrpc"])
	assert.Equal(t, float64(8), resp["id"])

	errorResp, ok := resp["error"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, float64(-32601), errorResp["code"])
	assert.Equal(t, "Method not found", errorResp["message"])
}

// TestAuthenticationRequired covers the outer SERVER_SECRET gate, which is
// independent of TeamCity credentials. Skipped when the gate is disabled -
// previously this test could not coexist with the rest of the suite.
func TestAuthenticationRequired(t *testing.T) {
	if serverSecret == "" {
		t.Skip("SERVER_SECRET is not set, so the outer authentication gate is disabled")
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "initialize",
	})
	require.NoError(t, err)

	t.Run("no Authorization header", func(t *testing.T) {
		httpReq, err := http.NewRequest("POST", serverURL+"/mcp", bytes.NewBuffer(reqBody))
		require.NoError(t, err)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(httpReq)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("the raw secret is not a valid token", func(t *testing.T) {
		httpReq, err := http.NewRequest("POST", serverURL+"/mcp", bytes.NewBuffer(reqBody))
		require.NoError(t, err)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+serverSecret)

		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(httpReq)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"the gate expects the HMAC digest of the secret, not the secret itself")
	})
}

// post sends an MCP request, adding the outer gate token when configured.
func post(t *testing.T, extra http.Header, req map[string]interface{}) map[string]interface{} {
	t.Helper()

	reqBody, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", serverURL+"/mcp", bytes.NewBuffer(reqBody))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")
	if token := gateToken(); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	for k, values := range extra {
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var response map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&response))
	return response
}
