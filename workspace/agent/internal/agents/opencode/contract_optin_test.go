//go:build contract

// The opencode 2 opt-in against the REAL binaries in an isolated HOME: the pinned 1.x on
// PATH, opencode 2 installed into the opt-in's private prefix, a loopback stub provider standing
// in for the model (no credentials, no billing).
//
//	AF_TEST_OPENCODE2_PREFIX=<prefix made by install-opencode2> \
//	  PATH=<dir with opencode 1.x>:$PATH go test -tags contract -run OptIn ./internal/agents/opencode/
//
// Not run in CI, like the other v2 contracts.
package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/testguard"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/tmuxx"
)

// dataDigest hashes every file under dir except the log directory: both majors append to the
// same opencode.log, which is not state either of them reads back.
func dataDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(rel, "log"+string(filepath.Separator)) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:8])
		return nil
	})
	return out
}

func digestString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(m[k] + " " + k + "\n")
	}
	return b.String()
}

func TestContractOptInRunsV2BesideV1WithoutTouchingV1Data(t *testing.T) {
	requireOpencode(t)
	if v := opencodeVersion(t); !strings.HasPrefix(strings.TrimPrefix(v, "opencode v"), "1.") {
		t.Skipf("needs opencode 1.x first on PATH, got %s", v)
	}
	prefix := os.Getenv("AF_TEST_OPENCODE2_PREFIX")
	if prefix == "" {
		t.Skip("AF_TEST_OPENCODE2_PREFIX not set")
	}
	stub := &stubProvider{}
	llm := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(func() { llm.CloseClientConnections(); llm.Close() })

	home := t.TempDir()
	for _, kv := range envWithHome(home)[:5] {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	t.Setenv(MajorEnv, "")
	t.Setenv("AF_SESSIONS_DIR", t.TempDir())
	cfgDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"model":"stub/m","small_model":"stub/m","enabled_providers":["stub"],"permission":{"*":"allow"},
	"provider":{"stub":{"npm":"@ai-sdk/openai-compatible","name":"stub",
	"options":{"baseURL":"` + llm.URL + `/v1","apiKey":"x"},
	"models":{"m":{"name":"m","tool_call":true,"limit":{"context":8000,"output":1000}}}}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "opencode.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()

	// --- the member's 1.x data: a real session in the real 1.x database ---
	v1 := exec.Command("opencode", "run", "--format", "json", "--dir", work, "-m", "stub/m", "hello from v1")
	v1.Env, v1.Dir = envWithHome(home), work
	if out, err := v1.CombinedOutput(); err != nil {
		t.Fatalf("v1 run: %v\n%s", err, out)
	}
	v1DataDir := filepath.Join(home, ".local", "share", "opencode")
	before := dataDigest(t, v1DataDir)
	if _, ok := before["opencode.db"]; !ok {
		t.Fatalf("v1 left no database: %v", before)
	}
	listV1 := func() string {
		c := exec.Command("opencode", "session", "list", "--format", "json")
		c.Env, c.Dir = envWithHome(home), work
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("v1 session list: %v\n%s", err, out)
		}
		return string(out)
	}
	v1Sessions := listV1()
	if !strings.Contains(v1Sessions, "ses_") {
		t.Fatalf("the v1 session is not listed:\n%s", v1Sessions)
	}
	before = dataDigest(t, v1DataDir)

	// --- install (by link to the prefix install-opencode2 made) and switch ---
	if err := os.MkdirAll(filepath.Dir(V2Prefix()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(prefix, V2Prefix()); err != nil {
		t.Fatal(err)
	}
	oldPref := UsagePref
	UsagePref = func() string { return UsageFree }
	t.Cleanup(func() { UsagePref = oldPref })
	addr := "http://127.0.0.1:" + freePort(t)
	t.Setenv(serveAddrEnv, addr)
	detectedMajor.Store(int32(apiV1))
	t.Cleanup(func() { Serve().Shutdown(); detectedMajor.Store(int32(apiV1)); resetServeAuth("") })

	// Off: the default is 1.x. Ensure starts a 1.x daemon and the probe says so.
	if installedMajor() != 1 || UseV2() {
		t.Fatalf("before the switch: major %d, UseV2 %v", installedMajor(), UseV2())
	}
	if _, _, err := Serve().Ensure(); err != nil {
		t.Fatalf("ensure on 1.x: %v", err)
	}
	if isV2() {
		t.Fatal("a default start produced a 2.x daemon")
	}
	genV1 := Serve().Generation()

	// On: the running 1.x daemon is replaced by a 2.x one.
	if err := SetWantsV2(true); err != nil {
		t.Fatal(err)
	}
	if err := EnsureServiceDisabled(); err != nil {
		t.Fatal(err)
	}
	if installedMajor() != 2 {
		t.Fatalf("after the switch installedMajor = %d, want 2", installedMajor())
	}
	// The 1.x daemon is gone once retireStale returns; its own writes (checkpointing the WAL on
	// exit) are 1.x activity, so the reference digest is taken here, before any 2.x process.
	if err := Serve().retireStale(); err != nil {
		t.Fatal(err)
	}
	if Serve().Running() {
		t.Fatal("the 1.x daemon is still supervised after the switch")
	}
	before = dataDigest(t, v1DataDir)
	if _, _, err := Serve().Ensure(); err != nil {
		t.Fatalf("ensure after the switch: %v", err)
	}
	if !isV2() || Serve().Generation() == genV1 {
		t.Fatalf("the daemon was not replaced (v2=%v gen %d→%d)", isV2(), genV1, Serve().Generation())
	}
	if _, err := os.Stat(V2DBPath()); err != nil {
		t.Fatalf("the 2.x daemon did not create its own database: %v", err)
	}

	// --- a Managed turn on 2.x ---
	ses, err := serveCreateSession(addr, work, "optin")
	if err != nil {
		t.Fatal(err)
	}
	st := agents.ThreadSettings{Model: "stub/m"}
	req, err := newTurnRequest(addr, ses, work, agents.TurnInput{Prompt: "hello from v2"}, st)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sendTurn(addr, ses, work, req, st)
	if err != nil {
		t.Fatalf("managed turn: %v", err)
	}
	if e, has := decodeTurnError(res.Body); has {
		t.Fatalf("managed turn failed: %+v", e)
	}
	res.Body.Close()
	m := session.Meta{Name: "optin-managed", Dir: work, Kind: session.KindOpencode, Driver: session.DriverManaged}
	sids.Write(session.UUID(m.Dir, m.Name), ses)
	t.Cleanup(func() { sids.Remove(session.UUID(m.Dir, m.Name)) })
	td, _ := readTranscript(m)
	if len(td.Turns) < 2 || !strings.Contains(td.Turns[len(td.Turns)-1].Text, "ok") {
		t.Fatalf("the 2.x managed turn is not in the mirror: %+v", td.Turns)
	}

	// --- a Terminal session on 2.x: the real BuildLaunch (af's shared serve) in a real tmux pane
	// running the opt-in binary's TUI through the real `workspace-agent opencode-attach` ---
	testguard.IsolateTmux(t)
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "workspace-agent"), ".")
	build.Dir = filepath.Join("..", "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build workspace-agent: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_WORKSPACE_AGENT_BIN", filepath.Join(bin, "workspace-agent"))
	tm := session.Meta{Name: "optin-term", Dir: work, Kind: session.KindOpencode, Driver: session.DriverTUI, Model: "stub/m"}
	plan, err := agentImpl{}.BuildLaunch(tm, agents.LaunchOpts{})
	if err != nil {
		t.Fatalf("BuildLaunch: %v", err)
	}
	session.WriteMeta(tm)
	t.Cleanup(func() { session.RemoveMeta(tm.Name) })
	targs := []string{"new-session", "-d", "-s", session.TmuxName(tm.Name), "-x", "140", "-y", "40", "-c", plan.Cwd}
	for _, kv := range append(plan.Env, "AF_SESSION_NAME="+tm.Name) {
		targs = append(targs, "-e", kv)
	}
	targs = append(targs, plan.Program)
	if out, err := tmuxx.Cmd(targs...).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v %s", err, out)
	}
	t.Cleanup(func() { _ = tmuxx.Cmd("kill-session", "-t", session.TmuxName(tm.Name)).Run() })
	paneHas := func(want string) bool {
		for d := time.Now().Add(40 * time.Second); time.Now().Before(d); time.Sleep(300 * time.Millisecond) {
			if strings.Contains(tmuxx.CapturePane(session.TmuxName(tm.Name)), want) {
				return true
			}
		}
		return false
	}
	if !paneHas("auto") {
		t.Fatalf("the TUI never came up:\n%s", tmuxx.CapturePane(session.TmuxName(tm.Name)))
	}
	seenBefore := stub.seen
	_ = tmuxx.Cmd("send-keys", "-t", tmuxx.SessionPaneID(session.TmuxName(tm.Name)), "hello from terminal", "Enter").Run()
	for d := time.Now().Add(40 * time.Second); time.Now().Before(d) && stub.seen == seenBefore; time.Sleep(300 * time.Millisecond) {
	}
	if stub.seen == seenBefore {
		t.Fatalf("the Terminal prompt never reached the stub:\n%s", tmuxx.CapturePane(session.TmuxName(tm.Name)))
	}
	if !paneHas("ok") {
		t.Fatalf("the Terminal answer never showed:\n%s", tmuxx.CapturePane(session.TmuxName(tm.Name)))
	}
	if ts := sids.Read(session.UUID(work, tm.Name)); ts == "" || LiveState(tm) == "" {
		t.Fatalf("the Terminal slot was not recorded or read (%q)", ts)
	}

	// --- 1.x data untouched ---
	Serve().Shutdown()
	if got := dataDigest(t, v1DataDir); digestString(got) != digestString(before) {
		t.Fatalf("1.x data changed while 2.x ran:\nbefore\n%safter\n%s", digestString(before), digestString(got))
	}

	// --- off again: 1.x, with its session ---
	if err := SetWantsV2(false); err != nil {
		t.Fatal(err)
	}
	if installedMajor() != 1 || Binary() != "opencode" {
		t.Fatalf("after switching back: major %d, binary %s", installedMajor(), Binary())
	}
	time.Sleep(100 * time.Millisecond)
	if got := listV1(); got != v1Sessions {
		t.Fatalf("1.x session list changed:\nbefore %s\nafter  %s", v1Sessions, got)
	}
	if _, _, err := Serve().Ensure(); err != nil {
		t.Fatalf("ensure back on 1.x: %v", err)
	}
	if isV2() {
		t.Fatal("after switching back the daemon is still 2.x")
	}
	if stub.seen < 2 {
		t.Fatalf("the stub saw %d requests; both majors should have reached it", stub.seen)
	}
}
