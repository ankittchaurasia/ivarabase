// Database webhooks suite: payloads, signatures, retries, function targets,
// disabled hooks, non-blocking inserts, backups and restores.
// Run with scripts/selftest.sh (SUITES=webhooks); needs IV (server URL) and
// TOKEN (admin token, allow_destructive). The server must allow private
// webhook targets (IVARA_WEBHOOKS_ALLOW_PRIVATE=1, the sink is on 127.0.0.1)
// and short retries (IVARA_DEV_WEBHOOK_RETRY_SCALE=0.02). Creates and deletes
// its own projects.
import { createClient } from "@supabase/supabase-js";
import crypto from "node:crypto";
import http from "node:http";

const { IV, TOKEN } = process.env;
const P = "webhooks_test";
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

// the receiver: /ok answers 200, /flaky fails once with 500, /slow takes 3 s
const hits = [];
let flaky = 0;
const sink = http.createServer(async (req, res) => {
  let body = "";
  for await (const c of req) body += c;
  const hit = { method: req.method, path: req.url, headers: req.headers, body, at: Date.now() };
  try { hit.json = JSON.parse(body); } catch {}
  hits.push(hit);
  if (req.url.startsWith("/slow")) await sleep(3000);
  const status = req.url.startsWith("/flaky") && flaky++ === 0 ? 500 : 200;
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify({ received: true, status }));
});
// SINK_HOST: how the server reaches this machine (e.g. host.lima.internal in Docker)
const SINK_HOST = process.env.SINK_HOST || "127.0.0.1";
await new Promise((r) => sink.listen(0, SINK_HOST === "127.0.0.1" ? "127.0.0.1" : "0.0.0.0", r));
const SINK = `http://${SINK_HOST}:${sink.address().port}`;
async function waitHit(pred, ms = 10000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    const h = hits.find(pred);
    if (h) return h;
    await sleep(100);
  }
  return null;
}
const verify = (h, secret) => {
  const ts = h.headers["x-ivarabase-timestamp"];
  const want = "sha256=" + crypto.createHmac("sha256", secret).update(`${ts}.${h.body}`).digest("hex");
  const got = h.headers["x-ivarabase-signature"] || "";
  return got.length === want.length && crypto.timingSafeEqual(Buffer.from(got), Buffer.from(want)) && Math.abs(Date.now() / 1000 - +ts) < 300;
};

for (const p of [P, P + "_r"]) await tryOp("projects.delete", { project: p, confirm: true });
await op("projects.create", { slug: P, name: "webhooks" });
const api = await op("projects.api", { project: P });
const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) }; };
const client = (proj, key) => createClient(`${IV}/proj/${proj}`, key, { auth: { storage: mem(), autoRefreshToken: false, persistSession: false } });
const admin = client(P, api.service_role_key);
const anon = client(P, api.anon_key);

await sql(`create table orders (id serial primary key, item text not null, qty int not null default 1);
  alter table orders enable row level security;
  create policy anon_insert on orders for insert to anon with check (true);
  create table payments (id serial primary key, amount int);
  create table slow_t (id serial primary key, v text);
  create table fn_t (id serial primary key, v text);
  create table off_t (id serial primary key, v text);`);
await sql("notify pgrst, 'reload schema'");
await sleep(500);

// ------------------------------------------------------------------ setup / validation
let r = await tryOp("webhooks.create", { project: P, name: "bad", table: "orders", url: "ftp://example.com/x" });
check("setup", "non-http URL is refused", r.e?.code === "invalid_url", r.e?.code ?? "accepted");
r = await tryOp("webhooks.create", { project: P, name: "bad", table: "nope", url: SINK + "/ok" });
check("setup", "unknown table is refused", r.e?.code === "table_not_found", r.e?.code ?? "accepted");
r = await tryOp("webhooks.create", { project: P, name: "bad", table: "orders", function: "does-not-exist" });
check("setup", "unknown function is refused", r.e?.code === "function_not_found", r.e?.code ?? "accepted");

