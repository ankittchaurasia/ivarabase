# Functions

TypeScript functions on Deno, called with `supabase.functions.invoke()` or plain HTTP at `/functions/v1/<name>`. Each deployed function is a warm Deno process: it starts on the first request, stops after 10 idle minutes, and restarts on redeploy.

```ts
// functions/hello/index.ts - Supabase style works as it is
Deno.serve(async (req) => {
  const { name } = await req.json();
  return Response.json({ message: `Hello ${name}` });
});
// or: export default (req: Request) => new Response("hi")
```

```sh
ivarabase functions deploy --project shop --dir ./functions/hello      # every file in the folder
ivarabase functions deploy --project shop --name hello --code @hello.ts
```

```ts
const { data } = await supabase.functions.invoke("hello", { body: { name: "Ankit" } });
```

You can also edit and deploy in the UI (**Functions**, with a code editor and templates), or keep functions in your repo with [config as code](CONFIG-AS-CODE.md).

## Access

- **JWT verification** is on by default: callers need the project's anon or service_role key or a user's access token, as in Supabase. Turn it off for webhooks from third parties (Stripe, GitHub) and check their signatures yourself.
- The caller's `Authorization` header reaches your function, so you can create a supabase client as that user and RLS applies.

## Environment

Each function runs with a clean environment. Nothing from the host leaks in, only these values and the project's **secrets**:

| Variable | |
|---|---|
| `SUPABASE_URL`, `IVARA_PROJECT_URL` | the project's API URL as seen from the server |
| `IVARA_PUBLIC_PROJECT_URL` | the public URL |
| `SUPABASE_ANON_KEY`, `SUPABASE_SERVICE_ROLE_KEY` | the project's keys |
| `SUPABASE_DB_URL`, `IVARA_DB_URL` | a Postgres connection string for the project |
| `IVARA_PROJECT`, `IVARA_FUNCTION` | names |

Secrets are encrypted at rest. Set them on the **Secrets** page or with `ivarabase secrets set --project shop --name STRIPE_KEY --value ...`, and functions restart to pick them up.

Permissions: network access, environment access, and reading the function's own folder. `npm:` and `jsr:` imports work, and their downloads are cached in the data directory.

## Schedules

Give a function a cron schedule (five fields, UTC) and it's called with `{"scheduled": true}`:

```sh
ivarabase functions deploy --project shop --dir ./functions/cleanup --schedule "0 3 * * *"
```

## Logs

**Functions → (function)** shows recent calls with status and duration, and the console output (`console.log` and `console.error`). Agents read the same through `functions.logs` and `functions.console`.

## Limits

Memory per function (default 128 MB) and a timeout (default 30 s) can be set per function. Functions run on the same machine as your database, so keep heavy work in a queue.
