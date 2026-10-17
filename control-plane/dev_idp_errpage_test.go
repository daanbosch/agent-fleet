package main

import (
	"context"
	"errors"
	"github.com/k-k1/agent-fleet/control-plane/internal/store"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const devIdPPageCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'"

func (d *devIdPTestEnv) getAs(userKey, target, acceptLang string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	d.asUser(userKey, func() {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if acceptLang != "" {
			req.Header.Set("Accept-Language", acceptLang)
		}
		d.mux.ServeHTTP(rec, req)
	})
	return rec
}

// wantErrPage checks what every error page owes: the status, the shell, the headers of the
// consent page, and nothing that posts, links, redirects or names the internal reason.
func wantErrPage(t *testing.T, name string, rec *httptest.ResponseRecorder, status int, title string) string {
	t.Helper()
	body := rec.Body.String()
	h := rec.Header()
	if rec.Code != status {
		t.Errorf("%s: status %d, want %d: %s", name, rec.Code, status, body)
	}
	for _, want := range []string{"<!doctype html>", `src="/brand/icon-192.png"`, "Agent Fleet", "DEV IdP", "prefers-color-scheme: light", "--accent:", html.EscapeString(title)} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: page lacks %q:\n%s", name, want, body)
		}
	}
	for _, bad := range []string{"<form", "<a ", "<script", "http-equiv", "client_id", "dev-idp:"} {
		if strings.Contains(body, bad) {
			t.Errorf("%s: page contains %q", name, bad)
		}
	}
	if h.Get("Location") != "" {
		t.Errorf("%s: redirected to %q", name, h.Get("Location"))
	}
	if !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("Cache-Control") != "no-store" ||
		h.Get("Referrer-Policy") != "same-origin" || h.Get("X-Frame-Options") != "DENY" ||
		h.Get("Content-Security-Policy") != devIdPPageCSP {
		t.Errorf("%s: headers %v", name, h)
	}
	return body
}

func TestDevIdPErrorPagesWearTheShellAndKeepTheirStatus(t *testing.T) {
	d := newDevIdPTestEnv(t)
	d.cfg.brand.env, _ = buildBrand("violet", "stg")
	good := d.ownHost(3000) + "/cb"

	unknown := d.authQuery(good, "")
	unknown.Set("client_id", "no-such-client")
	rec := d.getAs("preview-user", "https://af.example.com"+devIdPAuthorizePath+"?"+unknown.Encode(), "en")
	body := wantErrPage(t, "unknown client", rec, http.StatusBadRequest, "Can't start the sign-in")
	if !strings.Contains(body, "#7c4dff") || !strings.Contains(body, "stg") {
		t.Errorf("brand not applied:\n%s", body)
	}
	if strings.Contains(body, "no-such-client") {
		t.Error("client_id was echoed")
	}

	// Another member's preview address is as much a refused callback as a foreign one.
	d.addViewer(t, "mallory")
	rec = d.getAs("mallory", "https://af.example.com"+devIdPAuthorizePath+"?"+d.authQuery(good, "").Encode(), "en")
	wantErrPage(t, "another member's preview host", rec, http.StatusBadRequest, "Callback address not allowed")

	bad := "https://evil.example.org/cb?code=secret&state=s3cr3t"
	rec = d.getAs("preview-user", "https://af.example.com"+devIdPAuthorizePath+"?"+d.authQuery(bad, "").Encode(), "en")
	body = wantErrPage(t, "redirect_uri", rec, http.StatusBadRequest, "Callback address not allowed")
	if !strings.Contains(body, "<code>evil.example.org</code>") {
		t.Errorf("the rejected host is not shown:\n%s", body)
	}
	for _, leak := range []string{"secret", "s3cr3t", "/cb", d.clientID, "https://evil"} {
		if strings.Contains(body, leak) {
			t.Errorf("redirect_uri page leaks %q", leak)
		}
	}
	if !strings.Contains(body, "http://localhost:&lt;port&gt;") {
		t.Errorf("next step lacks the localhost hint:\n%s", body)
	}

	// Wrong method on the two page endpoints; the Allow header stays.
	rec = httptest.NewRecorder()
	d.asUser("preview-user", func() {
		req := httptest.NewRequest(http.MethodPost, "https://af.example.com"+devIdPAuthorizePath, nil)
		req.Header.Set("Accept-Language", "en")
		d.mux.ServeHTTP(rec, req)
	})
	wantErrPage(t, "authorize POST", rec, http.StatusMethodNotAllowed, "Sign in from your app")
	if rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("Allow = %q", rec.Header().Get("Allow"))
	}
	rec = d.getAs("preview-user", "https://af.example.com"+devIdPConsentPath, "en")
	wantErrPage(t, "consent GET", rec, http.StatusMethodNotAllowed, "Sign in from your app")

	en := map[string]string{"Accept-Language": "en"}
	// A spent ticket and a missing one are the same 400 page, in the member's language.
	rec = d.postConsent("preview-user", url.Values{"ticket": {"afdevask-nope"}, "choice": {"self"}}, en)
	wantErrPage(t, "unknown ticket", rec, http.StatusBadRequest, "Sign-in confirmation is no longer valid")
	rec = d.postConsent("preview-user", url.Values{"choice": {"self"}}, map[string]string{"Accept-Language": "ja"})
	wantErrPage(t, "no ticket", rec, http.StatusBadRequest, "サインインの確認が無効です")
	rec = d.postConsent("preview-user", url.Values{"ticket": {"x"}}, map[string]string{"Origin": "https://evil.example", "Accept-Language": "en"})
	wantErrPage(t, "cross-site", rec, http.StatusForbidden, "Sign-in confirmation is no longer valid")
}

