package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

var updateConsentGolden = flag.Bool("update-consent-golden", false,
	"rewrite testdata/dev_idp_consent_*.golden.html (state in the PR why the consent page changed)")

// The consent page shares its shell with the error pages, so a change to the shell must not move a
// byte of the consent page unless that was the intent. The random parts (ticket, persona ids, preview slug)
// are masked; everything else is compared literally.
func TestDevIdPConsentPageOutputIsPinned(t *testing.T) {
	d := newDevIdPTestEnv(t)
	d.cfg.brand.env, _ = buildBrand("violet", `Staging <1>`)
	d.addPersona(t, "admin", `Admin & "Co"`)
	d.addPersona(t, "viewer", "Viewer")
	_, challenge := pkcePair()
	ticketVal := regexp.MustCompile(`name="ticket" value="[^"]+"`)
	choiceVal := regexp.MustCompile(`name="choice" value="[0-9a-f]{32}"`)
	for _, lang := range []string{"ja", "en"} {
		var body string
		d.asUser("preview-user", func() {
			req := httptest.NewRequest(http.MethodGet, "https://af.example.com"+devIdPAuthorizePath+"?"+
				d.authQuery(d.ownHost(3000)+"/cb", challenge).Encode(), nil)
			req.Header.Set("Accept-Language", lang)
			rec := httptest.NewRecorder()
			d.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", lang, rec.Code, rec.Body)
			}
			body = rec.Body.String()
		})
		body = ticketVal.ReplaceAllString(body, `name="ticket" value="TICKET"`)
		body = choiceVal.ReplaceAllString(body, `name="choice" value="PERSONA-ID"`)
		body = strings.ReplaceAll(body, strings.TrimPrefix(d.ownHost(3000), "https://"), "PREVIEW-HOST")
		path := "testdata/dev_idp_consent_" + lang + ".golden.html"
		if *updateConsentGolden {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if body != string(want) {
			t.Errorf("%s consent page differs from %s (retake with -update-consent-golden only if the change was intended)\n--- got ---\n%s", lang, path, body)
		}
	}
}
