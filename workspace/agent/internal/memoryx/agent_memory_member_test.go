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

// Save goes through the revision check and the secret scan, and records the Console as author.
func TestAgentMemoryMemberSaveAndForget(t *testing.T) {
	_, _, _ = agentMemTestEnv(t)
	AgentMemoryEnabled = func() bool { return false }
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "proj-one", "project", now)
	pid := c.Project.ID
	save := func(rev int, body string) (int, []byte) {
		return memberDo(t, http.MethodPost, "/agents/memory/entries/member-save", map[string]any{
			"scope": "project", "project": pid, "name": "proj-one", "description": "fixed", "type": "feedback", "body": body, "revision": rev,
		})
	}
	if code, b := save(0, "x"); code != http.StatusConflict {
		t.Fatalf("blind create over an existing memory: %d %s", code, b)
	}
	if code, b := save(9, "x"); code != http.StatusConflict {
		t.Fatalf("stale revision: %d %s", code, b)
	}
	secret := agentMemFakeAWS()
	if code, b := save(1, "key "+secret); code != http.StatusUnprocessableEntity || strings.Contains(string(b), secret) {
		t.Fatalf("secret save: %d %s", code, b)
	}
	if code, b := save(1, "corrected"); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, b)
	}
	e, err := agentMemMemberRead("project", pid, "proj-one")
	if err != nil || e.Body != "corrected" || e.Revision != 2 || e.AuthorKind != "member" || e.AuthorSession != "console" {
		t.Fatalf("after save: %+v %v", e, err)
	}
	// A project the store does not hold is not created from the Console.
	code, _ := memberDo(t, http.MethodPost, "/agents/memory/entries/member-save", map[string]any{
		"scope": "project", "project": "ghost-0123456789ab", "name": "n", "description": "d", "type": "user", "body": "b",
	})
	if code != http.StatusNotFound {
		t.Fatalf("save into an unknown project: %d", code)
	}
	// User scope: create, then forget needs the revision that was read.
	code, b := memberDo(t, http.MethodPost, "/agents/memory/entries/member-save", map[string]any{
		"scope": "user", "name": "mine", "description": "d", "type": "user", "body": "b",
	})
	if code != http.StatusOK {
		t.Fatalf("user create: %d %s", code, b)
	}
	forget := func(rev int) (int, []byte) {
		return memberDo(t, http.MethodPost, "/agents/memory/entries/member-forget", map[string]any{"scope": "user", "name": "mine", "revision": rev})
	}
	if code, _ := forget(0); code != http.StatusBadRequest {
		t.Fatalf("forget without a revision: %d", code)
	}
	if code, _ := forget(5); code != http.StatusConflict {
		t.Fatalf("forget at a stale revision: %d", code)
	}
	if code, b := forget(1); code != http.StatusOK {
		t.Fatalf("forget: %d %s", code, b)
	}
	// The forget is the newest change, by the Console, and revert brings the memory back.
	ch, err := agentMemListChanges(5)
	if err != nil || len(ch.Changes) == 0 || ch.Changes[0].Op != "forget" || ch.Changes[0].AuthorSession != "console" || ch.Changes[0].AuthorKind != "member" {
		t.Fatalf("changes: %+v %v", ch, err)
	}
	if _, err := agentMemRevert(agentMemRevertReq{Commit: ch.Changes[0].Commit}, now); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := agentMemMemberRead("user", "", "mine"); err != nil {
		t.Fatalf("after restore: %v", err)
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
