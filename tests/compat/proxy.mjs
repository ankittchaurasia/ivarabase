// Behind-a-reverse-proxy checks (tls.mode proxy). Run with scripts/selftest.sh,
// which starts a throwaway instance configured like a VPS behind nginx:
// domain ivara.test, public_url https://ivara.test:<PROXYPORT>, the proxy's
// wildcard certificate files for Postgres TLS.
//
// The nginx stand-in below terminates TLS and forwards exactly the headers
// `ivarabase proxy-config` tells nginx to send. A test-only header lets one
// machine pretend to be many visitors ($remote_addr).
import { createClient } from "@supabase/supabase-js";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import https from "node:https";

const { IV, TOKEN, OWNER_PW, PROXYPORT, CERT, BIN, DATA, PSQL } = process.env;
const D = "ivara.test", PP = +PROXYPORT, P = "proxytest";
const PANEL = `https://${D}:${PP}`;
const cert = fs.readFileSync(`${CERT}/fullchain.pem`), key = fs.readFileSync(`${CERT}/privkey.pem`);
let pass = 0, fail = 0;
const check = (label, ok, info = "") => { ok ? pass++ : fail++; console.log(`${ok ? "PASS" : "FAIL"}  ${label.padEnd(66)} ${ok ? "" : String(info).slice(0, 300)}`); };

// ---- nginx stand-in
const up = new URL(IV);
const proxy = https.createServer({ cert, key }, (req, res) => {
  const remote = req.headers["x-test-remote-addr"] || req.socket.remoteAddress.replace(/^::ffff:/, "");
  const h = { ...req.headers };
  delete h["x-test-remote-addr"];
  h.host = (req.headers.host || "").replace(/:\d+$/, "");                          // proxy_set_header Host $host
  h["x-real-ip"] = remote;                                                          // X-Real-IP $remote_addr
  h["x-forwarded-for"] = h["x-forwarded-for"] ? `${h["x-forwarded-for"]}, ${remote}` : remote; // $proxy_add_x_forwarded_for
  h["x-forwarded-proto"] = "https";                                                  // X-Forwarded-Proto $scheme
  const p = http.request({ host: up.hostname, port: up.port, method: req.method, path: req.url, headers: h }, (r) => {
    res.writeHead(r.statusCode, r.headers);
    r.pipe(res);
  });
  p.on("error", (e) => { res.writeHead(502); res.end(String(e)); });
  req.pipe(p);
});
await new Promise((r) => proxy.listen(PP, "127.0.0.1", r));