const all = await op("webhooks.create", { project: P, name: "orders-all", table: "public.orders", events: ["INSERT", "UPDATE", "DELETE"],
  url: SINK + "/ok", headers: { "X-Custom": "abc" } });
check("setup", "create returns the signing secret", /^whsec_/.test(all.secret ?? ""), JSON.stringify(all));
const priv = await sql(`select has_schema_privilege('anon','ivarabase_hooks','usage') or has_schema_privilege('authenticated','ivarabase_hooks','usage')
  or has_schema_privilege('service_role','ivarabase_hooks','usage') as leak`);
check("setup", "API roles can't see the ivarabase_hooks schema", priv.rows?.[0]?.[0] === false || priv.rows?.[0]?.leak === false, JSON.stringify(priv.rows));

// ------------------------------------------------------------------ payloads
{
  const ins = await anon.from("orders").insert({ item: "anon-widget", qty: 2 });
  check("payload", "insert through the REST API as anon works", !ins.error, ins.error?.message);
  let h = await waitHit((x) => x.path === "/ok" && x.json?.type === "INSERT" && x.json?.record?.item === "anon-widget");
  check("payload", "INSERT delivered with Supabase-style payload", h && h.json.table === "orders" && h.json.schema === "public" && h.json.old_record === null && h.json.record.qty === 2,
    JSON.stringify(h?.json));
  check("payload", "signature verifies (HMAC of timestamp.body)", h && verify(h, all.secret), JSON.stringify(h?.headers));
  check("payload", "custom header and event id are sent", h?.headers["x-custom"] === "abc" && /^\d+$/.test(h?.headers["x-ivarabase-event-id"] ?? ""), JSON.stringify(h?.headers));
  check("payload", "wrong secret does not verify", h && !verify(h, "whsec_wrong"));

  await sql("update orders set qty = 5 where item = 'anon-widget'");
  h = await waitHit((x) => x.path === "/ok" && x.json?.type === "UPDATE");
  check("payload", "UPDATE has record and old_record", h?.json.record?.qty === 5 && h?.json.old_record?.qty === 2, JSON.stringify(h?.json));
  await sql("delete from orders where item = 'anon-widget'");
  h = await waitHit((x) => x.path === "/ok" && x.json?.type === "DELETE");
  check("payload", "DELETE has old_record and record null", h?.json.record === null && h?.json.old_record?.item === "anon-widget", JSON.stringify(h?.json));

  const before = hits.length;
  await sql("insert into orders(item) values ('rolled-back'); select 1/0;").catch(() => {}); // fails, so the insert rolls back
  await sql("insert into orders(item) values ('x'); delete from orders where item = 'x';"); // a marker after it
  await waitHit((x) => x.json?.type === "DELETE" && x.json?.old_record?.item === "x");
  check("payload", "rolled-back rows send nothing", !hits.slice(before).some((x) => x.json?.record?.item === "rolled-back"));
}

// ------------------------------------------------------------------ retries
{
  const hook = await op("webhooks.create", { project: P, name: "payments-flaky", table: "payments", events: ["INSERT"], url: SINK + "/flaky" });
  await sql("insert into payments(amount) values (42)");
  const second = await waitHit((x) => x.path === "/flaky" && x.headers["x-ivarabase-attempt"] === "2", 20000);
  const first = hits.find((x) => x.path === "/flaky" && x.headers["x-ivarabase-attempt"] === "1");
  check("retry", "a 500 is retried and the retry succeeds", !!first && !!second, `${hits.filter((x) => x.path === "/flaky").length} hits`);
  check("retry", "retry keeps the event id and is signed", first?.headers["x-ivarabase-event-id"] === second?.headers["x-ivarabase-event-id"] && second && verify(second, hook.secret));
  await sleep(300);
  const log = await op("webhooks.deliveries", { project: P, name: "payments-flaky" });
  const ok = log.find((d) => d.attempt === 2), bad = log.find((d) => d.attempt === 1);
  check("retry", "every attempt is logged (status, duration, response)", bad?.status_code === 500 && !bad.ok && ok?.status_code === 200 && ok.ok && ok.event_status === "delivered" && ok.response.includes("received"),
    JSON.stringify(log).slice(0, 300));
}

