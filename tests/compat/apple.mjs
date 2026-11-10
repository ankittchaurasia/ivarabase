// Sign in with Apple end-to-end checks (web redirect, PKCE, native id_token, config as code).
// Run with scripts/selftest.sh, which starts a throwaway instance wired to tests/compat/apple-mock.mjs.
import { createClient } from "@supabase/supabase-js";
import crypto from "node:crypto";
import fs from "node:fs";
const { IV, TOKEN, MOCK, P8 } = process.env;
const P = "apple_test";
const p8 = fs.readFileSync(P8, "utf8");
let pass = 0, fail = 0;
const check = (label, ok, info = "") => { ok ? pass++ : fail++; console.log(`${ok ? "PASS" : "FAIL"}  ${label.padEnd(62)} ${ok ? "" : String(info).slice(0, 200)}`); };
const opRaw = async (name, args) => {
  const r = await fetch(`${IV}/api/v1/ops/${name}`, { method: "POST", headers: { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" }, body: JSON.stringify(args) });
  return { status: r.status, body: await r.json() };
};
const op = async (name, args) => { const r = await opRaw(name, args); if (r.status >= 300) throw new Error(name + ": " + JSON.stringify(r.body)); return r.body.result ?? r.body; };

await opRaw("projects.delete", { project: P, confirm: true });
await op("projects.create", { slug: P, name: "Apple test" });
const api = await op("projects.api", { project: P });
const URL = `${IV}/proj/${P}`, ANON = api.anon_key;
const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) }; };
const client = (flowType = "implicit") => createClient(URL, ANON, { auth: { storage: mem(), autoRefreshToken: false, flowType } });
const base = { project: P, site_url: "http://localhost:3000", redirect_urls: ["http://localhost:3000/*"] };
const web = { apple_client_id: "com.example.web", apple_team_id: "TEAMID1234", apple_key_id: "KEYID12345" };

// ---- settings validation
let r = await opRaw("auth.config_set", { ...base, apple_enabled: true });
check("enable with nothing configured is refused", r.status === 400, JSON.stringify(r.body));
r = await opRaw("auth.config_set", { ...base, ...web, apple_enabled: true });
check("half-configured web sign-in is refused (names what's missing)", r.status === 400 && /private key/.test(JSON.stringify(r.body)), JSON.stringify(r.body));
r = await opRaw("auth.config_set", { ...base, ...web, apple_private_key: "garbage", apple_enabled: true });
check("invalid .p8 key is refused", r.status === 400 && /apple_private_key/.test(JSON.stringify(r.body)), JSON.stringify(r.body));
r = await opRaw("auth.config_set", { ...base, ...web, apple_private_key: p8.replace(/\n/g, "\\n"), apple_bundle_ids: ["com.example.ios"], apple_enabled: true });
check("full config (key with literal \\n, as from .env) accepted", r.status === 200, JSON.stringify(r.body));
const cfg = r.body.result?.config ?? r.body.config;
check("private key is masked when read back", cfg?.apple?.private_key === "********" && cfg?.apple?.team_id === "TEAMID1234", JSON.stringify(cfg?.apple));
const settings = await (await fetch(`${URL}/auth/v1/settings`)).json();
check("/settings reports external.apple", settings.external?.apple === true, JSON.stringify(settings.external));

