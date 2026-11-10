# Config as code

Keep a project in your repo and apply it to any IvaraBase server - a teammate's laptop,
staging, or production - with a plan you can review first.

```text
my-app/
  ivarabase.yaml
  .env                 # secret values (keep out of git)
  migrations/          # *.sql, applied in file-name order, each in one transaction
  functions/
    send-email/        # one folder per function (index.ts + any other files)
      index.ts
      lib/templates.ts
```

```yaml
project: shop                   # created if it doesn't exist
name: Shop
migrations: migrations
functions_dir: functions
functions:                      # optional per-function settings
  stripe-webhook: { verify_jwt: false }
  nightly-report: { schedule: "0 3 * * *", timeout_s: 120 }
secrets:
  STRIPE_KEY: ${STRIPE_KEY}     # from the environment or .env next to this file
auth:
  site_url: https://myapp.com
  redirect_urls: [http://localhost:3000/*]
  confirm_email: true
  password_min_length: 8
  google: { enabled: true, client_id: 123.apps.googleusercontent.com, client_secret: ${GOOGLE_CLIENT_SECRET} }
  apple:                        # see docs/APPLE.md
    enabled: true
    client_id: com.myapp.web    # Services ID (web sign-in)
    team_id: ABCDE12345
    key_id: XYZ9876543
    private_key: ${APPLE_PRIVATE_KEY}   # the .p8 contents; literal \n is fine in .env
    bundle_ids: [com.myapp.ios] # native apps (signInWithIdToken)
  smtp: { host: smtp.resend.com, port: 587, user: resend, pass: ${SMTP_PASS}, from_email: no-reply@myapp.com }
storage:
  buckets:
    avatars: { public: true, file_size_limit: 1MB, allowed_mime_types: [image/*] }
    documents: { public: false }
```

## Commands

```sh
ivarabase login --url https://base.example.com --token ivb_...
ivarabase export --project shop          # start from an existing project
ivarabase plan                            # what would change
ivarabase apply                           # asks, then applies (--yes for CI)
ivarabase apply --prune                   # also delete functions/secrets/buckets not in the files
ivarabase db types --project shop --out src/database.types.ts
```

## Rules

- **Migrations are append-only.** A migration that was applied is matched by name (or by
  identical SQL, so renamed exports still match). Editing an applied migration is an error -
  `apply` refuses to run and nothing changes. Add a new migration instead.
- **Steps run through the normal API**: the token's level and project scope apply, every
  change is in the audit log, and destructive SQL (DROP/TRUNCATE/unbounded DELETE) needs the
  same confirmation as anywhere else.
- **Deletions only with `--prune`**, and a token without `allow_destructive` gets an
  approval request instead - the owner approves it in the UI, then you run `apply` again.
- **Secrets never leave the server**: `plan` compares them on the server and only reports
  "create" or "update"; `export` writes `${NAME}` placeholders.
- `apply` stops at the first failed step and reports where.

The same operations are available to agents over MCP as `config_plan`, `config_apply` and
`config_export` (the spec is JSON instead of files).

## CI example (GitHub Actions)

```yaml
- run: curl -fsSL -o ivarabase https://YOUR-HOST/ivarabase-linux-amd64 && chmod +x ivarabase
- run: ./ivarabase apply --yes
  env:
    IVARA_URL: https://base.example.com
    IVARA_TOKEN: ${{ secrets.IVARA_TOKEN }}   # write-level token scoped to this project
    STRIPE_KEY: ${{ secrets.STRIPE_KEY }}
```

## Compatibility suite

`make compat` creates a test project from `tests/compat/project/ivarabase.yaml` and runs the
real `supabase-js` client against it: REST, auth, storage and functions (52 checks).
