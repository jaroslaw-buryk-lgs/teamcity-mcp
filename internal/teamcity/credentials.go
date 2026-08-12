package teamcity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// HTTP headers carrying per-request TeamCity credentials.
//
// These are deliberately distinct from "Authorization", which the server's own
// optional SERVER_SECRET gate consumes. Reusing Authorization for both would
// make the two failure modes indistinguishable.
const (
	HeaderToken = "X-TeamCity-Token"
	HeaderURL   = "X-TeamCity-Url"
)

// Credentials identifies which TeamCity server to talk to, and as whom.
type Credentials struct {
	// URL is the normalized TeamCity base URL.
	URL string
	// Token is a TeamCity API token belonging to the calling user. Never log it.
	Token string
}

// IsZero reports whether no credentials are present at all.
func (c Credentials) IsZero() bool { return c.URL == "" && c.Token == "" }

// Fingerprint returns a short, stable, non-reversible identifier for these
// credentials. Use it wherever a tenant needs to be named - log lines, cache
// keys - so the token itself never leaves this package.
func (c Credentials) Fingerprint() string {
	if c.IsZero() {
		return "none"
	}
	sum := sha256.Sum256([]byte(c.URL + "\x00" + c.Token))
	return hex.EncodeToString(sum[:])[:12]
}

type credsKey struct{}

// WithCredentials returns a context carrying the given credentials.
func WithCredentials(ctx context.Context, c Credentials) context.Context {
	return context.WithValue(ctx, credsKey{}, c)
}

// CredentialsFromContext extracts credentials placed by WithCredentials.
func CredentialsFromContext(ctx context.Context) (Credentials, bool) {
	c, ok := ctx.Value(credsKey{}).(Credentials)
	return c, ok
}

var (
	// ErrNoCredentials means the request carried no usable TeamCity token. This
	// is a client configuration problem, not a server error.
	ErrNoCredentials = errors.New("no TeamCity credentials supplied")

	// ErrURLNotAllowed means the client asked for a TeamCity server this
	// deployment does not permit.
	ErrURLNotAllowed = errors.New("TeamCity URL is not allowed by server policy")
)
