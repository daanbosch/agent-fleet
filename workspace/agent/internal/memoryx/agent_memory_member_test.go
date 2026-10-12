package memoryx

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func memberDo(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var s string
	if body != nil {
		b, _ := json.Marshal(body)
		s = string(b)
	}
	w := smokeDo(t, buildMux(), method, path, "", s)
	return w.Code, w.Body.Bytes()
}

// The member reads a body without a session, while the sessions' switch is off, and the read is
// not a use.
func TestAgentMemoryMemberReadIsUngatedAndUncounted(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "proj-one", "project", now)
	AgentMemoryEnabled = func() bool { return false }

	code, b := memberDo(t, http.MethodGet, "/agents/memory/entries/member-read?scope=project&project="+c.Project.ID+"&name=proj-one", nil)
	var e agentMemEntry
	if code != http.StatusOK || json.Unmarshal(b, &e) != nil || e.Body != "b" || e.AuthorSession != "claude-main" {
		t.Fatalf("read: %d %s", code, b)
	}
	if got := agentMemLoadUsage("projects/" + c.Project.ID)["proj-one"]; got != 0 {
		t.Fatalf("a member read counted as a use: %d", got)
	}
	for _, q := range []string{
		"scope=project&project=nope&name=proj-one",
		"scope=project&project=" + c.Project.ID + "&name=missing",
		"scope=other&name=x",
		"scope=user&name=" + "../x",
	} {
		if code, _ := memberDo(t, http.MethodGet, "/agents/memory/entries/member-read?"+q, nil); code < 400 {
			t.Errorf("%s: status %d", q, code)
		}
	}
}

