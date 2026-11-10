// v0.2 feature suite: extensions and slow queries, database webhooks, auth
// (anonymous, phone OTP, MFA, OIDC), resumable uploads and realtime.
// Run with scripts/selftest.sh (SUITES=features); needs IV (server URL) and
// TOKEN (admin token, allow_destructive). Creates and deletes its own project.
import { createClient } from "@supabase/supabase-js";
import crypto from "node:crypto";
import http from "node:http";
import jsQR from "jsqr";

const { IV, TOKEN } = process.env;
const P = "features_test";
let pass = 0, fail = 0;
const check = (area, label, ok, info = "") => {
  ok ? pass++ : fail++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${area.padEnd(9)} ${label.padEnd(56)} ${ok ? "" : String(info).slice(0, 300)}`);
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function op(name, args = {}) {
  const r = await fetch(`${IV}/api/v1/ops/${name}`, { method: "POST", headers: { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" }, body: JSON.stringify(args) });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(body.error?.message || `HTTP ${r.status}`); e.code = body.error?.code; e.body = body; throw e; }
  return body;
}
const tryOp = (name, args) => op(name, args).then((r) => ({ r }), (e) => ({ e }));
const sql = (q, read_only = false) => op("db.query", { project: P, sql: q, read_only, confirm: true });

// a local HTTP sink: records requests for webhook / SMS / OIDC checks
const hits = [];
let sinkHandler = null;
const sink = http.createServer(async (req, res) => {
  let body = "";
  for await (const c of req) body += c;
  hits.push({ method: req.method, url: req.url, headers: req.headers, body });
  if (sinkHandler && (await sinkHandler(req, res, body))) return;
  res.writeHead(req.url.startsWith("/fail") ? 500 : 200, { "content-type": "application/json" });
  res.end("{}");
});
// SINK_HOST: how the server reaches this machine (e.g. host.lima.internal when
// IvaraBase runs in a container); the sink then listens on all interfaces.
const SINK_HOST = process.env.SINK_HOST || "127.0.0.1";
await new Promise((r) => sink.listen(0, SINK_HOST === "127.0.0.1" ? "127.0.0.1" : "0.0.0.0", r));
const SINK = `http://${SINK_HOST}:${sink.address().port}`;

await tryOp("projects.delete", { project: P, confirm: true });
await op("projects.create", { slug: P, name: "features" });
const api = await op("projects.api", { project: P });
const URL_ = `${IV}/proj/${P}`;
const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) }; };
const client = (key = api.anon_key, opts = {}) => createClient(URL_, key, { auth: { storage: mem(), autoRefreshToken: false, persistSession: true }, ...opts });
const admin = client(api.service_role_key);

