// dev_idp_persona.go — test personas for the development OIDC IdP (ADR 0116 decisions 9 and 10,
// and the dated note on #2223): per-workspace identities a member can choose on the consent page
// to exercise role checks in the app they are building.
//
// A persona is only ever a handle (and a display name). Its address is <handle>@example.test
// (RFC 2606, never a real mailbox) and its sub is "persona:<handle>", a prefix neither a Google
// sub (numeric) nor the derived member subject ("af-…") can have. The handle is restricted to
// [a-z0-9._-] and stored as typed, never normalised, so no case or Unicode folding can turn it
// into someone else's address; the display name reaches the `name` claim only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/k-k1/agent-fleet/control-plane/internal/auth"
	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// devIdPConsentTTL is how long the consent page may stay open.
const devIdPConsentTTL = 5 * time.Minute

const (
	devIdPPersonaDomain = "example.test"
	devIdPPersonaPrefix = "persona:"
	devIdPPersonaMax    = 20
	devIdPConsentPath   = devIdPPath + "/consent"
)

var devIdPPersonaHandle = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func devIdPPersonaEmail(handle string) string { return handle + "@" + devIdPPersonaDomain }

// devIdPPersonaProfile is the claim set a persona signs in with.
func devIdPPersonaProfile(p store.DevIdPPersona) devIdPProfile {
	name := p.Name
	if name == "" {
		name = p.Handle
	}
	return devIdPProfile{
		Sub: devIdPPersonaPrefix + p.Handle, Email: devIdPPersonaEmail(p.Handle),
		EmailVerified: true, Name: name,
	}
}

// validPersonaName: a display name is free text, so the only things refused are what would make
// it ambiguous on screen or in a log (control, bidi and other format characters).
func validPersonaName(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 64 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// personaUsable re-checks at issue time that the persona still cannot be a real person: the
// address is on example.test by construction, but a deployment that really has such members
// (a test tenant) must not have a persona sign in as one of them.
func (m *manager) personaUsable(ctx context.Context, p store.DevIdPPersona) (bool, error) {
	if !devIdPPersonaHandle.MatchString(p.Handle) {
		return false, nil
	}
	taken, err := m.store.DevIdPEmailTaken(ctx, devIdPPersonaEmail(p.Handle))
	return !taken, err
}

// --- management API (Console editor) --------------------------------------------

type devIdPPersonaJSON struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Sub    string `json:"sub"`
}

// devIdPPersonaList is the answer of every persona endpoint: the whole list after the change.
type devIdPPersonaList struct {
	Personas []devIdPPersonaJSON `json:"personas"`
	Max      int                 `json:"max"`
	Domain   string              `json:"domain"`
}

func personaJSON(p store.DevIdPPersona) devIdPPersonaJSON {
	pr := devIdPPersonaProfile(p)
	return devIdPPersonaJSON{ID: p.ID, Handle: p.Handle, Name: pr.Name, Email: pr.Email, Sub: pr.Sub}
}

func (a wsSettingsAPI) personasServed(w http.ResponseWriter, r *http.Request) bool {
	if a.mgr.devIdPIssuer(r.Context()) == "" {
		writeAPIErr(w, &apiError{http.StatusConflict, "not_configured", "the development OIDC IdP is not enabled on this deployment"})
		return false
	}
	return true
}

