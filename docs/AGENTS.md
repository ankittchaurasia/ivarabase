# AI agents (MCP)

IvaraBase is built so an AI agent can run it. Every operation the web UI uses (over 100) is also an **MCP tool**: create projects, write and apply migrations, query data, deploy functions, configure auth and storage, manage webhooks and realtime, take and restore backups, read monitoring. Tool names are the operation names with `_` in place of `.`, for example `projects_create` and `db_migrations_apply`.

| Endpoint | For |
|---|---|
| `https://base.example.com/mcp` | Streamable HTTP; send `Authorization: Bearer ivb_...` |
| `https://base.example.com/mcp/ivb_...` | the same, with the token in the URL, for clients that can't send headers (ChatGPT) |
| `ivarabase mcp` | a stdio bridge for clients that only run local commands |

## 1. Create a token

In the UI: **Access tokens → New token**. Or on the server:

```sh
ivarabase token create --name claude-code --level write                     # every project
ivarabase token create --name shop-agent --level write --projects shop --expires-days 30
```

| Level | Can |
|---|---|
| `read` | list and inspect everything, run read-only SQL |
| `write` | change data and schema, deploy functions, configure projects |
| `admin` | also tokens, backup settings and server-wide settings |

A token **without** `--allow-destructive` (the default) can't drop tables, delete projects, replace data or rotate secrets on its own. Those calls become **approval requests**: the agent gets an approval id, you approve or reject it on the **Approvals** page (works on a phone), and the agent reads the result with `approvals_get`. Every call is in the **Audit log** with the token's name.

## 2. Add it to your client

### Claude Code

```sh
claude mcp add --transport http ivarabase https://base.example.com/mcp --header "Authorization: Bearer ivb_..."
```

### Codex (CLI and IDE extension)

`~/.codex/config.toml`:

```toml
[mcp_servers.ivarabase]
url = "https://base.example.com/mcp"
bearer_token_env_var = "IVARA_TOKEN"   # export IVARA_TOKEN=ivb_...
```

If your Codex version doesn't support HTTP servers, use the stdio bridge instead:

```toml
[mcp_servers.ivarabase]
command = "ivarabase"
args = ["mcp"]
env = { IVARA_URL = "https://base.example.com", IVARA_TOKEN = "ivb_..." }
```

### ChatGPT

ChatGPT connectors can't send custom headers, so put the token in the URL:

1. **Settings → Apps & Connectors → Advanced settings**: turn on **Developer mode**.
2. **Create** a connector. URL: `https://base.example.com/mcp/ivb_...`. Authentication: **No authentication**.

Anyone with that URL holds the token, so treat it as a password. Give this token only the projects it needs, set an expiry, and keep `allow_destructive` off. Proxy access logs record URLs, so on a shared server turn off logging for `/mcp/` or rotate the token now and then.

### Cursor

`~/.cursor/mcp.json` (or `.cursor/mcp.json` in a project):

```json
{
  "mcpServers": {
    "ivarabase": {
      "url": "https://base.example.com/mcp",
      "headers": { "Authorization": "Bearer ivb_..." }
    }
  }
}
```

### VS Code (Copilot agent mode)

`.vscode/mcp.json`:

```json
{
  "inputs": [{ "id": "ivarabase-token", "type": "promptString", "description": "IvaraBase token", "password": true }],
  "servers": {
    "ivarabase": {
      "type": "http",
      "url": "https://base.example.com/mcp",
      "headers": { "Authorization": "Bearer ${input:ivarabase-token}" }
    }
  }
}
```

### Claude Desktop and other stdio-only clients

```json
{
  "mcpServers": {
    "ivarabase": {
      "command": "/usr/local/bin/ivarabase",
      "args": ["mcp"],
      "env": { "IVARA_URL": "https://base.example.com", "IVARA_TOKEN": "ivb_..." }
    }
  }
}
```

The bridge is the same `ivarabase` binary, so install it on the machine where the client runs (`make build`, or a release binary).

Any other client works too (Windsurf, Gemini CLI, Zed, Cline...), with either the HTTP endpoint plus a Bearer header or the stdio bridge.

## 3. What to ask

Agents do best when they can look before they change things. Tools such as `system_health`, `projects_list`, `db_schema` and `db_tables` exist for that. Some prompts that work well:

- "Create a project called **shop** with products, orders and order_items. Users should only see their own orders. Add a migration, then generate TypeScript types."
- "Turn on pgvector in **docs**, add an embeddings table with an HNSW index and an RPC `match_documents`."
- "Add Google sign-in and email confirmation to **shop**; the site is https://shop.example.com."
- "Deploy a function that sends a receipt email when an order is inserted, and a database webhook that calls it."
- "Why is **shop** slow? Check the slow queries and suggest indexes."
- "Back up **shop**, verify it, and restore it as **shop-staging**."

## Safety notes

- Destructive tools return a **preview** first. The agent sees what would happen and has to call again with `confirm: true`. Without `allow_destructive`, that second call becomes an approval request for you.
- SQL through `db_query` is **read-only** unless the agent passes `read_only: false`, which needs a `write` token. `DROP`, `TRUNCATE`, and `DELETE`/`UPDATE` without `WHERE` count as destructive.
- Errors come back as `{code, message, hint}` so the agent can correct itself. `system_health` lists problems with the tool that fixes each one.
- Use separate tokens per agent or per job. Revoking one (**Access tokens → Revoke**) doesn't affect the others.

## The same operations everywhere

The MCP tools are the same operations as:

- REST: `GET/POST /api/v1/...`, documented at `/api/v1/openapi.json`.
- The generic endpoint: `POST /api/v1/ops/<operation>` with a JSON body.
- The CLI: `ivarabase <group> <action> --param value`, after `ivarabase login`.

So a script, a CI job and an agent all go through the same permission checks and the same audit log.
