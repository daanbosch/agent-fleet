//go:build contract

// The client layer (serveapi.go) against the REAL opencode binary on PATH, whichever major
// it is: the same test runs against the v1 pin and against a v2 install, which is how the
// "both pins" requirement is checked. No model turn is sent.
//
//	go test -tags contract -run ServeAPI ./internal/agents/opencode/
package opencode

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestContractServeAPIBothMajors(t *testing.T) {
	requireOpencode(t)
	wantMajor := apiV1
	if strings.HasPrefix(strings.TrimPrefix(opencodeVersion(t), "opencode v"), "2.") {
		wantMajor = apiV2
	}
	home := t.TempDir()
	port := freePort(t)
	addr := "http://127.0.0.1:" + port
	t.Setenv(serveAddrEnv, addr)
	t.Setenv("HOME", home)
	password, err := newServePassword()
	if err != nil {
		t.Fatal(err)
	}
	detectedMajor.Store(int32(apiV1))
	t.Cleanup(func() { detectedMajor.Store(int32(apiV1)); resetServeAuth("") })

	cmd := exec.Command("opencode", "serve", "--hostname", "127.0.0.1", "--port", port)
	cmd.Env = append(envWithHome(home), "OPENCODE_SERVER_USERNAME="+serveUser, "OPENCODE_SERVER_PASSWORD="+password)
	cmd.Dir = home
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(30 * time.Second)
	for !healthy(addr) {
		if time.Now().After(deadline) {
			t.Fatal("serve never became healthy through the production probe")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if serveMajor() != wantMajor {
		t.Fatalf("detected major %d, binary is %s", serveMajor(), opencodeVersion(t))
	}

	// Every API request needs the password. (v2 serves the web UI's static HTML for v1
	// paths without auth, so only the running major's own routes are asserted.)
	own := []string{"/global/health", "/global/event"}
	if wantMajor == apiV2 {
		own = []string{"/api/info", "/api/event", "/api/session/active"}
	}
	for _, p := range own {
		res, err := http.Get(addr + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", p, res.StatusCode)
		}
	}

	dir := t.TempDir()
	id, err := serveCreateSession(addr, dir, "contract")
	if err != nil {
		t.Fatal(err)
	}
	if !serveSessionExists(addr, id, dir) {
		t.Error("created session not found")
	}
	if serveSessionExists(addr, "ses_doesnotexist", dir) {
		t.Error("a missing session reported as existing")
	}
	m, err := activeSessions(addr, dir)
	if err != nil || len(m) != 0 {
		t.Errorf("idle daemon active list = %v, %v; want empty", m, err)
	}
	if serveSessionBusy(addr, id, dir) {
		t.Error("idle session reported busy")
	}
	if err := serveAbort(addr, id, dir); err != nil {
		t.Error(err)
	}

	// The event stream connects with auth and says hello.
	req, _ := http.NewRequest("GET", eventURL(addr), nil)
	res, err := sseClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("event stream = %d", res.StatusCode)
	}

	_ = os.Remove(servePasswordFile())
}
