#!/bin/bash

# TeamCity MCP Server Verification Script
# This script tests all major functionality to verify the server is working correctly

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
SERVER_URL="http://localhost:8123"

# Set default environment variables for testing.
#
# TC_URL pins which TeamCity clients may reach; it is not a secret.
# TC_TOKEN is NOT exported: on the HTTP transport each user supplies their own
# token per request, so the server runs without a TeamCity identity. Export it
# only to exercise TeamCity-backed calls.
export TC_URL="${TC_URL:-http://localhost:8111}"
export SERVER_SECRET="${SERVER_SECRET:-test-secret}"  # Optional - enables the outer gate
export LOG_LEVEL="${LOG_LEVEL:-info}"

# Header carrying the caller's TeamCity token (see internal/teamcity).
TC_TOKEN_HEADER="X-TeamCity-Token"
TC_URL_HEADER="X-TeamCity-Url"

# gate_token derives the outer gate's bearer token from SERVER_SECRET.
# The server expects the hex HMAC digest, NOT the raw secret - sending the secret
# itself yields 401.
gate_token() {
    if [ -z "$SERVER_SECRET" ]; then
        return 0
    fi
    printf 'teamcity-mcp' | openssl dgst -sha256 -hmac "$SERVER_SECRET" -r | cut -d' ' -f1
}

# mcp_post sends an MCP request, adding the gate token when one is configured.
# Usage: mcp_post '<json body>' [extra curl args...]
mcp_post() {
    local body=$1; shift
    local token
    token=$(gate_token)

    if [ -n "$token" ]; then
        curl -s -X POST "$SERVER_URL/mcp" \
            -H "Content-Type: application/json" \
            -H "Authorization: Bearer $token" \
            "$@" -d "$body" 2>/dev/null
    else
        curl -s -X POST "$SERVER_URL/mcp" \
            -H "Content-Type: application/json" \
            "$@" -d "$body" 2>/dev/null
    fi
}

# Function to print colored output
print_status() {
    local status=$1
    local message=$2
    case $status in
        "PASS") echo -e "${GREEN}✅ PASS${NC}: $message" ;;
        "FAIL") echo -e "${RED}❌ FAIL${NC}: $message" ;;
        "WARN") echo -e "${YELLOW}⚠️  WARN${NC}: $message" ;;
        "INFO") echo -e "${BLUE}ℹ️  INFO${NC}: $message" ;;
    esac
}

# Function to check if server is running
check_server_running() {
    if pgrep -f "server" > /dev/null; then
        return 0
    else
        return 1
    fi
}

# Function to start server if not running
start_server() {
    if ! check_server_running; then
        print_status "INFO" "Starting server with environment variables..."
        ./server &
        SERVER_PID=$!
        sleep 2
        if check_server_running; then
            print_status "PASS" "Server started successfully"
            return 0
        else
            print_status "FAIL" "Failed to start server"
            return 1
        fi
    else
        print_status "INFO" "Server already running"
        return 0
    fi
}

# Function to stop server
stop_server() {
    if check_server_running; then
        print_status "INFO" "Stopping server..."
        pkill -f "server" || true
        sleep 1
        if ! check_server_running; then
            print_status "PASS" "Server stopped successfully"
        else
            print_status "WARN" "Server may still be running"
        fi
    fi
}

# Test 1: Build verification
test_build() {
    print_status "INFO" "Testing build..."
    if make build > /dev/null 2>&1; then
        print_status "PASS" "Project builds successfully"
    else
        print_status "FAIL" "Build failed"
        return 1
    fi
}

# Test 2: Unit tests
test_units() {
    print_status "INFO" "Running unit tests..."
    if go test ./tests/unit -v > /dev/null 2>&1; then
        print_status "PASS" "Unit tests pass"
    else
        print_status "FAIL" "Unit tests failed"
        return 1
    fi
}