// ------------------------------------------------------------------ slow endpoint
{
  await op("webhooks.create", { project: P, name: "slow", table: "slow_t", events: ["INSERT"], url: SINK + "/slow", timeout_ms: 10000 });
  const t0 = Date.now();
  const ins = await admin.from("slow_t").insert({ v: "s" });
  const took = Date.now() - t0;
  check("async", "insert returns without waiting for a 3 s endpoint", !ins.error && took < 1500, `${took} ms ${ins.error?.message ?? ""}`);
  const h = await waitHit((x) => x.path === "/slow");
  check("async", "the slow endpoint still gets the event", !!h);
}

// ------------------------------------------------------------------ function target
{
  await op("functions.deploy", { project: P, name: "on-row", verify_jwt: true, code: `export default async (req) => {
    const body = await req.json();
    const tok = (req.headers.get("authorization") || "").split(" ")[1] || "";
    const role = JSON.parse(atob(tok.split(".")[1].replace(/-/g, "+").replace(/_/g, "/"))).role;
    return Response.json({ got: body.type, v: body.record?.v, role, signed: !!req.headers.get("x-ivarabase-signature") });
  }` });
  await op("webhooks.create", { project: P, name: "to-function", table: "fn_t", events: ["INSERT"], function: "on-row" });
  await sql("insert into fn_t(v) values ('hello-fn')");
  let d = null;
  for (let i = 0; i < 100 && !d; i++) {
    await sleep(200);
    d = (await op("webhooks.deliveries", { project: P, name: "to-function" }))[0];
  }
  let resp = {};
  try { resp = JSON.parse(d?.response ?? "{}"); } catch {}
  check("function", "function target receives the payload", d?.ok && resp.got === "INSERT" && resp.v === "hello-fn", JSON.stringify(d));
  check("function", "function is called with the service_role key", resp.role === "service_role" && resp.signed === true, JSON.stringify(resp));
}

// ------------------------------------------------------------------ disabled
{
  await op("webhooks.create", { project: P, name: "off", table: "off_t", events: ["INSERT"], url: SINK + "/off", enabled: false });
  await sql("insert into off_t(v) values ('a')");
  await sleep(1500);
  check("disabled", "a disabled webhook sends nothing", !hits.some((x) => x.path === "/off"));
  await op("webhooks.update", { project: P, name: "off", enabled: true });
  await sql("insert into off_t(v) values ('b')");
  const h = await waitHit((x) => x.path === "/off");
  check("disabled", "enabling it delivers new rows only", h?.json?.record?.v === "b" && hits.filter((x) => x.path === "/off").length === 1, JSON.stringify(h?.json));
  await op("webhooks.update", { project: P, name: "off", enabled: false });
  const n = hits.filter((x) => x.path === "/off").length;
  await sql("insert into off_t(v) values ('c')");
  await sleep(1500);
  check("disabled", "disabling again stops deliveries", hits.filter((x) => x.path === "/off").length === n);
}

