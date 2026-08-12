package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxResponseBytes caps how much of a TeamCity response is read into
// memory. Build logs are unbounded, so on a shared server an uncapped read is
// a trivial way to OOM the process.
const DefaultMaxResponseBytes int64 = 32 << 20 // 32 MiB

// DefaultMaxIdleConnsPerHost overrides net/http's default of 2, which is far
// too low when every request mints its own client over a shared transport.
const DefaultMaxIdleConnsPerHost = 32

// Config holds the complete server configuration
type Config struct {
	TeamCity TeamCityConfig
	Server   ServerConfig
	Logging  LoggingConfig
	Cache    CacheConfig
}

// TeamCityConfig holds TeamCity connection settings.
//
// URL and Token are optional: on the HTTP transport each user supplies their own
// token per request, and URL is only the default/pinned server. They are still
// required for the stdio transport and are used for the /readyz deep probe.
type TeamCityConfig struct {
	URL     string
	Token   string
	Timeout string

	// AllowedURLs is the set of TeamCity base URLs a client may select with the
	// X-TeamCity-Url header, in addition to URL.
	AllowedURLs []string

	// AllowAnyURL disables base-URL pinning entirely. This turns the server into
	// an SSRF proxy and must never be enabled in a shared deployment.
	AllowAnyURL bool

	// AllowServerToken lets HTTP/WebSocket requests that carry no token fall back
	// to Token. Off by default: on a shared server it would let any caller act as
	// whoever owns Token.
	AllowServerToken bool

	MaxResponseBytes    int64
	MaxIdleConnsPerHost int
}

// ServerConfig holds server settings
type ServerConfig struct {
	ListenAddr   string
	TLSCert      string
	TLSKey       string
	ServerSecret string

	// AllowedOrigins gates WebSocket upgrades that carry an Origin header.
	AllowedOrigins []string
}

// LoggingConfig holds logging settings
type LoggingConfig struct {
	Level  string
	Format string
}

// CacheConfig holds cache settings
type CacheConfig struct {
	TTL string
}

// EnvVar documents a single environment variable. This is the single source of
// truth for --help output, so the help text cannot drift from the code.
type EnvVar struct {
	Name        string
	Default     string
	Section     string
	Description string
}

// Section names, ordered as they should be printed.
const (
	SectionMultiTenant = "Multi-tenant HTTP (recommended)"
	SectionSingle      = "Single-tenant & stdio"
	SectionSecurity    = "Security"
	SectionOptional    = "Optional"
)

var sectionOrder = []string{SectionMultiTenant, SectionSingle, SectionSecurity, SectionOptional}

// EnvVars documents every environment variable the server reads.
var EnvVars = []EnvVar{
	{"TC_URL", "", SectionMultiTenant, "TeamCity base URL clients connect to (e.g. https://teamcity.example.com)"},
	{"TC_ALLOWED_URLS", "", SectionMultiTenant, "Comma-separated extra TeamCity base URLs a client may select via the X-TeamCity-Url header"},

	{"TC_TOKEN", "", SectionSingle, "TeamCity API token. Required for --transport stdio; also used for the /readyz deep probe"},

	{"SERVER_SECRET", "", SectionSecurity, "Enables the outer HMAC gate on 'Authorization: Bearer'. Unset disables it"},
	{"ALLOWED_ORIGINS", "", SectionSecurity, "Comma-separated origins allowed to open a WebSocket. Requests without an Origin header are always allowed"},
	{"TC_ALLOW_SERVER_TOKEN_FALLBACK", "false", SectionSecurity, "Let HTTP/WS requests with no token use TC_TOKEN. Unsafe on a shared server"},
	{"TC_ALLOW_ANY_URL", "false", SectionSecurity, "Disable TeamCity URL pinning. Turns the server into an SSRF proxy - do not enable"},
	{"TLS_CERT", "", SectionSecurity, "Path to TLS certificate file"},
	{"TLS_KEY", "", SectionSecurity, "Path to TLS private key file"},

	{"LISTEN_ADDR", ":8123", SectionOptional, "Address to listen on"},
	{"TC_TIMEOUT", "30s", SectionOptional, "HTTP timeout for TeamCity API calls"},
	{"TC_MAX_RESPONSE_BYTES", "33554432", SectionOptional, "Maximum TeamCity response size read into memory"},
	{"TC_MAX_IDLE_CONNS_PER_HOST", "32", SectionOptional, "Idle connections kept per TeamCity host"},
	{"LOG_LEVEL", "info", SectionOptional, "Log level: debug, info, warn, error"},
	{"LOG_FORMAT", "json", SectionOptional, "Log format: json, console"},
	{"CACHE_TTL", "10s", SectionOptional, "Cache TTL for TeamCity API responses"},
}

