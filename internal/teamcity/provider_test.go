package teamcity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/itcaat/teamcity-mcp/internal/config"
)

// recorder is a fake TeamCity that records what it was sent.
type recorder struct {
	mu      sync.Mutex
	auths   []string
	cookies []string
	paths   []string

	handler http.HandlerFunc
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	rec.auths = append(rec.auths, r.Header.Get("Authorization"))
	rec.cookies = append(rec.cookies, r.Header.Get("Cookie"))
	rec.paths = append(rec.paths, r.URL.Path)
	rec.mu.Unlock()

	if rec.handler != nil {
		rec.handler(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"project":[{"id":"p1","name":"Project One"}]}`)
}

func (rec *recorder) snapshot() ([]string, []string, []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.auths...),
		append([]string(nil), rec.cookies...),
		append([]string(nil), rec.paths...)
}

func newTestProvider(t *testing.T, cfg config.TeamCityConfig) *Provider {
	t.Helper()
	if cfg.Timeout == "" {
		cfg.Timeout = "30s"
	}
	p, err := NewProvider(cfg, zaptest.NewLogger(t).Sugar())
	require.NoError(t, err)
	return p
}

// TestPerRequestTokenIsolation is the core multi-tenant guarantee: two callers
// hitting the same server must be authenticated as themselves, not as each other.
func TestPerRequestTokenIsolation(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	p := newTestProvider(t, config.TeamCityConfig{URL: srv.URL})

	for _, token := range []string{"token-A", "token-B"} {
		c, err := p.ClientFor(Credentials{Token: token})
		require.NoError(t, err)
		_, err = c.ListProjects(context.Background())
		require.NoError(t, err)
	}

	auths, _, _ := rec.snapshot()
	require.Len(t, auths, 2)
	assert.Equal(t, "Bearer token-A", auths[0])
	assert.Equal(t, "Bearer token-B", auths[1])
}

// TestNoCookieReplayAcrossTenants guards the sharpest cross-tenant leak: TeamCity
// issues a TCSESSIONID cookie, and a shared cookie jar would replay one user's
// authenticated session on another user's request.
func TestNoCookieReplayAcrossTenants(t *testing.T) {
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "TCSESSIONID", Value: "session-for-A", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"project":[]}`)
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	p := newTestProvider(t, config.TeamCityConfig{URL: srv.URL})
	assert.Nil(t, p.httpClient.Jar, "the shared http.Client must not have a cookie jar")

	for _, token := range []string{"token-A", "token-B"} {
		c, err := p.ClientFor(Credentials{Token: token})
		require.NoError(t, err)
		_, err = c.ListProjects(context.Background())
		require.NoError(t, err)
	}

	_, cookies, _ := rec.snapshot()
	require.Len(t, cookies, 2)
	for i, cookie := range cookies {
		assert.Empty(t, cookie, "request %d must not carry a cookie from an earlier tenant", i)
	}
}

// TestSharedTransport verifies connection pooling is actually shared, so building
// a client per request does not leak connections.
func TestSharedTransport(t *testing.T) {
	p := newTestProvider(t, config.TeamCityConfig{URL: "https://tc.example.com"})

	c1, err := p.ClientFor(Credentials{Token: "a"})
	require.NoError(t, err)
	c2, err := p.ClientFor(Credentials{Token: "b"})
	require.NoError(t, err)

	assert.Same(t, c1.httpClient, c2.httpClient)
	assert.NotSame(t, c1, c2)
	assert.NotEqual(t, c1.token, c2.token)
}

// TestFetchBuildLogUsesPerRequestToken covers the second auth site. FetchBuildLog
// builds its own request against a non-REST endpoint, so it needs its own test.
func TestFetchBuildLogUsesPerRequestToken(t *testing.T) {
	rec := &recorder{handler: func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "build log line one\nbuild log line two\n")
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	p := newTestProvider(t, config.TeamCityConfig{URL: srv.URL})
	c, err := p.ClientFor(Credentials{Token: "log-token"})
	require.NoError(t, err)

	out, err := c.FetchBuildLog(context.Background(), json.RawMessage(`{"buildId":"123"}`))
	require.NoError(t, err)
	assert.Contains(t, out, "build log line one")

	auths, _, paths := rec.snapshot()
	require.Len(t, auths, 1)
	assert.Equal(t, "Bearer log-token", auths[0], "FetchBuildLog must use the caller's token")
	assert.NotContains(t, paths[0], "/app/rest", "FetchBuildLog uses the non-REST download endpoint")
}

