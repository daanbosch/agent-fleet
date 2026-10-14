package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// grantAgent is a fake Agent for the temporary-grant paths: it reports each port's listening
// sockets (and probe result) the way /preview-listener does, and relays /proxy only while the
// listener header names a socket that is still listening, as the real Agent does.
type grantAgent struct {
	srv *httptest.Server
	mu  sync.Mutex
	// per port: inodes and the probe verdict ("http", "preview_not_http", ...)
	listen map[int]agentListener
	// noEndpoint answers 404 on /preview-listener like an older Agent.
	noEndpoint bool
	// last proxied request headers, per path
	seen http.Header
}

func newGrantAgent(t *testing.T) *grantAgent {
	t.Helper()
	g := &grantAgent{listen: map[int]agentListener{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/preview-listener/"):
			if g.noEndpoint {
				http.NotFound(w, r)
				return
			}
			port, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/preview-listener/"))
			if port == 7700 {
				http.Error(w, "bad_port", http.StatusBadRequest)
				return
			}
			l, ok := g.listen[port]
			if !ok {
				l = agentListener{Inodes: []string{}}
			}
			_ = json.NewEncoder(w).Encode(l)
		case strings.HasPrefix(r.URL.Path, "/proxy/"):
			g.seen = r.Header.Clone()
			port, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(r.URL.Path, "/proxy/"), "/")[0])
			if want := r.Header.Get(previewListenerHeader); want != "" {
				alive := false
				for _, in := range strings.Split(want, ",") {
					for _, have := range g.listen[port].Inodes {
						alive = alive || in == have
					}
				}
				if !alive {
					w.Header().Set(previewErrorHeader, previewListenerChanged)
					w.WriteHeader(http.StatusGone)
					return
				}
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *grantAgent) set(port int, probe string, inodes ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listen[port] = agentListener{Inodes: inodes, Probe: probe}
}

func (g *grantAgent) seenHeader(k string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen.Get(k)
}

// openOwnAs calls /preview-open-own on the Console origin as userKey.
func (e *previewHostEnv) openOwnAs(t *testing.T, userKey string, port int) *httptest.ResponseRecorder {
	t.Helper()
	var rec *httptest.ResponseRecorder
	e.asUser(userKey, func() {
		rec = e.console(t, "https://af.example.com"+previewOwnOpenPath+"?port="+strconv.Itoa(port))
	})
	return rec
}

func (e *previewHostEnv) hostGet(t *testing.T, slug string, port int, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	host := previewHostname(slug, port, e.domain)
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	req.Header.Set("X-Forwarded-Proto", "https")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.host.ServeHTTP(rec, req)
	return rec
}

func (e *previewHostEnv) tempPorts(t *testing.T) []int {
	t.Helper()
	gs, err := e.mgr.store.ListPreviewTempPorts(context.Background(), e.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, g := range gs {
		out = append(out, g.Port)
	}
	return out
}

// owner opens :5173 → grant → handshake → cookie → relayed with the listener bound.
func ownerOpens(t *testing.T, e *previewHostEnv, slug string, port int) *http.Cookie {
	t.Helper()
	rec := e.openOwnAs(t, "preview-user", port)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), previewHandshakePath) {
		t.Fatalf("open-own: %d %s loc=%q", rec.Code, rec.Body, rec.Header().Get("Location"))
	}
	return e.viewerCookie(t, previewViewer{userKey: "preview-user"}, slug, port)
}

func TestPreviewOwnerOpensUnlistedPortBoundToListener(t *testing.T) {
	ag := newGrantAgent(t)
	ag.set(5173, "http", "111", "222")
	e := newPreviewHostEnv(t, ag.srv.URL)
	slug := e.mintSlug(t)

	// Without a grant the port does not exist, even to the owner with no cookie: 404, no
	// handshake redirect.
	if rec := e.hostGet(t, slug, 5173, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no grant: %d, want 404", rec.Code)
	}
	ck := ownerOpens(t, e, slug, 5173)
	// A client-supplied listener header is replaced by the grant's own.
	rec := e.hostGet(t, slug, 5173, map[string]string{previewListenerHeader: "999"}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner relay: %d %s", rec.Code, rec.Body)
	}
	if got := ag.seenHeader(previewListenerHeader); got != "111,222" {
		t.Errorf("listener header to the Agent = %q, want the granted inodes", got)
	}
	// Exactly one audit row, and renewing the same listener adds none.
	e.openOwnAs(t, "preview-user", 5173)
	n := auditCount(t, e, "workspace.preview_temp_port")
	if n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
	// Listed ports are unchanged: no grant, no listener header.
	ck3000 := e.viewerCookie(t, previewViewer{userKey: "preview-user"}, slug, 3000)
	if rec := e.hostGet(t, slug, 3000, map[string]string{previewListenerHeader: "999"}, ck3000); rec.Code != 200 {
		t.Fatalf("listed port: %d", rec.Code)
	}
	if got := ag.seenHeader(previewListenerHeader); got != "" {
		t.Errorf("a client-supplied listener header reached the Agent on a listed port: %q", got)
	}
}

