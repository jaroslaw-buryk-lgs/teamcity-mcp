#!/usr/bin/env bash
#
# Remove the teamcity-mcp systemd service.
#
#   sudo ./scripts/uninstall-service.sh              # keep configuration
#   sudo ./scripts/uninstall-service.sh --purge      # also remove /etc/teamcity-mcp

# Re-exec under bash when invoked as `sh script.sh`: /bin/sh is dash here, which
# has no `set -o pipefail` and would abort on the next line.
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"

set -euo pipefail

SERVICE_NAME="teamcity-mcp"
BIN_DEST="/usr/local/bin/teamcity-mcp"
CONF_DIR="/etc/teamcity-mcp"
UNIT_DEST="/etc/systemd/system/$SERVICE_NAME.service"

PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info() { echo -e "${BLUE}==>${NC} $*"; }
ok()   { echo -e "${GREEN}  ok${NC} $*"; }
warn() { echo -e "${YELLOW}  !${NC} $*"; }

[ "$(id -u)" -eq 0 ] || { echo "must run as root (use sudo)" >&2; exit 1; }

info "Stopping and disabling $SERVICE_NAME"
systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || warn "was not enabled/running"
ok "stopped"

for path in "$UNIT_DEST" "$BIN_DEST"; do
    if [ -e "$path" ]; then
        rm -f "$path"
        ok "removed $path"
    fi
done

systemctl daemon-reload
systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true

if [ "$PURGE" -eq 1 ]; then
    if [ -d "$CONF_DIR" ]; then
        rm -rf "$CONF_DIR"
        ok "removed $CONF_DIR"
    fi
else
    [ -d "$CONF_DIR" ] && warn "kept $CONF_DIR (use --purge to remove)"
fi

echo -e "\n${GREEN}Uninstalled.${NC}"