func (a wsSettingsAPI) writePersonas(w http.ResponseWriter, r *http.Request, res *resolved, status int) {
	list, err := a.mgr.store.ListDevIdPPersonas(r.Context(), res.ws.ID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	out := make([]devIdPPersonaJSON, 0, len(list))
	for _, p := range list {
		out = append(out, personaJSON(p))
	}
	writeJSON(w, status, devIdPPersonaList{Personas: out, Max: devIdPPersonaMax, Domain: devIdPPersonaDomain})
}

func (a wsSettingsAPI) listPersonas(w http.ResponseWriter, r *http.Request, res *resolved) {
	if a.personasServed(w, r) {
		a.writePersonas(w, r, res, http.StatusOK)
	}
}

func (a wsSettingsAPI) auditPersona(ctx context.Context, res *resolved, detail string) error {
	return a.mgr.store.InsertAudit(ctx, store.AuditLog{
		ID: store.NewID(), TenantID: res.ws.TenantID, ActorKind: "user", ActorID: res.ident.ID,
		Action: "workspace.dev_idp_persona", Target: res.ws.ID, Detail: detail, At: store.NowTS(),
	})
}

func (a wsSettingsAPI) createPersona(w http.ResponseWriter, r *http.Request, res *resolved) {
	if !a.personasServed(w, r) {
		return
	}
	ctx := r.Context()
	var body struct {
		Handle string `json:"handle"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, "invalid_request", "unreadable body"})
		return
	}
	// Stored exactly as typed: an upper-case or non-ASCII handle is refused, never folded.
	if !devIdPPersonaHandle.MatchString(body.Handle) {
		writeAPIErr(w, &apiError{http.StatusBadRequest, "invalid_persona", "handle must be 1-32 characters of a-z 0-9 . _ - and start with a letter or digit"})
		return
	}
	if !validPersonaName(body.Name) {
		writeAPIErr(w, &apiError{http.StatusBadRequest, "invalid_persona", "name must be at most 64 characters without control characters"})
		return
	}
	p := store.DevIdPPersona{
		ID: store.NewID(), WorkspaceID: res.ws.ID, Handle: body.Handle,
		Name: strings.TrimSpace(body.Name), CreatedAt: time.Now().Unix(),
	}
	if ok, err := a.mgr.personaUsable(ctx, p); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	} else if !ok {
		writeAPIErr(w, &apiError{http.StatusConflict, "persona_address_taken", "that address belongs to a real member"})
		return
	}
	existing, err := a.mgr.store.ListDevIdPPersonas(ctx, res.ws.ID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	// Fast path only; CreateDevIdPPersona enforces the limit atomically.
	if len(existing) >= devIdPPersonaMax {
		writeAPIErr(w, &apiError{http.StatusConflict, "persona_limit", "at most " + strconv.Itoa(devIdPPersonaMax) + " personas per workspace"})
		return
	}
	for _, e := range existing {
		if e.Handle == p.Handle { // answered before the audit row, which records changes only
			writeAPIErr(w, &apiError{http.StatusConflict, "persona_exists", "a persona with that handle already exists"})
			return
		}
	}
	if err := a.auditPersona(ctx, res, "add handle="+strconv.Quote(p.Handle)); err != nil {
		writeAPIErr(w, &apiError{http.StatusServiceUnavailable, errCodeAuditUnavailable, "nothing was changed: " + err.Error()})
		return
	}
	if err := a.mgr.store.CreateDevIdPPersona(ctx, p, devIdPPersonaMax); err != nil {
		if errors.Is(err, store.ErrDevIdPWorkspaceGone) {
			writeAPIErr(w, &apiError{http.StatusNotFound, "not_found", "no such workspace"})
			return
		}
		if errors.Is(err, store.ErrDevIdPPersonaLimit) {
			writeAPIErr(w, &apiError{http.StatusConflict, "persona_limit", "at most " + strconv.Itoa(devIdPPersonaMax) + " personas per workspace"})
			return
		}
		if errors.Is(err, store.ErrDevIdPPersonaExists) {
			writeAPIErr(w, &apiError{http.StatusConflict, "persona_exists", "a persona with that handle already exists"})
			return
		}
		writeAPIErr(w, internalErr(err))
		return
	}
	a.writePersonas(w, r, res, http.StatusCreated)
}

func (a wsSettingsAPI) deletePersona(w http.ResponseWriter, r *http.Request, res *resolved) {
	if !a.personasServed(w, r) {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	p, ok, err := a.mgr.store.GetDevIdPPersona(ctx, res.ws.ID, id)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	if !ok {
		writeAPIErr(w, &apiError{http.StatusNotFound, "not_found", "no such persona"})
		return
	}
	if err := a.auditPersona(ctx, res, "remove handle="+strconv.Quote(p.Handle)); err != nil {
		writeAPIErr(w, &apiError{http.StatusServiceUnavailable, errCodeAuditUnavailable, "nothing was changed: " + err.Error()})
		return
	}
	if _, err := a.mgr.store.DeleteDevIdPPersona(ctx, res.ws.ID, id); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	a.writePersonas(w, r, res, http.StatusOK)
}

// --- consent page ---------------------------------------------------------------

// devIdPAuthRequest is the vetted authorize request that a code will be issued for.
type devIdPAuthRequest struct {
	redirectURI, state, nonce, challenge, scope string
}

// devIdPIssueCode stores a code for the request and sends the browser back with it.
func (a devIdPAPI) issueCode(w http.ResponseWriter, r *http.Request, cc devIdPClientCtx, ident store.Identity, membershipID string, req devIdPAuthRequest, profile devIdPProfile) {
	code := "afdevcode-" + devIdPRandom(32)
	now := a.now()
	fail := func(c, d string) {
		a.redirectBack(w, r, req.redirectURI, req.state, url.Values{"error": {c}, "error_description": {d}})
	}
	err := a.mgr.store.InsertDevIdPGrant(r.Context(), store.DevIdPGrant{
		Hash: devIdPHash(code), Kind: store.DevIdPGrantCode,
		ClientID: cc.row.ClientID, WorkspaceID: cc.ws.ID, MembershipID: membershipID, IdentityID: ident.ID,
		RedirectURI: req.redirectURI, Challenge: req.challenge, Nonce: req.nonce, Scope: req.scope,
		Claims:    devIdPProfileJSON(devIdPScopedClaims(profile, req.scope)),
		ExpiresAt: now.Add(devIdPCodeTTL).Unix(),
	}, now.Unix())
	if err != nil {
		log.Printf("dev idp: store code: %v", err)
		fail("server_error", "could not issue a code")
		return
	}
	a.redirectBack(w, r, req.redirectURI, req.state, url.Values{"code": {code}})
}

// askConsent parks the request behind a single-use ticket and shows the member who to sign in
// as. The ticket is the CSRF token: it is random, bound to this member, only ever rendered into
// this page, and the redirect target comes from the stored request, never from the form.
func (a devIdPAPI) askConsent(w http.ResponseWriter, r *http.Request, cc devIdPClientCtx, ident store.Identity, membershipID string, req devIdPAuthRequest, personas []store.DevIdPPersona) {
	ticket := "afdevask-" + devIdPRandom(32)
	now := a.now()
	st, _ := json.Marshal(map[string]string{"state": req.state})
	err := a.mgr.store.InsertDevIdPGrant(r.Context(), store.DevIdPGrant{
		Hash: devIdPHash(ticket), Kind: store.DevIdPGrantConsent,
		ClientID: cc.row.ClientID, WorkspaceID: cc.ws.ID, MembershipID: membershipID, IdentityID: ident.ID,
		RedirectURI: req.redirectURI, Challenge: req.challenge, Nonce: req.nonce, Scope: req.scope,
		Claims: string(st), ExpiresAt: now.Add(devIdPConsentTTL).Unix(),
	}, now.Unix())
	if err != nil {
		log.Printf("dev idp: store consent: %v", err)
		a.redirectBack(w, r, req.redirectURI, req.state, url.Values{"error": {"server_error"}, "error_description": {"could not start the consent"}})
		return
	}
	lang := auth.PreferredUILang(r)
	type row struct{ ID, Label, Email string }
	data := struct {
		Ticket, Action, Target, Self, Lang string
		T                                  devIdPConsentStrings
		Personas                           []row
	}{Lang: lang, T: devIdPConsentText[lang], Ticket: ticket, Action: devIdPConsentPath, Self: ident.Email}
	if u, err := url.Parse(req.redirectURI); err == nil {
		data.Target = u.Host
	}
	for _, p := range personas {
		pr := devIdPPersonaProfile(p)
		data.Personas = append(data.Personas, row{ID: p.ID, Label: pr.Name, Email: pr.Email})
	}
	noStore(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The page is a click target: refuse framing so it cannot be clicked through. No form-action:
	// Chromium applies it to the 302 that follows the POST too, so 'self' would block the return
	// to the app on its preview host (measured in headless Chromium). The page has no script and
	// the form's own action is fixed in the template.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = devIdPConsentTmpl.Execute(w, data)
}

// devIdPConsentStrings is the consent page's text for one language. The language follows the
// browser's Accept-Language like every other CP-rendered page (auth.PreferredUILang): the page
// is a top-level navigation from the app being developed, so there is no Console session to ask.
type devIdPConsentStrings struct {
	Title, Heading, SignInTo, Myself, Persona, Continue, Note, Refused string
}

var devIdPConsentText = map[string]devIdPConsentStrings{
	"ja": {
		Title:    "開発用サインイン",
		Heading:  "開発用サインイン",
		SignInTo: "次の身元でサインインします",
		Myself:   "自分",
		Persona:  "テスト用身元",
		Continue: "続ける",
		Note:     "テスト用身元は、この開発用 IdP の中だけにあります。",
		Refused:  "dev-idp: このサインインの確認は拒否されたか、期限が切れています。最初からサインインし直してください",
	},
	"en": {
		Title:    "Development sign-in",
		Heading:  "Development sign-in",
		SignInTo: "Sign in as:",
		Myself:   "Myself",
		Persona:  "test persona",
		Continue: "Continue",
		Note:     "Test personas exist only on this development IdP.",
		Refused:  "dev-idp: this sign-in confirmation was refused or has expired; start the sign-in again",
	},
}

var devIdPConsentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.T.Title}}</title>
<style>body{font:15px system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem}
label{display:block;padding:.5rem;border:1px solid #8884;border-radius:6px;margin:.4rem 0}
small{color:#666}button{margin-top:1rem;padding:.5rem 1.2rem}</style></head>
<body><h1>{{.T.Heading}}</h1>
<p><b>{{.Target}}</b> — {{.T.SignInTo}}</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="ticket" value="{{.Ticket}}">
<label><input type="radio" name="choice" value="self" checked> {{.T.Myself}} <small>{{.Self}}</small></label>
{{range .Personas}}<label><input type="radio" name="choice" value="{{.ID}}"> {{.Label}} <small>{{.Email}} ({{$.T.Persona}})</small></label>
{{end}}<button type="submit">{{.T.Continue}}</button>
</form>
<p><small>{{.T.Note}}</small></p>
</body></html>`))

func consentRefuse(w http.ResponseWriter, r *http.Request, status int, why string) {
	log.Printf("dev idp: consent refused: %s", why)
	noStore(w)
	http.Error(w, devIdPConsentText[auth.PreferredUILang(r)].Refused, status)
}

// sameOrigin is the cross-site guard for the consent POST: a browser that sends Origin or
// Sec-Fetch-Site must say it came from the Console origin itself. (The ticket is the real
// defence; this refuses early and keeps the failure legible.)
func (a devIdPAPI) sameOrigin(r *http.Request) bool {
	base, err := url.Parse(a.mgr.publicBaseURL)
	if err != nil || base.Host == "" {
		return false
	}
	want := base.Scheme + "://" + base.Host
	if o := r.Header.Get("Origin"); o != "" && o != want {
		return false
	}
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" {
		return false
	}
	return true
}

