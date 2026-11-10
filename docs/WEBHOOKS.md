# Database webhooks

When rows in a table are inserted, updated or deleted, IvaraBase calls a URL or one of the project's functions, like Supabase Database Webhooks. Set them up on the project's **Webhooks** page, or with the `webhooks.*` operations from an agent or the CLI:

```sh
ivarabase webhooks create --project shop --name order-created --table orders --events '["INSERT"]' \
  --url https://example.com/hooks/orders
ivarabase webhooks create --project shop --name receipt --table orders --events '["INSERT"]' --function send-receipt
```

## Payload

Same shape as Supabase:

```json
{ "type": "INSERT", "table": "orders", "schema": "public", "record": { "id": 7, "total": 42 }, "old_record": null }
```

`UPDATE` has both `record` and `old_record`. `DELETE` has only `old_record`.

## Targets

- **URL:** POST (or PUT/PATCH) to any `https` or `http` endpoint, with your own headers (for example an `Authorization` header) and a timeout from 0.5 to 60 seconds (default 10).
- **Function:** a function in the same project, called with the project's service_role key. Inside it, `await req.json()` is the payload above.

## Verifying requests

Each hook has a signing secret (`whsec_...`), shown when it's created and in the UI. Rotate it with `rotate_secret`. Every request carries:

| Header | |
|---|---|
| `X-Ivarabase-Timestamp` | unix seconds |
| `X-Ivarabase-Signature` | `sha256=` + hex HMAC-SHA256 of `"<timestamp>.<raw body>"` with the secret |
| `X-Ivarabase-Event-Id` | the same on every retry of one event: use it to ignore duplicates |

```ts
import crypto from "node:crypto";

function verify(req, rawBody, secret) {
  const ts = req.headers["x-ivarabase-timestamp"];
  const expected = "sha256=" + crypto.createHmac("sha256", secret).update(`${ts}.${rawBody}`).digest("hex");
  const ok = crypto.timingSafeEqual(Buffer.from(expected), Buffer.from(req.headers["x-ivarabase-signature"] ?? ""));
  return ok && Math.abs(Date.now() / 1000 - Number(ts)) < 300; // reject old requests (replays)
}
```

## Delivery

- **The write never waits.** A trigger records the event in the project's database (in the `ivarabase_hooks` schema, which the API roles can't read) and IvaraBase delivers it right after. A rolled-back transaction sends nothing.
- **At least once, with retries.** A non-2xx response, a timeout or a connection error is retried 5 times over about an hour (after 1, 5, 15 and 40 minutes, with jitter). Make receivers idempotent with `X-Ivarabase-Event-Id`.
- **Order:** one event at a time per hook, so events usually arrive in order.
- **Log:** every attempt is kept with its status, duration, an excerpt of the response and any error, for 7 days or up to 1000 events per hook. **Webhooks → Deliveries** shows them, and **Redeliver** sends an event again. **Test** sends a sample payload.
- **Disabling** a hook stops new events. Events still waiting are delivered.

## Safety

URLs that point at the server itself or at private networks (`127.0.0.1`, `10.x`, `192.168.x`, link-local, cloud metadata addresses) are refused, both when the hook is saved and when connecting, which also covers DNS tricks. To call services on your own private network, allow it for the whole server with `"webhooks_allow_private_targets": true` in `config.json` or `IVARA_WEBHOOKS_ALLOW_PRIVATE=1`. Redirects are not followed.

## Backups and restores

Hooks are part of the project's database, so they're in every backup and come back with restores (as a new project or replacing one). Events that were still queued when the backup was taken are marked cancelled on restore rather than sent again.

## Limits

- Row-level only: `TRUNCATE` doesn't fire hooks.
- If a watched table is dropped, the hook shows **table missing**. If the table is created again, edit the hook (or restart) to attach it.
