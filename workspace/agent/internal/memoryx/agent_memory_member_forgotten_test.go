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

func forgottenGet(t *testing.T, query string) (int, agentMemForgottenWire, string) {
	t.Helper()
	code, b := memberDo(t, http.MethodGet, "/agents/memory/entries/member-forgotten"+query, nil)
	var w agentMemForgottenWire
	_ = json.Unmarshal(b, &w)
	return code, w, string(b)
}

func saveScoped(t *testing.T, c agentMemCaller, name, scope string, at time.Time) {
	t.Helper()
	if _, err := agentMemSave(c, agentMemSaveReq{Name: name, Description: "d " + name, Scope: scope, Body: "b"}, at); err != nil {
		t.Fatal(err)
	}
}

func forgetViaRevert(t *testing.T, name, scope string, c agentMemCaller, at time.Time) {
	t.Helper()
	rel := "user/" + name + ".md"
	if scope == agentMemScopeProject {
		rel = "projects/" + c.Project.ID + "/" + name + ".md"
	}
	commit, err := agentMemLatestChange(rel)
	if err != nil || commit == "" {
		t.Fatalf("latest of %s: %q %v", rel, commit, err)
	}
	if _, err := agentMemRevert(agentMemRevertReq{Commit: commit, Forget: true}, at); err != nil {
		t.Fatalf("forget %s: %v", name, err)
	}
}

