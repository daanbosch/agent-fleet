package memberorigin_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin/memberorigintest"
)

// echo answers 200 with the body it was handed and the subject of the verified mark.
func echo() http.Handler {
	return memberorigin.Require(1<<10, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cl, ok := memberorigin.FromContext(r.Context())
		if !ok {
			http.Error(w, "no claims", http.StatusInternalServerError)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte(cl.Subject + "|" + string(b)))
	}))
}

const target = "/agents/memory/entries/pin?project=app-0123456789ab"

func newReq(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer agent-token")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	echo().ServeHTTP(w, r)
	return w
}

func TestValidMarkIsAccepted(t *testing.T) {
	s := memberorigintest.Install(t)
	r := newReq(`{"name":"x"}`)
	s.Sign(r, []byte(`{"name":"x"}`))
	if w := serve(r); w.Code != http.StatusOK || w.Body.String() != `id-test-member|{"name":"x"}` {
		t.Fatalf("valid mark: %d %s", w.Code, w.Body)
	}
}

// AGENT_TOKEN alone, and the relay hint any caller can set, are not a member.
func TestTokenAndRelayHintAreNotAMark(t *testing.T) {
	memberorigintest.Install(t)
	r := newReq(`{}`)
	r.Header.Set("X-AF-Relay", "cp")
	if w := serve(r); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), memberorigin.CodeRequired) {
		t.Fatalf("token + relay hint: %d %s", w.Code, w.Body)
	}
}

func TestTamperedMarksAreRefused(t *testing.T) {
	s := memberorigintest.Install(t)
	body := []byte(`{"name":"x"}`)
	other, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = other
	cases := map[string]func(*http.Request) string{
		"path": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.Path = "/agents/memory/entries/revert"
			return s.Mark(cl)
		},
		"query": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.Query = "project=other-0123456789ab"
			return s.Mark(cl)
		},
		"body": func(r *http.Request) string { return s.Mark(memberorigintest.Claims(r, []byte(`{"name":"y"}`))) },
		"method": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.Method = http.MethodPut
			return s.Mark(cl)
		},
		"workspace": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.Workspace = "ws-someone-else"
			return s.Mark(cl)
		},
		"membership": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.Membership = "mb-someone-else"
			return s.Mark(cl)
		},
		"expired": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.IssuedAt, cl.Expires = cl.IssuedAt-120, cl.IssuedAt-60
			return s.Mark(cl)
		},
		"too long-lived": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.Expires = cl.IssuedAt + int64(memberorigin.MaxLifetime/time.Second) + 1
			return s.Mark(cl)
		},
		"from the future": func(r *http.Request) string {
			cl := memberorigintest.Claims(r, body)
			cl.IssuedAt, cl.Expires = cl.IssuedAt+300, cl.IssuedAt+360
			return s.Mark(cl)
		},
		"another key": func(r *http.Request) string {
			return (&memberorigintest.Signer{Priv: otherPriv, KID: memberorigintest.KID}).Mark(memberorigintest.Claims(r, body))
		},
		"unknown kid": func(r *http.Request) string {
			return (&memberorigintest.Signer{Priv: s.Priv, KID: "v9"}).Mark(memberorigintest.Claims(r, body))
		},
		"payload swapped": func(r *http.Request) string {
			good := strings.Split(s.Mark(memberorigintest.Claims(r, body)), ".")
			cl := memberorigintest.Claims(r, body)
			cl.Subject = "someone"
			forged := strings.Split(s.Mark(cl), ".")
			return good[0] + "." + forged[1] + "." + good[2]
		},
		"garbage": func(*http.Request) string { return "v1.???." + base64.RawURLEncoding.EncodeToString([]byte("x")) },
	}
	for name, mark := range cases {
		r := newReq(string(body))
		r.Header.Set(memberorigin.Header, mark(r))
		if w := serve(r); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), memberorigin.CodeInvalid) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestReplayIsRefused(t *testing.T) {
	s := memberorigintest.Install(t)
	body := []byte(`{}`)
	r := newReq(string(body))
	s.Sign(r, body)
	mark := r.Header.Get(memberorigin.Header)
	if w := serve(r); w.Code != http.StatusOK {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	again := newReq(string(body))
	again.Header.Set(memberorigin.Header, mark)
	if w := serve(again); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "replayed") {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
}

// A workspace started without the CP's keys refuses: the feature is off, not open.
func TestNoKeysIsUnavailable(t *testing.T) {
	s := memberorigintest.Install(t)
	t.Setenv(memberorigin.EnvKeys, "")
	memberorigin.Reload()
	r := newReq(`{}`)
	s.Sign(r, []byte(`{}`))
	if w := serve(r); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), memberorigin.CodeUnavailable) {
		t.Fatalf("no keys: %d %s", w.Code, w.Body)
	}
}

// Two keys are valid at once, so a CP that moves to the next key keeps running workspaces working.
func TestSecondKeyVerifies(t *testing.T) {
	s := memberorigintest.Install(t)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	pub1 := s.Priv.Public().(ed25519.PublicKey)
	t.Setenv(memberorigin.EnvKeys, "v1:"+base64.RawURLEncoding.EncodeToString(pub1)+",v2:"+base64.RawURLEncoding.EncodeToString(pub2))
	memberorigin.Reload()
	for _, sg := range []*memberorigintest.Signer{s, {Priv: priv2, KID: "v2"}} {
		r := newReq(`{}`)
		sg.Sign(r, []byte(`{}`))
		if w := serve(r); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", sg.KID, w.Code, w.Body)
		}
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	s := memberorigintest.Install(t)
	body := strings.Repeat("x", 2<<10)
	r := newReq(body)
	s.Sign(r, []byte(body))
	if w := serve(r); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d %s", w.Code, w.Body)
	}
}

