package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// optInHome gives the test its own HOME with a pinned-1.x install on PATH-resolution
// (opencodeBinary left at its real default, so the opt-in's own branch is what is measured).
func optInHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(MajorEnv, "")
	oldPins := buildPinsPath
	buildPinsPath = filepath.Join(home, "no-versions.json")
	t.Cleanup(func() { buildPinsPath = oldPins })
	return home
}

// installFakeV2 lays out the private prefix the way `install-opencode2` leaves it.
func installFakeV2(t *testing.T) string {
	t.Helper()
	pkg := filepath.Join(V2Prefix(), "node_modules", "@opencode", "cli")
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@opencode/cli","version":"2.0.28"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(pkg, "bin", "opencode.exe")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

// With nothing opted in, every launch surface answers what it answered before the opt-in
// existed — even when the v2 package is installed. This is the "default stays as it is" pin.
func TestOptInOffChangesNothingEvenWithV2Installed(t *testing.T) {
	optInHome(t)
	useMajor(t, 1)
	stubUsagePref(t, UsageOwn)
	before := buildProgram("anthropic/claude-x", "plan", "ses_abc", false)
	envBefore := strings.Join(env(), "\n")
	installFakeV2(t) // installed, but not switched on

	if UseV2() || Binary() != "opencode" {
		t.Fatalf("UseV2=%v Binary=%q, want off and the bare name", UseV2(), Binary())
	}
	if got := buildProgram("anthropic/claude-x", "plan", "ses_abc", false); got != before {
		t.Fatalf("launch line changed by an installed-but-off v2:\n got %s\nwant %s", got, before)
	}
	if want := "opencode --auto --session 'ses_abc' --model 'anthropic/claude-x' --agent plan"; before != want {
		t.Fatalf("v1 launch line = %q, want %q", before, want)
	}
	if got := strings.Join(env(), "\n"); got != envBefore || strings.Contains(got, "OPENCODE_DB") {
		t.Fatalf("env changed or carries OPENCODE_DB while off: %q", got)
	}
	if launchKey() != "" {
		t.Fatalf("launchKey = %q, want empty", launchKey())
	}
}

func TestOptInOnLaunchesTheV2BinaryWithItsOwnDatabase(t *testing.T) {
	home := optInHome(t)
	exe := installFakeV2(t)
	stubUsagePref(t, UsageOwn)
	if err := SetWantsV2(true); err != nil {
		t.Fatal(err)
	}

	if !UseV2() || Binary() != exe {
		t.Fatalf("UseV2=%v Binary=%q, want on and %q", UseV2(), Binary(), exe)
	}
	if got := installedMajor(); got != 2 {
		t.Fatalf("installedMajor = %d, want 2 from the opt-in binary's manifest", got)
	}
	line := buildProgram("", "", "", false)
	if !strings.HasPrefix(line, "'"+exe+"' --standalone") {
		t.Fatalf("v2 line does not start with the quoted opt-in binary: %q", line)
	}
	resume := buildProgram("anthropic/m", "plan", "ses_abc", false)
	if !strings.Contains(resume, "AF_OC_OUT=$('"+exe+"' api --standalone") || strings.Contains(resume, "$(opencode ") || strings.Contains(resume, "then opencode ") {
		t.Fatalf("resume line still names the bare opencode: %q", resume)
	}
	wantDB := filepath.Join(home, ".local", "share", "agent-fleet", "opencode2-data", "opencode.db")
	found := false
	for _, kv := range env() {
		if kv == "OPENCODE_DB="+wantDB {
			found = true
		}
	}
	if !found {
		t.Fatalf("env() lacks OPENCODE_DB=%s: %v", wantDB, env())
	}
	if filepath.Dir(V2DBPath()) == filepath.Join(home, ".local", "share", "opencode") {
		t.Fatal("the v2 database sits in the v1 data directory")
	}
}

func TestOptInOnWithoutBinaryStaysOnV1(t *testing.T) {
	optInHome(t)
	useMajor(t, 1)
	if err := SetWantsV2(true); err != nil {
		t.Fatal(err)
	}
	if UseV2() || Binary() != "opencode" || len(v2ProcessEnv()) != 0 {
		t.Fatalf("a request without an installed binary must not change anything (UseV2=%v)", UseV2())
	}
}

func TestOptInEnvironmentVariableWinsOverTheFile(t *testing.T) {
	optInHome(t)
	installFakeV2(t)
	if err := SetWantsV2(true); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MajorEnv, "1")
	if UseV2() {
		t.Fatal("AF_OPENCODE_MAJOR=1 must opt out whatever the file says")
	}
	if err := SetWantsV2(false); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MajorEnv, "2")
	if !UseV2() {
		t.Fatal("AF_OPENCODE_MAJOR=2 must opt in")
	}
}

