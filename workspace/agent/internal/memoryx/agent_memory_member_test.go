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
