package memoryx

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func TestAgentMemoryNormalizeRemote(t *testing.T) {
	const want = "github.com/k-k1/agent-fleet"
	for _, in := range []string{
		"git@github.com:k-k1/agent-fleet.git",
		"git@github.com:k-k1/agent-fleet",
		"ssh://git@github.com/k-k1/agent-fleet.git",
		"ssh://git@github.com:2222/k-k1/agent-fleet.git/",
		"https://github.com/k-k1/agent-fleet.git",
		"https://github.com/k-k1/agent-fleet/",
		"HTTPS://GitHub.com/k-k1/agent-fleet",
		"https://alice:s3cret-token@github.com/k-k1/agent-fleet.git",
		"https://x-access-token:ghp_abcdef@github.com:443/k-k1/agent-fleet",
		" git@GITHUB.com:k-k1/agent-fleet.git\n",
	} {
		got := agentMemNormalizeRemote(in)
		if got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "@") || strings.Contains(got, "s3cret") || strings.Contains(got, "ghp_") || strings.Contains(got, "alice") {
			t.Errorf("normalize(%q) = %q keeps a credential", in, got)
		}
	}
	for _, in := range []string{"", "/srv/git/repo.git", "../repo", "file:///srv/git/repo.git", "repo", "https://github.com", "https://github.com/", "git@github.com:", "https://h/a b"} {
		if got := agentMemNormalizeRemote(in); got != "" {
			t.Errorf("normalize(%q) = %q, want none", in, got)
		}
	}
	// A generic ssh server reads "host:team/repo" relative to the login home and
	// "ssh://host/team/repo" as absolute: they must not be made equal, so ssh is not recorded there.
	for _, in := range []string{"git@forge.invalid:team/repo.git", "ssh://git@forge.invalid/team/repo.git", "alice@forge.invalid:team/repo", "git://forge.invalid/team/repo"} {
		if got := agentMemNormalizeRemote(in); got != "" {
			t.Errorf("normalize(%q) = %q, want none", in, got)
		}
	}
	if got := agentMemNormalizeRemote("https://forge.invalid:8443/team/repo.git"); got != "forge.invalid/team/repo" {
		t.Errorf("https on a generic host = %q", got)
	}
	if agentMemNormalizeRemote("git@github.com:x/y.git") == agentMemNormalizeRemote("git@github.com:x/z.git") {
		t.Error("different repositories must not match")
	}
}

func remoteGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// remoteRepo makes ~/repos/<name> a git repository with the given origin and a session in it.
func remoteRepo(t *testing.T, home, name, origin string) string {
	t.Helper()
	dir := filepath.Join(home, "repos", name)
	memoryMkdirAll(t, dir)
	remoteGit(t, dir, "init", "--quiet", "-b", "main")
	if origin != "" {
		remoteGit(t, dir, "remote", "add", "origin", origin)
	}
	session.WriteMeta(session.Meta{Name: "claude-" + name, Dir: dir, Kind: "claude"})
	return dir
}

