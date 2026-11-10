# Architecture

IvaraBase is one Go binary that runs a multi-project Postgres platform. A project is a database, a few roles and some metadata rows in one Postgres cluster, so an idle project costs close to nothing.

Three goals shape the design:

1. **API first, agent operable.** Every action is an *operation* in one registry. The REST API, the OpenAPI spec, the MCP server, the CLI and the web UI are all generated from that registry, or are clients of it. Nothing is UI-only.
2. **A complete web UI** for every operation, usable from a phone, with a real code editor for SQL and functions.
3. **Backups you can trust:** encrypted, verified, off-site, and a one-command rebuild on a new machine.

## Overview

```text
                ┌──────────────── ivarabase (one Go binary) ────────────────┐
 browser ──────▶│  Web UI (embedded SPA) ─┐                                 │
 curl / SDK ───▶│  REST  /api/v1/...      ├─▶  Operation registry ──▶ handlers│
 CLI ──────────▶│  Ops   /api/v1/ops/{op} │     (schema, level,      │       │
 AI agent ─────▶│  MCP   /mcp (JSON-RPC)  ┘      destructive flag)   │       │
                │                                                    ▼       │
                │  auth: sessions + scoped tokens + approvals    meta store  │
                │  backup engine ─ scheduler ─ local / S3 targets            │
                │  functions runtime (warm Deno processes)                    │
                │  Postgres supervisor (initdb / pg_ctl, health, restart)     │
                └───────────────────────────────┬────────────────────────────┘
                                                ▼
                        Postgres cluster (Homebrew / apt binaries, or external)
                        db per project · role per project · ivarabase_meta
```

### Operation registry (the core idea)
Each operation declares: `name` (`projects.create`), summary, REST method + path,
typed parameters, access level (`read` / `write` / `admin`), and whether it is
`destructive`. From that one declaration we get:

| Surface | How it is produced |
|---|---|
| REST | router built from method + path |
| Generic ops endpoint | `POST /api/v1/ops/{name}` with a JSON body |
| OpenAPI 3.1 | generated at `/api/v1/openapi.json` |
| MCP tools | generated at `/mcp` (tools/list, tools/call) |
| CLI | `ivarabase projects create --slug demo` maps to `projects.create` |
| UI | the SPA calls the same REST endpoints |

### Safety model for agents
- **Scoped tokens**: level (read / write / admin), optional project allowlist,
  optional expiry, `allow_destructive` off by default.
- **Destructive operations** (delete project, restore over live data, delete row,
  delete backup) never run silently: called without `confirm: true` they return a
  **preview** of what would happen.
- **Approvals**: when a token without `allow_destructive` calls a destructive
  operation, the server stores an **approval request** and returns its id. The
  owner approves or rejects it in the UI (works on mobile); the server then runs
  it and the agent reads the result with `approvals.get`.
- **Structured errors** `{error: {code, message, hint}}` so agents can self-correct.
- **Health with fixes**: `system.health` returns checks with a `hint` and, where
  possible, the operation that fixes it.
- **Audit log** of every mutation with the actor (user or token name).

### Backup format (`.ivbak`)
- Per project: `pg_dump -Fc` (zstd when available) + project metadata (functions,
  secrets, migration history) in a tar, **encrypted with AES-256-GCM** in 64 KiB
  authenticated chunks (truncation-proof), key derived from the **recovery key**
  with PBKDF2-SHA256 and a per-bundle salt.
- A `catalog.json` per target lists every bundle with size and SHA-256.
- Targets: local directory (always) + optional S3-compatible bucket (AWS, R2, B2,
  MinIO, Hetzner) via built-in SigV4, no SDK.
- Scheduler: every N hours, retention `keep_last`, optional **automatic verify**
  (restore into a scratch database, check it, drop it) after each run.
- Restore: into a **new project** (safe default) or **replace** (destructive,
  needs confirm / approval).
- Disaster recovery: `ivarabase dr restore --from <dir|s3> ` on a fresh machine
  rebuilds every project from the newest bundles. You only need the recovery key.


## Inside a project

| Piece | How |
|---|---|
| Database | its own database, owned by its own role (`<project>`), with a connection limit |
| REST API | a PostgREST process per project, started on demand, connecting as `<project>_authenticator` and switching to `anon` / `authenticated` / `service_role` per request |
| Keys | `anon` and `service_role` are HS256 JWTs signed with a per-project secret, like Supabase |
| Auth | GoTrue-compatible handlers in the Go server; users, sessions and factors live in the project's `auth` schema |
| Storage | metadata in `storage.objects` (RLS applies), bytes on local disk or an S3-compatible bucket |
| Realtime | WebSocket server in the Go process; table changes are captured by row triggers on tables in the `supabase_realtime` publication, and checked against each subscriber's RLS policies before delivery |
| Functions | one warm Deno process per function, with a clean environment |
| Webhooks | a row trigger queues events in the `ivarabase_hooks` schema; the server delivers them with retries |
| Extensions | created by the superuser from an allowlist into the `extensions` schema |

Every project-controlled object that runs code (triggers, functions in project schemas) runs as the project role, never as the superuser.

## Source layout

| Path | |
|---|---|
| `cmd/ivarabase/` | server, CLI and MCP stdio bridge |
| `internal/core/registry.go` | the operation registry, auth checks, previews, approvals, audit |
| `internal/core/` | everything else: Postgres supervisor, project APIs, backups, monitoring |
| `web/ui/` | React + Vite + Tailwind source; built into `web/static/` and embedded in the binary |
| `tests/compat/` | end-to-end suites with the real `supabase-js`, `tus-js-client` and others |
| `docker/`, `Dockerfile` | the container image |
