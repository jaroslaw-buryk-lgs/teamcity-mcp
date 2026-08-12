package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/itcaat/teamcity-mcp/internal/metrics"
	"github.com/itcaat/teamcity-mcp/internal/teamcity"
)

// JSON-RPC error codes in the implementation-defined range (-32000..-32099).
//
// Deliberately not -32603 (which reads as a server bug and prompts clients to
// retry) and not -32602 (the params were fine; the transport headers were not).
const (
	CodeCredentialsRequired = -32001
	CodeURLNotAllowed       = -32002
)

// credsHelp is returned verbatim as the JSON-RPC error message. The whole
// instruction has to be in `message`, because several MCP clients surface only
// that field.
const credsHelp = "TeamCity credentials required. Send the HTTP header " +
	"'" + teamcity.HeaderToken + ": <your TeamCity API token>' with each request. " +
	"(On the stdio transport, set the TC_URL and TC_TOKEN environment variables instead.)"

// ClientResolver produces a TeamCity client for a request. It is satisfied by
// *teamcity.Provider (multi-tenant HTTP) and by *teamcity.Client itself
// (single-tenant stdio and tests).
type ClientResolver interface {
	ClientForContext(ctx context.Context) (*teamcity.Client, error)
}

// Handler handles MCP protocol messages
type Handler struct {
	clients ClientResolver
	logger  *zap.SugaredLogger
}

// NewHandler creates a new MCP handler
func NewHandler(clients ClientResolver, logger *zap.SugaredLogger) *Handler {
	return &Handler{
		clients: clients,
		logger:  logger,
	}
}

// client resolves the TeamCity client for this request.
func (h *Handler) client(ctx context.Context) (*teamcity.Client, error) {
	return h.clients.ClientForContext(ctx)
}

// credentialError maps the credential sentinels onto actionable JSON-RPC errors.
// Returns nil when err is not credential-related, so callers fall through to
// their normal error handling.
func (h *Handler) credentialError(id interface{}, err error) interface{} {
	switch {
	case errors.Is(err, teamcity.ErrNoCredentials):
		return h.errorResponse(id, CodeCredentialsRequired, credsHelp, map[string]interface{}{
			"tokenHeader": teamcity.HeaderToken,
			"urlHeader":   teamcity.HeaderURL,
		})
	case errors.Is(err, teamcity.ErrURLNotAllowed):
		return h.errorResponse(id, CodeURLNotAllowed, err.Error(), nil)
	default:
		return nil
	}
}

