package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/itcaat/teamcity-mcp/internal/config"
	"github.com/itcaat/teamcity-mcp/internal/health"
	"github.com/itcaat/teamcity-mcp/internal/mcp"
	"github.com/itcaat/teamcity-mcp/internal/metrics"
	"github.com/itcaat/teamcity-mcp/internal/teamcity"
)

// wsRequestTimeout bounds a single WebSocket message. Without it, a hijacked
// connection has no cancellation path at all.
const wsRequestTimeout = 5 * time.Minute

// unauthenticatedPaths bypass the SERVER_SECRET gate. Matched exactly: a prefix
// match would let /readyzzz through as well.
var unauthenticatedPaths = []string{"/healthz", "/readyz", "/metrics"}

// Server represents the MCP server
type Server struct {
	// cfg is swapped atomically so SIGHUP does not race in-flight requests.
	cfg      atomic.Pointer[config.Config]
	logger   *zap.SugaredLogger
	provider *teamcity.Provider
	health   *health.Checker
	mcp      *mcp.Handler
	upgrader websocket.Upgrader

	// baseCtx is the server lifecycle context, used to scope hijacked
	// WebSocket connections. Set by startHTTP.
	baseCtx context.Context
}

// New creates a new MCP server instance
func New(cfg *config.Config, logger *zap.SugaredLogger) (*Server, error) {
	// One provider for the process. It mints a per-request client from the
	// caller's own credentials over a single shared HTTP transport, so no
	// process-wide TeamCity identity exists.
	provider, err := teamcity.NewProvider(cfg.TeamCity, logger)
	if err != nil {
		return nil, fmt.Errorf("creating TeamCity provider: %w", err)
	}

	// The readiness probe is the only consumer of the operator's own credentials,
	// and tolerates their absence.
	probeClient, _ := provider.DefaultClient()

	s := &Server{
		logger:   logger,
		provider: provider,
		health:   health.New(probeClient, logger),
		mcp:      mcp.NewHandler(provider, logger),
	}
	s.cfg.Store(cfg)

	s.upgrader = websocket.Upgrader{CheckOrigin: s.checkOrigin}

	return s, nil
}

// config returns the current configuration snapshot.
func (s *Server) config() *config.Config { return s.cfg.Load() }

// Start starts the server with the specified transport
func (s *Server) Start(ctx context.Context, transport string) error {
	switch transport {
	case "http":
		return s.startHTTP(ctx)
	case "stdio":
		return s.startSTDIO(ctx)
	default:
		return fmt.Errorf("unsupported transport: %s", transport)
	}
}

// startHTTP starts the HTTP server
func (s *Server) startHTTP(ctx context.Context) error {
	s.baseCtx = ctx
	cfg := s.config()

	mux := http.NewServeMux()

	// MCP endpoint - the only route that accepts per-request TeamCity credentials.
	mux.Handle("/mcp", s.credentialsMiddleware(http.HandlerFunc(s.handleMCP)))

	// Health and metrics endpoints are registered OUTSIDE credentialsMiddleware.
	// They are unauthenticated, so they must be structurally incapable of acting
	// on a client-supplied TeamCity URL.
	mux.HandleFunc("/healthz", s.health.LivenessHandler)
	mux.HandleFunc("/readyz", s.health.ReadinessHandler)
	mux.HandleFunc("/metrics", s.handleMetrics)

	server := &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: s.authMiddleware(mux),
	}

	// Configure TLS if certificates are provided
	if cfg.Server.TLSCert != "" && cfg.Server.TLSKey != "" {
		server.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS13,
		}
	}

	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		s.logger.Info("Starting HTTP server", "addr", cfg.Server.ListenAddr)
		if cfg.Server.TLSCert != "" && cfg.Server.TLSKey != "" {
			errChan <- server.ListenAndServeTLS(cfg.Server.TLSCert, cfg.Server.TLSKey)
		} else {
			errChan <- server.ListenAndServe()
		}
	}()

	// Wait for context cancellation or server error
	select {
	case <-ctx.Done():
		s.logger.Info("Shutting down HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errChan:
		return err
	}
}

// startSTDIO starts the STDIO transport.
//
// stdio has no headers, so the operator's TC_URL/TC_TOKEN are bound into the
// context once for the lifetime of the process. This keeps stdio on exactly the
// same handler code path as the multi-tenant HTTP transport.
func (s *Server) startSTDIO(ctx context.Context) error {
	s.logger.Info("Starting STDIO transport")

	creds, ok := s.provider.Policy().ServerCredentials()
	if !ok {
		return fmt.Errorf("the stdio transport requires TC_URL and TC_TOKEN to be set")
	}
	ctx = teamcity.WithCredentials(ctx, creds)

	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			var req json.RawMessage
			if err := decoder.Decode(&req); err != nil {
				if err == io.EOF {
					return nil
				}
				s.logger.Error("Failed to decode request", "error", err)
				continue
			}

			resp, err := s.mcp.HandleRequest(ctx, req)
			if err != nil {
				s.logger.Error("Failed to handle request", "error", err)
				continue
			}

			if resp != nil {
				if err := encoder.Encode(resp); err != nil {
					s.logger.Error("Failed to encode response", "error", err)
				}
			}
		}
	}
}