// The wire format is shared with control-plane/member_origin.go, a separate module. This vector is
// the CP's TestMemberOriginWireVector output for a fixed seed; if either side changes the
// encoding, one of the two tests goes red.
func TestCPWireVectorVerifies(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	t.Setenv(memberorigin.EnvKeys, "v1:"+base64.RawURLEncoding.EncodeToString(pub))
	t.Setenv(memberorigin.EnvWorkspace, "ws-vector")
	t.Setenv(memberorigin.EnvMembership, "mb-vector")
	memberorigin.Reload()
	t.Cleanup(memberorigin.Reload)
	old := memberorigin.SetStartedAt(1699999000)
	t.Cleanup(func() { memberorigin.SetStartedAt(old) })

	r := httptest.NewRequest(http.MethodPost, "/agents/memory/entries/pin?project=p-0123456789ab", strings.NewReader(`{"name":"n"}`))
	r.Header.Set(memberorigin.Header, cpWireVector)
	_, err := memberorigin.VerifyAt(r, []byte(`{"name":"n"}`), time.Unix(1700000010, 0))
	if err != nil {
		t.Fatalf("CP vector: %v", err)
	}
}

// The nonce cache is this process's alone. After a restart, a mark the previous process accepted is
// refused whatever the clocks did: restarted in the same second it was used, or signed by a CP
// whose clock runs ahead within the accepted skew. A mark signed after the fence is accepted.
func TestReplayAcrossRestartIsRefused(t *testing.T) {
	s := memberorigintest.Install(t)
	body := []byte(`{}`)
	const t0 = 1700000000
	signAt := func(iat int64) string {
		r := newReq(string(body))
		cl := memberorigintest.Claims(r, body)
		cl.IssuedAt, cl.Expires = iat, iat+60
		return s.Mark(cl)
	}
	verify := func(mark string, at time.Time) error {
		r := newReq(string(body))
		r.Header.Set(memberorigin.Header, mark)
		_, err := memberorigin.VerifyAt(r, body, at)
		return err
	}
	t.Cleanup(func() { memberorigin.SetStartedAt(time.Now().Unix() - 120) })
	for name, c := range map[string]struct {
		iat       int64
		used      time.Time
		restartAt int64
	}{
		"restart in the same second":       {t0, time.Unix(t0, 0), t0},
		"CP clock 20 s ahead":              {t0 + 20, time.Unix(t0, 0), t0 + 5},
		"CP clock 30 s ahead, used late":   {t0 + 30, time.Unix(t0, 900e6), t0},
		"restart one second after its use": {t0, time.Unix(t0, 0), t0 + 1},
	} {
		memberorigin.RestartForTest(t0 - 300)
		mark := signAt(c.iat)
		if err := verify(mark, c.used); err != nil {
			t.Fatalf("%s: first use: %v", name, err)
		}
		memberorigin.RestartForTest(c.restartAt)
		if err := verify(mark, c.used.Add(10*time.Second)); err == nil {
			t.Errorf("%s: replayed after the restart", name)
		}
	}
	// After the fence, a fresh mark is accepted.
	memberorigin.RestartForTest(t0)
	if err := verify(signAt(t0+31), time.Unix(t0+31, 0)); err != nil {
		t.Fatalf("a mark after the fence: %v", err)
	}
}

// A valid mark on a body that is not JSON is refused: the CP never signs one.
func TestNonJSONBodyIsRefused(t *testing.T) {
	s := memberorigintest.Install(t)
	r := newReq(`{}`)
	r.Header.Set("Content-Type", "text/plain")
	s.Sign(r, []byte(`{}`))
	if w := serve(r); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d %s", w.Code, w.Body)
	}
}

// An upload is hashed as it is spooled: the mark must cover exactly those bytes, the handler reads
// them back, an oversize body is refused, and no spool file outlives the request.
func TestRequireUploadVerifiesTheBodyHash(t *testing.T) {
	dir := t.TempDir()
	h := memberorigin.RequireUpload(1<<10, func() (string, error) { return dir, nil },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_, _ = w.Write(b)
		}))
	s := memberorigintest.Install(t)
	do := func(signed, sent string, ct string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/agents/memory/import", strings.NewReader(sent))
		r.Header.Set("Content-Type", ct)
		s.Sign(r, []byte(signed))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	mp := "multipart/form-data; boundary=B"
	if w := do("abc", "abc", mp); w.Code != http.StatusOK || w.Body.String() != "abc" {
		t.Fatalf("good upload: %d %q", w.Code, w.Body)
	}
	if w := do("abc", "abd", mp); w.Code != http.StatusForbidden {
		t.Fatalf("tampered upload: %d", w.Code)
	}
	if w := do("abc", "abc", "application/json"); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type: %d", w.Code)
	}
	big := strings.Repeat("x", 2<<10)
	if w := do(big, big, mp); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: %d", w.Code)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("%d spool files left behind", len(left))
	}
}
