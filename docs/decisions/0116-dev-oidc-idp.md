# 0116. A development OIDC IdP on the Console origin, so an app that signs in with Google can log in on a preview host by changing configuration only

English | [日本語](0116-dev-oidc-idp.ja.md)

- Status: **accepted** (2026-10-16). The endpoints, the per-workspace registration, the redirect
  allow-list, the env injection and the guide are implemented. **Not yet run against a real
  Auth.js or Spring Boot app on a preview host**: that needs a deployment with a `PreviewDomain`
  (the PR lists what the live run has to check).
- Follow-ups: #2222 (Console screen for the opt-in and the credentials), #2223 (test personas)
- Related: [0062-preview-subdomain.md](0062-preview-subdomain.md) (decisions 3, 7, 8, 14, 15 and 17:
  the slug, the separate origin, the ACL this one reuses) / [0005](0005-envelope-custodian.md) (the
  custodian that seals the keys and secrets; see `sealedColumns`)

## Background

An app behind a preview host that uses Google sign-in cannot complete it: Google accepts only
redirect URIs registered on the OAuth client, and the preview hostname changes at every start
(0062 decision 3). The routes that exist today do not close the gap:

- **Pinning the slug** (0062 decision 3) still needs one redirect URI per developer per app on
  the Google client, and that client is often owned by another team or shared with production.
- **The browser pane on a registered `http://localhost:…` callback** fails because Google refuses
  the sign-in inside the pane's headless, CDP-driven Chromium.
- **Overriding Google's hostnames inside the workspace (hosts file plus a private CA)** was
  rejected: the authorisation step runs in the user's own browser, `dev` has no root, and a CA
  that can sign `google.com` inside a container that also runs `gcloud` and `gh` is not
  acceptable.

The Control Plane is only an OIDC *client* today (`internal/auth/oauth_oidc.go`): it has no
signing key, no rotation and no JWKS. This record adds the server side, small and loud about
being a development tool.

## Decision

### 1. One issuer on the Console origin: `{PUBLIC_BASE_URL}/dev-idp`

The app switches issuer, client id and client secret in its own configuration; nothing else in
the app changes. The IdP never claims to be Google: the issuer is its own, and every ID token and
userinfo answer carries `af_dev: true`.

It lives on the **Console origin only**. A preview host is relayed to the user's app, so
`/dev-idp/…` there is the app's route, never ours (0062 decision 8). The endpoints that an app's
*server* calls (discovery, token, userinfo, jwks) are exempt from `authGate` because that server
has no session; `authorize` is inside it.

Endpoints:

| Path under `/dev-idp` | Auth | Purpose |
|---|---|---|
| `/.well-known/openid-configuration` | none | discovery |
| `/jwks` | none | public keys |
| `/authorize` | Console session (`authGate`) | the signed-in member is the user; no password prompt |
| `/token` | client secret | code → ID token + access token |
| `/userinfo` | bearer access token | the same claims |

`token`, `userinfo` and `jwks` are reachable from inside the workspace the way every other
workspace → CP call is: a server in the container calls the same public Console URL the browser does
(`AF_CP_BASE_URL`), so no second listener is involved.

### 2. Signing: RS256

openid-client (Auth.js) and Spring Security default to RS256, so it is the only algorithm
offered. 2048-bit keys, `kid` is a thumbprint of the public key.

### 3. Enablement: deployment flag, then workspace opt-in, off by default

`AF_DEV_IDP=1` makes the deployment serve it (it also needs `PUBLIC_BASE_URL`, the issuer's
origin; without it the IdP stays off and the CP says so at start). Until then **every endpoint
answers 404**, whatever the method. Then each workspace opts in with the `devIdp` workspace
setting (`PUT /api/env/ws-settings`); for a workspace that has not, `authorize` refuses and
`token` answers `invalid_client`. The setting cannot be stored on a deployment that does not serve
the IdP (409): a stored "on" would light up the day the flag is set, decided by nobody that day.

