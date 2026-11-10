# Comparison

How IvaraBase compares with Supabase, PocketBase and Firebase. All four are good tools, and each fits a different situation. This page tries to be fair. It's written by the IvaraBase project, so check the claims that matter to you.

## At a glance

|  | **IvaraBase** | Supabase (self-hosted) | PocketBase | Firebase |
|---|:---:|:---:|:---:|:---:|
| **Run by AI agents** | ✅ built-in MCP server; every action available | ⚠️ separate MCP server, mostly for the cloud | ❌ | ⚠️ MCP through the Firebase CLI |
| **Approvals for an agent's destructive actions** | ✅ previews and approval requests, audit log | ❌ | ❌ | ❌ |
| **Many projects on one server** | ✅ a project idles at ~0 MB | ❌ one full stack per project | ❌ one process per app | ☁️ cloud projects, billed separately |
| **Install** | ✅ 1 container or 1 binary | ⚠️ ~12 containers | ✅ 1 binary | ❌ not self-hostable |
| **Database** | PostgreSQL 18 | PostgreSQL | SQLite | Firestore (document, NoSQL) |
| **SQL, joins, transactions, extensions** | ✅ | ✅ | ⚠️ SQLite | ❌ |
| **`supabase-js` and Supabase SDKs** | ✅ | ✅ | ❌ | ❌ |
| **Row level security** | ✅ Postgres RLS | ✅ Postgres RLS | ✅ API rules | ✅ security rules |
| **Auth: email, magic links, OAuth** | ✅ | ✅ | ✅ | ✅ |
| **Auth: phone SMS, anonymous, MFA (TOTP)** | ✅ | ✅ | ⚠️ MFA only | ✅ |
| **Auth: any OpenID Connect provider** | ✅ | ⚠️ paid SSO on cloud | ✅ | ⚠️ Identity Platform |
| **Realtime** | ✅ RLS-aware | ✅ | ✅ | ✅ |
| **File storage** | ✅ with resumable uploads | ✅ | ✅ | ✅ |
| **Server functions** | ✅ Deno, TypeScript | ✅ Deno, TypeScript | ⚠️ JS hooks (embedded) | ✅ Cloud Functions |
| **Database webhooks** | ✅ | ✅ | ⚠️ via hooks | ✅ triggers |
| **Vector search** | ✅ pgvector | ✅ pgvector | ❌ | ✅ |
| **Backups** | ✅ encrypted, restore-tested, S3, one-command rebuild | ⚠️ your own scripts when self-hosted | ⚠️ zip backups, not encrypted | ☁️ scheduled exports (paid) |
| **Monitoring and alerts** | ✅ built in | ⚠️ partial when self-hosted | ❌ | ✅ |
| **GraphQL** | ❌ | ✅ | ❌ | ❌ |
| **Point-in-time recovery, branching** | ❌ | ✅ on paid cloud plans | ❌ | ⚠️ |
| **Managed hosting** | ❌ self-hosted | ✅ | ❌ (community hosts) | ✅ |
| **Maturity and ecosystem** | 🌱 new (v0.2) | ✅ large | ✅ large | ✅ very large |
| **License** | AGPL-3.0 (+ commercial) | Apache-2.0 | MIT | Proprietary |

✅ yes · ⚠️ partly or with extra work · ❌ no · ☁️ cloud only

## When to choose which

**Choose IvaraBase** when:
- you want AI agents to build and run your backends, with guardrails: scoped tokens, previews, approvals and an audit log;
- you run several projects (client work, side projects, staging copies) and want them on one small server without paying per project;
- you like Supabase's APIs and SDKs but want something light to self-host, with backups you don't have to script.

**Choose Supabase** when you want managed hosting, GraphQL, branching, point-in-time recovery, or the larger ecosystem and support. IvaraBase speaks the same APIs, so moving between them stays possible ([SUPABASE.md](SUPABASE.md)).

**Choose PocketBase** when one small app with SQLite is enough and you want the simplest possible single binary, or want to extend it in Go.

**Choose Firebase** when you want a fully managed Google service with offline-first mobile SDKs and NoSQL, and don't need SQL or self-hosting.

## Resource use

IvaraBase runs one Postgres cluster for every project, so adding a project adds a database rather than a stack of services. An idle project uses almost no memory: PostgREST processes and function runtimes start on the first request and stop when idle. Postgres settings follow the memory you give the container ([CONFIGURATION.md](CONFIGURATION.md#postgres-tuning)), so it runs on small VPSes too.

Self-hosted Supabase runs one set of services (database, auth, REST, realtime, storage, studio, gateway and more) per project.

*Last reviewed November 2026. Products change; corrections are welcome in [Discussions](https://github.com/ankittchaurasia/ivarabase/discussions).*
