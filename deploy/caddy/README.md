# HTTPS via Caddy

Terminates TLS for every service on this host behind one hostname, using a
certificate from Caddy's own internal CA.

## Why this setup

The constraints that force it:

- The host's DNS name resolves **only inside the network**, and the host has
  **no inbound internet**. Every public ACME challenge (HTTP-01, TLS-ALPN-01) is
  therefore unreachable, and DNS-01 would need write access to the DNS zone.
- **No publicly-trusted or corporate certificate is obtainable.**
- **No additional DNS names** can be added, so services cannot each get their own
  hostname.

So: one hostname, services distinguished by path, certificates from a CA you run.
Caddy's `tls internal` generates the root and intermediate, issues a leaf per
site, and rotates leaves automatically — there is no certificate to renew by hand
and no `openssl` in your future.

The single cost is that each client machine must trust the root **once**. That one
install covers every service you add behind this terminator later, which is
precisely why a shared terminator beats per-service certificates here.

## Install

```bash
sudo ./scripts/install-caddy.sh
# or, on a different host:
sudo SITE=myhost.example.com ./scripts/install-caddy.sh
```

Then hand the root CA to each client:

```bash
./scripts/export-ca.sh
```

That prints the fingerprint plus per-platform trust instructions. Verify the
fingerprint after transfer — it is what makes the copy tamper-evident.

## Layout

```
                        :443  Caddy (TLS, internal CA)
                          │
   /teamcity/*  ──────────┼──────────►  127.0.0.1:8123   teamcity-mcp
   /other/*     ──────────┘             127.0.0.1:9000   (future service)
```

Backends bind loopback only and are not directly reachable from the network.
`handle_path` strips the prefix, so each backend still sees its own routes
(`/mcp`, `/healthz`) and needs no prefix awareness.

Client URL for the MCP server:

```
https://spark-01.lgs-net.com/teamcity/mcp
```

## Adding a service

Services register their own routing by dropping a snippet into
`/etc/caddy/conf.d/`. Nothing else on the host needs to edit this repo, and no
other service's routing lives here.

```caddyfile
# /etc/caddy/conf.d/grafana.caddy
handle_path /grafana/* {
    reverse_proxy 127.0.0.1:3000
}
```

```bash
sudo -u caddy caddy validate --config /etc/caddy/Caddyfile   # see the warning below
sudo systemctl reload caddy
```

No new certificate and no client-side change — the trusted root already covers
every path behind this terminator.

### The contract for snippet files

| | |
|---|---|
| **Location** | `/etc/caddy/conf.d/<service>.caddy` — the glob is `*.caddy`, so other extensions are ignored |
| **Imported** | **inside** the `spark-01.lgs-net.com { … }` site block |
| **Contents** | site-level directives only, normally one `handle_path` block. Do **not** open a site block or set global options — the import is already inside one |
| **Ordering** | irrelevant. Caddy sorts `handle`/`handle_path` by path specificity, not file order, so the catch-all index below always stays last |
| **Empty dir** | fine. Caddy only warns that the glob matched nothing |
| **Ownership** | `scripts/install-caddy.sh` creates the directory and lists what it finds, but never writes to or removes anything inside it |

Verify ordering after adding a path — a bare `/` should still print the index
while the new prefix reaches its backend:

```bash
curl --cacert caddy-root.crt https://spark-01.lgs-net.com/          # index
curl --cacert caddy-root.crt https://spark-01.lgs-net.com/grafana/  # backend
```

### Shared fate — the one real risk of a shared terminator

`systemctl reload caddy` is safe: Caddy validates the new config first and keeps
the running one if it fails. **Starting** is not. A malformed snippet means Caddy
will not start at all, which takes down *every* service behind the terminator at
the next boot or restart — not just the one whose snippet is broken.

So always validate before reloading, and treat `/etc/caddy/conf.d/` as shared
production config even though each file has a different owner.

### TLS is not authentication

Putting a path behind this terminator encrypts it in transit. It does **not**
authenticate anyone. Each service remains responsible for its own access control,
and the posture genuinely differs per path — for example `/teamcity/` requires the
caller's own TeamCity token and grants exactly that user's permissions, whereas
another path may be entirely unauthenticated by its owner's choice.

Do not read "it's on HTTPS" as "it's secured". If you need a gate at this layer
for a specific path, that is a per-path decision for that service's owner:

```caddyfile
handle_path /example/* {
    @noauth not header Authorization "Bearer {env.EXAMPLE_TOKEN}"
    respond @noauth 401
    reverse_proxy 127.0.0.1:9000
}
```

If any path adopts that, keep `Authorization` in the log-redaction list below.

### Backends that validate the Host header

Some MCP server frameworks — the Python MCP SDK among them — enforce
DNS-rebinding protection by checking the `Host` header and answering
**`421 Invalid Host header`** to anything not on their allowlist, independently of
what address they bind. Caddy forwards the original Host, so such a backend sees
`spark-01.lgs-net.com` (no port, since 443 is implied) and must accept exactly
that value.

The failure mode is misleading in two ways: it looks like the proxy is broken, and
health endpoints typically **bypass** that middleware — so `/health` returns 200
through the proxy while every real call returns 421. Never treat a passing health
probe as evidence that a proxied path works; make a real call.

This does not affect `teamcity-mcp`, which does not validate Host.

## Operating

```bash
sudo systemctl reload caddy          # after a Caddyfile change
sudo journalctl -u caddy -f          # service log
sudo tail -f /var/log/caddy/access.log
sudo -u caddy caddy validate --config /etc/caddy/Caddyfile
```

Validate **as the `caddy` user**. Running `sudo caddy validate` creates
`/var/log/caddy/access.log` owned by root, after which the service cannot open it
and fails to start with `permission denied`. The install script repairs that
ownership on every run, but it is an easy trap to re-set by hand.

## Access-log redaction

Caddy's JSON access log records request headers and redacts only `Authorization`
and `Cookie` by default. Every request here carries the caller's TeamCity API
token in `X-TeamCity-Token`, so the Caddyfile deletes it explicitly:

```caddyfile
format filter {
    wrap json
    fields {
        request>headers>X-Teamcity-Token delete
        request>headers>Authorization delete
    }
}
```

Without that, the access log becomes a file full of live credentials. The header
name is Go-canonicalized in logs — `X-Teamcity-Token`, not `X-TeamCity-Token`.

Verify after any log-config change:

```bash
curl --cacert caddy-root.crt https://spark-01.lgs-net.com/teamcity/healthz \
  -H "X-TeamCity-Token: canary-value-123"
sudo grep -c canary-value-123 /var/log/caddy/access.log   # must be 0
```

## What this does and does not protect

TLS stops eavesdropping and tampering between client and host. It does **not**
control access: `SERVER_SECRET` is unset, so anyone who can reach the port may use
the MCP server with their own TeamCity token. They get exactly their own TeamCity
permissions, so no access leaks — but the endpoint itself is anonymous. Set
`SERVER_SECRET` in `/etc/teamcity-mcp/teamcity-mcp.env` to require a second
credential.

The internal CA root is trusted by clients for **all** names Caddy serves. Keep
`/var/lib/caddy/.local/share/caddy/pki/` as sensitive as any private key: whoever
holds that key can impersonate any host your clients trust it for.

## Migrating off later

If a publicly-trusted or corporate certificate becomes available, replace
`tls internal` with:

```caddyfile
tls /etc/caddy/tls/cert.pem /etc/caddy/tls/key.pem
```

Clients can then drop the private root. Nothing else in the layout changes.