// ---- web flow (implicit)
const webSignIn = async (c, user) => {
  const { data } = await c.auth.signInWithOAuth({ provider: "apple", options: { redirectTo: "http://localhost:3000/cb", skipBrowserRedirect: true } });
  const a = await fetch(data.url, { redirect: "manual" });
  const loc = new globalThis.URL(a.headers.get("location"));
  const form = new URLSearchParams({ code: "good-code", state: loc.searchParams.get("state") });
  if (user) form.set("user", JSON.stringify(user));
  const cb = await fetch(`${URL}/auth/v1/callback`, { method: "POST", body: form, redirect: "manual" }); // no apikey, like Apple's form post
  return { loc, cb, to: cb.headers.get("location") ?? "" };
};
let c = client();
let w = await webSignIn(c, { name: { firstName: "Asha", lastName: "Rao" }, email: "relay123@privaterelay.appleid.com" });
check("authorize redirects to Apple with response_mode=form_post", w.loc.origin === MOCK && w.loc.pathname === "/auth/authorize" && w.loc.searchParams.get("response_mode") === "form_post", w.loc.href);
check("authorize sends Services ID, scope 'name email', return URL", w.loc.searchParams.get("client_id") === "com.example.web" && w.loc.searchParams.get("scope") === "name email" && w.loc.searchParams.get("redirect_uri") === `${api.url}/auth/v1/callback`, w.loc.search);
check("callback POST (form_post) redirects with a session", w.cb.status === 303 && w.to.startsWith("http://localhost:3000/cb#access_token="), w.cb.status + " " + w.to);
const frag = new URLSearchParams(w.to.split("#")[1]);
const s1 = await c.auth.setSession({ access_token: frag.get("access_token"), refresh_token: frag.get("refresh_token") });
const u1 = s1.data.user;
check("user has Apple relay email, confirmed", u1?.email === "relay123@privaterelay.appleid.com" && !!u1?.email_confirmed_at, JSON.stringify(u1 && { e: u1.email, c: u1.email_confirmed_at }));
check("first-login name from Apple's user field saved", u1?.user_metadata?.full_name === "Asha Rao", JSON.stringify(u1?.user_metadata));
check("app_metadata.provider is apple", u1?.app_metadata?.provider === "apple" && u1?.identities?.[0]?.provider === "apple", JSON.stringify(u1?.app_metadata));
check("provider_token passed through", frag.get("provider_token") === "apple-at", frag.get("provider_token"));

// second login without the user field (Apple only sends it once) keeps the same account and name
w = await webSignIn(client());
const frag2 = new URLSearchParams(w.to.split("#")[1]);
const u2 = (await client().auth.getUser(frag2.get("access_token"))).data.user;
check("second web sign-in -> same user, name kept", u2?.id === u1?.id && u2?.user_metadata?.full_name === "Asha Rao", JSON.stringify(u2?.user_metadata));