// The shell of a session must not inherit the v2 database: a 1.x `opencode` run there would
// write 1.x tables into it.
func TestOptInKeepsTheDatabasePathOutOfSessionShells(t *testing.T) {
	optInHome(t)
	installFakeV2(t)
	if serverOnlyEnv("OPENCODE_DB") {
		t.Fatal("OPENCODE_DB is stripped while the opt-in is off (a member's own setting must pass)")
	}
	_ = SetWantsV2(true)
	got := shellEnvironment([]string{"OPENCODE_DB=/x", "FOO=1"})
	if _, ok := got["OPENCODE_DB"]; ok || got["FOO"] != "1" {
		t.Fatalf("shell environment = %v", got)
	}
}

func sleepServe(t *testing.T) *exec.Cmd {
	t.Helper()
	// "serve" in argv is what recordedServe checks before it trusts a pid.
	cmd := exec.Command("sh", "-c", "sleep 30", "serve")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func exitedBySignal(t *testing.T, cmd *exec.Cmd, sig syscall.Signal) bool {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return false
		}
		ws, ok := ee.Sys().(syscall.WaitStatus)
		return ok && ws.Signaled() && ws.Signal() == sig
	case <-time.After(5 * time.Second):
		return false
	}
}

// Flipping the switch while a daemon the Agent started is up replaces it (Restart: drain, stop,
// re-resume); a daemon already started the new way is left alone.
func TestFlippingTheSwitchRestartsTheOwnedDaemon(t *testing.T) {
	s := unspawnableAddr(t)
	t.Setenv(MajorEnv, "")
	installFakeV2(t)
	cmd := sleepServe(t)
	s.mu.Lock()
	s.up, s.cmd, s.key = true, cmd, "" // started on the pinned 1.x
	s.mu.Unlock()

	if err := SetWantsV2(true); err != nil {
		t.Fatal(err)
	}
	if err := s.retireStale(); err != nil {
		t.Fatal(err)
	}
	if !exitedBySignal(t, cmd, syscall.SIGTERM) {
		t.Fatal("the 1.x daemon was not stopped when the switch went to 2")
	}
	if s.Running() {
		t.Fatal("the supervisor still believes the old daemon is up")
	}
}

func TestSwitchUnchangedLeavesTheDaemonAlone(t *testing.T) {
	s := unspawnableAddr(t)
	t.Setenv(MajorEnv, "")
	installFakeV2(t)
	_ = SetWantsV2(true)
	cmd := sleepServe(t)
	s.mu.Lock()
	s.up, s.cmd, s.key = true, cmd, launchKey()
	s.mu.Unlock()
	if err := s.retireStale(); err != nil {
		t.Fatal(err)
	}
	if !s.Running() || cmd.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("a daemon started under the current setting was replaced")
	}
}

