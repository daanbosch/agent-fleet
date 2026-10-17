// Minimal OIDC relying party for checking the Agent Fleet development IdP (ADR 0116) by hand,
// standing in for a member's app. No dependencies: Node 18+ built-ins only (http, crypto, fetch).
//
// Run it inside a workspace that has opted in (the AF_DEV_IDP_* variables are present):
//
//   node rp.mjs --selftest   non-browser checks: discovery, JWKS, token endpoint refusals
//   node rp.mjs              serve on 127.0.0.1:$PORT (default 3000), then open the port's
//                            preview URL in a browser signed in to the Console
//
// The callback it sends is AF_PREVIEW_URL_<PORT> + /callback (falling back to
// http://127.0.0.1:<PORT>), which is what the IdP's redirect allow-list expects. The result page
// shows the verified ID token claims, userinfo and whether replaying the code was refused; it
// never prints the client secret or raw tokens.
import http from "node:http";
import crypto from "node:crypto";

const PORT = Number(process.env.PORT || 3000);
const ISSUER = (process.env.AF_DEV_IDP_ISSUER || "").replace(/\/$/, "");
const CLIENT_ID = process.env.AF_DEV_IDP_CLIENT_ID || "";
const CLIENT_SECRET = process.env.AF_DEV_IDP_CLIENT_SECRET || "";
const BASE = (process.env[`AF_PREVIEW_URL_${PORT}`] || `http://127.0.0.1:${PORT}`).replace(/\/$/, "");
const REDIRECT_URI = `${BASE}/callback`;

const b64url = (buf) => Buffer.from(buf).toString("base64url");
const pending = new Map(); // state -> { nonce, verifier, at }

async function discovery() {
  const r = await fetch(`${ISSUER}/.well-known/openid-configuration`);
  if (r.status !== 200) throw new Error(`discovery: HTTP ${r.status}`);
  const d = await r.json();
  if (d.issuer !== ISSUER) throw new Error(`discovery: issuer ${d.issuer} != ${ISSUER}`);
  return d;
}

async function verifyIdToken(d, idToken, nonce) {
  const [h, p, s] = idToken.split(".");
  const header = JSON.parse(Buffer.from(h, "base64url"));
  const claims = JSON.parse(Buffer.from(p, "base64url"));
  if (header.alg !== "RS256") throw new Error(`id_token alg ${header.alg}`);
  const jwks = await (await fetch(d.jwks_uri)).json();
  const jwk = jwks.keys.find((k) => k.kid === header.kid);
  if (!jwk) throw new Error(`id_token kid ${header.kid} not in JWKS`);
  const ok = crypto.verify("RSA-SHA256", Buffer.from(`${h}.${p}`), crypto.createPublicKey({ key: jwk, format: "jwk" }), Buffer.from(s, "base64url"));
  if (!ok) throw new Error("id_token signature invalid");
  const now = Math.floor(Date.now() / 1000);
  const aud = Array.isArray(claims.aud) ? claims.aud : [claims.aud];
  const problems = [];
  if (claims.iss !== d.issuer) problems.push(`iss ${claims.iss}`);
  if (!aud.includes(CLIENT_ID)) problems.push(`aud ${claims.aud}`);
  if (!(claims.exp > now)) problems.push(`exp ${claims.exp} <= now ${now}`);
  if (claims.nonce !== nonce) problems.push("nonce mismatch");
  if (claims.af_dev !== true) problems.push("af_dev missing");
  if (problems.length) throw new Error(`id_token claims: ${problems.join(", ")}`);
  return { header: { alg: header.alg, kid: header.kid }, claims };
}

function page(res, status, title, body) {
  res.writeHead(status, { "content-type": "text/html; charset=utf-8" });
  const esc = (t) => String(t).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c]);
  res.end(`<!doctype html><title>${esc(title)}</title><h1>${esc(title)}</h1><pre>${esc(body)}</pre>
<p><a href="/login">sign in</a> · <a href="/login?prompt=none">sign in (prompt=none)</a> · <a href="/login?nopkce=1">sign in without PKCE (Spring-like)</a></p>`);
}