# Test 3: Environment variable help
test_help() {
    print_status "INFO" "Testing help output..."
    if ./server --help > /dev/null 2>&1; then
        print_status "PASS" "Help command works"
    else
        print_status "FAIL" "Help command failed"
        return 1
    fi
}

# Test 4: Health endpoint
test_health() {
    print_status "INFO" "Testing health endpoint..."
    local response
    response=$(curl -s "$SERVER_URL/healthz" 2>/dev/null)
    if echo "$response" | grep -q '"status":"ok"'; then
        print_status "PASS" "Health endpoint responds correctly"
    else
        print_status "FAIL" "Health endpoint failed: $response"
        return 1
    fi
}

# Test 5: Metrics endpoint
test_metrics() {
    print_status "INFO" "Testing metrics endpoint..."
    local response
    response=$(curl -s "$SERVER_URL/metrics" 2>/dev/null)
    if [ -n "$response" ]; then
        print_status "PASS" "Metrics endpoint accessible"
    else
        print_status "FAIL" "Metrics endpoint failed"
        return 1
    fi
}

# Test 6: MCP Initialize - must work with no TeamCity credentials at all
test_mcp_initialize() {
    print_status "INFO" "Testing MCP initialize (no TeamCity token)..."
    local response
    response=$(mcp_post '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}')

    if echo "$response" | grep -q '"protocolVersion":"2024-11-05"'; then
        print_status "PASS" "MCP initialize works without TeamCity credentials"
    else
        print_status "FAIL" "MCP initialize failed: $response"
        return 1
    fi
}

# Test 7: MCP Resources List
test_mcp_resources() {
    print_status "INFO" "Testing MCP resources list..."
    local response
    response=$(mcp_post '{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{}}')

    if echo "$response" | grep -q '"resources"'; then
        print_status "PASS" "MCP resources list works"
    else
        print_status "FAIL" "MCP resources list failed: $response"
        return 1
    fi
}

# Test 8: MCP Tools List - tool discovery must not require credentials
test_mcp_tools() {
    print_status "INFO" "Testing MCP tools list (no TeamCity token)..."
    local response
    response=$(mcp_post '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}')

    if echo "$response" | grep -q '"tools"' && echo "$response" | grep -q 'trigger_build'; then
        print_status "PASS" "MCP tools list works and includes expected tools"
    else
        print_status "FAIL" "MCP tools list failed: $response"
        return 1
    fi
}

# Test 9: Outer authentication gate (SERVER_SECRET)
test_authentication() {
    if [ -z "$SERVER_SECRET" ]; then
        print_status "INFO" "SERVER_SECRET unset - outer gate disabled, skipping"
        return 0
    fi

    print_status "INFO" "Testing the outer authentication gate..."
    local code
    code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$SERVER_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer invalid-token" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' 2>/dev/null)

    if [ "$code" = "401" ]; then
        print_status "PASS" "Outer gate rejects an invalid token (401)"
    else
        print_status "FAIL" "Expected 401 from the outer gate, got $code"
        return 1
    fi

    # The gate expects the HMAC digest, not the raw secret.
    code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$SERVER_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $SERVER_SECRET" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' 2>/dev/null)

    if [ "$code" = "401" ]; then
        print_status "PASS" "The raw secret is correctly not accepted as a token"
    else
        print_status "FAIL" "Expected 401 when sending the raw secret, got $code"
        return 1
    fi
}

# Test 10: A TeamCity-backed call without a token must return -32001
test_missing_teamcity_token() {
    print_status "INFO" "Testing the missing-credentials error..."
    local response
    response=$(mcp_post '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_builds","arguments":{}}}')

    if echo "$response" | grep -q '\-32001' && echo "$response" | grep -q "$TC_TOKEN_HEADER"; then
        print_status "PASS" "Missing TeamCity token returns an actionable -32001 naming the header"
    else
        print_status "FAIL" "Expected -32001 mentioning $TC_TOKEN_HEADER, got: $response"
        return 1
    fi
}

