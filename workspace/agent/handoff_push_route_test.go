package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/memberorigin/memberorigintest"
)

// The handoff push is the CP's call on the owner's click, never a session's: in the real route
// table AGENT_TOKEN and the relay hint are refused, and so are a mark signed with another key and
// a replayed one; a mark the CP made gets through to the handler (#2072, ADR 0113).
func TestHandoffPushRouteRequiresTheMark(t *testing.T) {
	forger := memberorigintest.Install(t) // a key pair the Agent no longer trusts once s is installed
	s := memberorigintest.Install(t)
	mux := buildMux()
	const path = "/sessions/reviewpush/handoff-push"
	body := []byte(`{}`)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer agent-token")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-AF-Relay", "cp")
		return r
	}
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	if w := serve(req()); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), memberorigin.CodeRequired) {
		t.Fatalf("unmarked: %d %s", w.Code, w.Body)
	}
	forged := req()
	forged.Header.Set(memberorigin.Header, forger.Mark(memberorigintest.Claims(forged, body)))
	if w := serve(forged); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), memberorigin.CodeInvalid) {
		t.Fatalf("forged: %d %s", w.Code, w.Body)
	}
	signed := req()
	mark := s.Mark(memberorigintest.Claims(signed, body))
	signed.Header.Set(memberorigin.Header, mark)
	// Past the mark, the handler's own validation answers: an empty snapshot is a bad request.
	if w := serve(signed); w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "member_origin") {
		t.Fatalf("marked: %d %s", w.Code, w.Body)
	}
	replay := req()
	replay.Header.Set(memberorigin.Header, mark)
	if w := serve(replay); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "replayed") {
		t.Fatalf("replayed: %d %s", w.Code, w.Body)
	}
}
