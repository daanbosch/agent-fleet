package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// sharedDaemon is a v2 serve as far as the Terminal launch uses it: sessions with metadata,
// fork, model/agent switches, the environment, interrupt and /api/info.
type sharedDaemon struct {
	srv *httptest.Server
	mu  sync.Mutex
	// calls are "METHOD path body", in arrival order.
	calls    []string
	sessions map[string]map[string]string // id -> metadata
	envs     map[string]map[string]string // id -> PUT environment
	next     int
	pid      int
	version  string
	failEnv  int // fail this many environment PUTs with 503 before accepting
}

func newSharedDaemon(t *testing.T) *sharedDaemon {
	t.Helper()
	useMajor(t, 2)
	d := &sharedDaemon{sessions: map[string]map[string]string{}, envs: map[string]map[string]string{}, pid: 4242, version: "2.0.28"}
	d.srv = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.srv.Close)
	t.Setenv(serveAddrEnv, d.srv.URL)
	t.Setenv(standaloneEnv, "")
	resetServeAuth("pw-shared")
	detectedMajor.Store(int32(apiV2))
	old := ensureServe
	ensureServe = func() (string, int, error) { return d.srv.URL, 1, nil }
	t.Cleanup(func() { ensureServe = old; detectedMajor.Store(int32(apiV1)); resetServeAuth("") })
	return d
}

func (d *sharedDaemon) handle(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "pw-shared" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, r.Method+" "+r.URL.Path+" "+string(b))
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	md := func(m map[string]any) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
		return out
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // api session <id> <verb>
	switch {
	case r.URL.Path == "/api/info":
		reply(map[string]any{"version": d.version, "pid": d.pid})
	case r.Method == "POST" && r.URL.Path == "/api/session":
		d.next++
		id := fmt.Sprintf("ses_new%d", d.next)
		d.sessions[id] = map[string]string{}
		if m, ok := body["metadata"].(map[string]any); ok {
			d.sessions[id] = md(m)
		}
		reply(map[string]any{"data": map[string]any{"id": id}})
	case len(parts) >= 3 && parts[0] == "api" && parts[1] == "session":
		id := parts[2]
		cur, ok := d.sessions[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch {
		case len(parts) == 3 && r.Method == "GET":
			reply(map[string]any{"data": map[string]any{"id": id, "metadata": cur}})
		case len(parts) == 3 && r.Method == "PATCH":
			m, _ := body["metadata"].(map[string]any)
			d.sessions[id] = md(m) // replaces, as the real server does
			w.WriteHeader(http.StatusNoContent)
		case len(parts) == 4 && parts[3] == "fork":
			d.next++
			nid := fmt.Sprintf("ses_fork%d", d.next)
			d.sessions[nid] = map[string]string{}
			for k, v := range cur { // a fork copies the source's metadata
				d.sessions[nid][k] = v
			}
			reply(map[string]any{"data": map[string]any{"id": nid}})
		case len(parts) == 4 && parts[3] == "environment" && r.Method == "PUT":
			if d.failEnv > 0 {
				d.failEnv--
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			vars, _ := body["variables"].(map[string]any)
			d.envs[id] = md(vars)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent) // model, agent, interrupt
		}
	default:
		http.NotFound(w, r)
	}
}

