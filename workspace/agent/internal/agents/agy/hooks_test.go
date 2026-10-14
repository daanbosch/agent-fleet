package agy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func hookFixture(t *testing.T, name string) (session.Meta, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	m := session.Meta{Dir: "/home/dev/repos/proj", Name: name, Kind: session.KindAgy}
	session.WriteMeta(m)
	return m, session.UUID(m.Dir, m.Name)
}

// recordGen is recordHook under the slot's current launch generation, as the pane's env would carry.
func recordGen(name, event string, in hookInput, now time.Time) {
	m, _ := session.ReadMeta(name)
	sid := session.UUID(m.Dir, m.Name)
	gen := launchGens.Read(sid)
	if gen == "" {
		gen = "g1"
		launchGens.Write(sid, gen)
	}
	recordHook(name, gen, event, in, now)
}

func ptr(b bool) *bool { return &b }

func TestRecordHookNamesTheSlotByEnvName(t *testing.T) {
	m, sid := hookFixture(t, "slot-a")
	other := session.Meta{Dir: m.Dir, Name: "slot-b", Kind: session.KindAgy}
	session.WriteMeta(other)

	recordGen("slot-a", "SessionStart", hookInput{ConversationID: "c1"}, time.Now())
	if got := hookConversation(sid); got != "c1" {
		t.Fatalf("conversation = %q, want c1", got)
	}
	// Same cwd, different slot: must not see it.
	if got := hookConversation(session.UUID(other.Dir, other.Name)); got != "" {
		t.Fatalf("neighbour slot read %q", got)
	}
	// An agy the user runs by hand has no AF_SESSION_NAME; a non-agy session is not ours.
	recordHook("", "g1", "PreInvocation", hookInput{ConversationID: "c2"}, time.Now())
	session.WriteMeta(session.Meta{Dir: m.Dir, Name: "slot-c", Kind: session.KindClaude})
	recordGen("slot-c", "PreInvocation", hookInput{ConversationID: "c3"}, time.Now())
	if got := hookConversation(session.UUID(m.Dir, "slot-c")); got != "" {
		t.Fatalf("claude slot recorded %q", got)
	}
}

func TestHookStateMapping(t *testing.T) {
	for _, tc := range []struct {
		event string
		in    hookInput
		want  string
	}{
		{"PreInvocation", hookInput{}, "working"},
		{"PreToolUse", hookInput{}, "working"},
		{"Stop", hookInput{FullyIdle: ptr(true)}, "idle"},
		{"Stop", hookInput{}, "idle"},
		{"Stop", hookInput{FullyIdle: ptr(false)}, "working"},
		{"SessionStart", hookInput{}, ""},
		{"Bogus", hookInput{}, ""},
	} {
		if got := hookStateFor(tc.event, tc.in); got != tc.want {
			t.Errorf("%s %+v = %q, want %q", tc.event, tc.in, got, tc.want)
		}
	}
}

func TestCaptureConversationPrefersHook(t *testing.T) {
	m, sid := hookFixture(t, "slot-h")
	// The cwd map would attribute another conversation to this launch; the hook knows better.
	writeLastConversations(t, map[string]string{m.Dir: "map-uuid"})
	prelaunch.Write(sid, "")
	recordGen(m.Name, "SessionStart", hookInput{ConversationID: "hook-uuid"}, time.Now())
	captureConversation(m)
	if got := sids.Read(sid); got != "hook-uuid" {
		t.Fatalf("adopted %q, want hook-uuid", got)
	}
}

