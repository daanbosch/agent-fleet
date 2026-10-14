package main

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/browserx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/listenports"
)

// previewProbeTimeout bounds the one HTTP request a grant probe sends.
const previewProbeTimeout = 2 * time.Second

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
	out := map[string]any{"inodes": inodes}
	if r.URL.Query().Get("probe") == "1" && len(inodes) > 0 {
		out["probe"] = probePreviewPort(port)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// probePreviewPort sends one GET / to 127.0.0.1:port and classifies the outcome with the
// same rules as the relay, so a grant is refused for exactly what the relay would fail on.
func probePreviewPort(port int) string {
	tr := &http.Transport{DisableKeepAlives: true, Proxy: nil, ResponseHeaderTimeout: previewProbeTimeout,
		DialContext: (&net.Dialer{Timeout: previewProbeTimeout}).DialContext}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return classifyPreviewError(err)
	}
	defer resp.Body.Close()
	if err := sniffPlainHTTPToTLS(resp); errors.Is(err, errPlainHTTPToTLS) {
		return previewTLS
	}
	return "http"
}