func (d *sharedDaemon) callsMatching(prefix string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func terminalMeta(t *testing.T, name string) session.Meta {
	t.Helper()
	dir := t.TempDir()
	return session.Meta{Name: name, Dir: dir, Kind: session.KindOpencode, Driver: session.DriverTUI}
}

func launchEnv(plan agents.LaunchPlan) map[string]string { return shellEnvironment(plan.Env) }

// A fresh Terminal slot: the session is created through the API with its metadata, recorded at
// once, and the pane program is the attach wrapper — with the password in the pane environment
// and never on the command line.
func TestSharedLaunchCreatesTheSessionTaggedAndRecorded(t *testing.T) {
	d := newSharedDaemon(t)
	m := terminalMeta(t, "fresh")
	m.Model, m.Mode = "stub/m#high", "plan"
	plan, err := agentImpl{}.BuildLaunch(m, agents.LaunchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	sid := session.UUID(m.Dir, m.Name)
	creates := d.callsMatching("POST /api/session {")
	if len(creates) != 1 {
		t.Fatalf("creates = %v", d.calls)
	}
	var body struct {
		Metadata map[string]string `json:"metadata"`
		Model    map[string]string `json:"model"`
		Agent    string            `json:"agent"`
		Location map[string]string `json:"location"`
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(creates[0], "POST /api/session ")), &body)
	if body.Metadata[metaSID] != sid || body.Metadata[metaName] != "fresh" {
		t.Errorf("metadata = %v", body.Metadata)
	}
	if body.Model["providerID"] != "stub" || body.Model["id"] != "m" || body.Model["variant"] != "high" || body.Agent != "plan" {
		t.Errorf("model/agent = %v %q", body.Model, body.Agent)
	}
	if body.Location["directory"] != m.Dir {
		t.Errorf("location = %v", body.Location)
	}
	if got := sids.Read(sid); got != "ses_new1" {
		t.Errorf("slot = %q, want the created session recorded", got)
	}
	if strings.Contains(plan.Program, "pw-shared") {
		t.Errorf("the password is on the command line: %s", plan.Program)
	}
	for _, want := range []string{"workspace-agent opencode-attach", "--server '" + d.srv.URL + "'", "--session 'ses_new1'", "'--auto'"} {
		if !strings.Contains(plan.Program, want) {
			t.Errorf("program %q lacks %q", plan.Program, want)
		}
	}
	got := map[string]bool{}
	for _, kv := range plan.Env {
		got[kv] = true
	}
	for _, want := range []string{"OPENCODE_SERVER_PASSWORD=pw-shared", "OPENCODE_SERVER_USERNAME=opencode", "AF_OPENCODE_SHARED=1", "AF_SESSION_SID=" + sid} {
		if !got[want] {
			t.Errorf("pane env lacks %s", want)
		}
	}
	if strings.Contains(plan.Program, "--standalone") {
		t.Errorf("a shared launch must not be standalone: %s", plan.Program)
	}
}

// A slot with a recorded conversation resumes THAT session, switches its model and agent through
// the API (an existing session ignores config) and keeps the other metadata keys when it tags it.
func TestSharedLaunchResumeSwitchesAndKeepsOtherMetadata(t *testing.T) {
	d := newSharedDaemon(t)
	m := terminalMeta(t, "resume")
	m.Model, m.Mode = "stub/m", "normal"
	sid := session.UUID(m.Dir, m.Name)
	d.sessions["ses_old"] = map[string]string{"user.key": "keep"}
	sids.Write(sid, "ses_old")
	t.Cleanup(func() { sids.Remove(sid) })

	if _, err := (agentImpl{}).BuildLaunch(m, agents.LaunchOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(d.callsMatching("POST /api/session {")) != 0 || len(d.callsMatching("POST /api/session/ses_old/fork")) != 0 {
		t.Fatalf("a resume must not create or fork: %v", d.calls)
	}
	if len(d.callsMatching("POST /api/session/ses_old/model")) != 1 || len(d.callsMatching("POST /api/session/ses_old/agent {\"agent\":\"build\"}")) != 1 {
		t.Errorf("model/agent not switched: %v", d.calls)
	}
	want := map[string]string{"user.key": "keep", metaSID: sid, metaName: "resume"}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range want {
		if d.sessions["ses_old"][k] != v {
			t.Errorf("metadata[%s] = %q, want %q (PATCH replaces, so the merge must carry the rest): %v", k, d.sessions["ses_old"][k], v, d.sessions["ses_old"])
		}
	}
}

// A recorded session the server no longer has is not attached to: the slot starts a new one.
func TestSharedLaunchVanishedSessionStartsOver(t *testing.T) {
	d := newSharedDaemon(t)
	m := terminalMeta(t, "gone")
	sid := session.UUID(m.Dir, m.Name)
	sids.Write(sid, "ses_missing")
	t.Cleanup(func() { sids.Remove(sid) })
	if _, err := (agentImpl{}).BuildLaunch(m, agents.LaunchOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := sids.Read(sid); got != "ses_new1" || len(d.callsMatching("POST /api/session {")) != 1 {
		t.Errorf("slot = %q, calls %v", got, d.calls)
	}
}

// A fork: the copy is made with `before` when the fork is at a message, re-tagged for the NEW
// slot (the copy carries the source's af.sid), and recorded before anything else can fail.
func TestSharedLaunchForkRetagsTheCopy(t *testing.T) {
	d := newSharedDaemon(t)
	d.sessions["ses_src"] = sharedMetadata("source-sid", "source")
	m := terminalMeta(t, "copy")
	m.ForkFrom, m.ForkAt = "ses_src", "msg_5"
	if _, err := (agentImpl{}).BuildLaunch(m, agents.LaunchOpts{}); err != nil {
		t.Fatal(err)
	}
	if f := d.callsMatching("POST /api/session/ses_src/fork"); len(f) != 1 || !strings.Contains(f[0], `"before":"msg_5"`) {
		t.Fatalf("fork call = %v", f)
	}
	sid := session.UUID(m.Dir, m.Name)
	t.Cleanup(func() { sids.Remove(sid) })
	if got := sids.Read(sid); got != "ses_fork1" {
		t.Fatalf("slot = %q", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if md := d.sessions["ses_fork1"]; md[metaSID] != sid || md[metaName] != "copy" {
		t.Errorf("the fork keeps the source's identity: %v", md)
	}
	if md := d.sessions["ses_src"]; md[metaSID] != "source-sid" {
		t.Errorf("the source was retagged: %v", md)
	}
}

// Without a serve to attach to (not connected, disabled, or the switch back) the launch is the
// standalone one, byte for byte.
func TestSharedLaunchFallsBackToStandalone(t *testing.T) {
	newSharedDaemon(t)
	ensureServe = func() (string, int, error) { return "", 0, ErrNotConnected }
	m := terminalMeta(t, "fallback")
	plan, err := (agentImpl{}).BuildLaunch(m, agents.LaunchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Program != "opencode --standalone --auto" {
		t.Errorf("fallback program = %q", plan.Program)
	}

	t.Setenv(standaloneEnv, "1")
	ensureServe = func() (string, int, error) { t.Fatal("the daemon must not even be asked for"); return "", 0, nil }
	if plan, err = (agentImpl{}).BuildLaunch(m, agents.LaunchOpts{}); err != nil || plan.Program != "opencode --standalone --auto" {
		t.Errorf("switched back: %q, %v", plan.Program, err)
	}
}

// A daemon older than the installed opencode is replaced before a TUI of the new version attaches.
func TestSharedLaunchReplacesAnOutdatedDaemon(t *testing.T) {
	d := newSharedDaemon(t)
	d.version = "2.0.20"
	var reasons []string
	restartServe = func(reason string) bool {
		reasons = append(reasons, reason)
		d.mu.Lock()
		d.version = "2.0.28" // what the replacement reports
		d.mu.Unlock()
		return true
	}
	t.Cleanup(func() { restartServe = func(reason string) bool { return Serve().Restart(reason) } })
	if _, err := (agentImpl{}).BuildLaunch(terminalMeta(t, "upgraded"), agents.LaunchOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "2.0.20") || !strings.Contains(reasons[0], "2.0.28") {
		t.Errorf("restarts = %v", reasons)
	}
	// Matching versions: no restart.
	reasons = nil
	if _, err := (agentImpl{}).BuildLaunch(terminalMeta(t, "current"), agents.LaunchOpts{}); err != nil || len(reasons) != 0 {
		t.Errorf("a current daemon was restarted: %v %v", reasons, err)
	}
}

// opencode 1 never touches the API for a Terminal launch.
func TestSharedLaunchIsV2Only(t *testing.T) {
	useMajor(t, 1)
	ensureServe = func() (string, int, error) { t.Fatal("v1 must not use the shared serve"); return "", 0, nil }
	t.Cleanup(func() { ensureServe = func() (string, int, error) { return Serve().Ensure() } })
	plan, err := (agentImpl{}).BuildLaunch(terminalMeta(t, "v1"), agents.LaunchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plan.Program, "opencode --auto") || strings.Contains(plan.Program, "attach") {
		t.Errorf("v1 program = %q", plan.Program)
	}
}

// The attach wrapper puts the pane's environment — without the server credentials — and only
// then starts the TUI; a server that will not take it stops the launch.
func TestAttachPutsTheEnvironmentBeforeTheTUI(t *testing.T) {
	d := newSharedDaemon(t)
	d.sessions["ses_a"] = map[string]string{}
	d.failEnv = 2
	oldRetries := attachRetries
	attachRetries = 5
	t.Cleanup(func() { attachRetries = oldRetries })
	environ := []string{"AF_SESSION_SID=sid-1", "PATH=/x", "OPENCODE_SERVER_PASSWORD=pw-shared", "OPENCODE_SERVER_USERNAME=opencode", "junk"}
	var started []string
	err := runAttach([]string{"--server", d.srv.URL, "--session", "ses_a", "--", "--auto"}, environ, func(argv, _ []string) error {
		if len(d.envs["ses_a"]) == 0 {
			t.Error("the TUI started before the environment was put")
		}
		started = argv
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "--server " + d.srv.URL + " --session ses_a --auto"; strings.Join(started, " ") != want {
		t.Errorf("tui argv = %v, want %q", started, want)
	}
	got := d.envs["ses_a"]
	if got["AF_SESSION_SID"] != "sid-1" || got["PATH"] != "/x" {
		t.Errorf("environment = %v", got)
	}
	for k := range got {
		if strings.HasPrefix(k, "OPENCODE_SERVER") || k == "junk" {
			t.Errorf("%s must not reach the shell", k)
		}
	}

	d.failEnv = 100
	err = runAttach([]string{"--server", d.srv.URL, "--session", "ses_a", "--"}, environ, func(_, _ []string) error {
		t.Error("the TUI must not start without its environment")
		return nil
	})
	if err == nil {
		t.Error("an environment the server never took must stop the launch")
	}
}

// The pane's environment is read back from the process tree for a server restart.
func TestPaneEnvironmentFromTheProcessTree(t *testing.T) {
	proc := t.TempDir()
	write := func(pid, ppid int, environ ...string) {
		dir := filepath.Join(proc, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The command name may hold spaces and parentheses.
		stat := fmt.Sprintf("%d (sh -c (x)) S %d 1 1 0 -1", pid, ppid)
		_ = os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "environ"), []byte(strings.Join(environ, "\x00")+"\x00"), 0o644)
	}
	pane := []string{"AF_SESSION_NAME=slot", "OPENCODE_SERVER_PASSWORD=pw", "PATH=/a"}
	write(100, 1, pane...)                                                  // the pane's shell: tmux -e values only
	write(101, 100, append([]string{"JAVA_HOME=/jdk"}, pane...)...)         // the TUI: plus the shell's exports
	write(102, 101, "AF_SESSION_NAME=slot")                                 // a child that is not the TUI
	write(200, 1, "AF_SESSION_NAME=slot", "OPENCODE_SERVER_PASSWORD=other") // not under this pane
	got, ok := envOfPaneTree(proc, 100, "slot")
	if !ok || got["JAVA_HOME"] != "/jdk" || got["PATH"] != "/a" {
		t.Fatalf("env = %v ok=%v, want the TUI's (with the toolchain export)", got, ok)
	}
	if _, has := got["OPENCODE_SERVER_PASSWORD"]; has {
		t.Error("the server password must not be in the shell environment")
	}
	if _, ok := envOfPaneTree(proc, 100, "another"); ok {
		t.Error("another slot's name must not match")
	}
}

// After a restart every live Terminal slot gets its pane's environment back, and the daemon is
// marked ready only afterwards.
func TestRestoreTerminalEnvsPutsEveryLiveSlotThenMarksReady(t *testing.T) {
	d := newSharedDaemon(t)
	a, b := terminalMeta(t, "a"), terminalMeta(t, "b")
	d.sessions["ses_a"], d.sessions["ses_b"] = map[string]string{}, map[string]string{}
	sids.Write(session.UUID(a.Dir, a.Name), "ses_a")
	sids.Write(session.UUID(b.Dir, b.Name), "ses_b")
	t.Cleanup(func() { sids.Remove(session.UUID(a.Dir, a.Name)); sids.Remove(session.UUID(b.Dir, b.Name)) })
	oldSlots, oldEnv := terminalSlots, paneEnv
	terminalSlots = func() []session.Meta { return []session.Meta{a, b} }
	paneEnv = func(name string) (map[string]string, bool) {
		if name == "b" {
			return nil, false // its pane is gone: skipped, not fatal
		}
		return map[string]string{"AF_SESSION_NAME": name}, true
	}
	t.Cleanup(func() { terminalSlots, paneEnv = oldSlots, oldEnv })

	if readReadyMarker() != 0 {
		t.Fatal("a marker from nowhere")
	}
	restoreTerminalEnvs(d.srv.URL)
	if d.envs["ses_a"]["AF_SESSION_NAME"] != "a" {
		t.Errorf("slot a's environment not restored: %v", d.envs)
	}
	if _, has := d.envs["ses_b"]; has {
		t.Errorf("slot b has no pane environment to restore: %v", d.envs)
	}
	if readReadyMarker() != d.pid {
		t.Errorf("ready marker = %d, want the daemon's pid %d", readReadyMarker(), d.pid)
	}
}

// Terminal panes are dependents of the daemon (the idle stop must not pull it from under a TUI),
// and drain covers their running turns.
func TestTerminalSlotsAreDependentsAndDrained(t *testing.T) {
	d := newSharedDaemon(t)
	a := terminalMeta(t, "busy")
	sids.Write(session.UUID(a.Dir, a.Name), "ses_busy")
	t.Cleanup(func() { sids.Remove(session.UUID(a.Dir, a.Name)) })
	oldSlots := terminalSlots
	terminalSlots = func() []session.Meta { return []session.Meta{a} }
	t.Cleanup(func() { terminalSlots = oldSlots })

	base := dependents()
	if base < 1 {
		t.Fatalf("dependents() = %d with a live Terminal slot", base)
	}
	t.Setenv(standaloneEnv, "1")
	if got := dependents(); got != base-1 {
		t.Errorf("a standalone-mode workspace has no shared Terminal dependents: %d vs %d", got, base)
	}
	t.Setenv(standaloneEnv, "")

	// busyTerminals reads the global active list for the slot's session.
	active := ""
	d.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session/active" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{%s}}`, active)
			return
		}
		d.handle(w, r)
	})
	if got := busyTerminals(d.srv.URL); len(got) != 0 {
		t.Errorf("idle slot reported busy: %v", got)
	}
	active = `"ses_busy":{"type":"running"},"ses_other":{"type":"running"}`
	if got := busyTerminals(d.srv.URL); len(got) != 1 || got[0] != "ses_busy" {
		t.Errorf("busy terminals = %v, want only the slot's own session", got)
	}
}

// Stopping a Terminal slot interrupts its turn on the server: closing the pane does not.
func TestStopInterruptsTheTerminalTurn(t *testing.T) {
	d := newSharedDaemon(t)
	d.sessions["ses_x"] = map[string]string{}
	m := terminalMeta(t, "stopme")
	sids.Write(session.UUID(m.Dir, m.Name), "ses_x")
	t.Cleanup(func() { sids.Remove(session.UUID(m.Dir, m.Name)) })
	// Running() needs a daemon this supervisor believes in.
	supervisor.mu.Lock()
	supervisor.up = true
	supervisor.mu.Unlock()
	t.Cleanup(func() { supervisor.mu.Lock(); supervisor.up = false; supervisor.mu.Unlock() })

	if (agentImpl{}).GracefulStop(m) {
		t.Error("the pane still has to be killed by the caller")
	}
	if got := d.callsMatching("POST /api/session/ses_x/interrupt"); len(got) != 1 {
		t.Errorf("interrupt calls = %v", d.calls)
	}
	DropHandle(m.Name) // archive and trash go through here; the meta is what names the slot
	session.WriteMeta(m)
	DropHandle(m.Name)
	if got := d.callsMatching("POST /api/session/ses_x/interrupt"); len(got) != 2 {
		t.Errorf("DropHandle of a Terminal slot did not interrupt: %v", d.calls)
	}
}

// State reads for a shared Terminal slot go through the API (its store is the server's), while a
// standalone launch keeps the file reader.
func TestV2TargetForTerminalOnlyWhenShared(t *testing.T) {
	newSharedDaemon(t)
	m := terminalMeta(t, "reader")
	if _, ok := v2TargetFor(m); !ok {
		t.Error("a shared Terminal slot must be read through the API")
	}
	t.Setenv(standaloneEnv, "1")
	if _, ok := v2TargetFor(m); ok {
		t.Error("standalone mode keeps the SQLite reader")
	}
}

// A daemon the Terminal TUIs attach to keeps its password; any other gets a fresh one.
func TestServePasswordIsStableOnlyForTheSharedDaemon(t *testing.T) {
	if p, err := nextServePassword("kept", true); err != nil || p != "kept" {
		t.Errorf("shared: %q, %v — a restarted server must keep the password the TUIs hold", p, err)
	}
	p1, _ := nextServePassword("kept", false)
	p2, _ := nextServePassword("kept", false)
	if p1 == "kept" || p1 == p2 || len(p1) < 32 {
		t.Errorf("a non-shared daemon gets a fresh random password: %q %q", p1, p2)
	}
	if p, err := nextServePassword("", true); err != nil || p == "" {
		t.Errorf("first start: %q, %v", p, err)
	}
}

func TestSharedErrorsAreTypedStatus(t *testing.T) {
	d := newSharedDaemon(t)
	if _, err := getSharedSession(d.srv.URL, "ses_nope"); !errors.Is(err, statusError(http.StatusNotFound)) {
		t.Errorf("err = %v", err)
	}
}
