package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestDevIdPConsentFormSubmitsInChromium checks what the Go-level tests cannot: the Origin a real
// browser puts on the consent form's own POST, which depends on the page's Referrer-Policy.
// The real consent page (body and headers) is replayed from a local server; only the CSP is
// relaxed and an auto-submit script injected, because headless Chromium has no one to click.
// Skipped when no Chromium is installed.
func TestDevIdPConsentFormSubmitsInChromium(t *testing.T) {
	bin := os.Getenv("AF_CHROMIUM_BIN")
	if bin == "" {
		bin = "/usr/bin/chromium"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no Chromium at " + bin)
	}
	d := newDevIdPTestEnv(t)
	d.addPersona(t, "admin", "Admin")
	page := d.authorizeAs(t, "preview-user", d.authQuery(d.ownHost(3000)+"/cb", ""))
	if page.Code != http.StatusOK {
		t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}

	var gotOrigin string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			for k, v := range page.Header() {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
			body := strings.Replace(page.Body.String(), "</body>", "<script>document.forms[0].submit()</script></body>", 1)
			_, _ = w.Write([]byte(body))
			return
		}
		gotOrigin = r.Header.Get("Origin")
		api := devIdPAPI{mgr: &manager{publicBaseURL: srv.URL}}
		if !api.sameOrigin(r) {
			http.Error(w, "CROSS-SITE-REFUSED", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("SUBMIT-ACCEPTED"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	out, err := exec.CommandContext(ctx, bin, "--headless", "--disable-gpu", "--user-data-dir="+dir,
		"--virtual-time-budget=5000", "--dump-dom", srv.URL+"/").Output()
	if err != nil {
		t.Skipf("Chromium would not run here: %v", err)
	}
	if !strings.Contains(string(out), "SUBMIT-ACCEPTED") {
		t.Errorf("form submit not accepted (Origin seen: %q): %s", gotOrigin, out)
	}
	if gotOrigin != srv.URL {
		t.Errorf("Origin on the form POST = %q, want %q", gotOrigin, srv.URL)
	}
}
