package memoryx

import (
	"bytes"
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
)

// bundleRemote is the normalised form project.json records ("host/owner/repo").
const bundleRemote = "example.com/acme/demo"

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bundleEnv is the export environment with the main clone's origin set, so that its project has
// a remote to be matched by.
type bundleEnv struct {
	*claudeExportEnv
	local *agentMemProject
}

func newBundleEnv(t *testing.T) *bundleEnv {
	e := &bundleEnv{claudeExportEnv: newClaudeExportEnv(t)}
	testGit(t, e.clone, "remote", "add", "origin", "https://"+bundleRemote+".git")
	e.c = agentMemCallerT(t, "claude-main") // resolved again: the project now has a remote
	e.local = agentMemProjectFor(e.clone)
	if e.local == nil || e.local.Remote == "" {
		t.Fatalf("local project has no remote: %+v", e.local)
	}
	return e
}

// forged builds a memory bundle by hand: the files of an af/ tree, as another workspace's store
// would hold them (authors, revisions, pins and all).
func forgedBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	testGit(t, dir, "init", "--quiet", "-b", "main")
	for p, c := range files {
		memoryWrite(t, filepath.Join(dir, filepath.FromSlash(p)), c)
	}
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "--quiet", "-m", "snapshot")
	out := filepath.Join(t.TempDir(), "af-memory.bundle")
	testGit(t, dir, "bundle", "create", out, "main")
	return out
}

func bundleMemory(name, desc, body string, rev int, pinned bool, kinds ...string) string {
	return string(agentMemRender(agentMemEntry{
		Name: name, Scope: agentMemScopeProject, Description: desc, Type: "project", Kinds: kinds, Revision: rev,
		AuthorKind: "codex", AuthorSession: "someone-else", Created: "2026-01-02T03:04:05Z", Updated: "2026-02-03T04:05:06Z",
		Pinned: pinned, Body: body,
	}))
}

func bundleProjectJSON(id, display, remote string) string {
	return `{"id":"` + id + `","root":"/elsewhere/` + display + `","vcs":"git","display":"` + display + `","remote":"` + remote + `"}` + "\n"
}