// Load loads configuration from environment variables only
func Load() (*Config, error) {
	cfg := &Config{
		// Default values
		TeamCity: TeamCityConfig{
			Timeout: getEnvOrDefault("TC_TIMEOUT", "30s"),
		},
		Server: ServerConfig{
			ListenAddr: getEnvOrDefault("LISTEN_ADDR", ":8123"),
		},
		Logging: LoggingConfig{
			Level:  getEnvOrDefault("LOG_LEVEL", "info"),
			Format: getEnvOrDefault("LOG_FORMAT", "json"),
		},
		Cache: CacheConfig{
			TTL: getEnvOrDefault("CACHE_TTL", "10s"),
		},
	}

	// Load from environment variables
	if err := loadFromEnv(cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// Validate required fields
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvBool parses a boolean env var. A malformed value is an error rather
// than a silent false, so a typo in a security flag cannot go unnoticed.
func getEnvBool(key string, defaultValue bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultValue, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s: %q is not a boolean", key, raw)
	}
	return v, nil
}

func getEnvInt64(key string, defaultValue int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultValue, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q is not an integer", key, raw)
	}
	if v <= 0 {
		return 0, fmt.Errorf("invalid %s: must be positive, got %d", key, v)
	}
	return v, nil
}

// splitList splits a comma-separated env var, trimming blanks and dropping
// empty entries.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadFromEnv(cfg *Config) error {
	// TeamCity configuration
	cfg.TeamCity.URL = strings.TrimSpace(os.Getenv("TC_URL"))
	cfg.TeamCity.Token = strings.TrimSpace(os.Getenv("TC_TOKEN"))
	cfg.TeamCity.AllowedURLs = splitList(os.Getenv("TC_ALLOWED_URLS"))

	var err error
	if cfg.TeamCity.AllowAnyURL, err = getEnvBool("TC_ALLOW_ANY_URL", false); err != nil {
		return err
	}
	if cfg.TeamCity.AllowServerToken, err = getEnvBool("TC_ALLOW_SERVER_TOKEN_FALLBACK", false); err != nil {
		return err
	}
	if cfg.TeamCity.MaxResponseBytes, err = getEnvInt64("TC_MAX_RESPONSE_BYTES", DefaultMaxResponseBytes); err != nil {
		return err
	}
	maxIdle, err := getEnvInt64("TC_MAX_IDLE_CONNS_PER_HOST", DefaultMaxIdleConnsPerHost)
	if err != nil {
		return err
	}
	cfg.TeamCity.MaxIdleConnsPerHost = int(maxIdle)

	// Server configuration
	cfg.Server.TLSCert = os.Getenv("TLS_CERT")
	cfg.Server.TLSKey = os.Getenv("TLS_KEY")
	cfg.Server.ServerSecret = os.Getenv("SERVER_SECRET")
	cfg.Server.AllowedOrigins = splitList(os.Getenv("ALLOWED_ORIGINS"))

	return nil
}

