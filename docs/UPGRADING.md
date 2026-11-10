# Upgrading

## IvaraBase

IvaraBase upgrades itself on start. Its metadata database has numbered migrations that run once, in order, and each project's auth, storage, realtime and webhook schemas are brought up to date automatically. Downgrading isn't supported, so take a backup before upgrading.

**Docker**

```sh
docker compose pull && docker compose up -d
```

**Native (systemd or launchd)**

```sh
make linux        # or download a release binary
sudo install -m 0755 bin/ivarabase-linux-amd64 /usr/local/bin/ivarabase
sudo ivarabase service restart
```

If a release bumps PostgREST or Deno (see [CHANGELOG.md](../CHANGELOG.md)), re-run `sudo bash scripts/install-deps-ubuntu.sh` (with `scripts/deps.env` next to it) before restarting.

After upgrading, check **Dashboard → Health**, or run `ivarabase doctor`.

## PostgreSQL

**Minor versions** (18.1 → 18.6) arrive with system updates (`apt upgrade`, a new Docker image) and need only a restart.

**Major versions** (18 → 19) change the data format. IvaraBase refuses to start an old data directory with new binaries and tells you why. Move through backups, which restore into newer versions:

1. Back up every project with verification, and keep the bundles off the server (off-site S3, or copy the `backups` folder).
2. Install the new Postgres major (or switch to the `-pg19` image) with a **fresh data directory or volume**.
3. Before the first start, run `ivarabase dr restore --from <folder or S3> --recovery-key ...` (see [BACKUPS.md](BACKUPS.md#rebuilding-on-a-new-machine)).
4. Start IvaraBase and check the projects.

Keep the old data directory until you're happy with the result.

## From 0.1 to 0.2

Nothing to do by hand. On first start, 0.2:

- adds the `extensions` schema to every project and puts it on the REST API's search path;
- adds the MFA tables and phone columns to each project's `auth` schema;
- sets up realtime and webhook support in each project;
- writes Postgres tuning based on available memory. If you had tuned Postgres by editing `postgresql.conf`, move those settings to `postgres.settings` or `IVARA_PG_*`. IvaraBase's own `ivarabase.conf` is included last, so it wins over `postgresql.conf`.

`scripts/install-deps-ubuntu.sh` now reads `scripts/deps.env`, so copy both files to the server, and it also installs pgvector.
