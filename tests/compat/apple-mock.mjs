// Mock of Apple's identity server for scripts/selftest.sh: /auth/keys (JWKS), /auth/token, and /mint (test helper).
import http from "node:http";
import crypto from "node:crypto";
import fs from "node:fs";
const [, , port, p8path] = process.argv;
const base = `http://127.0.0.1:${port}`;
const clientPub = crypto.createPublicKey(fs.readFileSync(p8path));
const { privateKey, publicKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
const jwk = { ...publicKey.export({ format: "jwk" }), kid: "mockkid", alg: "RS256", use: "sig" };
const b64 = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
const mint = (claims) => {
  const u = b64({ alg: "RS256", kid: "mockkid" }) + "." + b64(claims);
  return u + "." + crypto.sign("sha256", Buffer.from(u), privateKey).toString("base64url");
};
const now = () => Math.floor(Date.now() / 1000);
const SUB = "001234.apple-user", EMAIL = "relay123@privaterelay.appleid.com";
const body = (req) => new Promise((r) => { let s = ""; req.on("data", (d) => (s += d)).on("end", () => r(s)); });
http.createServer(async (req, res) => {
  const send = (code, o) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(o)); };
  if (req.url === "/auth/keys") return send(200, { keys: [jwk] });
  if (req.url === "/auth/token" && req.method === "POST") {
    const f = new URLSearchParams(await body(req));
    const [h, c, sig] = (f.get("client_secret") || "..").split(".");
    let ok = false, claims = {};
    try {
      ok = crypto.verify("sha256", Buffer.from(h + "." + c), { key: clientPub, dsaEncoding: "ieee-p1363" }, Buffer.from(sig, "base64url"));
      claims = JSON.parse(Buffer.from(c, "base64url"));
    } catch {}
    const hdr = ok ? JSON.parse(Buffer.from(h, "base64url")) : {};
    const problems = [];
    if (!ok) problems.push("bad client_secret signature");
    if (hdr.kid !== "KEYID12345") problems.push("kid " + hdr.kid);
    if (claims.iss !== "TEAMID1234") problems.push("iss " + claims.iss);
    if (claims.sub !== f.get("client_id")) problems.push("sub != client_id");
    if (claims.aud !== base) problems.push("aud " + claims.aud);
    if (!(claims.exp > now() && claims.exp - claims.iat <= 15777000)) problems.push("exp");
    if (f.get("grant_type") !== "authorization_code" || !f.get("redirect_uri")) problems.push("grant/redirect");
    if (problems.length) { console.log("token: invalid_client", problems); return send(400, { error: "invalid_client", error_description: problems.join("; ") }); }
    if (f.get("code") !== "good-code") return send(400, { error: "invalid_grant", error_description: "code expired" });
    console.log("token: ok, client secret verified");
    return send(200, { access_token: "apple-at", token_type: "Bearer", expires_in: 3600, refresh_token: "apple-rt",
      id_token: mint({ iss: base, aud: f.get("client_id"), sub: SUB, email: EMAIL, email_verified: "true", is_private_email: "true", iat: now(), exp: now() + 600 }) });
  }
  if (req.url === "/mint" && req.method === "POST") {
    const o = JSON.parse(await body(req));
    return send(200, { id_token: mint({ iss: o.iss ?? base, aud: o.aud, sub: o.sub ?? SUB, email: o.email ?? EMAIL, email_verified: true, is_private_email: true, iat: now(), exp: now() + (o.exp ?? 600), ...(o.nonce ? { nonce: o.nonce } : {}) }) });
  }
  send(404, { error: "not_found" });
}).listen(+port, "127.0.0.1", () => console.log("mock apple on", base));
