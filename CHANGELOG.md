# Changelog

## Unreleased

## 0.2.3 - 2026-11-27

### Changed
- **Changing `IVARA_ADMIN_USER` now takes effect on its own.** On the next start the login the environment set last time is renamed (and keeps working with `IVARA_ADMIN_PASSWORD`); before, a new user name was only noticed when the password changed too. Upgrading from 0.2.2 with the same values changes nothing and keeps everyone signed in.
- The Dokploy compose file stops with a clear message when `IVARA_ADMIN_PASSWORD` is missing, instead of starting with a random password, and both platform files use `pull_policy: always` so a redeploy picks up new 0.2.x images.
- Every version tag now also publishes a GitHub release with Linux and macOS binaries, `SHA256SUMS` and the notes from this file.

### Fixed
- `IVARA_ADMIN_USER` / `IVARA_ADMIN_PASSWORD` in `<data>/ivarabase.env` were ignored: the server read them before loading that file.
- `docs/CONFIGURATION.md` still said these variables apply on the first start only.

## 0.2.2 - 2026-11-26

### Changed
- **`IVARA_ADMIN_PASSWORD` is no longer first-start only.** Change it and restart, and that login gets the new password (everyone is signed out). The same value on every restart does nothing, so a password changed in the UI or with `ivarabase reset-password` stays until you change the variable. If `IVARA_ADMIN_USER` names a user that doesn't exist, it is created.
- The Codex example in `docs/AGENTS.md` uses `IVARA_TOKEN`, like the other clients.
- The Dokploy and Coolify compose files read `IVARA_DOMAIN` (and, for Dokploy, `IVARA_ADMIN_PASSWORD`), the same names as everywhere else, instead of `IVARABASE_DOMAIN` / `IVARABASE_ADMIN_PASSWORD`. Rename them in the platform's environment settings when you paste the new file.

### Fixed
- Opening the panel while signed out went to the login page straight away instead of flashing the dashboard first.

## 0.2.1 - 2026-11-26

### Changed
- **Less disk use on the server.**
  - Backups: new `keep_local` setting (default 0). With an S3 target, bundles are removed from local disk once uploaded (after the restore test for scheduled runs); `keep_last` still sets how many stay on S3, and restores and verifies download from S3 when needed.
  - Log tables (`function_logs`, `function_console`, `audit`, `auth_mails`, `auth_sms`, `metrics_samples`) are partitioned by day. Expired days are dropped as whole partitions, so the space really goes back to the disk. Upgrading copies the rows still inside retention.
  - Audit log retention defaults to 30 days (was 180) and can be changed with `GET`/`PUT /api/v1/system/audit-retention`. **Upgrading keeps only the last 30 days of audit history.**
  - Postgres uses `wal_compression = zstd` and `default_toast_compression = lz4`.
  - Project databases and restore scratch databases are created with `STRATEGY FILE_COPY`, so creating them no longer writes the whole database into the WAL.

## 0.2.0 - 2026-11-10

First public release.

### Added
- **Realtime**: `supabase.channel()` postgres_changes (RLS-aware), broadcast and presence.
- **Database webhooks**: call a URL or a function on insert/update/delete, with retries, delivery logs and signed payloads.
- **Auth**: anonymous sign-ins, phone sign-in with SMS codes (Twilio or a webhook for any provider), TOTP multi-factor auth (`supabase.auth.mfa`), generic OpenID Connect providers (Keycloak, Auth0, Okta, Entra…), an SMS log, and resetting a user's MFA.
- **Storage**: resumable uploads (TUS 1.0.0) at `/storage/v1/upload/resumable`; S3 multipart for large files.
- **Extensions**: turn on pgvector, pg_trgm, pgcrypto, PostGIS, citext and more per project; a slow queries page (pg_stat_statements).
- **Docker image**: IvaraBase, Postgres, PostgREST and Deno in one container, with a compose file (and Traefik-labelled variants for Docker platforms), and an `ivarabase healthcheck` command.
- **Configuration from the environment**: `IVARA_*` variables or `<data>/ivarabase.env` override `config.json` without being saved; `ivarabase env` lists them.
- **Postgres tuning** that scales with available memory (including container limits), overridable per setting.
- MCP endpoint `/mcp/<token>` for clients that can't send headers.
- Monitoring reads container memory and CPU limits.
- Postgres gets a self-signed certificate when it accepts remote connections without a configured one.

- Highlighted code blocks across the UI (quick starts, MCP configs, JSON previews).
- Links to the project on GitHub in the UI, `ivarabase version`, the OpenAPI spec and the MCP server info.
- A documentation website (`website/`, Astro Starlight) generated from `docs/`.

### Changed
- "Edge functions" are now just "Functions" in the UI.
- Headings use Inter instead of Fraunces.
- Every project has an `extensions` schema, on the REST API's search path.
- `scripts/deps.env` holds the pinned PostgREST and Deno versions for the install script and the Dockerfile.

## 0.1.0

Not released publicly: the platform core, Supabase-compatible REST/auth/storage/functions, backups, monitoring, proxy mode.
