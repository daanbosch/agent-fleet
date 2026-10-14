package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPreviewForwardsPublicHostHeaders pins that the public name the CP attached reaches
// the previewed app.
//
// Why it is needed: in Rewrite mode httputil.ReverseProxy DELETES Forwarded /
// X-Forwarded-For / -Host / -Proto from Out before calling Rewrite (measured). "Anything
// you do not touch passes through" is false; the only header that was passing through was
// X-Forwarded-Prefix, which is not on that list.
//
// What happens when this breaks: Next.js checks Origin against x-forwarded-host for
// Server Actions and answers 403. With the headers gone, every Server Action through the
// preview is a 403 — and it looks like the app is broken rather than the proxy
// (docs/log/81 §2.5 (c)).
func TestPreviewForwardsPublicHostHeaders(t *testing.T) {
	var got http.Header
	var sawHost string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		sawHost = r.Host
	}))
	defer app.Close()

	port := app.Listener.Addr().(interface{ String() string }).String()
	// httptest listens on 127.0.0.1:<port> and handlePreview dials 127.0.0.1:{port}, so
	// pass it the port number alone.
	p := port[len("127.0.0.1:"):]

	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/{port}/{rest...}", handlePreview)
	req := httptest.NewRequest("GET", "/proxy/"+p+"/some/path", nil)
	req.Header.Set("X-Forwarded-Host", "abcdefghij0123456789-3000.pv.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if v := got.Get("X-Forwarded-Host"); v != "abcdefghij0123456789-3000.pv.example.com" {
		t.Errorf("X-Forwarded-Host = %q, want the public preview host", v)
	}
	if v := got.Get("X-Forwarded-Proto"); v != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", v)
	}
	// Host is forwarded as the internal address (decision 9). That is the only way to
	// pass a dev server's host check (Vite's allowedHosts, Next's allowedDevOrigins)
	// without making the user configure it; the public name rides X-Forwarded-Host.
	if sawHost != "127.0.0.1:"+p {
		t.Errorf("upstream Host = %q, want 127.0.0.1:%s", sawHost, p)
	}
	// The CP↔Agent bearer is never shown to the app.
	if v := got.Get("Authorization"); v != "" {
		t.Errorf("Authorization leaked to the previewed app: %q", v)
	}
}

// previewGet runs GET /proxy/{port}/ through handlePreview and returns the recorder.
func previewGet(t *testing.T, port string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/{port}/{rest...}", handlePreview)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/proxy/"+port+"/", nil))
	return rr
}

func portOf(t *testing.T, addr string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func wantPreviewError(t *testing.T, rr *httptest.ResponseRecorder, code, port string) {
	t.Helper()
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rr.Code, rr.Body)
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code {
		t.Errorf("code = %q, want %q (message %q)", body.Error.Code, code, body.Error.Message)
	}
	if got := rr.Header().Get(previewErrorHeader); got != code {
		t.Errorf("%s = %q, want %q", previewErrorHeader, got, code)
	}
	if !strings.Contains(body.Error.Message, port) {
		t.Errorf("message %q does not name port %s", body.Error.Message, port)
	}
}

func TestPreviewRefusedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := portOf(t, l.Addr().String())
	l.Close() // nothing listens any more
	wantPreviewError(t, previewGet(t, p), "preview_unreachable", p)
}

// rawListener accepts connections and runs fn on each.
func rawListener(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				// Read the request first, as a real server does; answering unasked
				// makes the client reject the reply as unsolicited.
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				fn(c)
			}()
		}
	}()
	return portOf(t, l.Addr().String())
}

func TestPreviewNonHTTPListener(t *testing.T) {
	// Writes something that is not an HTTP status line (a Redis/Postgres-like banner).
	p := rawListener(t, func(c net.Conn) { _, _ = c.Write([]byte("+PONG not http\r\n\r\n")) })
	wantPreviewError(t, previewGet(t, p), "preview_not_http", p)

	// Accepts and hangs up without a byte.
	p = rawListener(t, func(c net.Conn) {})
	wantPreviewError(t, previewGet(t, p), "preview_not_http", p)
}