func validate(cfg *Config) error {
	// TC_URL and TC_TOKEN are optional here: on the HTTP transport credentials
	// arrive per request. Transport-specific requirements are enforced by
	// ValidateForTransport, which is the only place that knows the transport.
	if cfg.TeamCity.URL != "" {
		normalized, err := NormalizeBaseURL(cfg.TeamCity.URL)
		if err != nil {
			return fmt.Errorf("invalid TC_URL: %w", err)
		}
		cfg.TeamCity.URL = normalized
	}

	if cfg.TeamCity.Token != "" && cfg.TeamCity.URL == "" {
		return fmt.Errorf("TC_TOKEN is set but TC_URL is empty: a token without a server is unusable")
	}

	seen := map[string]bool{}
	if cfg.TeamCity.URL != "" {
		seen[cfg.TeamCity.URL] = true
	}
	var allowed []string
	for _, raw := range cfg.TeamCity.AllowedURLs {
		normalized, err := NormalizeBaseURL(raw)
		if err != nil {
			return fmt.Errorf("invalid entry %q in TC_ALLOWED_URLS: %w", raw, err)
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		allowed = append(allowed, normalized)
	}
	cfg.TeamCity.AllowedURLs = allowed

	// Validate timeout format
	if _, err := time.ParseDuration(cfg.TeamCity.Timeout); err != nil {
		return fmt.Errorf("invalid TC_TIMEOUT format: %w", err)
	}

	// Validate cache TTL format
	if _, err := time.ParseDuration(cfg.Cache.TTL); err != nil {
		return fmt.Errorf("invalid CACHE_TTL format: %w", err)
	}

	return nil
}

// ValidateForTransport applies the rules that depend on the selected transport.
// Load cannot do this because it never sees the -transport flag.
func (c *Config) ValidateForTransport(transport string) error {
	switch transport {
	case "stdio":
		// stdio has no HTTP headers, so credentials can only come from the
		// environment.
		if c.TeamCity.URL == "" {
			return fmt.Errorf("TC_URL is required for the stdio transport")
		}
		if c.TeamCity.Token == "" {
			return fmt.Errorf("TC_TOKEN is required for the stdio transport")
		}
		return nil
	case "http":
		// Clients supply tokens per request, but they must be able to reach at
		// least one permitted TeamCity server.
		if c.TeamCity.AllowAnyURL {
			return nil
		}
		if c.TeamCity.URL == "" && len(c.TeamCity.AllowedURLs) == 0 {
			return fmt.Errorf("no TeamCity server is configured: set TC_URL (or TC_ALLOWED_URLS) so clients have a server to authenticate against")
		}
		return nil
	default:
		return fmt.Errorf("unsupported transport: %s", transport)
	}
}

// PrintEnvHelp prints help text for environment variables, rendered from EnvVars.
func PrintEnvHelp() {
	fmt.Println("TeamCity MCP Server - Environment Variables:")
	fmt.Println()

	for _, section := range sectionOrder {
		var vars []EnvVar
		for _, v := range EnvVars {
			if v.Section == section {
				vars = append(vars, v)
			}
		}
		if len(vars) == 0 {
			continue
		}

		fmt.Printf("%s:\n", section)
		width := 0
		for _, v := range vars {
			if len(v.Name) > width {
				width = len(v.Name)
			}
		}
		for _, v := range vars {
			fmt.Printf("  %-*s  %s", width, v.Name, v.Description)
			if v.Default != "" {
				fmt.Printf(" (default: %s)", v.Default)
			}
			fmt.Println()
		}
		fmt.Println()
	}

	fmt.Println("Multi-tenant HTTP example - each user sends their own token:")
	fmt.Println("  export TC_URL=https://teamcity.example.com")
	fmt.Println("  ./server --transport http")
	fmt.Println("  # clients then send:  X-TeamCity-Token: <their own TeamCity API token>")
	fmt.Println()
	fmt.Println("Single-tenant stdio example:")
	fmt.Println("  export TC_URL=https://teamcity.example.com")
	fmt.Println("  export TC_TOKEN=your-teamcity-api-token")
	fmt.Println("  ./server --transport stdio")
}