func TestCrossHostRedirectRefused(t *testing.T) {
	other := httptest.NewServer(&recorder{})
	defer other.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/app/rest/projects", http.StatusFound)
	}))
	defer redirector.Close()

	p := newTestProvider(t, config.TeamCityConfig{URL: redirector.URL})
	c, err := p.ClientFor(Credentials{Token: "t"})
	require.NoError(t, err)

	_, err = c.ListProjects(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-host redirect")
}

func TestSameHostRedirectFollowed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/elsewhere" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"project":[{"id":"p1","name":"One"}]}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, config.TeamCityConfig{URL: srv.URL})
	c, err := p.ClientFor(Credentials{Token: "t"})
	require.NoError(t, err)

	projects, err := c.ListProjects(context.Background())
	require.NoError(t, err, "same-host redirects must keep working - FetchBuildLog relies on them")
	assert.Len(t, projects, 1)
	assert.Equal(t, 2, hits)
}

func TestResponseSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 5000))
	}))
	defer srv.Close()

	p := newTestProvider(t, config.TeamCityConfig{URL: srv.URL, MaxResponseBytes: 1024})
	c, err := p.ClientFor(Credentials{Token: "t"})
	require.NoError(t, err)

	_, err = c.ListProjects(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded")
}

func TestAuthFailureMessageIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "Authentication required")
	}))
	defer srv.Close()

	p := newTestProvider(t, config.TeamCityConfig{URL: srv.URL})
	c, err := p.ClientFor(Credentials{Token: "bad"})
	require.NoError(t, err)

	_, err = c.ListProjects(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TeamCity rejected the supplied token")
	assert.NotContains(t, err.Error(), "bad", "the error must not echo the token")
}

func TestClientForContext(t *testing.T) {
	p := newTestProvider(t, config.TeamCityConfig{URL: "https://tc.example.com"})

	t.Run("no credentials in context", func(t *testing.T) {
		_, err := p.ClientForContext(context.Background())
		assert.ErrorIs(t, err, ErrNoCredentials)
	})

	t.Run("credentials in context", func(t *testing.T) {
		ctx := WithCredentials(context.Background(), Credentials{Token: "t"})
		c, err := p.ClientForContext(ctx)
		require.NoError(t, err)
		assert.Equal(t, "https://tc.example.com", c.baseURL)
		assert.Equal(t, "t", c.token)
	})
}

func TestCredentialsFromRequest(t *testing.T) {
	p := newTestProvider(t, config.TeamCityConfig{
		URL:         "https://tc.example.com",
		AllowedURLs: []string{"https://other.example.com"},
	})

	t.Run("token only uses the pinned URL", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(HeaderToken, "my-token")

		creds, err := p.CredentialsFromRequest(r)
		require.NoError(t, err)
		assert.Equal(t, "my-token", creds.Token)
		assert.Empty(t, creds.URL, "an unspecified URL is resolved later, from policy")
	})

	t.Run("allowed URL override", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(HeaderToken, "my-token")
		r.Header.Set(HeaderURL, "https://other.example.com/")

		creds, err := p.CredentialsFromRequest(r)
		require.NoError(t, err)
		assert.Equal(t, "https://other.example.com", creds.URL)
	})

	t.Run("disallowed URL is rejected", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(HeaderToken, "my-token")
		r.Header.Set(HeaderURL, "http://169.254.169.254/latest/meta-data")

		_, err := p.CredentialsFromRequest(r)
		assert.ErrorIs(t, err, ErrURLNotAllowed)
	})

	t.Run("the server secret header is not a TeamCity token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer server-secret-hmac")

		creds, err := p.CredentialsFromRequest(r)
		require.NoError(t, err)
		assert.True(t, creds.IsZero())
	})
}

func TestDefaultClient(t *testing.T) {
	t.Run("absent without a server token", func(t *testing.T) {
		p := newTestProvider(t, config.TeamCityConfig{URL: "https://tc.example.com"})
		_, ok := p.DefaultClient()
		assert.False(t, ok, "/readyz must degrade when TC_TOKEN is unset")
	})

	t.Run("present with a server token", func(t *testing.T) {
		p := newTestProvider(t, config.TeamCityConfig{URL: "https://tc.example.com", Token: "probe"})
		c, ok := p.DefaultClient()
		require.True(t, ok)
		assert.Equal(t, "probe", c.token)
	})
}

func TestFingerprintNeverRevealsToken(t *testing.T) {
	c := Credentials{URL: "https://tc.example.com", Token: "super-secret-token"}
	fp := c.Fingerprint()

	assert.Len(t, fp, 12)
	assert.NotContains(t, fp, "secret")
	assert.Equal(t, fp, c.Fingerprint(), "fingerprints must be stable")
	assert.NotEqual(t, fp, Credentials{URL: c.URL, Token: "other"}.Fingerprint())
	assert.Equal(t, "none", Credentials{}.Fingerprint())
}