// errors
{
  const { data } = await client().auth.signInWithOAuth({ provider: "apple", options: { redirectTo: "http://localhost:3000/cb", skipBrowserRedirect: true } });
  const loc = new globalThis.URL((await fetch(data.url, { redirect: "manual" })).headers.get("location"));
  let cb = await fetch(`${URL}/auth/v1/callback`, { method: "POST", body: new URLSearchParams({ code: "expired", state: loc.searchParams.get("state") }), redirect: "manual" });
  check("bad code -> redirect with error, no session", /#error=access_denied/.test(cb.headers.get("location")) && /oauth_exchange_failed/.test(cb.headers.get("location")), cb.headers.get("location"));
  cb = await fetch(`${URL}/auth/v1/callback`, { method: "POST", body: new URLSearchParams({ code: "good-code", state: loc.searchParams.get("state") + "x" }), redirect: "manual" });
  check("tampered state rejected", /bad_oauth_state/.test(cb.headers.get("location") ?? ""), cb.headers.get("location"));
  cb = await fetch(`${URL}/auth/v1/callback`, { method: "POST", body: new URLSearchParams({ error: "user_cancelled_authorize", state: loc.searchParams.get("state") }), redirect: "manual" });
  check("user cancelling at Apple -> error redirect", /user_cancelled_authorize/.test(cb.headers.get("location") ?? ""), cb.headers.get("location"));
}

// PKCE (supabase-js default for SSR / mobile deep links)
{
  const pk = client("pkce");
  const { data } = await pk.auth.signInWithOAuth({ provider: "apple", options: { redirectTo: "http://localhost:3000/cb", skipBrowserRedirect: true } });
  const loc = new globalThis.URL((await fetch(data.url, { redirect: "manual" })).headers.get("location"));
  const cb = await fetch(`${URL}/auth/v1/callback`, { method: "POST", body: new URLSearchParams({ code: "good-code", state: loc.searchParams.get("state") }), redirect: "manual" });
  const code = new globalThis.URL(cb.headers.get("location")).searchParams.get("code");
  const ex = await pk.auth.exchangeCodeForSession(code);
  check("PKCE web flow -> exchangeCodeForSession", ex.data.session?.user?.id === u1?.id, ex.error?.message);
}

// ---- native (signInWithIdToken)
const mint = async (o) => (await (await fetch(`${MOCK}/mint`, { method: "POST", body: JSON.stringify(o) })).json()).id_token;
const raw = crypto.randomBytes(16).toString("hex");
const hashed = crypto.createHash("sha256").update(raw).digest("hex");
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios", nonce: hashed }), nonce: raw });
check("native id_token (bundle ID audience, hashed nonce) -> session", !!r.data.session?.access_token, r.error?.message);
check("native sign-in finds the same Apple account as web", r.data.user?.id === u1?.id, r.data.user?.id);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.web" }) });
check("id_token for the Services ID also accepted (no nonce)", !!r.data.session, r.error?.message);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.evil.app", nonce: hashed }), nonce: raw });
check("id_token for another app's bundle ID rejected", !!r.error && /audience/.test(r.error.message), r.error?.message);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios", nonce: hashed }), nonce: "wrong" });
check("wrong nonce rejected", !!r.error && /nonce/i.test(r.error.message), r.error?.message);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios", nonce: hashed }) });
check("nonce in token but not passed -> rejected (replay guard)", !!r.error, r.error?.message);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios", exp: -3600 }) });
check("expired id_token rejected", !!r.error && /expired/.test(r.error.message), r.error?.message);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios", iss: "https://evil.example" }) });
check("wrong issuer rejected", !!r.error && /issuer/.test(r.error.message), r.error?.message);
const good = await mint({ aud: "com.example.ios" });
const [h, b] = good.split(".");
const forged = h + "." + Buffer.from(JSON.stringify({ ...JSON.parse(Buffer.from(b, "base64url")), sub: "someone-else" })).toString("base64url") + "." + good.split(".")[2];
r = await client().auth.signInWithIdToken({ provider: "apple", token: forged });
check("forged claims (bad signature) rejected", !!r.error && /signature/.test(r.error.message), r.error?.message);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios", sub: "000999.new-user", email: "fresh@privaterelay.appleid.com" }) });
check("native sign-in creates a new user for a new Apple ID", !!r.data.user && r.data.user.id !== u1?.id && r.data.user.email === "fresh@privaterelay.appleid.com", r.error?.message);
r = await client().auth.signInWithIdToken({ provider: "facebook", token: good });
check("unsupported provider rejected", !!r.error, "no error");

// ---- config as code
const exp = await op("config.export", { project: P });
const ea = (exp.spec ?? exp).auth?.apple;
check("export writes ${APPLE_PRIVATE_KEY}, keeps ids", ea?.private_key === "${APPLE_PRIVATE_KEY}" && ea?.team_id === "TEAMID1234" && ea?.bundle_ids?.[0] === "com.example.ios", JSON.stringify(ea));
let plan = await op("config.plan", { spec: { project: P, auth: { apple: { ...ea, private_key: p8 } } } });
const authSteps = (plan.changes ?? []).filter((s) => s.area === "auth");
check("plan with the same key (PEM form) -> no auth change", authSteps.length === 0, JSON.stringify(authSteps));
plan = await op("config.plan", { spec: { project: P, auth: { apple: { ...ea, private_key: p8, bundle_ids: ["com.example.ios", "com.example.mac"] } } } });
const st = (plan.changes ?? []).find((s) => s.area === "auth");
check("plan shows bundle_ids change", !!st && /apple_bundle_ids/.test(JSON.stringify(st)), JSON.stringify(plan));
check("plan output never contains the key", !JSON.stringify(plan).includes("PRIVATE KEY"), "key leaked in plan");

// ---- native-only and disabled
r = await opRaw("auth.config_set", { project: P, apple_client_id: "", apple_team_id: "", apple_key_id: "", apple_private_key: "", apple_bundle_ids: ["com.example.ios"], apple_enabled: true });
check("native-only config (bundle IDs only) accepted", r.status === 200, JSON.stringify(r.body));
let a = await fetch(`${URL}/auth/v1/authorize?provider=apple`, { redirect: "manual" });
check("native-only: web authorize says not enabled", a.status === 400, a.status);
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios" }) });
check("native-only: signInWithIdToken still works", !!r.data.session, r.error?.message);
await op("auth.config_set", { project: P, apple_enabled: false });
r = await client().auth.signInWithIdToken({ provider: "apple", token: await mint({ aud: "com.example.ios" }) });
check("disabled: signInWithIdToken refused", !!r.error && /not enabled/.test(r.error.message), r.error?.message);

const users = await op("auth.users_list", { project: P });
check("auth users list shows the Apple users", JSON.stringify(users).includes("relay123@privaterelay.appleid.com"), "");
if (!process.env.KEEP) await op("projects.delete", { project: P, confirm: true });
console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
