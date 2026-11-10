# Deploying IvaraBase

One binary, three dependencies (PostgreSQL, PostgREST, Deno), automatic HTTPS.

## 1. DNS

Point three records at the server's public IP:

| Record | Example | Used for |
|---|---|---|
| `A  base.example.com` | panel + API + MCP | `https://base.example.com` |
| `A  *.base.example.com` | every project | `https://<slug>.base.example.com` (supabase-js URL) |
| `A  db.base.example.com` *(optional)* | direct Postgres | `postgresql://…@base.example.com:54329/…?sslmode=require` |

## 2. Server (Ubuntu 22.04 / 24.04)

```sh
# on your machine: build the Linux binary
make linux                       # bin/ivarabase-linux-amd64 and -arm64
scp bin/ivarabase-linux-amd64 scripts/install-deps-ubuntu.sh scripts/deps.env root@SERVER:/root/

# on the server
sudo bash install-deps-ubuntu.sh                 # PostgreSQL 18, PostgREST 16.5, Deno 2.9.8 (checksum-verified)
sudo install -m 0755 ivarabase-linux-amd64 /usr/local/bin/ivarabase
ivarabase doctor --data /var/lib/ivarabase       # checks binaries, DNS, ports
sudo ivarabase service install --domain base.example.com --email you@example.com
sudo cat /var/lib/ivarabase/secrets/INITIAL_CREDENTIALS.txt   # login + RECOVERY KEY
```

Open `https://base.example.com`, sign in, and **store the recovery key in a password
manager** (it decrypts every backup). Then delete `INITIAL_CREDENTIALS.txt`.

`service install`:

- creates an `ivarabase` system user (PostgreSQL refuses to run as root) and `/var/lib/ivarabase`
- copies the binary to `/usr/local/bin/ivarabase`
- writes `/etc/systemd/system/ivarabase.service` (restart on failure, starts at boot,
  `CAP_NET_BIND_SERVICE` so it can bind :80/:443 without root) and starts it

Preview the unit without installing anything: `ivarabase service install --print`.

## 3. HTTPS

With `domain` set, certificates come from **Let's Encrypt automatically** (HTTP-01 on
port 80), on demand, only for `base.example.com`, `db.base.example.com` and
`<slug>.base.example.com` of projects that exist. Port 80 then only redirects to HTTPS.

The same certificate is installed into PostgreSQL, so direct database connections use
TLS (`sslmode=require`, or `verify-full` against the domain). When
`postgres.listen_addresses` is `*`, remote clients are **only** accepted over TLS.

Bring your own certificate instead (e.g. a wildcard, or a Cloudflare origin cert):

```sh
ivarabase config set tls.cert_file /etc/ssl/ivarabase/fullchain.pem --data /var/lib/ivarabase
ivarabase config set tls.key_file  /etc/ssl/ivarabase/privkey.pem   --data /var/lib/ivarabase
sudo ivarabase service restart
```

**Ports 80/443 already taken** (nginx, Caddy, Traefik, a hosting panel)? Run in proxy mode
instead: `sudo ivarabase service install --domain base.example.com --proxy`. Then
`ivarabase proxy-config` prints the nginx (or Caddy) config. Full guide:
[PROXY.md](PROXY.md).

## 4. Firewall

```sh
sudo ufw allow 22,80,443/tcp   # proxy mode: your proxy already owns 80/443
sudo ufw allow 54329/tcp   # only if apps outside the server connect to Postgres directly
sudo ufw enable
```

To accept remote Postgres connections at all: `ivarabase config set postgres.listen_addresses '*'`.

## 5. Day to day

```sh
ivarabase service status | logs | restart
ivarabase config show --data /var/lib/ivarabase
```

Upgrade: copy the new binary over `/usr/local/bin/ivarabase`, then `sudo ivarabase service restart`.
Projects are upgraded to the new schema automatically at start.

Backups: configure an S3-compatible bucket under **Backups → Off-site** in the panel.
To rebuild on a new server, see the README (`ivarabase dr restore`).

## macOS (always-on Mac)

```sh
ivarabase service install            # launchd agent: starts when you log in
sudo ivarabase service install       # launchd daemon: starts at boot, can bind :80/:443
ivarabase service uninstall          # stops it; data is kept
```