# Test 11: get_current_time needs no TeamCity access
test_local_tool_without_token() {
    print_status "INFO" "Testing get_current_time without credentials..."
    local response
    response=$(mcp_post '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_current_time","arguments":{}}}')

    if echo "$response" | grep -q '"result"'; then
        print_status "PASS" "get_current_time works without TeamCity credentials"
    else
        print_status "FAIL" "get_current_time failed: $response"
        return 1
    fi
}

# Test 12: An unpermitted TeamCity URL must be refused with 400
test_disallowed_teamcity_url() {
    print_status "INFO" "Testing TeamCity URL pinning..."
    local code token
    token=$(gate_token)

    if [ -n "$token" ]; then
        code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$SERVER_URL/mcp" \
            -H "Content-Type: application/json" \
            -H "Authorization: Bearer $token" \
            -H "$TC_TOKEN_HEADER: some-token" \
            -H "$TC_URL_HEADER: http://169.254.169.254/latest/meta-data" \
            -d '{"jsonrpc":"2.0","id":6,"method":"tools/list"}' 2>/dev/null)
    else
        code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$SERVER_URL/mcp" \
            -H "Content-Type: application/json" \
            -H "$TC_TOKEN_HEADER: some-token" \
            -H "$TC_URL_HEADER: http://169.254.169.254/latest/meta-data" \
            -d '{"jsonrpc":"2.0","id":6,"method":"tools/list"}' 2>/dev/null)
    fi

    if [ "$code" = "400" ]; then
        print_status "PASS" "An unpermitted TeamCity URL is refused (400)"
    else
        print_status "FAIL" "Expected 400 for a disallowed TeamCity URL, got $code"
        return 1
    fi
}

# Test 13: /readyz must ignore TeamCity headers - it is unauthenticated
test_readyz_ignores_headers() {
    print_status "INFO" "Testing that /readyz ignores TeamCity headers..."
    local plain with_headers
    plain=$(curl -s -o /dev/null -w "%{http_code}" "$SERVER_URL/readyz" 2>/dev/null)
    with_headers=$(curl -s -o /dev/null -w "%{http_code}" "$SERVER_URL/readyz" \
        -H "$TC_URL_HEADER: http://169.254.169.254" \
        -H "$TC_TOKEN_HEADER: attacker-token" 2>/dev/null)

    if [ "$plain" = "$with_headers" ]; then
        print_status "PASS" "/readyz behaves identically with and without TeamCity headers"
    else
        print_status "FAIL" "/readyz changed behavior with TeamCity headers ($plain vs $with_headers)"
        return 1
    fi
}

# Test 14: Per-request tokens (only meaningful against a real TeamCity)
test_per_request_token() {
    if [ -z "$TC_TOKEN" ]; then
        print_status "INFO" "TC_TOKEN unset - skipping the TeamCity-backed call"
        return 0
    fi

    print_status "INFO" "Testing a TeamCity-backed call with a per-request token..."
    local response
    response=$(mcp_post '{"jsonrpc":"2.0","id":7,"method":"resources/list","params":{"uri":"teamcity://projects"}}' \
        -H "$TC_TOKEN_HEADER: $TC_TOKEN")

    if echo "$response" | grep -q '"resources"'; then
        print_status "PASS" "A per-request TeamCity token is accepted"
    else
        print_status "WARN" "TeamCity-backed call did not succeed (is TeamCity reachable?): $response"
    fi
}

