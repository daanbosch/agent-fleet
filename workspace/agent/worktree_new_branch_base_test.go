package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/gitx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/sessionx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/testguard"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/tmuxx"
)

// newBranchBaseFixture is a bare origin, the clone that pushes to it ("up", standing in
// for the handover sender) and the parent clone a new-branch worktree is cut from (the
// recipient). It returns a server for POST /sessions.
func newBranchBaseFixture(t *testing.T) (up, parent string, srv *httptest.Server) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	testguard.IsolateTmux(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AF_SESSIONS_DIR", filepath.Join(home, "sessions"))

	origin := filepath.Join(home, "origin.git")
	runIntegrationGit(t, home, "init", "--quiet", "--bare", "-b", "main", origin)
	up = filepath.Join(home, "up")
	gitInit(t, up) // main, plus a local "feature"
	runIntegrationGit(t, up, "remote", "add", "origin", origin)
	runIntegrationGit(t, up, "push", "--quiet", "origin", "main", "feature")

	parent = filepath.Join(home, "repos", "app")
	if err := os.MkdirAll(filepath.Dir(parent), 0o755); err != nil {
		t.Fatal(err)
	}
	runIntegrationGit(t, home, "clone", "--quiet", origin, parent)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", sessionx.HandleCreateSession)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return up, parent, srv
}

// pushBranch commits on a new branch in up and pushes it, returning its tip.
func pushBranch(t *testing.T, up, branch string) string {
	t.Helper()
	runIntegrationGit(t, up, "checkout", "--quiet", "-b", branch, "main")
	commitIntegrationFile(t, up, strings.ReplaceAll(branch, "/", "-")+".txt")
	runIntegrationGit(t, up, "push", "--quiet", "origin", branch)
	runIntegrationGit(t, up, "checkout", "--quiet", "main")
	return gitRev(t, up, branch)
}

func launchNewBranch(t *testing.T, srv *httptest.Server, parent, base, newBranch string) session.Session {
	t.Helper()
	var created session.Session
	do(t, srv, "POST", "/sessions", map[string]any{
		"worktree": true, "dir": parent, "branch": base, "new_branch": newBranch, "kind": "shell",
	}, http.StatusCreated, &created)
	t.Cleanup(func() { _ = tmuxx.Cmd("kill-session", "-t", session.TmuxName(created.Name)).Run() })
	return created
}

// A new-branch launch whose base was pushed after the parent's last fetch: the case a
// handover recipient hits by default, since their clone has never fetched the sender's
// branch. It must fetch once and fork from the base's tip, not fail with "invalid
// reference".
func TestCreateSessionNewBranchFetchesUnfetchedBase(t *testing.T) {
	up, parent, srv := newBranchBaseFixture(t)
	tip := pushBranch(t, up, "handover/x")
	if local, remote := gitx.BranchNameStatus(parent, "handover/x"); local || remote {
		t.Fatalf("precondition: the parent already knows handover/x (local=%v remote=%v)", local, remote)
	}

	created := launchNewBranch(t, srv, parent, "handover/x", "take/x")

	if got := gitRev(t, created.Dir, "HEAD"); got != tip {
		t.Errorf("worktree started at %s, want handover/x's tip %s", got, tip)
	}
	if br, _ := gitx.Run(created.Dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "take/x" {
		t.Errorf("worktree is on %q, want take/x", br)
	}
	// Forking from a remote-tracking ref must not make the new branch track the base:
	// a later `git push` would then target handover/x instead of creating take/x.
	if up, err := gitx.Run(created.Dir, "rev-parse", "--abbrev-ref", "take/x@{upstream}"); err == nil {
		t.Errorf("take/x tracks %q; a fresh session branch must have no upstream", up)
	}
	if gitx.GitBranchExists(parent, "handover/x") {
		t.Error("the launch created a local handover/x in the parent; only origin/* may move")
	}
}

// A base that exists nowhere still fails, and says which base it could not find.
func TestCreateSessionNewBranchMissingBaseFails(t *testing.T) {
	_, parent, srv := newBranchBaseFixture(t)
	code, body := roundtrip(t, srv, "POST", "/sessions", map[string]any{
		"worktree": true, "dir": parent, "branch": "no-such-base", "new_branch": "take/y", "kind": "shell",
	})
	if code == http.StatusCreated {
		t.Fatalf("launch off a base that exists nowhere succeeded: %s", body)
	}
	if !strings.Contains(string(body), "no-such-base") {
		t.Errorf("error does not name the missing base: %s", body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(parent), "app@take-y")); err == nil {
		t.Error("a failed launch left its worktree folder behind")
	}
}

// A base that already resolves — a local branch, or a remote-tracking one the parent
// has fetched but never checked out — forks with no network round trip. The detector
// is a branch pushed after the clone: any fetch would bring origin/canary in. The last
// launch is the positive control: an unresolvable base does fetch, and the detector
// sees it.
func TestCreateSessionNewBranchResolvableBaseDoesNotFetch(t *testing.T) {
	up, parent, srv := newBranchBaseFixture(t)
	pushBranch(t, up, "canary")
	fetched := func() bool { _, remote := gitx.BranchNameStatus(parent, "canary"); return remote }

	created := launchNewBranch(t, srv, parent, "main", "from-main")
	if got, want := gitRev(t, created.Dir, "HEAD"), gitRev(t, parent, "main"); got != want {
		t.Errorf("from-main started at %s, want main %s", got, want)
	}
	if fetched() {
		t.Fatal("a launch off the local main fetched")
	}

	// "feature" exists only as origin/feature in the parent (clone checks out main alone).
	if gitx.GitBranchExists(parent, "feature") {
		t.Fatal("precondition: the parent has a local feature")
	}
	created = launchNewBranch(t, srv, parent, "feature", "from-feature")
	if got, want := gitRev(t, created.Dir, "HEAD"), gitRev(t, parent, "origin/feature"); got != want {
		t.Errorf("from-feature started at %s, want origin/feature %s", got, want)
	}
	if br, _ := gitx.Run(created.Dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "from-feature" {
		t.Errorf("worktree is on %q, want from-feature", br)
	}
	if up, err := gitx.Run(created.Dir, "rev-parse", "--abbrev-ref", "from-feature@{upstream}"); err == nil {
		t.Errorf("from-feature tracks %q; a fresh session branch must have no upstream", up)
	}
	if fetched() {
		t.Fatal("a launch off the fetched origin/feature fetched")
	}

	pushBranch(t, up, "late")
	launchNewBranch(t, srv, parent, "late", "from-late")
	if !fetched() {
		t.Fatal("positive control: the unfetched base did not fetch, so the detector proves nothing")
	}
}
