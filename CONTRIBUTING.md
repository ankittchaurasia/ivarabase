# Contributing

Thanks for helping. Bug reports, docs fixes and pull requests are all welcome.

## Before you start

- **Bugs:** open an issue with what you did, what happened and what you expected, plus `ivarabase version` and the output of `ivarabase doctor` or `system_health`.
- **Features:** open an issue first for anything bigger than a small fix, so we can agree on the approach before you spend time on it.
- **Security problems:** don't open an issue; see [SECURITY.md](SECURITY.md).

## Development

You need Go 1.21+, Node 20+, PostgreSQL 14+ (18 recommended), PostgREST and Deno. On macOS: `brew install go node postgresql@18 postgrest deno pgvector`.

```sh
make dev        # dev instance in ./.dev-data plus a hot-reloading UI on http://localhost:5173 (login in scripts/dev.env)
make test       # go vet + unit tests
make selftest   # every end-to-end suite on throwaway instances (several minutes)
SUITES=features make selftest   # one suite: compat | features | uploads | realtime | webhooks | apple | proxy
make ui         # rebuild the UI into web/static (commit the result: go build embeds it)
```

## How the code is organised

- `internal/core/registry.go`: **operations**. Every feature is an operation with typed parameters, an access level and a destructive flag. REST, OpenAPI, MCP tools and the CLI are generated from these. Add a feature as an operation first, then build its UI on top.
- `internal/core/`: the rest of the server: Postgres supervisor, project APIs (REST, auth, storage, realtime, functions), backups, monitoring.
- `web/ui/`: React + Vite + Tailwind. The UI only calls operations (`POST /api/v1/ops/<name>`).
- `tests/compat/`: end-to-end suites using the real `supabase-js`, `tus-js-client` and others.

## Pull requests

- Keep each PR to one change, and add or update tests (a Go test or an end-to-end check in `tests/compat/`).
- Destructive operations must return a preview without `confirm: true`. Errors are `APIError`s with a `code`, a `message` and a `hint` an agent can act on.
- Projects must stay isolated: nothing may let one project's token or key reach another project's data.
- Run `gofmt`, `make test` and the relevant `make selftest` suites.
- Add a line to [CHANGELOG.md](CHANGELOG.md) under "Unreleased".
- By submitting a PR you agree to the [CLA](CLA.md). Add "I have read the CLA and agree to its terms." to the PR description.