// handleMCP handles MCP requests over HTTP/WebSocket
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		s.handleWebSocket(w, r)
		return
	}

	// Handle regular HTTP MCP request
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	resp, err := s.mcp.HandleRequest(r.Context(), req)
	if err != nil {
		s.logger.Error("Failed to handle MCP request", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Error("Failed to encode response", "error", err)
	}
}

// handleWebSocket handles WebSocket connections.
//
// Headers exist only at upgrade time, so credentials are captured before the
// upgrade and bound to the connection for its whole lifetime.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	creds, hasCreds := teamcity.CredentialsFromContext(r.Context())

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("Failed to upgrade to WebSocket", "error", err)
		return
	}
	defer conn.Close()

	// Derive from the server lifecycle context, NOT r.Context(): once Upgrade has
	// hijacked the connection, net/http no longer cancels the request context, so
	// r.Context() would never fire - neither on client disconnect nor on shutdown.
	connCtx, cancel := context.WithCancel(s.baseContext())
	defer cancel()
	if hasCreds {
		connCtx = teamcity.WithCredentials(connCtx, creds)
	}

	metrics.ServerConnections.WithLabelValues("websocket").Inc()
	defer metrics.ServerConnections.WithLabelValues("websocket").Dec()

	s.logger.Info("WebSocket connection established", "tenant", creds.Fingerprint())

	for {
		var req json.RawMessage
		if err := conn.ReadJSON(&req); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				s.logger.Error("WebSocket error", "error", err)
			}
			break
		}

		reqCtx, reqCancel := context.WithTimeout(connCtx, wsRequestTimeout)
		resp, err := s.mcp.HandleRequest(reqCtx, req)
		reqCancel()

		if err != nil {
			s.logger.Error("Failed to handle WebSocket request", "error", err)
			continue
		}

		if resp != nil {
			if err := conn.WriteJSON(resp); err != nil {
				s.logger.Error("Failed to write WebSocket response", "error", err)
				break
			}
		}
	}
}

// baseContext returns the server lifecycle context, falling back to Background
// when the server was constructed without going through startHTTP (tests).
func (s *Server) baseContext() context.Context {
	if s.baseCtx != nil {
		return s.baseCtx
	}
	return context.Background()
}

// checkOrigin gates WebSocket upgrades.
//
// Non-browser clients send no Origin and are always allowed. A browser cannot
// set X-TeamCity-Token on a WebSocket, so a malicious page has no credentials -
// but it could still drive the server if server-token fallback were enabled,
// which is why that fallback is off by default.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	allowed := s.config().Server.AllowedOrigins
	if len(allowed) == 0 {
		s.logger.Warn("Rejected WebSocket upgrade with an Origin header; set ALLOWED_ORIGINS to permit browser clients",
			"origin", origin)
		return false
	}
	return slices.Contains(allowed, origin)
}

// handleMetrics handles Prometheus metrics endpoint
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// This will be implemented by importing prometheus handler
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("# Metrics endpoint placeholder\n"))
}

// credentialsMiddleware extracts the caller's TeamCity credentials from request
// headers and puts them in the request context.
//
// It deliberately does NOT reject a request that carries no token: initialize,
// tools/list, ping and get_current_time must work without credentials so a
// client can connect and discover tools. Only the MCP handler knows whether a
// given method needs TeamCity, and it answers with an actionable JSON-RPC error.
//
// A disallowed URL is different - that is rejected here with 400 so an SSRF
// probe never reaches the handler, and so a client cannot silently be served a
// different TeamCity than the one it asked for.
func (s *Server) credentialsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creds, err := s.provider.CredentialsFromRequest(r)
		if err != nil {
			s.logger.Warn("Rejected request with a disallowed TeamCity URL", "error", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if !creds.IsZero() {
			r = r.WithContext(teamcity.WithCredentials(r.Context(), creds))
		}

		next.ServeHTTP(w, r)
	})
}

// authMiddleware provides the optional outer HMAC gate (SERVER_SECRET).
// This is separate from TeamCity credentials: a 401 here always means the
// server secret was wrong, never that a TeamCity token was missing.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health and metrics endpoints (exact match).
		if slices.Contains(unauthenticatedPaths, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// If no server secret is configured, skip authentication
		secret := s.config().Server.ServerSecret
		if secret == "" {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}

		if !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "Bearer token required", http.StatusUnauthorized)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if !validateToken(secret, token) {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// validateToken validates the HMAC token
func validateToken(secret, token string) bool {
	// Simple HMAC validation - in production, implement proper token validation
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("teamcity-mcp"))
	expectedToken := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(token), []byte(expectedToken))
}

// UpdateConfig updates the server configuration (for SIGHUP).
//
// The TeamCity policy is rebuilt so a changed TC_URL/TC_ALLOWED_URLS actually
// takes effect - previously the reload silently did nothing for TeamCity.
func (s *Server) UpdateConfig(cfg *config.Config) {
	policy, err := teamcity.NewPolicy(cfg.TeamCity)
	if err != nil {
		s.logger.Error("Ignoring configuration reload: invalid TeamCity settings", "error", err)
		return
	}

	s.cfg.Store(cfg)
	s.provider.SetPolicy(policy)
	s.logger.Info("Configuration updated")
}
