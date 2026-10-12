package main

// The two tests of the memory family that stay in package main.
//
// The family itself lives in internal/memoryx, but moving these two would leave them measuring
// nothing: neither touches an unexported memoryx symbol (so keeping them here demands no extra
// exports), and what they check can only be checked against the real route table — that the
// ten memory routes do not swallow the existing pattern route `/agents/{kind}/models`. The mux
// memoryx builds for itself (internal/memoryx/mux_test.go) has no `{kind}` route, so the claim
// would disappear on the way over.
//
// The other side — that memoryx's test mux has not drifted from routes.go — is covered by
// TestMemoryRoutesMatchAgentRouteTable in internal/memoryx/mux_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin/memberorigintest"
)

func TestMemoryRoutesRegistered(t *testing.T) {
	mux := buildMux()
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/agents/memory/roots", "GET /agents/memory/roots"},
		{"GET", "/agents/memory/snapshots", "GET /agents/memory/snapshots"},
		{"POST", "/agents/memory/snapshots", "POST /agents/memory/snapshots"},
		{"GET", "/agents/memory/diff", "GET /agents/memory/diff"},
		{"GET", "/agents/memory/tree", "GET /agents/memory/tree"},
		{"POST", "/agents/memory/restore", "POST /agents/memory/restore"},
		{"PUT", "/agents/memory/settings", "PUT /agents/memory/settings"},
		{"GET", "/agents/memory/export", "GET /agents/memory/export"},
		{"POST", "/agents/memory/import", "POST /agents/memory/import"},
		{"POST", "/agents/memory/import/apply", "POST /agents/memory/import/apply"},
		// The existing pattern route still coexists: it is not swallowed by {kind}.
		{"GET", "/agents/codex/models", "GET /agents/{kind}/models"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		if _, pattern := mux.Handler(req); pattern != c.want {
			t.Errorf("%s %s resolved to %q, want %q", c.method, c.path, pattern, c.want)
		}
	}
}

func TestMemoryP2RoutesRegistered(t *testing.T) {
	mux := buildMux()
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/agents/memory/tree", "GET /agents/memory/tree"},
		{"POST", "/agents/memory/restore", "POST /agents/memory/restore"},
		{"PUT", "/agents/memory/settings", "PUT /agents/memory/settings"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		if _, pattern := mux.Handler(req); pattern != c.want {
			t.Errorf("%s %s resolved to %q, want %q", c.method, c.path, pattern, c.want)
		}
	}
}

// The member writes are wrapped in the real route table, not only in memoryx's copy: AGENT_TOKEN
// and the relay hint alone are refused before any handler runs (ADR 0113).
func TestMemberWriteRoutesRequireTheMark(t *testing.T) {
	memberorigintest.Install(t)
	mux := buildMux()
	for _, p := range []string{
		"/agents/memory/entries/pin",
		"/agents/memory/entries/revert",
		"/agents/memory/entries/member-save",
	} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer agent-token")
		req.Header.Set("X-AF-Relay", "cp")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), memberorigin.CodeRequired) {
			t.Errorf("%s without a mark: %d %s", p, w.Code, w.Body)
		}
	}
}