# Main execution
main() {
    echo "=============================================="
    echo "TeamCity MCP Server Verification"
    echo "=============================================="
    echo ""

    local failed_tests=0

    # Check prerequisites
    if [ ! -f "./server" ]; then
        print_status "FAIL" "Server binary not found. Run 'make build' first."
        exit 1
    fi

    # Run tests
    test_build || ((failed_tests++))
    test_units || ((failed_tests++))
    test_help || ((failed_tests++))

    # Start server for integration tests
    if start_server; then
        sleep 2  # Give server time to fully start
        
        test_health || ((failed_tests++))
        test_metrics || ((failed_tests++))
        test_mcp_initialize || ((failed_tests++))
        test_mcp_resources || ((failed_tests++))
        test_mcp_tools || ((failed_tests++))
        test_authentication || ((failed_tests++))
        test_missing_teamcity_token || ((failed_tests++))
        test_local_tool_without_token || ((failed_tests++))
        test_disallowed_teamcity_url || ((failed_tests++))
        test_readyz_ignores_headers || ((failed_tests++))
        test_per_request_token || ((failed_tests++))
        
        # Clean up
        stop_server
    else
        print_status "FAIL" "Could not start server for integration tests"
        ((failed_tests++))
    fi

    echo ""
    echo "=============================================="
    if [ $failed_tests -eq 0 ]; then
        print_status "PASS" "All tests passed! 🎉"
        echo ""
        echo "Your TeamCity MCP server is working correctly!"
        echo ""
        # Secret values are deliberately not printed.
        echo "Configuration used:"
        echo "  TC_URL=$TC_URL"
        echo "  TC_TOKEN=$([ -n "$TC_TOKEN" ] && echo '<set>' || echo '<unset - clients supply their own>')"
        echo "  SERVER_SECRET=$([ -n "$SERVER_SECRET" ] && echo '<set - outer gate enabled>' || echo '<unset - outer gate disabled>')"
        echo ""
        echo "Next steps - to run this as a shared multi-tenant server:"
        echo "1. Point it at your TeamCity (no TeamCity token on the server):"
        echo "   export TC_URL=https://your-teamcity-server.com"
        echo "   export SERVER_SECRET=your-real-secret   # optional outer gate"
        echo "2. Start the server: ./server --transport http"
        echo "3. Have each user send their own token per request:"
        echo "   X-TeamCity-Token: <their TeamCity API token>"
        echo "4. Deploy using Docker or Kubernetes"
        exit 0
    else
        print_status "FAIL" "$failed_tests test(s) failed"
        echo ""
        echo "Please check the failed tests and fix any issues."
        exit 1
    fi
}

# Handle script arguments
case "${1:-}" in
    "help"|"-h"|"--help")
        echo "Usage: $0 [OPTIONS]"
        echo ""
        echo "Options:"
        echo "  help, -h, --help    Show this help message"
        echo "  start               Start the server only"
        echo "  stop                Stop the server only"
        echo "  clean               Clean up any running servers"
        echo ""
        echo "Environment variables (set these for real usage):"
        echo "  TC_URL              TeamCity server URL - pins which server clients may reach"
        echo "  TC_TOKEN            Optional. A TeamCity API token used to exercise"
        echo "                      TeamCity-backed calls. The server does not need one:"
        echo "                      clients send their own via the X-TeamCity-Token header"
        echo "  SERVER_SECRET       Optional outer HMAC gate on Authorization: Bearer"
        echo "  LOG_LEVEL           Log level (default: info)"
        echo ""
        echo "Examples:"
        echo "  $0                  Run all verification tests"
        echo "  TC_URL=https://tc.example.com $0                 Multi-tenant checks only"
        echo "  TC_URL=https://tc.example.com TC_TOKEN=token123 $0   Also hit TeamCity"
        echo "  $0 start            Start server in background"
        exit 0
        ;;
    "start")
        start_server
        print_status "INFO" "Server running in background. Use '$0 stop' to stop it."
        exit 0
        ;;
    "stop")
        stop_server
        exit 0
        ;;
    "clean")
        stop_server
        print_status "INFO" "Cleanup complete"
        exit 0
        ;;
    "")
        main
        ;;
    *)
        print_status "FAIL" "Unknown option: $1"
        echo "Use '$0 help' for usage information."
        exit 1
        ;;
esac 