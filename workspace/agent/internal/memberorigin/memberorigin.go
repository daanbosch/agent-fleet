// Package memberorigin verifies the mark the Control Plane puts on a member's Console request it
// relays (ADR 0113). AGENT_TOKEN cannot tell the two apart: every session's shell holds it, and
// X-AF-Relay is a hint any caller can set. The mark is an Ed25519 signature made with a private
// key only the CP holds, over the method, path, query, body hash, workspace, membership, a short
// expiry and a nonce. The Agent holds the public keys alone, so a session that reads everything
// the Agent can read still cannot make one.
//
// What it does not do: make the store tamper-proof. A session runs as the Agent's uid and can
// write the store's files directly; the mark only decides what the Agent's own routes accept.
package memberorigin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
)

// Header carries the mark. The CP strips whatever a browser sent under this name before it signs.
const Header = "X-AF-Member-Origin"

// The env the CP injects at container start (control-plane/member_origin.go). The keys are public;
// a session reading them learns nothing it can sign with.
const (
	EnvKeys       = "AF_MEMBER_ORIGIN_KEYS" // "<kid>:<base64url public key>[,…]", current and next
	EnvWorkspace  = "AF_WORKSPACE_ID"
	EnvMembership = "AF_MEMBERSHIP_ID"
)

// signPrefix separates this signature from any other the same key could ever make.
const signPrefix = "af-member-origin/v1\n"

const (
	// MaxLifetime bounds exp-iat; the CP signs for 60 s. It also bounds how long a nonce is kept.
	MaxLifetime = 2 * time.Minute
	// clockSkew is how far ahead of the Agent's clock the CP's iat may be.
	clockSkew = 30 * time.Second
	// replayCap bounds the nonce cache. Past it, with nothing expired to drop, a mark is refused
	// rather than accepted unrecorded: a full cache must never become a replay window.
	replayCap = 4096
)

// Error codes. A refused mark answers 403, a missing configuration 503: the feature is off, not open.
const (
	CodeRequired    = "member_origin_required"
	CodeInvalid     = "member_origin_invalid"
	CodeUnavailable = "member_origin_unavailable"
)

// Claims is what the CP signed. Field names are short because the header travels on every member
// write; the CP's memberOriginClaims is the same shape.
type Claims struct {
	Method     string `json:"m"`
	Path       string `json:"p"`
	Query      string `json:"q"`
	BodySHA256 string `json:"b"`
	Workspace  string `json:"ws"`
	Membership string `json:"mb"`
	Subject    string `json:"sub"` // the CP identity that made the request
	IssuedAt   int64  `json:"iat"`
	Expires    int64  `json:"exp"`
	Nonce      string `json:"n"`
}

type config struct {
	keys       map[string]ed25519.PublicKey
	workspace  string
	membership string
}

var (
	mu     sync.Mutex
	cfg    *config
	nonces = map[string]int64{} // nonce -> exp (unix)
	// startedAt fences replay across a restart: the nonce cache lives in this process only, so a
	// mark issued before the process started is refused whatever the cache says. The comparison
	// is in whole seconds; a capture, a restart and a replay inside one second is not a case.
	startedAt = time.Now().Unix()
)

// Reload re-reads the env. The Agent reads it once on first use; tests change it.
func Reload() {
	mu.Lock()
	defer mu.Unlock()
	cfg = nil
	nonces = map[string]int64{}
}

func loadLocked() *config {
	if cfg != nil {
		return cfg
	}
	c := &config{keys: map[string]ed25519.PublicKey{},
		workspace: strings.TrimSpace(os.Getenv(EnvWorkspace)), membership: strings.TrimSpace(os.Getenv(EnvMembership))}
	for _, item := range strings.Split(os.Getenv(EnvKeys), ",") {
		kid, enc, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok || kid == "" {
			continue
		}
		pub, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			log.Printf("memberorigin: ignoring a malformed public key %q", kid)
			continue
		}
		c.keys[kid] = ed25519.PublicKey(pub)
	}
	cfg = c
	return c
}

// Error is a refusal with the HTTP status and code it answers with.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func invalid(msg string) error {
	return &Error{Status: http.StatusForbidden, Code: CodeInvalid, Msg: "member origin mark refused: " + msg}
}

// Verify checks the request's mark against its method, path, query and body, and records the
// nonce. body is the request body exactly as it will be read.
func Verify(r *http.Request, body []byte) (Claims, error) { return VerifyAt(r, body, time.Now()) }

