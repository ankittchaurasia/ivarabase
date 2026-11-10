# Behind a reverse proxy

Most servers already have something on ports 80 and 443: nginx, Caddy, Traefik,
a hosting panel or another Docker proxy. IvaraBase doesn't need those ports. In **proxy
mode** it listens on `127.0.0.1:8787` only, and your proxy forwards to it:

```text
browser ──https──► nginx / Caddy / Traefik (:443, wildcard certificate)
                     │  base.example.com      → panel, API, MCP
                     │  *.base.example.com    → projects (supabase-js URL)
                     ▼
                   IvaraBase  http://127.0.0.1:8787  (routes by the Host header)

app / psql ──TLS──► db.base.example.com:54329  (Postgres, direct - not through nginx)
```

Everything works as with built-in HTTPS: project subdomains, OAuth and Apple
callbacks, email links, secure cookies, per-visitor rate limits, Postgres over TLS.

## 1. DNS

| Record | Points to |
|---|---|
| `A base.example.com` | the server's IP |
| `A *.base.example.com` | the server's IP |

`db.base.example.com` is covered by the wildcard.

## 2. Install IvaraBase in proxy mode

```sh
sudo bash install-deps-ubuntu.sh       # PostgreSQL 18, PostgREST, Deno; leaves any existing Postgres alone
sudo install -m 0755 ivarabase-linux-amd64 /usr/local/bin/ivarabase
sudo ivarabase config set postgres.listen_addresses '*' --data /var/lib/ivarabase   # only if apps connect to Postgres directly
C=/etc/letsencrypt/live/base.example.com     # the proxy's certificate folder
sudo ivarabase service install --domain base.example.com --proxy --cert-file $C/fullchain.pem --key-file $C/privkey.pem
sudo cat /var/lib/ivarabase/secrets/INITIAL_CREDENTIALS.txt   # login + RECOVERY KEY, then delete the file
```

The login and recovery key go only into that file (owner-only), never into the
service log.

`--proxy` sets `tls.mode: proxy`. In this mode IvaraBase:

- never binds 80/443; it listens on `127.0.0.1:8787`, which only the proxy can reach;
- publishes `https://base.example.com` and `https://<project>.base.example.com` as its URLs;
- believes `X-Forwarded-For` / `X-Real-IP` / `X-Forwarded-Proto` **only from loopback**
  (`trusted_proxies`), so visitors can't fake their IP to dodge rate limits.

Its own Postgres runs on port 54329. Your existing PostgreSQL or MySQL keeps running.

## 3. The proxy

Print a config with your domain already filled in:

```sh
ivarabase proxy-config --data /var/lib/ivarabase                 # nginx
ivarabase proxy-config --data /var/lib/ivarabase --server caddy  # Caddy
```

(Agents can call the `system.proxy_config` operation instead.)

### Plain nginx

Use the full `server { }` blocks from `ivarabase proxy-config`, put in your
certificate paths, then run `nginx -t && systemctl reload nginx`.

### Hosting panels

Panels that manage nginx for you (aaPanel, CloudPanel, Plesk and others) work
the same way:

1. Create a **static** site (no PHP) for `base.example.com` and `*.base.example.com`.
2. Add a certificate covering both names. A wildcard needs DNS verification,
   which most panels support through your DNS provider's API.
3. Paste the `location / { … }` block from `ivarabase proxy-config` into the
   site's nginx configuration. Remove any static-file `location` blocks the panel
   added (for `.js`, `.css` or images): they answer before the proxy and break
   the dashboard. Keep the `/.well-known` block, which renews certificates.

If the panel has a "reverse proxy" form instead, target `http://127.0.0.1:8787`,
send the original host (`$host`), and add the `X-Forwarded-Proto`,
`client_max_body_size`, `proxy_request_buffering` and `proxy_buffering` lines
from the table below.

What the block sets, and why:

| Setting | Why |
|---|---|
| `proxy_set_header Host $host` | projects are routed by subdomain |
| `Upgrade` / `Connection` headers | realtime (`/realtime/v1/websocket`, supabase-js channels) is a WebSocket |
| `X-Real-IP`, `X-Forwarded-For`, `X-Forwarded-Proto` | real visitor IP for rate limits and logs, https for secure cookies |
| `client_max_body_size 0` + `proxy_request_buffering off` | IvaraBase enforces its own upload limits; uploads stream straight through |
| `proxy_buffering off` | streamed function responses arrive as they're written |
| `proxy_read_timeout 310s` | functions may run up to 300 s |

### Traefik

Route both names to the IvaraBase listener, for example with Docker labels:

```yaml
labels:
  - traefik.http.routers.ivarabase.rule=Host(`base.example.com`) || HostRegexp(`^[a-z0-9_-]+\.base\.example\.com$`)
  - traefik.http.routers.ivarabase.tls.certresolver=letsencrypt
  - traefik.http.routers.ivarabase.tls.domains[0].main=base.example.com
  - traefik.http.routers.ivarabase.tls.domains[0].sans=*.base.example.com
  - traefik.http.services.ivarabase.loadbalancer.server.port=8787
```

Traefik passes the Host header and WebSockets through by default. The wildcard
certificate needs a DNS challenge resolver. Add Traefik's network to
`trusted_proxies` (`IVARA_TRUSTED_PROXIES`); the Docker image already trusts
private networks.

### Caddy

`ivarabase proxy-config --server caddy` uses on-demand TLS. Caddy gets a certificate
per hostname the first time it's visited. Before that, it asks IvaraBase
(`/_tls/ask`, answered only for local callers) whether the name is the panel,
`db.`, or an existing project. Random names pointed at your server get nothing.
Prefer one wildcard? Use a Caddy DNS plugin instead.

## 4. Postgres over TLS

Postgres speaks its own protocol, so it can't go through nginx. Apps and `psql`
connect directly to `base.example.com:54329` (or `db.base.example.com:54329`).
To encrypt those connections, point IvaraBase at the same certificate the proxy uses:

```sh
C=/etc/letsencrypt/live/base.example.com   # wherever the proxy keeps the certificate
sudo ivarabase service install --domain base.example.com --proxy --cert-file $C/fullchain.pem --key-file $C/privkey.pem
ivarabase config set postgres.listen_addresses '*' --data /var/lib/ivarabase
sudo ivarabase service restart
```

The service runs as the `ivarabase` user, and proxies usually keep certificates
root-only. `service install` notices and leaves their permissions alone. Instead it:

- copies the pair into `/var/lib/ivarabase/tls/` (owned by `ivarabase`, mode 600);
- installs `ivarabase-tls-sync.path`, which copies again whenever the proxy renews
  the files, and `ivarabase-tls-sync.timer`, a daily safety check. Neither copies
  a half-written pair.

Run it by hand with `sudo systemctl start ivarabase-tls-sync`.
`ivarabase service uninstall` removes both units. Certificates the service user
can already read are used where they are, with no copy.

- The certificate is copied into Postgres and re-checked every hour, so renewals
  are picked up without a restart.
- Once a certificate is installed, remote clients are only accepted over TLS.
- Open port 54329 in the server's firewall and in your cloud provider's
  firewall, but only if apps outside the server connect directly.
- `psql "host=db.base.example.com port=54329 … sslmode=verify-full sslrootcert=system"`
  works because the wildcard covers `db.base.example.com`.
- If you skip this step, IvaraBase still runs, but `doctor` and the health page warn
  that remote Postgres traffic is unencrypted.

## 5. Check

```sh
ivarabase doctor --data /var/lib/ivarabase
```

In proxy mode, `doctor` warns when:

- the listen address is reachable without going through the proxy;
- the certificate files can't be read;
- the certificate doesn't cover `db.<domain>`;
- the certificate expires within 14 days.

Then open `https://base.example.com` and sign in.

## Other setups

- **Cloudflare (orange cloud) in front of nginx.** nginx sees Cloudflare's addresses.
  Restore the visitor IP in nginx (`set_real_ip_from` Cloudflare's ranges, plus
  `real_ip_header CF-Connecting-IP`), and IvaraBase gets the right `X-Real-IP`.
- **The proxy on another machine, or in Docker.** Trust its address explicitly. The
  list replaces the loopback default:
  `ivarabase config set trusted_proxies 172.16.0.0/12,127.0.0.1`
  Also set `listen` to an address that proxy can reach.
- **A proxy on a non-standard port.** Set `public_url` to `https://base.example.com:8443`.
  IvaraBase keeps an https `public_url` on your domain as it is.

## Safety notes

- **Uploaded files can't run script.** Files from storage are served with
  `Content-Security-Policy: sandbox`. An HTML or SVG in a public bucket can't run
  script on your domain or reach the dashboard with your session. Images, video,
  PDFs and downloads work normally.
- **The panel origin sandboxes project responses.** Function and project responses
  reached through the panel origin (`/fn/…`, `/proj/…`) are sandboxed the same way.
  To serve HTML from a function, use the project's own subdomain:
  `https://<project>.base.example.com/functions/v1/<name>`.
