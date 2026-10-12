package memoryx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type museImportEnv struct {
	t     *testing.T
	home  string
	clone string
	wt    string
	dir   string // the clone's directory name under ~/repos
	pid   string
}

func newMuseImportEnv(t *testing.T) *museImportEnv {
	home, clone, wt := agentMemTestEnv(t)
	return &museImportEnv{t: t, home: home, clone: clone, wt: wt, dir: filepath.Base(clone), pid: agentMemProjectFor(clone).ID}
}

// note writes a muse note: name / description / type at the top level, as muse writes them.
func (e *museImportEnv) note(name, desc, typ, body string) string {
	e.t.Helper()
	return e.raw(e.clone, name+".md", fmt.Sprintf("---\nname: %s\ndescription: %s\ntype: %s\n---\n\n%s\n", name, desc, typ, body))
}

func (e *museImportEnv) raw(wc, file, text string) string {
	e.t.Helper()
	p := filepath.Join(wc, ".agents", "memory", file)
	memoryWrite(e.t, p, text)
	return p
}

func (e *museImportEnv) preview() *agentMemMusePreview {
	e.t.Helper()
	pv, err := agentMemMusePreviewFor(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return pv
}

func (e *museImportEnv) item(pv *agentMemMusePreview, name string) agentMemImportItem {
	e.t.Helper()
	it := pv.byName[name]
	if it == nil {
		e.t.Fatalf("no item %q in %v", name, pv.Counts)
	}
	return *it
}

func (e *museImportEnv) apply(now time.Time, names ...string) agentMemImportApplied {
	e.t.Helper()
	pv := e.preview()
	req := agentMemMuseReq{Project: e.pid, Dir: e.dir}
	for _, n := range names {
		req.Items = append(req.Items, agentMemImportReqItem{Name: n, SourceHash: pv.byName[n].SourceHash})
	}
	out, err := agentMemMuseApply(req, now)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *museImportEnv) git(args ...string) {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = e.clone
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestMuseImportSourcesAndPreview(t *testing.T) {
	e := newMuseImportEnv(t)
	e.note("pref", "a preference", "feedback", "use tabs")
	e.raw(e.clone, "MEMORY.md", "- [Pref](pref.md) - a preference\n")
	e.raw(e.clone, "sub/nested.md", "---\ndescription: d\n---\nb\n") // subdirectories are not entered
	// A plain directory under ~/repos is its own project and nothing in it is shared through git.
	plain := filepath.Join(e.home, "repos", "plain")
	memoryMkdirAll(t, plain)
	e.raw(plain, "x.md", "---\ndescription: d\n---\nb\n")
	// A working copy with only the index lists nothing.
	e.raw(e.wt, "MEMORY.md", "- idx\n")

	by := map[string]agentMemMuseSource{}
	for _, s := range agentMemMuseList().Sources {
		by[s.Dir] = s
	}
	if s := by[e.dir]; s.Count != 1 || s.Project == nil || s.Project.ID != e.pid {
		t.Errorf("clone source = %+v", s)
	}
	if _, ok := by[filepath.Base(e.wt)]; ok {
		t.Errorf("index-only working copy listed")
	}
	if s := by["plain"]; s.Project == nil || s.Shared != 0 {
		t.Errorf("plain source = %+v", s)
	}

	pv := e.preview()
	if len(pv.Items) != 1 {
		t.Fatalf("items = %+v", pv.Items)
	}
	if it := e.item(pv, "pref"); it.Status != claudeImportNew || it.Type != "feedback" || it.Shared {
		t.Errorf("pref = %+v", it)
	}
	if pv, err := agentMemMusePreviewFor("plain"); err != nil || pv.Project == nil || pv.Items[0].Shared {
		t.Errorf("plain preview = %+v, %v", pv, err)
	}
	for _, bad := range []string{"", "..", "a/b", "nope"} {
		if _, err := agentMemMusePreviewFor(bad); err == nil {
			t.Errorf("preview of %q must fail", bad)
		}
	}
}

func TestMuseImportAppliesToProjectOnly(t *testing.T) {
	e := newMuseImportEnv(t)
	e.note("pref", "a preference", "user", "use tabs")
	out := e.apply(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), "pref")
	if len(out.Results) != 1 || out.Results[0].Result != "imported" {
		t.Fatalf("apply = %+v", out)
	}
	got, err := agentMemRead(agentMemCallerT(t, "claude-main"), "", "pref")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != agentMemScopeProject || got.AuthorKind != agentMemUnknown || got.AuthorSession != agentMemUnknown ||
		got.Source != "muse:project/.agents/memory/pref.md" || got.SourceHash == "" || got.Revision != 1 || got.Body != "use tabs" {
		t.Errorf("entry = %+v", got)
	}
	files, _ := memoryGitRun("log", "--name-only", "--format=", memoryBranch)
	for _, f := range strings.Fields(files) {
		if !strings.HasPrefix(f, "af/projects/"+e.pid+"/") {
			t.Errorf("import wrote outside the project: %s", f)
		}
	}
	msg, _ := memoryGitRun("log", "-1", "--format=%B", memoryBranch)
	if !strings.Contains(msg, "AF-Op: import") || !strings.Contains(msg, "AF-Author-Kind: member") {
		t.Errorf("commit = %s", msg)
	}
	if pv := e.preview(); pv.Counts[claudeImportUnchanged] != 1 || len(pv.Counts) != 1 {
		t.Errorf("not idempotent: %v", pv.Counts)
	}
}

func TestMuseImportSharedThroughGit(t *testing.T) {
	e := newMuseImportEnv(t)
	e.note("team", "shared note", "project", "b")
	e.note("mine", "private note", "project", "b")
	e.git("add", ".agents/memory/team.md")
	e.git("commit", "--quiet", "-m", "add team note")
	pv := e.preview()
	if !e.item(pv, "team").Shared || e.item(pv, "mine").Shared {
		t.Errorf("shared flags: team=%v mine=%v", e.item(pv, "team").Shared, e.item(pv, "mine").Shared)
	}
	for _, s := range agentMemMuseList().Sources {
		if s.Dir == e.dir && s.Shared != 1 {
			t.Errorf("source shared = %d, want 1", s.Shared)
		}
	}
}

func TestMuseImportUpdateOnlyReplacesIntactImport(t *testing.T) {
	e := newMuseImportEnv(t)
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, n := range []string{"keep", "edited", "claudes", "afown"} {
		p := e.note(n, "d", "user", "v1")
		_ = os.Chtimes(p, t0, t0)
	}
	e.apply(t0.Add(time.Hour), "keep", "edited")
	c := agentMemCallerT(t, "claude-main")
	// edited: the member saves their own revision in AF.
	if _, err := agentMemSave(c, agentMemSaveReq{Name: "edited", Description: "d", Type: "user", Body: "af text", Revision: 1}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// afown: written in AF, never imported. claudes: stored by another importer.
	if _, err := agentMemSave(c, agentMemSaveReq{Name: "afown", Description: "d", Type: "user", Body: "af own"}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(5 * time.Hour)
	for _, n := range []string{"keep", "edited", "afown"} {
		p := e.note(n, "d", "user", "v2")
		_ = os.Chtimes(p, later, later)
	}
	pv := e.preview()
	if it := e.item(pv, "keep"); it.Status != claudeImportUpdate {
		t.Errorf("keep = %+v", it)
	}
	if it := e.item(pv, "edited"); it.Status != museImportDiverged || it.Reason != "af_changed" {
		t.Errorf("edited = %+v", it)
	}
	if it := e.item(pv, "afown"); it.Status != museImportDiverged || it.Reason != "other_source" {
		t.Errorf("afown = %+v", it)
	}
	out := e.apply(later.Add(time.Hour), "keep", "edited", "afown")
	if r := out.Results[0]; r.Name != "keep" || r.Result != "updated" {
		t.Errorf("keep result = %+v", r)
	}
	for _, r := range out.Results[1:] {
		if r.Result != "skipped" || r.Reason != "status_diverged" {
			t.Errorf("%s result = %+v", r.Name, r)
		}
	}
	if got, _ := agentMemRead(c, "", "edited"); got.Body != "af text" {
		t.Errorf("the member's AF text was overwritten: %+v", got)
	}
	if got, _ := agentMemRead(c, "", "keep"); got.Body != "v2" || got.Revision != 2 {
		t.Errorf("keep = %+v", got)
	}
	// An older source file never replaces anything.
	older := e.note("keep", "d", "user", "v0")
	_ = os.Chtimes(older, t0, t0)
	if it := e.item(e.preview(), "keep"); it.Status != claudeImportUnchanged {
		t.Errorf("older source = %+v", it)
	}
}

func TestMuseImportForgottenAndSecrets(t *testing.T) {
	e := newMuseImportEnv(t)
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	e.note("gone", "d", "user", "b")
	e.note("fresh", "d", "user", "b")
	e.note("leaky", "d", "user", "token is ghp_"+strings.Repeat("q7Zx", 9))
	e.apply(now, "gone")
	if _, err := agentMemForget(agentMemCallerT(t, "claude-main"), agentMemForgetReq{Name: "gone", Revision: 1}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pv := e.preview()
	if it := e.item(pv, "gone"); it.Status != claudeImportForgotten {
		t.Errorf("gone = %+v", it)
	}
	if it := e.item(pv, "leaky"); it.Status != claudeImportSecret || len(it.Findings) == 0 {
		t.Fatalf("leaky = %+v", it)
	}
	out := e.apply(now.Add(time.Hour), "gone", "fresh", "leaky")
	want := map[string]string{"gone": "status_forgotten", "leaky": "status_secret"}
	for _, r := range out.Results {
		switch r.Name {
		case "fresh":
			if r.Result != "imported" {
				t.Errorf("fresh = %+v", r)
			}
		default:
			if r.Result != "skipped" || r.Reason != want[r.Name] {
				t.Errorf("%s = %+v", r.Name, r)
			}
		}
	}
	if all, _ := memoryGitRun("log", "-p", memoryBranch); strings.Contains(all, "ghp_") {
		t.Errorf("the secret reached the store")
	}
}

func TestMuseImportSymlinkedDirectoriesRefused(t *testing.T) {
	e := newMuseImportEnv(t)
	outside := filepath.Join(e.home, "outside")
	memoryWrite(t, filepath.Join(outside, "memory", "x.md"), "---\ndescription: d\n---\nb\n")
	if err := os.Symlink(outside, filepath.Join(e.clone, ".agents")); err != nil {
		t.Fatal(err)
	}
	for _, s := range agentMemMuseList().Sources {
		if s.Dir == e.dir {
			t.Errorf("a symlinked .agents was listed: %+v", s)
		}
	}
	if _, err := agentMemMusePreviewFor(e.dir); agentMemCode(err) != errCodeMemoryNotFound {
		t.Errorf("preview through a symlink: %v", err)
	}
}

func TestMuseImportHandlers(t *testing.T) {
	e := newMuseImportEnv(t)
	e.note("via-http", "d", "user", "b")
	do := func(method, target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		w := httptest.NewRecorder()
		switch {
		case method == http.MethodPost:
			HandleAgentMemoryMuseApply(w, r)
		case strings.Contains(target, "preview"):
			HandleAgentMemoryMusePreview(w, r)
		default:
			HandleAgentMemoryMuseSources(w, r)
		}
		return w
	}
	hash := e.preview().byName["via-http"].SourceHash
	post := fmt.Sprintf(`{"project":%q,"dir":%q,"items":[{"name":"via-http","sourceHash":%q}]}`, e.pid, e.dir, hash)
	AgentMemoryEnabled = func() bool { return false }
	if w := do("GET", "/agents/memory/muse-import", ""); w.Code != 200 {
		t.Errorf("sources off: %d", w.Code)
	}
	if w := do("GET", "/agents/memory/muse-import/preview?dir="+e.dir, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"new"`) {
		t.Errorf("preview off: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/agents/memory/muse-import", post); w.Code != 403 || !strings.Contains(w.Body.String(), errCodeMemoryDisabled) {
		t.Errorf("apply off: %d %s", w.Code, w.Body)
	}
	if memoryHasCommits() {
		t.Errorf("a refused apply committed")
	}
	AgentMemoryEnabled = func() bool { return true }
	if w := do("POST", "/agents/memory/muse-import", post); w.Code != 200 || !strings.Contains(w.Body.String(), `"imported"`) {
		t.Errorf("apply on: %d %s", w.Code, w.Body)
	}
}