func auditCount(t *testing.T, e *previewHostEnv, action string) int {
	t.Helper()
	rows, err := e.mgr.store.ListAuditByTenant(context.Background(), e.ws.TenantID, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Action == action {
			n++
		}
	}
	return n
}

// Public mode and tenant share govern listed ports only: an unlisted port stays the owner's.
func TestPreviewUnlistedPortStaysOwnerOnly(t *testing.T) {
	ag := newGrantAgent(t)
	ag.set(5173, "http", "111")
	e := newPreviewHostEnv(t, ag.srv.URL)
	v := e.addViewer(t, "colleague")
	slug := e.mintSlug(t)
	ownerOpens(t, e, slug, 5173)
	e.setPreviewSettings(t, func(st *wsSettings) { st.PreviewPublic = true; st.PreviewTenantShare = true })

	// Public visitor: no cookie → 404, not a handshake, not the page.
	if rec := e.hostGet(t, slug, 5173, nil); rec.Code != http.StatusNotFound {
		t.Errorf("public visitor: %d, want 404", rec.Code)
	}
	// A colleague cannot even get the handshake for it.
	e.asUser(v.userKey, func() {
		rec := e.console(t, "https://af.example.com"+previewHandshakePath+"?slug="+slug+"&port=5173&next=/")
		if rec.Code != http.StatusNotFound {
			t.Errorf("colleague handshake: %d, want 404", rec.Code)
		}
	})
	// A colleague's cookie for a LISTED port of the same workspace does not open it either.
	ckListed := e.viewerCookie(t, v, slug, 3000)
	if rec := e.hostGet(t, slug, 5173, nil, ckListed); rec.Code != http.StatusNotFound {
		t.Errorf("colleague cookie: %d, want 404", rec.Code)
	}
	// open-own acts on the CALLER's workspace: a colleague cannot create or probe anything
	// on the owner's. Theirs is not running (no slug), so it only says so.
	if rec := e.openOwnAs(t, v.userKey, 5173); rec.Code != http.StatusConflict {
		t.Errorf("colleague open-own: %d, want 409 on their own stopped workspace", rec.Code)
	}
	if got := e.tempPorts(t); len(got) != 1 {
		t.Errorf("owner's grants changed: %v", got)
	}
}

// The grant dies with the listener: stopped → 404 and the row is gone; another process on
// the port (different inode) → 404 too.
func TestPreviewGrantEndsWithTheListener(t *testing.T) {
	ag := newGrantAgent(t)
	ag.set(5173, "http", "111")
	e := newPreviewHostEnv(t, ag.srv.URL)
	slug := e.mintSlug(t)
	ck := ownerOpens(t, e, slug, 5173)
	if rec := e.hostGet(t, slug, 5173, nil, ck); rec.Code != 200 {
		t.Fatalf("live: %d", rec.Code)
	}
	ag.set(5173, "http", "333") // a different process took the port
	if rec := e.hostGet(t, slug, 5173, nil, ck); rec.Code != http.StatusNotFound {
		t.Fatalf("rebound port: %d, want 404", rec.Code)
	}
	if got := e.tempPorts(t); len(got) != 0 {
		t.Errorf("grant not deleted after the listener changed: %v", got)
	}
	// Without the grant it stays 404 even though something listens again.
	if rec := e.hostGet(t, slug, 5173, nil, ck); rec.Code != http.StatusNotFound {
		t.Errorf("after delete: %d", rec.Code)
	}
}

func TestPreviewGrantRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(g *grantAgent)
		status int
		kind   string
	}{
		"nothing listening": {func(g *grantAgent) {}, 409, "Nothing is listening"},
		"not http":          {func(g *grantAgent) { g.set(5173, "preview_not_http", "1") }, 409, "does not speak HTTP"},
		"tls":               {func(g *grantAgent) { g.set(5173, "preview_tls", "1") }, 409, "serving HTTPS"},
		"old agent":         {func(g *grantAgent) { g.noEndpoint = true; g.set(5173, "http", "1") }, 409, "not available until the workspace is restarted"},
	} {
		t.Run(name, func(t *testing.T) {
			ag := newGrantAgent(t)
			tc.setup(ag)
			e := newPreviewHostEnv(t, ag.srv.URL)
			e.mintSlug(t)
			var rec *httptest.ResponseRecorder
			e.asUser("preview-user", func() {
				req := httptest.NewRequest(http.MethodGet, "https://af.example.com"+previewOwnOpenPath+"?port=5173", nil)
				req.Header.Set("Accept", "text/html")
				req.Header.Set("Accept-Language", "en")
				rec = httptest.NewRecorder()
				e.mux.ServeHTTP(rec, req)
			})
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.kind) {
				t.Fatalf("%d %q, want %d containing %q", rec.Code, rec.Body, tc.status, tc.kind)
			}
			if got := e.tempPorts(t); len(got) != 0 {
				t.Errorf("grant recorded despite the refusal: %v", got)
			}
		})
	}
}

