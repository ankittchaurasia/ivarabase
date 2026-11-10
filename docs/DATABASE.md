# Database

Every project is its own PostgreSQL database, owned by its own role, in one cluster that IvaraBase manages. That cluster is on port 54329 and in the data directory, and it never touches another Postgres on the machine.

## SQL, tables and migrations

- **SQL editor:** read-only by default. Writes need a `write` token or a signed-in owner, and statements that drop or empty data ask for confirmation.
- **Table editor:** browse, filter, insert, edit and delete rows, see columns and foreign keys, and turn on RLS per table.
- **Migrations:** `db.migrations_apply` runs SQL in a transaction and records it with a checksum, so applying the same migration twice is a no-op and a changed migration is refused. Keep them in your repo with [config as code](CONFIG-AS-CODE.md):

```sh
ivarabase db migrations-apply --project shop --name add_orders --sql @migrations/002_orders.sql
ivarabase db types --project shop --out database.types.ts     # supabase-js typed client
```

Tables in `public` are reachable through the REST API by `anon`, `authenticated` and `service_role`, as in Supabase. **Row level security is what protects rows**: turn it on for every table, and the health checks warn about tables without it.

## Extensions

Turn extensions on per project on the **Extensions** page, or with `db.extension_enable`:

```sh
ivarabase db extension-enable --project shop --name vector
```

They go into the `extensions` schema, like Supabase. That schema is on the search path for SQL and the REST API, so `vector(1536)` and `similarity()` work without a prefix, and migrations written for Supabase (`create extension vector with schema extensions`) run as they are.

| Extension | For |
|---|---|
| `vector` (pgvector) | embeddings and similarity search (RAG) |
| `pg_trgm`, `unaccent`, `fuzzystrmatch` | fuzzy and accent-insensitive search |
| `pgcrypto`, `uuid-ossp` | hashing, encryption, UUIDs |
| `citext`, `hstore`, `ltree`, `intarray`, `cube`, `earthdistance` | data types |
| `btree_gin`, `btree_gist`, `tablefunc` | index operator classes, crosstab |
| `postgis` | geography and spatial indexes |
| `pg_stat_statements`, `pg_buffercache` | statistics |

Only this list is allowed, because extensions are created by the superuser. Extensions that reach files or other databases (`dblink`, `postgres_fdw`, `file_fdw`, untrusted languages) are not on it.

pgvector and the contrib extensions come with the Docker image and with `scripts/install-deps-ubuntu.sh`. PostGIS adds about 100 MB, so it's optional: `sudo WITH_POSTGIS=1 bash scripts/install-deps-ubuntu.sh` on Ubuntu/Debian, `brew install postgis` on a Mac.

Backups restore extensions too, including into a new project.

### pgvector example

```sql
create table documents (id bigserial primary key, body text, embedding vector(1536));
create index on documents using hnsw (embedding vector_cosine_ops);

create function match_documents(query vector(1536), n int default 5)
returns table (id bigint, body text, similarity float) language sql stable as $$
  select id, body, 1 - (embedding <=> query) from documents order by embedding <=> query limit n
$$;
```

```ts
const { data } = await supabase.rpc("match_documents", { query: embedding, n: 5 });
```

## Slow queries

**Extensions → Slow queries** (or `db.slow_queries`) lists the project's statements by total time, average time or number of calls, with the cache hit ratio, from `pg_stat_statements`. IvaraBase loads `pg_stat_statements` automatically when it's installed. Reset the numbers after adding an index to see the difference.

## Connecting directly

The project's **API** page shows a connection string for psql, ORMs (Prisma, Drizzle) and `pg_dump`:

```text
postgresql://shop:<password>@base.example.com:54329/shop?sslmode=require
```

By default Postgres only listens on `127.0.0.1`. To connect from elsewhere:

- **Native:** set `postgres.listen_addresses` to `*` in `config.json` (or `IVARA_PG_LISTEN=*`), open port 54329 in the firewall, and restart.
- **Docker:** publish `54329:54329` ([DOCKER.md](DOCKER.md#direct-postgres-connections-optional)).

Remote connections require TLS and a password. With the domain's certificate (built-in HTTPS, or the proxy's certificate files) clients can use `sslmode=verify-full`. Otherwise IvaraBase uses a self-signed certificate and `sslmode=require`. Rotate a project's password with `projects.rotate_password`.

Each project role is limited to 60 connections.

## Tuning

Postgres settings scale with the memory IvaraBase can use: the container's limit in Docker, the machine's RAM otherwise. The defaults are deliberately conservative, since PostgREST, Deno and often other apps share the machine:

| Setting | Default |
|---|---|
| `shared_buffers` | 20% of memory (128 MB to 8 GB) |
| `effective_cache_size` | 60% of memory |
| `work_mem` | scales with memory (4 MB to 64 MB) |
| `maintenance_work_mem` | 5% of memory (64 MB to 1 GB) |
| `max_connections` | 200 |
| `random_page_cost` | 1.1 (SSD) |

Override any of them, or set any other Postgres parameter, with `postgres.settings` in `config.json` or with `IVARA_PG_*` variables ([CONFIGURATION.md](CONFIGURATION.md#postgres-tuning)), then restart.

## Using an existing Postgres

Set `postgres.external_dsn` to a superuser connection string to use a cluster you run yourself instead of the managed one. IvaraBase then doesn't start, stop or tune it. Extensions must be installed on that server.