func TestLiveStateFromHook(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		event  string
		in     hookInput
		age    time.Duration
		footer bool
		adopt  string
		want   string
	}{
		{"working", "PreInvocation", hookInput{ConversationID: "c"}, time.Minute, false, "", "working"},
		{"stop is idle with no DB at all", "Stop", hookInput{ConversationID: "c", FullyIdle: ptr(true)}, time.Minute, false, "", "idle"},
		{"cancelled turn: idle footer beats a left-behind working", "PreInvocation", hookInput{ConversationID: "c"}, time.Minute, true, "", "idle"},
		{"stale working is withdrawn, DB has no opinion", "PreInvocation", hookInput{ConversationID: "c"}, 2 * time.Hour, false, "", ""},
		{"another conversation's record is ignored", "PreInvocation", hookInput{ConversationID: "c"}, time.Minute, false, "adopted-elsewhere", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sid := hookFixture(t, "slot-live")
			if tc.adopt != "" {
				sids.Write(sid, tc.adopt)
			}
			recordGen(m.Name, tc.event, tc.in, now.Add(-tc.age))
			p := probesAt(now)
			p.paneIdleSettled = func(string) bool { return tc.footer }
			if got := liveState(m, p); got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// A hook PreToolUse fires while the tool still waits for approval; the DB's pending prompt must
// keep winning so the permission card and chip survive.
func TestLiveStatePendingPromptBeatsHook(t *testing.T) {
	m, sid := hookFixture(t, "slot-perm")
	conv := "conv-perm"
	sids.Write(sid, conv)
	mkConvDB(t, conv, [][3]any{{132, stepStatusAwaitingUser, []byte("run_command")}})
	recordGen(m.Name, "PreToolUse", hookInput{ConversationID: conv}, time.Now())
	if got := liveState(m, probesAt(time.Now())); got != "permission" {
		t.Fatalf("state = %q, want permission", got)
	}
}

// With no hook record the DB path is unchanged.
func TestLiveStateFallsBackToDBWithoutHook(t *testing.T) {
	m, sid := hookFixture(t, "slot-nohook")
	conv := "conv-nohook"
	sids.Write(sid, conv)
	mkConvDB(t, conv, [][3]any{{14, stepStatusRunning, []byte("x")}})
	if got := liveState(m, probesAt(time.Now())); got != "working" {
		t.Fatalf("state = %q, want working", got)
	}
}

func TestClearHookRemovesRecord(t *testing.T) {
	m, sid := hookFixture(t, "slot-clear")
	recordGen(m.Name, "PreInvocation", hookInput{ConversationID: "c"}, time.Now())
	clearHook(sid)
	if _, ok := hookRecords.Read(sid); ok {
		t.Fatal("record survived clearHook")
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func TestEnsureHooksMergesByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	user := `{"mine":{"Stop":[{"type":"command","command":"echo hi"}]},"agent-fleet":{"Stop":[{"type":"command","command":"/old/path agy-hook Stop"}]}}`
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureHooksAt(path, "/opt/af agent"); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path)
	var wantMine any
	_ = json.Unmarshal([]byte(`{"Stop":[{"type":"command","command":"echo hi"}]}`), &wantMine)
	if !reflect.DeepEqual(got["mine"], wantMine) {
		t.Fatalf("user's hook changed: %v", got["mine"])
	}
	ours := got[hookName].(map[string]any)
	for _, ev := range []string{"SessionStart", "PreInvocation", "PreToolUse", "Stop"} {
		if _, ok := ours[ev]; !ok {
			t.Errorf("event %s missing", ev)
		}
	}
	stop := ours["Stop"].([]any)[0].(map[string]any)
	if stop["command"] != "'/opt/af agent' agy-hook Stop" {
		t.Errorf("stale command kept / unquoted: %v", stop["command"])
	}
	// The tool events are grouped, the others are not.
	if _, ok := ours["PreToolUse"].([]any)[0].(map[string]any)["hooks"]; !ok {
		t.Error("PreToolUse is not a {matcher, hooks} group")
	}

	// Idempotent: a second run writes nothing.
	fi, _ := os.Stat(path)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	if err := ensureHooksAt(path, "/opt/af agent"); err != nil {
		t.Fatal(err)
	}
	if fi2, _ := os.Stat(path); !fi2.ModTime().Equal(old) {
		t.Errorf("rewrote an up-to-date file (was %v)", fi.ModTime())
	}

	// A user edit to their own hooks survives the next launch; a deleted entry of ours returns.
	got["mine2"] = map[string]any{"Stop": []any{}}
	delete(got, hookName)
	b, _ := json.Marshal(got)
	_ = os.WriteFile(path, b, 0o600)
	if err := ensureHooksAt(path, "/opt/af agent"); err != nil {
		t.Fatal(err)
	}
	again := readJSON(t, path)
	if _, ok := again["mine2"]; !ok {
		t.Error("user's second hook lost")
	}
	if _, ok := again[hookName]; !ok {
		t.Error("our entry not restored")
	}
}

func TestEnsureHooksLeavesNonJSONAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	_ = os.WriteFile(path, []byte("// my notes\n{"), 0o600)
	if err := ensureHooksAt(path, "/af"); err == nil {
		t.Fatal("want an error for a file that is not JSON")
	}
	if b, _ := os.ReadFile(path); string(b) != "// my notes\n{" {
		t.Fatalf("file was modified: %q", b)
	}
}

func TestEnsureHooksKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "persist.json")
	_ = os.WriteFile(real, []byte(`{"mine":{}}`), 0o600)
	link := filepath.Join(dir, "hooks.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if err := ensureHooksAt(link, "/af"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	if _, ok := readJSON(t, real)[hookName]; !ok {
		t.Fatal("entry not written through the link")
	}
}

