package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/itcaat/teamcity-mcp/internal/config"
	"github.com/itcaat/teamcity-mcp/internal/teamcity"
)

// noCredsResolver stands in for a request that arrived without a TeamCity token.
type noCredsResolver struct{}

func (noCredsResolver) ClientForContext(context.Context) (*teamcity.Client, error) {
	return nil, teamcity.ErrNoCredentials
}

// badURLResolver stands in for a request naming a TeamCity server this
// deployment does not permit.
type badURLResolver struct{}

func (badURLResolver) ClientForContext(context.Context) (*teamcity.Client, error) {
	return nil, fmt.Errorf("%w: https://evil.example.com", teamcity.ErrURLNotAllowed)
}

func newHandler(t *testing.T, resolver ClientResolver) *Handler {
	t.Helper()
	return NewHandler(resolver, zaptest.NewLogger(t).Sugar())
}

func call(t *testing.T, h *Handler, req string) map[string]interface{} {
	t.Helper()
	resp, err := h.HandleRequest(context.Background(), json.RawMessage(req))
	require.NoError(t, err)
	m, ok := resp.(map[string]interface{})
	require.True(t, ok, "expected a JSON-RPC response object, got %T", resp)
	return m
}

func rpcError(t *testing.T, resp map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, ok := resp["error"]
	require.True(t, ok, "expected an error response, got %v", resp)
	e, ok := raw.(map[string]interface{})
	require.True(t, ok)
	return e
}

// TestCredentialFreeMethodsWork is what keeps an unconfigured client usable: it
// can complete the handshake and see the tool list, so the user gets an
// actionable error instead of a dead connection.
func TestCredentialFreeMethodsWork(t *testing.T) {
	h := newHandler(t, noCredsResolver{})

	tests := []struct {
		name string
		req  string
	}{
		{"initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`},
		{"ping", `{"jsonrpc":"2.0","id":2,"method":"ping"}`},
		{"tools/list", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`},
		{"resources/list", `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`},
		{"get_current_time", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_current_time","arguments":{}}}`},
		{"runtime resource list", `{"jsonrpc":"2.0","id":6,"method":"resources/list","params":{"uri":"teamcity://runtime"}}`},
		{"runtime resource read", `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"teamcity://runtime"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := call(t, h, tt.req)
			assert.NotContains(t, resp, "error", "%s must work without TeamCity credentials", tt.name)
			assert.Contains(t, resp, "result")
		})
	}
}

func TestToolsListIsCompleteWithoutCredentials(t *testing.T) {
	h := newHandler(t, noCredsResolver{})

	resp := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result := resp["result"].(map[string]interface{})
	tools := result["tools"].([]map[string]interface{})

	assert.NotEmpty(t, tools, "tool discovery must not require credentials")
}

// TestMissingCredentialsError checks that everything needing TeamCity fails with
// the dedicated code and an actionable message naming the header.
func TestMissingCredentialsError(t *testing.T) {
	h := newHandler(t, noCredsResolver{})

	tests := []struct {
		name string
		req  string
	}{
		{"tools/call search_builds", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_builds","arguments":{}}}`},
		{"tools/call trigger_build", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"trigger_build","arguments":{"buildTypeId":"x"}}}`},
		{"tools/call fetch_build_log", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fetch_build_log","arguments":{"buildId":"1"}}}`},
		{"resources/list projects", `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{"uri":"teamcity://projects"}}`},
		{"resources/list agents", `{"jsonrpc":"2.0","id":5,"method":"resources/list","params":{"uri":"teamcity://agents"}}`},
		{"resources/read projects", `{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"teamcity://projects"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := rpcError(t, call(t, h, tt.req))

			assert.EqualValues(t, CodeCredentialsRequired, e["code"])
			// The whole instruction must be in `message`: several MCP clients
			// surface only that field.
			assert.Contains(t, e["message"], teamcity.HeaderToken)
			assert.Contains(t, e["message"], "TC_TOKEN", "stdio users need the env-var alternative")

			data := e["data"].(map[string]interface{})
			assert.Equal(t, teamcity.HeaderToken, data["tokenHeader"])
		})
	}
}

func TestDisallowedURLError(t *testing.T) {
	h := newHandler(t, badURLResolver{})

	e := rpcError(t, call(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_builds","arguments":{}}}`))

	assert.EqualValues(t, CodeURLNotAllowed, e["code"])
	assert.Contains(t, e["message"], "not allowed")
}

func TestUnknownToolIsNotACredentialError(t *testing.T) {
	h := newHandler(t, noCredsResolver{})

	// Credentials are resolved before the tool name is checked, so an unknown
	// tool reports the credential problem first. Once credentials are present it
	// must report the real problem.
	e := rpcError(t, call(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`))
	assert.EqualValues(t, CodeCredentialsRequired, e["code"])

	h = newHandler(t, testClient(t, "https://tc.example.com"))
	e = rpcError(t, call(t, h,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`))
	assert.EqualValues(t, -32603, e["code"])
	assert.Contains(t, e["data"], "unknown tool")
}

// TestWithWorkingClient exercises the success path through a fake TeamCity.
func TestWithWorkingClient(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"project":[{"id":"p1","name":"Project One"}]}`)
	}))
	defer srv.Close()

	h := newHandler(t, testClient(t, srv.URL))

	resp := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{"uri":"teamcity://projects"}}`)
	require.NotContains(t, resp, "error")

	result := resp["result"].(map[string]interface{})
	assert.Len(t, result["resources"], 1)
	assert.Equal(t, "Bearer test-token", gotAuth)
}

func TestResponseStatus(t *testing.T) {
	assert.Equal(t, "error", responseStatus(nil, assert.AnError))
	assert.Equal(t, "error", responseStatus(map[string]interface{}{"error": map[string]interface{}{}}, nil))
	assert.Equal(t, "success", responseStatus(map[string]interface{}{"result": map[string]interface{}{}}, nil))
	// A notification produces no response at all, which is not a failure.
	assert.Equal(t, "success", responseStatus(nil, nil))
}

func TestUnsupportedResourceURI(t *testing.T) {
	h := newHandler(t, testClient(t, "https://tc.example.com"))

	e := rpcError(t, call(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{"uri":"teamcity://nope"}}`))
	assert.EqualValues(t, -32603, e["code"])
}

// testClient builds a real single-tenant client, which doubles as a ClientResolver.
func testClient(t *testing.T, url string) *teamcity.Client {
	t.Helper()
	c, err := teamcity.NewClient(config.TeamCityConfig{
		URL:     url,
		Token:   "test-token",
		Timeout: "30s",
	}, zaptest.NewLogger(t).Sugar())
	require.NoError(t, err)
	return c
}