func projectJSON(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(agentMemDir(), "projects", id, "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The first memory records the normalised remote and never the credentials of the URL.
func TestAgentMemoryProjectRecordsRemoteWithoutCredentials(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	remoteRepo(t, home, "cred", "https://alice:s3cret-token@github.com/k-k1/cred.git")
	c := agentMemCallerT(t, "claude-cred")
	agentMemSaveN(t, c, "one", "project", time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	got := projectJSON(t, c.Project.ID)
	if !strings.Contains(got, `"remote": "github.com/k-k1/cred"`) {
		t.Errorf("project.json lacks the remote: %s", got)
	}
	for _, bad := range []string{"s3cret", "alice", "@", "https://"} {
		if strings.Contains(got, bad) {
			t.Errorf("project.json holds %q: %s", bad, got)
		}
	}
	if p := agentMemProjectInfo(c.Project.ID); p.Remote != "github.com/k-k1/cred" {
		t.Errorf("info remote = %q", p.Remote)
	}
}

// A secret-looking path in the remote must not stop the project's first memory from being saved.
func TestAgentMemorySecretLookingRemoteDoesNotBlockTheSave(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	remoteRepo(t, home, "sec", "https://github.com/ghp_0123456789abcdefghijklmnopqrstuvwxyzAB/x.git")
	c := agentMemCallerT(t, "claude-sec")
	agentMemSaveN(t, c, "one", "project", time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	if got := projectJSON(t, c.Project.ID); strings.Contains(got, "ghp_") {
		t.Errorf("the secret-looking remote was recorded: %s", got)
	}
	// Nor does it leave through the caller, the projects list or a stored record read back.
	if c.Project.Remote != "" {
		t.Errorf("caller carries %q", c.Project.Remote)
	}
	out, err := agentMemMemberProjects()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), "ghp_") {
		t.Errorf("projects list leaks the token: %s", b)
	}
	// A hand-edited record naming the token is not trusted either.
	bad := strings.Replace(projectJSON(t, c.Project.ID), `"display"`, `"remote": "github.com/ghp_0123456789abcdefghijklmnopqrstuvwxyzAB/x", "display"`, 1)
	if err := os.WriteFile(filepath.Join(agentMemDir(), "projects", c.Project.ID, "project.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := agentMemProjectInfo(c.Project.ID); p.Remote != "" {
		t.Errorf("stored record handed out %q", p.Remote)
	}
}

// A live project that has no memory yet must not list a flagged remote either.
func TestAgentMemoryLiveProjectNeverListsASecretRemote(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	remoteRepo(t, home, "livesec", "https://github.com/ghp_0123456789abcdefghijklmnopqrstuvwxyzAB/x.git")
	out, err := agentMemMemberProjects()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), "ghp_") {
		t.Errorf("projects list leaks the token: %s", b)
	}
}

// The backfill splices one member into the stored bytes: order, spacing and fields it does not
// know stay, whether it runs from a write or at start.
func TestAgentMemoryBackfillKeepsTheRecordAsItWas(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	dir := remoteRepo(t, home, "keep", "")
	c := agentMemCallerT(t, "claude-keep")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "one", "project", now)
	path := filepath.Join(agentMemDir(), "projects", c.Project.ID, "project.json")
	orig := "{\n  \"display\": \"keep\",\n  \"id\": \"" + c.Project.ID + "\",\n  \"future\": {\"a\": [1, 2]},\n  \"root\": \"" + dir + "\"\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteGit(t, dir, "remote", "add", "origin", "git@github.com:k-k1/keep.git")
	check := func(how string) {
		t.Helper()
		got := projectJSON(t, c.Project.ID)
		want := strings.TrimSuffix(orig, "\n}\n") + ",\n  \"remote\": \"github.com/k-k1/keep\"\n}\n"
		if got != want {
			t.Errorf("%s: record =\n%s\nwant\n%s", how, got, want)
		}
	}
	agentMemBackfillRemotes(now.Add(time.Hour))
	check("backfill")

	// The same through the next write, from a record that has no remote yet.
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	c = agentMemCallerT(t, "claude-keep")
	agentMemSaveN(t, c, "two", "project", now.Add(2*time.Hour))
	check("write")
}

// The backfill commit is no memory change: the changes list keeps its limit and withholds nothing.
func TestAgentMemoryBackfillCommitDoesNotDisturbTheChangesList(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	dir := remoteRepo(t, home, "lst", "")
	c := agentMemCallerT(t, "claude-lst")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "one", "project", now)
	remoteGit(t, dir, "remote", "add", "origin", "git@github.com:k-k1/lst.git")
	agentMemBackfillRemotes(now.Add(time.Hour))
	if !strings.Contains(projectJSON(t, c.Project.ID), "remote") {
		t.Fatal("the backfill did not run")
	}
	w, err := agentMemListChanges(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Changes) != 1 || w.Withheld != 0 {
		t.Errorf("changes=%d withheld=%d", len(w.Changes), w.Withheld)
	}
}