func (a devIdPAPI) consent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.sameOrigin(r) {
		consentRefuse(w, r, http.StatusForbidden, "cross-site")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		consentRefuse(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	ticket := r.PostForm.Get("ticket")
	if ticket == "" {
		consentRefuse(w, r, http.StatusBadRequest, "no ticket")
		return
	}
	id := a.mgr.resolveIdentity(r)
	if id.key == "" {
		writeAPIErr(w, &apiError{http.StatusUnauthorized, "unauthenticated", "no gateway identity"})
		return
	}
	g, ok, err := a.mgr.store.ConsumeDevIdPConsent(ctx, devIdPHash(ticket), a.now().Unix())
	if err != nil {
		log.Printf("dev idp: consume consent: %v", err)
		consentRefuse(w, r, http.StatusInternalServerError, "store")
		return
	}
	if !ok {
		consentRefuse(w, r, http.StatusBadRequest, "unknown, spent or expired ticket")
		return
	}
	cc, found := a.loadClient(r, g.ClientID)
	if !found {
		consentRefuse(w, r, http.StatusBadRequest, "client")
		return
	}
	// Everything authorize decided is decided again: the member, the redirect and the opt-in
	// may have changed in the minutes the page was open.
	ident, mv, aerr := a.mgr.resolveMembership(ctx, id.key, id.email, cc.ws.TenantID)
	if aerr != nil || mv.MembershipID != g.MembershipID || ident.ID != g.IdentityID {
		consentRefuse(w, r, http.StatusForbidden, "ticket belongs to another member")
		return
	}
	if !a.mgr.devIdPRedirectAllowed(ctx, cc.ws, cc.st, mv.MembershipID, g.RedirectURI) {
		consentRefuse(w, r, http.StatusBadRequest, "redirect_uri")
		return
	}
	var stored struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal([]byte(g.Claims), &stored)
	req := devIdPAuthRequest{redirectURI: g.RedirectURI, state: stored.State, nonce: g.Nonce, challenge: g.Challenge, scope: g.Scope}
	profile := devIdPClaims(ctx, a.mgr.store, ident)
	if choice := r.PostForm.Get("choice"); choice != "self" {
		p, found, err := a.mgr.store.GetDevIdPPersona(ctx, cc.ws.ID, choice)
		if err != nil {
			consentRefuse(w, r, http.StatusInternalServerError, "store")
			return
		}
		if !found {
			consentRefuse(w, r, http.StatusBadRequest, "no such persona in this workspace")
			return
		}
		if ok, err := a.mgr.personaUsable(ctx, p); err != nil || !ok {
			a.redirectBack(w, r, req.redirectURI, req.state, url.Values{"error": {"access_denied"}, "error_description": {"that persona is not usable"}})
			return
		}
		profile = devIdPPersonaProfile(p)
	}
	a.issueCode(w, r, cc, ident, mv.MembershipID, req, profile)
}

// personaSubUsable re-checks a persona-shaped subject just before it is signed or served: the
// claims were fixed into the code at consent time, and a real member with that address may have
// appeared in the minute since. Anything that is not a persona subject passes; a persona subject
// whose handle is malformed, whose address is now a real person's, or whose check failed does not.
func (m *manager) personaSubUsable(ctx context.Context, sub string) (bool, error) {
	h, ok := strings.CutPrefix(sub, devIdPPersonaPrefix)
	if !ok {
		return true, nil
	}
	return m.personaUsable(ctx, store.DevIdPPersona{Handle: h})
}

// personaAuditDetail names the persona in the token audit row, or "" for a real member.
func personaAuditDetail(sub string) string {
	if h, ok := strings.CutPrefix(sub, devIdPPersonaPrefix); ok {
		return " persona=" + strconv.Quote(h)
	}
	return ""
}
