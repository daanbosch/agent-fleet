package browserx

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/portprobe"
)

// reasonHarness drives a fake-CDP page through failed navigations and records what the
// viewer was sent.
type reasonHarness struct {
	t      *testing.T
	m      *browserManager
	cdp    *fakeBrowserCDP
	p      *browserPage
	viewer *browserViewer
	probes atomic.Int32
}

func newReasonHarness(t *testing.T, port int, probe func(context.Context, string) string) *reasonHarness {
	t.Helper()
	cdp := newFakeBrowserCDP()
	m := fakeBrowserManager(cdp)
	t.Cleanup(m.Close)
	created, err := m.Create(browserCreateRequest{Port: port, Path: "/", Viewport: browserViewportRequest{Width: 900, Height: 600, DeviceScaleFactor: 1}})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	p := m.pages[created.ID]
	m.mu.Unlock()
	h := &reasonHarness{t: t, m: m, cdp: cdp, p: p, viewer: &browserViewer{control: make(chan browserOutbound, 64)}}
	p.mu.Lock()
	p.viewer = h.viewer
	p.mu.Unlock()
	prev := reasonProbe
	reasonProbe = func(ctx context.Context, addr string) string {
		h.probes.Add(1)
		return probe(ctx, addr)
	}
	t.Cleanup(func() { reasonProbe = prev })
	return h
}

func (h *reasonHarness) event(method, params string) {
	h.m.handleEvent(h.cdp, browserCDPEvent{Method: method, SessionID: h.p.sessionID, Params: json.RawMessage(params)})
}

// navigate starts a document navigation to url under loader id.
func (h *reasonHarness) navigate(id, url string) {
	h.m.handleRequestPaused(h.cdp, h.p, json.RawMessage(`{"requestId":"r-`+id+`","networkId":"`+id+`","frameId":"frame-1","resourceType":"Document","request":{"url":"`+url+`"}}`))
	h.event("Page.frameStartedNavigating", `{"frameId":"frame-1","loaderId":"`+id+`","url":"`+url+`","navigationType":"differentDocument"}`)
}

func (h *reasonHarness) fail(id string) {
	h.event("Network.loadingFailed", `{"requestId":"`+id+`","type":"Document","errorText":"net::ERR_EMPTY_RESPONSE"}`)
}

func (h *reasonHarness) reason() string { return h.p.response().Reason }

// stateMessages drains the viewer's queue and returns the state messages.
func (h *reasonHarness) stateMessages() []map[string]any {
	var out []map[string]any
	for {
		select {
		case o := <-h.viewer.control:
			var v map[string]any
			if json.Unmarshal(o.data, &v) == nil && v["type"] == "state" {
				out = append(out, v)
			}
		default:
			return out
		}
	}
}