An operator gate per tenant (a tenant limit beside `allow_agent_self_update`) was considered and
left out of the first change; the deployment flag is the operator's switch. It belongs with the
Console screen (#2222) if it turns out to be wanted.

### 4. Keys: sealed at rest, rotated every 30 days, retired keys published for a day

- Stored in `dev_idp_key`: the PKCS#8 private key sealed by the key custodian under the
  `deployment` key reference (the same as the engine tokens: it belongs to the deployment, not a
  tenant), and the PKIX public key in the clear, so serving the JWKS never unseals anything. The
  column is in `sealedColumns`, so `rewrap-keys` covers it.
- The first request creates the key. Past 30 days the next signing creates a new one and retires
  the old; a retired key stays in the JWKS for 24 hours, far longer than any token lives, then is
  deleted. Two Control Planes that race to create one both succeed; signing uses the newest and
  both are published.
- The private key is held in three places: the sealed column, this process's memory and the
  signature it makes. It is never logged, returned or written to an audit row (a test searches
  the token response, JWKS, discovery, logs and audit rows for it).

### 5. Client registration: one per workspace; the container gets the values as env

- `client_id` (`afdev-…`) and a 32-byte random secret are generated by the Control Plane when the
  workspace opts in, and stored in `dev_idp_client` with the secret sealed under the tenant's key,
  because the container needs it **again at every start**. `aud` is that client id, and a code or
  token is honoured only for the client it was issued to, whose workspace is still opted in.
- Turning the option off deletes the registration; turning it on again issues a new client id and
  secret. That is the rotation: there is no separate "rotate" call to forget.
- Injected as `AF_DEV_IDP_ISSUER`, `AF_DEV_IDP_WELL_KNOWN` (discovery URL, which Auth.js's
  `wellKnown` wants), `AF_DEV_IDP_CLIENT_ID` and `AF_DEV_IDP_CLIENT_SECRET`, in the same place as
  `AF_PREVIEW_URL_{port}` (`workspaceExtraEnv`). The names say `DEV_IDP` on purpose: a production
  configuration that reads them is visible in a grep, and they are not `GOOGLE_*`, so nobody maps
  them silently onto the production variables. A running container keeps the env it started with;
  the endpoints follow the setting at once.
- The ws-settings answer carries the issuer and the client id, never the secret.

### 6. Claims shaped like Google's

`sub`, `email`, `email_verified`, `name`, `picture`, `hd`, limited by the granted scopes (`sub`
always; `email` brings `email`, `email_verified`, `hd`; `profile` brings `name`, `picture`).

- When the member has a linked identity with realm `https://accounts.google.com`
  (`IdentityLink.Realm`), `sub` and the address are the **real Google values**, so the app's
  existing user rows match. `hd` is the address's domain, except for `gmail.com` and
  `googlemail.com`: Google sets it only for a Workspace account.
- Otherwise `sub` is `af-` plus a hash of the identity id: stable per person, and unable to equal a
  Google `sub` (numeric). The address is the identity's.
- `picture` is not known to the Control Plane and is omitted rather than invented.
- `iss` is the issuer, `aud` and `azp` the client id, `nonce` is passed through, `at_hash` is
  set, and tokens live **10 minutes**.

### 7. `authorize`: the Console member is the user, and the redirect is vetted before anything else

The caller must resolve as an active member of the **workspace's own tenant**, from an allowed
network and provider (the same resolver as `/preview-auth`), and must be someone who may open this
workspace's preview: the owner, or a tenant colleague while the owner has `previewTenantShare` on
(0062 decisions 14 and 15). A colleague therefore signs in as themselves on the owner's shared
preview.

`redirect_uri` is accepted only if it is one of:

- `https://{slug}-{port}.{PreviewDomain}/…` for **this workspace's current slug** and a port on its
  preview list. Another member's slug, a slug from before a restart, and a port off the list fail.
- `http://127.0.0.1:{port}/…` or `http://localhost:{port}/…`, with an explicit port.

The match is on the **canonical spelling**: an upper-case host, a trailing dot, an explicit `:443`,
userinfo, a fragment, a backslash, a suffix such as `host.evil.example` or `localhost:3000@evil`
are refused rather than normalised, because every normalisation is a chance for this check and the
browser to disagree. A request whose client or redirect cannot be trusted gets a **400 page, never
a redirect** (the open redirect this endpoint exists not to be), with one message for every cause.
Only after the redirect is trusted do protocol errors go back to it, with `state`. The response
carries `iss` (RFC 9207) so a client can reject a code from another issuer.

At the `token` step `redirect_uri` must equal what `authorize` carried, byte for byte, and the whole
permission is decided again then: a restart in the minute between moves the slug and the code dies.

### 8. `token`: confidential client, single-use short-lived code, PKCE where it is offered

- The client authenticates with `client_secret_basic` (credentials form-urlencoded before Base64,
  RFC 6749 §2.3.1) or `client_secret_post`; both at once, or the secret in the URL, is refused, and
  an unknown client and a wrong secret are the same answer.
- A code lives **60 seconds**, is stored as a hash, and is spent by one conditional `UPDATE` so
  concurrent redemptions yield exactly one winner. A second presentation is `invalid_grant` and
  also deletes the access tokens that code produced (RFC 6749 §4.1.2). A code presented by another
  client is refused and, being spent, is gone for the rightful one too.
- **PKCE: S256 only; enforced whenever the request carried a challenge; optional otherwise.**
  Spring Security sends no challenge for a confidential client by default, and this client is
  always confidential (the secret is required at `token`), with `state` and `nonce` as the other
  defences. A verifier sent for a request that had no challenge is refused (a downgrade probe,
  RFC 9700 §4.8); `plain` is refused. Making PKCE mandatory is a one-line change if the Spring case
  turns out not to matter.
- The access token is opaque (a stored hash, revocable with its code), not a JWT, so `userinfo`
  needs no key and a replay can take it down.

### 9. Audit

`workspace.dev_idp` (on/off, by whom) is written before the setting changes. `workspace.dev_idp_token`
is written for **every issuance, before the response leaves**: no row, no token. It names the client
and the subject, never a token or a key. Authorisation refusals are logged without the URL.

### 10. Personas are not in this change

Optional test identities (`admin@…`, `viewer@…`) need a consent page to choose from, and the IdP has
none: `authorize` approves silently as the signed-in member. The constraints are fixed now so the
follow-up (#2223) cannot get them wrong: a persona's address is on the reserved `example.test`
domain (RFC 2606) and its `sub` has a `persona:` prefix, so it can never claim a real member's
address or a Google `sub`.

### 11. The risk that matters, and what is done about it

The danger is an app that ships to production still trusting this issuer: then anyone with a Console
account can mint any identity the Control Plane will vouch for. What stands in the way:

- the issuer path is distinctive (`/dev-idp`), and the env names say `DEV_IDP`;
- the audience is one workspace's client id, so a token from here fails for any other client;
- tokens are 10 minutes, codes 60 seconds, every token carries `af_dev: true`, and a relying party can
  check it;
- the deployment is off by default and the workspace is off by default;
- the guide says it in one line (member/10): never let a production configuration trust this issuer.

## Rejected

- **A fixed redirect relay on the Console origin for real Google.** Frameworks such as Spring
  Security validate the returning URL against `redirect_uri`, so the relay breaks the very apps it is
  for. Out of scope together with Google API access tokens (Calendar, Drive).
- **Hosts and CA override inside the workspace.** See Background.
- **A password or consent prompt.** The member is already signed in to the Console; asking again
  trains clicking through. A consent page comes back only for personas (#2223).
- **Implicit and hybrid flows, `form_post`, refresh tokens, `plain` PKCE.** Nothing an app that uses
  Google sign-in through Auth.js or Spring needs, each a larger surface.
- **JWT access tokens.** They would need the key at `userinfo` and could not be revoked with the code.
- **Mandatory PKCE.** See decision 8.
- **One key per tenant.** The IdP is a deployment-level development tool; a per-tenant key would
  multiply rotation for no isolation the client binding does not already give.

## Consequences

- Control Plane: `dev_idp.go` (endpoints), `dev_idp_client.go` (registration, claims, the redirect
  allow-list), `dev_idp_keys.go` (keyring and RS256), `store_dev_idp.go`, `ws_settings.go` (`devIdp`).
  Migrations for **both sqlite and Postgres**: `0101_dev_idp.sql` / `0086_dev_idp.sql`, forward
  compatible; no downgrade path.
- Deployment: `AF_DEV_IDP=1` and `PUBLIC_BASE_URL`; nothing else (no certificate, no DNS).
- A deployment with several Control Plane replicas shares the key table; replicas refresh their view
  of it once a minute.
- Guide: `guide/member/10-integrations.md` (both languages): Auth.js and Spring Boot setup, and the
  production warning.

## Notes

- Not measured: sign-in of a real Next.js (Auth.js) app and a real Spring Boot app on a preview
  host, and the same app on `127.0.0.1` in the browser pane. The tests drive the protocol with a
  hand-written relying party (signature, issuer, audience, expiry), not the real libraries.
