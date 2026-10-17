package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Sign-in to a loopback redirect is a supported use of the dev IdP, also while the member's
// workspace is stopped: an app on the member's own machine has nothing else to reach. These
// tests pin that no step of the flow asks whether the workspace is running, so a later
// "refuse clients of stopped workspaces" check turns them red (ADR 0116, 2026-10-18 note).

// stopWorkspace puts the workspace in the state a stop leaves: state "stopped" and no preview
// slug. d.slug keeps the old value so a test can still build the (now stale) preview host.
func (d *devIdPTestEnv) stopWorkspace(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := d.mgr.store.SetWorkspacePreviewSlug(ctx, d.ws.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.mgr.store.SetWorkspaceState(ctx, d.ws.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	ws, ok, err := d.mgr.store.GetWorkspaceByMembership(ctx, d.ws.MembershipID)
	if err != nil || !ok || ws.State != "stopped" || ws.PreviewSlug != "" {
		t.Fatalf("workspace is not stopped: %+v ok=%v err=%v", ws, ok, err)
	}
}

func loopbackRedirects() []string {
	return []string{"http://localhost:5173/cb", "http://127.0.0.1:8080/cb"}
}

func (d *devIdPTestEnv) userinfoOK(t *testing.T, tok *httptest.ResponseRecorder) {
	t.Helper()
	if tok.Code != http.StatusOK {
		t.Fatalf("token: %d %s", tok.Code, tok.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "https://af.example.com"+devIdPUserinfoPath, nil)
	req.Header.Set("Authorization", "Bearer "+idpJSON(t, tok)["access_token"].(string))
	ui := httptest.NewRecorder()
	d.mux.ServeHTTP(ui, req)
	if ui.Code != http.StatusOK || idpJSON(t, ui)["sub"] == "" {
		t.Fatalf("userinfo: %d %s", ui.Code, ui.Body)
	}
}

func TestDevIdPStoppedWorkspaceOwnerSignsInToLoopback(t *testing.T) {
	for _, redirect := range loopbackRedirects() {
		for _, pkce := range []bool{true, false} {
			for _, basic := range []bool{true, false} {
				d := newDevIdPTestEnv(t)
				d.stopWorkspace(t)
				verifier, challenge := "", ""
				if pkce {
					verifier, challenge = pkcePair()
				}
				code := codeFrom(t, d.authorizeAs(t, "preview-user", d.authQuery(redirect, challenge)))
				var tok *httptest.ResponseRecorder
				if basic {
					tok = d.exchange(code, redirect, verifier)
				} else {
					f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
						"client_id": {d.clientID}, "client_secret": {d.secret}}
					if verifier != "" {
						f.Set("code_verifier", verifier)
					}
					tok = d.tokenReq(f, "", "")
				}
				d.userinfoOK(t, tok)
			}
		}
	}
}

func TestDevIdPStoppedWorkspaceOwnerSignsInToLoopbackThroughConsent(t *testing.T) {
	for _, redirect := range loopbackRedirects() {
		d := newDevIdPTestEnv(t)
		d.addPersona(t, "admin", "Admin User")
		d.stopWorkspace(t)
		verifier, challenge := pkcePair()
		_, ticket := d.askAs(t, "preview-user", d.authQuery(redirect, challenge))
		rec := d.postConsent("preview-user", url.Values{"ticket": {ticket}, "choice": {d.personaID(t, "admin")}}, nil)
		code := codeFrom(t, rec)
		if loc, _ := url.Parse(rec.Header().Get("Location")); loc.Scheme+"://"+loc.Host+loc.Path != redirect {
			t.Errorf("consent redirected to %q, want %q", rec.Header().Get("Location"), redirect)
		}
		d.userinfoOK(t, d.exchange(code, redirect, verifier))
		// Choosing myself works the same.
		_, ticket = d.askAs(t, "preview-user", d.authQuery(redirect, ""))
		self := codeFrom(t, d.postConsent("preview-user", url.Values{"ticket": {ticket}, "choice": {"self"}}, nil))
		d.userinfoOK(t, d.exchange(self, redirect, ""))
	}
}

// The preview host needs the current slug, which a stopped workspace does not have.
func TestDevIdPStoppedWorkspaceRefusesThePreviewHost(t *testing.T) {
	d := newDevIdPTestEnv(t)
	preview := d.ownHost(3000) + "/cb"
	if rec := d.authorizeAs(t, "preview-user", d.authQuery(preview, "")); rec.Code != http.StatusFound {
		t.Fatalf("preview host while running: %d", rec.Code)
	}
	d.stopWorkspace(t)
	if rec := d.authorizeAs(t, "preview-user", d.authQuery(preview, "")); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Errorf("preview host while stopped: %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// A tenant colleague follows the preview rules, stopped or not: only with tenant share on.
func TestDevIdPStoppedWorkspaceColleagueFollowsTheShareRules(t *testing.T) {
	for _, redirect := range loopbackRedirects() {
		d := newDevIdPTestEnv(t)
		d.addViewer(t, "colleague")
		d.stopWorkspace(t)
		if rec := d.authorizeAs(t, "colleague", d.authQuery(redirect, "")); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("colleague, sharing off, %s: %d loc=%q", redirect, rec.Code, rec.Header().Get("Location"))
		}
		d.setPreviewSettings(t, func(st *wsSettings) { st.PreviewTenantShare = true })
		code := codeFrom(t, d.authorizeAs(t, "colleague", d.authQuery(redirect, "")))
		d.userinfoOK(t, d.exchange(code, redirect, ""))
		d.setPreviewSettings(t, func(st *wsSettings) { st.PreviewTenantShare = false })
		if rec := d.authorizeAs(t, "colleague", d.authQuery(redirect, "")); rec.Code != http.StatusBadRequest {
			t.Errorf("colleague after sharing was turned off, %s: %d", redirect, rec.Code)
		}
	}
}

// Someone outside the workspace's tenant is refused whatever the workspace's state.
func TestDevIdPStoppedWorkspaceRefusesANonMember(t *testing.T) {
	d := newDevIdPTestEnv(t)
	d.stopWorkspace(t)
	if _, err := d.mgr.store.UpsertIdentity(context.Background(), "", "stranger", ""); err != nil {
		t.Fatal(err)
	}
	for _, redirect := range loopbackRedirects() {
		if rec := d.authorizeAs(t, "stranger", d.authQuery(redirect, "")); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("non-member, %s: %d loc=%q", redirect, rec.Code, rec.Header().Get("Location"))
		}
	}
}
