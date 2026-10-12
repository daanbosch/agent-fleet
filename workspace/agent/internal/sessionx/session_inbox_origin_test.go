package sessionx

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/testguard"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/tmuxx"
)

// TestCreateSessionKeepsInboxOrigin: the inbox provenance the Control Plane stamps on a create
// (ADR 0111 decision 3) is kept in the meta and comes back on the session, for the Console's
// badge; a create without one has none.
func TestCreateSessionKeepsInboxOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	testguard.IsolateTmux(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AF_SESSIONS_DIR", filepath.Join(home, "sessions"))
	parent := filepath.Join(home, "repos", "app")
	gitInit(t, parent)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", HandleCreateSession)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var with, without session.Session
	do(t, srv, "POST", "/sessions", map[string]any{"dir": parent, "kind": "shell",
		"inbox_origin": map[string]any{"sender": "a-example-com", "item": "item-1"}}, http.StatusCreated, &with)
	defer tmuxx.Cmd("kill-session", "-t", session.TmuxName(with.Name)).Run()
	do(t, srv, "POST", "/sessions", map[string]any{"dir": parent, "kind": "shell"}, http.StatusCreated, &without)
	defer tmuxx.Cmd("kill-session", "-t", session.TmuxName(without.Name)).Run()
	if with.InboxOrigin == nil || with.InboxOrigin.Item != "item-1" || with.InboxOrigin.Sender != "a-example-com" {
		t.Fatalf("with an origin: %+v", with.InboxOrigin)
	}
	if without.InboxOrigin != nil {
		t.Fatalf("without an origin: %+v", without.InboxOrigin)
	}
	m, ok := session.ReadMeta(with.Name)
	if !ok || m.InboxOrigin == nil || m.InboxOrigin.Item != "item-1" {
		t.Fatalf("meta: %+v %v", m.InboxOrigin, ok)
	}
}

func TestCleanInboxOrigin(t *testing.T) {
	if cleanInboxOrigin(nil) != nil || cleanInboxOrigin(&session.InboxOriginRef{Sender: "a"}) != nil {
		t.Fatal("an origin without an item was kept")
	}
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	if cleanInboxOrigin(&session.InboxOriginRef{Item: string(long)}) != nil {
		t.Fatal("an unbounded item was kept")
	}
	if o := cleanInboxOrigin(&session.InboxOriginRef{Sender: " a ", Item: " i "}); o == nil || o.Item != "i" || o.Sender != "a" {
		t.Fatalf("clean: %+v", o)
	}
}
