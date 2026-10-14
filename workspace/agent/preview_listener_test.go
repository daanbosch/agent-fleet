package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/listenports"
)

type listenerReply struct {
	Inodes []string
	Probe  string
}

func listenerGet(t *testing.T, port, query string) (int, listenerReply) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /preview-listener/{port}", handlePreviewListener)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/preview-listener/"+port+query, nil))
	var out listenerReply
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func TestPreviewListenerReportsInodesAndProbe(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer app.Close()
	p := portOf(t, app.Listener.Addr().String())
	code, got := listenerGet(t, p, "?probe=1")
	if code != 200 || len(got.Inodes) == 0 || got.Probe != "http" {
		t.Fatalf("http listener: %d %+v", code, got)
	}

	// Without probe=1 nothing is sent to the port.
	hits := 0
	spy := rawListener(t, func(c netConn) { hits++ })
	if _, got := listenerGet(t, spy, ""); len(got.Inodes) == 0 || got.Probe != "" {
		t.Fatalf("no-probe: %+v", got)
	}
	if hits != 0 {
		t.Fatalf("a listing without probe=1 connected to the port %d times", hits)
	}

	raw := rawListener(t, func(c netConn) { _, _ = c.Write([]byte("-ERR not http\r\n")) })
	if _, got := listenerGet(t, raw, "?probe=1"); got.Probe != previewNotHTTP {
		t.Errorf("raw listener probe = %q", got.Probe)
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tls.Close()
	if _, got := listenerGet(t, portOf(t, tls.Listener.Addr().String()), "?probe=1"); got.Probe != previewTLS {
		t.Errorf("tls listener probe = %q", got.Probe)
	}
}

func TestPreviewListenerNothingListening(t *testing.T) {
	l := rawListenerClosed(t)
	code, got := listenerGet(t, l, "?probe=1")
	if code != 200 || len(got.Inodes) != 0 || got.Probe != "" {
		t.Fatalf("closed port: %d %+v", code, got)
	}
	if code, _ := listenerGet(t, "99999", ""); code != 400 {
		t.Errorf("bad port code = %d", code)
	}
	if code, _ := listenerGet(t, "7700", ""); code != 400 {
		t.Errorf("agent port code = %d", code)
	}
}

// A request bound to a listener passes while it is alive, and is refused with 410 once the
// listener is gone — also when another process took the port. The header never reaches the app.
func TestPreviewProxyChecksGrantedListener(t *testing.T) {
	var seen http.Header
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() }))
	port := portOf(t, app.Listener.Addr().String())
	inodes := listenports.Inodes("/proc", atoi(t, port))
	if len(inodes) == 0 {
		t.Fatal("no inode for a live listener")
	}
	previewListeners.TTL = 0 // each request reads /proc afresh
	t.Cleanup(func() { previewListeners.TTL = 2 * time.Second })

	get := func(hdr string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		mux.HandleFunc("/proxy/{port}/{rest...}", handlePreview)
		req := httptest.NewRequest("GET", "/proxy/"+port+"/", nil)
		req.Header.Set(previewListenerHeader, hdr)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}
	if rr := get(strings.Join(inodes, ",")); rr.Code != 200 {
		t.Fatalf("live listener: %d %s", rr.Code, rr.Body)
	}
	if seen.Get(previewListenerHeader) != "" {
		t.Errorf("listener header leaked to the app")
	}
	if rr := get("1"); rr.Code != http.StatusGone || rr.Header().Get(previewErrorHeader) != previewListenerChanged {
		t.Fatalf("wrong inode: %d %s", rr.Code, rr.Body)
	}
	app.Close()
	if rr := get(strings.Join(inodes, ",")); rr.Code != http.StatusGone {
		t.Fatalf("stopped listener: %d %s", rr.Code, rr.Body)
	}
}

func proxyWith(t *testing.T, port string, inodes []string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/{port}/{rest...}", handlePreview)
	req := httptest.NewRequest("GET", "/proxy/"+port+"/", nil)
	req.Header.Set(previewListenerHeader, strings.Join(inodes, ","))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// A grant made for the IPv4 server and an IPv6-only [::] socket on the same port must not pass
// to a different server that binds the IPv4 port after the first one stopped, even though the
// IPv6 socket (one of the granted inodes) is still there.
func TestPreviewGrantDoesNotMoveToANewSocketBesideASurvivor(t *testing.T) {
	previewListeners.TTL = 0
	t.Cleanup(func() { previewListeners.TTL = 2 * time.Second })
	l4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, l4.Addr().String())
	l6, err := net.Listen("tcp6", "[::]:"+port)
	if err != nil {
		l4.Close()
		t.Skipf("no IPv6 listener here: %v", err)
	}
	defer l6.Close()
	srv4 := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "old") })}
	go srv4.Serve(l4)
	granted := listenports.Inodes("/proc", atoi(t, port))
	if len(granted) != 2 {
		t.Fatalf("granted inodes = %v, want the tcp and tcp6 sockets", granted)
	}
	if rr := proxyWith(t, port, granted); rr.Code != 200 || rr.Body.String() != "old" {
		t.Fatalf("live grant: %d %q", rr.Code, rr.Body)
	}
	srv4.Close() // the IPv4 server stops; the IPv6 socket stays
	l4n, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "NEW") })}
	go srv.Serve(l4n)
	defer srv.Close()
	rr := proxyWith(t, port, granted)
	if rr.Code != http.StatusGone || strings.Contains(rr.Body.String(), "NEW") {
		t.Fatalf("the new server inherited the grant: %d %q", rr.Code, rr.Body)
	}
}

// With the real 2 s cache, a grant made from a fresh read passes its first request even when
// the cache was taken before the listener existed.
func TestPreviewFirstRelayAfterGrantPassesWithWarmCache(t *testing.T) {
	previewListeners.TTL = 2 * time.Second
	// Warm the cache before the listener exists.
	previewListeners.Matches(time.Now(), 1, []string{"x"})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer app.Close()
	port := portOf(t, app.Listener.Addr().String())
	granted := listenports.Inodes("/proc", atoi(t, port))
	if rr := proxyWith(t, port, granted); rr.Code != 200 {
		t.Fatalf("first request after the grant: %d %s", rr.Code, rr.Body)
	}
}