// ------------------------------------------------------------------ test / redeliver / list
{
  const t = await op("webhooks.test", { project: P, name: "orders-all" });
  const h = await waitHit((x) => x.headers["x-ivarabase-test"] === "1");
  check("ops", "webhooks.test sends a sample and returns the result", t.ok && t.status_code === 200 && !!h && h.json?.table === "orders", JSON.stringify(t).slice(0, 200));
  const log = await op("webhooks.deliveries", { project: P, name: "orders-all" });
  const ins = log.find((d) => d.type === "INSERT" && !d.test);
  const n = hits.filter((x) => x.headers["x-ivarabase-event-id"] === String(ins?.event_id)).length;
  r = await tryOp("webhooks.redeliver", { project: P, event_id: ins?.event_id });
  await sleep(1500);
  check("ops", "webhooks.redeliver sends the event again", r.r?.queued && hits.filter((x) => x.headers["x-ivarabase-event-id"] === String(ins?.event_id)).length === n + 1,
    r.e?.message ?? n);
  const list = await op("webhooks.list", { project: P });
  const a = list.find((w) => w.name === "orders-all");
  check("ops", "list shows state, table, target and counts", a?.state === "active" && a.table === "public.orders" && a.url === SINK + "/ok" && a.delivered_24h >= 3 && !a.secret,
    JSON.stringify(a));
  check("ops", "list hides secrets unless asked", (await op("webhooks.list", { project: P, reveal_secrets: true })).find((w) => w.name === "orders-all")?.secret === all.secret);
}

// ------------------------------------------------------------------ dropped table
{
  await sql("drop table payments");
  const w = (await op("webhooks.list", { project: P })).find((x) => x.name === "payments-flaky");
  check("drop", "dropping a watched table marks the hook table_missing", w?.state === "table_missing", JSON.stringify(w));
  await sql("insert into orders(item) values ('after-drop')");
  check("drop", "other webhooks keep working", !!(await waitHit((x) => x.json?.record?.item === "after-drop")));
}

// ------------------------------------------------------------------ backups
{
  const bk = await op("backups.create", { project: P });
  const bid = bk.id ?? bk.backup?.id;
  const rs = await tryOp("backups.restore", { id: bid, mode: "new", target_project: P + "_r" });
  check("backup", "restore as a new project", rs.r?.restored === true, rs.e?.message);
  const list = await tryOp("webhooks.list", { project: P + "_r" });
  check("backup", "webhooks come back with the restored project", list.r?.find((w) => w.name === "orders-all")?.state === "active", JSON.stringify(list.r ?? list.e?.message).slice(0, 300));
  await sql("insert into orders(item) values ('in-restored')", P + "_r");
  const h = await waitHit((x) => x.json?.record?.item === "in-restored");
  check("backup", "webhooks fire in the restored project", !!h && verify(h, all.secret), JSON.stringify(h?.headers ?? {}));
  const old = hits.filter((x) => x.json?.record?.item === "after-drop").length;
  await sleep(800);
  check("backup", "events queued in the backup are not sent again", hits.filter((x) => x.json?.record?.item === "after-drop").length === old);

  const rp = await tryOp("backups.restore", { id: bid, mode: "replace", target_project: P, confirm: true });
  check("backup", "restore replacing the project", rp.r?.restored === true, rp.e?.message);
  await sql("insert into orders(item) values ('after-replace')");
  check("backup", "webhooks fire after a replace restore", !!(await waitHit((x) => x.json?.record?.item === "after-replace")));
}

// ------------------------------------------------------------------ delete
{
  r = await tryOp("webhooks.delete", { project: P, name: "orders-all" });
  check("delete", "delete without confirm returns a preview", r.e?.code === "confirmation_required" && r.e.body?.error?.details?.webhook === "orders-all", r.e?.code);
  r = await tryOp("webhooks.delete", { project: P, name: "orders-all", confirm: true });
  check("delete", "delete with confirm", r.r?.deleted === "orders-all", r.e?.message);
  const trg = await sql("select count(*)::int as n from pg_trigger where tgname like 'ivarabase_hook_%' and tgrelid = 'orders'::regclass");
  check("delete", "its trigger is removed", (trg.rows?.[0]?.[0] ?? trg.rows?.[0]?.n) === 0, JSON.stringify(trg.rows));
  const n = hits.length;
  await sql("insert into orders(item) values ('after-delete')");
  await sleep(1200);
  check("delete", "nothing is sent after delete", !hits.slice(n).some((x) => x.json?.record?.item === "after-delete"));
}

if (!process.env.KEEP) for (const p of [P, P + "_r"]) await tryOp("projects.delete", { project: p, confirm: true });
sink.close();
console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
