// dev_idp_page.go — what the development IdP's browser-facing pages share: the shell (header,
// tokens, card) the consent page and the error pages are drawn in, the response headers, and the
// error pages themselves. Token, userinfo and jwks answer in OAuth JSON and are not pages.
package main

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"unicode"

	"github.com/k-k1/agent-fleet/control-plane/internal/auth"
)

// devIdPShell is what the shell template reads: language, the deployment's brand colour and label,
// and the icon (the branded /brand/ asset, served from the page's own origin).
type devIdPShell struct {
	Lang, Accent, BrandLabel, Icon string
}

func (a devIdPAPI) shell(r *http.Request, lang string) devIdPShell {
	br := a.brand.get(r.Context())
	s := devIdPShell{Lang: lang, Accent: brandBaseHex, BrandLabel: br.label, Icon: "/brand/icon-192.png"}
	if br.hex != "" {
		s.Accent = br.hex
	}
	return s
}

// setDevIdPPageHeaders is the header set of every HTML page this IdP serves.
func setDevIdPPageHeaders(w http.ResponseWriter) {
	noStore(w)
	// Never no-referrer: it makes the browser send "Origin: null" on this page's own form POST, which
	// sameOrigin refuses (observed: every submit got 403). same-origin still sends no Referer to the app.
	w.Header().Set("Referrer-Policy", "same-origin")
	// The page is a click target: refuse framing so it cannot be clicked through. No form-action:
	// Chromium applies it to the 302 that follows the POST too, so 'self' would block the return
	// to the app on its preview host (measured in headless Chromium). The page has no script and
	// the form's own action is fixed in the template.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// devIdPShellTmpl holds the parts both pages are made of. The page is inline CSS only (the CSP
// allows no script, font or remote style). Dark is the Console's default; the light tokens apply
// under prefers-color-scheme. --accent is the deployment's brand colour.
var devIdPShellTmpl = template.Must(template.New("shell").Parse(`{{define "devidp-shell-css"}}:root{--bg:#1e1e1e;--panel:#181818;--panel2:#141414;--border:#333;--fg:#ddd;--muted:#888;--accent-fg:#fff;--warn:#e0a23a;--sel:#ffffff12}
@media (prefers-color-scheme: light){:root{--bg:#f4f5f7;--panel:#fff;--panel2:#eaecef;--border:#d2d6dc;--fg:#1c2024;--muted:#4b5563;--sel:#00000009;--warn:#a15c00}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI","Hiragino Sans","Noto Sans JP",sans-serif;padding:24px}
.card{width:100%;max-width:440px;background:var(--panel);border:1px solid var(--border);border-radius:14px;overflow:hidden;box-shadow:0 10px 40px #0006}
.top{display:flex;flex-wrap:wrap;align-items:center;gap:6px 10px;padding:14px 20px;border-bottom:1px solid var(--border);background:var(--panel2)}
.top img{flex:none;width:28px;height:28px;border-radius:7px}
.top .brand{font-weight:600;letter-spacing:.01em}
.top .dep{color:var(--muted);font-size:13px;min-width:0;max-width:100%;overflow-wrap:anywhere}
.badge{margin-left:auto;flex:none;font-size:11px;font-weight:700;letter-spacing:.08em;color:var(--warn);border:1px solid currentColor;border-radius:999px;padding:2px 8px}
.body{padding:22px 20px 18px}
h1{font-size:20px;margin:0 0 6px}
.lead{color:var(--muted);margin:0 0 16px;font-size:14px}
{{end}}
{{define "devidp-top"}}<div class="top"><img src="{{.Icon}}" alt=""><span class="brand">Agent Fleet</span>{{if .BrandLabel}}<span class="dep">· {{.BrandLabel}}</span>{{end}}<span class="badge">DEV IdP</span></div>{{end}}`))

// devIdPErrKind picks the wording of an error page.
type devIdPErrKind int

const (
	devIdPErrRefused     devIdPErrKind = iota // the request cannot be trusted (unknown client, not a member)
	devIdPErrRedirect                         // the callback address is not one this client may use
	devIdPErrExpired                          // the consent confirmation was refused, spent or expired
	devIdPErrUnavailable                      // the store failed; retrying is all the member can do
	devIdPErrMethod                           // the address was opened on its own, not by an app
)

type devIdPErrText struct{ Title, What, Next string }

// devIdPErrStrings is the error pages' text for one language, chosen like the consent page's.
type devIdPErrStrings struct {
	HostLabel                                       string
	Refused, Redirect, Expired, Unavailable, Method devIdPErrText
}

func (s devIdPErrStrings) text(k devIdPErrKind) devIdPErrText {
	switch k {
	case devIdPErrRedirect:
		return s.Redirect
	case devIdPErrExpired:
		return s.Expired
	case devIdPErrUnavailable:
		return s.Unavailable
	case devIdPErrMethod:
		return s.Method
	}
	return s.Refused
}

var devIdPErrCatalogue = map[string]devIdPErrStrings{
	"ja": {
		HostLabel: "拒否された戻り先",
		Refused: devIdPErrText{
			Title: "サインインを開始できません",
			What:  "このサインイン要求は受け付けられませんでした。",
			Next:  "アプリに設定したクライアント ID とコールバック URL がこのワークスペースのものか確認し、アプリからもう一度サインインを始めてください。",
		},
		Redirect: devIdPErrText{
			Title: "戻り先のアドレスが許可されていません",
			What:  "アプリが、このワークスペースでは使えないアドレスへのサインイン完了を求めました。",
			Next:  "アプリのコールバック URL が、このワークスペースの現在のプレビュー URL か http://localhost:<ポート> になっているか確認し、アプリからもう一度サインインを始めてください。",
		},
		Expired: devIdPErrText{
			Title: "サインインの確認が無効です",
			What:  "この確認は拒否されたか、期限が切れたか、すでに使われています。",
			Next:  "アプリからもう一度サインインを始めてください。",
		},
		Unavailable: devIdPErrText{
			Title: "サインインを完了できません",
			What:  "サーバー側の問題で、開発用サインインを完了できませんでした。",
			Next:  "少し待ってからアプリでもう一度お試しください。続く場合はこのデプロイの管理者に連絡してください。",
		},
		Method: devIdPErrText{
			Title: "アプリからサインインしてください",
			What:  "このアドレスは開発用サインインの途中の手順で、単独では開けません。",
			Next:  "プレビュー中のアプリからサインインを始めてください。",
		},
	},
	"en": {
		HostLabel: "Rejected callback",
		Refused: devIdPErrText{
			Title: "Can't start the sign-in",
			What:  "This sign-in request was refused.",
			Next:  "Check that the client ID and callback URL set in the app belong to this workspace, then start the sign-in again from the app.",
		},
		Redirect: devIdPErrText{
			Title: "Callback address not allowed",
			What:  "The app asked to finish the sign-in at an address this workspace does not allow.",
			Next:  "Check that the app's callback URL is this workspace's current preview URL or http://localhost:<port>, then start the sign-in again from the app.",
		},
		Expired: devIdPErrText{
			Title: "Sign-in confirmation is no longer valid",
			What:  "This confirmation was refused, has expired, or was already used.",
			Next:  "Start the sign-in again from the app.",
		},
		Unavailable: devIdPErrText{
			Title: "Can't complete the sign-in",
			What:  "A problem on the server stopped the development sign-in from finishing.",
			Next:  "Try again from the app in a moment. If it keeps failing, ask this deployment's administrator.",
		},
		Method: devIdPErrText{
			Title: "Sign in from your app",
			What:  "This address is one step of a development sign-in and cannot be opened on its own.",
			Next:  "Start the sign-in from the app you are previewing.",
		},
	},
}

// devIdPErrTmpl: nothing on the page posts, links or redirects.
var devIdPErrTmpl = template.Must(template.Must(devIdPShellTmpl.Clone()).New("error").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · Agent Fleet</title>
<style>
{{template "devidp-shell-css"}}.app{padding:10px 12px;border:1px dashed var(--border);border-radius:10px;margin-bottom:16px;font-size:13px}
.app small{display:block;color:var(--muted);font-size:12px;margin-bottom:2px}
.app code{font:12.5px ui-monospace,SFMono-Regular,Menlo,monospace;overflow-wrap:anywhere}
.next{margin:0;padding-top:14px;border-top:1px solid var(--border);font-size:14px}
</style></head><body style="--accent:{{.Accent}}">
<main class="card">
{{template "devidp-top" .}}
<div class="body">
<h1>{{.Title}}</h1>
<p class="lead">{{.What}}</p>
{{if .Host}}<div class="app"><small>{{.HostLabel}}</small><code>{{.Host}}</code></div>
{{end}}<p class="next">{{.Next}}</p>
</div>
</main>
</body></html>`))

// writeErrPage answers a browser navigation that cannot proceed. The reason it failed is logged by
// the caller and never shown; host is the rejected callback's host, or "".
func (a devIdPAPI) writeErrPage(w http.ResponseWriter, r *http.Request, status int, kind devIdPErrKind, host string) {
	lang := auth.PreferredUILang(r)
	t := devIdPErrCatalogue[lang]
	data := struct {
		devIdPShell
		devIdPErrText
		HostLabel, Host string
	}{a.shell(r, lang), t.text(kind), t.HostLabel, host}
	var buf bytes.Buffer
	if err := devIdPErrTmpl.Execute(&buf, data); err != nil {
		log.Printf("dev idp: error page: %v", err)
		http.Error(w, http.StatusText(status), status)
		return
	}
	setDevIdPPageHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// callbackHost is what an error page may say about a rejected redirect_uri: its host and nothing
// else (no path, query or userinfo, which can carry a state or a secret). Anything that is not a
// plain printable host name comes back empty, and a long one is cut.
func callbackHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	h := []rune(u.Host)
	if len(h) > 100 {
		h = append(h[:100], '…')
	}
	for _, c := range h {
		if !unicode.IsPrint(c) {
			return ""
		}
	}
	return string(h)
}
