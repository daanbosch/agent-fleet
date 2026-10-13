package memoryx

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	purgeOldText = "The customer is Hollowbrook Dairy and their contact line is kept private."
	purgeMidText = "Second draft naming Hollowbrook Dairy once more."
	purgeNewText = "Current text with nothing to hide."
)

// purgeFixture saves a (three revisions, the first two with the text to purge) and b (two
// revisions), takes a manual snapshot and parks an imported lineage on the first revision of a.
type purgeFixture struct {
	c         agentMemCaller
	pid       string
	oldBlob   string
	oldCommit string
	bBlobs    []string
}

func purgeSetup(t *testing.T) purgeFixture {
	t.Helper()
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	f := purgeFixture{c: c, pid: c.Project.ID}
	save := func(name, body string, rev int) {
		t.Helper()
		if _, err := agentMemSave(c, agentMemSaveReq{Name: name, Description: "d", Body: body, Revision: rev}, now); err != nil {
			t.Fatal(err)
		}
	}
	save("a", purgeOldText, 0)
	f.oldBlob, _ = memoryGitRun("rev-parse", memoryBranch+":af/projects/"+f.pid+"/a.md")
	f.oldCommit, _ = memoryGitRun("rev-parse", memoryBranch)
	save("b", "b one", 0)
	save("a", purgeMidText, 1)
	save("b", "b two", 1)
	for _, ref := range []string{"main"} {
		_ = ref
	}
	if _, err := memorySnapshot(memoryTriggerManual, now); err != nil {
		t.Fatal(err)
	}
	save("a", purgeNewText, 2)
	// An imported lineage and a rescue ref that still hold the old commits.
	if _, err := memoryGitRun("update-ref", "refs/imports/20261004T090000Z/main", f.oldCommit); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryGitRun("update-ref", "refs/premigrate/20261004T090000Z", f.oldCommit); err != nil {
		t.Fatal(err)
	}
	return f
}

func purgeDo(t *testing.T, f purgeFixture, name string) (agentMemPurgeResult, error) {
	t.Helper()
	pv, err := agentMemPurgePreview(agentMemScopeProject, f.pid, name)
	if err != nil {
		t.Fatal(err)
	}
	return agentMemPurge(agentMemPurgeReq{Scope: agentMemScopeProject, Project: f.pid, Name: name, Plan: pv.Plan, Confirm: name})
}

// allObjectsContain reads every object on disk, loose or packed, reachable or not.
func allObjectsContain(t *testing.T, needle string) bool {
	t.Helper()
	cmd := memoryGit("cat-file", "--batch-all-objects", "--batch")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(out, []byte(needle))
}

func objectExists(sha string) bool {
	_, err := memoryGitRun("cat-file", "-e", sha)
	return err == nil
}

