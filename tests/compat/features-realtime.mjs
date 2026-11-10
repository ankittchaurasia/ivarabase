// Realtime suite: supabase-js channels against an IvaraBase project -
// broadcast (self, ack, REST send), presence, postgres_changes with filters,
// row level security, the supabase_realtime publication switch, auth and
// project isolation. Run with scripts/selftest.sh (SUITES=realtime); needs IV
// (server URL) and TOKEN (admin token, allow_destructive). Creates and deletes
// its own projects.
import { createClient } from "@supabase/supabase-js";
import crypto from "node:crypto";

const { IV, TOKEN } = process.env;
const P = "realtime_test";
const P2 = "realtime_test_b";
let pass = 0, fail = 0;
const check = (area, label, ok, info = "") => {
  ok ? pass++ : fail++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${area.padEnd(9)} ${label.padEnd(60)} ${ok ? "" : String(info).slice(0, 300)}`);
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function op(name, args = {}) {
  const r = await fetch(`${IV}/api/v1/ops/${name}`, { method: "POST", headers: { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" }, body: JSON.stringify(args) });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(body.error?.message || `HTTP ${r.status}`); e.code = body.error?.code; e.body = body; throw e; }
  return body;
}
const tryOp = (name, args) => op(name, args).then((r) => ({ r }), (e) => ({ e }));
const sql = (q, project = P) => op("db.query", { project, sql: q, read_only: false, confirm: true });

await tryOp("projects.delete", { project: P, confirm: true });
await tryOp("projects.delete", { project: P2, confirm: true });
await op("projects.create", { slug: P, name: "realtime" });
await op("projects.create", { slug: P2, name: "realtime b" });
const api = await op("projects.api", { project: P });
const api2 = await op("projects.api", { project: P2 });
const URL_ = `${IV}/proj/${P}`;
const URL2 = `${IV}/proj/${P2}`;
const WS_ = `${URL_.replace(/^http/, "ws")}/realtime/v1/websocket`;
const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) }; };
const clients = [];
const client = (key = api.anon_key, url = URL_) => {
  const c = createClient(url, key, { auth: { storage: mem(), autoRefreshToken: false, persistSession: true } });
  clients.push(c);
  return c;
};

// subscribe resolves with the first terminal status
const subscribe = (ch, ms = 8000) => new Promise((resolve) => {
  const t = setTimeout(() => resolve({ status: "TIMEOUT" }), ms);
  ch.subscribe((status, err) => {
    if (status === "SUBSCRIBED" || status === "CHANNEL_ERROR" || status === "TIMED_OUT" || status === "CLOSED") {
      clearTimeout(t);
      resolve({ status, err });
    }
  });
});
const until = async (fn, ms = 5000) => {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (fn()) return true; await sleep(50); }
  return fn();
};

// HS256 tokens signed with the project secret, for expiry checks
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
const sign = (claims, secret = api.jwt_secret) => {
  const u = b64({ alg: "HS256", typ: "JWT" }) + "." + b64(claims);
  return u + "." + crypto.createHmac("sha256", secret).update(u).digest("base64url");
};

// a raw Phoenix socket for protocol-level checks
async function raw(key = api.anon_key) {
  const ws = new WebSocket(`${WS_}?apikey=${encodeURIComponent(key)}&vsn=1.0.0`);
  const msgs = [];
  ws.onmessage = (e) => msgs.push(JSON.parse(e.data));
  const opened = await new Promise((r) => { ws.onopen = () => r(true); ws.onerror = () => r(false); ws.onclose = () => r(false); });
  let ref = 0;
  return {
    ws, msgs, opened,
    push(topic, event, payload = {}) { const r = String(++ref); ws.send(JSON.stringify({ topic, event, payload, ref: r, join_ref: r })); return r; },
    async reply(r, ms = 3000) { await until(() => msgs.some((m) => m.ref === r && m.event === "phx_reply"), ms); return msgs.find((m) => m.ref === r && m.event === "phx_reply"); },
  };
}

// ------------------------------------------------------------------ protocol
{
  const s = await raw();
  check("protocol", "websocket connects with the anon key", s.opened);
  const hb = await s.reply(s.push("phoenix", "heartbeat"));
  check("protocol", "heartbeat on topic phoenix gets phx_reply ok", hb?.payload?.status === "ok", JSON.stringify(hb));
  const jr = await s.reply(s.push("realtime:proto", "phx_join", { config: { broadcast: { self: true }, presence: { key: "" }, postgres_changes: [] } }));
  check("protocol", "phx_join replies ok with postgres_changes []", jr?.payload?.status === "ok" && Array.isArray(jr.payload.response.postgres_changes), JSON.stringify(jr));
  await until(() => s.msgs.some((m) => m.event === "presence_state"));
  check("protocol", "presence_state follows the join", s.msgs.some((m) => m.event === "presence_state" && m.topic === "realtime:proto"));
  const bad = await s.reply(s.push("realtime:proto2", "phx_join", { config: {}, access_token: "not.a.jwt" }));
  check("protocol", "join with an invalid access_token is refused", bad?.payload?.status === "error", JSON.stringify(bad));
  const expired = await s.reply(s.push("realtime:proto3", "phx_join", { config: {}, access_token: sign({ role: "anon", ref: P, exp: Math.floor(Date.now() / 1000) - 10 }) }));
  check("protocol", "join with an expired token is refused", expired?.payload?.status === "error" && /expired/i.test(expired.payload.response.reason), JSON.stringify(expired));
  const foreign = await s.reply(s.push("realtime:proto4", "phx_join", { config: {}, access_token: api2.anon_key }));
  check("protocol", "another project's key is refused on join", foreign?.payload?.status === "error", JSON.stringify(foreign));
  // a token that expires while joined closes the channel with a system error
  const jr2 = s.push("realtime:short", "phx_join", { config: {}, access_token: sign({ role: "anon", ref: P, exp: Math.floor(Date.now() / 1000) + 2 }) });
  const okShort = await s.reply(jr2);
  const closed = await until(() => s.msgs.some((m) => m.topic === "realtime:short" && m.event === "phx_close"), 5000);
  const sys = s.msgs.find((m) => m.topic === "realtime:short" && m.event === "system");
  check("protocol", "an expiring token closes the channel (system error + phx_close)", okShort?.payload?.status === "ok" && closed && sys?.payload?.status === "error", JSON.stringify(sys));
  const lr = await s.reply(s.push("realtime:proto", "phx_leave"));
  check("protocol", "phx_leave replies ok", lr?.payload?.status === "ok");
  const after = await s.reply(s.push("realtime:proto", "broadcast", { type: "broadcast", event: "x", payload: {} }));
  check("protocol", "pushing to a left topic is an error", after?.payload?.status === "error");
  s.ws.close();

  const r = await fetch(`${URL_}/realtime/v1/websocket?apikey=bad&vsn=1.0.0`);
  check("auth", "upgrade with an invalid apikey is 401", r.status === 401, r.status);
  const r2 = await fetch(`${URL_}/realtime/v1/websocket?apikey=${api2.anon_key}&vsn=1.0.0`);
  check("auth", "upgrade with another project's key is 401", r2.status === 401, r2.status);
  const bc = client("bad.key.value");
  const st = await subscribe(bc.channel("nope"), 4000);
  check("auth", "supabase-js with an invalid key does not subscribe", st.status !== "SUBSCRIBED", st.status);
  bc.removeAllChannels();
}

// ------------------------------------------------------------------ broadcast
{
  const a = client(), b = client();
  const got = { a: [], b: [], self: [] };
  const ca = a.channel("room1").on("broadcast", { event: "msg" }, (m) => got.a.push(m));
  const cb = b.channel("room1").on("broadcast", { event: "msg" }, (m) => got.b.push(m));
  const cs = b.channel("room1-self", { config: { broadcast: { self: true, ack: true } } }).on("broadcast", { event: "*" }, (m) => got.self.push(m));
  const [s1, s2, s3] = await Promise.all([subscribe(ca), subscribe(cb), subscribe(cs)]);
  check("broadcast", "two clients subscribe", s1.status === "SUBSCRIBED" && s2.status === "SUBSCRIBED" && s3.status === "SUBSCRIBED", `${s1.status} ${s2.status} ${s3.status} ${s1.err ?? ""}`);
  const res = await ca.send({ type: "broadcast", event: "msg", payload: { text: "hello" } });
  await until(() => got.b.length > 0);
  check("broadcast", "client B receives client A's message", got.b[0]?.payload?.text === "hello" && got.b[0]?.event === "msg", JSON.stringify(got.b));
  await sleep(300);
  check("broadcast", "self: false - the sender doesn't get its own message", res === "ok" && got.a.length === 0, JSON.stringify(got.a));
  const ack = await cs.send({ type: "broadcast", event: "ping", payload: { n: 1 } });
  check("broadcast", "ack: true - send resolves ok after the server acks", ack === "ok", ack);
  await until(() => got.self.length > 0);
  check("broadcast", "self: true - the sender receives its own message", got.self[0]?.payload?.n === 1, JSON.stringify(got.self));
  await cb.send({ type: "broadcast", event: "other", payload: {} });
  await sleep(300);
  check("broadcast", "event filter: listeners only get their event", got.a.length === 0);

  // channel.send() on an unsubscribed channel goes through POST /realtime/v1/api/broadcast
  const c = client();
  const out = await c.channel("room1").send({ type: "broadcast", event: "msg", payload: { text: "via rest" } });
  await until(() => got.b.some((m) => m.payload?.text === "via rest"));
  check("broadcast", "channel.send() without subscribing uses the REST endpoint", out === "ok" && got.b.some((m) => m.payload?.text === "via rest"), out);
  const rr = await fetch(`${URL_}/realtime/v1/api/broadcast`, { method: "POST", headers: { apikey: api.service_role_key, "content-type": "application/json" },
    body: JSON.stringify({ messages: [{ topic: "room1", event: "msg", payload: { text: "service" } }] }) });
  await until(() => got.b.some((m) => m.payload?.text === "service"));
  check("broadcast", "REST broadcast with the service_role key (202)", rr.status === 202 && got.b.some((m) => m.payload?.text === "service"), rr.status);
  const rb = await fetch(`${URL_}/realtime/v1/api/broadcast`, { method: "POST", headers: { apikey: "nope", "content-type": "application/json" }, body: "{}" });
  check("broadcast", "REST broadcast with an invalid key is 401", rb.status === 401, rb.status);

  // isolation: same channel name in another project never sees these
  const other = client(api2.anon_key, URL2);
  const gotOther = [];
  const co = other.channel("room1").on("broadcast", { event: "msg" }, (m) => gotOther.push(m));
  const so = await subscribe(co);
  await ca.send({ type: "broadcast", event: "msg", payload: { text: "p1 only" } });
  await fetch(`${URL_}/realtime/v1/api/broadcast`, { method: "POST", headers: { apikey: api.anon_key, "content-type": "application/json" },
    body: JSON.stringify({ messages: [{ topic: "room1", event: "msg", payload: { text: "p1 rest" } }] }) });
  await sleep(500);
  check("isolation", "same channel name in another project receives nothing", so.status === "SUBSCRIBED" && gotOther.length === 0, JSON.stringify(gotOther));
  for (const x of [a, b, c, other]) await x.removeAllChannels();
}

// ------------------------------------------------------------------ presence
{
  const a = client(), b = client();
  const ca = a.channel("lobby", { config: { presence: { key: "alice" } } });
  const cb = b.channel("lobby", { config: { presence: { key: "bob" } } });
  const joins = [], leaves = [];
  ca.on("presence", { event: "sync" }, () => {}).on("presence", { event: "join" }, (e) => joins.push(e)).on("presence", { event: "leave" }, (e) => leaves.push(e));
  cb.on("presence", { event: "sync" }, () => {});
  await Promise.all([subscribe(ca), subscribe(cb)]);
  await ca.track({ name: "Alice", online_at: 1 });
  const t = await cb.track({ name: "Bob" });
  check("presence", "track resolves ok", t === "ok", t);
  const synced = await until(() => Object.keys(ca.presenceState()).length === 2 && Object.keys(cb.presenceState()).length === 2);
  check("presence", "both clients see both keys in presenceState()", synced, JSON.stringify(ca.presenceState()));
  check("presence", "presence carries the tracked payload", cb.presenceState().alice?.[0]?.name === "Alice" && !!cb.presenceState().alice?.[0]?.presence_ref, JSON.stringify(cb.presenceState()));
  check("presence", "join event fired for bob", joins.some((j) => j.key === "bob"));
  await cb.track({ name: "Bob 2" });
  await until(() => ca.presenceState().bob?.[0]?.name === "Bob 2");
  check("presence", "re-track replaces the payload", ca.presenceState().bob?.length === 1 && ca.presenceState().bob[0].name === "Bob 2", JSON.stringify(ca.presenceState().bob));
  await cb.untrack();
  await until(() => !ca.presenceState().bob);
  check("presence", "untrack removes bob", !ca.presenceState().bob && leaves.some((l) => l.key === "bob"), JSON.stringify(ca.presenceState()));
  await cb.track({ name: "Bob 3" });
  await until(() => !!ca.presenceState().bob);
  // late joiner gets the full state
  const c = client();
  const cc = c.channel("lobby");
  cc.on("presence", { event: "sync" }, () => {});
  await subscribe(cc);
  await until(() => Object.keys(cc.presenceState()).length === 2);
  check("presence", "a late joiner gets presence_state with everyone", !!cc.presenceState().alice && !!cc.presenceState().bob, JSON.stringify(cc.presenceState()));
  b.realtime.disconnect();
  const gone = await until(() => !ca.presenceState().bob, 5000);
  check("presence", "disconnecting cleans up the presence", gone, JSON.stringify(ca.presenceState()));
  for (const x of [a, b, c]) await x.removeAllChannels();
}

// ------------------------------------------------------------------ postgres_changes
await sql(`create table messages (id serial primary key, room text not null, body text);
  alter table messages replica identity full;
  create table quiet (id serial primary key, body text);
  create table notes (id serial primary key, owner uuid not null, body text);
  alter table notes enable row level security;
  create policy own_notes on notes for select to authenticated using (owner = auth.uid());
  create policy add_notes on notes for insert to authenticated with check (owner = auth.uid());
  alter publication supabase_realtime add table messages, notes;`);
{
  const a = client();
  const ev = [], all = [], quiet = [];
  const ch = a.channel("db")
    .on("postgres_changes", { event: "*", schema: "public", table: "messages", filter: "room=eq.a" }, (p) => ev.push(p))
    .on("postgres_changes", { event: "INSERT", schema: "public", table: "messages" }, (p) => all.push(p))
    .on("postgres_changes", { event: "*", schema: "public", table: "quiet" }, (p) => quiet.push(p));
  const s = await subscribe(ch);
  check("changes", "subscribe with postgres_changes bindings", s.status === "SUBSCRIBED", `${s.status} ${s.err ?? ""}`);
  await sql("insert into messages (room, body) values ('a', 'first'), ('b', 'other room')");
  await sql("update messages set body = 'edited' where room = 'a'");
  await sql("delete from messages where room = 'a'");
  await sql("insert into quiet (body) values ('not published')");
  await until(() => ev.length >= 3);
  const [ins, upd, del] = ev;
  check("changes", "INSERT with filter room=eq.a", ins?.eventType === "INSERT" && ins.new.body === "first" && ins.new.room === "a" && ins.table === "messages" && ins.schema === "public", JSON.stringify(ins));
  check("changes", "payload has commit_timestamp and errors", typeof ins?.commit_timestamp === "string" && !isNaN(Date.parse(ins.commit_timestamp)) && "errors" in (ins ?? {}), JSON.stringify(ins));
  check("changes", "numbers keep their type (id is a number)", typeof ins?.new?.id === "number", typeof ins?.new?.id);
  check("changes", "UPDATE carries new and full old (replica identity full)", upd?.eventType === "UPDATE" && upd.new.body === "edited" && upd.old.body === "first", JSON.stringify(upd));
  check("changes", "DELETE carries old", del?.eventType === "DELETE" && del.old.id === ins?.new?.id && Object.keys(del.new).length === 0, JSON.stringify(del));
  check("changes", "filter excludes room b", ev.length === 3 && !ev.some((e) => e.new?.room === "b"), JSON.stringify(ev.map((e) => e.new?.room ?? e.old?.room)));
  check("changes", "unfiltered INSERT binding sees both rooms", all.length === 2, all.length);
  await sleep(800);
  check("changes", "a table not in the publication emits nothing", quiet.length === 0, JSON.stringify(quiet));

  // more filter operators
  const f = { in: [], gt: [], neq: [] };
  const ch2 = a.channel("db-ops")
    .on("postgres_changes", { event: "INSERT", schema: "public", table: "messages", filter: "room=in.(x,y)" }, (p) => f.in.push(p.new.room))
    .on("postgres_changes", { event: "INSERT", schema: "public", table: "messages", filter: "id=gt.1000" }, (p) => f.gt.push(p.new.id))
    .on("postgres_changes", { event: "INSERT", schema: "public", table: "messages", filter: "room=neq.x" }, (p) => f.neq.push(p.new.room));
  await subscribe(ch2);
  await sql("insert into messages (id, room) values (5, 'x'), (1001, 'y'), (1002, 'z')");
  await until(() => f.in.length >= 2 && f.gt.length >= 2 && f.neq.length >= 2);
  await sleep(300);
  check("changes", "in / gt / neq filters", f.in.join() === "x,y" && f.gt.join() === "1001,1002" && f.neq.join() === "y,z", JSON.stringify(f));

  const bad = client().channel("badfilter").on("postgres_changes", { event: "*", schema: "public", table: "messages", filter: "room=like.a%" }, () => {});
  const sb = await subscribe(bad, 4000);
  check("changes", "an unsupported filter operator is rejected", sb.status === "CHANNEL_ERROR", sb.status);

  // the publication is the switch: turn quiet on through the op, then off with SQL
  const on = await tryOp("realtime.set_table", { project: P, table: "quiet", enabled: true });
  check("changes", "realtime.set_table adds a table to the publication", on.r?.tables?.some((t) => t.table === "quiet" && t.watched), JSON.stringify(on.r ?? on.e?.message));
  await sql("insert into quiet (body) values ('now published')");
  await until(() => quiet.length > 0);
  check("changes", "after adding it, the table emits changes", quiet[0]?.new?.body === "now published", JSON.stringify(quiet));
  await sql("alter publication supabase_realtime drop table quiet");
  await sql("insert into quiet (body) values ('dropped again')");
  await sleep(800);
  check("changes", "alter publication ... drop table stops it", quiet.length === 1, quiet.length);

  const st = await op("realtime.status", { project: P });
  check("ops", "realtime.status lists connections, channels and tables", st.connections >= 1 && st.channels.some((c) => c.topic === "db") && st.tables.some((t) => t.table === "messages") && st.postgres_changes?.listening === true, JSON.stringify(st).slice(0, 300));
  const ov = await op("realtime.overview", {});
  check("ops", "realtime.overview counts the project", ov.projects.some((p) => p.project === P && p.connections >= 1), JSON.stringify(ov).slice(0, 300));
  await a.removeAllChannels();
}

// ------------------------------------------------------------------ RLS
{
  const email = (n) => `${n}.${Date.now()}@example.com`;
  const alice = client(), bob = client(), anon = client(), svc = client(api.service_role_key);
  const ra = await alice.auth.signUp({ email: email("alice"), password: "secret-123" });
  const rb = await bob.auth.signUp({ email: email("bob"), password: "secret-123" });
  const aliceId = ra.data.user?.id, bobId = rb.data.user?.id;
  check("rls", "two users signed up", !!aliceId && !!bobId, ra.error?.message ?? rb.error?.message);
  alice.realtime.setAuth(ra.data.session.access_token);
  bob.realtime.setAuth(rb.data.session.access_token);
  const got = { alice: [], bob: [], anon: [], svc: [] };
  const sub = (c, k) => subscribe(c.channel("notes-" + k).on("postgres_changes", { event: "*", schema: "public", table: "notes" }, (p) => got[k].push(p)));
  const ss = await Promise.all([sub(alice, "alice"), sub(bob, "bob"), sub(anon, "anon"), sub(svc, "svc")]);
  check("rls", "every role subscribes", ss.every((s) => s.status === "SUBSCRIBED"), ss.map((s) => s.status).join());
  await sql(`insert into notes (owner, body) values ('${aliceId}', 'for alice'), ('${bobId}', 'for bob'), ('${aliceId}', 'alice 2')`);
  await sql(`update notes set body = 'bob edited' where owner = '${bobId}'`);
  await until(() => got.alice.length >= 2 && got.bob.length >= 2 && got.svc.length >= 4);
  await sleep(500);
  check("rls", "alice only receives her own rows", got.alice.length === 2 && got.alice.every((p) => p.new.owner === aliceId), JSON.stringify(got.alice.map((p) => p.new.body)));
  check("rls", "bob only receives his own rows (insert + update)", got.bob.length === 2 && got.bob.every((p) => p.new.owner === bobId) && got.bob[1].eventType === "UPDATE", JSON.stringify(got.bob.map((p) => p.new.body)));
  check("rls", "anon receives nothing from the protected table", got.anon.length === 0, JSON.stringify(got.anon));
  check("rls", "service_role (bypasses RLS) receives everything", got.svc.length === 4, got.svc.length);
  check("rls", "UPDATE old on an RLS table holds only the primary key", got.bob[1] && Object.keys(got.bob[1].old).join() === "id", JSON.stringify(got.bob[1]?.old));
  // a write through the REST API (as the authenticated role) is captured too
  const ins = await alice.from("notes").insert({ owner: aliceId, body: "via rest" });
  await until(() => got.alice.some((p) => p.new.body === "via rest"));
  await sleep(300);
  check("rls", "a REST insert by alice reaches alice, not bob or anon", !ins.error && got.alice.some((p) => p.new.body === "via rest") && got.bob.length === 2 && got.anon.length === 0, ins.error?.message ?? JSON.stringify(got.bob.map((p) => p.new.body)));
  // DELETEs are judged against the old row too
  await sql(`delete from notes where body = 'for alice'`);
  await until(() => got.alice.some((p) => p.eventType === "DELETE") && got.svc.some((p) => p.eventType === "DELETE"));
  await sleep(300);
  const ad = got.alice.find((p) => p.eventType === "DELETE");
  check("rls", "DELETE reaches the row's owner (old = primary key only)", ad && Object.keys(ad.old).join() === "id", JSON.stringify(ad));
  check("rls", "DELETE of alice's row is not sent to bob or anon", !got.bob.some((p) => p.eventType === "DELETE") && got.anon.length === 0, JSON.stringify(got.bob.map((p) => p.eventType)));
  for (const x of [alice, bob, anon, svc]) await x.removeAllChannels();
}

// ------------------------------------------------------------------ backup / restore
{
  const PR = P + "_r";
  await tryOp("projects.delete", { project: PR, confirm: true });
  const bk = await tryOp("backups.create", { project: P, verify: true });
  const bid = bk.r?.id ?? bk.r?.backup?.id;
  const rs = await tryOp("backups.restore", { id: bid, mode: "new", target_project: PR });
  check("restore", "backup and restore a realtime project", !!bid && !rs.e, (bk.e?.message ?? "") + (rs.e?.message ?? ""));
  const apiR = await op("projects.api", { project: PR });
  const st = await op("realtime.status", { project: PR });
  check("restore", "the restored publication keeps its tables", st.tables.some((t) => t.table === "messages" && t.watched), JSON.stringify(st.tables));
  const c = client(apiR.service_role_key, `${IV}/proj/${PR}`);
  const got = [];
  const s = await subscribe(c.channel("db").on("postgres_changes", { event: "INSERT", schema: "public", table: "messages" }, (p) => got.push(p)));
  await sql("insert into messages (room, body) values ('r', 'after restore')", PR);
  await until(() => got.length > 0);
  check("restore", "postgres_changes work in the restored project", s.status === "SUBSCRIBED" && got[0]?.new?.body === "after restore", `${s.status} ${JSON.stringify(got)}`);
  await c.removeAllChannels();
  if (!process.env.KEEP) await tryOp("projects.delete", { project: PR, confirm: true });
}

for (const c of clients) { try { c.realtime.disconnect(); } catch {} }
if (!process.env.KEEP) {
  await tryOp("projects.delete", { project: P, confirm: true });
  await tryOp("projects.delete", { project: P2, confirm: true });
}
console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
