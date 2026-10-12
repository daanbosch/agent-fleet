// Package memberorigintest stands in for the Control Plane's signer in tests: it installs a fresh
// key pair as the Agent's member-origin configuration and signs requests the way the CP does.
package memberorigintest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin"
)

const (
	Workspace  = "ws-test-0001"
	Membership = "mb-test-0001"
	KID        = "v1"
)

// Signer holds the private key the Agent under test has the public half of.
type Signer struct {
	Priv ed25519.PrivateKey
	KID  string
}

// Install sets the env a CP would inject and returns a signer for it. The configuration is
// dropped again when the test ends.
func Install(t testing.TB) *Signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(memberorigin.EnvKeys, KID+":"+base64.RawURLEncoding.EncodeToString(pub))
	t.Setenv(memberorigin.EnvWorkspace, Workspace)
	t.Setenv(memberorigin.EnvMembership, Membership)
	memberorigin.Reload()
	t.Cleanup(memberorigin.Reload)
	// Marks are signed "now", which is inside the restart fence of a test process just started.
	old := memberorigin.SetStartedAt(time.Now().Unix() - 120)
	t.Cleanup(func() { memberorigin.SetStartedAt(old) })
	return &Signer{Priv: priv, KID: KID}
}

// Claims are the claims the CP would sign for this request and body.
func Claims(r *http.Request, body []byte) memberorigin.Claims {
	sum := sha256.Sum256(body)
	iat := time.Now().Unix()
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	return memberorigin.Claims{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, BodySHA256: hex.EncodeToString(sum[:]),
		Workspace: Workspace, Membership: Membership, Subject: "id-test-member",
		IssuedAt: iat, Expires: iat + 60, Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	}
}

// Mark encodes and signs claims into the header value.
func (s *Signer) Mark(cl memberorigin.Claims) string {
	payload, _ := json.Marshal(cl)
	sig := ed25519.Sign(s.Priv, append([]byte("af-member-origin/v1\n"), payload...))
	return s.KID + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Sign puts a valid mark for body on r.
func (s *Signer) Sign(r *http.Request, body []byte) {
	r.Header.Set(memberorigin.Header, s.Mark(Claims(r, body)))
}