func TestPreviewTLSListener(t *testing.T) {
	// Go's TLS server answers a plain request with a 400 saying so.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	p := portOf(t, srv.Listener.Addr().String())
	wantPreviewError(t, previewGet(t, p), "preview_tls", p)

	// A server that answers with a raw TLS alert record instead.
	p = rawListener(t, func(c net.Conn) {
		_, _ = c.Write([]byte{0x15, 0x03, 0x01, 0x00, 0x02, 0x02, 0x16})
	})
	wantPreviewError(t, previewGet(t, p), "preview_tls", p)
}

// An app's own 400 (and its own forged error header) must pass untouched.
func TestPreviewAppBadRequestAndForgedHeaderPass(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(previewErrorHeader, "preview_tls")
		http.Error(w, "bad input", http.StatusBadRequest)
	}))
	defer app.Close()
	rr := previewGet(t, portOf(t, app.Listener.Addr().String()))
	if rr.Code != http.StatusBadRequest || strings.TrimSpace(rr.Body.String()) != "bad input" {
		t.Fatalf("got %d %q, want the app's own 400", rr.Code, rr.Body)
	}
	if h := rr.Header().Get(previewErrorHeader); h != "" {
		t.Errorf("forged %s survived: %q", previewErrorHeader, h)
	}
}

// Streaming (SSE) and WebSocket-style upgrades keep working: no header timeout, and the
// 400 sniff never touches other statuses.
func TestPreviewStreamingAndUpgradeStillWork(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "echo" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			buf := make([]byte, 4)
			io.ReadFull(rw, buf)
			conn.Write(buf)
			return
		}
		// Long-poll: headers held back past any plausible header timeout would be cut;
		// keep it short for the test but longer than the sniffing path.
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: hi\n\n")
	}))
	defer app.Close()
	p := portOf(t, app.Listener.Addr().String())

	rr := previewGet(t, p)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "data: hi") {
		t.Fatalf("long-poll: %d %q", rr.Code, rr.Body)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/{port}/{rest...}", handlePreview)
	front := httptest.NewServer(mux)
	defer front.Close()
	c, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /proxy/%s/ws HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", p)
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("upgrade status line = %q, err %v", line, err)
	}
	for {
		l, _ := br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
	c.Write([]byte("ping"))
	got := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo through upgrade = %q, %v", got, err)
	}
}

// getThroughFront sends GET /proxy/{port}/ through a live handlePreview server and
// returns the response as soon as its headers arrive (or fails after 3 s).
func getThroughFront(t *testing.T, port string) *http.Response {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/{port}/{rest...}", handlePreview)
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(front.URL + "/proxy/" + port + "/")
	if err != nil {
		t.Fatalf("headers did not arrive: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A 400 that is really a live stream (close-delimited, never ends) must not be held back
// by the TLS sniff: neither an event stream nor an unknown body that stalls.
func TestPreviewStreaming400IsNotHeldBack(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	for name, head := range map[string]string{
		"sse":      "HTTP/1.0 400 Bad Request\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\ndata: one\n\n",
		"no-type":  "HTTP/1.0 400 Bad Request\r\n\r\n", // headers only, body comes later
		"no-type2": "HTTP/1.0 400 Bad Request\r\n\r\nstreaming...",
	} {
		t.Run(name, func(t *testing.T) {
			p := rawListener(t, func(c net.Conn) {
				_, _ = c.Write([]byte(head))
				<-hold
			})
			start := time.Now()
			resp := getThroughFront(t, p)
			if resp.StatusCode != 400 {
				t.Fatalf("status = %d, want the app's 400", resp.StatusCode)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("headers took %v", d)
			}
		})
	}
}

// An app's own JSON 400 that merely quotes the phrase is the app's, not a TLS signal.
func TestPreviewAppJSON400QuotingNoticeIsKept(t *testing.T) {
	const body = `{"error":"invalid input","rejected":"http request to an https server"}`
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, body)
	}))
	defer app.Close()
	rr := previewGet(t, portOf(t, app.Listener.Addr().String()))
	if rr.Code != 400 || rr.Body.String() != body {
		t.Fatalf("got %d %q, want the app's 400 untouched", rr.Code, rr.Body)
	}
}

