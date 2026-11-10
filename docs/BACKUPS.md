# Backups

Each project backs up to one **`.ivbak` bundle** holding the database (`pg_dump`, zstd-compressed), its functions, secrets, migration history, auth users, webhooks and storage files on local disk. Bundles are **encrypted with AES-256-GCM** in authenticated chunks, so tampering and truncation are detected. The key comes from your **recovery key**.

> The recovery key is printed once, when IvaraBase is first initialized (and saved in `secrets/INITIAL_CREDENTIALS.txt`). Keep it in a password manager. Without it, nobody can restore your backups on another machine, including you.

## Taking backups

- **Now:** **Backups → Back up + verify now** on a project, or `ivarabase backups create --project shop --verify`.
- **Scheduled:** every N hours, keeping the last N bundles per project (default: daily, keep 7). Set it under **Backups → Settings**.
- **Restore tests:** with verification on, each bundle is restored into a scratch database after it's taken. Every table's row count is compared with the manifest, then the scratch database is dropped. A backup that can't be restored shows up as failed, so you find out now and not on the day you need it.
- **Off-site:** copy every bundle to an S3-compatible bucket (AWS, Cloudflare R2, Backblaze B2, MinIO, Hetzner, Wasabi). The SigV4 client is built in, with no SDK. **Backups → Settings → Off-site** has a test button.

## Restoring

- **As a new project** (safe): the bundle becomes a new project next to the old one, for example `shop-restored`.
- **Replace** (destructive, needs confirmation, or approval for agents): a safety backup of the current project is taken first. Then the bundle is restored into a side database and checked, and only after that is it swapped in. If anything fails, the current project stays as it was.

```sh
ivarabase call backups.restore '{"id":"<backup id>","mode":"new","target_project":"shop-restored"}'
```

## Rebuilding on a new machine

When the server is gone, install IvaraBase on the new one (native or Docker) but **don't start it yet**. Then run:

```sh
# from a folder of .ivbak files
ivarabase dr restore --from /path/to/backups --recovery-key XXXX-XXXX-...
# or straight from the off-site bucket
ivarabase dr restore --s3-endpoint https://s3.eu-central-003.backblazeb2.com --s3-bucket my-backups \
  --s3-access-key ... --s3-secret-key ... --recovery-key XXXX-XXXX-...
ivarabase serve
```

Every project comes back from its newest bundle, with its functions, secrets, users and files. The key is checked against a bundle before anything is restored. With Docker, run it as `docker compose run --rm ivarabase dr restore ...` before `docker compose up -d` (see [DOCKER.md](DOCKER.md#moving-to-a-new-postgres-major-version)).

## What's not covered

- **Point-in-time recovery.** Restores go back to the last bundle, not to any second. For busy projects, back up more often.
- **Files in your own S3 storage bucket** stay in that bucket and aren't copied into bundles. Use the provider's versioning or replication for them.
- **Server settings** (`config.json`, access tokens, the owner login) aren't in project bundles. They're small; keep a copy of `config.json`.
