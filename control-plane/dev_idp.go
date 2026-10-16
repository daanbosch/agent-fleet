// dev_idp.go — a development OIDC IdP served by the Control Plane on the Console origin
// (ADR 0116), so an app that signs in with Google can log in on a preview host by changing
// its issuer / client id / client secret, without touching the Google Cloud console.
//
//	app (container, :3000)  ──► https://<console>/dev-idp/authorize   (browser; inside authGate)
//	                        ◄── redirect_uri?code=…                    (own preview host / localhost only)
//	app server              ──► /dev-idp/token, /jwks, /userinfo       (server to server; no session)
//
// It never claims to be Google: the issuer is its own, every token carries `af_dev: true`, and
// the audience is the one workspace's client id. Off unless the deployment sets AF_DEV_IDP and
// the workspace opts in; while off, every route answers 404.
//
// The preview host serves none of this (ADR 0062 decision 8): those hostnames are relayed to
// the user's app, so this code is reachable on the Console origin only.
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

const (
	devIdPCodeTTL  = 60 * time.Second
	devIdPTokenTTL = 10 * time.Minute

	devIdPAuthorizePath = devIdPPath + "/authorize"
	devIdPTokenPath     = devIdPPath + "/token"
	devIdPUserinfoPath  = devIdPPath + "/userinfo"
	devIdPJWKSPath      = devIdPPath + "/jwks"
	devIdPDiscoveryPath = devIdPPath + "/.well-known/openid-configuration"
)

type devIdPAPI struct {
	mgr  *manager
	keys *devIdPKeyring
	now  func() time.Time
}

func newDevIdPAPI(cfg config) devIdPAPI {
	m := cfg.mgr
	return devIdPAPI{mgr: m, keys: newDevIdPKeyring(m.store, m, time.Now), now: time.Now}
}

// registerDevIdPRoutes mounts the endpoints. authorize is deliberately NOT exempt from
// authGate (the signed-in Console member is the user); the others are called by the app's own
// server, which has no session, and authenticate by client secret / bearer token instead.
// Method checks live in the handlers so that a disabled IdP answers 404 whatever the method.
func registerDevIdPRoutes(mux *http.ServeMux, cfg config) { newDevIdPAPI(cfg).mount(mux) }

func (a devIdPAPI) mount(mux *http.ServeMux) {
	exemptExact(devIdPDiscoveryPath, devIdPJWKSPath, devIdPTokenPath, devIdPUserinfoPath)
	mux.HandleFunc(devIdPDiscoveryPath, a.gate(http.MethodGet, a.discovery))
	mux.HandleFunc(devIdPJWKSPath, a.gate(http.MethodGet, a.jwks))
	mux.HandleFunc(devIdPAuthorizePath, a.gate(http.MethodGet, a.authorize))
	mux.HandleFunc(devIdPTokenPath, a.gate(http.MethodPost, a.token))
	mux.HandleFunc(devIdPUserinfoPath, a.gate("GET, POST", a.userinfo))
}

// gate answers 404 while the deployment does not serve the IdP, then 405 for a wrong method.
func (a devIdPAPI) gate(methods string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.mgr.devIdPIssuer() == "" {
			http.NotFound(w, r)
			return
		}
		ok := false
		for _, m := range strings.Split(methods, ", ") {
			ok = ok || r.Method == m
		}
		if !ok {
			w.Header().Set("Allow", methods)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// devIdPDiscovery is the OpenID Provider Metadata document (OpenID Connect Discovery 1.0 §3).
type devIdPDiscovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ResponseTypes         []string `json:"response_types_supported"`
	ResponseModes         []string `json:"response_modes_supported"`
	GrantTypes            []string `json:"grant_types_supported"`
	SubjectTypes          []string `json:"subject_types_supported"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
	Scopes                []string `json:"scopes_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	Claims                []string `json:"claims_supported"`
	// RFC 9207: authorize responses carry `iss`, which lets a client refuse a code that came
	// from some other issuer.
	IssParameterSupported bool `json:"authorization_response_iss_parameter_supported"`
}

func (a devIdPAPI) discovery(w http.ResponseWriter, _ *http.Request) {
	iss := a.mgr.devIdPIssuer()
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, devIdPDiscovery{
		Issuer: iss, AuthorizationEndpoint: iss + "/authorize", TokenEndpoint: iss + "/token",
		UserinfoEndpoint: iss + "/userinfo", JWKSURI: iss + "/jwks",
		ResponseTypes: []string{"code"}, ResponseModes: []string{"query"},
		GrantTypes: []string{"authorization_code"}, SubjectTypes: []string{"public"},
		SigningAlgs: []string{"RS256"}, Scopes: []string{"openid", "email", "profile"},
		TokenAuthMethods:     []string{"client_secret_basic", "client_secret_post"},
		CodeChallengeMethods: []string{"S256"},
		Claims: []string{"sub", "iss", "aud", "exp", "iat", "nonce", "email", "email_verified",
			"name", "picture", "hd", "af_dev"},
		IssParameterSupported: true,
	})
}

