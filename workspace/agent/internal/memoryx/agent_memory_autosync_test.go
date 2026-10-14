package memoryx

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/claude"
)

// autoEnv is the export environment plus the hooks the reconciler is wired with. Everything
// lives under a temp HOME / CLAUDE_CONFIG_DIR.
type autoEnv struct {
	*claudeExportEnv
	want       bool
	live       []string
	codexVal   bool
	codexFound bool
	clock      time.Time
}

func newAutoEnv(t *testing.T) *autoEnv {
	e := &autoEnv{claudeExportEnv: newClaudeExportEnv(t), clock: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	oldWant, oldLive, oldFlag, oldSet, oldNotify, oldNow, oldChunk :=
		AutoSyncWant, AutoSyncLiveClaudeSessions, AutoSyncCodexFlag, AutoSyncCodexSet, AutoSyncNotify, autoNow, autoImportChunk
	AutoSyncWant = func() bool { return e.want }
	AutoSyncLiveClaudeSessions = func() []string { return e.live }
	AutoSyncCodexFlag = func() (bool, bool) { return e.codexVal, e.codexFound }
	AutoSyncCodexSet = func(on bool) error { e.codexVal, e.codexFound = on, true; return nil }
	autoNow = func() time.Time { return e.clock }
	t.Cleanup(func() {
		AutoSyncWant, AutoSyncLiveClaudeSessions, AutoSyncCodexFlag, AutoSyncCodexSet, AutoSyncNotify, autoNow, autoImportChunk =
			oldWant, oldLive, oldFlag, oldSet, oldNotify, oldNow, oldChunk
		autoChunkHook = nil
		agentMemExportTestHook = nil
	})
	autoAdopt()
	return e
}

// flip is the ui-prefs PUT: the switch changes, the phase moves at once, the work follows.
func (e *autoEnv) flip(on bool) {
	e.t.Helper()
	e.want = on
	AutoSyncWake()
	e.drain()
}

// drain runs the loop until it has nothing to do now.
func (e *autoEnv) drain() {
	e.t.Helper()
	for i := 0; i < 20; i++ {
		if autoSyncStep() > 0 {
			return
		}
	}
	e.t.Fatal("the reconciler never settled")
}

func (e *autoEnv) phase() string { return autoStatus().Phase }

// otherProject makes a second git working copy under ~/repos and returns its project id and
// claude directory slug.
func (e *autoEnv) otherProject(name string) (pid, slug string) {
	e.t.Helper()
	dir := filepath.Join(e.home, "repos", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet", "-b", "main"}, {"commit", "--quiet", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return agentMemProjectFor(dir).ID, claude.ProjectKey(dir)
}

func (e *autoEnv) nativeNames(slug string) []string {
	ents, _ := os.ReadDir(e.memDir(slug))
	var out []string
	for _, d := range ents {
		if n, ok := strings.CutSuffix(d.Name(), ".md"); ok && n != "MEMORY" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// afNames lists the live AF memories of a project.
func afNames(t *testing.T, pid string) []string {
	t.Helper()
	agentMemMu.RLock()
	defer agentMemMu.RUnlock()
	es, _, err := agentMemLoadDir(agentMemScopeProject, "projects/"+pid)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func (e *autoEnv) writeNative(slug, name, desc, body string) {
	e.t.Helper()
	e.raw(slug, name+".md", "---\nname: "+name+"\ndescription: "+desc+"\nmetadata:\n  type: project\n---\n\n"+body+"\n")
}

// Switching on imports every mapped claude project with no manual step, with the counts the
// confirmation announced; switching off writes AF memory back; codex's flag goes false and
// comes back to the member's own value.
func TestAutoSyncOnImportsAndOffWritesBack(t *testing.T) {
	e := newAutoEnv(t)
	pid2, slug2 := e.otherProject("second")
	e.file("one", "first", "project", "body one")
	e.file("two", "second", "feedback", "body two")
	e.writeNative(slug2, "three", "third", "body three")
	e.raw("-home-dev-repos-nowhere", "lost.md", "---\nname: lost\ndescription: no project\n---\nbody\n")
	e.codexVal, e.codexFound = true, true

	pv, err := autoPreview("on")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Apply != 3 || pv.Unmapped != 2 {
		t.Fatalf("preview = %+v, want 3 to apply and 2 unmapped (the seeded directory and the one with no project)", pv)
	}

	e.flip(true)
	st := autoStatus()
	if st.Phase != autoPhaseOn || !st.Settled || !st.NativeMemoryOff {
		t.Fatalf("status after switch-on = %+v", st)
	}
	if !eq(afNames(t, e.pid), []string{"one", "two"}) || !eq(afNames(t, pid2), []string{"three"}) {
		t.Fatalf("AF after import: %v %v", afNames(t, e.pid), afNames(t, pid2))
	}
	if st.Run == nil || st.Run.Kind != autoRunImport || !st.Run.Complete || st.Run.Unmapped != 2 {
		t.Fatalf("run = %+v", st.Run)
	}
	if e.codexVal {
		t.Error("codex features.memories should be off while AF memory is on")
	}

	// Something only AF learned, then the switch goes off.
	e.save("only-af", "learned in AF", "project", "af body")
	e.flip(false)
	st = autoStatus()
	if st.Phase != autoPhaseOff || !st.Settled || st.NativeMemoryOff {
		t.Fatalf("status after switch-off = %+v", st)
	}
	if !eq(e.nativeNames(e.slug), []string{"one", "only-af", "two"}) {
		t.Errorf("claude's memory after the write-back: %v", e.nativeNames(e.slug))
	}
	if st.Run.Kind != autoRunWriteBack || !st.Run.Complete {
		t.Errorf("run = %+v", st.Run)
	}
	for _, p := range st.Run.Projects {
		if p.Counts["conflict"] != 0 {
			t.Errorf("conflicts in %+v", p)
		}
	}
	infos, err := memoryListSnapshots(20, "")
	if err != nil {
		t.Fatal(err)
	}
	snap := false
	for _, s := range infos {
		snap = snap || s.Trigger == memoryTriggerPreExport
	}
	if !snap {
		t.Error("the write-back took no pre-export snapshot")
	}
	if !e.codexVal || !e.codexFound {
		t.Error("codex features.memories was not restored to the member's own value")
	}
	if got, _ := os.ReadFile(autoStatePath()); strings.Contains(string(got), "body one") {
		t.Error("the state file carries memory text")
	}
}

// On → off → on → off converges: nothing duplicated, nothing resurrected, no conflicts.
func TestAutoSyncRoundTripConverges(t *testing.T) {
	e := newAutoEnv(t)
	e.file("keep", "kept", "project", "keep body")
	e.file("doomed", "will be forgotten", "project", "doomed body")
	e.flip(true)
	if _, err := agentMemForget(e.c, agentMemForgetReq{Name: "doomed", Revision: 1}, e.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	e.save("fresh", "learned later", "project", "fresh body")
	for i := 0; i < 2; i++ {
		e.flip(false)
		if st := autoStatus(); st.Run.Complete == false || len(st.Run.Rows) != 0 {
			t.Fatalf("round %d: write-back left rows: %+v", i, st.Run)
		}
		e.flip(true)
		if st := autoStatus(); !st.Run.Complete || len(st.Run.Rows) != 0 {
			t.Fatalf("round %d: import left rows: %+v", i, st.Run)
		}
		if got := afNames(t, e.pid); !eq(got, []string{"fresh", "keep"}) {
			t.Fatalf("round %d: AF holds %v", i, got)
		}
	}
	e.flip(false)
	// "doomed" was imported from claude, not written by AF, so the write-back leaves the native
	// file alone: a native file AF did not write is never deleted.
	if got := e.nativeNames(e.slug); !eq(got, []string{"doomed", "fresh", "keep"}) {
		t.Errorf("claude holds %v", got)
	}
}

// A forgotten memory whose native copy is still in claude's directory is not brought back.
func TestAutoSyncNeverResurrectsAForgottenMemory(t *testing.T) {
	e := newAutoEnv(t)
	e.file("doomed", "will be forgotten", "project", "doomed body")
	e.flip(true)
	if _, err := agentMemForget(e.c, agentMemForgetReq{Name: "doomed", Revision: 1}, e.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The claude file is still there (a session that launched before the switch kept it); the
	// catch-up import sees it again.
	e.live = []string{"claude-main"}
	e.want = false
	e.want = true
	autoUpdate(func(st *autoSyncState) { st.CatchUp = []string{"claude-main"} })
	e.live = nil
	e.drain()
	if got := afNames(t, e.pid); len(got) != 0 {
		t.Fatalf("a forgotten memory came back: %v", got)
	}
	if st := autoStatus(); st.Run.Kind != autoRunCatchUp || st.Run.Projects[0].Counts["forgotten"] != 1 {
		t.Fatalf("run = %+v", st.Run)
	}
}

// The automatic import reports an AF-edited copy as a conflict and leaves it; the member's own
// import still replaces it.
func TestAutoSyncImportReportsAnAFEditedCopyAsConflict(t *testing.T) {
	e := newAutoEnv(t)
	e.writeNative(e.slug, "shared", "from claude", "claude v1")
	e.flip(true)
	e.save("shared", "edited in AF", "project", "af edit")
	later := time.Now().Add(time.Hour)
	e.writeNative(e.slug, "shared", "claude again", "claude v2")
	if err := os.Chtimes(filepath.Join(e.memDir(e.slug), "shared.md"), later, later); err != nil {
		t.Fatal(err)
	}

	e.live = nil
	autoUpdate(func(st *autoSyncState) { st.CatchUp = []string{"gone"} })
	e.drain()
	st := autoStatus()
	if st.Run.Kind != autoRunCatchUp || st.Run.Projects[0].Counts["conflict"] != 1 || len(st.Run.Rows) != 1 || st.Run.Rows[0].Reason != "af_changed" {
		t.Fatalf("run = %+v", st.Run)
	}
	got, err := agentMemRead(e.c, "", "shared")
	if err != nil || got.Body != "af edit" {
		t.Fatalf("the AF edit was replaced: %+v %v", got, err)
	}
	// The manual import keeps today's behaviour: the member's choice.
	if it := e.item(e.claudeImportEnv.preview(), "shared"); it.Status != claudeImportUpdate {
		t.Errorf("manual preview = %s, want update", it.Status)
	}
	// A copy that is still the import's own is updated unattended.
	e.writeNative(e.slug, "pristine", "p", "v1")
	e.live = nil
	autoUpdate(func(st *autoSyncState) { st.CatchUp = []string{"gone"} })
	e.drain()
	e.clock = e.clock.Add(time.Hour)
	e.writeNative(e.slug, "pristine", "p", "v2")
	later = later.Add(time.Hour)
	_ = os.Chtimes(filepath.Join(e.memDir(e.slug), "pristine.md"), later, later)
	autoUpdate(func(st *autoSyncState) { st.CatchUp = []string{"gone"} })
	e.drain()
	if got, _ := agentMemRead(e.c, "", "pristine"); got.Body != "v2" {
		t.Errorf("pristine = %q, want v2", got.Body)
	}
}

// A session that launched before the switch writes after the import; the catch-up brings it in
// once the last such session has ended, and not before.
func TestAutoSyncCatchUpWaitsForTheLastEarlierSession(t *testing.T) {
	e := newAutoEnv(t)
	e.file("early", "early", "project", "early body")
	e.live = []string{"claude-main", "claude-other"}
	e.flip(true)
	if st := autoStatus(); st.CatchUpPending != 2 {
		t.Fatalf("catch-up pending = %d, want 2", st.CatchUpPending)
	}
	e.file("late", "written after the import", "project", "late body")
	e.live = []string{"claude-other"}
	e.drain()
	if got := afNames(t, e.pid); !eq(got, []string{"early"}) {
		t.Fatalf("imported before the last session ended: %v", got)
	}
	e.live = nil
	e.drain()
	if got := afNames(t, e.pid); !eq(got, []string{"early", "late"}) {
		t.Fatalf("catch-up missed it: %v", got)
	}
	if st := autoStatus(); st.CatchUpPending != 0 || st.Run.Kind != autoRunCatchUp {
		t.Fatalf("status = %+v", st)
	}
}

// Native memory stays off from the flip until the write-back has finished.
func TestAutoSyncNativeMemoryFollowsThePhase(t *testing.T) {
	e := newAutoEnv(t)
	e.file("one", "one", "project", "body")
	if AutoSyncNativeMemoryOff() {
		t.Fatal("native memory should be on before the switch")
	}
	e.want = true
	AutoSyncWake() // the PUT: the phase moves before anything runs
	if !AutoSyncNativeMemoryOff() || e.phase() != autoPhaseImporting {
		t.Fatalf("right after the flip: phase %s", e.phase())
	}
	e.drain()
	e.want = false
	AutoSyncWake()
	if !AutoSyncNativeMemoryOff() || e.phase() != autoPhaseWritingBack {
		t.Fatalf("switch off, write-back not run yet: phase %s, off=%v", e.phase(), AutoSyncNativeMemoryOff())
	}
	e.drain()
	if AutoSyncNativeMemoryOff() {
		t.Error("native memory should resume once the write-back is done")
	}
}

// A failed write-back keeps native memory off and retries with backoff; the member can switch
// it back on anyway. The failure is the snapshot the write-back takes first: with the history
// unreadable nothing is written.
func TestAutoSyncWriteBackFailureRetriesAndCanBeAbandoned(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	e := newAutoEnv(t)
	e.file("one", "one", "project", "body")
	e.flip(true)
	e.save("only-af", "af", "project", "af body")
	block := func(on bool) {
		mode := os.FileMode(0o700)
		if on {
			mode = 0
		}
		if err := os.Chmod(memoryRepoDir(), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(memoryRepoDir(), 0o700) })
	block(true)
	e.want = false
	AutoSyncWake()
	e.drain()
	st := autoStatus()
	if st.Phase != autoPhaseWritingBack || st.Attempts != 1 || st.NextAttempt == "" || !st.NativeMemoryOff || st.Run.Error == "" {
		t.Fatalf("after a failed write-back: %+v", st)
	}
	if len(e.nativeNames(e.slug)) != 1 {
		t.Errorf("a file was written although the snapshot failed: %v", e.nativeNames(e.slug))
	}
	// Not due yet: nothing runs, native memory stays off.
	if wait := autoSyncStep(); wait <= 0 || autoStatus().Attempts != 1 {
		t.Fatalf("a retry ran before it was due (wait %v)", wait)
	}
	for i := 0; i < 2; i++ {
		e.clock = e.clock.Add(autoBackoff[len(autoBackoff)-1] + time.Second)
		e.drain()
	}
	if st := autoStatus(); st.Attempts != 3 || !st.Failing || !st.NativeMemoryOff {
		t.Fatalf("after three failures: %+v", st)
	}
	if err := autoAbandon(); err != nil {
		t.Fatal(err)
	}
	if st := autoStatus(); st.Phase != autoPhaseOff || st.NativeMemoryOff || !st.NativeBackOn {
		t.Fatalf("after switching native memory back on anyway: %+v", st)
	}

	// The same failure, then the cause goes away before the retry: it succeeds.
	block(false)
	e.flip(true)
	e.save("second", "af", "project", "second body")
	block(true)
	e.want = false
	AutoSyncWake()
	e.drain()
	if st := autoStatus(); st.Phase != autoPhaseWritingBack || st.Attempts != 1 {
		t.Fatalf("status = %+v", st)
	}
	block(false)
	e.clock = e.clock.Add(time.Hour)
	e.drain()
	if st := autoStatus(); st.Phase != autoPhaseOff || st.NativeMemoryOff {
		t.Fatalf("the retry did not finish the write-back: %+v", st)
	}
	if !eq(e.nativeNames(e.slug), []string{"one", "only-af", "second"}) {
		t.Errorf("claude holds %v", e.nativeNames(e.slug))
	}
}

// The overwrite never happens, even when the conflict is the whole reason for the retry.
func TestAutoSyncWriteBackNeverOverwrites(t *testing.T) {
	e := newAutoEnv(t)
	e.save("clash", "af text", "project", "af body")
	e.writeNative(e.slug, "clash", "theirs", "member body")
	e.want = true
	autoUpdate(func(st *autoSyncState) { st.Phase = autoPhaseOn })
	e.flip(false)
	st := autoStatus()
	if st.Phase != autoPhaseOff || len(st.Run.Rows) != 1 || st.Run.Rows[0].Status != claudeExportConflict {
		t.Fatalf("status = %+v", st)
	}
	if got, _ := os.ReadFile(filepath.Join(e.memDir(e.slug), "clash.md")); !strings.Contains(string(got), "member body") {
		t.Errorf("a conflict was overwritten: %s", got)
	}
}

// Killed in the middle of an import: the phase is on disk, and the next step finishes the job.
func TestAutoSyncResumesAnInterruptedImport(t *testing.T) {
	e := newAutoEnv(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		e.file(n, n, "project", "body "+n)
	}
	autoImportChunk = 1
	chunks := 0
	autoChunkHook = func() {
		chunks++
		if chunks == 3 {
			panic("the agent was killed") // stands for the process dying: only the state file survives
		}
	}
	e.want = true
	AutoSyncWake()
	func() {
		defer func() { _ = recover() }()
		autoSyncStep()
	}()
	autoChunkHook = nil
	if n := len(afNames(t, e.pid)); n != 2 {
		t.Fatalf("%d memories after the kill, want 2", n)
	}
	if st := autoStatus(); st.Phase != autoPhaseImporting || !st.NativeMemoryOff {
		t.Fatalf("status after the kill = %+v", st)
	}
	e.drain() // the restarted Agent
	if got := afNames(t, e.pid); !eq(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("after the resume: %v", got)
	}
	if st := autoStatus(); st.Phase != autoPhaseOn || !st.Run.Complete {
		t.Fatalf("status = %+v", st)
	}
}

// Killed in the middle of a write-back: the other project is written on the resume.
func TestAutoSyncResumesAnInterruptedWriteBack(t *testing.T) {
	e := newAutoEnv(t)
	pid2, slug2 := e.otherProject("second")
	e.writeNative(slug2, "seed", "seed", "imports record the project's root")
	e.flip(true)
	e.save("one", "one", "project", "body one")
	c2 := agentMemCallerT(t, "claude-main")
	c2.Project = agentMemProjectInfo(pid2)
	if _, err := agentMemSave(c2, agentMemSaveReq{Name: "two", Description: "two", Type: "project", Body: "body two"}, e.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	writes := 0
	agentMemExportTestHook = func(stage string) {
		if stage == "before-write" {
			writes++
			if writes == 2 {
				panic("the agent was killed")
			}
		}
	}
	e.want = false
	AutoSyncWake()
	func() {
		defer func() { _ = recover() }()
		autoSyncStep()
	}()
	agentMemExportTestHook = nil
	if st := autoStatus(); st.Phase != autoPhaseWritingBack || !st.NativeMemoryOff {
		t.Fatalf("status after the kill = %+v", st)
	}
	e.drain()
	if st := autoStatus(); st.Phase != autoPhaseOff {
		t.Fatalf("status = %+v", st)
	}
	if !eq(e.nativeNames(e.slug), []string{"one"}) || !eq(e.nativeNames(slug2), []string{"seed", "two"}) {
		t.Fatalf("claude holds %v and %v", e.nativeNames(e.slug), e.nativeNames(slug2))
	}
}

// Flipping back while an import runs turns the phase around: what was imported stays, and the
// write-back finishes the job in the other direction.
func TestAutoSyncFlipBackDuringImport(t *testing.T) {
	e := newAutoEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.file(n, n, "project", "body "+n)
	}
	autoImportChunk = 1
	n := 0
	autoChunkHook = func() {
		n++
		if n == 2 {
			e.want = false
		}
	}
	e.want = true
	AutoSyncWake()
	e.drain()
	autoChunkHook = nil
	if st := autoStatus(); st.Phase != autoPhaseOff || !st.Settled {
		t.Fatalf("status = %+v", st)
	}
	if got := e.nativeNames(e.slug); !eq(got, []string{"a", "b", "c"}) {
		t.Errorf("claude lost files: %v", got)
	}
	e.flip(true)
	if got := afNames(t, e.pid); !eq(got, []string{"a", "b", "c"}) {
		t.Errorf("AF after switching on again: %v", got)
	}
}

// The codex flag is recorded when AF memory goes on and restored when it goes off, never forced
// on; the member's own edit while it is on replaces the recorded value.
func TestAutoSyncCodexFlag(t *testing.T) {
	t.Run("off stays off", func(t *testing.T) {
		e := newAutoEnv(t)
		e.flip(true)
		e.flip(false)
		if e.codexFound || e.codexVal {
			t.Errorf("a codex flag that was absent was written: %v %v", e.codexVal, e.codexFound)
		}
	})
	t.Run("restored", func(t *testing.T) {
		e := newAutoEnv(t)
		e.codexVal, e.codexFound = true, true
		e.flip(true)
		if e.codexVal {
			t.Fatal("not switched off")
		}
		e.flip(false)
		if !e.codexVal {
			t.Fatal("not restored")
		}
	})
	t.Run("the member turns it off while AF memory is on", func(t *testing.T) {
		e := newAutoEnv(t)
		e.codexVal, e.codexFound = true, true
		e.flip(true)
		e.codexVal = true // the member turns codex memories on…
		e.drain()
		e.codexVal = false // …and off again: their last value is the one that counts
		e.drain()
		e.flip(false)
		if e.codexVal {
			t.Error("the flag was forced on although the member's last value was off")
		}
	})
	t.Run("the member turns it on while AF memory is on", func(t *testing.T) {
		e := newAutoEnv(t)
		e.codexVal, e.codexFound = true, true
		e.flip(true)
		e.codexVal = true
		e.drain()
		e.flip(false)
		if !e.codexVal {
			t.Error("the member's value was lost")
		}
	})
}

// A restart with the switch already on and no state adopts `on`; a flip on an Agent with no state
// imports.
func TestAutoSyncAdoptsAnExistingSwitchOnlyAtStart(t *testing.T) {
	e := newAutoEnv(t)
	e.file("one", "one", "project", "body")
	_ = os.Remove(autoStatePath())
	e.want = true
	autoAdopt()
	if st := autoStatus(); st.Phase != autoPhaseOn {
		t.Fatalf("adopted phase = %s", st.Phase)
	}
	e.drain()
	if len(afNames(t, e.pid)) != 0 {
		t.Error("an Agent that already had the switch on imported unasked")
	}
	_ = os.Remove(autoStatePath())
	e.want = false
	autoAdopt()
	e.flip(true)
	if len(afNames(t, e.pid)) != 1 {
		t.Error("a flip with no recorded state did not import")
	}
}

// The notification fires once for a run that left rows, and the record carries no text.
func TestAutoSyncNotifiesOnceAndNeverCarriesText(t *testing.T) {
	e := newAutoEnv(t)
	var got []AutoSyncRun
	AutoSyncNotify = func(r AutoSyncRun) { got = append(got, r) }
	key := "ghp_" + strings.Repeat("q7Zx", 9)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	e.raw(e.slug, "leaky.md", "---\nname: leaky\ndescription: has a key\n---\ntoken is "+key+"\n")
	e.file("fine", "fine", "project", "body")
	e.flip(true)
	e.drain()
	if len(got) != 1 || got[0].Rows[0].Status != claudeImportSecret || len(got[0].Rows[0].Rules) == 0 {
		t.Fatalf("notifications = %+v", got)
	}
	b, _ := os.ReadFile(autoStatePath())
	if strings.Contains(string(b), key) || strings.Contains(logs.String(), key) {
		t.Errorf("a secret reached the state file or the log: %s %s", b, logs.String())
	}
}

// The routes: the status carries no memory text, a preview of either direction counts without
// writing, and the actions refuse what does not apply.
func TestAutoSyncHandlers(t *testing.T) {
	e := newAutoEnv(t)
	e.file("one", "one", "project", "SECRET-LOOKING-BODY-TEXT")
	do := func(h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
		return rec
	}
	rec := do(HandleAutoSyncPreview, "GET", "/agents/memory/auto-sync/preview?to=on", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"apply":1`) {
		t.Fatalf("preview on = %d %s", rec.Code, rec.Body)
	}
	if len(afNames(t, e.pid)) != 0 {
		t.Fatal("a preview wrote")
	}
	if rec := do(HandleAutoSyncPreview, "GET", "/agents/memory/auto-sync/preview?to=sideways", ""); rec.Code != 400 {
		t.Errorf("bad direction = %d", rec.Code)
	}
	if rec := do(HandleAutoSyncAction, "POST", "/agents/memory/auto-sync", `{"action":"explode"}`); rec.Code != 400 {
		t.Errorf("unknown action = %d", rec.Code)
	}
	if rec := do(HandleAutoSyncAction, "POST", "/agents/memory/auto-sync", `{"action":"abandon"}`); rec.Code != 409 {
		t.Errorf("abandon with nothing pending = %d", rec.Code)
	}
	e.flip(true)
	rec = do(HandleAutoSyncStatus, "GET", "/agents/memory/auto-sync", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"phase":"on"`) || strings.Contains(rec.Body.String(), "SECRET-LOOKING") {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if rec := do(HandleAutoSyncAction, "POST", "/agents/memory/auto-sync", `{"action":"abandon"}`); rec.Code != 409 {
		t.Errorf("abandon while the switch is on = %d", rec.Code)
	}
	if rec := do(HandleAutoSyncAction, "POST", "/agents/memory/auto-sync", `{"action":"wake"}`); rec.Code != 200 {
		t.Errorf("wake = %d", rec.Code)
	}
}

// blockStateSave makes the state file's temp name a directory, so every save fails.
func blockStateSave(t *testing.T, on bool) {
	t.Helper()
	tmp := autoStatePath() + ".tmp"
	if on {
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
		return
	}
	_ = os.RemoveAll(tmp)
}

func diskPhase(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(autoStatePath())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if json.Unmarshal(b, &m) != nil {
		t.Fatalf("state file is not JSON: %s", b)
	}
	p, _ := m["phase"].(string)
	return p
}

// A state that cannot be saved starts nothing: no import runs against a phase that is not on
// disk, and native memory stays off, in both directions.
func TestAutoSyncStateSaveFailureStartsNothing(t *testing.T) {
	e := newAutoEnv(t)
	e.file("one", "one", "project", "body")
	blockStateSave(t, true)
	e.want = true
	AutoSyncWake()
	e.drain()
	if len(afNames(t, e.pid)) != 0 {
		t.Fatal("an import ran although the phase could not be saved")
	}
	if diskPhase(t) != autoPhaseOff || !AutoSyncNativeMemoryOff() || !strings.Contains(strings.Join(autoStatus().Problems, ","), autoProblemNotSaved) {
		t.Fatalf("status = %+v", autoStatus())
	}
	blockStateSave(t, false)
	e.drain()
	if got := afNames(t, e.pid); !eq(got, []string{"one"}) || autoStatus().Phase != autoPhaseOn {
		t.Fatalf("after the save works again: %v %+v", got, autoStatus())
	}

	// Switching off while saves fail: the durable phase is still `on`, so the write-back is not
	// skipped and native memory does not resume.
	e.save("only-af", "af", "project", "af body")
	blockStateSave(t, true)
	e.want = false
	AutoSyncWake()
	e.drain()
	if diskPhase(t) != autoPhaseOn || !AutoSyncNativeMemoryOff() {
		t.Fatalf("phase %s, native off %v", diskPhase(t), AutoSyncNativeMemoryOff())
	}
	blockStateSave(t, false)
	e.drain()
	if st := autoStatus(); st.Phase != autoPhaseOff || !eq(e.nativeNames(e.slug), []string{"one", "only-af"}) {
		t.Fatalf("status %+v, claude holds %v", st, e.nativeNames(e.slug))
	}
}

// A state file that exists but cannot be read is not a first start: nothing is adopted over it,
// native memory stays off, and only the member's choice replaces it.
func TestAutoSyncUnreadableStateIsNotAdopted(t *testing.T) {
	e := newAutoEnv(t)
	e.file("one", "one", "project", "body")
	e.flip(true)
	e.save("only-af", "af", "project", "af body")
	e.want = false
	AutoSyncWake() // writing_back is on disk, the write-back has not run
	if diskPhase(t) != autoPhaseWritingBack {
		t.Fatal(diskPhase(t))
	}
	if err := os.WriteFile(autoStatePath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	autoAdopt() // the restart
	if b, _ := os.ReadFile(autoStatePath()); string(b) != "{broken" {
		t.Fatalf("the unreadable state was overwritten: %s", b)
	}
	e.drain()
	st := autoStatus()
	if st.Phase != "unreadable" || !st.Failing || !st.NativeMemoryOff || !AutoSyncNativeMemoryOff() {
		t.Fatalf("status = %+v", st)
	}
	if len(e.nativeNames(e.slug)) != 1 {
		t.Fatal("something was written while the phase was unknown")
	}
	if err := autoAbandon(); err != nil {
		t.Fatal(err)
	}
	if st := autoStatus(); st.Phase != autoPhaseOff || st.NativeMemoryOff || !st.NativeBackOn {
		t.Fatalf("after giving up: %+v", st)
	}
}

// off → on → off (write-back not run yet) → on: the sessions that launched with native memory on
// are still owed their catch-up.
func TestAutoSyncCatchUpSurvivesAFlipBackBeforeTheWriteBack(t *testing.T) {
	e := newAutoEnv(t)
	e.file("early", "early", "project", "early body")
	e.live = []string{"claude-main"}
	e.flip(true)
	e.want = false
	AutoSyncWake() // writing_back, not run
	e.flip(true)
	if st := autoStatus(); st.Phase != autoPhaseOn || st.CatchUpPending != 1 {
		t.Fatalf("status = %+v", st)
	}
	e.file("late", "written by the early session", "project", "late body")
	e.live = nil
	e.drain()
	if got := afNames(t, e.pid); !eq(got, []string{"early", "late"}) {
		t.Fatalf("the catch-up was lost: %v", got)
	}
}

// The codex flag is retried until it takes, in a stable phase too, and an Agent that adopts a
// switch that was already on switches it off.
func TestAutoSyncCodexFlagIsRetriedAndAdopted(t *testing.T) {
	e := newAutoEnv(t)
	e.codexVal, e.codexFound = true, true
	failing := true
	AutoSyncCodexSet = func(on bool) error {
		if failing {
			return os.ErrPermission
		}
		e.codexVal, e.codexFound = on, true
		return nil
	}
	e.flip(true)
	if !e.codexVal || !strings.Contains(strings.Join(autoStatus().Problems, ","), autoProblemCodex) {
		t.Fatalf("the failed write was not reported: %+v", autoStatus())
	}
	failing = false
	e.drain()
	if e.codexVal || len(autoStatus().Problems) != 0 {
		t.Fatalf("switch-on write never retried: flag %v, %+v", e.codexVal, autoStatus())
	}
	failing = true
	e.flip(false)
	if e.codexVal || autoStatus().Phase != autoPhaseOff {
		t.Fatalf("flag %v phase %s", e.codexVal, autoStatus().Phase)
	}
	failing = false
	e.drain()
	if !e.codexVal {
		t.Fatal("the restore never retried in the stable `off` phase")
	}

	// Adoption of a switch that was already on.
	e.codexVal, e.codexFound = true, true
	_ = os.Remove(autoStatePath())
	e.want = true
	autoAdopt()
	e.drain()
	if e.codexVal {
		t.Error("an adopted switch left codex's memories on")
	}
}

// The member turns the flag on, then off again, and AF memory goes off before the next poll: the
// last edit wins over the recorded value.
func TestAutoSyncCodexMemberEditJustBeforeOff(t *testing.T) {
	e := newAutoEnv(t)
	e.codexVal, e.codexFound = true, true
	e.flip(true)
	e.codexVal = true
	e.drain() // a poll sees it
	e.codexVal = false
	e.want = false
	AutoSyncWake() // straight into the write-back, no poll in between
	e.drain()
	if e.codexVal {
		t.Error("the restore overwrote the member's last edit")
	}
}
