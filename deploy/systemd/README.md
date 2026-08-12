# Running teamcity-mcp as a systemd service

Runs the server continuously and starts it again after a reboot.

## Install

```bash
make build                                    # or build via Docker, see below
sudo TC_URL=https://teamcity.example.com ./scripts/install-service.sh
```

That installs the binary to `/usr/local/bin/teamcity-mcp`, writes
`/etc/teamcity-mcp/teamcity-mcp.env` from the example, installs the unit, then
enables and starts it. `systemctl enable` is what makes it survive a reboot.

Re-run the script to upgrade: it replaces the binary and unit but never
overwrites your existing env file.

No `TC_TOKEN` is configured. Each user sends their own token per request:

```
X-TeamCity-Token: <their own TeamCity API token>
```

so every caller acts as themselves in TeamCity, with their own permissions and
audit trail.

### No Go toolchain on the host

```bash
docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false \
  golang:1.23 go build -o bin/teamcity-mcp ./cmd/server
sudo ./scripts/install-service.sh
```

The install script refuses a binary built for the wrong architecture — otherwise
it installs cleanly and then fails at every start with `Exec format error`.

## Day to day

```bash
sudo systemctl status teamcity-mcp
sudo journalctl -u teamcity-mcp -f          # logs
sudo systemctl restart teamcity-mcp
sudo systemctl reload teamcity-mcp          # SIGHUP: TC_URL / TC_ALLOWED_URLS only
sudo $EDITOR /etc/teamcity-mcp/teamcity-mcp.env
```

`reload` rebuilds the TeamCity URL policy in place. Changing `LISTEN_ADDR`,
`SERVER_SECRET` or the TLS paths needs a full `restart`.

Convenience targets: `make service-status`, `make service-logs`,
`make install-service`, `make uninstall-service`.

## Verifying it survives a reboot

```bash
systemctl is-enabled teamcity-mcp     # must print: enabled
```

`enabled` means systemd starts it at boot. `active` alone only means it is
running right now. To rehearse without rebooting:

```bash
sudo systemctl stop teamcity-mcp
sudo systemctl start teamcity-mcp
curl -s localhost:8123/healthz
```

## Uninstall

```bash
sudo ./scripts/uninstall-service.sh            # keeps /etc/teamcity-mcp
sudo ./scripts/uninstall-service.sh --purge    # removes it too
```

## How the unit is put together

**`DynamicUser=yes`** — the service gets a transient unprivileged UID with no
home, no shell and no login. It is stateless: it owns no files and holds no
TeamCity credentials of its own, so nothing needs a stable owner.

**`EnvironmentFile`** is read by systemd as root *before* privileges are dropped,
so `/etc/teamcity-mcp/teamcity-mcp.env` stays `0600` root-owned and is
unreadable by the service account itself. Secrets never appear in the unit file,
in `systemctl show`, or in the process environment of anything else.

**No capabilities at all** (`CapabilityBoundingSet=`) — the default port 8123 is
above 1024, so none are needed. Binding a port below 1024 would require adding
`AmbientCapabilities=CAP_NET_BIND_SERVICE`; prefer a reverse proxy instead.

**`RestrictAddressFamilies=AF_INET AF_INET6`** — IP only. No unix sockets, no
netlink.

**`Restart=always` with `StartLimitBurst=0`** — systemd's default gives up after
5 restarts in 10 seconds. That default turns a temporary DNS or TeamCity outage
into a permanently dead service, which is the opposite of what you want here.

**`MemoryMax=512M`** — responses are already capped by `TC_MAX_RESPONSE_BYTES`
(32 MiB default), so this only catches something pathological. Raise it if you
serve many concurrent large build-log fetches.

Inspect the applied sandbox with:

```bash
systemd-analyze security teamcity-mcp
```

## Security notes

The example config serves **plain HTTP**, so TeamCity tokens cross the network in
cleartext. That is only acceptable on a network you fully trust. Two ways to fix
it:

- **Reverse proxy** (see [`deploy/caddy/`](../caddy/README.md)) — set
  `LISTEN_ADDR=127.0.0.1:8123` and let Caddy terminate TLS. This is the setup
  documented in this repo, and it is the right choice when the host runs more
  than one service: one certificate, one open port, one CA for clients to trust.
- **Direct TLS** — set `TLS_CERT` and `TLS_KEY` (TLS 1.3) and skip the proxy. The
  service runs with `ProtectHome=yes` and a dynamic user, so the files must live
  somewhere it can read, such as `/etc/teamcity-mcp/tls/`, and be readable by it.
  Simpler for a single service, but you own certificate renewal.

`SERVER_SECRET` is unset in the example, so anyone who can reach the port may use
the service with their own TeamCity token. They gain no TeamCity access they did
not already have, but set it if the endpoint is reachable from anywhere
untrusted.

`LISTEN_ADDR=0.0.0.0:8123` accepts remote connections. If a firewall is active,
open the port:

```bash
sudo ufw allow 8123/tcp
```

## Connecting from another machine

```bash
claude mcp add --transport http teamcity http://THIS_HOST:8123/mcp \
  --header "X-TeamCity-Token: YOUR_TEAMCITY_TOKEN"
```

The token is stored in plaintext in the client's `~/.claude.json`.