// A project.json from before the field loads, and the next write from its working copy (or the
// start-up backfill) adds the remote and keeps what the record said.
func TestAgentMemoryRemoteBackfill(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	dir := remoteRepo(t, home, "late", "")
	c := agentMemCallerT(t, "claude-late")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "one", "project", now)
	if strings.Contains(projectJSON(t, c.Project.ID), "remote") {
		t.Fatalf("no remote yet: %s", projectJSON(t, c.Project.ID))
	}
	if p := agentMemProjectInfo(c.Project.ID); p.Display != "late" || p.Remote != "" {
		t.Fatalf("old record must load: %+v", p)
	}
	remoteGit(t, dir, "remote", "add", "origin", "git@github.com:k-k1/late.git")

	agentMemBackfillRemotes(now.Add(time.Hour))
	got := projectJSON(t, c.Project.ID)
	if !strings.Contains(got, `"remote": "github.com/k-k1/late"`) || !strings.Contains(got, `"display": "late"`) {
		t.Errorf("backfilled record: %s", got)
	}
	// A second run changes nothing.
	before, _ := exec.Command("git", "-C", memoryRepoDir(), "rev-parse", "HEAD").Output()
	agentMemBackfillRemotes(now.Add(2 * time.Hour))
	after, _ := exec.Command("git", "-C", memoryRepoDir(), "rev-parse", "HEAD").Output()
	if string(before) != string(after) {
		t.Error("a second backfill committed again")
	}
	// The record's commit is not a memory change.
	body, _ := exec.Command("git", "-C", memoryRepoDir(), "log", "-1", "--format=%B").Output()
	if !strings.Contains(string(body), "AF-Op: project-info") {
		t.Fatalf("last commit is not the backfill: %s", body)
	}
	if _, _, _, ok := agentMemChangeFrom(strings.TrimSpace(string(after)), now.Format(time.RFC3339), string(body)); ok {
		t.Error("the backfill commit is listed as a memory change")
	}
}

// An orphan is suggested a move into the one live project with the same remote.
func TestAgentMemoryRemoteMatchSuggestion(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	orphanOf := func(name, origin string) string {
		dir := remoteRepo(t, home, name, origin)
		c := agentMemCallerT(t, "claude-"+name)
		agentMemSaveN(t, c, "m", "project", now)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		return c.Project.ID
	}
	row := func(id string) agentMemProjectRow {
		out, err := agentMemMemberProjects()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range out.Projects {
			if r.Project.ID == id {
				return r
			}
		}
		t.Fatalf("project %s not listed", id)
		return agentMemProjectRow{}
	}

	agentMemBackfillRemotes(now) // nothing to do, must not fail
	withRemote := orphanOf("old", "ssh://git@github.com/k-k1/shared.git")
	noRemote := orphanOf("older", "")
	live := remoteRepo(t, home, "fresh", "https://github.com/k-k1/shared")
	_ = live
	freshID := agentMemCallerT(t, "claude-fresh").Project.ID

	r := row(withRemote)
	if !r.Orphaned || r.MergeInto == nil || r.MergeInto.ID != freshID {
		t.Fatalf("orphan with a matching live remote: %+v", r)
	}
	if r := row(noRemote); !r.Orphaned || r.MergeInto != nil {
		t.Errorf("no recorded remote, no suggestion: %+v", r)
	}
	if r := row(freshID); r.MergeInto != nil {
		t.Errorf("a live project is never a source: %+v", r)
	}

	// Two live clones of the same repository: the suggestion would be a coin toss, so none.
	remoteRepo(t, home, "fresh2", "git@github.com:k-k1/shared.git")
	if r := row(withRemote); !r.Orphaned || r.MergeInto != nil {
		t.Errorf("two live projects share the remote: %+v", r)
	}
}

// A record is only extended when its decoded content is clean too: a JSON escape must not carry
// a secret into a new commit.
func TestAgentMemoryBackfillScansTheDecodedRecord(t *testing.T) {
	home, _, _ := agentMemTestEnv(t)
	dir := remoteRepo(t, home, "esc", "")
	c := agentMemCallerT(t, "claude-esc")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	agentMemSaveN(t, c, "one", "project", now)
	remoteGit(t, dir, "remote", "add", "origin", "git@github.com:k-k1/esc.git")
	path := filepath.Join(agentMemDir(), "projects", c.Project.ID, "project.json")
	const tok = `\u0067hp_0123456789abcdefghijklmnopqrstuvwxyzAB`
	for name, rec := range map[string]string{
		"root":   `{"id": "` + c.Project.ID + `", "root": "/r/` + tok + `", "display": "esc"}`,
		"nested": `{"id": "` + c.Project.ID + `", "display": "esc", "future": {"a": ["` + tok + `"]}}`,
		"key":    `{"id": "` + c.Project.ID + `", "display": "esc", "` + tok + `": 1}`,
	} {
		if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		head, _ := exec.Command("git", "-C", memoryRepoDir(), "rev-parse", "HEAD").Output()
		agentMemBackfillRemotes(now.Add(time.Hour))
		if after, _ := exec.Command("git", "-C", memoryRepoDir(), "rev-parse", "HEAD").Output(); string(head) != string(after) {
			t.Errorf("%s: a record with a hidden secret was committed", name)
		}
		if got := projectJSON(t, c.Project.ID); got != rec {
			t.Errorf("%s: record changed: %s", name, got)
		}
	}
}