// The notice may arrive split across reads; it is still recognised.
func TestPreviewTLSNoticeSplitAcrossWrites(t *testing.T) {
	p := rawListener(t, func(c net.Conn) {
		_, _ = c.Write([]byte("HTTP/1.0 400 Bad Request\r\n\r\nClient sent an HTTP "))
		time.Sleep(50 * time.Millisecond)
		_, _ = c.Write([]byte("request to an HTTPS server.\n"))
	})
	wantPreviewError(t, previewGet(t, p), "preview_tls", p)
}

// An ordinary 400 whose body is late, or split, comes through whole with its length.
func TestPreviewLate400BodyIsDeliveredWhole(t *testing.T) {
	p := rawListener(t, func(c net.Conn) {
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 12\r\nConnection: close\r\n\r\nhello "))
		time.Sleep(400 * time.Millisecond) // longer than sniffWait
		_, _ = c.Write([]byte("world\n"))
	})
	rr := previewGet(t, p)
	if rr.Code != 400 || rr.Body.String() != "hello world\n" || rr.Header().Get("Content-Length") != "12" {
		t.Fatalf("got %d %q CL=%q", rr.Code, rr.Body, rr.Header().Get("Content-Length"))
	}
}

// A first body chunk that arrives and is then followed by silence reaches the client
// while the upstream is still open: the sniff's wait must not park bytes already read.
func TestPreviewFirstBodyChunkOf400StreamIsNotParked(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	p := rawListener(t, func(c net.Conn) {
		_, _ = c.Write([]byte("HTTP/1.0 400 Bad Request\r\n\r\nstreaming..."))
		<-hold
	})
	resp := getThroughFront(t, p)
	got := make([]byte, len("streaming..."))
	if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != "streaming..." {
		t.Fatalf("first chunk = %q, err %v (upstream still open)", got, err)
	}
}

type headEOFBody struct{ data []byte }

func (b *headEOFBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}
func (b *headEOFBody) Close() error { return nil }

// The sniff finishing exactly as the wait runs out must not be mistaken for an abandon: the
// body and its EOF still arrive.
func TestPreviewSniffFinishingAtTheTimeoutBoundary(t *testing.T) {
	sniffDecidedHook = func() { time.Sleep(sniffWait + 50*time.Millisecond) }
	t.Cleanup(func() { sniffDecidedHook = nil })
	resp := &http.Response{StatusCode: 400, Header: http.Header{}, ContentLength: -1,
		Body: &headEOFBody{data: []byte("bad input")}}
	if err := sniffPlainHTTPToTLS(resp); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { b, err := io.ReadAll(resp.Body); done <- string(b) + "|" + fmt.Sprint(err) }()
	select {
	case got := <-done:
		if got != "bad input|<nil>" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("body read hung at the sniff boundary")
	}
}

func TestPreviewSniffTLSNoticeAtTheTimeoutBoundary(t *testing.T) {
	sniffDecidedHook = func() { time.Sleep(sniffWait + 50*time.Millisecond) }
	t.Cleanup(func() { sniffDecidedHook = nil })
	resp := &http.Response{StatusCode: 400, Header: http.Header{}, ContentLength: -1,
		Body: &headEOFBody{data: []byte("Client sent an HTTP request to an HTTPS server.\n")}}
	if err := sniffPlainHTTPToTLS(resp); !errors.Is(err, errPlainHTTPToTLS) {
		t.Fatalf("err = %v, want errPlainHTTPToTLS", err)
	}
}