func (e *bundleEnv) take(bundle string) string {
	e.t.Helper()
	pv, err := memoryImportPrepare(bundle, "af-memory.bundle", time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return pv.ImportID
}

func (e *bundleEnv) preview(id string) *agentMemBundlePreview {
	e.t.Helper()
	pv, err := agentMemBundlePreviewFor(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return pv
}

func bundleScopeOf(pv *agentMemBundlePreview, source string) *agentMemBundleScope {
	for i := range pv.Scopes {
		if pv.Scopes[i].Source == source {
			return &pv.Scopes[i]
		}
	}
	return nil
}

func bundleItemOf(sc *agentMemBundleScope, name string) *agentMemBundleItem {
	for i := range sc.Items {
		if sc.Items[i].Name == name {
			return &sc.Items[i]
		}
	}
	return nil
}

func (e *bundleEnv) req(id string, pv *agentMemBundlePreview, scope string, names ...string) agentMemBundleReq {
	sc := bundleScopeOf(pv, scope)
	r := agentMemBundleReq{ImportID: id}
	for _, n := range names {
		it := bundleItemOf(sc, n)
		ri := agentMemBundleReqItem{Scope: scope, Name: n}
		if it != nil {
			ri.SourceHash = it.SourceHash
		}
		if sc.Target != nil {
			ri.Target = sc.Target.ID
		}
		r.Items = append(r.Items, ri)
	}
	return r
}

func (e *bundleEnv) apply(r agentMemBundleReq) agentMemBundleApplied {
	e.t.Helper()
	out, err := agentMemBundleApply(r, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func bundleResultOf(a agentMemBundleApplied, scope, name string) agentMemBundleResult {
	for _, r := range a.Results {
		if r.Scope == scope && r.Name == name {
			return r
		}
	}
	return agentMemBundleResult{}
}

// The whole flow with a foreign store: projects are mapped by remote, an unmatched one is listed
// and left out, new memories are published with the documented reset, and a second import finds
// nothing to do.
func TestBundleImportMapsPreviewsAndResets(t *testing.T) {
	e := newBundleEnv(t)
	files := map[string]string{
		"af/projects/foreign-aaaaaaaaaaaa/project.json": bundleProjectJSON("foreign-aaaaaaaaaaaa", "demo", bundleRemote),
		"af/projects/foreign-aaaaaaaaaaaa/alpha.md":     bundleMemory("alpha", "the alpha rule", "alpha body", 7, true, "claude", "codex"),
		"af/projects/foreign-aaaaaaaaaaaa/beta.md":      bundleMemory("beta", "the beta rule", "beta body", 2, false),
		"af/projects/lost-bbbbbbbbbbbb/project.json":    bundleProjectJSON("lost-bbbbbbbbbbbb", "lost", "example.com/acme/other"),
		"af/projects/lost-bbbbbbbbbbbb/gamma.md":        bundleMemory("gamma", "unmatched", "gamma body", 1, false),
		"af/user/uservalue.md":                          bundleMemory("uservalue", "a user one", "user body", 3, true),
	}
	id := e.take(forgedBundle(t, files))
	pv := e.preview(id)
	if !pv.HasAF {
		t.Fatal("hasAF = false")
	}
	proj := bundleScopeOf(pv, "foreign-aaaaaaaaaaaa")
	if proj == nil || proj.Target == nil || proj.Target.ID != e.local.ID || proj.Match != "remote" || proj.Counts[bundleStatusNew] != 2 {
		t.Fatalf("matched project = %+v", proj)
	}
	lost := bundleScopeOf(pv, "lost-bbbbbbbbbbbb")
	if lost == nil || lost.Target != nil || lost.Reason != bundleNoLocal || len(lost.Items) != 0 || lost.Counts["skipped"] != 1 {
		t.Fatalf("unmatched project = %+v", lost)
	}
	if u := bundleScopeOf(pv, "user"); u == nil || u.Counts[bundleStatusNew] != 1 {
		t.Fatalf("user scope = %+v", u)
	}

	r := e.req(id, pv, "foreign-aaaaaaaaaaaa", "alpha", "beta")
	r.Items = append(r.Items, e.req(id, pv, "user", "uservalue").Items...)
	// The unmatched project cannot be asked for either.
	r.Items = append(r.Items, agentMemBundleReqItem{Scope: "lost-bbbbbbbbbbbb", Name: "gamma", SourceHash: "x"})
	out := e.apply(r)
	for _, k := range [][2]string{{"foreign-aaaaaaaaaaaa", "alpha"}, {"foreign-aaaaaaaaaaaa", "beta"}, {"user", "uservalue"}} {
		if res := bundleResultOf(out, k[0], k[1]); res.Result != "imported" || res.Commit == "" {
			t.Fatalf("%v = %+v", k, res)
		}
	}
	if res := bundleResultOf(out, "lost-bbbbbbbbbbbb", "gamma"); res.Result != "skipped" || res.Reason != "no_local_project" {
		t.Fatalf("gamma = %+v", res)
	}

	// What was published: text and kinds kept; revision, authorship, dates and pin reset; source recorded.
	got, err := agentMemMemberRead(agentMemScopeProject, e.local.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "alpha body" || got.Description != "the alpha rule" || strings.Join(got.Kinds, ",") != "claude,codex" {
		t.Errorf("text or kinds lost: %+v", got)
	}
	if got.Revision != 1 || got.Pinned || got.AuthorKind != agentMemUnknown || got.AuthorSession != agentMemUnknown ||
		got.Created != "2026-10-14T12:00:00Z" || got.Updated != "2026-10-14T12:00:00Z" {
		t.Errorf("reset not applied: %+v", got)
	}
	if want := "bundle:" + id + ":projects/foreign-aaaaaaaaaaaa/alpha@7"; got.Source != want {
		t.Errorf("source = %q, want %q", got.Source, want)
	}
	if u, err := agentMemMemberRead(agentMemScopeUser, "", "uservalue"); err != nil || u.Pinned || u.Revision != 1 {
		t.Errorf("user memory = %+v %v", u, err)
	}
	if _, err := agentMemCheckDir("projects/lost-bbbbbbbbbbbb", false); err == nil {
		t.Error("an unmatched project got a directory")
	}
	// The history records the operation.
	log, err := memoryGitRun("log", "-n", "6", "--format=%B")
	if err != nil || !strings.Contains(log, "AF-Bundle-Import: "+id) || !strings.Contains(log, "AF-Bundle-Source: projects/foreign-aaaaaaaaaaaa/alpha.md@7") {
		t.Errorf("no provenance in the history: %v\n%s", err, log)
	}

	// Again: nothing new, nothing duplicated.
	pv2 := e.preview(id)
	if sc := bundleScopeOf(pv2, "foreign-aaaaaaaaaaaa"); sc.Counts[bundleStatusUnchanged] != 2 || sc.Counts[bundleStatusNew] != 0 {
		t.Fatalf("second preview = %+v", sc.Counts)
	}
	out = e.apply(e.req(id, pv, "foreign-aaaaaaaaaaaa", "alpha"))
	if res := bundleResultOf(out, "foreign-aaaaaaaaaaaa", "alpha"); res.Result != "skipped" || res.Reason != "status_unchanged" {
		t.Fatalf("a second apply = %+v", res)
	}
}

// A memory that is in the way is never replaced, and one forgotten here is never brought back.
func TestBundleImportNeverReplacesOrResurrects(t *testing.T) {
	e := newBundleEnv(t)
	e.save("same", "same text", "project", "same body")
	e.save("differs", "local text", "project", "local body")
	e.save("doomed", "forgotten here", "project", "doomed body")
	if _, err := agentMemForget(e.c, agentMemForgetReq{Name: "doomed", Revision: 1}, e.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	src := "foreign-cccccccccccc"
	files := map[string]string{
		"af/projects/" + src + "/project.json": bundleProjectJSON(src, "demo", bundleRemote),
		"af/projects/" + src + "/same.md":      bundleMemory("same", "same text", "same body", 4, false),
		"af/projects/" + src + "/differs.md":   bundleMemory("differs", "bundle text", "bundle body", 4, false),
		"af/projects/" + src + "/doomed.md":    bundleMemory("doomed", "forgotten here", "doomed body", 4, false),
		// Forgotten in the bundle's workspace: only its tombstone travels, and it is not a memory.
		"af/projects/" + src + "/.forgotten/gone": "3\n",
	}
	id := e.take(forgedBundle(t, files))
	pv := e.preview(id)
	sc := bundleScopeOf(pv, src)
	want := map[string]string{"same": bundleStatusUnchanged, "differs": bundleStatusExists, "doomed": bundleStatusForgotten}
	for n, st := range want {
		if it := bundleItemOf(sc, n); it == nil || it.Status != st {
			t.Fatalf("%s = %+v, want %s", n, it, st)
		}
	}
	if len(sc.Items) != 3 {
		t.Fatalf("items = %+v", sc.Items)
	}
	out := e.apply(e.req(id, pv, src, "same", "differs", "doomed"))
	for n, reason := range map[string]string{"same": "status_unchanged", "differs": "status_exists", "doomed": "status_forgotten"} {
		if r := bundleResultOf(out, src, n); r.Result != "skipped" || r.Reason != reason {
			t.Errorf("%s = %+v", n, r)
		}
	}
	if got, _ := agentMemMemberRead(agentMemScopeProject, e.local.ID, "differs"); got.Body != "local body" {
		t.Errorf("a local memory was replaced: %+v", got)
	}
	if names := bundleAFNames(t, e.local.ID); strings.Join(names, ",") != "differs,same" {
		t.Errorf("store = %v", names)
	}
	// A forgotten memory whose tombstone is all that is left of it is not brought back either.
	if err := os.RemoveAll(filepath.Join(agentMemDir(), "projects", e.local.ID, ".forgotten")); err != nil {
		t.Fatal(err)
	}
	if it := bundleItemOf(bundleScopeOf(e.preview(id), src), "doomed"); it == nil || it.Status != bundleStatusForgotten {
		t.Errorf("without the tombstone the history alone must still say forgotten: %+v", it)
	}
}

// Every part of every file is scanned before anything is published, with no override, and a
// secret is never echoed.
func TestBundleImportScansEveryBodyAndNeverEchoes(t *testing.T) {
	e := newBundleEnv(t)
	key := "ghp_" + strings.Repeat("q7Zx", 9)
	src := "foreign-dddddddddddd"
	leakyFM := bundleMemory("fm", "frontmatter leak", "clean body", 1, false)
	leakyFM = strings.Replace(leakyFM, "---\nname:", "---\nsource: \""+key+"\"\nname:", 1)
	files := map[string]string{
		"af/projects/" + src + "/project.json": bundleProjectJSON(src, "demo", bundleRemote),
		"af/projects/" + src + "/body.md":      bundleMemory("body", "body leak", "the token is "+key, 1, false),
		"af/projects/" + src + "/desc.md":      bundleMemory("desc", "desc "+key, "clean", 1, false),
		"af/projects/" + src + "/fm.md":        leakyFM,
		"af/projects/" + src + "/clean.md":     bundleMemory("clean", "fine", "nothing here", 1, false),
		"af/projects/" + src + "/bad.md":       "not a memory file",
		// The raw file is clean: the token only exists once the JSON escape is decoded.
		"af/projects/" + src + "/esc.md": "---\nname: \"esc\"\ndescription: \"\\u0067hp_" + strings.Repeat("q7Zx", 9) + "\"\n---\nclean body\n",
	}
	id := e.take(forgedBundle(t, files))
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	pv := e.preview(id)
	sc := bundleScopeOf(pv, src)
	for _, n := range []string{"body", "desc", "fm", "esc"} {
		if it := bundleItemOf(sc, n); it == nil || it.Status != bundleStatusSecret || len(it.Findings) == 0 {
			t.Fatalf("%s = %+v", n, it)
		}
	}
	if it := bundleItemOf(sc, "bad"); it == nil || it.Status != bundleStatusInvalid || it.Reason != "no_frontmatter" {
		t.Fatalf("bad = %+v", it)
	}
	out := e.apply(e.req(id, pv, src, "body", "desc", "fm", "esc", "clean", "bad"))
	for _, n := range []string{"body", "desc", "fm", "esc"} {
		if r := bundleResultOf(out, src, n); r.Result != "skipped" || r.Reason != "status_secret" {
			t.Errorf("%s = %+v", n, r)
		}
	}
	if r := bundleResultOf(out, src, "clean"); r.Result != "imported" {
		t.Errorf("clean = %+v", r)
	}
	if names := bundleAFNames(t, e.local.ID); strings.Join(names, ",") != "clean" {
		t.Errorf("store = %v", names)
	}
	wire := httptest.NewRecorder()
	HandleAgentMemoryBundlePreview(wire, httptest.NewRequest("GET", "/x?importId="+id, nil))
	if strings.Contains(wire.Body.String(), key) || strings.Contains(logs.String(), key) {
		t.Errorf("a secret reached the preview: %s", wire.Body)
	}
}

// What a request can and cannot do.
func TestBundleImportRequestIsBoundToThePreview(t *testing.T) {
	e := newBundleEnv(t)
	src := "foreign-eeeeeeeeeeee"
	files := map[string]string{
		"af/projects/" + src + "/project.json": bundleProjectJSON(src, "demo", bundleRemote),
		"af/projects/" + src + "/one.md":       bundleMemory("one", "one", "one body", 1, false),
	}
	id := e.take(forgedBundle(t, files))
	pv := e.preview(id)
	r := e.req(id, pv, src, "one", "one")
	r.Items[0].SourceHash = "0000"
	r.Items[1].Target = "somewhere-else"
	r.Items = append(r.Items, agentMemBundleReqItem{Scope: "nope", Name: "one"}, agentMemBundleReqItem{Scope: src, Name: "../x"})
	out := e.apply(r)
	got := map[string]int{}
	for _, x := range out.Results {
		if x.Result != "skipped" {
			t.Fatalf("a tampered item was written: %+v", x)
		}
		got[x.Reason]++
	}
	if got["changed_since_preview"] != 2 || got["not_found"] != 1 || got["bad_name"] != 1 {
		t.Fatalf("reasons = %v", got)
	}
	if names := bundleAFNames(t, e.local.ID); len(names) != 0 {
		t.Fatalf("store = %v", names)
	}
	if _, err := agentMemBundleApply(agentMemBundleReq{ImportID: id}, time.Now()); err == nil {
		t.Error("an empty request was accepted")
	}
	big := agentMemBundleReq{ImportID: id}
	for i := 0; i < agentMemBundleBatch+1; i++ {
		big.Items = append(big.Items, agentMemBundleReqItem{Scope: src, Name: "one"})
	}
	if _, err := agentMemBundleApply(big, time.Now()); err == nil {
		t.Error("an oversize request was accepted")
	}
	if _, err := agentMemBundlePreviewFor("not-an-id"); err == nil {
		t.Error("a bad import id was accepted")
	}
	if _, err := agentMemBundlePreviewFor("20200101T000000Z"); err == nil {
		t.Error("an unknown import id was accepted")
	}
	// Bare, the apply handler refuses: it answers only the member-origin mark.
	w := httptest.NewRecorder()
	HandleAgentMemoryBundleApply(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)))
	if w.Code != http.StatusForbidden {
		t.Errorf("bare handler = %d", w.Code)
	}
}

// Two local projects with the remote of a source project: ambiguous, listed, not imported.
func TestBundleImportAmbiguousRemote(t *testing.T) {
	e := newBundleEnv(t)
	dir := filepath.Join(e.home, "repos", "twin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "init", "--quiet", "-b", "main")
	testGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "init")
	testGit(t, dir, "remote", "add", "origin", "https://"+bundleRemote+".git")
	src := "foreign-ffffffffffff"
	files := map[string]string{
		"af/projects/" + src + "/project.json": bundleProjectJSON(src, "demo", bundleRemote),
		"af/projects/" + src + "/one.md":       bundleMemory("one", "one", "one body", 1, false),
	}
	id := e.take(forgedBundle(t, files))
	sc := bundleScopeOf(e.preview(id), src)
	if sc.Target != nil || sc.Reason != bundleAmbiguous || len(sc.Items) != 0 {
		t.Fatalf("scope = %+v", sc)
	}
}

// A bundle with no AF memory says so; and the generic apply of the same lineage still skips af/.
func TestBundleImportGenericPathStillSkipsAF(t *testing.T) {
	e := newBundleEnv(t)
	empty := e.take(forgedBundle(t, map[string]string{"claude/projects/-x/memory/a.md": "plain\n"}))
	if pv := e.preview(empty); pv.HasAF || len(pv.Scopes) != 0 {
		t.Fatalf("preview of a bundle without AF memory = %+v", pv)
	}
	src := "foreign-111111111111"
	id := e.take(forgedBundle(t, map[string]string{
		"af/projects/" + src + "/project.json": bundleProjectJSON(src, "demo", bundleRemote),
		"af/projects/" + src + "/one.md":       bundleMemory("one", "one", "one body", 1, false),
	}))
	if _, err := memoryImportApply(id, memoryRestoreScope{All: true}, time.Now(), memoryApplyOpts{}); err != nil {
		t.Logf("generic apply: %v", err)
	}
	if names := bundleAFNames(t, e.local.ID); len(names) != 0 {
		t.Fatalf("the generic apply wrote AF memory: %v", names)
	}
	if _, err := os.Stat(filepath.Join(agentMemDir(), "projects", src)); err == nil {
		t.Fatal("the generic apply created an AF project directory")
	}
}

// The real thing: a bundle exported by one workspace, carried to another whose clone sits at a
// different path (a different project id) with the same remote.
func TestBundleImportRealExportBetweenWorkspaces(t *testing.T) {
	share := t.TempDir()
	a := newBundleEnv(t)
	a.save("carried", "carried over", "project", "carried body")
	a.save("second", "second", "feedback", "second body")
	src, err := memoryExportBundle()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(share, "af-memory.bundle")
	if err := memoryCopyFile(src, bundle); err != nil {
		t.Fatal(err)
	}
	aID := a.local.ID

	b := newBundleEnv(t) // another HOME: another clone path, another project id
	if b.local.ID == aID {
		t.Fatal("the two workspaces ended up with one project id")
	}
	id := b.take(bundle)
	pv := b.preview(id)
	sc := bundleScopeOf(pv, aID)
	if sc == nil || sc.Target == nil || sc.Target.ID != b.local.ID || sc.Match != "remote" || sc.Counts[bundleStatusNew] != 2 {
		t.Fatalf("scope = %+v", sc)
	}
	b.apply(b.req(id, pv, aID, "carried", "second"))
	if names := bundleAFNames(t, b.local.ID); strings.Join(names, ",") != "carried,second" {
		t.Fatalf("store = %v", names)
	}
}

// bundleAFNames lists the live memories of a local project.
func bundleAFNames(t *testing.T, pid string) []string {
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