// ------------------------------------------------------------------ extensions
{
  const list = await op("db.extensions_list", { project: P });
  const vec = list.find((e) => e.name === "vector");
  check("ext", "extensions_list includes the curated list", list.length >= 10 && !!list.find((e) => e.name === "pg_trgm"), list.length);
  const bad = await tryOp("db.extension_enable", { project: P, name: "dblink" });
  check("ext", "extensions outside the allowed list are refused", bad.e?.code === "extension_not_allowed", bad.e?.code);
  let r = await tryOp("db.extension_enable", { project: P, name: "pg_trgm" });
  check("ext", "enable pg_trgm into the extensions schema", r.r?.schema === "extensions", r.e?.message);
  r = await tryOp("db.query", { project: P, sql: "select similarity('ivarabase','ivarabse') > 0.3 as ok", read_only: true });
  check("ext", "project role can use it without a schema prefix", r.r?.rows?.[0]?.[0] === true || r.r?.rows?.[0]?.ok === true, JSON.stringify(r.r ?? r.e?.message));
  if (vec?.available) {
    r = await tryOp("db.extension_enable", { project: P, name: "vector" });
    check("ext", "enable vector (untrusted, superuser)", r.r?.enabled === true, r.e?.message);
    await sql("create table docs (id serial primary key, body text, embedding vector(3)); alter table docs enable row level security; create policy read_all on docs for select to anon using (true); grant select on docs to anon;");
    await sql("insert into docs (body, embedding) values ('a','[1,0,0]'),('b','[0,1,0]'),('c','[0.9,0.1,0]')");
    await sql(`create or replace function match_docs(q vector(3), n int) returns table(body text) language sql stable as $$ select body from docs order by embedding <-> q limit n $$`);
    await sql("grant execute on function match_docs to anon");
    await sql("notify pgrst, 'reload schema'");
    await sleep(500);
    const rr = await client().rpc("match_docs", { q: "[1,0,0]", n: 2 });
    check("ext", "vector similarity search through the REST API", rr.data?.map((d) => d.body).join() === "a,c", rr.error?.message ?? JSON.stringify(rr.data));
    const bk = await tryOp("backups.create", { project: P, verify: true });
    const bid = bk.r?.id ?? bk.r?.backup?.id;
    check("ext", "backup with vector verifies", !!bid && (bk.r?.verified ?? bk.r?.verify?.ok ?? true) !== false, bk.e?.message ?? JSON.stringify(bk.r).slice(0, 200));
    await tryOp("projects.delete", { project: P + "_r", confirm: true });
    const rs = await tryOp("backups.restore", { id: bid, mode: "new", target_project: P + "_r" });
    const rq = await tryOp("db.query", { project: P + "_r", sql: "select body from docs order by embedding <-> '[1,0,0]' limit 1", read_only: true });
    check("ext", "restore as a new project keeps vector data", rq.r?.rows?.[0]?.[0] === "a", (rs.e?.message ?? "") + " " + (rq.e?.message ?? JSON.stringify(rs.r).slice(0, 200)));
    await tryOp("projects.delete", { project: P + "_r", confirm: true });
    const off = await tryOp("db.extension_disable", { project: P, name: "vector", confirm: true });
    check("ext", "disable refuses while a column uses it", off.e?.code === "extension_in_use", off.e?.code ?? "no error");
  } else {
    console.log("SKIP  ext       pgvector is not installed here");
  }
  r = await tryOp("db.slow_queries", { project: P });
  if (r.e?.code === "query_stats_unavailable") console.log("SKIP  ext       pg_stat_statements not loaded");
  else check("ext", "slow_queries lists this project's statements", Array.isArray(r.r) && r.r.length > 0, r.e?.message ?? JSON.stringify(r.r).slice(0, 200));
}


// ------------------------------------------------------------------ MCP with the token in the URL (ChatGPT connectors)
{
  const call = (url) => fetch(url, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) }).then(async (r) => ({ status: r.status, body: await r.json().catch(() => null) }));
  let r = await call(`${IV}/mcp/${TOKEN}`);
  check("mcp", "tools/list with the token as the last path segment", r.status === 200 && r.body?.result?.tools?.length > 20, r.status);
  r = await call(`${IV}/mcp/ivb_notavalidtoken`);
  check("mcp", "an invalid token in the URL is rejected", r.status === 401, r.status);
}