func TestPreviewGrantGuardsAndCap(t *testing.T) {
	ag := newGrantAgent(t)
	e := newPreviewHostEnv(t, ag.srv.URL)
	e.mintSlug(t)
	for _, port := range []int{0, 70000} {
		if rec := e.openOwnAs(t, "preview-user", port); rec.Code != http.StatusBadRequest {
			t.Errorf("port %d: %d, want 400", port, rec.Code)
		}
	}
	if rec := e.openOwnAs(t, "preview-user", 7700); rec.Code != http.StatusBadRequest {
		t.Errorf("agent port: %d, want 400", rec.Code)
	}
	for i := 0; i < maxPreviewTempPorts; i++ {
		ag.set(20000+i, "http", strconv.Itoa(i+1))
		if rec := e.openOwnAs(t, "preview-user", 20000+i); rec.Code != http.StatusFound {
			t.Fatalf("grant %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	ag.set(21000, "http", "9999")
	if rec := e.openOwnAs(t, "preview-user", 21000); rec.Code != http.StatusConflict {
		t.Errorf("over the cap: %d, want 409", rec.Code)
	}
	// Renewing an existing grant at the cap still works.
	if rec := e.openOwnAs(t, "preview-user", 20000); rec.Code != http.StatusFound {
		t.Errorf("renewal at the cap: %d", rec.Code)
	}
}

// Slug rotation (start / reissue) and stop clear the grants.
func TestPreviewGrantsClearedOnRotateAndStop(t *testing.T) {
	ag := newGrantAgent(t)
	ag.set(5173, "http", "111")
	e := newPreviewHostEnv(t, ag.srv.URL)
	slug := e.mintSlug(t)
	ownerOpens(t, e, slug, 5173)
	if len(e.tempPorts(t)) != 1 {
		t.Fatal("grant not recorded")
	}
	e.mintSlug(t)
	if got := e.tempPorts(t); len(got) != 0 {
		t.Errorf("after slug rotation: %v", got)
	}
	ownerOpens(t, e, e.currentSlug(t), 5173)
	if err := e.mgr.store.SetWorkspaceState(context.Background(), e.ws.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if got := e.tempPorts(t); len(got) != 0 {
		t.Errorf("after stop: %v", got)
	}
}

func (e *previewHostEnv) currentSlug(t *testing.T) string {
	t.Helper()
	ws, ok, err := e.mgr.store.GetWorkspaceByMembership(context.Background(), e.ws.MembershipID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	return ws.PreviewSlug
}

// An unlisted sibling counts for CORS only with a live grant and the owner's own cookie.
func TestPreviewSiblingOriginUnlistedNeedsGrantAndOwner(t *testing.T) {
	ag := newGrantAgent(t)
	ag.set(5173, "http", "111")
	e := newPreviewHostEnv(t, ag.srv.URL)
	slug := e.mintSlug(t)
	e.setPreviewSettings(t, func(st *wsSettings) { st.PreviewCrossOrigin = true })
	sibling := "https://" + previewHostname(slug, 5173, e.domain)
	ck3000 := e.viewerCookie(t, previewViewer{userKey: "preview-user"}, slug, 3000)
	call := func(cookies ...*http.Cookie) string {
		rec := e.hostGet(t, slug, 3000, map[string]string{"Origin": sibling}, cookies...)
		return rec.Header().Get("Access-Control-Allow-Origin")
	}
	if got := call(ck3000); got != "" {
		t.Errorf("no grant: ACAO=%q", got)
	}
	ownerOpens(t, e, slug, 5173)
	if got := call(ck3000); got != sibling {
		t.Errorf("grant + owner cookie: ACAO=%q, want %q", got, sibling)
	}
	e.setPreviewSettings(t, func(st *wsSettings) { st.PreviewCrossOrigin = true; st.PreviewPublic = true })
	if got := call(); got != "" {
		t.Errorf("grant but anonymous caller: ACAO=%q, want none", got)
	}
}

func TestPreviewSettingsListsTempPorts(t *testing.T) {
	ag := newGrantAgent(t)
	ag.set(5173, "http", "111")
	e := newPreviewHostEnv(t, ag.srv.URL)
	slug := e.mintSlug(t)
	ownerOpens(t, e, slug, 5173)
	rec := e.console(t, "https://af.example.com/api/env/ws-settings")
	var out struct{ PreviewTempPorts []int }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.PreviewTempPorts) != 1 || out.PreviewTempPorts[0] != 5173 {
		t.Fatalf("ws-settings = %s (%v)", rec.Body, err)
	}
}

// The path form (/preview/{port}/) never carries a listener binding, and a client-supplied
// header never reaches the Agent through it either.
func TestPreviewPathFormDropsClientListenerHeader(t *testing.T) {
	ag := newGrantAgent(t)
	mux := newPreviewTenantTestEnv(t, ag.srv.URL)
	req := httptest.NewRequest(http.MethodGet, "/preview/5173/?tenant=default", nil)
	req.Header.Set(previewListenerHeader, "1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("path form: %d %s", rec.Code, rec.Body)
	}
	if got := ag.seenHeader(previewListenerHeader); got != "" {
		t.Errorf("client listener header reached the Agent: %q", got)
	}
}
