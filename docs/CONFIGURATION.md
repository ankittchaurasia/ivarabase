# Configuration

IvaraBase works with no configuration on a laptop. On a server you set a few things: the domain, the HTTPS mode, and sometimes Postgres settings. There are three ways, and the first one that sets a value wins:

1. **Environment variables** (`IVARA_*`), for example from a Docker compose file or a systemd drop-in.
2. **`<data>/ivarabase.env`**: `KEY=VALUE` lines, read on start. Set `IVARA_ENV_FILE` to use another file. Real environment variables win over this file.
3. **`<data>/config.json`**: written by `ivarabase init`, `service install` and `ivarabase config set`.

Values from 1 and 2 are **never written back** to `config.json`, so the compose file or env file stays the one place to change them. `ivarabase env` lists every variable. Restart after changes.

The data directory is `~/.ivarabase` by default (`/var/lib/ivarabase` for `service install`, `/data` in Docker). Set it with `--data DIR` or `IVARA_DATA`.

## Server

| Variable | `config.json` | |
|---|---|---|
| `IVARA_LISTEN` | `listen` | HTTP address for the UI, API and MCP. Default `127.0.0.1:8787` (`0.0.0.0:8787` in Docker) |
| `IVARA_DOMAIN` | `domain` | turns on name-based hosting: panel at `https://<domain>`, projects at `https://<project>.<domain>` |
| `IVARA_PUBLIC_URL` | `public_url` | how clients reach the server; worked out from the domain when unset |
| `IVARA_TLS_MODE` | `tls.mode` | `auto` (Let's Encrypt on 80/443), `files` (your certificate), `proxy` (HTTPS ends at nginx/Caddy/Traefik), `off` |
| `IVARA_TLS_EMAIL` | `tls.email` | Let's Encrypt contact |
| `IVARA_TLS_CERT_FILE`, `IVARA_TLS_KEY_FILE` | `tls.cert_file`, `tls.key_file` | certificate files. In `proxy` mode they're used for Postgres TLS only |
| `IVARA_TRUSTED_PROXIES` | `trusted_proxies` | comma-separated IPs/CIDRs allowed to set `X-Forwarded-For`. Default: loopback (Docker: loopback + private ranges) |
| `IVARA_DENO_PATH`, `IVARA_POSTGREST_PATH` | `deno_path`, `postgrest_path` | found on `PATH` when unset |
| `IVARA_ADMIN_USER`, `IVARA_ADMIN_PASSWORD` | none | the owner login (password at least 10 characters). Set on first start; change either one later and the next start applies it: a new password replaces the old one, a new user name renames that login, and everyone is signed out. The same values on every start change nothing, so a password changed in the UI stays. Read from the environment or `<data>/ivarabase.env` |
| `IVARA_WEBHOOKS_ALLOW_PRIVATE` | `webhooks_allow_private_targets` | `1` lets database webhooks call private and loopback addresses ([WEBHOOKS.md](WEBHOOKS.md#safety)) |

`ivarabase config set KEY VALUE` changes `config.json`, for example `ivarabase config set tls.mode proxy`.

## Postgres

| Variable | `config.json` | |
|---|---|---|
| `IVARA_PG_PORT` | `postgres.port` | managed cluster port, default `54329` |
| `IVARA_PG_LISTEN` | `postgres.listen_addresses` | `127.0.0.1` (default) or `*` for remote connections (TLS required) |
| `IVARA_PG_PUBLIC_HOST` | `postgres.public_host` | host shown in connection strings (default: the domain) |
| `IVARA_PG_BIN_DIR` | `postgres.bin_dir` | folder with `postgres`, `initdb`, `pg_dump`; the newest install is found when unset |
| `IVARA_PG_EXTERNAL_DSN` | `postgres.external_dsn` | use an existing cluster (superuser DSN) instead of a managed one |

## Postgres tuning

The defaults scale with the memory IvaraBase may use: a container's cgroup limit when there is one, otherwise the machine's RAM.

| Variable | Postgres setting | Default |
|---|---|---|
| `IVARA_PG_MAX_CONNECTIONS` | `max_connections` | 200 |
| `IVARA_PG_SHARED_BUFFERS` | `shared_buffers` | 20% of memory, 128 MB to 8 GB |
| `IVARA_PG_EFFECTIVE_CACHE_SIZE` | `effective_cache_size` | 60% of memory |
| `IVARA_PG_WORK_MEM` | `work_mem` | (memory − shared_buffers) / (3 × max_connections), 4 MB to 64 MB |
| `IVARA_PG_MAINTENANCE_WORK_MEM` | `maintenance_work_mem` | 5% of memory, 64 MB to 1 GB |
| `IVARA_PG_MAX_WAL_SIZE` | `max_wal_size` | 1GB |
| `IVARA_PG_RANDOM_PAGE_COST` | `random_page_cost` | 1.1 (SSD) |
| `IVARA_PG_MAX_WORKER_PROCESSES` | `max_worker_processes` | Postgres default (8) |
| `IVARA_PG_MAX_PARALLEL_WORKERS` | `max_parallel_workers` | Postgres default (8) |
| `IVARA_PG_STATEMENT_TIMEOUT` | `statement_timeout` | off |
| `IVARA_PG_SET_<NAME>` | any other setting | for example `IVARA_PG_SET_LOG_MIN_DURATION_STATEMENT=500ms` |

The same keys work in `config.json`:

```json
{ "postgres": { "settings": { "shared_buffers": "2GB", "log_min_duration_statement": "500ms" } } }
```

IvaraBase writes these to `<data>/pg/ivarabase.conf` on every start. `port`, `listen_addresses` and the TLS files can't be set this way; use the fields above. `pg_stat_statements` is added to `shared_preload_libraries` automatically when it's installed, unless you set `shared_preload_libraries` yourself.

### Example: `ivarabase.env` on a 4 GB VPS shared with other apps

```sh
IVARA_PG_SHARED_BUFFERS=512MB
IVARA_PG_EFFECTIVE_CACHE_SIZE=1536MB
IVARA_PG_MAX_CONNECTIONS=100
```

## Everything else

Auth, storage, backups, alerts and functions are configured per project or per server in the UI, or through their operations (`auth.config_set`, `storage.config_set`, `backups.config_set`, `alerts.*`). Those settings live encrypted in IvaraBase's metadata database, not in files.