// VerifyAt is Verify at a given time.
func VerifyAt(r *http.Request, body []byte, t time.Time) (Claims, error) {
	h := strings.TrimSpace(r.Header.Get(Header))
	mu.Lock()
	defer mu.Unlock()
	c := loadLocked()
	if len(c.keys) == 0 || c.workspace == "" || c.membership == "" {
		return Claims{}, &Error{Status: http.StatusServiceUnavailable, Code: CodeUnavailable,
			Msg: "this workspace was started without the Control Plane's member-origin keys; restart it from the Console"}
	}
	if h == "" {
		return Claims{}, &Error{Status: http.StatusForbidden, Code: CodeRequired,
			Msg: "this route accepts only a member's request relayed by the Control Plane"}
	}
	parts := strings.Split(h, ".")
	if len(parts) != 3 {
		return Claims{}, invalid("malformed")
	}
	key, ok := c.keys[parts[0]]
	if !ok {
		return Claims{}, invalid("unknown key")
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return Claims{}, invalid("malformed")
	}
	if !ed25519.Verify(key, append([]byte(signPrefix), payload...), sig) {
		return Claims{}, invalid("bad signature")
	}
	var cl Claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cl); err != nil {
		return Claims{}, invalid("malformed claims")
	}
	sum := sha256.Sum256(body)
	switch {
	case cl.Method != r.Method:
		return Claims{}, invalid("method mismatch")
	case cl.Path != r.URL.Path:
		return Claims{}, invalid("path mismatch")
	case cl.Query != r.URL.RawQuery:
		return Claims{}, invalid("query mismatch")
	case cl.BodySHA256 != hex.EncodeToString(sum[:]):
		return Claims{}, invalid("body mismatch")
	case cl.Workspace != c.workspace:
		return Claims{}, invalid("signed for another workspace")
	case cl.Membership != c.membership:
		return Claims{}, invalid("signed for another member")
	case cl.Subject == "" || len(cl.Nonce) < 16:
		return Claims{}, invalid("missing subject or nonce")
	}
	switch {
	case cl.Expires <= cl.IssuedAt || time.Duration(cl.Expires-cl.IssuedAt)*time.Second > MaxLifetime:
		return Claims{}, invalid("lifetime out of range")
	case time.Unix(cl.IssuedAt, 0).After(t.Add(clockSkew)):
		return Claims{}, invalid("issued in the future")
	case !t.Before(time.Unix(cl.Expires, 0)):
		return Claims{}, invalid("expired")
	case cl.IssuedAt < startedAt:
		return Claims{}, invalid("issued before this Agent started")
	}
	if _, seen := nonces[cl.Nonce]; seen {
		return Claims{}, invalid("replayed")
	}
	if len(nonces) >= replayCap {
		for n, exp := range nonces {
			if exp <= t.Unix() {
				delete(nonces, n)
			}
		}
		if len(nonces) >= replayCap {
			return Claims{}, &Error{Status: http.StatusServiceUnavailable, Code: CodeUnavailable,
				Msg: "too many member requests in flight; try again in a minute"}
		}
	}
	nonces[cl.Nonce] = cl.Expires
	return cl, nil
}

type ctxKey struct{}

// FromContext is the verified mark Require put on the request; ok is false when there is none.
func FromContext(ctx context.Context) (Claims, bool) {
	cl, ok := ctx.Value(ctxKey{}).(Claims)
	return cl, ok
}

// Require admits only a request with a valid mark. It reads the body (up to maxBody bytes, which
// must cover the route's own limit), verifies, and hands the handler the same bytes with the
// claims in the context. Handlers still check FromContext, so a route registered without this
// refuses rather than opens.
func Require(maxBody int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			httpx.WriteErr(w, http.StatusBadRequest, "bad_request", "could not read the request body")
			return
		}
		if int64(len(body)) > maxBody {
			httpx.WriteErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
			return
		}
		cl, err := Verify(r, body)
		if err != nil {
			var e *Error
			if errors.As(err, &e) {
				httpx.WriteErr(w, e.Status, e.Code, e.Msg)
			} else {
				httpx.WriteErr(w, http.StatusForbidden, CodeInvalid, err.Error())
			}
			return
		}
		// The CP signs JSON only; a text/plain body is what a cross-origin page sends without a
		// preflight, so it is refused here as well.
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			httpx.WriteErr(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "member writes take application/json")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, cl))
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// RefuseUnverified answers 403 and returns false when the request carries no verified mark.
func RefuseUnverified(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := FromContext(r.Context()); ok {
		return true
	}
	httpx.WriteErr(w, http.StatusForbidden, CodeRequired, "this route accepts only a member's request relayed by the Control Plane")
	return false
}