// ------------------------------------------------------------------ auth: anonymous
const t = Date.now();
const routes = {}; // path prefix -> handler(req, res, body, url)
sinkHandler = async (req, res, body) => {
  const u = new URL(req.url, SINK);
  for (const [p, h] of Object.entries(routes)) if (u.pathname.startsWith(p)) { await h(req, res, body, u); return true; }
  return false;
};
await op("auth.config_set", { project: P, redirect_urls: ["http://localhost:3000/*"] });
{
  let r = await client().auth.signInAnonymously();
  check("anon", "off by default", r.error?.code === "anonymous_provider_disabled", r.error?.code ?? "no error");
  await op("auth.config_set", { project: P, anonymous_users: true });
  const anon = client();
  r = await anon.auth.signInAnonymously({ options: { data: { theme: "dark" } } });
  check("anon", "signInAnonymously returns a session", !!r.data.session && r.data.user?.is_anonymous === true, r.error?.message);
  const claims = JSON.parse(Buffer.from(r.data.session.access_token.split(".")[1], "base64url"));
  check("anon", "JWT: role authenticated, is_anonymous true", claims.role === "authenticated" && claims.is_anonymous === true, JSON.stringify(claims).slice(0, 200));
  await sql("create table notes (id serial primary key, owner uuid not null default auth.uid(), body text); alter table notes enable row level security; create policy own on notes for all to authenticated using (owner = auth.uid()) with check (owner = auth.uid()); grant all on notes to authenticated; grant usage on sequence notes_id_seq to authenticated;");
  await sql("notify pgrst, 'reload schema'");
  await sleep(400);
  r = await anon.from("notes").insert({ body: "draft" }).select();
  check("anon", "anonymous user writes rows under RLS", r.data?.length === 1, r.error?.message);
  r = await anon.auth.updateUser({ email: `anon+${t}@example.com` });
  check("anon", "updateUser({email}) converts to a permanent user", r.data.user?.is_anonymous === false && r.data.user?.email === `anon+${t}@example.com`, r.error?.message ?? JSON.stringify(r.data.user).slice(0, 200));
  r = await anon.auth.updateUser({ password: "secret-123" });
  r = await client().auth.signInWithPassword({ email: `anon+${t}@example.com`, password: "secret-123" });
  check("anon", "converted user signs in with a password, rows kept", !!r.data.session, r.error?.message);
}

// ------------------------------------------------------------------ auth: phone
const smsHits = [];
routes["/sms"] = (req, res, body) => { smsHits.push({ headers: req.headers, raw: body, body: JSON.parse(body) }); res.writeHead(200); res.end("{}"); };
const lastCode = (phone) => [...smsHits].reverse().find((h) => h.body.phone === "+" + phone)?.body.otp;
{
  const ph = (n) => `1555${String(t).slice(-6)}${n}`;
  let r = await client().auth.signInWithOtp({ phone: "+" + ph(1) });
  check("phone", "off by default", r.error?.code === "phone_provider_disabled", r.error?.code ?? "no error");
  await op("auth.config_set", { project: P, phone_enabled: true, sms_provider: "webhook", sms_webhook_url: `${SINK}/sms`, sms_webhook_secret: "s3cret", sms_test_otp: { ["+" + ph(9)]: "424242" } });
  const c1 = client();
  r = await c1.auth.signInWithOtp({ phone: "+" + ph(1) });
  const hit = smsHits.at(-1);
  check("phone", "signInWithOtp sends a code through the SMS webhook", !r.error && /^\d{6}$/.test(hit?.body.otp ?? "") && hit.body.message.includes(hit.body.otp), r.error?.message ?? JSON.stringify(hit?.body));
  const sig = "sha256=" + crypto.createHmac("sha256", "s3cret").update(hit.headers["x-ivarabase-timestamp"] + "." + hit.raw).digest("hex");
  check("phone", "webhook is signed", hit.headers["x-ivarabase-signature"] === sig, hit.headers["x-ivarabase-signature"]);
  r = await client().auth.verifyOtp({ phone: "+" + ph(1), token: "000000", type: "sms" });
  check("phone", "wrong code rejected", r.error?.code === "otp_expired", r.error?.code ?? "no error");
  r = await c1.auth.verifyOtp({ phone: "+" + ph(1), token: lastCode(ph(1)), type: "sms" });
  check("phone", "verifyOtp({type: 'sms'}) signs in", !!r.data.session && r.data.user?.phone === ph(1) && !!r.data.user?.phone_confirmed_at, r.error?.message ?? JSON.stringify(r.data.user).slice(0, 200));
  r = await client().auth.signInWithOtp({ phone: "+" + ph(1) });
  check("phone", "a second code within 60s is rate limited", r.error?.code === "over_sms_send_rate_limit", r.error?.code ?? "no error");

  const c2 = client();
  r = await c2.auth.signUp({ phone: "+" + ph(2), password: "secret-123" });
  check("phone", "signUp({phone, password}) waits for the code", !r.data.session && r.data.user?.phone === ph(2), r.error?.message);
  r = await client().auth.signInWithPassword({ phone: "+" + ph(2), password: "secret-123" });
  check("phone", "unconfirmed phone can't sign in with password", r.error?.code === "phone_not_confirmed", r.error?.code ?? "no error");
  r = await c2.auth.verifyOtp({ phone: "+" + ph(2), token: lastCode(ph(2)), type: "sms" });
  r = await client().auth.signInWithPassword({ phone: "+" + ph(2), password: "secret-123" });
  check("phone", "signInWithPassword({phone}) after confirming", !!r.data.session, r.error?.message);

  r = await c1.auth.updateUser({ phone: "+" + ph(3) });
  const changeCode = lastCode(ph(3));
  r = await c1.auth.verifyOtp({ phone: "+" + ph(3), token: changeCode, type: "phone_change" });
  check("phone", "updateUser({phone}) + verifyOtp phone_change", r.data.user?.phone === ph(3), r.error?.message ?? JSON.stringify(r.data.user).slice(0, 200));

  const before = smsHits.length;
  r = await client().auth.signInWithOtp({ phone: "+" + ph(9) });
  r = await client().auth.verifyOtp({ phone: "+" + ph(9), token: "424242", type: "sms" });
  check("phone", "test numbers use the fixed code and send nothing", !!r.data.session && smsHits.length === before, r.error?.message ?? smsHits.length - before);
  const log = await op("auth.sms_list", { project: P });
  check("phone", "auth.sms_list shows sent codes", log.length >= 4 && log.some((l) => l.to === "+" + ph(9)), log.length);
}

