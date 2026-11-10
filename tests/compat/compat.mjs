// supabase-js compatibility suite for IvaraBase.
// Needs SUPABASE_URL, SUPABASE_ANON_KEY, SUPABASE_SERVICE_ROLE_KEY for the
// compat_test project (scripts/compat.sh sets them up). Exit code 1 on failure.
import { createClient } from "@supabase/supabase-js";

const URL = process.env.SUPABASE_URL, ANON = process.env.SUPABASE_ANON_KEY, SERVICE = process.env.SUPABASE_SERVICE_ROLE_KEY;
if (!URL || !ANON || !SERVICE) {
  console.error("set SUPABASE_URL, SUPABASE_ANON_KEY and SUPABASE_SERVICE_ROLE_KEY (scripts/compat.sh does this)");
  process.exit(2);
}
const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) }; };
const client = (key = ANON) => createClient(URL, key, { auth: { storage: mem(), autoRefreshToken: false, persistSession: key === ANON } });
const admin = client(SERVICE);
let pass = 0, fail = 0;
const failures = [];
function check(area, label, ok, info = "") {
  ok ? pass++ : (fail++, failures.push(`${area}: ${label} ${info}`));
  console.log(`${ok ? "PASS" : "FAIL"}  ${area.padEnd(9)} ${label.padEnd(52)} ${ok ? "" : String(info).slice(0, 120)}`);
}
const t = Date.now();
const email = (n) => `${n}+${t}@example.com`;

// ------------------------------------------------------------------ REST
{
  let r = await client().from("orders").select("item, qty").order("qty", { ascending: false });
  check("rest", "anon select filtered by RLS policy", r.data?.length === 2 && r.data[0].item === "apple", r.error?.message ?? JSON.stringify(r.data));
  r = await client().from("orders").select("*", { count: "exact", head: true });
  check("rest", "count with head: true", r.count === 2, r.error?.message ?? r.count);
  r = await client().from("orders").insert({ item: "x" });
  check("rest", "anon insert blocked by RLS", !!r.error, "no error");
  r = await admin.from("orders").select("id,item").in("item", ["apple", "kiwi"]).order("id");
  check("rest", "service role bypasses RLS + in() filter", r.data?.length === 2, r.error?.message ?? JSON.stringify(r.data));
  r = await admin.from("orders").insert({ item: `mango-${t}`, qty: 3 }).select().single();
  const id = r.data?.id;
  check("rest", "insert + select().single()", !!id, r.error?.message);
  r = await admin.from("orders").upsert({ id, item: `mango-${t}`, qty: 9 }).select("qty").single();
  check("rest", "upsert", r.data?.qty === 9, r.error?.message);
  r = await admin.from("orders").update({ qty: 4 }).eq("id", id).select("qty");
  check("rest", "update + eq", r.data?.[0]?.qty === 4, r.error?.message);
  r = await admin.from("orders").select("item").ilike("item", "%ANGO%").range(0, 0);
  check("rest", "ilike + range", r.data?.length === 1, r.error?.message);
  r = await admin.from("orders").delete().eq("id", id).select("id");
  check("rest", "delete", r.data?.length === 1, r.error?.message);
  r = await admin.rpc("add_numbers", { a: 2, b: 40 });
  check("rest", "rpc", r.data === 42, r.error?.message ?? r.data);
  r = await admin.from("missing_table").select("*");
  check("rest", "unknown table returns an error", !!r.error, "no error");
}

// ------------------------------------------------------------------ Auth
const alice = client();
let aliceId;
{
  let r = await alice.auth.signUp({ email: email("alice"), password: "secret-123", options: { data: { name: "Alice" } } });
  aliceId = r.data.user?.id;
  check("auth", "signUp returns a session", !!r.data.session, r.error?.message);
  r = await client().auth.signUp({ email: email("alice"), password: "secret-123" });
  check("auth", "duplicate signUp rejected", !!r.error, "no error");
  r = await client().auth.signUp({ email: email("weak"), password: "1" });
  check("auth", "weak password rejected", r.error?.code === "weak_password", r.error?.code);
  r = await alice.auth.getUser();
  check("auth", "getUser with user_metadata", r.data.user?.user_metadata?.name === "Alice", r.error?.message);
  r = await alice.auth.updateUser({ data: { plan: "pro" } });
  check("auth", "updateUser merges metadata", r.data.user?.user_metadata?.plan === "pro", r.error?.message);
  r = await alice.from("todos").insert({ title: "alice todo" }).select();
  check("auth", "RLS insert as user (auth.uid default)", r.data?.length === 1, r.error?.message);
  const bob = client();
  await bob.auth.signUp({ email: email("bob"), password: "secret-123" });
  r = await bob.from("todos").select("title");
  check("auth", "RLS: other user sees none of alice's rows", r.data?.length === 0, JSON.stringify(r.data));
  r = await alice.auth.refreshSession();
  const oldRefresh = r.data.session?.refresh_token;
  check("auth", "refreshSession", !!r.data.session?.access_token, r.error?.message);
  r = await alice.auth.signOut();
  check("auth", "signOut", !r.error, r.error?.message);
  r = await client().auth.refreshSession({ refresh_token: oldRefresh });
  check("auth", "refresh token dead after signOut", !!r.error, "no error");
  r = await client().auth.signInWithPassword({ email: email("alice"), password: "wrong" });
  check("auth", "wrong password rejected", r.error?.code === "invalid_credentials", r.error?.code);
  r = await alice.auth.signInWithPassword({ email: email("alice"), password: "secret-123" });
  check("auth", "signInWithPassword", !!r.data.session, r.error?.message);
  r = await admin.auth.admin.listUsers();
  check("auth", "admin.listUsers", r.data?.users?.length >= 2, r.error?.message);
  r = await admin.auth.admin.createUser({ email: email("carol"), password: "secret-789", email_confirm: true });
  const carol = r.data.user;
  check("auth", "admin.createUser", !!carol?.id, r.error?.message);
  r = await admin.auth.admin.generateLink({ type: "magiclink", email: carol.email });
  check("auth", "admin.generateLink", !!r.data?.properties?.action_link, r.error?.message);
  r = await admin.auth.admin.deleteUser(carol.id);
  check("auth", "admin.deleteUser", !r.error, r.error?.message);
  r = await client().auth.admin.listUsers();
  check("auth", "admin API refuses the anon key", !!r.error, "no error");
}

