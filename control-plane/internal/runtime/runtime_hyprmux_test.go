package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hyprmux-host is single-user like native: it serves the host's own session registry.
func TestNewFactoryHyprmuxRefusesNonDev(t *testing.T) {
	for _, profile := range []string{"hyprmux-host", "hyprmux"} {
		for _, auth := range []string{"proxy", ""} {
			_, err := NewFactory(profile, Config{AuthMode: auth})
			if err == nil || !strings.Contains(err.Error(), "AUTH=dev") {
				t.Fatalf("NewFactory(%q, AUTH=%q) = %v, want the AUTH=dev refusal", profile, auth, err)
			}
		}
	}
}

func TestNewFactoryHyprmuxRequiresPaths(t *testing.T) {
	t.Setenv("AF_HYPRMUX_SHIM", "")
	t.Setenv("AF_HYPRMUX_REGISTRY", "/nonexistent/sessions-v4.json")
	t.Setenv("AF_HYPRMUX_LOGS", "/nonexistent/agent-logs")
	_, err := NewFactory("hyprmux-host", Config{AuthMode: "dev"})
	if err == nil || !strings.Contains(err.Error(), "AF_HYPRMUX_SHIM") {
		t.Fatalf("NewFactory with no AF_HYPRMUX_SHIM = %v, want it named as required", err)
	}
}

// The shim is built from source and started for real; its registry is the committed
// fixture, never the host's live sessions-v4.json.
func TestHyprmuxHostStartStop(t *testing.T) {
	shim := buildHyprmuxShimForTest(t)
	registry, err := filepath.Abs(filepath.Join("..", "..", "..", "workspace", "hyprmux-shim", "testdata", "sessions-v4.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AF_HYPRMUX_SHIM", shim)
	t.Setenv("AF_HYPRMUX_REGISTRY", registry)
	t.Setenv("AF_HYPRMUX_LOGS", t.TempDir())

	f, err := NewFactory("hyprmux-host", Config{AuthMode: "dev"})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	ws := Workspace{ContainerName: "hyprmux-host", DataDir: t.TempDir(), AgentPort: freeLoopbackPortForTest(t), AgentToken: "test-token"}
	rt := f.New(ws, SecretKeys{}, nil)
	ctx := context.Background()
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	if got := rt.State(ctx); got != "none" {
		t.Fatalf("before Start: State=%q, want none", got)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := rt.State(ctx); got != "running" {
		t.Fatalf("after Start: State=%q, want running", got)
	}
	pid := readPidFile(filepath.Join(ws.DataDir, "shim.pid"))
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if again := readPidFile(filepath.Join(ws.DataDir, "shim.pid")); again != pid {
		t.Fatalf("second Start respawned the shim: pid %d -> %d", pid, again)
	}

	want := fixtureRowCount(t, registry)
	if got := listShimSessions(t, rt.Endpoint(), rt.Token()); got != want {
		t.Fatalf("GET /sessions returned %d rows, want all %d from the fixture", got, want)
	}

	if err := rt.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := rt.State(ctx); got != "none" {
		t.Fatalf("after Stop: State=%q, want none", got)
	}
	if _, err := http.Get(rt.Endpoint() + "/healthz"); err == nil {
		t.Fatal("shim still answers after Stop")
	}
}

func buildHyprmuxShimForTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hyprmux-shim")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join("..", "..", "..", "workspace", "hyprmux-shim")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build hyprmux-shim: %v\n%s", err, out)
	}
	return bin
}

func freeLoopbackPortForTest(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func fixtureRowCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return len(f.Sessions)
}

func listShimSessions(t *testing.T, endpoint, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, endpoint+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sessions: status %d", resp.StatusCode)
	}
	var body struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return len(body.Sessions)
}

