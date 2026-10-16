package main

import (
	"bytes"
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
// Skipped only when no Chromium is installed or the workspace says it has no browser.
func TestDevIdPConsentFormSubmitsInChromium(t *testing.T) {
	bin := os.Getenv("AF_CHROMIUM_BIN")
	if bin == "" {
		bin = "/usr/bin/chromium"
	}
	if os.Getenv("AF_BROWSER_UNAVAILABLE") != "" {
		t.Skip("no browser in this workspace")
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

	// The address is known before Serve starts, so handlers read an immutable string; the Origin the
	// POST carried goes back through a channel.
	srv := httptest.NewUnstartedServer(nil)
	baseURL := "http://" + srv.Listener.Addr().String()
	origins := make(chan string, 4)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			for k, v := range page.Header() {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
			body := strings.Replace(page.Body.String(), "</body>", "<script>document.forms[0].submit()</script></body>", 1)
			_, _ = w.Write([]byte(body))
			return
		}
		select {
		case origins <- r.Header.Get("Origin"):
		default:
		}
		api := devIdPAPI{mgr: &manager{publicBaseURL: baseURL}}
		if !api.sameOrigin(r) {
			http.Error(w, "CROSS-SITE-REFUSED", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("SUBMIT-ACCEPTED"))
	})
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--headless", "--disable-gpu", "--user-data-dir="+t.TempDir(),
		"--virtual-time-budget=5000", "--dump-dom", baseURL+"/")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// An installed Chromium that times out or crashes is a failure, not a skip: a skip here would
	// let the regression this test guards pass unnoticed.
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Chromium failed: %v (ctx: %v)\nstderr: %s", err, ctx.Err(), stderr.String())
	}
	var gotOrigin string
	select {
	case gotOrigin = <-origins:
	default:
		t.Fatalf("the form was never POSTed: %s", out)
	}
	if !strings.Contains(string(out), "SUBMIT-ACCEPTED") {
		t.Errorf("form submit not accepted (Origin seen: %q): %s", gotOrigin, out)
	}
	if gotOrigin != baseURL {
		t.Errorf("Origin on the form POST = %q, want %q", gotOrigin, baseURL)
	}
}