func TestDevIdPErrorPageEchoesOnlyAPlainHost(t *testing.T) {
	d := newDevIdPTestEnv(t)
	rec := d.getAs("preview-user", "https://af.example.com"+devIdPAuthorizePath+"?"+
		d.authQuery("https://a'b.example/cb", "").Encode(), "en")
	body := wantErrPage(t, "quote in host", rec, http.StatusBadRequest, "Callback address not allowed")
	// Only plain host characters are ever shown, so a host that needs escaping is left out.
	if strings.Contains(body, "b.example") || strings.Contains(body, "Rejected callback") {
		t.Errorf("odd host was echoed:\n%s", body)
	}
	long := strings.Repeat("trusted-prefix.", 12) + "evil.example"
	for raw, want := range map[string]string{
		"javascript:alert(1)":                 "",
		"javascript://h.example/%0aalert(1)":  "h.example",
		"":                                    "",
		"https://u:p@h.example:8443/x?y=1#z":  "h.example:8443",
		"https://[::1]:3000/cb":               "[::1]:3000",
		"https://" + strings.Repeat("a", 300): "",
		"https://" + long:                     long, // never cut: the tail is the real domain
		"https://bad\x7f.example/":            "",
		"https://パスワードを入力してください。例.jp/cb":  "", // an instruction, not a host
		"https://xn--28j2a3ar1p.example/": "xn--28j2a3ar1p.example",
		"https://a b.example/":            "",
	} {
		if got := callbackHost(raw); got != want {
			t.Errorf("callbackHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestDevIdPErrorPageCatalogueHasJaAndEnInStep(t *testing.T) {
	ja, en := devIdPErrCatalogue["ja"], devIdPErrCatalogue["en"]
	if len(devIdPErrCatalogue) != 2 {
		t.Fatalf("languages: %d", len(devIdPErrCatalogue))
	}
	var walk func(path string, a, b reflect.Value)
	walk = func(path string, a, b reflect.Value) {
		if a.Kind() == reflect.Struct {
			for i := 0; i < a.NumField(); i++ {
				walk(path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
			}
			return
		}
		if a.String() == "" || b.String() == "" {
			t.Errorf("%s is empty (ja %q, en %q)", path, a, b)
		}
		if a.String() == b.String() {
			t.Errorf("%s is not translated: %q", path, a)
		}
	}
	walk("err", reflect.ValueOf(ja), reflect.ValueOf(en))
	// Every kind has its own text, and the consent catalogue is in step too.
	seen := map[string]bool{}
	for k := devIdPErrRefused; k <= devIdPErrMethod; k++ {
		seen[en.text(k).Title] = true
	}
	if len(seen) != 5 {
		t.Errorf("kinds share a title: %v", seen)
	}
	walk("consent", reflect.ValueOf(devIdPConsentText["ja"]), reflect.ValueOf(devIdPConsentText["en"]))
}

// failingConsume is a store whose consent consumption fails, as a database outage would.
type failingConsume struct{ store.Store }

func (failingConsume) ConsumeDevIdPConsent(context.Context, string, int64) (store.DevIdPGrant, bool, error) {
	return store.DevIdPGrant{}, false, errors.New("boom: secret-internal-detail")
}

func TestDevIdPConsentStoreFailureIsTheUnavailablePage(t *testing.T) {
	d := newDevIdPTestEnv(t)
	d.mgr.store = failingConsume{d.mgr.store}
	for lang, title := range map[string]string{"en": "Can't complete the sign-in", "ja": "サインインを完了できません"} {
		rec := d.postConsent("preview-user", url.Values{"ticket": {"afdevask-x"}, "choice": {"self"}}, map[string]string{"Accept-Language": lang})
		body := wantErrPage(t, "store failure "+lang, rec, http.StatusInternalServerError, title)
		if strings.Contains(body, "boom") || strings.Contains(body, "secret-internal-detail") {
			t.Errorf("internal error reached the page: %s", body)
		}
	}
}
