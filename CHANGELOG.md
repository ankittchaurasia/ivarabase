# Changelog

## Unreleased

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
