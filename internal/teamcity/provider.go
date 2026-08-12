package teamcity

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/itcaat/teamcity-mcp/internal/config"
)

// Provider mints a per-request Client from per-request credentials.
//
// Clients are cheap value-like wrappers and are created per request rather than
// cached per tenant: the cache key would be attacker-controlled, so a map keyed
// by it is unbounded growth. What must be shared is the HTTP transport.
type Provider struct {
	httpClient *http.Client
	policy     atomic.Pointer[Policy]
	maxBytes   int64
	logger     *zap.SugaredLogger
}

// NewProvider creates a Provider with one shared HTTP client for the process.
func NewProvider(cfg config.TeamCityConfig, logger *zap.SugaredLogger) (*Provider, error) {
	policy, err := NewPolicy(cfg)
	if err != nil {
		return nil, err
	}

	httpClient, err := newHTTPClient(cfg, policy.CheckRedirect)
	if err != nil {
		return nil, err
	}

	p := &Provider{
		httpClient: httpClient,
		maxBytes:   maxBytesOrDefault(cfg.MaxResponseBytes),
		logger:     logger,
	}
	p.policy.Store(policy)
	return p, nil
}

// SetPolicy swaps the policy atomically, so SIGHUP reloads take effect without
// racing in-flight requests.
func (p *Provider) SetPolicy(policy *Policy) { p.policy.Store(policy) }

// Policy returns the current policy.
func (p *Provider) Policy() *Policy { return p.policy.Load() }

// CredentialsFromRequest extracts and validates credentials from HTTP headers.
// A missing token is not an error here - only the handler knows whether the
// request actually needs TeamCity.
func (p *Provider) CredentialsFromRequest(r *http.Request) (Credentials, error) {
	raw := Credentials{
		URL:   r.Header.Get(HeaderURL),
		Token: r.Header.Get(HeaderToken),
	}

	// An explicitly requested server must be permitted even if the request turns
	// out not to need credentials, so a rejected URL is reported immediately
	// rather than silently falling back to the default server.
	if raw.URL != "" {
		resolved, err := p.policy.Load().ResolveURL(raw.URL)
		if err != nil {
			return Credentials{}, err
		}
		raw.URL = resolved
	}

	return raw, nil
}

// ClientFor resolves credentials against the current policy and returns a Client.
func (p *Provider) ClientFor(creds Credentials) (*Client, error) {
	resolved, err := p.policy.Load().Resolve(creds)
	if err != nil {
		return nil, err
	}
	return &Client{
		httpClient: p.httpClient,
		baseURL:    resolved.URL,
		token:      resolved.Token,
		maxBytes:   p.maxBytes,
		logger:     p.logger,
	}, nil
}

// ClientForContext returns a Client for the credentials carried by ctx.
// Returns ErrNoCredentials when the request supplied none.
func (p *Provider) ClientForContext(ctx context.Context) (*Client, error) {
	creds, ok := CredentialsFromContext(ctx)
	if !ok {
		creds = Credentials{}
	}
	return p.ClientFor(creds)
}

// DefaultClient returns a Client built from the operator-configured credentials,
// for the /readyz deep probe. ok is false when TC_URL/TC_TOKEN are unset.
//
// This deliberately ignores request context: /readyz is unauthenticated, so
// honouring client headers there would make it an SSRF probe with a success oracle.
func (p *Provider) DefaultClient() (*Client, bool) {
	creds, ok := p.policy.Load().ServerCredentials()
	if !ok {
		return nil, false
	}
	return &Client{
		httpClient: p.httpClient,
		baseURL:    creds.URL,
		token:      creds.Token,
		maxBytes:   p.maxBytes,
		logger:     p.logger,
	}, true
}

// newHTTPClient builds the single HTTP client shared by every tenant.
func newHTTPClient(cfg config.TeamCityConfig, checkRedirect func(*http.Request, []*http.Request) error) (*http.Client, error) {
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("invalid timeout: %w", err)
	}

	maxIdlePerHost := cfg.MaxIdleConnsPerHost
	if maxIdlePerHost <= 0 {
		maxIdlePerHost = config.DefaultMaxIdleConnsPerHost
	}

	// Sharing a transport across tenants is safe: http.Transport keys its idle
	// connection pool on (scheme, host, proxy) only, never on request headers,
	// and the Authorization header is set per request.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          maxIdlePerHost * 8,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// Jar MUST stay nil. TeamCity issues a TCSESSIONID cookie; a shared cookie
		// jar would store one user's authenticated session and replay it on
		// another user's request - a cross-tenant authentication bypass.
		Jar:           nil,
		CheckRedirect: checkRedirect,
	}, nil
}

func maxBytesOrDefault(n int64) int64 {
	if n <= 0 {
		return config.DefaultMaxResponseBytes
	}
	return n
}
