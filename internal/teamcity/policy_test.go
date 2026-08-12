package teamcity

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/itcaat/teamcity-mcp/internal/config"
)

func TestResolveURL(t *testing.T) {
	policy, err := NewPolicy(config.TeamCityConfig{
		URL:         "https://tc.example.com",
		AllowedURLs: []string{"https://tc2.example.com/context"},
	})
	require.NoError(t, err)

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{"empty falls back to the pinned URL", "", "https://tc.example.com", nil},
		{"exact match", "https://tc.example.com", "https://tc.example.com", nil},
		{"trailing slash is equivalent", "https://tc.example.com/", "https://tc.example.com", nil},
		{"host case is ignored", "https://TC.EXAMPLE.COM", "https://tc.example.com", nil},
		{"second allowlist entry", "https://tc2.example.com/context", "https://tc2.example.com/context", nil},
		{"context path may not be escaped", "https://tc2.example.com", "", ErrURLNotAllowed},
		{"different host", "https://evil.example.com", "", ErrURLNotAllowed},
		{"different port", "https://tc.example.com:8111", "", ErrURLNotAllowed},
		{"cloud metadata endpoint", "http://169.254.169.254/latest/meta-data", "", ErrURLNotAllowed},
		{"loopback", "http://127.0.0.1:8111", "", ErrURLNotAllowed},
		{"file scheme", "file:///etc/passwd", "", ErrURLNotAllowed},
		{"userinfo", "https://user:pw@tc.example.com", "", ErrURLNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := policy.ResolveURL(tt.input)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveURLAllowAny(t *testing.T) {
	policy, err := NewPolicy(config.TeamCityConfig{URL: "https://tc.example.com", AllowAnyURL: true})
	require.NoError(t, err)

	got, err := policy.ResolveURL("https://anything.example.net/tc/")
	require.NoError(t, err)
	assert.Equal(t, "https://anything.example.net/tc", got)

	// Even with pinning off, a non-HTTP scheme is still refused.
	_, err = policy.ResolveURL("file:///etc/passwd")
	assert.ErrorIs(t, err, ErrURLNotAllowed)
}

func TestResolveURLNoServerURL(t *testing.T) {
	policy, err := NewPolicy(config.TeamCityConfig{AllowAnyURL: true})
	require.NoError(t, err)

	_, err = policy.ResolveURL("")
	assert.ErrorIs(t, err, ErrNoCredentials)
}

func TestResolveCredentials(t *testing.T) {
	t.Run("token supplied by the client", func(t *testing.T) {
		policy, err := NewPolicy(config.TeamCityConfig{URL: "https://tc.example.com"})
		require.NoError(t, err)

		got, err := policy.Resolve(Credentials{Token: "user-token"})
		require.NoError(t, err)
		assert.Equal(t, Credentials{URL: "https://tc.example.com", Token: "user-token"}, got)
	})

	t.Run("no token and fallback disabled", func(t *testing.T) {
		policy, err := NewPolicy(config.TeamCityConfig{URL: "https://tc.example.com", Token: "server-token"})
		require.NoError(t, err)

		_, err = policy.Resolve(Credentials{})
		assert.ErrorIs(t, err, ErrNoCredentials,
			"the server token must not be usable by an anonymous caller by default")
	})

	t.Run("no token and fallback enabled", func(t *testing.T) {
		policy, err := NewPolicy(config.TeamCityConfig{
			URL:              "https://tc.example.com",
			Token:            "server-token",
			AllowServerToken: true,
		})
		require.NoError(t, err)

		got, err := policy.Resolve(Credentials{})
		require.NoError(t, err)
		assert.Equal(t, "server-token", got.Token)
	})

	t.Run("fallback enabled but no server token configured", func(t *testing.T) {
		policy, err := NewPolicy(config.TeamCityConfig{URL: "https://tc.example.com", AllowServerToken: true})
		require.NoError(t, err)

		_, err = policy.Resolve(Credentials{})
		assert.ErrorIs(t, err, ErrNoCredentials)
	})

	t.Run("the server token is confined to the server's own URL", func(t *testing.T) {
		policy, err := NewPolicy(config.TeamCityConfig{
			URL:              "https://tc.example.com",
			Token:            "server-token",
			AllowServerToken: true,
			AllowedURLs:      []string{"https://tc2.example.com"},
		})
		require.NoError(t, err)

		_, err = policy.Resolve(Credentials{URL: "https://tc2.example.com"})
		assert.ErrorIs(t, err, ErrNoCredentials,
			"the operator's token must never be sent to a client-selected server")
	})
}

func TestServerCredentials(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		policy, err := NewPolicy(config.TeamCityConfig{URL: "https://tc.example.com"})
		require.NoError(t, err)
		_, ok := policy.ServerCredentials()
		assert.False(t, ok)
	})

	t.Run("complete", func(t *testing.T) {
		policy, err := NewPolicy(config.TeamCityConfig{URL: "https://tc.example.com", Token: "t"})
		require.NoError(t, err)
		creds, ok := policy.ServerCredentials()
		require.True(t, ok)
		assert.Equal(t, Credentials{URL: "https://tc.example.com", Token: "t"}, creds)
	})
}

func TestCheckRedirect(t *testing.T) {
	policy, err := NewPolicy(config.TeamCityConfig{URL: "https://tc.example.com"})
	require.NoError(t, err)

	req := func(url string) *http.Request { return httptest.NewRequest(http.MethodGet, url, nil) }

	t.Run("same host allowed", func(t *testing.T) {
		err := policy.CheckRedirect(
			req("https://tc.example.com/elsewhere"),
			[]*http.Request{req("https://tc.example.com/app/rest/projects")},
		)
		assert.NoError(t, err)
	})

	t.Run("cross host refused", func(t *testing.T) {
		err := policy.CheckRedirect(
			req("https://evil.example.com/"),
			[]*http.Request{req("https://tc.example.com/app/rest/projects")},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cross-host redirect")
	})

	t.Run("redirect chain is bounded", func(t *testing.T) {
		via := make([]*http.Request, maxRedirects)
		for i := range via {
			via[i] = req("https://tc.example.com/")
		}
		err := policy.CheckRedirect(req("https://tc.example.com/again"), via)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "redirects")
	})
}

func TestIsCredentialError(t *testing.T) {
	assert.True(t, IsCredentialError(ErrNoCredentials))
	assert.True(t, IsCredentialError(ErrURLNotAllowed))
	assert.False(t, IsCredentialError(assert.AnError))
	assert.False(t, IsCredentialError(nil))
}
