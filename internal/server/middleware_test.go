package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/itcaat/teamcity-mcp/internal/config"
	"github.com/itcaat/teamcity-mcp/internal/teamcity"
)

func newTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	if cfg.TeamCity.Timeout == "" {
		cfg.TeamCity.Timeout = "30s"
	}
	if cfg.Cache.TTL == "" {
		cfg.Cache.TTL = "10s"
	}
	s, err := New(cfg, zaptest.NewLogger(t).Sugar())
	require.NoError(t, err)
	return s
}

// capture returns a handler that records the credentials it observed.
func capture(called *bool, got *teamcity.Credentials, hadCreds *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		*got, *hadCreds = teamcity.CredentialsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestCredentialsMiddleware(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{
		URL:         "https://tc.example.com",
		AllowedURLs: []string{"https://tc2.example.com"},
	}})

	t.Run("token header only", func(t *testing.T) {
		var called, hadCreds bool
		var got teamcity.Credentials

		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(teamcity.HeaderToken, "user-token")
		w := httptest.NewRecorder()

		s.credentialsMiddleware(capture(&called, &got, &hadCreds)).ServeHTTP(w, r)

		assert.True(t, called)
		require.True(t, hadCreds)
		assert.Equal(t, "user-token", got.Token)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("allowed URL override", func(t *testing.T) {
		var called, hadCreds bool
		var got teamcity.Credentials

		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(teamcity.HeaderToken, "user-token")
		r.Header.Set(teamcity.HeaderURL, "https://tc2.example.com/")
		w := httptest.NewRecorder()

		s.credentialsMiddleware(capture(&called, &got, &hadCreds)).ServeHTTP(w, r)

		require.True(t, hadCreds)
		assert.Equal(t, "https://tc2.example.com", got.URL)
	})

	// A disallowed URL is rejected here rather than in the handler, so an SSRF
	// probe never reaches TeamCity and a client is never silently served a
	// different server than the one it asked for.
	t.Run("disallowed URL is a 400 and the handler is not reached", func(t *testing.T) {
		var called, hadCreds bool
		var got teamcity.Credentials

		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(teamcity.HeaderToken, "user-token")
		r.Header.Set(teamcity.HeaderURL, "http://169.254.169.254/latest/meta-data")
		w := httptest.NewRecorder()

		s.credentialsMiddleware(capture(&called, &got, &hadCreds)).ServeHTTP(w, r)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.False(t, called, "a rejected URL must not reach the handler")
	})

	// This is what keeps initialize and tools/list working for a client that has
	// not been given a token yet.
	t.Run("no headers passes through without credentials", func(t *testing.T) {
		var called, hadCreds bool
		var got teamcity.Credentials

		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		w := httptest.NewRecorder()

		s.credentialsMiddleware(capture(&called, &got, &hadCreds)).ServeHTTP(w, r)

		assert.True(t, called, "a request without a token must still be handled")
		assert.False(t, hadCreds)
		assert.Equal(t, http.StatusOK, w.Code, "missing TeamCity credentials must not be a 401")
	})

	t.Run("the Authorization header is not treated as a TeamCity token", func(t *testing.T) {
		var called, hadCreds bool
		var got teamcity.Credentials

		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer server-secret-hmac")
		w := httptest.NewRecorder()

		s.credentialsMiddleware(capture(&called, &got, &hadCreds)).ServeHTTP(w, r)

		assert.True(t, called)
		assert.False(t, hadCreds, "the outer gate's credential must not leak into TeamCity auth")
	})
}

func TestAuthMiddleware(t *testing.T) {
	const secret = "test-secret"
	validToken := func() string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("teamcity-mcp"))
		return hex.EncodeToString(mac.Sum(nil))
	}()

	s := newTestServer(t, &config.Config{
		TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"},
		Server:   config.ServerConfig{ServerSecret: secret},
	})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	tests := []struct {
		name     string
		path     string
		header   string
		wantCode int
	}{
		{"valid HMAC", "/mcp", "Bearer " + validToken, http.StatusOK},
		{"no header", "/mcp", "", http.StatusUnauthorized},
		{"not a bearer token", "/mcp", "Basic abc", http.StatusUnauthorized},
		{"the raw secret is not the token", "/mcp", "Bearer " + secret, http.StatusUnauthorized},
		{"wrong token", "/mcp", "Bearer nope", http.StatusUnauthorized},
		{"healthz is exempt", "/healthz", "", http.StatusOK},
		{"readyz is exempt", "/readyz", "", http.StatusOK},
		{"metrics is exempt", "/metrics", "", http.StatusOK},
		// A prefix match would have let these through.
		{"readyz lookalike is not exempt", "/readyzzz", "", http.StatusUnauthorized},
		{"health prefix is not exempt", "/healthz-internal", "", http.StatusUnauthorized},
		{"metrics prefix is not exempt", "/metricsfoo", "", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tt.path, nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			w := httptest.NewRecorder()

			s.authMiddleware(next).ServeHTTP(w, r)
			assert.Equal(t, tt.wantCode, w.Code)
		})
	}
}

