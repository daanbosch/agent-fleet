package portprobe

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// rawListener accepts connections and runs handle on each; it counts them.
func rawListener(t *testing.T, handle func(net.Conn)) (addr string, conns *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	conns = new(atomic.Int32)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go handle(c)
		}
	}()
	return l.Addr().String(), conns
}

func TestProbeKinds(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := closed.Addr().String()
	_ = closed.Close()

	notHTTP, _ := rawListener(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	})
	hangsUp, _ := rawListener(t, func(c net.Conn) { _ = c.Close() })
	// What a TLS server that cannot parse a plain request answers with: a fatal alert record.
	tlsAlert, _ := rawListener(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write([]byte("\x15\x03\x01\x00\x02\x02\x0a"))
	})
	// Go's own TLS server answers a plain request with an HTTP 400, which the pane would
	// have rendered rather than failed on; the probe leaves that one alone.
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // still HTTP: no protocol reason to name
	}))
	defer httpSrv.Close()

	for _, tc := range []struct{ name, addr, want string }{
		{"nothing listening", deadAddr, Unreachable},
		{"banner that is not HTTP", notHTTP, NotHTTP},
		{"hangs up after connect", hangsUp, NotHTTP},
		{"TLS alert record", tlsAlert, TLS},
		{"Go TLS server answering HTTP 400", tlsSrv.Listener.Addr().String(), ""},
		{"HTTP server answering 400", httpSrv.Listener.Addr().String(), ""},
	} {
		if got := Probe(context.Background(), tc.addr); got != tc.want {
			t.Errorf("%s: Probe = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A port that accepts and then says nothing must cost the caller a bounded wait.
func TestProbeIsBounded(t *testing.T) {
	silent, _ := rawListener(t, func(c net.Conn) {
		defer c.Close()
		time.Sleep(10 * time.Second)
	})
	start := time.Now()
	got := Probe(context.Background(), silent)
	if el := time.Since(start); el > probeTimeout+time.Second {
		t.Fatalf("probe of a silent port took %v, bound is %v", el, probeTimeout)
	}
	if got == "" || got == Unreachable {
		t.Fatalf("silent port classified %q; it must not read as HTTP or as nothing listening", got)
	}
}

// One HEAD, no redirect followed, no second connection.
func TestProbeSendsOneHeadRequest(t *testing.T) {
	var methods, paths atomic.Int32
	var gotMethod atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods.Add(1)
		gotMethod.Store(r.Method)
		if r.URL.Path != "/" {
			paths.Add(1)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	if got := Probe(context.Background(), srv.Listener.Addr().String()); got != "" {
		t.Fatalf("Probe = %q, want \"\"", got)
	}
	if methods.Load() != 1 || paths.Load() != 0 || gotMethod.Load() != http.MethodHead {
		t.Fatalf("requests=%d offPath=%d method=%v, want one HEAD /", methods.Load(), paths.Load(), gotMethod.Load())
	}
}

// A server that speaks first (an SSH banner) must read as not-HTTP every time, not only
// when the banner loses the race against the request (measured: with http.Transport about
// 1 in 50 probes failed with "readLoopPeekFailLocked").
func TestProbeBannerFirstIsStable(t *testing.T) {
	addr, _ := rawListener(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	})
	for i := 0; i < 300; i++ {
		if got := Probe(context.Background(), addr); got != NotHTTP {
			t.Fatalf("probe %d: %q, want %q", i, got, NotHTTP)
		}
	}
}
