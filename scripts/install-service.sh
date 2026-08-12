#!/usr/bin/env bash
#
# Install teamcity-mcp as a systemd service that starts at boot.
#
#   sudo ./scripts/install-service.sh                       # install / upgrade
#   sudo TC_URL=https://tc.example.com ./scripts/install-service.sh
#
# Idempotent: re-running upgrades the binary and unit file but never overwrites
# an existing /etc/teamcity-mcp/teamcity-mcp.env.

set -euo pipefail

SERVICE_NAME="teamcity-mcp"
BIN_SRC_DEFAULT="bin/teamcity-mcp"
BIN_DEST="/usr/local/bin/teamcity-mcp"
CONF_DIR="/etc/teamcity-mcp"
CONF_FILE="$CONF_DIR/teamcity-mcp.env"
UNIT_DEST="/etc/systemd/system/$SERVICE_NAME.service"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNIT_SRC="$REPO_ROOT/deploy/systemd/$SERVICE_NAME.service"
CONF_SRC="$REPO_ROOT/deploy/systemd/$SERVICE_NAME.env.example"
BIN_SRC="${BIN_SRC:-$REPO_ROOT/$BIN_SRC_DEFAULT}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info() { echo -e "${BLUE}==>${NC} $*"; }
ok()   { echo -e "${GREEN}  ok${NC} $*"; }
warn() { echo -e "${YELLOW}  !${NC} $*"; }
die()  { echo -e "${RED}error:${NC} $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root (use sudo)"
command -v systemctl >/dev/null || die "systemd is required"
[ -f "$UNIT_SRC" ] || die "missing unit file: $UNIT_SRC"
[ -f "$CONF_SRC" ] || die "missing env example: $CONF_SRC"

# --- binary ----------------------------------------------------------------
if [ ! -x "$BIN_SRC" ]; then
    cat >&2 <<EOF
error: no binary at $BIN_SRC

Build it first:
    make build

There is no Go toolchain on some hosts; to build via Docker instead:
    docker run --rm -v "\$PWD":/src -w /src -e GOFLAGS=-buildvcs=false \\
      golang:1.23 go build -o bin/teamcity-mcp ./cmd/server

Or point this script at an existing binary:
    sudo BIN_SRC=/path/to/teamcity-mcp $0
EOF
    exit 1
fi

# Refuse a binary built for the wrong architecture - it would install cleanly and
# then fail at every start with an unhelpful Exec format error.
host_arch=$(uname -m)
if command -v file >/dev/null; then
    bin_desc=$(file -b "$BIN_SRC")
    case "$host_arch:$bin_desc" in
        aarch64:*aarch64*|aarch64:*ARM\ aarch64*) ;;
        x86_64:*x86-64*)                          ;;
        armv7l:*ARM,*)                            ;;
        *) die "binary architecture does not match this host ($host_arch): $bin_desc" ;;
    esac
fi

info "Installing binary"
install -m 0755 -o root -g root "$BIN_SRC" "$BIN_DEST"
ok "$BIN_DEST ($("$BIN_DEST" -version 2>/dev/null | head -1))"

# --- configuration ---------------------------------------------------------
info "Configuring $CONF_DIR"
install -d -m 0750 -o root -g root "$CONF_DIR"

if [ -f "$CONF_FILE" ]; then
    ok "kept existing $CONF_FILE (not overwritten)"
else
    install -m 0600 -o root -g root "$CONF_SRC" "$CONF_FILE"
    if [ -n "${TC_URL:-}" ]; then
        # Replace the placeholder with the URL supplied in the environment.
        sed -i "s|^TC_URL=.*|TC_URL=${TC_URL}|" "$CONF_FILE"
        ok "created $CONF_FILE with TC_URL=${TC_URL}"
    else
        warn "created $CONF_FILE from the example - edit TC_URL before starting"
    fi
fi

# Fail early rather than starting a service that cannot serve anything.
conf_url=$(grep -E '^TC_URL=' "$CONF_FILE" | head -1 | cut -d= -f2-)
if [ -z "$conf_url" ] || [ "$conf_url" = "https://teamcity.example.com" ]; then
    die "TC_URL in $CONF_FILE is unset or still the placeholder.
       Edit it, or re-run: sudo TC_URL=https://your-teamcity $0"
fi

# --- unit ------------------------------------------------------------------
info "Installing systemd unit"
install -m 0644 -o root -g root "$UNIT_SRC" "$UNIT_DEST"
systemctl daemon-reload
ok "$UNIT_DEST"

# 'enable' is what makes it survive a reboot; --now also starts it immediately.
info "Enabling and starting $SERVICE_NAME"
systemctl enable --now "$SERVICE_NAME" >/dev/null 2>&1 || {
    systemctl --no-pager --full status "$SERVICE_NAME" || true
    die "failed to start; see: journalctl -u $SERVICE_NAME -n 50"
}

# Give it a moment to bind, then confirm it is genuinely serving.
for _ in $(seq 1 25); do
    systemctl is-active --quiet "$SERVICE_NAME" || break
    listen_addr=$(grep -E '^LISTEN_ADDR=' "$CONF_FILE" | head -1 | cut -d= -f2-)
    probe_host=${listen_addr%:*}; probe_port=${listen_addr##*:}
    [ -z "$probe_port" ] && probe_port=8123
    case "$probe_host" in ""|0.0.0.0|"[::]") probe_host=127.0.0.1 ;; esac
    if curl -fsS --max-time 2 "http://$probe_host:$probe_port/healthz" >/dev/null 2>&1; then
        ok "responding on http://$probe_host:$probe_port"
        break
    fi
    sleep 0.2
done

echo
if systemctl is-active --quiet "$SERVICE_NAME" && systemctl is-enabled --quiet "$SERVICE_NAME"; then
    echo -e "${GREEN}Installed.${NC} $SERVICE_NAME is running and enabled at boot."
else
    die "service is not active/enabled; see: journalctl -u $SERVICE_NAME -n 50"
fi

cat <<EOF

  Status   sudo systemctl status $SERVICE_NAME
  Logs     sudo journalctl -u $SERVICE_NAME -f
  Config   sudo \${EDITOR:-nano} $CONF_FILE
  Reload   sudo systemctl reload $SERVICE_NAME    # TC_URL / TC_ALLOWED_URLS
  Restart  sudo systemctl restart $SERVICE_NAME   # anything else

  Clients send their own TeamCity token on every request:
    X-TeamCity-Token: <their own TeamCity API token>
EOF