// An orphan shim from an earlier CP is still alive, but it answers on another port, so this
// runtime's endpoint is dead. Start must replace it rather than report "starting" forever.
func TestHyprmuxStartReplacesOrphanThatNeverAnswers(t *testing.T) {
	shim := buildHyprmuxShimForTest(t)
	registry, err := filepath.Abs(filepath.Join("..", "..", "..", "workspace", "hyprmux-shim", "testdata", "sessions-v4.json"))
	if err != nil {
		t.Fatal(err)
	}
	logs := t.TempDir()
	t.Setenv("AF_HYPRMUX_SHIM", shim)
	t.Setenv("AF_HYPRMUX_REGISTRY", registry)
	t.Setenv("AF_HYPRMUX_LOGS", logs)
	f, err := NewFactory("hyprmux-host", Config{AuthMode: "dev"})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	ws := Workspace{ContainerName: "hyprmux-host", DataDir: t.TempDir(), AgentPort: freeLoopbackPortForTest(t), AgentToken: "test-token"}

	orphanPID := startOrphanShim(t, shim, registry, logs)
	if err := os.WriteFile(filepath.Join(ws.DataDir, hyprmuxPidFile), []byte(strconv.Itoa(orphanPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := f.New(ws, SecretKeys{}, nil)
	ctx := context.Background()
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	if got := rt.State(ctx); got != "starting" {
		t.Fatalf("with an unanswering orphan: State=%q, want starting", got)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start over an orphan: %v", err)
	}
	if got := rt.State(ctx); got != "running" {
		t.Fatalf("after Start over an orphan: State=%q, want running", got)
	}
	if pid := readPidFile(filepath.Join(ws.DataDir, hyprmuxPidFile)); pid == orphanPID {
		t.Fatalf("Start adopted the orphan pid %d", pid)
	}
	if pidAlive(orphanPID, shim) {
		t.Fatalf("orphan pid %d still alive after Start", orphanPID)
	}
}

// Start on a context that is already done returns its error before any side effect: the data
// dir is not created, no log is opened, and no shim is left listening on the workspace port.
func TestHyprmuxStartFailsFastOnDoneContext(t *testing.T) {
	shim := buildHyprmuxShimForTest(t)
	registry, err := filepath.Abs(filepath.Join("..", "..", "..", "workspace", "hyprmux-shim", "testdata", "sessions-v4.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AF_HYPRMUX_SHIM", shim)
	t.Setenv("AF_HYPRMUX_REGISTRY", registry)
	t.Setenv("AF_HYPRMUX_LOGS", t.TempDir())
	f, err := NewFactory("hyprmux-host", Config{AuthMode: "dev"})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"canceled", canceled, context.Canceled},
		{"deadline passed", expired, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "hyprmux-host")
			ws := Workspace{ContainerName: "hyprmux-host", DataDir: dataDir, AgentPort: freeLoopbackPortForTest(t), AgentToken: "test-token"}
			rt := f.New(ws, SecretKeys{}, nil)

			started := time.Now()
			err := rt.Start(tc.ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Start = %v, want %v", err, tc.want)
			}
			if elapsed := time.Since(started); elapsed >= hyprmuxReadyWait {
				t.Fatalf("Start took %v, want it to fail before the ready wait (%v)", elapsed, hyprmuxReadyWait)
			}
			if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Start touched the data dir (stat err=%v); it must not run before the ctx check", err)
			}
			if conn, err := net.Dial("tcp", "127.0.0.1:"+ws.AgentPort); err == nil {
				conn.Close()
				t.Fatal("a shim is listening on the workspace port after a refused Start")
			}
		})
	}
}

// startOrphanShim runs a real shim that answers on a loopback port of its own. The port is
// picked and then released before the shim binds it, so another process can take it in
// between; that bind race is retried rather than reported as a fault in the code under test.
func startOrphanShim(t *testing.T, shim, registry, logs string) int {
	t.Helper()
	for range 3 {
		port := freeLoopbackPortForTest(t)
		cmd := exec.Command(shim)
		cmd.Env = []string{
			"HYPRMUX_SHIM_ADDR=127.0.0.1:" + port,
			"HYPRMUX_SHIM_TOKEN=orphan-token",
			"HYPRMUX_REGISTRY=" + registry,
			"HYPRMUX_AGENT_LOGS=" + logs,
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start orphan: %v", err)
		}
		go func() { _ = cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		if err := WaitAgentHealthy(context.Background(), "http://127.0.0.1:"+port, hyprmuxReadyWait); err == nil {
			return cmd.Process.Pid
		}
		_ = cmd.Process.Kill()
	}
	t.Fatal("orphan shim never answered on its own port")
	return 0
}
