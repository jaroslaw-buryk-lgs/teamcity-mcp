package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/itcaat/teamcity-mcp/internal/config"
	"github.com/itcaat/teamcity-mcp/internal/mcp"
	"github.com/itcaat/teamcity-mcp/internal/teamcity"
)

// fakeTeamCity records the Authorization header of every request it receives.
type fakeTeamCity struct {
	mu    sync.Mutex
	auths []string
}

func (f *fakeTeamCity) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"project":[{"id":"p1","name":"Project One"}]}`)
}

func (f *fakeTeamCity) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

// mcpTestServer wires a Server behind httptest with the same middleware stack
// startHTTP uses.
func mcpTestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.credentialsMiddleware(http.HandlerFunc(s.handleMCP)))
	srv := httptest.NewServer(s.authMiddleware(mux))
	t.Cleanup(srv.Close)
	return srv
}

func dialWS(t *testing.T, url string, header http.Header) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(strings.Replace(url, "http://", "ws://", 1)+"/mcp", header)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func wsRoundTrip(t *testing.T, conn *websocket.Conn, req string) map[string]interface{} {
	t.Helper()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(req)))

	var resp map[string]interface{}
	require.NoError(t, conn.ReadJSON(&resp))
	return resp
}

// TestWebSocketCredentialsBoundToConnection is the regression test for the
// upgrade-time header problem: headers exist only during the handshake, so a
// naive implementation that looks for them per message would authenticate the
// first frame and then lose the token. Two frames are required to catch that.
func TestWebSocketCredentialsBoundToConnection(t *testing.T) {
	tc := &fakeTeamCity{}
	tcSrv := httptest.NewServer(tc)
	defer tcSrv.Close()

	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: tcSrv.URL}})
	srv := mcpTestServer(t, s)

	conn := dialWS(t, srv.URL, http.Header{teamcity.HeaderToken: {"ws-token"}})

	for i := 0; i < 2; i++ {
		resp := wsRoundTrip(t, conn,
			`{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{"uri":"teamcity://projects"}}`)
		assert.NotContains(t, resp, "error", "frame %d should succeed", i+1)
	}

	auths := tc.seen()
	require.Len(t, auths, 2, "both frames must have reached TeamCity")
	for i, auth := range auths {
		assert.Equal(t, "Bearer ws-token", auths[i], "frame %d lost the connection's token (got %q)", i+1, auth)
	}
}

func TestWebSocketWithoutCredentials(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})
	srv := mcpTestServer(t, s)

	conn := dialWS(t, srv.URL, nil)

	t.Run("handshake succeeds", func(t *testing.T) {
		resp := wsRoundTrip(t, conn, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
		assert.NotContains(t, resp, "error")
		assert.Contains(t, resp, "result")
	})

	t.Run("tool discovery succeeds", func(t *testing.T) {
		resp := wsRoundTrip(t, conn, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		assert.NotContains(t, resp, "error")
	})

	t.Run("a TeamCity-backed call reports the missing header", func(t *testing.T) {
		resp := wsRoundTrip(t, conn,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_builds","arguments":{}}}`)

		e, ok := resp["error"].(map[string]interface{})
		require.True(t, ok, "expected an error, got %v", resp)
		assert.EqualValues(t, mcp.CodeCredentialsRequired, e["code"])
		assert.Contains(t, e["message"], teamcity.HeaderToken)
	})
}

// TestHTTPPerRequestTokens is the end-to-end multi-tenant path: two users, one
// server, each authenticated as themselves.
func TestHTTPPerRequestTokens(t *testing.T) {
	tc := &fakeTeamCity{}
	tcSrv := httptest.NewServer(tc)
	defer tcSrv.Close()

	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: tcSrv.URL}})
	srv := mcpTestServer(t, s)

	for _, token := range []string{"alice-token", "bob-token"} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{"uri":"teamcity://projects"}}`))
		require.NoError(t, err)
		req.Header.Set(teamcity.HeaderToken, token)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		resp.Body.Close()

		assert.NotContains(t, body, "error")
	}

	auths := tc.seen()
	require.Len(t, auths, 2)
	assert.Equal(t, "Bearer alice-token", auths[0])
	assert.Equal(t, "Bearer bob-token", auths[1])
}

func TestHTTPMissingTokenIsNotUnauthorized(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})
	srv := mcpTestServer(t, s)

	resp, err := http.Post(srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_builds","arguments":{}}}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	// 401 is reserved for a SERVER_SECRET failure. A missing TeamCity token is a
	// 200 carrying an actionable JSON-RPC error.
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	e, ok := body["error"].(map[string]interface{})
	require.True(t, ok)
	assert.EqualValues(t, mcp.CodeCredentialsRequired, e["code"])
}

func TestHTTPDisallowedURLIsBadRequest(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})
	srv := mcpTestServer(t, s)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	req.Header.Set(teamcity.HeaderToken, "t")
	req.Header.Set(teamcity.HeaderURL, "http://169.254.169.254/latest/meta-data")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