// A file that fails the scan is not shown, and the refusal does not repeat its text.
func TestAgentMemoryMemberReadWithholdsSecret(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "leaky", "project", now)
	f := filepath.Join(agentMemDir(), "projects", c.Project.ID, "leaky.md")
	raw, _ := os.ReadFile(f)
	secret := agentMemFakeAWS()
	if err := os.WriteFile(f, []byte(strings.Replace(string(raw), "\nb\n", "\n"+secret+"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, b := memberDo(t, http.MethodGet, "/agents/memory/entries/member-read?scope=project&project="+c.Project.ID+"&name=leaky", nil)
	if code != http.StatusUnprocessableEntity || strings.Contains(string(b), secret) {
		t.Fatalf("read of a leaky memory: %d %s", code, b)
	}
}

// The list carries what the browser shows and acts on.
func TestAgentMemoryListCarriesAuthorAndRevision(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	agentMemSaveN(t, c, "proj-one", "project", time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	out, err := agentMemListAll()
	if err != nil || len(out.Entries) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if e := out.Entries[0]; e.Revision != 1 || e.AuthorSession != "claude-main" || e.AuthorKind == "" {
		t.Errorf("entry = %+v", e)
	}
}

// A forget pinned to the revision the member saw is refused once an agent has updated the memory,
// even though the newest change (read after the update) is a valid revert target; the reloaded
// revision goes through. Without the check the newer text is deleted unseen.
func TestAgentMemoryRevertExpectRevision(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "same", "project", now)
	listed, err := agentMemListAll() // the row the member looked at: revision 1
	if err != nil || len(listed.Entries) != 1 || listed.Entries[0].Revision != 1 {
		t.Fatalf("list: %+v %v", listed, err)
	}
	if _, err := agentMemSave(c, agentMemSaveReq{Name: "same", Description: "new", Type: "project", Body: "newer", Revision: 1}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ch, err := agentMemListChanges(10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agentMemRevert(agentMemRevertReq{Commit: ch.Changes[0].Commit, Forget: true, ExpectRevision: listed.Entries[0].Revision}, now)
	if agentMemCode(err) != errCodeMemoryConflict {
		t.Fatalf("forget of a row older than the memory: %v", err)
	}
	if _, err := agentMemRead(c, "project", "same"); err != nil {
		t.Fatalf("memory was removed: %v", err)
	}
	if _, err := agentMemRevert(agentMemRevertReq{Commit: ch.Changes[0].Commit, Forget: true, ExpectRevision: 2}, now); err != nil {
		t.Fatalf("forget at the reloaded revision: %v", err)
	}
}

func memberSearch(t *testing.T, q string) (int, agentMemMemberSearchOut, []byte) {
	t.Helper()
	code, b := memberDo(t, http.MethodGet, "/agents/memory/entries/member-search?"+q, nil)
	var out agentMemMemberSearchOut
	_ = json.Unmarshal(b, &out)
	return code, out, b
}

// The member finds a body in the user scope and in a project, ranked like the tools' search,
// while the sessions' switch is off; the search is not a use of any hit. Withheld text and names
// never show.
func TestAgentMemoryMemberSearchIsUngatedUncountedAndNeverLeaks(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	memSearchSave(t, c, "proj-note", "about builds", "the zebra rule applies here", now)
	if _, err := agentMemSave(c, agentMemSaveReq{Scope: "user", Name: "user-note", Description: "about me", Body: "zebra in user scope"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	memSearchSave(t, c, "leaky-name", "d", "zebra body", now.Add(2*time.Minute))
	f := filepath.Join(agentMemDir(), "projects", c.Project.ID, "leaky-name.md")
	raw, _ := os.ReadFile(f)
	secret := agentMemFakeAWS()
	if err := os.WriteFile(f, []byte(strings.Replace(string(raw), "zebra body", "zebra "+secret, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	AgentMemoryEnabled = func() bool { return false }

	code, out, b := memberSearch(t, "q=zebra")
	if code != http.StatusOK || len(out.Hits) != 2 {
		t.Fatalf("search: %d %s", code, b)
	}
	byName := map[string]agentMemMemberHit{}
	for _, h := range out.Hits {
		byName[h.Name] = h
	}
	if h := byName["proj-note"]; h.Project == nil || h.Project.ID != c.Project.ID || h.Scope != "project" || len(h.Snippets) == 0 || h.Body != "" {
		t.Errorf("project hit = %+v", h)
	}
	if h := byName["user-note"]; h.Project != nil || h.Scope != "user" {
		t.Errorf("user hit = %+v", h)
	}
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "leaky-name") {
		t.Fatalf("a withheld memory reached the answer: %s", b)
	}
	for _, rel := range []string{"user", "projects/" + c.Project.ID} {
		if u := agentMemLoadUsage(rel); len(u) != 0 {
			t.Fatalf("a member search counted uses in %s: %v", rel, u)
		}
	}
	// The scope parameters narrow it; a bad one is refused; the query is required.
	if _, out, _ := memberSearch(t, "q=zebra&scope=user"); len(out.Hits) != 1 || out.Hits[0].Name != "user-note" {
		t.Errorf("scope=user: %+v", out.Hits)
	}
	if _, out, _ := memberSearch(t, "q=zebra&scope=project&project="+c.Project.ID); len(out.Hits) != 1 || out.Hits[0].Name != "proj-note" {
		t.Errorf("scope=project: %+v", out.Hits)
	}
	for _, q := range []string{"q=", "q=zebra&scope=other", "q=zebra&scope=project&project=nope"} {
		if code, _, _ := memberSearch(t, q); code < 400 {
			t.Errorf("%s: status %d", q, code)
		}
	}
	// The tools' search, by contrast, counts: the sidecar check above is not vacuous.
	AgentMemoryEnabled = func() bool { return true }
	if _, err := agentMemSearch(c, "zebra", 0); err != nil {
		t.Fatal(err)
	}
	if got := agentMemLoadUsage("projects/" + c.Project.ID)["proj-note"]; got != 1 {
		t.Fatalf("the tools' search should count one use, got %d", got)
	}
}

// A memory whose newest change is older than any window of the changes list is still found, as
// live or forgotten, with the commit a revert has to name.
func TestAgentMemoryMemberLatestFindsChangeOutsideTheWindow(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "old", "project", now)
	for i, n := range []string{"n1", "n2", "n3"} {
		agentMemSaveN(t, c, n, "project", now.Add(time.Duration(i+1)*time.Minute))
	}
	win, err := agentMemListChanges(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range win.Changes {
		if ch.Name == "old" {
			t.Fatalf("fixture: old is inside the window: %+v", win.Changes)
		}
	}
	AgentMemoryEnabled = func() bool { return false }
	code, b := memberDo(t, http.MethodGet, "/agents/memory/entries/member-latest?scope=project&project="+c.Project.ID+"&name=old", nil)
	var v agentMemChangeView
	if code != http.StatusOK || json.Unmarshal(b, &v) != nil || v.Name != "old" || !v.Latest || !v.Live || !v.Revertible || v.Project == nil || v.Op != "create" {
		t.Fatalf("latest: %d %s", code, b)
	}
	// The commit it names goes through the existing revert, with its revision check.
	if _, err := agentMemRevert(agentMemRevertReq{Commit: v.Commit, Forget: true, ExpectRevision: 1}, now); err != nil {
		t.Fatalf("forget from the resolved change: %v", err)
	}
	code, b = memberDo(t, http.MethodGet, "/agents/memory/entries/member-latest?scope=project&project="+c.Project.ID+"&name=old", nil)
	var gone agentMemChangeView
	if code != http.StatusOK || json.Unmarshal(b, &gone) != nil || gone.Op != "forget" || gone.Live || !gone.Revertible || gone.Commit == v.Commit {
		t.Fatalf("latest of a forgotten memory: %d %s", code, b)
	}
	for _, q := range []string{
		"scope=project&project=" + c.Project.ID + "&name=never-saved",
		"scope=project&project=nope&name=old",
		"scope=other&name=old",
		"scope=user&name=../x",
	} {
		if code, _ := memberDo(t, http.MethodGet, "/agents/memory/entries/member-latest?"+q, nil); code < 400 {
			t.Errorf("%s: status %d", q, code)
		}
	}
}

// A memory whose file now fails the scan is withheld from member-latest too: the trailers of its
// commit are clean, but answering would name a memory the list and the search hide.
func TestAgentMemoryMemberLatestWithholdsSecretFile(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	agentMemSaveN(t, c, "leaky-latest", "project", time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	f := filepath.Join(agentMemDir(), "projects", c.Project.ID, "leaky-latest.md")
	raw, _ := os.ReadFile(f)
	if err := os.WriteFile(f, []byte(strings.Replace(string(raw), "\nb\n", "\n"+agentMemFakeAWS()+"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, b := memberDo(t, http.MethodGet, "/agents/memory/entries/member-latest?scope=project&project="+c.Project.ID+"&name=leaky-latest", nil)
	if code != http.StatusUnprocessableEntity || strings.Contains(string(b), "leaky-latest") || strings.Contains(string(b), c.Project.ID) || strings.Contains(string(b), agentMemFakeAWS()) {
		t.Fatalf("latest of a leaky memory: %d %s", code, b)
	}
}

// A forgotten memory whose earlier text carries a secret behind a JSON escape (raw bytes clean,
// decoded value not) is withheld like a live one: the history can hold text the save path's scan
// never saw (an adopted lineage, a hand edit). Without the decoded check this answers 200 with
// the name. An unparseable earlier text is refused too.
func TestAgentMemoryMemberLatestWithholdsForgottenSecretBehindEscape(t *testing.T) {
	trailers := func(op, name string) string {
		return "agent-memory: " + op + "\n\nAF-Trigger: agent-memory\nAF-Op: " + op + "\nAF-Memory: af/user/" + name + ".md\nAF-Author-Kind: claude\nAF-Author-Session: s\n"
	}
	for name, text := range map[string]string{
		"esc":    "---\nname: esc\ndescription: \"key \\u0041" + agentMemFakeAWS()[1:] + "\"\nrevision: 1\n---\nb\n",
		"broken": "not a memory file at all\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _ = agentMemTestEnv(t)
			agentMemCommitRaw(t, map[string]string{"af/user/" + name + ".md": text}, trailers("create", name))
			if _, err := memoryGitRun("rm", "--quiet", "--", "af/user/"+name+".md"); err != nil {
				t.Fatal(err)
			}
			if _, err := memoryGitRun("commit", "--quiet", "--no-verify", "-m", trailers("forget", name)); err != nil {
				t.Fatal(err)
			}
			code, b := memberDo(t, http.MethodGet, "/agents/memory/entries/member-latest?scope=user&name="+name, nil)
			if code != http.StatusUnprocessableEntity || strings.Contains(string(b), name) {
				t.Fatalf("latest of a forgotten memory with a hidden secret: %d %s", code, b)
			}
		})
	}
}