// ---- fetch through the proxy as a visitor from `remote` (no DNS needed for *.ivara.test)
const via = (remote, extra = {}) => async (input, init = {}) => {
  const rq = new Request(input, init);
  const u = new URL(rq.url);
  const body = rq.body ? Buffer.from(await rq.arrayBuffer()) : null;
  const headers = { ...Object.fromEntries(rq.headers), ...extra, host: u.host };
  if (remote) headers["x-test-remote-addr"] = remote;
  if (body) headers["content-length"] = String(body.length);
  return new Promise((resolve, reject) => {
    const r = https.request({ host: "127.0.0.1", port: PP, servername: u.hostname, ca: cert, method: rq.method, path: u.pathname + u.search, headers }, (res) => {
      const chunks = [];
      res.on("data", (c) => chunks.push(c)).on("end", () => {
        const h = new Headers();
        for (const [k, v] of Object.entries(res.headers)) (Array.isArray(v) ? v : [v]).forEach((x) => h.append(k, x));
        const nobody = [101, 204, 205, 304].includes(res.statusCode);
        resolve(new Response(nobody ? null : Buffer.concat(chunks), { status: res.statusCode, headers: h }));
      });
    });
    r.on("error", reject);
    if (body) r.write(body);
    r.end();
  });
};
const pf = via("198.51.100.10");
const admin = { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" };
const opRaw = async (name, args, f = fetch, base = IV) => {
  const r = await f(`${base}/api/v1/ops/${name}`, { method: "POST", headers: admin, body: JSON.stringify(args) });
  return { status: r.status, body: await r.json() };
};
const op = async (name, args, f, base) => { const r = await opRaw(name, args, f, base); if (r.status >= 300) throw new Error(name + ": " + JSON.stringify(r.body)); return r.body.result ?? r.body; };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

try {
  // ---- panel through the proxy
  let r = await pf(`${PANEL}/healthz`);
  check("panel reachable through the proxy (Host kept)", r.status === 200, r.status);
  check("HSTS sent when the proxy says https", /max-age/.test(r.headers.get("strict-transport-security") || ""), [...r.headers].join(" "));
  r = await fetch(`${IV}/healthz`);
  check("no HSTS on the plain local port", !r.headers.get("strict-transport-security"));
  let info = await op("system.info", {}, pf, PANEL);
  for (let i = 0; i < 20 && !info.tls?.postgres_tls; i++) { await sleep(500); info = await op("system.info", {}, pf, PANEL); }
  check("system.info: tls mode proxy, panel https://<domain>", info.tls?.mode === "proxy" && info.endpoints?.ui === PANEL, JSON.stringify({ tls: info.tls, ui: info.endpoints?.ui }));
  check("Postgres TLS installed from the proxy's certificate files", info.tls?.postgres_tls === true, JSON.stringify(info.tls));
  const pc = await op("system.proxy_config", { server: "nginx" }, pf, PANEL);
  check("system.proxy_config renders nginx for the domain + wildcard", pc.config?.includes(`server_name ${D} *.${D};`) && pc.config.includes(`proxy_pass http://${up.host};`) && !pc.warning, pc.config?.slice(0, 120));
  const pcc = await op("system.proxy_config", { server: "caddy" }, pf, PANEL);
  check("system.proxy_config renders Caddy with the on-demand ask URL", pcc.config?.includes(`ask http://${up.host}/_tls/ask`), pcc.config?.slice(0, 120));

  // ---- Caddy on-demand TLS ask
  const ask = async (d) => (await fetch(`${IV}/_tls/ask?domain=${d}`)).status;
  await opRaw("projects.delete", { project: P, confirm: true });
  const created = await op("projects.create", { slug: P, name: "Proxy test" });
  check("tls ask: panel, db., existing project allowed", (await ask(D)) === 200 && (await ask("db." + D)) === 200 && (await ask(`${P}.${D}`)) === 200);
  check("tls ask: not answered for visitors forwarded by the proxy", (await pf(`${PANEL}/_tls/ask?domain=${D}`)).status === 404);
  check("tls ask: unknown project and foreign domains refused", (await ask("nope." + D)) === 403 && (await ask("example.com")) === 403 && (await ask("a.b." + D)) === 403);

  // ---- project on its subdomain through the proxy
  const api = await op("projects.api", { project: P });
  check("project URL is https://<project>.<domain>:<proxy port>", api.url === `https://${P}.${D}:${PP}`, api.url);
  await op("db.query", { project: P, read_only: false, sql: "create table notes (id serial primary key, body text)" });
  const svc = createClient(api.url, api.service_role_key, { auth: { persistSession: false }, global: { fetch: pf } });
  let q = await svc.from("notes").insert({ body: "hello via proxy" }).select();
  check("REST insert via subdomain", !q.error && q.data?.[0]?.body === "hello via proxy", JSON.stringify(q.error));
  q = await svc.from("notes").select("body");
  check("REST select via subdomain", !q.error && q.data?.length === 1, JSON.stringify(q.error));

  // ---- auth: rate limits follow the real visitor, not nginx's 127.0.0.1
  const anon = (remote, extra) => createClient(api.url, api.anon_key, { auth: { persistSession: false, autoRefreshToken: false }, global: { fetch: via(remote, extra) } });
  const su = await anon("198.51.100.20").auth.signUp({ email: "visitor@example.com", password: "correct-horse-1" });
  check("auth sign-up via subdomain", !su.error && !!su.data.user, JSON.stringify(su.error));
  const A = "198.51.100.21", B2 = "198.51.100.22";
  let limited = false;
  for (let i = 0; i < 35 && !limited; i++) {
    const { error } = await anon(A).auth.signInWithPassword({ email: "visitor@example.com", password: "wrong-" + i });
    limited = error?.status === 429;
  }
  check("visitor A hits the auth rate limit", limited);
  let s = await anon(B2).auth.signInWithPassword({ email: "visitor@example.com", password: "correct-horse-1" });
  check("visitor B is not affected by A's limit", !s.error && !!s.data.session, JSON.stringify(s.error));
  s = await anon(A, { "x-forwarded-for": "203.0.113.77" }).auth.signInWithPassword({ email: "visitor@example.com", password: "correct-horse-1" });
  check("A can't escape the limit with a forged X-Forwarded-For", s.error?.status === 429, JSON.stringify(s.error));
  const audit = await op("db.query", { project: P, read_only: true, sql: "select count(*)::int as n from auth.audit_log_entries where ip_address = '198.51.100.22'" }).catch((e) => ({ error: e.message }));
  const n = audit.rows?.[0]?.n ?? audit.rows?.[0]?.[0];
  check("auth audit log records the visitor's IP", n >= 1, JSON.stringify(audit).slice(0, 200));

  // ---- storage: uploaded HTML can't run script on our origins
  await svc.storage.createBucket("pub", { public: true });
  const up1 = await svc.storage.from("pub").upload("page.html", Buffer.from("<script>alert(1)</script>"), { contentType: "text/html", upsert: true });
  check("storage upload via subdomain", !up1.error, JSON.stringify(up1.error));
  const pubURL = svc.storage.from("pub").getPublicUrl("page.html").data.publicUrl;
  r = await pf(pubURL);
  check("public object on the project subdomain is sandboxed", r.status === 200 && r.headers.get("content-security-policy") === "sandbox", `${r.status} ${r.headers.get("content-security-policy")}`);
  r = await pf(`${PANEL}/proj/${P}/storage/v1/object/public/pub/page.html`);
  check("same object via the panel origin is sandboxed", r.status === 200 && /sandbox/.test(r.headers.get("content-security-policy") || ""), `${r.status} ${r.headers.get("content-security-policy")}`);
  const up2 = await svc.storage.from("pub").upload("big.bin", Buffer.alloc(3 << 20, 7), { contentType: "application/octet-stream", upsert: true });
  check("3 MB upload streams through the proxy", !up2.error, JSON.stringify(up2.error));

  // ---- functions see the real visitor
  await op("functions.deploy", { project: P, name: "whoami", verify_jwt: false, code: `export default (req) => new Response(JSON.stringify({
      xff: req.headers.get("x-forwarded-for"), ip: req.headers.get("x-real-ip"), proto: req.headers.get("x-forwarded-proto") }),
      { headers: { "content-type": "application/json" } });` });
  r = await via("198.51.100.30", { "x-forwarded-for": "6.6.6.6" })(`${api.url}/functions/v1/whoami`, { headers: { apikey: api.anon_key } });
  const who = await r.json().catch(() => ({}));
  check("function gets the visitor IP in x-real-ip / x-forwarded-for", who.ip === "198.51.100.30" && who.xff?.startsWith("198.51.100.30"), JSON.stringify(who));
  check("function sees x-forwarded-proto https", who.proto === "https", JSON.stringify(who));
  check("function on the project subdomain is not sandboxed", !r.headers.get("content-security-policy"), r.headers.get("content-security-policy"));
  r = await pf(`${PANEL}/fn/${P}/whoami`);
  check("function via the panel origin (/fn/) is sandboxed", r.status === 200 && r.headers.get("content-security-policy") === "sandbox", `${r.status} ${r.headers.get("content-security-policy")}`);

  // ---- panel login: Secure cookie and per-visitor lockout
  const login = (f, base, pw) => f(`${base}/api/v1/auth/login`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ username: "owner", password: pw }) });
  r = await login(via("198.51.100.40"), PANEL, OWNER_PW);
  check("panel login through the proxy sets a Secure cookie", r.status === 200 && /;\s*Secure/i.test(r.headers.get("set-cookie") || ""), `${r.status} ${r.headers.get("set-cookie")}`);
  r = await login(fetch, IV, OWNER_PW);
  check("login on the plain local port (SSH tunnel) works without Secure", r.status === 200 && !/Secure/i.test(r.headers.get("set-cookie") || ""), r.headers.get("set-cookie"));
  let locked = false;
  for (let i = 0; i < 12 && !locked; i++) locked = (await login(via("198.51.100.41"), PANEL, "wrong")).status === 429;
  check("10 bad panel logins lock out that visitor", locked);
  r = await login(via("198.51.100.42"), PANEL, OWNER_PW);
  check("another visitor can still sign in", r.status === 200, r.status);

  // ---- Postgres: direct TLS with the proxy's wildcard certificate
  const conn = created.connection;
  check("connection info asks for sslmode=require", conn?.sslmode === "require", JSON.stringify(conn && { host: conn.host, sslmode: conn.sslmode }));
  const ps = spawnSync(PSQL, ["-Atc", "select ssl from pg_stat_ssl where pid = pg_backend_pid()",
    `host=db.${D} hostaddr=127.0.0.1 port=${conn.port} dbname=${conn.database} user=${conn.user} sslmode=verify-full sslrootcert=${CERT}/fullchain.pem`],
    { env: { ...process.env, PGPASSWORD: conn.password }, encoding: "utf8" });
  check("psql sslmode=verify-full to db.<domain> succeeds over TLS", ps.status === 0 && ps.stdout.trim() === "t", (ps.stderr || ps.error?.message || "").trim());

  // ---- doctor understands proxy mode
  const doc = spawnSync(BIN, ["doctor", "--data", DATA], { encoding: "utf8" });
  check("doctor: proxy line and certificate covers domain + db.", /proxy .*HTTPS ends at your proxy/.test(doc.stdout) && new RegExp(`certificate +covers ${D}, db\\.${D}`).test(doc.stdout), doc.stdout.split("\n").filter((l) => /proxy|certificate/.test(l)).join(" | "));
  check("doctor doesn't ask for ports 80/443 in proxy mode", !/port :443|port :80/.test(doc.stdout), doc.stdout);

  await opRaw("projects.delete", { project: P, confirm: true });
} catch (e) {
  check("suite ran to the end", false, e.stack);
}
proxy.close();
console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
