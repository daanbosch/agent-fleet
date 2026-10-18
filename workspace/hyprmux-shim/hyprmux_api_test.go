package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeCLI stands in for the installed hyprmux. The real CLI writes to the host's live board,
// so tests record each argv and answer from a fixture keyed by the first two arguments.
type fakeCLI struct {
	answers map[string][]byte
	fail    error
	calls   [][]string
}

func (f *fakeCLI) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.fail != nil {
		return nil, f.fail
	}
	joined := strings.Join(args, " ")
	best := ""
	for key := range f.answers {
		if strings.HasPrefix(joined, key) && len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return nil, fmt.Errorf("no fixture for %q", joined)
	}
	return f.answers[best], nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newAPIServer(t *testing.T, cli hyprmuxCLI, checkpoint string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newHandler(config{
		Token: testToken, RegistryPath: fixtureRegistry, LogsDir: t.TempDir(),
		CLI: cli, TelegramCheckpoint: checkpoint,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func apiCall(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func defaultCLI(t *testing.T) *fakeCLI {
	return &fakeCLI{answers: map[string][]byte{
		"status --json":    fixture(t, "hyprmux-status.json"),
		"room item list":   []byte(`{"items":[]}`),
		"room item create": []byte(`{"ok":true,"id":"item-1"}`),
		"session launch":   []byte(`{"ok":true,"sessionId":"s-1"}`),
	}}
}

func TestHyprmuxRoutesRequireBearer(t *testing.T) {
	srv := newAPIServer(t, defaultCLI(t), "")
	for _, route := range []string{"GET /api/hyprmux/status", "GET /api/hyprmux/projects", "GET /api/hyprmux/room-items", "POST /api/hyprmux/room-item", "POST /api/hyprmux/session-launch", "GET /api/telegram/status", "GET /api/telegram/topics", "POST /api/telegram/topic-message"} {
		method, path, _ := strings.Cut(route, " ")
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", route, resp.StatusCode)
		}
	}
}

func TestHyprmuxAPIFailsClosedWithoutCLI(t *testing.T) {
	srv := newAPIServer(t, nil, t.TempDir()+"/cp.json")
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/hyprmux/status", ""},
		{"GET", "/api/hyprmux/projects", ""},
		{"GET", "/api/hyprmux/room-items?project=proj-alpha", ""},
		{"POST", "/api/hyprmux/room-item", `{"project":"proj-alpha","kind":"blocker","title":"t","content":"c"}`},
		{"POST", "/api/hyprmux/session-launch", `{"project":"proj-alpha","agent":"pi","mode":"normal"}`},
		{"GET", "/api/telegram/status", ""},
	} {
		code, body := apiCall(t, srv, c.method, c.path, c.body)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, "not on PATH") {
			t.Errorf("%s %s without CLI = %d %s, want 503", c.method, c.path, code, body)
		}
	}
}

func TestHyprmuxStatusKeepsOnlyHallAndProjects(t *testing.T) {
	srv := newAPIServer(t, defaultCLI(t), "")
	code, body := apiCall(t, srv, "GET", "/api/hyprmux/status", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, body)
	}
	for _, leak := range []string{"SECRET-PROMPT-MARKER", "/work/alpha", "example.invalid"} {
		if strings.Contains(body, leak) {
			t.Errorf("status leaked %q: %s", leak, body)
		}
	}
	var st statusWire
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 2 || st.Projects[0].ID != "proj-alpha" || st.Projects[0].Slug != "alpha" {
		t.Fatalf("status projects = %+v", st.Projects)
	}
}

func TestHyprmuxProjectsListsIdentityOnly(t *testing.T) {
	srv := newAPIServer(t, defaultCLI(t), "")
	code, body := apiCall(t, srv, "GET", "/api/hyprmux/projects", "")
	if code != http.StatusOK || strings.Contains(body, "/work/") {
		t.Fatalf("projects = %d %s", code, body)
	}
	if !strings.Contains(body, `"id":"proj-beta"`) || !strings.Contains(body, `"group":"Work"`) {
		t.Fatalf("projects = %s, want both projects", body)
	}
}

func TestRoomItemsPassesProjectAndRelaysJSON(t *testing.T) {
	cli := defaultCLI(t)
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "GET", "/api/hyprmux/room-items?project=proj-alpha", "")
	if code != http.StatusOK || strings.TrimSpace(body) != `{"items":[]}` {
		t.Fatalf("room-items = %d %s", code, body)
	}
	want := []string{"room", "item", "list", "--project", "proj-alpha", "--json"}
	if got := cli.calls[len(cli.calls)-1]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", got, want)
	}
}

func TestRoomItemsRejectsBadProject(t *testing.T) {
	cli := defaultCLI(t)
	srv := newAPIServer(t, cli, "")
	for _, q := range []string{"", "..%2Fx", "a%20b"} {
		if code, body := apiCall(t, srv, "GET", "/api/hyprmux/room-items?project="+q, ""); code != http.StatusBadRequest {
			t.Errorf("project=%q = %d %s, want 400", q, code, body)
		}
	}
	if len(cli.calls) != 0 {
		t.Fatalf("CLI ran for a bad project: %v", cli.calls)
	}
}