func TestAuthMiddlewareDisabledWithoutSecret(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	w := httptest.NewRecorder()

	s.authMiddleware(next).ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestHealthEndpointsIgnoreTeamCityHeaders is the /readyz SSRF guard. These
// routes are unauthenticated, so they must be structurally unable to act on a
// client-supplied TeamCity URL - otherwise /readyz becomes an SSRF probe with a
// success oracle.
func TestHealthEndpointsIgnoreTeamCityHeaders(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the readiness probe must not reach a client-supplied server")
	}))
	defer probe.Close()

	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{
		URL:         "https://tc.example.com",
		AllowAnyURL: true,
	}})

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.credentialsMiddleware(http.HandlerFunc(s.handleMCP)))
	mux.HandleFunc("/readyz", s.health.ReadinessHandler)

	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	r.Header.Set(teamcity.HeaderURL, probe.URL)
	r.Header.Set(teamcity.HeaderToken, "attacker-token")
	w := httptest.NewRecorder()

	s.authMiddleware(mux).ServeHTTP(w, r)

	// No server token is configured, so the deep probe is skipped entirely.
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "skipped")
}

func TestReadyzSkipsProbeWithoutServerCredentials(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})

	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	s.health.ReadinessHandler(w, r)

	assert.Equal(t, http.StatusOK, w.Code, "a multi-tenant server with no own token is still ready")
	assert.Contains(t, w.Body.String(), "skipped")
}

func TestUpdateConfigRebuildsPolicy(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})

	newCfg := &config.Config{TeamCity: config.TeamCityConfig{
		URL:     "https://tc-new.example.com",
		Timeout: "30s",
	}}
	s.UpdateConfig(newCfg)

	// Previously the reload swapped only the config struct, so a changed TC_URL
	// was silently ignored.
	assert.Equal(t, "https://tc-new.example.com", s.provider.Policy().DefaultURL)
	assert.Equal(t, "https://tc-new.example.com", s.config().TeamCity.URL)
}

func TestUpdateConfigRejectsInvalidSettings(t *testing.T) {
	s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})

	s.UpdateConfig(&config.Config{TeamCity: config.TeamCityConfig{URL: "ftp://nope", Timeout: "30s"}})

	assert.Equal(t, "https://tc.example.com", s.provider.Policy().DefaultURL,
		"a bad reload must leave the running configuration intact")
}

func TestCheckOrigin(t *testing.T) {
	t.Run("absent Origin is allowed", func(t *testing.T) {
		s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		assert.True(t, s.checkOrigin(r), "non-browser clients send no Origin")
	})

	t.Run("Origin is rejected when no allowlist is configured", func(t *testing.T) {
		s := newTestServer(t, &config.Config{TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"}})
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		r.Header.Set("Origin", "https://evil.example.com")
		assert.False(t, s.checkOrigin(r))
	})

	t.Run("allowlisted Origin", func(t *testing.T) {
		s := newTestServer(t, &config.Config{
			TeamCity: config.TeamCityConfig{URL: "https://tc.example.com"},
			Server:   config.ServerConfig{AllowedOrigins: []string{"https://app.example.com"}},
		})
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		r.Header.Set("Origin", "https://app.example.com")
		assert.True(t, s.checkOrigin(r))

		r.Header.Set("Origin", "https://evil.example.com")
		assert.False(t, s.checkOrigin(r))
	})
}
