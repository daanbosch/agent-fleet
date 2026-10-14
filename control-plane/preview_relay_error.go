package main

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/k-k1/agent-fleet/control-plane/internal/auth"
)

// previewErrorHeader is what the Agent sets on its own relay-error responses
// (workspace/agent/preview_errors.go). The Agent deletes it from every upstream response,
// so its presence means the Agent, not the previewed app, produced the failure.
const previewErrorHeader = "X-AF-Preview-Error"

// previewWorkspaceUnreachable is the CP's own failure: the dial toward the Agent failed.
// Distinct from every Agent code, which all mean "the Agent answered".
const previewWorkspaceUnreachable = "preview_workspace_unreachable"

// previewErrorCodes are the failure kinds the Agent may report; anything else in the
// header is left to pass through untouched.
var previewErrorCodes = map[string]bool{
	"preview_unreachable": true,
	"preview_not_http":    true,
	"preview_tls":         true,
	"preview_failed":      true,
}

// previewErrorText returns the localized sentence for a kind. It names the port and
// nothing the relay saw — a public preview's anonymous visitors read it, so no address,
// error string or stack ever goes in.
func previewErrorText(lang, kind string, port int) string {
	p := strconv.Itoa(port)
	if lang == "en" {
		switch kind {
		case "preview_unreachable":
			return "Nothing is listening on port " + p + " inside the workspace. Start the app and reload."
		case "preview_not_http":
			return "Port " + p + " is listening but does not speak HTTP — the preview carries HTTP and WebSocket only."
		case "preview_tls":
			return "Port " + p + " is serving HTTPS — turn off the dev server's HTTPS to preview it."
		case previewWorkspaceUnreachable:
			return "The workspace is not running or cannot be reached."
		}
		return "The preview could not complete the request to port " + p + "."
	}
	switch kind {
	case "preview_unreachable":
		return "ワークスペース内のポート " + p + " で待ち受けているものがありません。アプリを起動してから再読み込みしてください。"
	case "preview_not_http":
		return "ポート " + p + " は待ち受けていますが HTTP を話しません。プレビューは HTTP と WebSocket のみ対応です。"
	case "preview_tls":
		return "ポート " + p + " は HTTPS で動いています。プレビューするには開発サーバーの HTTPS を無効にしてください。"
	case previewWorkspaceUnreachable:
		return "ワークスペースが起動していないか、到達できません。"
	}
	return "ポート " + p + " へのプレビュー要求を完了できませんでした。"
}

// writePreviewRelayError renders a relay failure: a small localized HTML page for a
// browser navigation (the lightweight preview opens in a new tab, so the browser shows
// this body as the page), JSON for everything else (fetch / XHR callers read the code).
func writePreviewRelayError(w http.ResponseWriter, r *http.Request, kind string, port int) {
	body, ctype := previewErrorBody(r, kind, port)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(body)
}

func previewErrorBody(r *http.Request, kind string, port int) ([]byte, string) {
	lang := auth.PreferredUILang(r)
	msg := previewErrorText(lang, kind, port)
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		page := "<!doctype html><meta charset=utf-8><meta name=viewport content=\"width=device-width,initial-scale=1\">" +
			"<body style=\"font:16px system-ui;padding:2rem;background:#1e1e1e;color:#ddd\">" +
			html.EscapeString(msg) + "</body>"
		return []byte(page), "text/html; charset=utf-8"
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": kind, "message": msg}})
	return b, "application/json"
}

// rewritePreviewAgentError replaces an Agent relay-error response (marked by
// previewErrorHeader) with the localized form, keeping the Agent's specific code. It is a
// ModifyResponse step; a response without a known marker is left alone, so an app's own
// 502 is never touched.
func rewritePreviewAgentError(resp *http.Response, in *http.Request, port int) {
	kind := resp.Header.Get(previewErrorHeader)
	if !previewErrorCodes[kind] || resp.StatusCode != http.StatusBadGateway {
		return
	}
	_ = resp.Body.Close()
	body, ctype := previewErrorBody(in, kind, port)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil
	resp.Header.Del(previewErrorHeader)
	resp.Header.Set("Content-Type", ctype)
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Cache-Control", "no-store")
}