// ------------------------------------------------------------------ Storage
{
  const png = Buffer.from("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8cfc0f01f0005000201e2d2e7d10000000049454e44ae426082", "hex");
  let r = await admin.storage.listBuckets();
  check("storage", "listBuckets", r.data?.some((b) => b.id === "avatars") && r.data?.some((b) => b.id === "docs"), r.error?.message);
  r = await client().storage.createBucket(`nope-${t}`);
  check("storage", "anon cannot create buckets", !!r.error, "no error");
  r = await admin.storage.from("avatars").upload(`p/${t}.png`, png, { contentType: "image/png" });
  check("storage", "upload to public bucket", !r.error, r.error?.message);
  r = await admin.storage.from("avatars").upload(`p/${t}.txt`, Buffer.from("x"), { contentType: "text/plain" });
  check("storage", "bucket mime-type rule enforced", !!r.error, "no error");
  r = await admin.storage.from("avatars").upload(`p/${t}-big.png`, Buffer.alloc(2 << 20), { contentType: "image/png" });
  check("storage", "bucket size limit enforced", !!r.error, "no error");
  const res = await fetch(admin.storage.from("avatars").getPublicUrl(`p/${t}.png`).data.publicUrl);
  check("storage", "public URL works without a key", res.status === 200 && (await res.arrayBuffer()).byteLength === png.length, res.status);
  const folder = aliceId;
  r = await alice.storage.from("docs").upload(`${folder}/a.txt`, Buffer.from("v1"), { contentType: "text/plain" });
  check("storage", "user uploads into own folder", !r.error, r.error?.message);
  r = await alice.storage.from("docs").upload(`someone/a.txt`, Buffer.from("x"), { contentType: "text/plain" });
  check("storage", "upload outside own folder blocked", !!r.error, "no error");
  r = await alice.storage.from("docs").update(`${folder}/a.txt`, Buffer.from("v2"), { contentType: "text/plain" });
  check("storage", "update own file", !r.error, r.error?.message);
  r = await alice.storage.from("docs").download(`${folder}/a.txt`);
  check("storage", "download own file", (await r.data?.text()) === "v2", r.error?.message);
  r = await client().storage.from("docs").download(`${folder}/a.txt`);
  check("storage", "anon cannot read private file", !!r.error, "no error");
  r = await alice.storage.from("docs").list(folder);
  check("storage", "list folder", r.data?.length === 1, JSON.stringify(r.data?.map((x) => x.name)));
  r = await alice.storage.from("docs").createSignedUrl(`${folder}/a.txt`, 60);
  const s = await fetch(r.data?.signedUrl ?? "http://invalid");
  check("storage", "signed URL works without a key", s.status === 200 && (await s.text()) === "v2", r.error?.message ?? s.status);
  r = await alice.storage.from("docs").move(`${folder}/a.txt`, `${folder}/b.txt`);
  check("storage", "move", !r.error, r.error?.message);
  r = await alice.storage.from("docs").remove([`${folder}/b.txt`]);
  check("storage", "remove", r.data?.length === 1, r.error?.message);
  await admin.storage.from("avatars").remove([`p/${t}.png`]);
}

// ------------------------------------------------------------------ Functions
{
  let r = await alice.functions.invoke("whoami");
  check("functions", "invoke as signed-in user", r.data?.email === email("alice"), r.error?.message ?? JSON.stringify(r.data));
  check("functions", "function queries as the caller (RLS)", r.data?.todos?.length === 1, JSON.stringify(r.data?.todos));
  check("functions", "project secret in env", r.data?.greeting === "namaste", r.data?.greeting);
  r = await client().functions.invoke("whoami");
  check("functions", "invoke with anon key", r.data?.email === null, JSON.stringify(r.data));
  let res = await fetch(`${URL}/functions/v1/whoami`, { method: "POST" });
  check("functions", "no JWT -> 401 when verify_jwt is on", res.status === 401, res.status);
  res = await fetch(`${URL}/functions/v1/webhook`, { method: "POST", body: "event" });
  check("functions", "verify_jwt off: callable without a key", res.status === 200, res.status);
  r = await client().functions.invoke("multi", { body: { name: "ivara" } });
  check("functions", "multi-file function (relative import)", r.data?.message === "hello ivara from lib/greet.ts", r.error?.message ?? JSON.stringify(r.data));
  if (r.data?.deno) console.log(`          (functions ran on Deno ${r.data.deno})`);
  r = await client().functions.invoke("webhook", { body: "boom" });
  check("functions", "thrown error -> FunctionsHttpError 500", r.error?.context?.status === 500, r.error?.context?.status);
}

console.log(`\n${pass} passed, ${fail} failed`);
if (fail) {
  console.log("\nFailures:\n  " + failures.join("\n  "));
  process.exit(1);
}