func (a devIdPAPI) jwks(w http.ResponseWriter, r *http.Request) {
	set, err := a.keys.jwks(r.Context())
	if err != nil {
		log.Printf("dev idp: jwks: %v", err)
		http.Error(w, "dev idp unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, set)
}

// client is a registration with the workspace and settings it belongs to.
type devIdPClientCtx struct {
	row store.DevIdPClient
	ws  store.Workspace
	st  wsSettings
}

// loadClient resolves a client id to its workspace. ok is false for an unknown client AND for
// one whose workspace has not opted in (or no longer exists): callers do not tell them apart.
func (a devIdPAPI) loadClient(r *http.Request, clientID string) (devIdPClientCtx, bool) {
	ctx := r.Context()
	if clientID == "" {
		return devIdPClientCtx{}, false
	}
	row, ok, err := a.mgr.store.GetDevIdPClientByID(ctx, clientID)
	if err != nil || !ok {
		return devIdPClientCtx{}, false
	}
	ws, ok, err := a.mgr.store.GetWorkspaceByMembership(ctx, row.MembershipID)
	if err != nil || !ok || ws.ID != row.WorkspaceID {
		return devIdPClientCtx{}, false
	}
	st := parseWSSettings(mustSettings(ctx, a.mgr, ws.ID))
	if !st.DevIdP {
		return devIdPClientCtx{}, false
	}
	return devIdPClientCtx{row: row, ws: ws, st: st}, true
}

// authorizeRefuse is the answer for a request whose client or redirect_uri cannot be trusted:
// shown to the person, never redirected (redirecting to an unvetted URI is the open redirect
// this endpoint exists not to be). One message for every cause, so it confirms nothing.
func authorizeRefuse(w http.ResponseWriter, why string) {
	log.Printf("dev idp: authorize refused: %s", why)
	noStore(w)
	http.Error(w, "dev-idp: this sign-in request was refused (unknown client, or a redirect_uri this client may not use)", http.StatusBadRequest)
}

func (a devIdPAPI) authorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	cc, ok := a.loadClient(r, q.Get("client_id"))
	if !ok {
		authorizeRefuse(w, "client")
		return
	}
	id := a.mgr.resolveIdentity(r)
	if id.key == "" {
		writeAPIErr(w, &apiError{http.StatusUnauthorized, "unauthenticated", "no gateway identity"})
		return
	}
	// The caller must be an active member of the workspace's OWN tenant, from an allowed
	// network, through an allowed provider: the same resolver as every API call.
	ident, mv, aerr := a.mgr.resolveMembership(ctx, id.key, id.email, cc.ws.TenantID)
	if aerr != nil {
		authorizeRefuse(w, "membership")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if !a.mgr.devIdPRedirectAllowed(ctx, cc.ws, cc.st, mv.MembershipID, redirectURI) {
		authorizeRefuse(w, "redirect_uri")
		return
	}
	// From here the redirect target is trusted, so protocol errors go back to the app.
	state := q.Get("state")
	fail := func(code, desc string) {
		a.redirectBack(w, r, redirectURI, state, url.Values{"error": {code}, "error_description": {desc}})
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if mode := q.Get("response_mode"); mode != "" && mode != "query" {
		fail("invalid_request", "only response_mode=query is supported")
		return
	}
	// Re-authentication cannot be honoured: the Console session is the only authentication
	// there is, and its age is not carried into the ID token (no auth_time). Say so rather
	// than let a max_age / prompt=login test in the app pass by accident.
	if prompt := q.Get("prompt"); prompt != "" && prompt != "none" {
		fail("invalid_request", "prompt="+prompt+" is not supported")
		return
	}
	if q.Has("max_age") {
		fail("invalid_request", "max_age is not supported")
		return
	}
	scope := q.Get("scope")
	if !strings.Contains(" "+scope+" ", " openid ") {
		fail("invalid_scope", "the openid scope is required")
		return
	}
	challenge := q.Get("code_challenge")
	if challenge != "" && q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "code_challenge_method must be S256")
		return
	}
	if challenge == "" && q.Get("code_challenge_method") != "" {
		fail("invalid_request", "code_challenge_method without code_challenge")
		return
	}
	if challenge != "" && !validPKCEChallenge(challenge) {
		fail("invalid_request", "malformed code_challenge")
		return
	}
	code := "afdevcode-" + devIdPRandom(32)
	now := a.now()
	err := a.mgr.store.InsertDevIdPGrant(ctx, store.DevIdPGrant{
		Hash: devIdPHash(code), Kind: store.DevIdPGrantCode,
		ClientID: cc.row.ClientID, WorkspaceID: cc.ws.ID, MembershipID: mv.MembershipID, IdentityID: ident.ID,
		RedirectURI: redirectURI, Challenge: challenge, Nonce: q.Get("nonce"), Scope: scope,
		Claims:    devIdPProfileJSON(devIdPScopedClaims(devIdPClaims(ctx, a.mgr.store, ident), scope)),
		ExpiresAt: now.Add(devIdPCodeTTL).Unix(),
	}, now.Unix())
	if err != nil {
		log.Printf("dev idp: store code: %v", err)
		fail("server_error", "could not issue a code")
		return
	}
	a.redirectBack(w, r, redirectURI, state, url.Values{"code": {code}})
}

// redirectBack sends the browser to the vetted redirect_uri with the response parameters.
func (a devIdPAPI) redirectBack(w http.ResponseWriter, r *http.Request, redirectURI, state string, extra url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		authorizeRefuse(w, "redirect_uri unparsable")
		return
	}
	q := u.Query()
	for k, v := range extra {
		q[k] = v
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", a.mgr.devIdPIssuer())
	u.RawQuery = q.Encode()
	noStore(w)
	// The app's page may carry the code in Referer to whatever it loads next: the code is
	// single-use and expires in a minute, and this keeps it off third parties anyway.
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func validPKCEChallenge(s string) bool {
	if len(s) != 43 { // base64url of a SHA-256 digest, unpadded
		return false
	}
	for _, c := range []byte(s) {
		if !isPKCEChar(c) || c == '.' || c == '~' {
			return false
		}
	}
	return true
}

func isPKCEChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func validPKCEVerifier(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		if !isPKCEChar(c) {
			return false
		}
	}
	return true
}

// --- token --------------------------------------------------------------------

// devIdPTokenError is the RFC 6749 §5.2 error body.
type devIdPTokenError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func tokenError(w http.ResponseWriter, status int, code, desc string) {
	noStore(w)
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="dev-idp", charset="UTF-8"`)
	}
	writeJSON(w, status, devIdPTokenError{Error: code, Description: desc})
}

// clientCredentials reads client_secret_basic or client_secret_post. Offering both is an
// error (RFC 6749 §2.3: one method per request).
func clientCredentials(r *http.Request) (id, secret string, ok bool) {
	user, pass, basic := r.BasicAuth()
	postID, postSecret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	switch {
	case basic && postSecret != "":
		return "", "", false
	case basic:
		// The credentials are form-urlencoded before Base64 (RFC 6749 §2.3.1).
		u, err1 := url.QueryUnescape(user)
		p, err2 := url.QueryUnescape(pass)
		if err1 != nil || err2 != nil {
			return "", "", false
		}
		return u, p, true
	default:
		return postID, postSecret, postSecret != ""
	}
}

func (a devIdPAPI) token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "unreadable form body")
		return
	}
	clientID, secret, ok := clientCredentials(r)
	if !ok {
		tokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	cc, found := a.loadClient(r, clientID)
	// Compare even when the client is unknown, so the two failures take the same path.
	want := ""
	if found {
		var err error
		if want, err = a.mgr.openTenantSecret(ctx, cc.row.SecretEnc, cc.row.KeyRef); err != nil {
			log.Printf("dev idp: client secret unreadable (client=%s): %v", clientID, err)
			tokenError(w, http.StatusInternalServerError, "server_error", "client secret unreadable")
			return
		}
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(want)) != 1 || !found || want == "" {
		tokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	code := r.PostForm.Get("code")
	now := a.now()
	grant, state, err := a.mgr.store.ConsumeDevIdPCode(ctx, devIdPHash(code), now.Unix())
	if err != nil {
		log.Printf("dev idp: consume code: %v", err)
		tokenError(w, http.StatusInternalServerError, "server_error", "could not redeem the code")
		return
	}
	switch state {
	case store.DevIdPCodeUnknown:
		tokenError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown or expired")
		return
	case store.DevIdPCodeReplayed:
		// A spent code presented again: whoever holds the tokens it produced may not be who
		// was meant to (RFC 6749 §4.1.2), so take them down.
		_ = a.mgr.store.RevokeDevIdPCode(ctx, grant.Hash, now.Add(devIdPTokenTTL).Unix())
		tokenError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
		return
	}
	if grant.ClientID != cc.row.ClientID {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
		return
	}
	// Exact match with what authorize carried (RFC 6749 §4.1.3): not a prefix, not a
	// normalised form.
	if r.PostForm.Get("redirect_uri") != grant.RedirectURI {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	verifier := r.PostForm.Get("code_verifier")
	if grant.Challenge != "" {
		sum := sha256.Sum256([]byte(verifier))
		if !validPKCEVerifier(verifier) || subtle.ConstantTimeCompare([]byte(devIdPB64.EncodeToString(sum[:])), []byte(grant.Challenge)) != 1 {
			tokenError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
			return
		}
	} else if verifier != "" {
		// A verifier for a code that never had a challenge is a downgrade probe, not a client
		// bug worth tolerating (RFC 9700 §4.8).
		tokenError(w, http.StatusBadRequest, "invalid_grant", "code_verifier was sent but the request had no code_challenge")
		return
	}
	// The permission to sign in is re-decided now, not trusted from the minute before.
	if !a.mgr.devIdPRedirectAllowed(ctx, cc.ws, cc.st, grant.MembershipID, grant.RedirectURI) {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "this sign-in is no longer permitted")
		return
	}
	signer, err := a.keys.signer(ctx)
	if err != nil {
		log.Printf("dev idp: signing key: %v", err)
		tokenError(w, http.StatusInternalServerError, "server_error", "no signing key")
		return
	}
	access := "afdevtok-" + devIdPRandom(32)
	atHash := sha256.Sum256([]byte(access))
	profile := devIdPParseProfile(grant.Claims)
	idToken, err := signer.sign(devIdPIDToken{
		devIdPProfile: profile,
		Iss:           a.mgr.devIdPIssuer(), Aud: cc.row.ClientID, Azp: cc.row.ClientID,
		Iat: now.Unix(), Exp: now.Add(devIdPTokenTTL).Unix(),
		AtHash: devIdPB64.EncodeToString(atHash[:16]), Nonce: grant.Nonce, AfDev: true,
	})
	if err != nil {
		log.Printf("dev idp: sign: %v", err)
		tokenError(w, http.StatusInternalServerError, "server_error", "could not sign the token")
		return
	}
	// Audited before anything leaves: no row, no token. The row names the subject and the
	// client, never a token or a key.
	if err := a.mgr.store.InsertAudit(ctx, store.AuditLog{
		ID: store.NewID(), TenantID: cc.ws.TenantID, ActorKind: "user", ActorID: grant.IdentityID,
		Action: "workspace.dev_idp_token", Target: cc.ws.ID,
		Detail: "client=" + cc.row.ClientID + " sub=" + strconv.Quote(profile.Sub), At: store.NowTS(),
	}); err != nil {
		log.Printf("dev idp: audit: %v", err)
		tokenError(w, http.StatusInternalServerError, "server_error", "audit unavailable")
		return
	}
	if err := a.mgr.store.InsertDevIdPGrant(ctx, store.DevIdPGrant{
		Hash: devIdPHash(access), Kind: store.DevIdPGrantAccess, Parent: grant.Hash,
		ClientID: grant.ClientID, WorkspaceID: grant.WorkspaceID, MembershipID: grant.MembershipID,
		IdentityID: grant.IdentityID, Scope: grant.Scope, Claims: grant.Claims,
		ExpiresAt: now.Add(devIdPTokenTTL).Unix(),
	}, now.Unix()); err != nil {
		if errors.Is(err, store.ErrDevIdPCodeRevoked) {
			tokenError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
			return
		}
		log.Printf("dev idp: store access token: %v", err)
		tokenError(w, http.StatusInternalServerError, "server_error", "could not issue a token")
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, devIdPTokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(devIdPTokenTTL.Seconds()),
		IDToken: idToken, Scope: grant.Scope,
	})
}

// devIdPIDToken is the ID token payload: the profile claims plus the registered ones.
type devIdPIDToken struct {
	devIdPProfile
	Iss    string `json:"iss"`
	Aud    string `json:"aud"`
	Azp    string `json:"azp"`
	Iat    int64  `json:"iat"`
	Exp    int64  `json:"exp"`
	AtHash string `json:"at_hash"`
	Nonce  string `json:"nonce,omitempty"`
	AfDev  bool   `json:"af_dev"`
}

type devIdPTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	IDToken     string `json:"id_token"`
	Scope       string `json:"scope"`
}

// --- userinfo -----------------------------------------------------------------

func (a devIdPAPI) userinfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deny := func() {
		noStore(w)
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "invalid_token", http.StatusUnauthorized)
	}
	tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || tok == "" {
		deny()
		return
	}
	grant, ok, err := a.mgr.store.GetDevIdPAccess(ctx, devIdPHash(tok), a.now().Unix())
	if err != nil || !ok {
		deny()
		return
	}
	// The client must still be registered and opted in.
	if _, ok := a.loadClient(r, grant.ClientID); !ok {
		deny()
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, devIdPUserinfo{devIdPProfile: devIdPParseProfile(grant.Claims), AfDev: true})
}

type devIdPUserinfo struct {
	devIdPProfile
	AfDev bool `json:"af_dev"`
}