func TestSwitchingBackReplacesTheV2Daemon(t *testing.T) {
	s := unspawnableAddr(t)
	t.Setenv(MajorEnv, "")
	installFakeV2(t)
	_ = SetWantsV2(true)
	key := launchKey()
	cmd := sleepServe(t)
	s.mu.Lock()
	s.up, s.cmd, s.key = true, cmd, key
	s.mu.Unlock()
	if err := SetWantsV2(false); err != nil {
		t.Fatal(err)
	}
	if err := s.retireStale(); err != nil {
		t.Fatal(err)
	}
	if !exitedBySignal(t, cmd, syscall.SIGTERM) {
		t.Fatal("the 2.x daemon was not stopped when the switch went back to 1")
	}
}

// A daemon an earlier Agent process started is found by the pid it recorded.
func TestFlippingTheSwitchStopsAnAdoptedDaemonByItsRecordedPid(t *testing.T) {
	s := unspawnableAddr(t)
	t.Setenv(MajorEnv, "")
	installFakeV2(t)
	_ = SetWantsV2(true)
	cmd := sleepServe(t)
	noteServeStart(cmd.Process.Pid, launchKey())
	_ = SetWantsV2(false)
	s.mu.Lock()
	s.up, s.cmd, s.key = true, nil, adoptedKey()
	s.mu.Unlock()
	if err := s.retireStale(); err != nil {
		t.Fatal(err)
	}
	if !exitedBySignal(t, cmd, syscall.SIGTERM) {
		t.Fatal("the adopted 2.x daemon was not stopped by its recorded pid")
	}
}

func TestFlippingWithAnUntraceableAdoptedDaemonSaysSo(t *testing.T) {
	s := unspawnableAddr(t)
	t.Setenv(MajorEnv, "")
	installFakeV2(t)
	_ = SetWantsV2(true)
	s.mu.Lock()
	s.up, s.cmd, s.key = true, nil, "" // adopted, no record: started by an Agent without the opt-in
	s.mu.Unlock()
	if err := s.retireStale(); err == nil {
		t.Fatal("an adopted daemon of the other major with no recorded pid must be reported, not attached to")
	}
}

// A workspace that never opts in never gets the record file.
func TestDefaultStartLeavesNoServeRecord(t *testing.T) {
	optInHome(t)
	noteServeStart(os.Getpid(), "")
	if _, err := os.Stat(serveRecordFile()); err == nil {
		t.Fatal("a default start wrote the opt-in's record file")
	}
}

// A slot launched on 2.x keeps a "shared" record; a later 1.x launch of it must clear that or
// switching back to 2 would count the 1.x pane as attached to the serve.
func TestV1LaunchClearsAStaleSharedSlotMode(t *testing.T) {
	optInHome(t)
	useMajor(t, 1)
	stubUsagePref(t, UsageOwn)
	m := session.Meta{Name: "oc-1", Dir: t.TempDir(), Kind: session.KindOpencode}
	sid := session.UUID(m.Dir, m.Name)
	markSlotMode(sid, modeShared)
	if _, err := (agentImpl{}).BuildLaunch(m, agents.LaunchOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := slotModes.Read(sid); got != "" {
		t.Fatalf("slot mode after a 1.x launch = %q, want none", got)
	}
}

// The replacement is driven by ensure itself, not only by retireStale: the Console's polling
// of Resume is what notices the flip.
func TestEnsureReplacesADaemonStartedUnderTheOtherSetting(t *testing.T) {
	s := unspawnableAddr(t)
	stubUsagePref(t, UsageOff) // past the replacement, ensure stops at the connection gate
	t.Setenv(MajorEnv, "")
	installFakeV2(t)
	cmd := sleepServe(t)
	s.mu.Lock()
	s.up, s.cmd, s.key = true, cmd, ""
	s.mu.Unlock()
	if err := SetWantsV2(true); err != nil {
		t.Fatal(err)
	}
	_, _, _ = s.Ensure()
	if !exitedBySignal(t, cmd, syscall.SIGTERM) {
		t.Fatal("Ensure left the daemon of the other setting running")
	}
}
