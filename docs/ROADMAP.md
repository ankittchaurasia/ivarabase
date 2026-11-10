# Roadmap

What's shipped, what's next, and what's deliberately out of scope. Ideas and votes are welcome in [GitHub Discussions](https://github.com/ankittchaurasia/ivarabase/discussions).

## Shipped

**0.2** (first public release)
- Realtime: `postgres_changes` with RLS, broadcast, presence
- Database webhooks with retries and signed requests
- Resumable uploads (TUS)
- Auth: anonymous users, phone sign-in with SMS codes, TOTP MFA, any OpenID Connect provider
- Extensions per project (pgvector, PostGIS, contrib) and a slow-queries page
- Postgres tuning that follows available memory; `IVARA_*` environment settings
- Docker image for amd64 and arm64
- MCP for clients that can't send headers (ChatGPT)

**0.1**
- Projects with their own database, REST API (PostgREST), auth, storage and functions, compatible with `supabase-js`
- One operation registry behind the UI, REST, OpenAPI, CLI and MCP; scoped tokens, previews, approvals and an audit log
- Encrypted, verified backups with schedules, restore tests, S3 copies and disaster recovery
- Monitoring and alerts; automatic HTTPS or proxy mode; config as code

## Next

Ideas, not promises:

- Realtime private channels (policies on `realtime.messages`)
- Auth hooks and captcha
- Storage image transformations and signed resumable uploads
- Team members with roles on the panel (today: one owner plus scoped tokens)
- Connection pooling for serverless clients
- OAuth for the MCP endpoint, instead of tokens in URLs

## Out of scope

GraphQL, point-in-time recovery, branching, and read replicas or failover. One server is one Postgres cluster.
