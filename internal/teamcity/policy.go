package teamcity

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/itcaat/teamcity-mcp/internal/config"
)

// maxRedirects bounds how far a TeamCity response may bounce us. FetchBuildLog
// legitimately redirects within the same host; nothing needs more than a couple.
const maxRedirects = 3

// Policy decides which TeamCity server a request may reach and whether a request
// without a token is allowed to borrow the server's own token.
//
// Pinning the base URL server-side is what keeps a client-supplied
// X-TeamCity-Url from turning this process into an SSRF proxy.
type Policy struct {
	// DefaultURL is used when the client sends no X-TeamCity-Url. May be empty.
	DefaultURL string
	// Allowed is the full set of permitted normalized base URLs.
	Allowed []string
	// AllowAnyURL disables pinning entirely.
	AllowAnyURL bool
	// AllowServerToken permits falling back to serverToken when a request has none.
	AllowServerToken bool

	serverToken string
}

// NewPolicy builds a Policy from configuration. cfg.URL and cfg.AllowedURLs are
// expected to be already normalized by config.validate.
func NewPolicy(cfg config.TeamCityConfig) (*Policy, error) {
	p := &Policy{
		DefaultURL:       cfg.URL,
		AllowAnyURL:      cfg.AllowAnyURL,
		AllowServerToken: cfg.AllowServerToken,
		serverToken:      cfg.Token,
	}

	seen := map[string]bool{}
	add := func(raw string) error {
		if raw == "" {
			return nil
		}
		normalized, err := config.NormalizeBaseURL(raw)
		if err != nil {
			return fmt.Errorf("invalid TeamCity URL %q: %w", raw, err)
		}
		if !seen[normalized] {
			seen[normalized] = true
			p.Allowed = append(p.Allowed, normalized)
		}
		return nil
	}

	if err := add(cfg.URL); err != nil {
		return nil, err
	}
	for _, u := range cfg.AllowedURLs {
		if err := add(u); err != nil {
			return nil, err
		}
	}

	// Re-normalize DefaultURL so Policy is usable even if built from a config
	// that did not pass through validate (tests, direct construction).
	if p.DefaultURL != "" {
		normalized, err := config.NormalizeBaseURL(p.DefaultURL)
		if err != nil {
			return nil, fmt.Errorf("invalid default TeamCity URL: %w", err)
		}
		p.DefaultURL = normalized
	}

	return p, nil
}

// ResolveURL maps a client-supplied base URL (possibly empty) onto a permitted one.
func (p *Policy) ResolveURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		if p.DefaultURL == "" {
			return "", fmt.Errorf("%w: no TeamCity URL configured on the server and none supplied by the client", ErrNoCredentials)
		}
		return p.DefaultURL, nil
	}

	normalized, err := config.NormalizeBaseURL(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrURLNotAllowed, err)
	}
	if p.AllowAnyURL {
		return normalized, nil
	}
	// Compare whole normalized base URLs rather than hosts, so a TeamCity behind
	// a context path cannot be escaped to another app on the same host.
	if slices.Contains(p.Allowed, normalized) {
		return normalized, nil
	}
	return "", fmt.Errorf("%w: %s", ErrURLNotAllowed, normalized)
}

// Resolve completes a set of client-supplied credentials, applying URL pinning
// and the server-token fallback policy.
func (p *Policy) Resolve(c Credentials) (Credentials, error) {
	url, err := p.ResolveURL(c.URL)
	if err != nil {
		return Credentials{}, err
	}

	token := c.Token
	if token == "" {
		if !p.AllowServerToken || p.serverToken == "" {
			return Credentials{}, ErrNoCredentials
		}
		// Only legitimate when the operator has explicitly opted in, and only
		// against the server's own pinned URL - never a client-chosen one.
		if url != p.DefaultURL {
			return Credentials{}, fmt.Errorf("%w: the server token may only be used with the server's own TeamCity URL", ErrNoCredentials)
		}
		token = p.serverToken
	}

	return Credentials{URL: url, Token: token}, nil
}

// ServerCredentials returns the operator-configured credentials, if complete.
// Used for the /readyz deep probe and the stdio transport, never for HTTP callers.
func (p *Policy) ServerCredentials() (Credentials, bool) {
	if p.DefaultURL == "" || p.serverToken == "" {
		return Credentials{}, false
	}
	return Credentials{URL: p.DefaultURL, Token: p.serverToken}, true
}

// CheckRedirect refuses to follow a redirect to a different host.
//
// Go strips the Authorization header on cross-domain redirects, but the request
// itself still leaves for the new host - which is the SSRF pivot that URL
// pinning would otherwise miss. Same-host redirects stay allowed because
// FetchBuildLog depends on them.
func (p *Policy) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if len(via) == 0 {
		return nil
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return fmt.Errorf("refusing cross-host redirect to %s", req.URL.Host)
	}
	return nil
}

// IsCredentialError reports whether err is one of the credential sentinels.
func IsCredentialError(err error) bool {
	return errors.Is(err, ErrNoCredentials) || errors.Is(err, ErrURLNotAllowed)
}