func listenBanner(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// A failed navigation to a port that is listening but not HTTP carries the reason next
// to the unchanged target-unreachable state, probes once however many handlers report
// the failure, and a later navigation clears it.
func TestBrowserUnreachableCarriesProbedReason(t *testing.T) {
	port := listenBanner(t)
	h := newReasonHarness(t, port, portprobe.Probe)
	u := "http://127.0.0.1:" + strconv.Itoa(port) + "/"

	h.navigate("L1", u)
	h.fail("L1")
	if !waitFor(5*time.Second, func() bool { return h.reason() == portprobe.NotHTTP }) {
		t.Fatalf("no reason after a failed navigation to a non-HTTP port: %+v", h.p.response())
	}
	if got := h.p.response(); got.State != "target-unreachable" {
		t.Fatalf("state %q, want target-unreachable (the reason must not be a new state)", got.State)
	}
	// The same navigation reported again (Page.navigate's errorText, a re-fired event).
	h.p.settleNavigateError("L1", "net::ERR_EMPTY_RESPONSE")
	h.fail("L1")
	time.Sleep(50 * time.Millisecond)
	if n := h.probes.Load(); n != 1 {
		t.Fatalf("%d probes for one failed navigation, want 1", n)
	}
	var withReason bool
	for _, m := range h.stateMessages() {
		if m["state"] == "target-unreachable" && m["reason"] == portprobe.NotHTTP {
			withReason = true
		}
	}
	if !withReason {
		t.Fatal("the viewer was never sent {state: target-unreachable, reason: preview_not_http}")
	}

	h.navigate("L2", u) // a new navigation: the old reason must not outlive it
	if got := h.p.response(); got.State != "loading" || got.Reason != "" {
		t.Fatalf("after a new navigation: %+v, want loading with no reason", got)
	}
}

// Nothing listening keeps today's text: no reason on the wire.
func TestBrowserUnreachableWithNothingListeningHasNoReason(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	h := newReasonHarness(t, port, portprobe.Probe)
	h.navigate("L1", "http://127.0.0.1:"+strconv.Itoa(port)+"/")
	h.fail("L1")
	if !waitFor(5*time.Second, func() bool { return h.probes.Load() == 1 }) {
		t.Fatal("the failed navigation was never probed")
	}
	time.Sleep(100 * time.Millisecond)
	if got := h.p.response(); got.State != "target-unreachable" || got.Reason != "" {
		t.Fatalf("%+v, want target-unreachable with no reason", got)
	}
	for _, m := range h.stateMessages() {
		if _, ok := m["reason"]; ok {
			t.Fatalf("a refused port carried a reason: %v", m)
		}
	}
}

// A probe that finishes after the page has moved on is dropped, and a probe never holds
// the page's lock.
func TestBrowserStaleProbeResultIsDropped(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	h := newReasonHarness(t, 3000, func(ctx context.Context, addr string) string {
		<-release
		return portprobe.TLS
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	h.navigate("L1", "http://127.0.0.1:3000/")
	h.fail("L1")
	if !waitFor(5*time.Second, func() bool { return h.probes.Load() == 1 }) {
		t.Fatal("probe never started")
	}
	// The probe is parked: the page must still answer (no lock held across it).
	done := make(chan struct{})
	go func() { _ = h.p.response(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the page's lock is held while probing")
	}
	h.navigate("L2", "http://127.0.0.1:3000/other") // the page moves on
	h.stateMessages()                               // drop what was sent so far
	once.Do(func() { close(release) })
	time.Sleep(100 * time.Millisecond)
	for _, m := range h.stateMessages() {
		if m["state"] == "target-unreachable" {
			t.Fatalf("a stale failure was sent after the newer navigation: %v", m)
		}
	}
	if got := h.p.response(); got.Reason != "" {
		t.Fatalf("a stale probe result reached the page: %+v", got)
	}
}

// Only loopback targets are probed.
func TestBrowserProbeSkipsNonLoopback(t *testing.T) {
	h := newReasonHarness(t, 3000, func(context.Context, string) string { return portprobe.TLS })
	p := h.p
	p.mu.Lock()
	p.topRequestURL = "http://example.com:3000/"
	addr, key := p.beginProbeLocked("L1")
	p.mu.Unlock()
	if addr != "" || key != "" {
		t.Fatalf("probed %q for a non-loopback URL", addr)
	}
}

// Real Chromium against raw listeners: the failed navigation is probed and the reason
// reaches the page; a port with nothing behind it keeps the generic state.
func TestBrowserLiveUnreachableReason(t *testing.T) {
	factory := browserTestCDPFactory(t)
	serve := func(reply string) int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_, _ = c.Write([]byte(reply))
				_ = c.Close()
			}
		}()
		return l.Addr().(*net.TCPAddr).Port
	}
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	_ = dead.Close()

	for _, tc := range []struct {
		name   string
		port   int
		reason string
	}{
		{"not HTTP", serve("SSH-2.0-OpenSSH_9.6\r\n"), portprobe.NotHTTP},
		{"TLS alert", serve("\x15\x03\x01\x00\x02\x02\x0a"), portprobe.TLS},
		{"nothing listening", deadPort, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewBrowserManager(browserManagerConfig{
				MaxPages: 1, DetachedGrace: time.Minute, ChromiumIdle: time.Minute,
				CommandTimeout: 10 * time.Second, FrameInterval: time.Second / 12, JPEGQuality: 70,
				CDPFactory: factory,
			})
			defer m.Close()
			created, err := m.Create(browserCreateRequest{
				Port: tc.port, Path: "/", Viewport: browserViewportRequest{Width: 800, Height: 600, DeviceScaleFactor: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			p := m.pages[created.ID]
			m.mu.Unlock()
			if !waitFor(10*time.Second, func() bool { return p.response().State == "target-unreachable" }) {
				t.Fatalf("never target-unreachable: %+v", p.response())
			}
			if tc.reason != "" {
				if !waitFor(5*time.Second, func() bool { return p.response().Reason == tc.reason }) {
					t.Fatalf("reason %q never arrived: %+v", tc.reason, p.response())
				}
				return
			}
			time.Sleep(300 * time.Millisecond)
			if got := p.response(); got.Reason != "" {
				t.Fatalf("%+v, want no reason", got)
			}
		})
	}
}

// A probe that finishes before any viewer attaches has nobody to tell; the viewer that
// attaches later must still learn the reason, after the "ready" the Console takes as a
// state of its own.
func TestBrowserReasonReachesAViewerAttachedAfterTheProbe(t *testing.T) {
	port := listenBanner(t)
	h := newReasonHarness(t, port, portprobe.Probe)
	h.p.mu.Lock()
	h.p.viewer = nil
	h.p.mu.Unlock()
	previous := WorkspaceBrowserManager
	WorkspaceBrowserManager = h.m
	server := httptest.NewServer(buildMux())
	t.Cleanup(func() {
		server.Close()
		WorkspaceBrowserManager = previous
	})

	h.navigate("L1", "http://127.0.0.1:"+strconv.Itoa(port)+"/")
	h.fail("L1")
	if !waitFor(5*time.Second, func() bool { return h.reason() == portprobe.NotHTTP }) {
		t.Fatalf("no reason: %+v", h.p.response())
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/browser?id="+h.p.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var types []string
	var last map[string]any
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for last == nil || last["type"] != "state" {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read after %v: %v", types, err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		var m map[string]any
		if json.Unmarshal(payload, &m) != nil {
			continue
		}
		types = append(types, m["type"].(string))
		last = m
	}
	if last["state"] != "target-unreachable" || last["reason"] != portprobe.NotHTTP {
		t.Fatalf("late viewer got %v, want target-unreachable with %s", last, portprobe.NotHTTP)
	}
	if types[0] != "ready" {
		t.Fatalf("message order %v: ready must come first", types)
	}
}
