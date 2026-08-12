# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added — Multi-tenant credentials (breaking)

The TeamCity API token is now supplied by the client, per request, instead of
being configured on the server. One deployment can serve many users, each acting
as themselves in TeamCity with their own permissions and audit trail.

- **Per-request credentials on HTTP/WebSocket**: clients send
  `X-TeamCity-Token: <their own token>`. An optional `X-TeamCity-Url` selects a
  different TeamCity server, but only one permitted by `TC_URL`/`TC_ALLOWED_URLS`.
- **New JSON-RPC error codes**: `-32001` (no TeamCity token; the message names the
  required header) and `-32002` (TeamCity URL not permitted). `initialize`,
  `tools/list`, `ping` and `get_current_time` deliberately work without
  credentials so a client can connect and discover tools before it has a token.
- **URL pinning** prevents `X-TeamCity-Url` from being used to reach arbitrary
  hosts. New: `TC_ALLOWED_URLS`, `TC_ALLOW_ANY_URL` (unsafe, off by default).
  Cross-host redirects are refused.
- **New env vars**: `TC_ALLOWED_URLS`, `TC_ALLOW_ANY_URL`,
  `TC_ALLOW_SERVER_TOKEN_FALLBACK`, `TC_MAX_RESPONSE_BYTES`,
  `TC_MAX_IDLE_CONNS_PER_HOST`, `ALLOWED_ORIGINS`.
- **Response size cap** (`TC_MAX_RESPONSE_BYTES`, default 32 MiB): build logs are
  unbounded, so an uncapped read was an easy way to OOM a shared server.
- Tests for per-tenant isolation, cookie non-replay, URL pinning, redirect
  policy, WebSocket credential binding, and the previously untested
  `internal/config` package.

### Changed (breaking)

- **`TC_URL` and `TC_TOKEN` are now optional.** The server no longer refuses to
  start without them. `TC_TOKEN` is required only for `--transport stdio`, and on
  HTTP it is used solely for the `/readyz` deep probe — it is **not** a fallback
  for requests that arrive without a token unless
  `TC_ALLOW_SERVER_TOKEN_FALLBACK=true` is set explicitly.
- **`/readyz`** returns 200 with the TeamCity check reported as `"skipped"` when
  no server-side credentials are configured, and ignores TeamCity headers
  entirely (it is unauthenticated, so honouring them would make it an SSRF probe).
- **`mcp.NewHandler` signature**: now `NewHandler(clients ClientResolver, logger)`
  — the `*cache.Cache` parameter is gone and the client is resolved per request.
- **WebSocket** credentials are captured at upgrade time and bound to the
  connection; upgrades carrying an `Origin` header now require `ALLOWED_ORIGINS`.
- `SERVER_SECRET`'s HMAC gate is unchanged and remains optional, but `401` now
  means only that this gate rejected the request — never a missing TeamCity token.
- TeamCity 401/403 responses report that the supplied token was rejected, without
  echoing the token.

### Fixed

- `FetchBuildLog` built its own request with a second, separate copy of the auth
  logic; both auth sites now go through one request builder, so it cannot use
  stale credentials.
- `SIGHUP` reload never rebuilt the TeamCity client, so a changed `TC_URL` was
  silently ignored; the config swap was also racy (written under a mutex, read
  without one). Config is now an atomic pointer and the policy is rebuilt.
- The MCP request metrics `status` label was hardcoded to `"success"`, hiding
  every failure.
- `authMiddleware` used a prefix match for its exempt paths, so `/readyzzz` and
  `/metricsfoo` also bypassed authentication. Now matched exactly.
- The `cache` package was constructed and injected but never read. It is left
  deliberately unwired: with per-user credentials, an unnamespaced shared cache
  would serve one user's data to another.
- `.gitignore` had a bare `server` entry, intended for the convenience binary at
  the repo root, which also matched the `internal/server/` package directory —
  silently excluding any new file added there. Anchored to `/server`.
- `docker-compose.yml` pointed `TC_URL` at `http://teamcity:8111`, which does not
  resolve — the service is named `teamcity-server`.
- `scripts/verify.sh` sent the raw `SERVER_SECRET` as the bearer token where the
  code expects its HMAC digest, so its MCP tests could not pass with the gate
  enabled. It also printed secrets in its summary.
- The integration suite was self-contradictory: `TestAuthenticationRequired`
  expected 401 while every other test sent a token that could never be valid.
  Auth tests are now skipped when the gate is disabled.

### Migration

- **stdio users**: no change. Keep setting `TC_URL` and `TC_TOKEN`.
- **HTTP users**: remove `TC_TOKEN` from the server, keep `TC_URL`, and have each
  client send `X-TeamCity-Token`. To keep the old single-identity behavior
  temporarily, set `TC_ALLOW_SERVER_TOKEN_FALLBACK=true` — but note that this lets
  any caller act as whoever owns `TC_TOKEN`.

### Added
- **Runtime Date/Time Support**: Added comprehensive current date/time functionality to prevent AI models from using training data dates
  - New `teamcity://runtime` resource providing current server date, time, and timezone information
  - New `get_current_time` tool with flexible formatting and timezone support
  - Current time information included in server initialization response
  - Support for multiple date formats: RFC3339, date-only, timestamp, and custom Go formats
  - Support for multiple timezones including UTC, Local, and IANA timezone names
  - Comprehensive documentation with examples in `docs/RUNTIME_DATE_EXAMPLES.md`
  - Unit tests covering all new functionality

### Changed
- Updated Protocol.md with documentation for new runtime resource and get_current_time tool
- Updated README.md to include new tool in the count (9 tools total) and usage examples
- Enhanced server initialization to include current time information in serverInfo

### Technical Details
- Added `listRuntimeInfo()` and `getRuntimeInfo()` methods to MCP handler
- Added `getCurrentTime()` tool implementation with timezone and format support
- Enhanced `handleInitialize()` to include current time in serverInfo
- Added comprehensive test suite in `tests/unit/runtime_test.go`

## [1.0.0] - Previous Release

### Added
- Initial release of TeamCity MCP Server
- Full MCP protocol support with JSON-RPC 2.0
- 8 powerful tools for TeamCity management:
  - `trigger_build` - Trigger new builds
  - `cancel_build` - Cancel running builds  
  - `pin_build` - Pin/unpin builds
  - `set_build_tag` - Add/remove build tags
  - `download_artifact` - Download build artifacts
  - `search_builds` - Advanced build search with filters
  - `fetch_build_log` - Get build logs (plain text or archived)
  - `search_build_configurations` - Search build configurations with detailed filters
- Resource access for projects, build types, builds, and agents
- HTTP/WebSocket and STDIO transport support
- HMAC authentication support
- Production-ready features:
  - Docker and Kubernetes deployment
  - Prometheus metrics
  - Health checks
  - Structured logging
  - Caching with configurable TTL
  - TLS support
- Comprehensive documentation and examples
- Full test coverage
- CI/CD pipeline with GitHub Actions