func fsckClean(t *testing.T) {
	t.Helper()
	if out, err := memoryGitRun("fsck", "--no-dangling", "--strict"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

func TestAgentMemoryPurgeRemovesPastRevisionsEverywhere(t *testing.T) {
	f := purgeSetup(t)
	if !allObjectsContain(t, "Hollowbrook") {
		t.Fatal("fixture: the text is not in the repo")
	}
	res, err := purgeDo(t, f, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !res.KeptCurrent || res.CommitsDropped == 0 || res.RefsRewritten+res.RefsDeleted < 2 || res.BlobsRemoved < 2 {
		t.Fatalf("result = %+v", res)
	}
	// Gone from the objects, not only unreachable: the old blob, the text in any object, and the
	// refs that held it.
	if objectExists(f.oldBlob) || allObjectsContain(t, "Hollowbrook") {
		t.Fatal("the purged text is still in the repo")
	}
	if !allObjectsContain(t, purgeNewText) {
		t.Fatal("the current text must stay")
	}
	for _, ref := range []string{"refs/imports/20261004T090000Z/main", "refs/premigrate/20261004T090000Z"} {
		if out, err := memoryGitRun("log", "--format=%H", "--", "af"); err == nil && strings.Contains(out, f.oldCommit) {
			t.Fatalf("%s still reaches the old commit", ref)
		}
		if _, err := memoryGitRun("rev-parse", "--verify", "--quiet", ref); err == nil {
			if _, err := memoryGitRun("cat-file", "-e", f.oldCommit); err == nil {
				t.Fatalf("%s: old commit object survived", ref)
			}
		}
	}
	fsckClean(t)
	if _, err := os.Stat(agentMemPurgePendingPath()); err == nil {
		t.Fatal("the gc marker was left behind")
	}
	// The memory itself is untouched and keeps its revision.
	e, err := agentMemRead(f.c, "", "a")
	if err != nil || e.Body != purgeNewText || e.Revision != 3 {
		t.Fatalf("a = %+v, %v", e, err)
	}
	if out, _ := memoryGitRun("show", memoryBranch+":af/projects/"+f.pid+"/a.md"); !strings.Contains(out, purgeNewText) {
		t.Fatalf("the tip lost the current text")
	}
}

func TestAgentMemoryPurgeLeavesOtherMemoriesHistory(t *testing.T) {
	f := purgeSetup(t)
	before := agentMemChangesT(t)
	var bBefore []agentMemChangeView
	for _, ch := range before {
		if ch.Name == "b" {
			bBefore = append(bBefore, ch)
		}
	}
	if _, err := purgeDo(t, f, "a"); err != nil {
		t.Fatal(err)
	}
	var bAfter []agentMemChangeView
	for _, ch := range agentMemChangesT(t) {
		if ch.Name == "b" {
			bAfter = append(bAfter, ch)
		}
	}
	if len(bAfter) != len(bBefore) || len(bAfter) != 2 {
		t.Fatalf("b changes: %d before, %d after", len(bBefore), len(bAfter))
	}
	// b's revisions are readable and its newest change reverts to the earlier text.
	if _, err := agentMemRevert(agentMemRevertReq{Commit: bAfter[0].Commit}, time.Now()); err != nil {
		t.Fatalf("revert b: %v", err)
	}
	e, err := agentMemRead(f.c, "", "b")
	if err != nil || e.Body != "b one" {
		t.Fatalf("b after revert = %+v, %v", e, err)
	}
	diff, err := agentMemChangeDiff(bAfter[1].Commit)
	if err != nil || !strings.Contains(diff.Diff, "b one") {
		t.Fatalf("diff of b's first change: %+v, %v", diff, err)
	}
	fsckClean(t)
}

func TestAgentMemoryPurgeRevertAcrossPurgedRangeFails(t *testing.T) {
	f := purgeSetup(t)
	if _, err := purgeDo(t, f, "a"); err != nil {
		t.Fatal(err)
	}
	var rows []agentMemChangeView
	for _, ch := range agentMemChangesT(t) {
		if ch.Name == "a" {
			rows = append(rows, ch)
		}
	}
	if len(rows) != 1 || !rows[0].PurgedBefore || !rows[0].Latest || !rows[0].Live {
		t.Fatalf("a's rows after the purge = %+v", rows)
	}
	_, err := agentMemRevert(agentMemRevertReq{Commit: rows[0].Commit}, time.Now())
	if agentMemCode(err) != errCodeMemoryConflict || !strings.Contains(err.Error(), "purged") {
		t.Fatalf("revert across the purge: %v", err)
	}
	if e, _ := agentMemRead(f.c, "", "a"); e.Body != purgeNewText {
		t.Fatalf("a changed: %+v", e)
	}
	if allObjectsContain(t, "Hollowbrook") {
		t.Fatal("the failed revert brought the text back")
	}
	// Forgetting still works, and a new save writes a change that reverts normally.
	if _, err := agentMemRevert(agentMemRevertReq{Commit: rows[0].Commit, Forget: true}, time.Now()); err != nil {
		t.Fatalf("forget: %v", err)
	}
}

func TestAgentMemoryPurgeKeepsWorkingAfterwards(t *testing.T) {
	f := purgeSetup(t)
	if _, err := purgeDo(t, f, "a"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := agentMemSave(f.c, agentMemSaveReq{Name: "a", Description: "d", Body: "after the purge", Revision: 3}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := memorySnapshot(memoryTriggerManual, now); err != nil {
		t.Fatal(err)
	}
	if st, err := memoryGitRun("status", "--porcelain"); err != nil || strings.Contains(st, "Hollowbrook") {
		t.Fatalf("status: %q %v", st, err)
	}
	newest := agentMemChangesT(t)[0]
	if _, err := agentMemRevert(agentMemRevertReq{Commit: newest.Commit}, now); err != nil {
		t.Fatalf("revert after purge: %v", err)
	}
	if e, _ := agentMemRead(f.c, "", "a"); e.Body != purgeNewText {
		t.Fatalf("a = %+v", e)
	}
	fsckClean(t)
	if allObjectsContain(t, "Hollowbrook") {
		t.Fatal("the text is back")
	}
}

func TestAgentMemoryPurgeForgottenMemory(t *testing.T) {
	f := purgeSetup(t)
	last := agentMemChangesT(t)[0]
	if last.Name != "a" {
		t.Fatalf("fixture: %+v", last)
	}
	if _, err := agentMemRevert(agentMemRevertReq{Commit: last.Commit, Forget: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	pv, err := agentMemPurgePreview(agentMemScopeProject, f.pid, "a")
	if err != nil || pv.Live || pv.KeepsCurrent || pv.Nothing {
		t.Fatalf("preview = %+v, %v", pv, err)
	}
	if _, err := purgeDo(t, f, "a"); err != nil {
		t.Fatal(err)
	}
	if allObjectsContain(t, "Hollowbrook") || allObjectsContain(t, purgeNewText) {
		t.Fatal("a forgotten memory's text survived its purge")
	}
	for _, ch := range agentMemChangesT(t) {
		if ch.Name == "a" {
			t.Fatalf("a row for a purged memory: %+v", ch)
		}
	}
	// Its revision floor survives, so a new memory by that name does not reuse a revision.
	if _, err := agentMemSave(f.c, agentMemSaveReq{Name: "a", Description: "d", Body: "again"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if e, _ := agentMemRead(f.c, "", "a"); e.Revision <= 3 {
		t.Fatalf("revision = %d", e.Revision)
	}
	fsckClean(t)
}

func TestAgentMemoryPurgePreviewShowsNoText(t *testing.T) {
	f := purgeSetup(t)
	pv, err := agentMemPurgePreview(agentMemScopeProject, f.pid, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pv)
	for _, leak := range []string{"Hollowbrook", purgeNewText, "contact line"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("the preview echoes the text: %s", b)
		}
	}
	if !pv.KeepsCurrent || !pv.Live || pv.ChangesTotal != 2 || len(pv.Refs) < 3 {
		t.Fatalf("preview = %+v", pv)
	}
	kinds := map[string]bool{}
	for _, r := range pv.Refs {
		kinds[r.Kind] = true
	}
	if !kinds["main"] || !kinds["import"] || !kinds["rescue"] {
		t.Fatalf("refs = %+v", pv.Refs)
	}
	if _, err := memoryGitRun("cat-file", "-e", f.oldBlob); err != nil {
		t.Fatal("the preview changed the repo")
	}
}

func TestAgentMemoryPurgeReportsScannerFindingsMasked(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Now()
	leak := "ghp_" + strings.Repeat("a1B2c3D4e5", 4)
	// The save path's scan refuses a secret, so the history gets it the way an import or a hand
	// edit would: a commit made behind the store's back.
	if _, err := agentMemSave(c, agentMemSaveReq{Name: "a", Description: "d", Body: "ok"}, now); err != nil {
		t.Fatal(err)
	}
	pid := c.Project.ID
	path := agentMemDir() + "/projects/" + pid + "/a.md"
	orig, _ := os.ReadFile(path)
	staging := memoryStagingDir() + "/af/projects/" + pid + "/a.md"
	bad := strings.Replace(string(orig), "ok", "token "+leak, 1)
	if err := os.WriteFile(staging, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryGitRun("add", "-A", "--", "af"); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryGitRun("commit", "--quiet", "-m", "hand edit"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryGitRun("add", "-A", "--", "af"); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryGitRun("commit", "--quiet", "-m", "hand edit back"); err != nil {
		t.Fatal(err)
	}
	pv, err := agentMemPurgePreview(agentMemScopeProject, pid, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Findings) == 0 {
		t.Fatalf("no findings: %+v", pv)
	}
	b, _ := json.Marshal(pv)
	if strings.Contains(string(b), leak) {
		t.Fatalf("the preview echoes the secret: %s", b)
	}
	if _, err := agentMemPurge(agentMemPurgeReq{Scope: agentMemScopeProject, Project: pid, Name: "a", Plan: pv.Plan, Confirm: "a"}); err != nil {
		t.Fatal(err)
	}
	if allObjectsContain(t, leak) {
		t.Fatal("the secret survived the purge")
	}
	fsckClean(t)
}

func TestAgentMemoryPurgeRefusals(t *testing.T) {
	f := purgeSetup(t)
	pv, _ := agentMemPurgePreview(agentMemScopeProject, f.pid, "a")
	req := agentMemPurgeReq{Scope: agentMemScopeProject, Project: f.pid, Name: "a", Plan: pv.Plan, Confirm: "a"}
	bad := req
	bad.Confirm = "b"
	if _, err := agentMemPurge(bad); agentMemCode(err) != errCodeMemoryBadRequest {
		t.Fatalf("wrong confirmation: %v", err)
	}
	bad = req
	bad.Plan = ""
	if _, err := agentMemPurge(bad); agentMemCode(err) != errCodeMemoryConflict {
		t.Fatalf("no plan: %v", err)
	}
	bad = req
	bad.Scope = "elsewhere"
	if _, err := agentMemPurge(bad); agentMemCode(err) != errCodeMemoryBadRequest {
		t.Fatalf("bad scope: %v", err)
	}
	bad = req
	bad.Name = "../x"
	if _, err := agentMemPurge(bad); err == nil {
		t.Fatal("a bad name was accepted")
	}
	// The history moves on after the preview: the plan no longer matches.
	if _, err := agentMemSave(f.c, agentMemSaveReq{Name: "b", Description: "d", Body: "b three", Revision: 2}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := agentMemPurge(req); agentMemCode(err) != errCodeMemoryConflict {
		t.Fatalf("stale plan: %v", err)
	}
	if !objectExists(f.oldBlob) {
		t.Fatal("a refused purge changed the repo")
	}
	// A memory with a single revision has no past to purge.
	if _, err := agentMemSave(f.c, agentMemSaveReq{Name: "solo", Description: "d", Body: "once"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	spv, _ := agentMemPurgePreview(agentMemScopeProject, f.pid, "solo")
	if !spv.Nothing {
		t.Fatalf("solo preview = %+v", spv)
	}
	if _, err := agentMemPurge(agentMemPurgeReq{Scope: agentMemScopeProject, Project: f.pid, Name: "solo", Plan: spv.Plan, Confirm: "solo"}); agentMemCode(err) != errCodeMemoryConflict {
		t.Fatalf("nothing to purge: %v", err)
	}
}

func TestAgentMemoryPurgeRefusesAnUnknownRef(t *testing.T) {
	f := purgeSetup(t)
	if _, err := memoryGitRun("update-ref", "refs/tags/keepsake", f.oldCommit); err != nil {
		t.Fatal(err)
	}
	if _, err := agentMemPurgePreview(agentMemScopeProject, f.pid, "a"); agentMemCode(err) != errCodeMemoryConflict {
		t.Fatalf("preview with a foreign ref: %v", err)
	}
	if !objectExists(f.oldBlob) {
		t.Fatal("the repo changed")
	}
}

// A crash between the steps leaves the repo consistent and the next run finishes the job.
func TestAgentMemoryPurgeCrashSafety(t *testing.T) {
	boom := errors.New("simulated crash")
	for _, stage := range []string{"objects", "refs", "gc"} {
		t.Run(stage, func(t *testing.T) {
			f := purgeSetup(t)
			headBefore, _ := memoryGitRun("rev-parse", memoryBranch)
			agentMemPurgeFault = func(s string) error {
				if s == stage {
					return boom
				}
				return nil
			}
			t.Cleanup(func() { agentMemPurgeFault = nil })
			if _, err := purgeDo(t, f, "a"); !errors.Is(err, boom) {
				t.Fatalf("purge = %v", err)
			}
			agentMemPurgeFault = nil
			fsckClean(t)
			head, _ := memoryGitRun("rev-parse", memoryBranch)
			switch stage {
			case "objects":
				if head != headBefore || !objectExists(f.oldBlob) {
					t.Fatal("a purge that never moved the refs changed the history")
				}
			default:
				if head == headBefore {
					t.Fatal("the refs did not move")
				}
			}
			// Whatever the stage, every memory still reads and writes.
			if e, err := agentMemRead(f.c, "", "a"); err != nil || e.Body != purgeNewText {
				t.Fatalf("a = %+v, %v", e, err)
			}
			if _, err := agentMemSave(f.c, agentMemSaveReq{Name: "b", Description: "d", Body: "b after", Revision: 2}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if stage == "objects" {
				if _, err := purgeDo(t, f, "a"); err != nil {
					t.Fatalf("retry: %v", err)
				}
			} else {
				ResumeMemoryPurge()
			}
			if objectExists(f.oldBlob) || allObjectsContain(t, "Hollowbrook") {
				t.Fatal("the text is still there after the follow-up")
			}
			if _, err := os.Stat(agentMemPurgePendingPath()); err == nil {
				t.Fatal("marker left")
			}
			fsckClean(t)
		})
	}
}

func TestAgentMemoryPurgeConcurrentWithWrites(t *testing.T) {
	f := purgeSetup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			e, err := agentMemRead(f.c, "", "b")
			if err != nil {
				errs <- err
				return
			}
			if _, err := agentMemSave(f.c, agentMemSaveReq{Name: "b", Description: "d", Body: "b " + strings.Repeat("x", i+1), Revision: e.Revision}, time.Now()); err != nil {
				errs <- err
				return
			}
			if _, err := memorySnapshot(memoryTriggerManual, time.Now()); err != nil {
				errs <- err
				return
			}
		}
	}()
	// The plan can go stale while b is written; the purge then asks to be reviewed again.
	var purged bool
	for i := 0; i < 50 && !purged; i++ {
		_, err := purgeDo(t, f, "a")
		switch {
		case err == nil:
			purged = true
		case agentMemCode(err) == errCodeMemoryConflict:
		default:
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if !purged {
		t.Fatal("the purge never got through")
	}
	if allObjectsContain(t, "Hollowbrook") {
		t.Fatal("text survived")
	}
	fsckClean(t)
	if e, err := agentMemRead(f.c, "", "b"); err != nil || !strings.HasPrefix(e.Body, "b x") {
		t.Fatalf("b = %+v, %v", e, err)
	}
}

func TestAgentMemoryPurgeRouteNeedsTheMark(t *testing.T) {
	f := purgeSetup(t)
	pv, _ := agentMemPurgePreview(agentMemScopeProject, f.pid, "a")
	body, _ := json.Marshal(agentMemPurgeReq{Scope: agentMemScopeProject, Project: f.pid, Name: "a", Plan: pv.Plan, Confirm: "a"})
	req := httptest.NewRequest(http.MethodPost, "/agents/memory/entries/purge", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleAgentMemoryPurge(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an unmarked purge: %d %s", w.Code, w.Body)
	}
	if !objectExists(f.oldBlob) {
		t.Fatal("an unmarked purge rewrote the history")
	}
}

// The bare repo's index can hold a revision that was staged and never committed (a snapshot that
// died after `git add`); gc keeps whatever the index names, so the purge has to rebuild it.
func TestAgentMemoryPurgeDropsAStagedOldRevision(t *testing.T) {
	f := purgeSetup(t)
	staging := memoryStagingDir() + "/af/projects/" + f.pid + "/a.md"
	cur, err := os.ReadFile(staging)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := memoryGitRun("show", f.oldBlob)
	if err := os.WriteFile(staging, []byte(old+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryGitRun("add", "--", "af"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, cur, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := purgeDo(t, f, "a"); err != nil {
		t.Fatal(err)
	}
	if allObjectsContain(t, "Hollowbrook") {
		t.Fatal("the staged copy kept the text")
	}
	fsckClean(t)
}
