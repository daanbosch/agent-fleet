package main

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/listenports"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/portprobe"
)

// Preview relay failure kinds; the definitions live in portprobe, which the browser pane
// shares. The codes are wire-visible: the Control Plane keys its localized page on them
// (control-plane/preview_relay_error.go), so rename none.
const (
	previewUnreachable = portprobe.Unreachable
	previewNotHTTP     = portprobe.NotHTTP
	previewTLS         = portprobe.TLS
	previewFailed      = portprobe.Failed
)

// previewListenerChanged: the listener a temporary grant was bound to is gone (the server
// stopped, or another process took the port). Answered with 410, not 502. Specific to the
// relay, so it is not part of portprobe's vocabulary.
const previewListenerChanged = "preview_listener_changed"

// previewListenerHeader carries a grant's listener inodes, comma separated, from the CP.
const previewListenerHeader = "X-AF-Preview-Listener"

// previewListeners answers "does the grant still describe this port's listeners" with a 2 s
// cache: the bound on how long a stopped listener keeps passing.
var previewListeners = &listenports.InodeCache{Root: "/proc", TTL: 2 * time.Second}

func splitInodes(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// previewErrorHeader marks a response as the Agent's own relay error rather than the
// previewed app's, so the Control Plane can tell the two apart. The contract covers the
// FINAL response header only: handlePreview deletes it there, and the Control Plane reads
// nothing else (not 1xx headers, not trailers), so an app cannot forge it by those routes.
const previewErrorHeader = "X-AF-Preview-Error"

// errPlainHTTPToTLS is returned by sniffPlainHTTPToTLS when a 400 says "you sent HTTP to
// an HTTPS port".
var errPlainHTTPToTLS = portprobe.ErrPlainHTTPToTLS

// classifyPreviewError maps an error from the reverse proxy to a failure kind.
func classifyPreviewError(err error) string { return portprobe.Classify(err) }

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
	case previewListenerChanged:
		return "the listener on port " + p + " has changed; reopen it from the Console"
	}
	return "the preview could not complete the request to 127.0.0.1:" + p
}

func writePreviewError(w http.ResponseWriter, kind string, port int) {
	w.Header().Set(previewErrorHeader, kind)
	status := http.StatusBadGateway
	if kind == previewListenerChanged {
		status = http.StatusGone
	}
	httpx.WriteErr(w, status, kind, previewErrorMessage(kind, port))
}

// sniffWait bounds how long a 400 candidate may hold its headers back while its first
// body bytes are collected. The known notices arrive with the headers, so waiting longer
// only ever delays a stream that happens to answer 400.
const (
	sniffWait  = 200 * time.Millisecond
	sniffLimit = 256 // bytes examined
)

// sniffPlainHTTPToTLS turns a 400 whose body says "plain HTTP sent to an HTTPS port"
// into errPlainHTTPToTLS. Servers answer this on the TLS port itself (Go, nginx, Apache),
// so it reaches the browser as a bare 400 that explains nothing. There is no typed signal,
// so the body text is matched, narrowly: only a 400 whose Content-Type is absent, text/plain
// or text/html (those servers' own pages; an app's JSON or event stream is never touched),
// only the first sniffLimit bytes, and at most sniffWait. A body still unfinished by then is
// handed on, whatever was read so far first, and the rest follows as it arrives.
func sniffPlainHTTPToTLS(resp *http.Response) error {
	if resp.StatusCode != http.StatusBadRequest || len(resp.TransferEncoding) > 0 ||
		resp.ContentLength > 512 || !plainNoticeContentType(resp.Header.Get("Content-Type")) {
		return nil
	}
	sn := &sniffer{orig: resp.Body, finished: make(chan struct{}), lateDone: make(chan struct{})}
	go sn.collect()
	select {
	case <-sn.finished:
	case <-time.After(sniffWait):
		// "finished" and "abandoned" are decided under the one mutex, so exactly one of
		// them holds: a sniff that ended on its own can never also be abandoned (nobody
		// would be left to close lateDone).
		sn.mu.Lock()
		if !sn.done {
			// Out of time: hand over what has been read so far. The Read in flight is not
			// waited for and its result is not tied to forwarding the prefix; it is
			// delivered after the prefix when it completes.
			sn.abandoned = true
		}
		sn.mu.Unlock()
	}
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if sn.abandoned {
		resp.Body = &peekBody{sn: sn, prefix: append([]byte(nil), sn.got...)}
		return nil
	}
	if looksLikeHTTPToHTTPSNotice(sn.got) {
		_ = sn.orig.Close()
		return errPlainHTTPToTLS
	}
	resp.Body = &peekBody{sn: sn, prefix: sn.got, err: sn.err, ready: true}
	return nil
}

// sniffDecidedHook, when set by a test, runs after the sniff has decided it is done and
// before it announces so, to stretch the window a timeout could otherwise slip into.
var sniffDecidedHook func()

// sniffer reads the head of a body on its own goroutine so the caller can stop waiting.
// The goroutine is the only reader of orig until it finishes or is abandoned.
type sniffer struct {
	orig io.ReadCloser

	mu        sync.Mutex
	got       []byte // bytes read so far
	err       error  // read error that ended the sniff
	abandoned bool
	done      bool          // the sniff ended on its own; exclusive with abandoned, set under mu
	finished  chan struct{} // closed, with mu held, when done is set
	lateDone  chan struct{} // closed when the Read in flight at abandonment returned
	late      []byte
	lateErr   error
}

func (sn *sniffer) collect() {
	tmp := make([]byte, sniffLimit)
	have := 0 // only this goroutine appends to got, so it can size the read without the lock
	for {
		n, err := sn.orig.Read(tmp[:sniffLimit-have])
		sn.mu.Lock()
		if sn.abandoned {
			sn.late, sn.lateErr = append([]byte(nil), tmp[:n]...), err
			sn.mu.Unlock()
			close(sn.lateDone)
			return
		}
		sn.got = append(sn.got, tmp[:n]...)
		have = len(sn.got)
		if err != nil || len(sn.got) >= sniffLimit || looksLikeHTTPToHTTPSNotice(sn.got) {
			sn.err = err
			sn.done = true
			if sniffDecidedHook != nil {
				sniffDecidedHook()
			}
			close(sn.finished)
			sn.mu.Unlock()
			return
		}
		sn.mu.Unlock()
	}
}

// peekBody replays the head collected by sniffPlainHTTPToTLS, then whatever the abandoned
// Read returns, then continues with the original body.
type peekBody struct {
	sn     *sniffer
	prefix []byte
	err    error // error to return once prefix is drained (finished sniff)
	ready  bool  // the sniff finished: no Read in flight
}

func (p *peekBody) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	if p.ready {
		if p.err != nil {
			return 0, p.err
		}
		return p.sn.orig.Read(b)
	}
	// Abandoned: the next bytes are whatever the in-flight Read gets, as soon as it does.
	<-p.sn.lateDone
	p.ready = true
	p.prefix, p.err = p.sn.late, p.sn.lateErr
	return p.Read(b)
}

func (p *peekBody) Close() error { return p.sn.orig.Close() }

func plainNoticeContentType(ct string) bool {
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "text/plain" || mt == "text/html")
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
