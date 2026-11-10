# Supabase compatibility

Each IvaraBase project speaks the same HTTP APIs as a Supabase project, so `supabase-js` (and the Swift, Flutter, Kotlin and Python clients, which use the same APIs) work by changing only the URL and the key. The end-to-end suites run the real `@supabase/supabase-js` against every release.

```ts
const supabase = createClient("https://shop.base.example.com", "<anon key>");
// without a wildcard domain: createClient("https://base.example.com/proj/shop", "<anon key>")
```

The URL and both keys (`anon`, `service_role`) are on the project's **API** page and in `ivarabase projects api --project shop`.

## What works

| Area | Supported |
|---|---|
| **Database / REST** (`/rest/v1`, PostgREST 16) | select with filters, embedding, ordering, ranges, counts, insert/upsert/update/delete, RPC, `Prefer` headers, RLS with `anon` / `authenticated` / `service_role`, `auth.uid()`, `auth.jwt()`, `auth.role()` |
| **Auth** (`/auth/v1`) | sign up, password sign-in, magic links, email OTP, password recovery, email change, refresh-token rotation, sign-out scopes, PKCE, OAuth (Google, GitHub, Apple, any OIDC provider), `signInWithIdToken` (Apple, Google, OIDC), **anonymous users**, **phone OTP / phone + password**, **MFA (TOTP)**, admin API (`auth.admin.*`, including MFA factors) |
| **Storage** (`/storage/v1`) | buckets (public/private, size and mime limits), upload, download, list, move, copy, remove, signed URLs, signed upload URLs, RLS on `storage.objects`, **resumable uploads (TUS)** |
| **Realtime** (`/realtime/v1`) | `postgres_changes` (RLS-aware, filters), broadcast (`self`, `ack`, REST send), presence; `alter publication supabase_realtime add table ...` |
| **Functions** (`/functions/v1`) | `supabase.functions.invoke()`, `Deno.serve()` handlers, JWT verification per function, secrets as env vars, `SUPABASE_URL` / `SUPABASE_ANON_KEY` / `SUPABASE_SERVICE_ROLE_KEY` / `SUPABASE_DB_URL` |
| **Database webhooks** | insert/update/delete → URL or function, Supabase's payload shape |
| **Extensions** | pgvector, pg_trgm, pgcrypto, uuid-ossp, citext, PostGIS, hstore, ltree and others, in the `extensions` schema |
| **Schema** | the `auth.users`, `auth.identities` and `storage.objects` tables use Supabase's names, so `references auth.users(id)` and existing policies work |

## Not supported

- **GraphQL** (`pg_graphql`).
- **Point-in-time recovery, branching, read replicas.** Backups are full logical snapshots (see [BACKUPS.md](BACKUPS.md)).
- **Supabase CLI** (`supabase db push`, `supabase functions deploy`). Use `ivarabase apply` or `ivarabase functions deploy` instead ([CONFIG-AS-CODE.md](CONFIG-AS-CODE.md)).
- **SAML SSO, web3 sign-in, phone MFA factors, auth hooks, captcha.**
- **Storage image transformations** (`/render/image`).
- **Realtime private channels** (`config.private: true` with policies on `realtime.messages`); they currently need the service_role key.
- **Supavisor/pgbouncer pooling.** Apps connect to Postgres directly, which suits most self-hosted sizes.
- Less common extensions: `pg_cron`, `pg_net`, `pgsodium`/Vault, `wrappers`.

## Moving a project from Supabase

1. **Schema and data.** Dump from Supabase. The connection string is under Project Settings → Database:

   ```sh
   pg_dump "postgresql://postgres:...@db.xxxx.supabase.co:5432/postgres" \
     --schema=public --no-owner --no-privileges -Fc -f app.dump
   ```

   Create the project in IvaraBase, turn on the extensions it uses (**Extensions** page), then restore with the project's own connection string from its **API** page:

   ```sh
   pg_restore --no-owner --no-privileges -d "postgresql://shop:...@base.example.com:54329/shop?sslmode=require" app.dump
   ```

   Then re-create your RLS policies and grants if the dump didn't include them (they're in your migrations, usually).

2. **Users.** `auth.users` has the same columns. Copy the rows you need, including `encrypted_password` (bcrypt hashes work as they are), using `pg_dump --table=auth.users --data-only` and `--table=auth.identities --data-only`. Users keep their passwords.

3. **Storage.** Download the buckets (for example with the Supabase CLI or rclone over S3), create the same buckets in IvaraBase, and upload with `supabase-js` or the Storage page.

4. **Functions.** Copy `supabase/functions/<name>/` and deploy it with `ivarabase functions deploy --project shop --dir ./supabase/functions/<name>`. Code using `Deno.serve` and the `SUPABASE_*` env vars runs unchanged. Set your secrets on the **Secrets** page.

5. **Auth settings.** Copy the site URL, redirect URLs, SMTP and OAuth providers to the project's **Authentication** page. Each OAuth provider's callback URL changes to the one shown there.

6. **Apps.** Change the URL and anon key in your app.