// A memory forgotten before any window of the changes list is listed from its tombstone, in
// every scope or in one, with who forgot it and a commit a restore can name. Without the route
// the only source is the 300 newest changes.
func TestAgentMemoryMemberForgottenListsOutsideTheWindow(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	saveScoped(t, c, "old-proj", "project", now)
	saveScoped(t, c, "old-user", "user", now)
	saveScoped(t, c, "kept", "user", now)
	forgetViaRevert(t, "old-proj", "project", c, now.Add(time.Minute))
	forgetViaRevert(t, "old-user", "user", c, now.Add(2*time.Minute))
	for i, n := range []string{"n1", "n2", "n3"} {
		saveScoped(t, c, n, "project", now.Add(time.Duration(i+3)*time.Minute))
	}
	win, err := agentMemListChanges(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range win.Changes {
		if strings.HasPrefix(ch.Name, "old-") {
			t.Fatalf("fixture: a forget is inside the window: %+v", win.Changes)
		}
	}
	AgentMemoryEnabled = func() bool { return false }

	code, w, body := forgottenGet(t, "")
	if code != http.StatusOK || len(w.Forgotten) != 2 || w.Withheld != 0 || w.Truncated {
		t.Fatalf("all scopes: %d %s", code, body)
	}
	if w.Forgotten[0].Name != "old-user" || w.Forgotten[1].Name != "old-proj" {
		t.Fatalf("newest forget first: %s", body)
	}
	p := w.Forgotten[1]
	if p.Op != "forget" || p.Live || !p.Revertible || !p.Latest || p.Project == nil || p.Project.ID != c.Project.ID ||
		p.AuthorKind != "member" || p.AuthorSession != "console" || p.Commit == "" || p.At == "" {
		t.Fatalf("project row: %+v", p)
	}
	if code, w, body = forgottenGet(t, "?scope=user"); code != http.StatusOK || len(w.Forgotten) != 1 || w.Forgotten[0].Name != "old-user" {
		t.Fatalf("user scope: %d %s", code, body)
	}
	if code, w, body = forgottenGet(t, "?scope=project&project="+c.Project.ID); code != http.StatusOK || len(w.Forgotten) != 1 || w.Forgotten[0].Name != "old-proj" {
		t.Fatalf("project scope: %d %s", code, body)
	}
	for _, q := range []string{"?scope=other", "?scope=project&project=nope", "?scope=project&project=../x"} {
		if code, _, _ := forgottenGet(t, q); code < 400 {
			t.Errorf("%s: status %d", q, code)
		}
	}
	// The listed commit goes through the existing revert and brings the memory back; the list
	// then no longer has it.
	if _, err := agentMemRevert(agentMemRevertReq{Commit: p.Commit}, now.Add(time.Hour)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, w, body = forgottenGet(t, ""); len(w.Forgotten) != 1 || w.Forgotten[0].Name != "old-user" {
		t.Fatalf("after restore: %s", body)
	}
	if got := agentMemLoadUsage("projects/" + c.Project.ID)["old-proj"]; got != 0 {
		t.Errorf("listing counted a use: %d", got)
	}
}

// Tombstones are untrusted files: a symlink, a directory, an oversized file or a name that is
// not a memory name is no tombstone, and a name that is a secret is counted but never listed.
func TestAgentMemoryMemberForgottenRefusesUntrustedTombstones(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	saveScoped(t, c, "gone", "user", now)
	forgetViaRevert(t, "gone", "user", c, now.Add(time.Minute))
	saveScoped(t, c, "linked", "user", now)
	forgetViaRevert(t, "linked", "user", c, now.Add(2*time.Minute))
	tomb := filepath.Join(agentMemDir(), "user", agentMemTombDir)
	// "linked" becomes a short symlink (its own size is under the size limit, so only the
	// type check can refuse it); "big" is oversized; "Bad Name" is not a name; the AWS-looking
	// name is a secret.
	if err := os.Remove(filepath.Join(tomb, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gone", filepath.Join(tomb, "linked")); err != nil {
		t.Fatal(err)
	}
	secretName := strings.ToLower(agentMemFakeAWS())
	for n, content := range map[string]string{"big": strings.Repeat("1", 4096), "Bad Name": "1\n", secretName: "1\n"} {
		if err := os.WriteFile(filepath.Join(tomb, n), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(tomb, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, w, body := forgottenGet(t, "")
	if code != http.StatusOK || len(w.Forgotten) != 1 || w.Forgotten[0].Name != "gone" {
		t.Fatalf("untrusted tombstones: %d %s", code, body)
	}
	if w.Withheld < 1 || strings.Contains(body, secretName) || strings.Contains(body, "linked") || strings.Contains(body, "Bad Name") {
		t.Fatalf("withheld name leaked or uncounted: %s", body)
	}
	// A symlinked .forgotten directory lists nothing.
	if err := os.RemoveAll(tomb); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), tomb); err != nil {
		t.Fatal(err)
	}
	if code, w, body = forgottenGet(t, "?scope=user"); code != http.StatusOK || len(w.Forgotten) != 0 {
		t.Fatalf("symlinked tombstone dir: %d %s", code, body)
	}
}

// A forgotten memory whose earlier text hides a secret behind a JSON escape is counted, not
// listed; a memory that was re-created (live again) is not listed even if a tombstone lingers.
func TestAgentMemoryMemberForgottenWithholdsAndSkipsLive(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	trailers := func(op, name string) string {
		return "agent-memory: " + op + "\n\nAF-Trigger: agent-memory\nAF-Op: " + op + "\nAF-Memory: af/user/" + name + ".md\nAF-Author-Kind: claude\nAF-Author-Session: s\n"
	}
	agentMemCommitRaw(t, map[string]string{"af/user/esc.md": "---\nname: esc\ndescription: \"key \\u0041" + agentMemFakeAWS()[1:] + "\"\nrevision: 1\n---\nb\n"}, trailers("create", "esc"))
	if _, err := memoryGitRun("rm", "--quiet", "--", "af/user/esc.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryGitRun("commit", "--quiet", "--no-verify", "-m", trailers("forget", "esc")); err != nil {
		t.Fatal(err)
	}
	tomb, err := agentMemCheckDir("user/"+agentMemTombDir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"esc", "never-published"} {
		if err := os.WriteFile(filepath.Join(tomb, n), []byte("1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	saveScoped(t, c, "back", "user", now)
	if err := os.WriteFile(filepath.Join(tomb, "back"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, w, body := forgottenGet(t, "?scope=user")
	if code != http.StatusOK || len(w.Forgotten) != 0 || w.Withheld != 1 || strings.Contains(body, "esc") || strings.Contains(body, "back") {
		t.Fatalf("withheld and live: %d %s", code, body)
	}
}
