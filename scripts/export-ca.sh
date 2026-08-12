#!/usr/bin/env bash
#
# Export Caddy's internal CA root so client machines can trust this host.
#
#   ./scripts/export-ca.sh                  # write ./caddy-root.crt
#   ./scripts/export-ca.sh /tmp/root.crt    # write somewhere specific
#
# One install per client machine covers every service behind this host's HTTPS
# terminator, now and in future.

# Re-exec under bash when invoked as `sh script.sh`: /bin/sh is dash here, which
# has no `set -o pipefail` and would abort on the next line.
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"

set -euo pipefail

CA_SRC="/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt"
DEST="${1:-./caddy-root.crt}"

GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
die() { echo -e "${RED}error:${NC} $*" >&2; exit 1; }

# The file is readable only by the caddy user, so reading it needs sudo.
if [ -r "$CA_SRC" ]; then
    cp "$CA_SRC" "$DEST"
else
    command -v sudo >/dev/null || die "cannot read $CA_SRC and sudo is unavailable"
    sudo cat "$CA_SRC" > "$DEST" 2>/dev/null || die "cannot read $CA_SRC - is Caddy installed and has it served a request yet?"
fi
chmod 644 "$DEST"

subject=$(openssl x509 -in "$DEST" -noout -subject 2>/dev/null | sed 's/^subject=//')
expires=$(openssl x509 -in "$DEST" -noout -enddate 2>/dev/null | cut -d= -f2)
fingerprint=$(openssl x509 -in "$DEST" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2)
site=$(grep -m1 -E '^[a-z0-9.-]+ \{' /etc/caddy/Caddyfile 2>/dev/null | awk '{print $1}')
site="${site:-THIS_HOST}"

echo -e "${GREEN}Exported${NC} $DEST"
cat <<EOF

  Subject      $subject
  Expires      $expires
  SHA-256      $fingerprint

Copy that file to each client machine and trust it. Verify the fingerprint above
matches on arrival - it is what makes the transfer tamper-evident.

--- Claude Code / any Node-based MCP client -------------------------------
IMPORTANT: Node does NOT consult the operating system trust store, so installing
the root system-wide is not sufficient on its own. Without NODE_EXTRA_CA_CERTS the
client fails with:

    UNABLE_TO_GET_ISSUER_CERT_LOCALLY: unable to get local issuer certificate

For Claude Code specifically, the durable fix is settings.json, which applies to
every session without touching a shell profile. In ~/.claude/settings.json:

    {
      "env": {
        "NODE_EXTRA_CA_CERTS": "/absolute/path/to/caddy-root.crt"
      }
    }

Or per-shell, which only affects clients launched from that shell:

    export NODE_EXTRA_CA_CERTS=/absolute/path/to/caddy-root.crt

Then register the server:

    claude mcp add --transport http teamcity https://$site/teamcity/mcp \\
      --header "X-TeamCity-Token: YOUR_TEAMCITY_TOKEN"

--- Whole machine, Debian/Ubuntu ------------------------------------------
Covers curl, git and other OpenSSL-based tools. Node still needs the variable above.

    sudo cp caddy-root.crt /usr/local/share/ca-certificates/caddy-root.crt
    sudo update-ca-certificates

--- Whole machine, macOS --------------------------------------------------
    sudo security add-trusted-cert -d -r trustRoot \\
      -k /Library/Keychains/System.keychain caddy-root.crt

--- Whole machine, Windows (admin PowerShell) -----------------------------
    Import-Certificate -FilePath caddy-root.crt \\
      -CertStoreLocation Cert:\\LocalMachine\\Root

--- Verify from the client ------------------------------------------------
    curl --cacert caddy-root.crt https://$site/teamcity/healthz
EOF
