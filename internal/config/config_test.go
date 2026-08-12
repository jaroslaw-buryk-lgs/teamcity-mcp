package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearEnv unsets every variable the server reads, so a test never inherits the
// developer's own TC_URL/TC_TOKEN.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, v := range EnvVars {
		t.Setenv(v.Name, "")
		require.NoError(t, os.Unsetenv(v.Name))
	}
}

// TestLoadWithEmptyEnvironment is the headline behavior change: the server used
// to refuse to start without TC_URL and TC_TOKEN, because it needed one global
// identity. Credentials are now per request, so an empty environment is valid.
func TestLoadWithEmptyEnvironment(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.TeamCity.URL)
	assert.Empty(t, cfg.TeamCity.Token)
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":8123", cfg.Server.ListenAddr)
	assert.Equal(t, "30s", cfg.TeamCity.Timeout)
	assert.Equal(t, "10s", cfg.Cache.TTL)
	assert.Equal(t, "info", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
	assert.Equal(t, DefaultMaxResponseBytes, cfg.TeamCity.MaxResponseBytes)
	assert.Equal(t, DefaultMaxIdleConnsPerHost, cfg.TeamCity.MaxIdleConnsPerHost)
	assert.False(t, cfg.TeamCity.AllowAnyURL)
	assert.False(t, cfg.TeamCity.AllowServerToken, "server-token fallback must be off by default")
}

func TestLoadTokenWithoutURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("TC_TOKEN", "some-token")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TC_URL")
}

func TestLoadNormalizesURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "trailing slash", in: "https://tc.example.com/", want: "https://tc.example.com"},
		{name: "uppercase host", in: "https://TC.Example.COM", want: "https://tc.example.com"},
		{name: "context path preserved", in: "https://tc.example.com/teamcity/", want: "https://tc.example.com/teamcity"},
		{name: "port preserved", in: "http://tc.example.com:8111", want: "http://tc.example.com:8111"},
		{name: "userinfo rejected", in: "https://user:pw@tc.example.com", wantErr: "userinfo"},
		{name: "bad scheme rejected", in: "ftp://tc.example.com", wantErr: "scheme"},
		{name: "query rejected", in: "https://tc.example.com/?a=b", wantErr: "query"},
		{name: "fragment rejected", in: "https://tc.example.com/#x", wantErr: "fragment"},
		{name: "missing host rejected", in: "https:///path", wantErr: "host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TC_URL", tt.in)

			cfg, err := Load()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.TeamCity.URL)
		})
	}
}

func TestLoadAllowedURLs(t *testing.T) {
	t.Run("parses, trims and dedupes", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("TC_URL", "https://tc.example.com")
		t.Setenv("TC_ALLOWED_URLS", " https://a.example.com , ,https://b.example.com/, https://a.example.com , https://tc.example.com ")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, cfg.TeamCity.AllowedURLs,
			"duplicates and the already-pinned TC_URL are dropped")
	})

	t.Run("an invalid entry fails the load", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("TC_ALLOWED_URLS", "https://ok.example.com,not a url at all")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TC_ALLOWED_URLS")
	})
}

func TestLoadBooleanParsing(t *testing.T) {
	for _, raw := range []string{"true", "TRUE", "1", "t"} {
		t.Run("accepts "+raw, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TC_ALLOW_ANY_URL", raw)

			cfg, err := Load()
			require.NoError(t, err)
			assert.True(t, cfg.TeamCity.AllowAnyURL)
		})
	}

	t.Run("a malformed value is an error, not a silent false", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("TC_ALLOW_ANY_URL", "maybe")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TC_ALLOW_ANY_URL")
	})
}

func TestLoadNumericParsing(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("TC_MAX_RESPONSE_BYTES", "2048")
		t.Setenv("TC_MAX_IDLE_CONNS_PER_HOST", "8")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, int64(2048), cfg.TeamCity.MaxResponseBytes)
		assert.Equal(t, 8, cfg.TeamCity.MaxIdleConnsPerHost)
	})

	for _, raw := range []string{"lots", "0", "-1"} {
		t.Run("rejects "+raw, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TC_MAX_RESPONSE_BYTES", raw)

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "TC_MAX_RESPONSE_BYTES")
		})
	}
}

func TestLoadDurationValidationStillApplies(t *testing.T) {
	t.Run("bad TC_TIMEOUT", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("TC_TIMEOUT", "thirty seconds")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TC_TIMEOUT")
	})

	t.Run("bad CACHE_TTL", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("CACHE_TTL", "ages")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CACHE_TTL")
	})
}

func TestLoadAllowedOrigins(t *testing.T) {
	clearEnv(t)
	t.Setenv("ALLOWED_ORIGINS", "https://app.example.com, https://other.example.com")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"https://app.example.com", "https://other.example.com"}, cfg.Server.AllowedOrigins)
}

func TestValidateForTransport(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		cfg       TeamCityConfig
		wantErr   string
	}{
		{
			name:      "stdio requires a URL",
			transport: "stdio",
			cfg:       TeamCityConfig{Token: "t"},
			wantErr:   "TC_URL",
		},
		{
			name:      "stdio requires a token",
			transport: "stdio",
			cfg:       TeamCityConfig{URL: "https://tc.example.com"},
			wantErr:   "TC_TOKEN",
		},
		{
			name:      "stdio with both",
			transport: "stdio",
			cfg:       TeamCityConfig{URL: "https://tc.example.com", Token: "t"},
		},
		{
			name:      "http needs a reachable server",
			transport: "http",
			cfg:       TeamCityConfig{},
			wantErr:   "no TeamCity server is configured",
		},
		{
			name:      "http with a pinned URL and no token",
			transport: "http",
			cfg:       TeamCityConfig{URL: "https://tc.example.com"},
		},
		{
			name:      "http with only an allowlist",
			transport: "http",
			cfg:       TeamCityConfig{AllowedURLs: []string{"https://tc.example.com"}},
		},
		{
			name:      "http with pinning disabled",
			transport: "http",
			cfg:       TeamCityConfig{AllowAnyURL: true},
		},
		{
			name:      "unknown transport",
			transport: "carrier-pigeon",
			cfg:       TeamCityConfig{URL: "https://tc.example.com", Token: "t"},
			wantErr:   "unsupported transport",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{TeamCity: tt.cfg}
			err := cfg.ValidateForTransport(tt.transport)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestPrintEnvHelpCoversEveryVar guards against the drift that made the old
// hand-maintained help text disagree with the code.
func TestPrintEnvHelpCoversEveryVar(t *testing.T) {
	out := captureStdout(t, PrintEnvHelp)

	for _, v := range EnvVars {
		assert.Contains(t, out, v.Name, "PrintEnvHelp must document %s", v.Name)
	}
	for _, section := range sectionOrder {
		assert.Contains(t, out, section)
	}
	// The per-request contract is the thing a new operator most needs to see.
	assert.Contains(t, out, "X-TeamCity-Token")
}

func TestEnvVarsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range EnvVars {
		assert.NotEmpty(t, v.Description, "%s needs a description", v.Name)
		assert.Contains(t, sectionOrder, v.Section, "%s has an unknown section %q", v.Name, v.Section)
		assert.False(t, seen[v.Name], "%s is listed twice", v.Name)
		seen[v.Name] = true
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()
	require.NoError(t, w.Close())

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
