package main

// member_origin.go — the mark the CP puts on a member's Console request it relays to the Agent,
// so the Agent can tell it from a session (ADR 0113).
//
// AGENT_TOKEN cannot: every session's shell holds it, and X-AF-Relay is a hint any caller can set.
// A shared secret delivered to the Agent would not do either: sessions run as the Agent's uid and
// can read whatever it reads. So the mark is asymmetric. The CP signs with an Ed25519 key derived
// from the token-signing master, which never leaves the CP; the Agent is given only the public
// keys, in its start env, and a session that reads them can verify but not sign.
//
// The signature covers the method, the Agent-side path, the raw query, the body hash, the
// workspace, the membership, the identity, a 60 s expiry and a nonce the Agent remembers until
// the expiry. Only memberOriginRoutes are signed; every other relayed request has the header
// removed.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	memberOriginHeader = "X-AF-Member-Origin"
	// memberOriginTTL is how long a mark is good for. The relay sends it at once; the margin is
	// for a slow Agent and clock skew, and the Agent keeps each nonce this long.
	memberOriginTTL = 60 * time.Second
	// memberOriginMaxBody bounds what the CP buffers to hash. It covers the Agent's own save
	// limit (agentMemSaveMaxRequest, about 1.3 MiB); a larger body is refused here.
	memberOriginMaxBody = 2 << 20
	// memberOriginKeyVersionEnv selects the signing key. Raising it by one rotates: workspaces
	// were started with this version's public key and the next one's, so the running ones keep
	// verifying, and the old private key is never used again.
	memberOriginKeyVersionEnv = "AF_MEMBER_ORIGIN_KEY_VERSION"
)

// memberOriginRoutes are the relayed routes the CP signs: the member-only memory writes. A route
// that is not here never carries a mark, so adding a member route to the Agent without adding it
// here leaves it refused rather than open.
var memberOriginRoutes = map[string]bool{
	"POST /api/agents/memory/entries/pin":         true,
	"POST /api/agents/memory/entries/revert":      true,
	"POST /api/agents/memory/entries/member-save": true,
	// The 0022 history routes only the Console calls (ADR 0113, 2026-10-13 note): a manual
	// snapshot and the apply step of a bundle import.
	"POST /api/agents/memory/snapshots":    true,
	"POST /api/agents/memory/import/apply": true,
}

func memberOriginRoute(r *http.Request) bool { return memberOriginRoutes[r.Method+" "+r.URL.Path] }

// memberOriginClaims is what is signed; the Agent's memberorigin.Claims is the same shape.
type memberOriginClaims struct {
	Method     string `json:"m"`
	Path       string `json:"p"`
	Query      string `json:"q"`
	BodySHA256 string `json:"b"`
	Workspace  string `json:"ws"`
	Membership string `json:"mb"`
	Subject    string `json:"sub"`
	IssuedAt   int64  `json:"iat"`
	Expires    int64  `json:"exp"`
	Nonce      string `json:"n"`
}

// memberOriginKeyVersion is the signing key's version; 0 turns signing off (an unparsable value
// does too, and says so once), and the Agent then refuses the member routes.
func memberOriginKeyVersion() int {
	raw := strings.TrimSpace(os.Getenv(memberOriginKeyVersionEnv))
	if raw == "" {
		return 1
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		log.Printf("%s=%q is not a key version; member-origin signing is off", memberOriginKeyVersionEnv, raw)
		return 0
	}
	return v
}

func memberOriginKID(v int) string { return "v" + strconv.Itoa(v) }

// memberOriginKey derives version v's key pair. A leaked private key of one version says nothing
// about the next: each is its own HMAC of the master.
func memberOriginKey(master32 []byte, v int) ed25519.PrivateKey {
	mac := hmac.New(sha256.New, master32)
	mac.Write([]byte("af-member-origin-sign/" + memberOriginKID(v)))
	return ed25519.NewKeyFromSeed(mac.Sum(nil))
}

// memberOriginEnv is what a workspace is started with: the current and the next public key, its
// own workspace id and its membership id. Nothing secret.
func memberOriginEnv(master32 []byte, v int, wsID, membershipID string) []string {
	if v <= 0 || wsID == "" || membershipID == "" {
		return nil
	}
	pub := func(v int) string {
		return memberOriginKID(v) + ":" +
			base64.RawURLEncoding.EncodeToString(memberOriginKey(master32, v).Public().(ed25519.PublicKey))
	}
	return []string{
		"AF_MEMBER_ORIGIN_KEYS=" + pub(v) + "," + pub(v+1),
		"AF_WORKSPACE_ID=" + wsID,
		"AF_MEMBERSHIP_ID=" + membershipID,
	}
}

