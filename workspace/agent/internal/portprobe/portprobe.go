// Package portprobe says why a loopback port cannot be served as plain HTTP. The
// Agent's preview relay (/proxy/{port}) classifies the error its reverse proxy got;
// the browser pane, whose Chromium dials the port directly, probes the port once to
// reach the same classification. Both share this vocabulary.
package portprobe

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Failure kinds. The codes are wire-visible: the Control Plane keys its localized
// preview page on them (control-plane/preview_relay_error.go) and the browser pane
// carries two of them to the Console, so rename none.
const (
	// Unreachable: the dial itself failed — nothing is listening.
	Unreachable = "preview_unreachable"
	// NotHTTP: the port accepted the connection but did not answer with HTTP
	// (a database, SSH, h2c gRPC, a server that hangs up right after connect).
	NotHTTP = "preview_not_http"
	// TLS: the port is serving TLS and the caller speaks plain HTTP to it.
	TLS = "preview_tls"
	// Failed: any other failure (the catch-all keeps "nothing is listening" from
	// being said when it is not known to be true).
	Failed = "preview_failed"
)

// ErrPlainHTTPToTLS marks a 400 that says "you sent HTTP to an HTTPS port".
var ErrPlainHTTPToTLS = errors.New("upstream is a TLS server")

// Classify maps an error from an HTTP round trip to a failure kind.
//
// Typed checks come first. net/http has no exported type for "the reply was not HTTP":
// the status-line parser returns a plain fmt error ("malformed HTTP response …"), so
// that one case (and only it) is matched on text — the last resort.
func Classify(err error) string {
	switch {
	case errors.Is(err, ErrPlainHTTPToTLS):
		return TLS
	case errors.Is(err, syscall.ECONNREFUSED):
		return Unreachable
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		// Connected to nothing: unroutable, timed out, … — still "not listening".
		return Unreachable
	}
	msg := err.Error()
	if strings.Contains(msg, "malformed HTTP") {
		// Go quotes the offending bytes into the message. A TLS record header
		// (alert 0x15 / handshake 0x16, version 0x03xx) is what a TLS server answers a
		// plain request with.
		if strings.Contains(msg, `\x15\x03`) || strings.Contains(msg, `\x16\x03`) {
			return TLS
		}
		return NotHTTP
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		// Accepted, then hung up (or reset) without a response.
		return NotHTTP
	}
	return Failed
}

// Probe timeouts. The whole probe is bounded by probeTimeout; nothing in it may wait on
// a server that accepts and then stays silent.
const (
	dialTimeout  = 500 * time.Millisecond
	probeTimeout = 1500 * time.Millisecond
)

// Probe sends one HEAD / to addr ("127.0.0.1:3000") and returns the kind Classify gives
// the outcome, or "" when the port answered with HTTP. The caller guarantees addr is a
// loopback address of this workspace.
//
// HEAD, once, no redirects followed, no keep-alive: the browser's own navigation has
// already sent a GET to this port, so this adds no request a handler could not have seen,
// and it carries no body. A port that answers HTTP at all (even a 400) returns "": the
// pane would have loaded that page, so a navigation that failed against it did not fail
// for a protocol reason this probe can name.
func Probe(ctx context.Context, addr string) string {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+addr+"/", nil)
	if err != nil {
		return Failed
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             nil, // never route a loopback probe through an environment proxy
			DialContext:       (&net.Dialer{Timeout: dialTimeout}).DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return Classify(err)
	}
	_ = resp.Body.Close()
	return ""
}