// HandleRequest handles an MCP JSON-RPC request
func (h *Handler) HandleRequest(ctx context.Context, req json.RawMessage) (interface{}, error) {
	start := time.Now()

	// Parse basic JSON-RPC structure
	var baseReq struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      interface{}     `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}

	if err := json.Unmarshal(req, &baseReq); err != nil {
		return h.errorResponse(nil, -32700, "Parse error", nil), nil
	}

	// Validate JSON-RPC version
	if baseReq.JSONRPC != "2.0" {
		return h.errorResponse(baseReq.ID, -32600, "Invalid Request", nil), nil
	}

	resp, err := h.route(ctx, baseReq.Method, baseReq.ID, baseReq.Params)

	// Record metrics. The status has to be derived from the response, not
	// hardcoded, or credential and tool failures are invisible in metrics.
	metrics.RecordMCPRequest(baseReq.Method, responseStatus(resp, err), time.Since(start).Seconds())

	return resp, err
}

// responseStatus classifies a handled request for the metrics status label.
func responseStatus(resp interface{}, err error) string {
	if err != nil {
		return "error"
	}
	if m, ok := resp.(map[string]interface{}); ok {
		if _, isErr := m["error"]; isErr {
			return "error"
		}
	}
	return "success"
}

// route dispatches a parsed JSON-RPC request to the appropriate handler.
func (h *Handler) route(ctx context.Context, method string, id interface{}, params json.RawMessage) (interface{}, error) {
	switch method {
	case "initialize":
		return h.handleInitialize(id, params)
	case "initialized":
		return h.handleInitialized(id)
	case "notifications/initialized":
		return h.handleInitialized(id)
	case "notifications/cancelled":
		// Handle cancellation notifications - just log and return nil (no response for notifications)
		h.logger.Debug("Received cancellation notification")
		return nil, nil
	case "resources/list":
		return h.handleResourcesList(ctx, id, params)
	case "resources/read":
		return h.handleResourcesRead(ctx, id, params)
	case "tools/list":
		return h.handleToolsList(id)
	case "tools/call":
		return h.handleToolsCall(ctx, id, params)
	case "ping":
		return h.handlePing(id)
	default:
		h.logger.Warn("Unknown method called", "method", method, "id", id)
		// Only return an error response if this is a request (has an ID), not a notification
		if id != nil {
			return h.errorResponse(id, -32601, "Method not found", nil), nil
		}
		// For notifications, just return nil (no response)
		return nil, nil
	}
}

// handleInitialize handles the initialize request
func (h *Handler) handleInitialize(id interface{}, params json.RawMessage) (interface{}, error) {
	currentTime := time.Now()
	return h.successResponse(id, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"resources": map[string]interface{}{
				"subscribe":   false,
				"listChanged": false,
			},
			"tools":   map[string]interface{}{},
			"logging": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":        "teamcity-mcp",
			"version":     "1.0.0",
			"currentTime": currentTime.Format(time.RFC3339),
			"currentDate": currentTime.Format("2006-01-02"),
			"timezone":    currentTime.Location().String(),
		},
	}), nil
}

// handleInitialized handles the initialized notification
func (h *Handler) handleInitialized(id interface{}) (interface{}, error) {
	// Notification - no response needed
	return nil, nil
}

// handleResourcesList handles resources/list requests
func (h *Handler) handleResourcesList(ctx context.Context, id interface{}, params json.RawMessage) (interface{}, error) {
	var req struct {
		URI string `json:"uri"`
	}

	// Params can be empty or null for resources/list
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &req); err != nil {
			return h.errorResponse(id, -32602, "Invalid params", nil), nil
		}
	}

	resources, err := h.listResources(ctx, req.URI)
	if err != nil {
		if credErr := h.credentialError(id, err); credErr != nil {
			return credErr, nil
		}
		return h.errorResponse(id, -32603, "Internal error", err.Error()), nil
	}

	return h.successResponse(id, map[string]interface{}{
		"resources": resources,
	}), nil
}

// handleResourcesRead handles resources/read requests
func (h *Handler) handleResourcesRead(ctx context.Context, id interface{}, params json.RawMessage) (interface{}, error) {
	var req struct {
		URI string `json:"uri"`
	}

	if err := json.Unmarshal(params, &req); err != nil {
		return h.errorResponse(id, -32602, "Invalid params", nil), nil
	}

	resource, err := h.readResource(ctx, req.URI)
	if err != nil {
		if credErr := h.credentialError(id, err); credErr != nil {
			return credErr, nil
		}
		return h.errorResponse(id, -32603, "Internal error", err.Error()), nil
	}

	return h.successResponse(id, map[string]interface{}{
		"contents": []interface{}{resource},
	}), nil
}

// handleToolsList handles tools/list requests
func (h *Handler) handleToolsList(id interface{}) (interface{}, error) {
	tools := []map[string]interface{}{
		{
			"name":        "trigger_build",
			"description": "Trigger a new build",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildTypeId": map[string]interface{}{
						"type":        "string",
						"description": "Build configuration ID",
					},
					"branchName": map[string]interface{}{
						"type":        "string",
						"description": "Branch name (optional)",
					},
					"properties": map[string]interface{}{
						"type":        "object",
						"description": "Build properties",
					},
				},
				"required": []string{"buildTypeId"},
			},
		},
		{
			"name":        "cancel_build",
			"description": "Cancel a running build",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildId": map[string]interface{}{
						"type":        "string",
						"description": "Build ID to cancel",
					},
					"comment": map[string]interface{}{
						"type":        "string",
						"description": "Cancellation comment",
					},
				},
				"required": []string{"buildId"},
			},
		},
		{
			"name":        "pin_build",
			"description": "Pin or unpin a build",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildId": map[string]interface{}{
						"type":        "string",
						"description": "Build ID to pin/unpin",
					},
					"pin": map[string]interface{}{
						"type":        "boolean",
						"description": "True to pin, false to unpin",
					},
					"comment": map[string]interface{}{
						"type":        "string",
						"description": "Pin comment",
					},
				},
				"required": []string{"buildId", "pin"},
			},
		},
		{
			"name":        "set_build_tag",
			"description": "Add or remove build tags",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildId": map[string]interface{}{
						"type":        "string",
						"description": "Build ID",
					},
					"tags": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Tags to add",
					},
					"removeTags": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Tags to remove",
					},
				},
				"required": []string{"buildId"},
			},
		},
		{
			"name":        "download_artifact",
			"description": "Download build artifacts",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildId": map[string]interface{}{
						"type":        "string",
						"description": "Build ID",
					},
					"artifactPath": map[string]interface{}{
						"type":        "string",
						"description": "Artifact path",
					},
				},
				"required": []string{"buildId", "artifactPath"},
			},
		},
		{
			"name":        "search_builds",
			"description": "Search for builds with various filters",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildTypeId": map[string]interface{}{
						"type":        "string",
						"description": "Build configuration ID to filter by",
					},
					"status": map[string]interface{}{
						"type":        "string",
						"description": "Build status: SUCCESS, FAILURE, ERROR, UNKNOWN",
					},
					"state": map[string]interface{}{
						"type":        "string",
						"description": "Build state: queued, running, finished",
					},
					"branch": map[string]interface{}{
						"type":        "string",
						"description": "Branch name to filter by",
					},
					"agent": map[string]interface{}{
						"type":        "string",
						"description": "Agent name to filter by",
					},
					"user": map[string]interface{}{
						"type":        "string",
						"description": "User who triggered the build",
					},
					"sinceBuild": map[string]interface{}{
						"type":        "string",
						"description": "Search builds since this build ID",
					},
					"sinceDate": map[string]interface{}{
						"type":        "string",
						"description": "Search builds since this date (YYYYMMDDTHHMMSS+HHMM)",
					},
					"untilDate": map[string]interface{}{
						"type":        "string",
						"description": "Search builds until this date (YYYYMMDDTHHMMSS+HHMM)",
					},
					"tags": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Tags to filter by",
					},
					"personal": map[string]interface{}{
						"type":        "boolean",
						"description": "Include personal builds",
					},
					"pinned": map[string]interface{}{
						"type":        "boolean",
						"description": "Filter by pinned status",
					},
					"count": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of builds to return (default: 100)",
						"minimum":     1,
						"maximum":     1000,
					},
				},
			},
		},
		{
			"name":        "fetch_build_log",
			"description": "Fetch build log for a specific build with filtering options",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildId": map[string]interface{}{
						"type":        "string",
						"description": "Build ID to fetch log for",
					},
					"plain": map[string]interface{}{
						"type":        "boolean",
						"description": "Return log as plain text (default: true)",
					},
					"archived": map[string]interface{}{
						"type":        "boolean",
						"description": "Return log as zip archive (default: false)",
					},
					"dateFormat": map[string]interface{}{
						"type":        "string",
						"description": "Custom timestamp format (Java SimpleDateFormat)",
					},
					"maxLines": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of lines to return (limits output after filtering)",
					},
					"filterPattern": map[string]interface{}{
						"type":        "string",
						"description": "Regex pattern to filter log lines (only matching lines are returned)",
					},
					"severity": map[string]interface{}{
						"type":        "string",
						"description": "Filter by severity level: 'error', 'warning', or 'info'",
						"enum":        []string{"error", "warning", "info"},
					},
					"tailLines": map[string]interface{}{
						"type":        "integer",
						"description": "Return only the last N lines (applied after filtering, before maxLines)",
					},
				},
				"required": []string{"buildId"},
			},
		},
		{
			"name":        "search_build_configurations",
			"description": "Search for build configurations with comprehensive filters including basic filters, parameters, steps, and VCS roots",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"projectId": map[string]interface{}{
						"type":        "string",
						"description": "Filter by project ID",
					},
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Search by configuration name (partial matching)",
					},
					"enabled": map[string]interface{}{
						"type":        "boolean",
						"description": "Filter by enabled status",
					},
					"paused": map[string]interface{}{
						"type":        "boolean",
						"description": "Filter by paused status",
					},
					"template": map[string]interface{}{
						"type":        "boolean",
						"description": "Filter templates (true) or regular configurations (false)",
					},
					"parameterName": map[string]interface{}{
						"type":        "string",
						"description": "Search by parameter name (partial matching)",
					},
					"parameterValue": map[string]interface{}{
						"type":        "string",
						"description": "Search by parameter value (partial matching)",
					},
					"stepType": map[string]interface{}{
						"type":        "string",
						"description": "Search by build step type (e.g., 'gradle', 'docker', 'powershell')",
					},
					"stepName": map[string]interface{}{
						"type":        "string",
						"description": "Search by build step name (partial matching)",
					},
					"vcsType": map[string]interface{}{
						"type":        "string",
						"description": "Search by VCS type (e.g., 'git', 'subversion')",
					},
					"includeDetails": map[string]interface{}{
						"type":        "boolean",
						"description": "Include detailed information (parameters, steps, VCS) in results (default: false)",
					},
					"count": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of configurations to return (default: 100)",
						"minimum":     1,
						"maximum":     1000,
					},
				},
			},
		},
		{
			"name":        "get_current_time",
			"description": "Get the current server date and time - use this to get the real current date/time instead of assuming any training data dates",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"format": map[string]interface{}{
						"type":        "string",
						"description": "Date format (rfc3339, date, timestamp, or custom Go format)",
						"default":     "rfc3339",
					},
					"timezone": map[string]interface{}{
						"type":        "string",
						"description": "Timezone (e.g., 'UTC', 'Local', 'America/New_York')",
						"default":     "Local",
					},
				},
			},
		},
		{
			"name":        "get_test_results",
			"description": "Get test results for a specific build. Returns test names, status, duration, and optionally error details/stack traces for failed tests. Use includeDetails=true to get full error messages and stack traces for debugging test failures.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"buildId": map[string]interface{}{
						"type":        "string",
						"description": "Build ID to get test results for (required). Example: '19333979'",
					},
					"status": map[string]interface{}{
						"type":        "string",
						"description": "Filter by test status (optional). Use 'FAILURE' to see only failed tests.",
						"enum":        []string{"SUCCESS", "FAILURE", "UNKNOWN", "IGNORED"},
					},
					"includeDetails": map[string]interface{}{
						"type":        "boolean",
						"description": "Include detailed error messages and stack traces for failed tests (optional, default: false). Set to true when investigating test failures.",
						"default":     false,
					},
					"count": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of tests to return (optional, default: 100, max: 1000)",
						"minimum":     1,
						"maximum":     1000,
						"default":     100,
					},
				},
				"required": []string{"buildId"},
			},
		},
	}

	return h.successResponse(id, map[string]interface{}{
		"tools": tools,
	}), nil
}

// handleToolsCall handles tools/call requests
func (h *Handler) handleToolsCall(ctx context.Context, id interface{}, params json.RawMessage) (interface{}, error) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}

	if err := json.Unmarshal(params, &req); err != nil {
		return h.errorResponse(id, -32602, "Invalid params", nil), nil
	}

	result, err := h.callTool(ctx, req.Name, req.Arguments)
	if err != nil {
		// A missing or rejected token is a client configuration problem, so it
		// gets its own actionable error rather than looking like a server fault.
		if credErr := h.credentialError(id, err); credErr != nil {
			h.logger.Info("Tool call rejected: TeamCity credentials unusable",
				"tool", req.Name, "error", err.Error())
			return credErr, nil
		}
		h.logger.Error("Tool execution failed", "tool", req.Name, "error", err.Error())
		return h.errorResponse(id, -32603, "Tool execution failed", err.Error()), nil
	}

	return h.successResponse(id, map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": result,
			},
		},
	}), nil
}

// handlePing handles ping requests
func (h *Handler) handlePing(id interface{}) (interface{}, error) {
	return h.successResponse(id, map[string]interface{}{}), nil
}

// successResponse creates a JSON-RPC success response
func (h *Handler) successResponse(id interface{}, result interface{}) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
}

// errorResponse creates a JSON-RPC error response
func (h *Handler) errorResponse(id interface{}, code int, message string, data interface{}) map[string]interface{} {
	error := map[string]interface{}{
		"code":    code,
		"message": message,
	}
	if data != nil {
		error["data"] = data
	}

	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   error,
	}
}

// listResources lists available resources
func (h *Handler) listResources(ctx context.Context, uri string) ([]interface{}, error) {
	// When uri is empty, return the list of available resource types (not the actual data)
	if uri == "" {
		return []interface{}{
			map[string]interface{}{
				"uri":         "teamcity://projects",
				"name":        "Projects",
				"description": "TeamCity projects",
				"mimeType":    "application/json",
			},
			map[string]interface{}{
				"uri":         "teamcity://buildTypes",
				"name":        "Build Types",
				"description": "TeamCity build configurations",
				"mimeType":    "application/json",
			},
			map[string]interface{}{
				"uri":         "teamcity://builds",
				"name":        "Builds",
				"description": "Recent TeamCity builds",
				"mimeType":    "application/json",
			},
			map[string]interface{}{
				"uri":         "teamcity://agents",
				"name":        "Agents",
				"description": "TeamCity build agents",
				"mimeType":    "application/json",
			},
			map[string]interface{}{
				"uri":         "teamcity://runtime",
				"name":        "Runtime Information",
				"description": "Current server date, time, and runtime information",
				"mimeType":    "application/json",
			},
		}, nil
	}

	// teamcity://runtime is served locally and needs no credentials.
	if uri == "teamcity://runtime" {
		return h.listRuntimeInfo(ctx)
	}

	// Everything below talks to TeamCity, so resolve credentials now.
	tc, err := h.client(ctx)
	if err != nil {
		return nil, err
	}

	switch uri {
	case "teamcity://projects":
		return tc.ListProjects(ctx)
	case "teamcity://buildTypes":
		return tc.ListBuildTypes(ctx)
	case "teamcity://builds":
		return tc.ListBuilds(ctx)
	case "teamcity://agents":
		return tc.ListAgents(ctx)
	default:
		return nil, fmt.Errorf("unsupported resource URI: %s", uri)
	}
}

// readResource reads a specific resource
func (h *Handler) readResource(ctx context.Context, uri string) (interface{}, error) {
	// Handle runtime resource separately - no credentials needed
	if uri == "teamcity://runtime" {
		return h.getRuntimeInfo(ctx)
	}

	tc, err := h.client(ctx)
	if err != nil {
		return nil, err
	}

	// Parse URI and delegate to appropriate handler
	return tc.GetResource(ctx, uri)
}

// callTool executes a tool
func (h *Handler) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	// get_current_time is served locally, so it must work without credentials.
	if name == "get_current_time" {
		return h.getCurrentTime(ctx, args)
	}

	tc, err := h.client(ctx)
	if err != nil {
		return "", err
	}

	switch name {
	case "trigger_build":
		return tc.TriggerBuild(ctx, args)
	case "cancel_build":
		return tc.CancelBuild(ctx, args)
	case "pin_build":
		return tc.PinBuild(ctx, args)
	case "set_build_tag":
		return tc.SetBuildTag(ctx, args)
	case "download_artifact":
		return tc.DownloadArtifact(ctx, args)
	case "search_builds":
		return tc.SearchBuilds(ctx, args)
	case "fetch_build_log":
		return tc.FetchBuildLog(ctx, args)
	case "search_build_configurations":
		return tc.SearchBuildConfigurations(ctx, args)
	case "get_test_results":
		return tc.GetTestResults(ctx, args)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// listRuntimeInfo lists runtime information resources
func (h *Handler) listRuntimeInfo(ctx context.Context) ([]interface{}, error) {
	return []interface{}{
		map[string]interface{}{
			"uri":         "teamcity://runtime",
			"name":        "Runtime Information",
			"description": "Current server date, time, and runtime information",
			"mimeType":    "application/json",
		},
	}, nil
}

// getRuntimeInfo returns current runtime information
func (h *Handler) getRuntimeInfo(ctx context.Context) (interface{}, error) {
	currentTime := time.Now()

	return map[string]interface{}{
		"type":             "runtime-info",
		"currentTime":      currentTime.Format(time.RFC3339),
		"currentDate":      currentTime.Format("2006-01-02"),
		"currentTimestamp": currentTime.Unix(),
		"timezone":         currentTime.Location().String(),
		"utcTime":          currentTime.UTC().Format(time.RFC3339),
		"utcDate":          currentTime.UTC().Format("2006-01-02"),
		"utcTimestamp":     currentTime.UTC().Unix(),
		"serverInfo": map[string]interface{}{
			"name":    "teamcity-mcp",
			"version": "1.0.0",
		},
		"note": "This is the REAL current date and time. Do not use any training data dates. Use this information for all time-based queries and operations.",
	}, nil
}

// getCurrentTime tool implementation
func (h *Handler) getCurrentTime(ctx context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Format   string `json:"format"`
		Timezone string `json:"timezone"`
	}

	// Set defaults
	req.Format = "rfc3339"
	req.Timezone = "Local"

	if len(args) > 0 {
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}

	currentTime := time.Now()

	// Handle timezone
	if req.Timezone != "Local" {
		if req.Timezone == "UTC" {
			currentTime = currentTime.UTC()
		} else {
			loc, err := time.LoadLocation(req.Timezone)
			if err != nil {
				return "", fmt.Errorf("invalid timezone '%s': %w", req.Timezone, err)
			}
			currentTime = currentTime.In(loc)
		}
	}

	var result string

	// Handle format
	switch req.Format {
	case "rfc3339":
		result = currentTime.Format(time.RFC3339)
	case "date":
		result = currentTime.Format("2006-01-02")
	case "timestamp":
		result = fmt.Sprintf("%d", currentTime.Unix())
	default:
		// Treat as custom Go time format
		result = currentTime.Format(req.Format)
	}

	return fmt.Sprintf("Current time: %s\nTimezone: %s\nNote: This is the REAL current date/time. Use this for all time-based operations instead of any training data dates.",
		result, currentTime.Location().String()), nil
}
