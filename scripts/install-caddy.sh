#!/usr/bin/env bash
#
# Install Caddy as the shared HTTPS terminator for services on this host.
#
#   sudo ./scripts/install-caddy.sh
#   sudo SITE=spark-01.example.com ./scripts/install-caddy.sh
#
# Caddy issues certificates from its own internal CA, because this host has no
# publicly-trusted or corporate certificate available: the DNS name resolves only
# inside the network and there is no inbound internet, so every ACME challenge
# type is unreachable.
#
# Clients must trust the exported root once - see scripts/export-ca.sh.

# Re-exec under bash when invoked as `sh script.sh`: /bin/sh is dash here, which
# has no `set -o pipefail` and would abort on the next line.
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"

set -euo pipefail

CADDYFILE_DEST="/etc/caddy/Caddyfile"
CONF_D="/etc/caddy/conf.d"
LOG_DIR="/var/log/caddy"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CADDYFILE_SRC="$REPO_ROOT/deploy/caddy/Caddyfile"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info() { echo -e "${BLUE}==>${NC} $*"; }
ok()   { echo -e "${GREEN}  ok${NC} $*"; }
warn() { echo -e "${YELLOW}  !${NC} $*"; }
die()  { echo -e "${RED}error:${NC} $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root (use sudo)"
[ -f "$CADDYFILE_SRC" ] || die "missing $CADDYFILE_SRC"

# --- package ---------------------------------------------------------------
if ! command -v caddy >/dev/null; then
    info "Adding the official Caddy repository"
    command -v curl >/dev/null || die "curl is required"
    install -d -m 0755 /usr/share/keyrings
    curl -1sSLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
        | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sSLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
        > /etc/apt/sources.list.d/caddy-stable.list

    # Ubuntu ships an old caddy (2.6.2) via the Pro/ESM apps repo at a HIGHER
    # apt priority than third-party repos, so without this pin `apt install
    # caddy` silently installs the ESM version - and only works at all if the
    # host has an active Ubuntu Pro subscription.
    cat > /etc/apt/preferences.d/caddy-official <<'EOF'
Package: caddy
Pin: origin dl.cloudsmith.io
Pin-Priority: 1001
EOF

    info "Installing Caddy"
    DEBIAN_FRONTEND=noninteractive apt-get -q update
    DEBIAN_FRONTEND=noninteractive apt-get -q install -y caddy
fi
ok "$(caddy version | head -1)"

# --- log directory ---------------------------------------------------------
# Caddy runs as the unprivileged 'caddy' user, so both the directory and the
# log file must belong to it. Note that running `sudo caddy validate` against a
# config with a file logger CREATES the log file as root, after which the
# service cannot open it and fails to start - so ownership is enforced here
# every run, not just on first install.
info "Preparing $LOG_DIR"
install -d -m 0750 -o caddy -g caddy "$LOG_DIR"
[ -e "$LOG_DIR/access.log" ] && chown caddy:caddy "$LOG_DIR/access.log"
ok "owned by caddy"

# --- configuration ---------------------------------------------------------
info "Installing $CADDYFILE_DEST"
if [ -f "$CADDYFILE_DEST" ] && ! cmp -s "$CADDYFILE_SRC" "$CADDYFILE_DEST"; then
    backup="$CADDYFILE_DEST.$(date +%Y%m%d%H%M%S).bak"
    cp -a "$CADDYFILE_DEST" "$backup"
    warn "existing config backed up to $backup"
fi
install -m 0644 -o root -g root "$CADDYFILE_SRC" "$CADDYFILE_DEST"

if [ -n "${SITE:-}" ]; then
    sed -i "s|^spark-01\.lgs-net\.com|$SITE|" "$CADDYFILE_DEST"
    ok "site set to $SITE"
fi

site=$(grep -m1 -E '^[a-z0-9.-]+ \{' "$CADDYFILE_DEST" | awk '{print $1}')
[ -n "$site" ] || die "could not determine the site name from $CADDYFILE_DEST"

# Other services on this host register their own routing by dropping a snippet
# here. This script creates the directory but NEVER writes to or removes
# anything inside it - those files belong to other packages and owners.
install -d -m 0755 -o root -g root "$CONF_D"
snippets=$(find "$CONF_D" -maxdepth 1 -name '*.caddy' -type f 2>/dev/null | sort)
if [ -n "$snippets" ]; then
    ok "found $(echo "$snippets" | wc -l) foreign snippet(s) in $CONF_D:"
    echo "$snippets" | sed 's|^|       |'
else
    ok "$CONF_D (empty - no other services registered)"
fi

# Validate as the caddy user so this check cannot create root-owned state.
info "Validating configuration"
sudo -u caddy caddy validate --config "$CADDYFILE_DEST" 2>&1 | grep -qi 'valid configuration' \
    || die "invalid configuration; run: sudo -u caddy caddy validate --config $CADDYFILE_DEST"
ok "valid"

# --- service ---------------------------------------------------------------
info "Enabling and starting Caddy"
systemctl enable caddy >/dev/null 2>&1 || true
systemctl restart caddy
for _ in $(seq 1 30); do
    systemctl is-active --quiet caddy && break
    sleep 0.2
done
systemctl is-active --quiet caddy || {
    journalctl -u caddy -n 20 --no-pager || true
    die "Caddy failed to start"
}
ok "running"

# --- verify ----------------------------------------------------------------
info "Verifying HTTPS"
ca=$(ls /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt 2>/dev/null || true)
for _ in $(seq 1 25); do
    [ -f "$ca" ] && break
    sleep 0.2
    ca=$(ls /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt 2>/dev/null || true)
done

if [ -f "$ca" ]; then
    tmp_ca=$(mktemp); cp "$ca" "$tmp_ca"; chmod 644 "$tmp_ca"
    if curl -fsS --cacert "$tmp_ca" --max-time 5 "https://$site/" >/dev/null 2>&1; then
        ok "https://$site/ verified against the internal CA"
    else
        warn "could not verify https://$site/ - check DNS resolves here and that a backend is up"
    fi
    rm -f "$tmp_ca"
else
    warn "internal CA root not found yet; it is created on the first TLS handshake"
fi

if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
    ufw allow 443/tcp >/dev/null 2>&1 && ok "opened 443/tcp in ufw"
fi

echo -e "\n${GREEN}Done.${NC} Caddy is serving https://$site/ and is enabled at boot."
cat <<EOF

  Reload    sudo systemctl reload caddy
  Logs      sudo journalctl -u caddy -f
  Access    sudo tail -f $LOG_DIR/access.log
  Config    sudo \${EDITOR:-nano} $CADDYFILE_DEST

  Next: give each client machine the internal CA root, once:
      ./scripts/export-ca.sh
EOF