async function serve(req, res) {
  const url = new URL(req.url, BASE);
  try {
    if (url.pathname === "/") return page(res, 200, "dev IdP check", `issuer: ${ISSUER}\nclient_id: ${CLIENT_ID}\nredirect_uri: ${REDIRECT_URI}`);
    if (url.pathname === "/login") {
      const d = await discovery();
      const state = b64url(crypto.randomBytes(16));
      const nonce = b64url(crypto.randomBytes(16));
      const verifier = b64url(crypto.randomBytes(32));
      const pkce = url.searchParams.get("nopkce") !== "1";
      pending.set(state, { nonce, verifier: pkce ? verifier : null, at: Date.now() });
      const q = new URLSearchParams({ response_type: "code", client_id: CLIENT_ID, redirect_uri: REDIRECT_URI, scope: "openid email profile", state, nonce });
      if (pkce) { q.set("code_challenge", b64url(crypto.createHash("sha256").update(verifier).digest())); q.set("code_challenge_method", "S256"); }
      if (url.searchParams.get("prompt")) q.set("prompt", url.searchParams.get("prompt"));
      res.writeHead(302, { location: `${d.authorization_endpoint}?${q}` });
      return res.end();
    }
    if (url.pathname === "/callback") {
      if (url.searchParams.get("error")) return page(res, 400, "IdP returned an error", `${url.searchParams.get("error")}: ${url.searchParams.get("error_description") || ""}`);
      const p = pending.get(url.searchParams.get("state"));
      if (!p) return page(res, 400, "unknown state", "state did not match a login started here");
      pending.delete(url.searchParams.get("state"));
      const d = await discovery();
      const form = new URLSearchParams({ grant_type: "authorization_code", code: url.searchParams.get("code"), redirect_uri: REDIRECT_URI });
      if (p.verifier) form.set("code_verifier", p.verifier);
      const basic = Buffer.from(`${encodeURIComponent(CLIENT_ID)}:${encodeURIComponent(CLIENT_SECRET)}`).toString("base64");
      const tr = await fetch(d.token_endpoint, { method: "POST", headers: { authorization: `Basic ${basic}`, "content-type": "application/x-www-form-urlencoded" }, body: form });
      const tok = await tr.json();
      if (tr.status !== 200) return page(res, 502, "token endpoint refused", `HTTP ${tr.status} ${JSON.stringify(tok)}`);
      const id = await verifyIdToken(d, tok.id_token, p.nonce);
      const ur = await fetch(d.userinfo_endpoint, { headers: { authorization: `Bearer ${tok.access_token}` } });
      const userinfo = ur.status === 200 ? await ur.json() : `HTTP ${ur.status}`;
      // Replaying the same code must fail (single-use).
      const replay = await fetch(d.token_endpoint, { method: "POST", headers: { authorization: `Basic ${basic}`, "content-type": "application/x-www-form-urlencoded" }, body: form });
      return page(res, 200, "signed in", JSON.stringify({ id_token: id, userinfo, token_type: tok.token_type, expires_in: tok.expires_in, code_replay_status: replay.status, pkce: Boolean(p.verifier) }, null, 2));
    }
    page(res, 404, "not found", url.pathname);
  } catch (e) {
    page(res, 500, "check failed", e.message);
  }
}

async function selftest() {
  const results = [];
  const check = (name, ok, detail) => results.push(`${ok ? "PASS" : "FAIL"} ${name}${detail ? ` — ${detail}` : ""}`);
  const d = await discovery();
  check("discovery issuer", d.issuer === ISSUER, d.issuer);
  check("discovery RS256", (d.id_token_signing_alg_values_supported || []).includes("RS256"));
  check("discovery S256", (d.code_challenge_methods_supported || []).includes("S256"));
  const jwks = await (await fetch(d.jwks_uri)).json();
  const priv = jwks.keys.filter((k) => ["d", "p", "q", "dp", "dq", "qi"].some((f) => f in k));
  check("jwks has RSA keys", jwks.keys.length > 0 && jwks.keys.every((k) => k.kty === "RSA"), `${jwks.keys.length} key(s)`);
  check("jwks has no private parts", priv.length === 0);
  for (const k of jwks.keys) crypto.createPublicKey({ key: k, format: "jwk" }); // throws on a malformed key
  const bad = await fetch(d.token_endpoint, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ grant_type: "authorization_code", code: "x", redirect_uri: REDIRECT_URI, client_id: "afdev-nope", client_secret: "nope" }) });
  const badBody = await bad.json().catch(() => ({}));
  check("token refuses an unknown client", bad.status === 401 && badBody.error === "invalid_client", `HTTP ${bad.status} ${badBody.error}`);
  if (CLIENT_ID && CLIENT_SECRET) {
    const basic = Buffer.from(`${encodeURIComponent(CLIENT_ID)}:${encodeURIComponent(CLIENT_SECRET)}`).toString("base64");
    const g = await fetch(d.token_endpoint, { method: "POST", headers: { authorization: `Basic ${basic}`, "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ grant_type: "authorization_code", code: "not-a-code", redirect_uri: REDIRECT_URI }) });
    const gb = await g.json().catch(() => ({}));
    check("token: own client authenticates, bogus code refused", g.status === 400 && gb.error === "invalid_grant", `HTTP ${g.status} ${gb.error}`);
  } else {
    check("token with own client", true, "skipped: AF_DEV_IDP_CLIENT_ID/SECRET not set (workspace not opted in)");
  }
  console.log(results.join("\n"));
  process.exitCode = results.some((r) => r.startsWith("FAIL")) ? 1 : 0;
}

if (!ISSUER) { console.error("AF_DEV_IDP_ISSUER is not set (or pass it for --selftest)"); process.exit(2); }
if (process.argv.includes("--selftest")) await selftest();
else http.createServer(serve).listen(PORT, "127.0.0.1", () => console.log(`dev IdP check on ${BASE} (redirect_uri ${REDIRECT_URI})`));