func TestHookRejectsForeignInput(t *testing.T) {
	m, sid := hookFixture(t, "slot-own")
	now := time.Now()
	recordGen(m.Name, "SessionStart", hookInput{ConversationID: "outer"}, now)
	recordGen(m.Name, "PreInvocation", hookInput{ConversationID: "outer"}, now)
	// An agy started by the model's run_command inherits the pane env: its events must not
	// replace the launch's conversation.
	recordGen(m.Name, "SessionStart", hookInput{ConversationID: "child"}, now)
	recordGen(m.Name, "Stop", hookInput{ConversationID: "child"}, now)
	if rec, _ := hookRecords.Read(sid); rec.ConvID != "outer" || rec.State != "working" {
		t.Fatalf("record = %+v", rec)
	}
	// A payload without a conversation (malformed stdin) records nothing.
	recordGen(m.Name, "Stop", hookInput{}, now)
	if rec, _ := hookRecords.Read(sid); rec.State != "working" {
		t.Fatalf("empty-conv Stop overwrote: %+v", rec)
	}
	// A hook from before a restart carries the old generation.
	recordHook(m.Name, "stale-gen", "Stop", hookInput{ConversationID: "outer"}, now)
	if rec, _ := hookRecords.Read(sid); rec.State != "working" {
		t.Fatalf("stale generation accepted: %+v", rec)
	}
	// A resumed slot's UUID is fixed: another conversation is refused even on a fresh record.
	clearHook(sid)
	sids.Write(sid, "resumed")
	recordGen(m.Name, "SessionStart", hookInput{ConversationID: "other"}, now)
	if _, ok := hookRecords.Read(sid); ok {
		t.Fatal("recorded a conversation other than the resumed one")
	}
}

func TestRunHookMalformedStdinRecordsNothing(t *testing.T) {
	m, sid := hookFixture(t, "slot-bad")
	recordGen(m.Name, "PreInvocation", hookInput{ConversationID: "c"}, time.Now())
	t.Setenv("AF_SESSION_NAME", m.Name)
	t.Setenv(launchEnvVar, launchGens.Read(sid))
	r, w, _ := os.Pipe()
	_, _ = w.WriteString("{not json")
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	RunHook([]string{"Stop"})
	if rec, _ := hookRecords.Read(sid); rec.State != "working" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestStaleIdleWithdrawnByNewerDBActivity(t *testing.T) {
	m, sid := hookFixture(t, "slot-stale-idle")
	conv := "conv-si"
	sids.Write(sid, conv)
	mkConvDB(t, conv, [][3]any{{14, stepStatusRunning, []byte("x")}})
	recordGen(m.Name, "Stop", hookInput{ConversationID: conv, FullyIdle: ptr(true)}, time.Now().Add(-time.Minute))
	if got := liveState(m, probesAt(time.Now())); got != "working" {
		t.Fatalf("state = %q, want working (DB is newer than the Stop)", got)
	}
}

func TestEnsureHooksDanglingSymlinkAndLinkedDir(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "hooks.json")
	if err := os.Symlink(filepath.Join(dir, "gone.json"), link); err != nil {
		t.Skip(err)
	}
	if err := ensureHooksAt(link, "/af"); err == nil {
		t.Fatal("want an error for a dangling link")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dangling link was replaced")
	}
	// A linked config directory with no hooks.json yet: the file is created in the real directory.
	real := filepath.Join(dir, "real")
	_ = os.MkdirAll(real, 0o700)
	cfg := filepath.Join(dir, "config")
	_ = os.Symlink(real, cfg)
	if err := ensureHooksAt(filepath.Join(cfg, "hooks.json"), "/af"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(cfg); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config dir link replaced")
	}
	if _, ok := readJSON(t, filepath.Join(real, "hooks.json"))[hookName]; !ok {
		t.Fatal("entry not created in the real directory")
	}
}

func TestEnsureHooksKeepsForeignBytesAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	_ = os.WriteFile(path, []byte(`{"mine":{"big":9007199254740993}}`), 0o640)
	_ = os.Chmod(path, 0o640)
	if err := ensureHooksAt(path, "/af"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "9007199254740993") {
		t.Fatalf("foreign number rewritten: %s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestEnsureHooksConcurrentWritersLeaveValidJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "hooks.json")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = ensureHooksAt(path, "/af") }()
	}
	wg.Wait()
	if _, ok := readJSON(t, path)[hookName]; !ok {
		t.Fatal("entry missing")
	}
}
