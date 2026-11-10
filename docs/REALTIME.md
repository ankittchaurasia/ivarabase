# Realtime

`supabase.channel()` works against every project at `/realtime/v1/websocket`, using Supabase's protocol: **Postgres changes**, **broadcast** and **presence**.

```ts
const channel = supabase
  .channel("room-1")
  .on("postgres_changes", { event: "INSERT", schema: "public", table: "messages", filter: "room_id=eq.1" }, (p) => console.log(p.new))
  .on("broadcast", { event: "cursor" }, ({ payload }) => moveCursor(payload))
  .on("presence", { event: "sync" }, () => console.log(channel.presenceState()))
  .subscribe(async (status) => {
    if (status === "SUBSCRIBED") await channel.track({ user: "ankit", online_at: new Date().toISOString() });
  });

channel.send({ type: "broadcast", event: "cursor", payload: { x: 10, y: 20 } });
```

## Postgres changes

A table sends changes once it's in the `supabase_realtime` publication, as in Supabase:

```sql
alter publication supabase_realtime add table messages;
alter table messages replica identity full;   -- optional: full old rows on UPDATE/DELETE
```

You can also do this from the **Realtime** card on the project's **API** page, or with `realtime.set_table`.

- **Events:** `INSERT`, `UPDATE`, `DELETE` or `*`. **Filters:** `eq`, `neq`, `lt`, `lte`, `gt`, `gte`, `in`, for example `filter: "status=in.(open,pending)"`.
- **Payload:** `{ schema, table, commit_timestamp, eventType, new, old, errors }`. `old` has every column with `REPLICA IDENTITY FULL`, otherwise just the primary key. On tables with RLS it only ever has the primary key, as in Supabase.
- **Row level security applies.** A subscriber gets a change only if its token's role has SELECT on the table and the table's SELECT policies let it see that row. Anon gets nothing from protected tables, and each user only sees their own rows if that's what your policies say. The service_role key sees everything.
- Rows over 1 MB arrive with only their short fields, plus `errors: ["Error 413: Payload Too Large"]`.

How it works: each table in the publication gets a row trigger that records the change in an unlogged table and sends an empty `NOTIFY`. The server reads the change only while someone is subscribed to that project, checks it against each subscriber's policies, and deletes it. It needs no replication slot or `wal_level=logical`, and it works with an external Postgres. While nobody is connected, the triggers do nothing.

## Broadcast

Messages between clients on the same channel. `config: { broadcast: { self: true, ack: true } }` echoes messages back to the sender and confirms delivery. Send from a server with the REST endpoint, which is what `channel.send()` uses when it isn't subscribed:

```sh
curl -X POST https://shop.base.example.com/realtime/v1/api/broadcast \
  -H "apikey: $SERVICE_ROLE_KEY" -H "content-type: application/json" \
  -d '{"messages":[{"topic":"room-1","event":"cursor","payload":{"x":1}}]}'
```

## Presence

`channel.track(state)` and `untrack()`. Clients get `presence_state` when they join and `presence_diff` on changes, and the state is cleaned up when a client disconnects. Set the key with `config: { presence: { key: user.id } }`.

## Auth

The `apikey` (anon or service_role) and the user's access token are verified with the project's secret. Tokens for another project are refused. When a token expires mid-session, its channels close with an error, as in Supabase. supabase-js refreshes and rejoins on its own.

## Limits

| | |
|---|---|
| Connections | 500 per project, 10,000 per server |
| Channels | 100 per connection |
| Messages | 100/s per connection (bursts to 200), 2,000/s per project |

**Not supported yet:** private channels (`config: { private: true }`, which Supabase protects with policies on `realtime.messages`) need the service_role key, and `commit_timestamp` is the transaction's start time.

## Behind a proxy

WebSockets need the proxy to pass `Upgrade` and `Connection` through. `ivarabase proxy-config` includes that for nginx, and Caddy and Traefik do it on their own.
