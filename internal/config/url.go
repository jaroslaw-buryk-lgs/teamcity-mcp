package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// NormalizeBaseURL validates and canonicalizes a TeamCity base URL so that two
// spellings of the same server compare equal.
//
// This lives in config rather than the teamcity package because config cannot
// import teamcity (teamcity imports config), and both need it.
//
// Normalization is deliberately strict: the result is used for allowlist
// matching, so anything that could make two different servers look identical -
// or one server look like another - is rejected rather than massaged.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("URL is empty")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	// Credentials in the URL would be a second, undocumented way to authenticate.
	if u.User != nil {
		return "", errors.New("userinfo (user:password@) is not allowed in a TeamCity URL")
	}
	if u.RawQuery != "" {
		return "", errors.New("query string is not allowed in a TeamCity base URL")
	}
	if u.Fragment != "" {
		return "", errors.New("fragment is not allowed in a TeamCity base URL")
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("URL must be absolute, got %q", raw)
	}

	normalized := &url.URL{
		Scheme: scheme,
		Host:   strings.ToLower(u.Host),
		// TeamCity may be served under a context path, so the path is preserved -
		// only the trailing slash is dropped so ".../tc" and ".../tc/" match.
		Path: strings.TrimRight(u.Path, "/"),
	}

	return normalized.String(), nil
}