func TestRoomItemCreateUsesStableOperationKey(t *testing.T) {
	cli := defaultCLI(t)
	srv := newAPIServer(t, cli, "")
	body := `{"project":"proj-alpha","kind":"blocker","title":"Disk full","content":"Ceph at 91%"}`
	for range 2 {
		if code, resp := apiCall(t, srv, "POST", "/api/hyprmux/room-item", body); code != http.StatusOK {
			t.Fatalf("room-item = %d %s", code, resp)
		}
	}
	keyOf := func(argv []string) string {
		for i, a := range argv {
			if a == "--operation-key" && i+1 < len(argv) {
				return argv[i+1]
			}
		}
		t.Fatalf("no --operation-key in %v", argv)
		return ""
	}
	first, second := keyOf(cli.calls[0]), keyOf(cli.calls[1])
	if first != second || !strings.HasPrefix(first, "shim-") || len(first) != len("shim-")+40 {
		t.Fatalf("keys = %q, %q; want the same shim-… key on a retry", first, second)
	}
	apiCall(t, srv, "POST", "/api/hyprmux/room-item", `{"project":"proj-alpha","kind":"blocker","title":"Disk full","content":"Ceph at 92%"}`)
	if keyOf(cli.calls[2]) == first {
		t.Fatal("different content reused the operation key")
	}
}

func TestRoomItemCreateRejectsInvalidInputBeforeCLI(t *testing.T) {
	cli := defaultCLI(t)
	srv := newAPIServer(t, cli, "")
	for name, body := range map[string]string{
		"bad kind":      `{"project":"proj-alpha","kind":"Blocker!","title":"t","content":"c"}`,
		"empty title":   `{"project":"proj-alpha","kind":"task","title":"","content":"c"}`,
		"nul in body":   `{"project":"proj-alpha","kind":"task","title":"t","content":"a\u0000b"}`,
		"unknown field": `{"project":"proj-alpha","kind":"task","title":"t","content":"c","extra":1}`,
		"bad project":   `{"project":"a/b","kind":"task","title":"t","content":"c"}`,
	} {
		if code, resp := apiCall(t, srv, "POST", "/api/hyprmux/room-item", body); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, code, resp)
		}
	}
	if len(cli.calls) != 0 {
		t.Fatalf("CLI ran for invalid input: %v", cli.calls)
	}
}

func TestCLIFailureIsBadGatewayWithTheCLIsOwnLine(t *testing.T) {
	cli := defaultCLI(t)
	cli.fail = fmt.Errorf("hyprmux room failed: project not found")
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/api/hyprmux/room-item", `{"project":"proj-alpha","kind":"task","title":"t","content":"c"}`)
	if code != http.StatusBadGateway || !strings.Contains(body, "project not found") {
		t.Fatalf("CLI failure = %d %s, want 502 with the CLI line", code, body)
	}
}

func TestSessionLaunchRunsCLIWithoutPrompt(t *testing.T) {
	cli := defaultCLI(t)
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/api/hyprmux/session-launch", `{"project":"proj-alpha","agent":"pi","mode":"yolo"}`)
	if code != http.StatusOK || strings.TrimSpace(body) != `{"ok":true,"sessionId":"s-1"}` {
		t.Fatalf("session-launch = %d %s", code, body)
	}
	want := "session launch -project proj-alpha -agent pi -mode yolo -json"
	if got := strings.Join(cli.calls[0], " "); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestSessionLaunchRefusesPromptOrHeadlessBeforeCLI(t *testing.T) {
	cli := defaultCLI(t)
	srv := newAPIServer(t, cli, "")
	for _, body := range []string{
		`{"project":"proj-alpha","agent":"pi","mode":"normal","prompt":"do it"}`,
		`{"project":"proj-alpha","agent":"pi","mode":"normal","headless":true}`,
	} {
		if code, resp := apiCall(t, srv, "POST", "/api/hyprmux/session-launch", body); code != http.StatusNotImplemented {
			t.Errorf("%s = %d %s, want 501", body, code, resp)
		}
	}
	if len(cli.calls) != 0 {
		t.Fatalf("CLI ran for a refused launch: %v", cli.calls)
	}
}

func TestSessionLaunchRejectsUnknownAgentAndMode(t *testing.T) {
	srv := newAPIServer(t, defaultCLI(t), "")
	for _, body := range []string{
		`{"project":"proj-alpha","agent":"bash","mode":"normal"}`,
		`{"project":"proj-alpha","agent":"pi","mode":"root"}`,
	} {
		if code, resp := apiCall(t, srv, "POST", "/api/hyprmux/session-launch", body); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", body, code, resp)
		}
	}
}

func TestOperationKeyIsDeterministicAndSeparatesFields(t *testing.T) {
	if operationKey("a", "bc") != operationKey("a", "bc") {
		t.Fatal("same parts gave different keys")
	}
	if operationKey("a", "bc") == operationKey("ab", "c") {
		t.Fatal("field boundary is not part of the key")
	}
}
