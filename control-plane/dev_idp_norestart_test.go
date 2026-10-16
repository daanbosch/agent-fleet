package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// envCreds reads the client id and secret the container would get at a start.
func envCreds(d *devIdPTestEnv) (id, secret string, ok bool) {
	for _, kv := range d.mgr.devIdPEnv(context.Background(), d.ws) {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "AF_DEV_IDP_CLIENT_ID":
			id = v
		case "AF_DEV_IDP_CLIENT_SECRET":
			secret = v
		}
	}
	return id, secret, id != "" && secret != ""
}

func (d *devIdPTestEnv) signInWith(t *testing.T, id, secret string) int {
	t.Helper()
	redirect := d.ownHost(3000) + "/cb"
	d.clientID, d.secret = id, secret
	rec := d.authorizeAs(t, "preview-user", d.authQuery(redirect, ""))
	if rec.Code != http.StatusFound {
		return rec.Code
	}
	return d.exchange(codeFrom(t, rec), redirect, "").Code
}

// The values injected at a workspace's start are the ones that keep working: opting in later,
// in a running workspace, needs no restart, and off then on again hands back the same pair.
func TestDevIdPOptInWorksInARunningWorkspaceWithoutARestart(t *testing.T) {
	d := newDevIdPTestEnv(t)
	d.setDevIdP(t, false)
	// "Start" while not opted in: the env is still injected.
	id, secret, ok := envCreds(d)
	if !ok {
		t.Fatal("no AF_DEV_IDP_* injected for a workspace that has not opted in")
	}
	// Not opted in: the client is refused at token.
	redirect := d.ownHost(3000) + "/cb"
	if rec := d.authorizeAs(t, "preview-user", d.authQuery(redirect, "")); rec.Code != http.StatusBadRequest {
		t.Fatalf("authorize before opting in = %d", rec.Code)
	}
	// Opt in with no restart: the env from the earlier start works.
	d.setDevIdP(t, true)
	if got := d.signInWith(t, id, secret); got != http.StatusOK {
		t.Fatalf("sign-in after opting in without restart = %d", got)
	}
	// Off refuses at once, on again keeps the same credentials.
	d.setDevIdP(t, false)
	if got := d.signInWith(t, id, secret); got != http.StatusBadRequest {
		t.Errorf("authorize right after off = %d, want 400", got)
	}
	d.setDevIdP(t, true)
	if id2, secret2, _ := envCreds(d); id2 != id || secret2 != secret {
		t.Error("toggling changed the credentials")
	}
	if got := d.signInWith(t, id, secret); got != http.StatusOK {
		t.Errorf("sign-in after on again = %d", got)
	}
}

func TestDevIdPRotateIssuesANewSecretAndRefusesTheOldOne(t *testing.T) {
	d := newDevIdPTestEnv(t)
	oldID, oldSecret, _ := envCreds(d)
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "https://af.example.com/api/env/ws-settings/dev-idp/rotate", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body)
	}
	if b := idpJSON(t, rec); b["restartRequired"] != true || b["devIdpClientId"] == oldID {
		t.Errorf("rotate answer = %v", b)
	}
	newID, newSecret, _ := envCreds(d)
	if newID == oldID || newSecret == oldSecret {
		t.Fatal("rotate kept the credentials")
	}
	if got := d.signInWith(t, oldID, oldSecret); got != http.StatusBadRequest {
		t.Errorf("the old client still authorizes: %d", got)
	}
	if got := d.signInWith(t, newID, newSecret); got != http.StatusOK {
		t.Errorf("the new client = %d", got)
	}
	rows, err := d.mgr.store.ListAuditByTenant(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		found = found || (r.Action == "workspace.dev_idp_rotate" && r.Target == d.ws.ID && r.ActorID != "")
	}
	if !found {
		t.Error("rotate was not audited")
	}
}

func TestDevIdPNoEnvAndNoRotateWhereTheDeploymentDoesNotOfferIt(t *testing.T) {
	for name, off := range map[string]func(*devIdPTestEnv){
		"no preview domain": func(d *devIdPTestEnv) { d.mgr.previewDomain = "" },
		"switched off":      func(d *devIdPTestEnv) { setDevIdPDeployment(t, d.mgr, false) },
	} {
		d := newDevIdPTestEnv(t)
		off(d)
		if _, _, ok := envCreds(d); ok {
			t.Errorf("%s: AF_DEV_IDP_* injected", name)
		}
		rec := httptest.NewRecorder()
		d.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "https://af.example.com/api/env/ws-settings/dev-idp/rotate", nil))
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: rotate = %d, want 409", name, rec.Code)
		}
	}
}

// Many starts at once must settle on one registration.
func TestDevIdPLazyCreationAtStartIsIdempotentUnderConcurrentStarts(t *testing.T) {
	d := newDevIdPTestEnv(t)
	if err := d.mgr.store.DeleteDevIdPClient(context.Background(), d.ws.ID); err != nil {
		t.Fatal(err)
	}
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for _, kv := range d.mgr.devIdPEnv(context.Background(), d.ws) {
				if v, ok := strings.CutPrefix(kv, "AF_DEV_IDP_CLIENT_SECRET="); ok {
					ids[i] = v
				}
			}
		}(i)
	}
	wg.Wait()
	for i, v := range ids {
		if v == "" || v != ids[0] {
			t.Fatalf("start %d got secret %q, start 0 got %q", i, v, ids[0])
		}
	}
	if c, _ := d.mgr.store.CountDevIdPClients(context.Background()); c != 1 {
		t.Errorf("clients = %d, want 1", c)
	}
}
