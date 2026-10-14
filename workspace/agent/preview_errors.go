package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
)

// Preview relay failure kinds. The codes are wire-visible: the Control Plane keys its
// localized page on them (control-plane/preview_relay_error.go), so rename none.
const (
	// previewUnreachable: the dial itself failed — nothing is listening.
	previewUnreachable = "preview_unreachable"
	// previewNotHTTP: the port accepted the connection but did not answer with HTTP
	// (a database, SSH, h2c gRPC, a server that hangs up right after connect).
	previewNotHTTP = "preview_not_http"
	// previewTLS: the port is serving TLS and the relay speaks plain HTTP to it.
	previewTLS = "preview_tls"
	// previewFailed: any other relay failure (the catch-all keeps "nothing is listening"
	// from being said when it is not known to be true).
	previewFailed = "preview_failed"
)

// previewErrorHeader marks a response as the Agent's own relay error rather than the
// previewed app's, so the Control Plane can tell the two apart. handlePreview deletes it
// from every upstream response: an app must not be able to forge it.
const previewErrorHeader = "X-AF-Preview-Error"

// errPlainHTTPToTLS is returned by sniffPlainHTTPToTLS when a 400 says "you sent HTTP to
// an HTTPS port".
var errPlainHTTPToTLS = errors.New("upstream is a TLS server")

// classifyPreviewError maps an error from the reverse proxy to a failure kind.
//
// Typed checks come first. net/http has no exported type for "the reply was not HTTP":
// the status-line parser returns a plain fmt error ("malformed HTTP response …"), so
// that one case (and only it) is matched on text — the last resort.
func classifyPreviewError(err error) string {
	switch {
	case errors.Is(err, errPlainHTTPToTLS):
		return previewTLS
	case errors.Is(err, syscall.ECONNREFUSED):
		return previewUnreachable
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		// Connected to nothing: unroutable, timed out, … — still "not listening".
		return previewUnreachable
	}
	msg := err.Error()
	if strings.Contains(msg, "malformed HTTP") {
		// Go quotes the offending bytes into the message. A TLS record header
		// (alert 0x15 / handshake 0x16, version 0x03xx) is what a TLS server answers a
		// plain request with.
		if strings.Contains(msg, `\x15\x03`) || strings.Contains(msg, `\x16\x03`) {
			return previewTLS
		}
		return previewNotHTTP
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		// Accepted, then hung up (or reset) without a response.
		return previewNotHTTP
	}
	return previewFailed
}

// previewErrorMessage is the English text carried in the JSON body. It names the port and
// nothing else: a public preview's visitors see it.
func previewErrorMessage(kind string, port int) string {
	p := strconv.Itoa(port)
	switch kind {
	case previewUnreachable:
		return "nothing is listening on 127.0.0.1:" + p + " inside the workspace"
	case previewNotHTTP:
		return "127.0.0.1:" + p + " is listening but does not speak HTTP; the preview carries HTTP and WebSocket only"
	case previewTLS:
		return "127.0.0.1:" + p + " is serving HTTPS; turn off the dev server's HTTPS to preview it"
	}
	return "the preview could not complete the request to 127.0.0.1:" + p
}

func writePreviewError(w http.ResponseWriter, kind string, port int) {
	w.Header().Set(previewErrorHeader, kind)
	httpx.WriteErr(w, http.StatusBadGateway, kind, previewErrorMessage(kind, port))
}

// sniffPlainHTTPToTLS turns a 400 whose body says "plain HTTP sent to an HTTPS port"
// into errPlainHTTPToTLS. Servers answer this on the TLS port itself (Go, nginx, Apache),
// so it reaches the browser as a bare 400 that explains nothing. There is no typed signal,
// so the body text is matched — only for a 400 with a small or close-delimited body,
// never for chunked/streamed ones, so nothing that streams is held back.
func sniffPlainHTTPToTLS(resp *http.Response) error {
	if resp.StatusCode != http.StatusBadRequest || len(resp.TransferEncoding) > 0 ||
		resp.ContentLength > 512 {
		return nil
	}
	buf := make([]byte, 256)
	n, _ := io.ReadFull(resp.Body, buf)
	buf = buf[:n]
	if looksLikeHTTPToHTTPSNotice(buf) {
		_ = resp.Body.Close()
		return errPlainHTTPToTLS
	}
	orig := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), orig), orig}
	return nil
}

func looksLikeHTTPToHTTPSNotice(b []byte) bool {
	s := strings.ToLower(string(b))
	for _, m := range []string{
		"http request to an https server",              // Go net/http
		"plain http request was sent to https port",    // nginx
		"speaking plain http to an ssl-enabled server", // Apache
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