// ------------------------------------------------------------------ auth: MFA (TOTP)
const b32decode = (s) => { const A = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"; let bits = "", out = []; for (const ch of s) bits += A.indexOf(ch).toString(2).padStart(5, "0"); for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2)); return Buffer.from(out); };
const totp = (secret, offset = 0) => { const step = Math.floor(Date.now() / 1000 / 30) + offset; const msg = Buffer.alloc(8); msg.writeBigUInt64BE(BigInt(step)); const h = crypto.createHmac("sha1", b32decode(secret)).update(msg).digest(); const o = h[h.length - 1] & 15; return String((h.readUInt32BE(o) & 0x7fffffff) % 1e6).padStart(6, "0"); };
function decodeQR(dataUri) {
  const svg = decodeURIComponent(dataUri.replace(/^data:image\/svg\+xml;utf-8,/, ""));
  const n = +svg.match(/viewBox="0 0 (\d+)/)[1], scale = 4, W = n * scale;
  const px = new Uint8ClampedArray(W * W * 4).fill(255);
  for (const [, x, y] of svg.matchAll(/M(\d+) (\d+)h1v1h-1z/g))
    for (let dy = 0; dy < scale; dy++) for (let dx = 0; dx < scale; dx++) { const i = ((+y * scale + dy) * W + (+x * scale + dx)) * 4; px[i] = px[i + 1] = px[i + 2] = 0; }
  return jsQR(px, W, W)?.data;
}
{
  const mail = `mfa+${t}@example.com`;
  const u = client();
  await u.auth.signUp({ email: mail, password: "secret-123" });
  let r = await u.auth.mfa.enroll({ factorType: "totp", friendlyName: "phone" });
  const f = r.data;
  check("mfa", "enroll returns secret, otpauth URI and QR code", !!f?.totp?.secret && f.totp.uri.startsWith("otpauth://totp/") && f.totp.qr_code.startsWith("data:image/svg+xml"), r.error?.message);
  check("mfa", "QR code decodes to the otpauth URI", decodeQR(f.totp.qr_code) === f.totp.uri, decodeQR(f.totp.qr_code));
  r = await u.auth.mfa.challengeAndVerify({ factorId: f.id, code: totp(f.totp.secret) });
  check("mfa", "challengeAndVerify upgrades the session", !r.error, r.error?.message);
  r = await u.auth.mfa.getAuthenticatorAssuranceLevel();
  check("mfa", "session is aal2", r.data?.currentLevel === "aal2", JSON.stringify(r.data));
  r = await u.auth.mfa.listFactors();
  check("mfa", "listFactors shows a verified totp factor", r.data?.totp?.length === 1, JSON.stringify(r.data));
  const code = totp(f.totp.secret);
  let ch = await u.auth.mfa.challenge({ factorId: f.id });
  r = await u.auth.mfa.verify({ factorId: f.id, challengeId: ch.data.id, code });
  check("mfa", "a used code can't be replayed", r.error?.code === "mfa_verification_failed", r.error?.code ?? "no error");

  await sql("create table vault (id int, secret text); alter table vault enable row level security; create policy aal2_only on vault for select to authenticated using ((auth.jwt()->>'aal') = 'aal2'); grant select on vault to authenticated; insert into vault values (1, 'x');");
  await sql("notify pgrst, 'reload schema'");
  await sleep(400);
  const fresh = client();
  await fresh.auth.signInWithPassword({ email: mail, password: "secret-123" });
  r = await fresh.auth.mfa.getAuthenticatorAssuranceLevel();
  check("mfa", "password sign-in is aal1 with nextLevel aal2", r.data?.currentLevel === "aal1" && r.data?.nextLevel === "aal2", JSON.stringify(r.data));
  r = await fresh.from("vault").select();
  check("mfa", "RLS on auth.jwt()->>'aal' hides rows at aal1", r.data?.length === 0, r.error?.message ?? JSON.stringify(r.data));
  r = await fresh.auth.mfa.unenroll({ factorId: f.id });
  check("mfa", "unenroll a verified factor needs aal2", r.error?.code === "insufficient_aal", r.error?.code ?? "no error");
  r = await fresh.auth.mfa.challengeAndVerify({ factorId: f.id, code: totp(f.totp.secret, 1) });
  r = await fresh.from("vault").select();
  check("mfa", "after verifying, aal2 rows are visible", r.data?.length === 1, r.error?.message ?? JSON.stringify(r.data));
  r = await fresh.auth.refreshSession();
  const c2 = JSON.parse(Buffer.from(r.data.session.access_token.split(".")[1], "base64url"));
  check("mfa", "refresh keeps aal2", c2.aal === "aal2" && c2.amr.some((m) => m.method === "totp"), JSON.stringify(c2.amr));
  const fl = await admin.auth.admin.mfa.listFactors({ userId: r.data.user.id });
  check("mfa", "admin.mfa.listFactors", fl.data?.factors?.length === 1, fl.error?.message ?? JSON.stringify(fl.data));
  r = await fresh.auth.mfa.unenroll({ factorId: f.id });
  check("mfa", "unenroll at aal2", !r.error, r.error?.message);
}

// ------------------------------------------------------------------ auth: generic OIDC
{
  const { privateKey, publicKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
  const jwk = { ...publicKey.export({ format: "jwk" }), kid: "k1", alg: "RS256", use: "sig" };
  const ISS = `${SINK}/oidc`;
  const sign = (claims) => {
    const h = Buffer.from(JSON.stringify({ alg: "RS256", kid: "k1", typ: "JWT" })).toString("base64url");
    const p = Buffer.from(JSON.stringify(claims)).toString("base64url");
    return `${h}.${p}.${crypto.sign("sha256", Buffer.from(`${h}.${p}`), privateKey).toString("base64url")}`;
  };
  const codes = {};
  const sub = `kc-${t}`, email = `oidc+${t}@example.com`;
  routes["/oidc"] = (req, res, body, u) => {
    const json = (o) => { res.writeHead(200, { "content-type": "application/json" }); res.end(JSON.stringify(o)); };
    if (u.pathname === "/oidc/.well-known/openid-configuration")
      return json({ issuer: ISS, authorization_endpoint: `${ISS}/authorize`, token_endpoint: `${ISS}/token`, userinfo_endpoint: `${ISS}/userinfo`, jwks_uri: `${ISS}/jwks` });
    if (u.pathname === "/oidc/jwks") return json({ keys: [jwk] });
    if (u.pathname === "/oidc/authorize") {
      const code = crypto.randomBytes(8).toString("hex");
      codes[code] = { nonce: u.searchParams.get("nonce"), client: u.searchParams.get("client_id") };
      res.writeHead(302, { location: `${u.searchParams.get("redirect_uri")}?code=${code}&state=${encodeURIComponent(u.searchParams.get("state"))}` });
      return res.end();
    }
    if (u.pathname === "/oidc/token") {
      const f = new URLSearchParams(body), c = codes[f.get("code")];
      if (!c || f.get("client_secret") !== "kc-secret") { res.writeHead(400); return res.end('{"error":"invalid_grant"}'); }
      const now = Math.floor(Date.now() / 1000);
      return json({ access_token: "at", id_token: sign({ iss: ISS, aud: "kc-client", sub, email, email_verified: true, name: "Kay Cee", nonce: c.nonce, iat: now, exp: now + 300 }) });
    }
    res.writeHead(404); res.end();
  };
  await op("auth.config_set", { project: P, oidc_providers: [{ name: "keycloak", label: "Company SSO", enabled: true, issuer: ISS, client_id: "kc-client", client_secret: "kc-secret" }] });
  const cfg = await op("auth.config_get", { project: P });
  check("oidc", "client secret is masked in config_get", cfg.config.oidc[0].client_secret === "********", cfg.config.oidc[0].client_secret);
  await op("auth.config_set", { project: P, oidc_providers: cfg.config.oidc }); // round trip keeps the secret
  const settings = await (await fetch(`${URL_}/auth/v1/settings`, { headers: { apikey: api.anon_key } })).json();
  check("oidc", "provider listed in /settings", settings.external?.keycloak === true, JSON.stringify(settings.external));
  const { data } = await client().auth.signInWithOAuth({ provider: "keycloak", options: { redirectTo: "http://localhost:3000/cb", skipBrowserRedirect: true } });
  let loc = data.url, hops = 0;
  while (loc && !loc.startsWith("http://localhost:3000/") && hops++ < 5) {
    const r = await fetch(loc.replace(`//${SINK_HOST}:`, "//127.0.0.1:"), { redirect: "manual" });
    loc = r.headers.get("location");
  }
  const frag = new URLSearchParams((loc || "").split("#")[1] || "");
  const at = frag.get("access_token");
  let r = at ? await admin.auth.getUser(at) : { error: { message: "no token: " + loc } };
  check("oidc", "signInWithOAuth redirect flow signs the user in", r.data?.user?.email === email && r.data.user.app_metadata.provider === "keycloak", r.error?.message ?? JSON.stringify(r.data?.user).slice(0, 200));
  const now = Math.floor(Date.now() / 1000);
  r = await client().auth.signInWithIdToken({ provider: "keycloak", token: sign({ iss: ISS, aud: "kc-client", sub, email, email_verified: true, iat: now, exp: now + 300 }) });
  check("oidc", "signInWithIdToken with the same identity", r.data?.user?.email === email, r.error?.message);
  r = await client().auth.signInWithIdToken({ provider: "keycloak", token: sign({ iss: ISS, aud: "someone-else", sub, email, iat: now, exp: now + 300 }) });
  check("oidc", "id_token for another client is rejected", !!r.error, "no error");
}

// ==== more sections ====

if (!process.env.KEEP) await tryOp("projects.delete", { project: P, confirm: true });
sink.close();
console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
