// Resumable uploads (TUS 1.0.0 at /storage/v1/upload/resumable) with the real
// tus-js-client, the way Supabase's docs use it. Run with scripts/selftest.sh
// (SUITES=uploads); needs IV (server URL) and TOKEN (admin token,
// allow_destructive). Creates and deletes its own project.
import { createClient } from "@supabase/supabase-js";
import * as tus from "tus-js-client";
import crypto from "node:crypto";

const { IV, TOKEN } = process.env;
const P = "uploads_test";
let pass = 0, fail = 0;
const check = (area, label, ok, info = "") => {
  ok ? pass++ : fail++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${area.padEnd(9)} ${label.padEnd(56)} ${ok ? "" : String(info).slice(0, 300)}`);
};
async function op(name, args = {}) {
  const r = await fetch(`${IV}/api/v1/ops/${name}`, { method: "POST", headers: { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" }, body: JSON.stringify(args) });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(body.error?.message || `HTTP ${r.status}`); e.code = body.error?.code; e.body = body; throw e; }
  return body;
}
const tryOp = (name, args) => op(name, args).then((r) => ({ r }), (e) => ({ e }));
const sql = (q) => op("db.query", { project: P, sql: q, read_only: false, confirm: true });

await tryOp("projects.delete", { project: P, confirm: true });
await op("projects.create", { slug: P, name: "uploads" });
const api = await op("projects.api", { project: P });
const URL_ = `${IV}/proj/${P}`;
const EP = `${URL_}/storage/v1/upload/resumable`;
const mem = () => { const m = new Map(); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) }; };
const client = (key = api.anon_key) => createClient(URL_, key, { auth: { storage: mem(), autoRefreshToken: false, persistSession: true } });
const admin = client(api.service_role_key);

await op("storage.bucket_create", { project: P, bucket: "files", public: false });
await op("storage.bucket_create", { project: P, bucket: "small", public: false, file_size_limit: "1MB", allowed_mime_types: ["image/*"] });
const policies = `
  create policy files_insert on storage.objects for insert to authenticated with check (bucket_id = 'files' and (storage.foldername(name))[1] = auth.uid()::text);
  create policy files_select on storage.objects for select to authenticated using (bucket_id = 'files' and owner = auth.uid());
  create policy files_update on storage.objects for update to authenticated using (bucket_id = 'files' and owner = auth.uid());`;
await sql(policies);

async function user(name) {
  const email = `${name}-${Date.now()}@example.com`;
  const r = await admin.auth.admin.createUser({ email, password: "secret-123", email_confirm: true });
  const c = client();
  const s = await c.auth.signInWithPassword({ email, password: "secret-123" });
  return { c, id: r.data.user.id, token: s.data.session.access_token };
}
const alice = await user("alice");
const bob = await user("bob");

// tusUpload runs one tus-js-client upload; resolves { upload, status, err }.
function tusUpload(data, { token, bucket = "files", object, contentType = "application/octet-stream", upsert, chunkSize = 1 << 20, during = false, uploadUrl, hooks = {} }) {
  return new Promise((resolve) => {
    const headers = { authorization: `Bearer ${token}` };
    if (upsert) headers["x-upsert"] = "true";
    const upload = new tus.Upload(data, {
      endpoint: EP, uploadUrl, headers, chunkSize, retryDelays: [], uploadDataDuringCreation: during, removeFingerprintOnSuccess: true,
      metadata: { bucketName: bucket, objectName: object, contentType, cacheControl: "3600" },
      onError: (err) => resolve({ upload, err, status: err.originalResponse?.getStatus?.() }),
      onSuccess: () => resolve({ upload, status: 0 }),
      ...hooks,
    });
    hooks.start?.(upload);
    upload.start();
  });
}
const raw = (method, url, token, headers = {}, body) =>
  fetch(url, { method, body, headers: { "tus-resumable": "1.0.0", authorization: `Bearer ${token}`, apikey: api.anon_key, ...headers } });
const md = (o) => Object.entries(o).map(([k, v]) => `${k} ${Buffer.from(v).toString("base64")}`).join(",");
async function create(token, object, length, extra = {}) {
  const r = await raw("POST", EP, token, { "upload-length": String(length), "upload-metadata": md({ bucketName: "files", objectName: object, contentType: "text/plain" }), ...extra });
  return { r, url: r.headers.get("location") ? new URL(r.headers.get("location"), EP).href : null };
}
const patch = (url, token, offset, body) => raw("PATCH", url, token, { "upload-offset": String(offset), "content-type": "application/offset+octet-stream" }, body);
async function download(c, bucket, path) {
  const r = await c.storage.from(bucket).download(path);
  return r.data ? Buffer.from(await r.data.arrayBuffer()) : null;
}
const MB = 1 << 20;

// ------------------------------------------------------------------ protocol
{
  const r = await fetch(EP, { method: "OPTIONS" });
  check("tus", "OPTIONS advertises version, extensions, max size", r.headers.get("tus-version") === "1.0.0" &&
    r.headers.get("tus-extension") === "creation,creation-with-upload,termination,expiration" && +r.headers.get("tus-max-size") > 0,
    [...r.headers].join(" "));
  const n = await raw("POST", EP, alice.token, { "upload-metadata": md({ bucketName: "files", objectName: `${alice.id}/x` }) });
  check("tus", "POST without Upload-Length is refused", n.status === 400, n.status);
  const nk = await fetch(EP, { method: "POST", headers: { "tus-resumable": "1.0.0", "upload-length": "1" } });
  check("tus", "POST without any credentials is refused", nk.status === 401, nk.status);
}

// ------------------------------------------------------------------ multi-chunk upload
{
  const data = crypto.randomBytes(5 * MB);
  const path = `${alice.id}/big.bin`;
  let chunks = 0, visibleEarly = null;
  const res = await tusUpload(data, { token: alice.token, object: path, hooks: { onChunkComplete: () => { chunks++; } ,
    onAfterResponse: async (req) => { if (chunks === 2 && visibleEarly === null) { const l = await admin.storage.from("files").list(alice.id); visibleEarly = !!l.data?.find((f) => f.name === "big.bin"); } } } });
  check("upload", "5 MB in 1 MB chunks with tus-js-client", res.status === 0 && chunks === 5, `${res.err?.message} chunks=${chunks}`);
  check("upload", "object is not listed before the last byte", visibleEarly === false, visibleEarly);
  const id = res.upload.url.split("/").pop();
  check("upload", "Location id is base64url(bucket/object/version)", Buffer.from(id, "base64url").toString().startsWith(`files/${path}/`), id);
  const got = await download(alice.c, "files", path);
  check("upload", "downloaded bytes match", got?.equals(data), got?.length);
  const info = await admin.storage.from("files").list(alice.id);
  const f = info.data?.find((x) => x.name === "big.bin");
  check("upload", "metadata: size, mimetype, md5 eTag, cacheControl", f?.metadata?.size === data.length && f.metadata.mimetype === "application/octet-stream" &&
    f.metadata.eTag === `"${crypto.createHash("md5").update(data).digest("hex")}"` && f.metadata.cacheControl === "max-age=3600", JSON.stringify(f?.metadata));
  const head = await raw("HEAD", res.upload.url, alice.token);
  check("upload", "finished upload is gone (HEAD 404)", head.status === 404, head.status);

  const small = Buffer.from("one request: creation-with-upload");
  const r2 = await tusUpload(small, { token: alice.token, object: `${alice.id}/small.txt`, contentType: "text/plain", during: true });
  check("upload", "creation-with-upload (body on POST)", r2.status === 0 && (await download(alice.c, "files", `${alice.id}/small.txt`))?.equals(small), r2.err?.message);
  const r3 = await tusUpload(Buffer.alloc(0), { token: alice.token, object: `${alice.id}/empty.txt`, contentType: "text/plain" });
  check("upload", "empty file", r3.status === 0 && (await download(alice.c, "files", `${alice.id}/empty.txt`))?.length === 0, r3.err?.message);
}

// ------------------------------------------------------------------ resume
{
  const data = crypto.randomBytes(5 * MB);
  const path = `${alice.id}/resumed.bin`;
  let up;
  const first = await new Promise((resolve) => {
    tusUpload(data, { token: alice.token, object: path, hooks: { start: (u) => (up = u),
      onChunkComplete: (_c, accepted) => { if (accepted >= 2 * MB) { up.abort(); resolve({ url: up.url }); } } } }).then(resolve);
  });
  await new Promise((r) => setTimeout(r, 200));
  const head = await raw("HEAD", first.url, alice.token);
  const offset = +head.headers.get("upload-offset");
  check("resume", "aborted upload keeps its offset (HEAD)", head.status === 200 && offset === 2 * MB && +head.headers.get("upload-length") === data.length &&
    !!head.headers.get("upload-expires"), `${head.status} ${offset}`);
  let firstPatch = null;
  const res = await tusUpload(data, { token: alice.token, object: path, uploadUrl: first.url, hooks: {
    onBeforeRequest: (req) => { if (req.getMethod() === "PATCH" && firstPatch === null) firstPatch = +req.getHeader("Upload-Offset"); } } });
  check("resume", "resumes from the server's offset", res.status === 0 && firstPatch === offset, `${res.err?.message} first PATCH at ${firstPatch}`);
  check("resume", "resumed file matches", (await download(alice.c, "files", path))?.equals(data));
  const exp = new Date(head.headers.get("upload-expires")) - Date.now();
  check("resume", "Upload-Expires is ~24h ahead", exp > 23.9 * 3600e3 && exp <= 24 * 3600e3, exp);
}

// ------------------------------------------------------------------ authorization and bucket rules
{
  let r = await tusUpload(Buffer.from("x"), { token: api.anon_key, object: "anon/x.txt" });
  check("auth", "RLS denies an anon upload to a private bucket", r.status === 403, `${r.status} ${r.err?.message}`);
  r = await tusUpload(Buffer.from("x"), { token: alice.token, object: `${bob.id}/x.txt` });
  check("auth", "RLS denies a user outside their folder", r.status === 403, `${r.status} ${r.err?.message}`);
  r = await tusUpload(Buffer.alloc(2 * MB), { token: api.service_role_key, bucket: "small", object: "big.png", contentType: "image/png" });
  check("rules", "Upload-Length over the bucket limit is rejected", r.status === 413, `${r.status} ${r.err?.message}`);
  const opt = await fetch(EP, { method: "OPTIONS" });
  const { r: big } = await create(alice.token, `${alice.id}/huge.bin`, +opt.headers.get("tus-max-size") + 1);
  check("rules", "Upload-Length over the server limit is rejected", big.status === 413, big.status);
  r = await tusUpload(Buffer.from("hello"), { token: api.service_role_key, bucket: "small", object: "a.txt", contentType: "text/plain" });
  check("rules", "wrong mime type is rejected", r.status === 415, `${r.status} ${r.err?.message}`);
  r = await tusUpload(Buffer.from("89504e470d0a1a0a", "hex"), { token: api.service_role_key, bucket: "small", object: "ok.png", contentType: "image/png" });
  check("rules", "allowed mime type under the limit is stored", r.status === 0, r.err?.message);

  const { r: c, url } = await create(alice.token, `${alice.id}/mine.txt`, 10);
  check("owner", "create returns 201 with a Location", c.status === 201 && !!url, c.status);
  let p = await patch(url, bob.token, 0, "0123456789");
  check("owner", "another user cannot PATCH someone else's upload", p.status === 403, p.status);
  p = await raw("HEAD", url, bob.token);
  check("owner", "another user cannot HEAD it", p.status === 403, p.status);
  p = await raw("DELETE", url, bob.token);
  check("owner", "another user cannot DELETE it", p.status === 403, p.status);
  p = await patch(url, api.service_role_key, 0, "0123456789");
  check("owner", "not even with the service_role key", p.status === 403, p.status);
  p = await patch(url, alice.token, 0, "01234");
  check("owner", "the creator can PATCH", p.status === 204 && p.headers.get("upload-offset") === "5", p.status);
  p = await patch(url, alice.token, 3, "34567");
  check("tus", "wrong Upload-Offset is a 409", p.status === 409, p.status);
  p = await patch(url, alice.token, 5, "56789-too-long");
  check("tus", "a chunk past Upload-Length is refused", p.status === 413, p.status);
  p = await patch(url, alice.token, 5, "56789");
  check("tus", "last chunk completes the upload", p.status === 204 && (await download(alice.c, "files", `${alice.id}/mine.txt`))?.toString() === "0123456789", p.status);

  // RLS is checked again when the upload completes
  const { url: u2 } = await create(alice.token, `${alice.id}/late.txt`, 4);
  await sql("drop policy files_insert on storage.objects");
  p = await patch(u2, alice.token, 0, "late");
  const listed = (await admin.storage.from("files").list(alice.id)).data?.some((f) => f.name === "late.txt");
  check("auth", "RLS is checked again on the last chunk", p.status === 403 && !listed, `${p.status} listed=${listed}`);
  await sql(policies.split(";")[0]);
}

// ------------------------------------------------------------------ upsert, concurrency, termination
{
  const path = `${alice.id}/up.txt`;
  let r = await tusUpload(Buffer.from("v1"), { token: alice.token, object: path, contentType: "text/plain" });
  check("upsert", "first version", r.status === 0, r.err?.message);
  r = await tusUpload(Buffer.from("v2"), { token: alice.token, object: path, contentType: "text/plain" });
  check("upsert", "same name without x-upsert is a 409", r.status === 409, `${r.status} ${r.err?.message}`);
  r = await tusUpload(Buffer.from("v2"), { token: alice.token, object: path, contentType: "text/plain", upsert: true });
  check("upsert", "x-upsert: true overwrites", r.status === 0 && (await download(alice.c, "files", path))?.toString() === "v2", r.err?.message);

  const a = crypto.randomBytes(MB), b = crypto.randomBytes(MB);
  const { url } = await create(alice.token, `${alice.id}/race.bin`, 2 * MB);
  const [p1, p2] = await Promise.all([patch(url, alice.token, 0, a), patch(url, alice.token, 0, b)]);
  const st = [p1.status, p2.status].sort().join();
  const head = await raw("HEAD", url, alice.token);
  check("lock", "two PATCHes at one offset: one wins, one 409", st === "204,409" && head.headers.get("upload-offset") === String(MB), `${st} ${head.headers.get("upload-offset")}`);
  const winner = p1.status === 204 ? a : b;
  const p3 = await patch(url, alice.token, MB, b);
  check("lock", "upload is not corrupted", p3.status === 204 && (await download(alice.c, "files", `${alice.id}/race.bin`))?.equals(Buffer.concat([winner, b])), p3.status);

  const { url: t } = await create(alice.token, `${alice.id}/term.txt`, 10);
  await patch(t, alice.token, 0, "01234");
  let err = null;
  await tus.Upload.terminate(t, { headers: { authorization: `Bearer ${alice.token}` }, retryDelays: [] }).catch((e) => (err = e));
  const h = await raw("HEAD", t, alice.token);
  const pp = await patch(t, alice.token, 5, "56789");
  check("delete", "DELETE terminates the upload (tus terminate)", !err && h.status === 404 && pp.status === 404, `${err?.message} ${h.status} ${pp.status}`);
  const listed = (await admin.storage.from("files").list(alice.id)).data?.some((f) => f.name === "term.txt");
  check("delete", "terminated upload leaves no object", !listed);
}

if (!process.env.KEEP) await tryOp("projects.delete", { project: P, confirm: true });
console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
