# Install with Docker

One container runs everything: IvaraBase, its own PostgreSQL 18, PostgREST and Deno. It sits behind the reverse proxy your server already has (nginx, Caddy, Traefik, Nginx Proxy Manager...). HTTPS ends at that proxy, so the container never needs ports 80/443.

Image: `ghcr.io/ankittchaurasia/ivarabase:<version>-pg<postgres major>`, for example `0.2-pg18`, for amd64 and arm64.

## DNS

Point two records at the server:

| Record | For |
|---|---|
| `base.example.com` | the panel, the API and MCP |
| `*.base.example.com` | projects, at `https://<project>.base.example.com` (what supabase-js uses) |

Without the wildcard, projects still work at `https://base.example.com/proj/<project>`, and supabase-js accepts that URL too.

## Run it

[`docker-compose.yml`](../docker-compose.yml):

```yaml
services:
  ivarabase:
    image: ghcr.io/ankittchaurasia/ivarabase:0.2-pg18
    container_name: ivarabase
    restart: unless-stopped
    environment:
      IVARA_DOMAIN: base.example.com
    ports:
      - "127.0.0.1:8787:8787"
    volumes:
      - ivarabase-data:/data
    shm_size: 256mb
    stop_grace_period: 60s
volumes:
  ivarabase-data:
```

```sh
docker compose up -d
docker exec ivarabase cat /data/secrets/INITIAL_CREDENTIALS.txt   # login + RECOVERY KEY
```

**Save the recovery key in a password manager**, then delete the file: `docker exec ivarabase rm /data/secrets/INITIAL_CREDENTIALS.txt`.

To try it locally without a domain, leave out `IVARA_DOMAIN` and open http://127.0.0.1:8787.

## Put a reverse proxy in front

Point the proxy at `http://127.0.0.1:8787` for both `base.example.com` and `*.base.example.com`, passing the original `Host` header and allowing WebSockets (realtime). IvaraBase prints a ready config with your domain filled in:

```sh
docker exec ivarabase ivarabase proxy-config                 # nginx
docker exec ivarabase ivarabase proxy-config --server caddy  # Caddy
```

See [PROXY.md](PROXY.md) for what each setting does, Traefik, and hosting panels.

### Direct Postgres connections (optional)

To connect with `psql`, `pg_dump` or an ORM from outside the server, publish the port:

```yaml
    ports:
      - "127.0.0.1:8787:8787"
      - "54329:54329"
```

Remote connections must use TLS and a password. Without certificate files, IvaraBase makes a **self-signed certificate**: `sslmode=require` encrypts, but `verify-full` fails. For full verification, mount the proxy's certificate:

```yaml
    environment:
      IVARA_DOMAIN: base.example.com
      IVARA_TLS_CERT_FILE: /certs/fullchain.pem
      IVARA_TLS_KEY_FILE: /certs/privkey.pem
    volumes:
      - ivarabase-data:/data
      - /etc/letsencrypt/live/base.example.com:/certs:ro    # wherever your proxy keeps it
```

The container copies the files into the volume when it starts and again every hour, so renewals are picked up. The source files can stay root-only.

## Platforms

Any Docker host works the same way: the image, a volume on `/data`, port 8787 behind HTTPS.

- **Portainer and similar:** add a stack with the compose file above.
- **Coolify / Dokploy:** ready-made compose files with Traefik labels are in [`deploy/`](../deploy). Set `IVARA_DOMAIN` and an admin password, and the platform's Traefik handles HTTPS. Project subdomains need a wildcard certificate, which Let's Encrypt only issues through a DNS challenge; configure one for your DNS provider in the platform's Traefik settings, or use the `/proj/<project>` URLs. Behind Cloudflare's proxy you don't need the DNS challenge: Cloudflare's certificate covers `*.<domain>`, so remove the `tls.certresolver` and `tls.domains` labels and, for Full (strict), add a Cloudflare origin certificate to the platform's Traefik.

## Running commands

The `ivarabase` CLI is inside the image and already knows where the data is:

```sh
docker exec ivarabase ivarabase token create --name claude-code --level write
docker exec ivarabase ivarabase reset-password
docker exec ivarabase ivarabase doctor
docker exec ivarabase ivarabase env          # every IVARA_* setting
```

Commands run as the `ivarabase` user even under `docker exec`, so nothing in `/data` ends up owned by root.

## Settings

Any `IVARA_*` variable overrides `config.json` without being saved to it, so the compose file stays the one place to change settings. The common ones:

| Variable | Default in the image | |
|---|---|---|
| `IVARA_DOMAIN` | none | panel domain; projects at `<project>.<domain>` |
| `IVARA_PUBLIC_URL` | `https://<domain>` | override when the proxy uses another port or a path |
| `IVARA_ADMIN_USER` / `IVARA_ADMIN_PASSWORD` | `admin` / random | set on first start; changing `IVARA_ADMIN_PASSWORD` later resets that login on the next restart (and signs everyone out) |
| `IVARA_TLS_CERT_FILE` / `IVARA_TLS_KEY_FILE` | none | the proxy's certificate, for Postgres verify-full |
| `IVARA_TRUSTED_PROXIES` | loopback + private ranges | who may set `X-Forwarded-For` |
| `IVARA_PG_SHARED_BUFFERS`, `IVARA_PG_WORK_MEM`, … | scaled to the container's memory | see [CONFIGURATION.md](CONFIGURATION.md) |
| `IVARA_ALLOW_EPHEMERAL` | off | `1` runs without a volume (tests only) |

The full list is in [CONFIGURATION.md](CONFIGURATION.md).

## Things Docker does differently

- **Published ports bypass the firewall.** Docker writes its own iptables rules, so a port published as `8787:8787` (on `0.0.0.0`) is reachable from the internet even when ufw or the host firewall blocks it. Keep `127.0.0.1:8787:8787`. The image trusts `X-Forwarded-For` from private networks, and that's only safe because nothing outside the host can reach the container directly.
- **A volume is required.** The container refuses to start when `/data` isn't a volume or a bind mount, so a `docker rm` can never silently delete your databases.
- **Shared memory.** Docker gives containers 64 MB of `/dev/shm`; Postgres needs more for large queries and parallel workers. Keep `shm_size: 256mb`.
- **Stopping.** `stop_grace_period: 60s` gives Postgres time to shut down cleanly.
- **Memory limits.** Postgres tuning and the monitoring charts follow the container's memory and CPU limits (`deploy.resources.limits` or `--memory`/`--cpus`), not the host's.
- **Bind mounts.** With `- ./data:/data` instead of a named volume, the container takes ownership of the folder (uid 10001) when it starts.

## Updating

```sh
docker compose pull && docker compose up -d
```

Updates within one Postgres major version (`-pg18`) need nothing else. IvaraBase migrates its own metadata on start. Take a backup first anyway: **Backups → Back up + verify now**.

### Moving to a new Postgres major version

An image with a different major (for example `-pg19`) refuses to open a data volume created by `-pg18`, and says why. To move:

1. On the old image, back up every project: **Backups → Back up + verify now** on each project, or `docker exec ivarabase ivarabase backups create --project <name> --verify`.
2. Copy the bundles out: `docker cp ivarabase:/data/backups ./ivb-backups`, or use your off-site S3 copies.
3. Point the compose file at the new tag **and a new volume**, then restore *before* the first start, with the recovery key of the old installation:

```sh
docker compose run --rm -v "$PWD/ivb-backups:/import:ro" ivarabase dr restore --from /import --recovery-key XXXX-XXXX-...
docker compose up -d
```

Backups are `pg_dump` bundles, so they restore into newer Postgres versions.

## Building the image yourself

```sh
make docker                       # ghcr.io/ankittchaurasia/ivarabase:<version>-pg18 for this machine
make docker-multiarch IMAGE=ghcr.io/you/ivarabase   # amd64 + arm64, pushed (docker buildx)
```

The PostgREST and Deno versions and checksums come from `scripts/deps.env`, the same file the native install script uses.