// signMemberOrigin encodes and signs claims into the header value.
func signMemberOrigin(priv ed25519.PrivateKey, kid string, c memberOriginClaims) string {
	payload, _ := json.Marshal(c)
	sig := ed25519.Sign(priv, append([]byte("af-member-origin/v1\n"), payload...))
	return kid + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// memberOriginMark reads the body of a relayed member route and returns it with the mark for it.
// The workspace must be the caller's own: the store it writes is that member's memory.
func (m *manager) memberOriginMark(r *http.Request, res *resolved, now time.Time) ([]byte, string, *apiError) {
	if res.ws.MembershipID == "" || res.ws.MembershipID != res.mv.MembershipID {
		return nil, "", &apiError{http.StatusForbidden, "forbidden", "only the workspace's own member can change its memory"}
	}
	if aerr := memberOriginRequestCheck(r); aerr != nil {
		return nil, "", aerr
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, memberOriginMaxBody+1))
	if err != nil {
		return nil, "", &apiError{http.StatusBadRequest, "bad_request", "could not read the request body"}
	}
	if len(body) > memberOriginMaxBody {
		return nil, "", &apiError{http.StatusRequestEntityTooLarge, "too_large", "request body is too large"}
	}
	mark, aerr := m.memberOriginSign(res, r.Method, strings.TrimPrefix(r.URL.Path, "/api"), r.URL.RawQuery, body, now)
	if aerr != nil {
		return nil, "", aerr
	}
	return body, mark, nil
}

// memberOriginSign marks one request to res's Agent: method, Agent-side path, raw query and the
// exact body. "" with no error means signing is off, and the Agent then refuses the route. The
// relay above uses it for a member's Console request; a CP handler uses it directly for an Agent
// call it makes on the member's own request (the handoff push), after checking that the workspace
// is the caller's own.
func (m *manager) memberOriginSign(res *resolved, method, path, query string, body []byte, now time.Time) (string, *apiError) {
	v := memberOriginKeyVersion()
	if v <= 0 {
		return "", nil
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", &apiError{http.StatusInternalServerError, "internal", "could not sign the request"}
	}
	sum := sha256.Sum256(body)
	c := memberOriginClaims{
		Method: method, Path: path, Query: query,
		BodySHA256: hex.EncodeToString(sum[:]), Workspace: res.ws.ID, Membership: res.ws.MembershipID,
		Subject: res.ident.ID, IssuedAt: now.Unix(), Expires: now.Add(memberOriginTTL).Unix(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	}
	return signMemberOrigin(memberOriginKey(m.tokenSignMaster(), v), memberOriginKID(v), c), nil
}

// relayBody is the body and mark a relayed request goes out with: the member routes are buffered
// and signed, everything else streams through untouched and unmarked.
func (m *manager) relayBody(r *http.Request, res *resolved) (io.Reader, string, *apiError) {
	if !memberOriginRoute(r) {
		return r.Body, "", nil
	}
	body, mark, aerr := m.memberOriginMark(r, res, time.Now())
	if aerr != nil {
		return nil, "", aerr
	}
	return bytes.NewReader(body), mark, nil
}

// stripAgentRelayCredentials removes from a relayed request what the browser sent that the Agent
// has no use for and a workspace must not see: the Console's and a front gateway's login cookies
// and identity headers (preview's stripConsoleCredentials), every other cookie (no Agent route
// reads one), and any member-origin mark. A session that obtained the member's login cookie could
// otherwise have the CP sign whatever it sends. The caller sets the Agent bearer afterwards.
func stripAgentRelayCredentials(out, in *http.Request, identityHeader string) {
	stripConsoleCredentials(out, in, identityHeader)
	out.Header.Del("Cookie")
}

// memberOriginRequestCheck refuses, before anything is signed, a request a page other than the
// Console could have made with the member's cookie. A preview app on its own host is same-site, so
// SameSite=Lax still sends the cookie; a text/plain no-cors POST from it needs no preflight. So:
// the body must be JSON (a cross-origin page cannot send that without a preflight, and the CP
// answers none), Sec-Fetch-Site, when the browser sends it, must be same-origin, and Origin, when
// sent, must be the Console's own host. A caller with neither header is not a browser; it needs the
// member's login cookie, which the relays keep out of the workspace.
func memberOriginRequestCheck(r *http.Request) *apiError {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return &apiError{http.StatusUnsupportedMediaType, "unsupported_media_type", "member writes take application/json"}
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" {
		return &apiError{http.StatusForbidden, "forbidden", "member writes are accepted from the Console's own origin only"}
	}
	if r.Header.Get("Origin") != "" && !checkWSOrigin(r) {
		return &apiError{http.StatusForbidden, "forbidden", "member writes are accepted from the Console's own origin only"}
	}
	return nil
}
