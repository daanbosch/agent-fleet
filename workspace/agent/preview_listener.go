package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/browserx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/listenports"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/portprobe"
)

// previewProbeTimeout bounds the one HTTP request a grant probe sends.
const previewProbeTimeout = 2 * time.Second

// previewListenerReply is the body of GET /preview-listener/{port}. Probe is absent unless
// probe=1 was asked for and something listens.
type previewListenerReply struct {
	Inodes []string `json:"inodes"`
	Probe  string   `json:"probe,omitempty"`
}

// handlePreviewListener answers GET /preview-listener/{port}[?probe=1] for the Control
// Plane when the owner opens a port that is not on the preview list: the inodes of the
// sockets listening on it now (the grant binds to them), and with probe=1 whether the port
// answers HTTP at all.
//
//	{"inodes":["123","456"],"probe":"http"}   probe ∈ http | preview_not_http | preview_tls |
//	                                          preview_failed | preview_unreachable
//
// The probe is an explicit act of the click, never part of the periodic port scan: probing
// every listener every few seconds would put a line like "invalid startup packet" in a
// database's log each time. It sends one request, with a short timeout.
func handlePreviewListener(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 || browserx.ReservedBrowserAgentPort(port) {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_port", "preview port must be 1..65535 and not the workspace agent")
		return
	}
	inodes := listenports.Inodes("/proc", port)
	if inodes == nil {
		inodes = []string{}
	}
	out := previewListenerReply{Inodes: inodes}
	if r.URL.Query().Get("probe") == "1" && len(inodes) > 0 {
		out.Probe = probePreviewPort(port)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// probePreviewPort says whether 127.0.0.1:port can be previewed: "http", or the failure kind the
// relay would report. portprobe.Probe does the classification shared with the browser pane (a
// hand-driven HEAD, so a server that speaks first reads as not-HTTP, not as a transport quirk).
// A port that answered HTTP gets one more look for the "plain HTTP sent to an HTTPS port" 400
// notice a TLS dev server answers with, which the HEAD cannot show (it has no body).
func probePreviewPort(port int) string {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if kind := portprobe.Probe(context.Background(), addr); kind != "" {
		return kind
	}
	tr := &http.Transport{DisableKeepAlives: true, Proxy: nil, ResponseHeaderTimeout: previewProbeTimeout,
		DialContext: (&net.Dialer{Timeout: previewProbeTimeout}).DialContext}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return "http" // it answered HTTP a moment ago: a later hiccup is not a protocol verdict
	}
	defer resp.Body.Close()
	if err := sniffPlainHTTPToTLS(resp); errors.Is(err, errPlainHTTPToTLS) {
		return previewTLS
	}
	return "http"
}
