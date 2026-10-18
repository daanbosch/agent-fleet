package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "tok-shim"

func newTestServer(t *testing.T, registry string) *httptest.Server {
	t.Helper()
	logs := t.TempDir()
	if err := os.WriteFile(filepath.Join(logs, "h-pi-0003.log"), []byte("line1\nline2\nline3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newHandler(config{Token: testToken, RegistryPath: registry, LogsDir: logs}))
	t.Cleanup(srv.Close)
	return srv
}

func request(t *testing.T, srv *httptest.Server, path string, authed bool) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authed {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

func TestHealthzIsOpenAndCountsSessions(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	code, body, _ := request(t, srv, "/healthz", false)
	if code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", code)
	}
	var h struct {
		OK       bool `json:"ok"`
		Sessions int  `json:"sessions"`
		Alive    int  `json:"alive"`
		Headless int  `json:"headless"`
	}
	decodeJSON(t, body, &h)
	if !h.OK || h.Sessions != 4 || h.Alive != 2 || h.Headless != 2 {
		t.Fatalf("healthz = %+v, want ok 4 sessions, 2 alive, 2 headless", h)
	}
}

func TestSessionRoutesRequireBearer(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	for _, path := range []string{"/sessions", "/sessions/h-pi-0003", "/sessions/h-pi-0003/log", "/ws/pty?session=h-pi-0003"} {
		if code, _, _ := request(t, srv, path, false); code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, code)
		}
	}
}

func TestSessionsListsRegistryRowsWithoutPrompts(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	code, body, _ := request(t, srv, "/sessions", true)
	if code != http.StatusOK {
		t.Fatalf("/sessions = %d, want 200", code)
	}
	if strings.Contains(body, "SECRET-PROMPT-MARKER") {
		t.Fatal("prompt text leaked into the session list")
	}
	var list struct {
		Sessions []sessionView `json:"sessions"`
	}
	decodeJSON(t, body, &list)
	if len(list.Sessions) != 4 {
		t.Fatalf("sessions = %d, want 4", len(list.Sessions))
	}
	wantNames := []string{"h-claude-0001", "h-codex-0002", "h-pi-0003", "h-pi-0004"}
	for i, s := range list.Sessions {
		if s.Name != wantNames[i] {
			t.Errorf("sessions[%d].name = %q, want %q (registry order)", i, s.Name, wantNames[i])
		}
		if s.Attachable {
			t.Errorf("sessions[%d] %s is attachable; attach is refused for every row in this slice", i, s.Name)
		}
	}
}

func TestMissingRegistryListsNothing(t *testing.T) {
	srv := newTestServer(t, filepath.Join(t.TempDir(), "none.json"))
	code, body, _ := request(t, srv, "/sessions", true)
	if code != http.StatusOK || !strings.Contains(body, `"sessions":[]`) {
		t.Fatalf("/sessions with no registry = %d %s, want 200 with an empty list", code, body)
	}
}

func TestMalformedRegistryIsServerError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "sessions-v4.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, bad)
	if code, _, _ := request(t, srv, "/sessions", true); code != http.StatusInternalServerError {
		t.Fatalf("/sessions with malformed registry = %d, want 500", code)
	}
}

func TestSessionDetailCarriesLogTailForHeadless(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	code, body, _ := request(t, srv, "/sessions/h-pi-0003", true)
	if code != http.StatusOK {
		t.Fatalf("detail = %d, want 200", code)
	}
	var d sessionDetail
	decodeJSON(t, body, &d)
	if d.Name != "h-pi-0003" || !d.LogAvailable || d.LogTail != "line1\nline2\nline3\n" {
		t.Fatalf("detail = %+v", d)
	}
}

func TestSessionDetailWithoutLogHasNoTail(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	code, body, _ := request(t, srv, "/sessions/h-pi-0004", true)
	if code != http.StatusOK {
		t.Fatalf("detail = %d, want 200", code)
	}
	var d sessionDetail
	decodeJSON(t, body, &d)
	if d.LogAvailable || d.LogTail != "" {
		t.Fatalf("detail without a log file = %+v", d)
	}
}

func TestUnknownSessionDetailIsNotFound(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	if code, _, _ := request(t, srv, "/sessions/h-nope-9999", true); code != http.StatusNotFound {
		t.Fatalf("unknown detail = %d, want 404", code)
	}
}

func TestLogEndpointDropsPartialFirstLine(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	code, body, hdr := request(t, srv, "/sessions/h-pi-0003/log?bytes=8", true)
	if code != http.StatusOK {
		t.Fatalf("log = %d, want 200", code)
	}
	if body != "line3\n" {
		t.Fatalf("log tail = %q, want %q", body, "line3\n")
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
}

func TestLogEndpointRefusesRowsWithoutALog(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	if code, _, _ := request(t, srv, "/sessions/h-claude-0001/log", true); code != http.StatusNotFound {
		t.Fatalf("kitty row log = %d, want 404", code)
	}
	if code, _, _ := request(t, srv, "/sessions/h-pi-0004/log", true); code != http.StatusNotFound {
		t.Fatalf("headless row with no log file = %d, want 404", code)
	}
}

func TestLogEndpointRejectsBadInput(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	for _, path := range []string{
		"/sessions/h-pi-0003/log?bytes=0",
		"/sessions/h-pi-0003/log?bytes=999999999",
		"/sessions/bad%20name/log",
	} {
		if code, _, _ := request(t, srv, path, true); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, code)
		}
	}
}

func TestWsPtyRefusesAttachHonestly(t *testing.T) {
	srv := newTestServer(t, fixtureRegistry)
	code, body, _ := request(t, srv, "/ws/pty?session=h-claude-0001", true)
	if code != http.StatusConflict {
		t.Fatalf("attach = %d, want 409", code)
	}
	if !strings.Contains(body, "log view only") {
		t.Fatalf("attach refusal does not explain itself: %s", body)
	}
	if code, _, _ := request(t, srv, "/ws/pty", true); code != http.StatusBadRequest {
		t.Fatalf("attach without session = %d, want 400", code)
	}
	if code, _, _ := request(t, srv, "/ws/pty?session=h-nope-9999", true); code != http.StatusNotFound {
		t.Fatalf("attach to unknown session = %d, want 404", code)
	}
}
