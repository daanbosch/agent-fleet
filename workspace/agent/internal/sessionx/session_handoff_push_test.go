package sessionx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// pushFixture is a working copy cloned from a bare origin with main pushed and tracked. HOME
// is a temp dir, so the developer's global git config (hooksPath, credential helpers) plays
// no part.
func pushFixture(t *testing.T) (work, origin string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}
	work = filepath.Join(root, "work")
	gitInit(t, work)
	gitRun(t, work, "remote", "add", "origin", origin)
	gitRun(t, work, "push", "-q", "-u", "origin", "main")
	return work, origin
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// remoteRef is the sha of ref in the bare remote, "" when it does not exist there.
func remoteRef(dir, ref string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-qm", msg)
}

// TestHandoffPushNoUpstreamSetsUpstream: the most common handoff state, a branch never
// pushed. The push publishes that branch alone and records its upstream, and the rebuilt
// verdict lets the offer through. Another unpushed branch and a tag stay local.
func TestHandoffPushNoUpstreamSetsUpstream(t *testing.T) {
	work, origin := pushFixture(t)
	gitRun(t, work, "checkout", "-q", "-b", "temp/work")
	commitFile(t, work, "a", "one")
	gitRun(t, work, "tag", "v-local")
	gitRun(t, work, "checkout", "-q", "feature")
	commitFile(t, work, "b", "other branch")
	gitRun(t, work, "checkout", "-q", "temp/work")
	// Uncommitted work is not pushed, nor committed.
	if err := os.WriteFile(filepath.Join(work, "dirty"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := buildHandoffContext(work)
	if c.Blocked != handoffBlockNoUpstream || c.PushRemote != "origin" || c.PushCommits != 1 {
		t.Fatalf("before: blocked=%q pushRemote=%q pushCommits=%d, want no_upstream origin 1", c.Blocked, c.PushRemote, c.PushCommits)
	}
	head := gitOut(t, work, "rev-parse", "HEAD")

	res, perr := pushForHandoff(context.Background(), work)
	if perr != nil {
		t.Fatalf("push refused: %+v", perr)
	}
	if !res.Pushed || res.Remote != "origin" || res.Branch != "temp/work" {
		t.Fatalf("result = %+v", res)
	}
	if got := remoteRef(origin, "refs/heads/temp/work"); got != head {
		t.Fatalf("origin temp/work = %q, want %s", got, head)
	}
	if got := gitOut(t, work, "rev-parse", "--abbrev-ref", "temp/work@{u}"); got != "origin/temp/work" {
		t.Fatalf("upstream = %q, want origin/temp/work", got)
	}
	if res.Context.Blocked != "" || res.Context.HeadSha != head || res.Context.Warning != handoffWarnDirty {
		t.Fatalf("after: %+v, want the gate open on %s with the dirty warning kept", res.Context, head)
	}
	if got := remoteRef(origin, "refs/heads/feature"); got != "" {
		t.Fatalf("the other branch reached origin (%s); only the current branch may be pushed", got)
	}
	if got := remoteRef(origin, "refs/tags/v-local"); got != "" {
		t.Fatalf("a tag reached origin (%s)", got)
	}
	if st := gitOut(t, work, "status", "--porcelain"); !strings.Contains(st, "?? dirty") {
		t.Fatalf("the uncommitted file was touched: %q", st)
	}
}

func TestHandoffPushUnpushedCommits(t *testing.T) {
	work, origin := pushFixture(t)
	commitFile(t, work, "a", "one")
	commitFile(t, work, "b", "two")
	c := buildHandoffContext(work)
	if c.Blocked != handoffBlockUnpushed || c.PushRemote != "origin" || c.PushCommits != 2 {
		t.Fatalf("before: blocked=%q pushRemote=%q pushCommits=%d", c.Blocked, c.PushRemote, c.PushCommits)
	}
	res, perr := pushForHandoff(context.Background(), work)
	if perr != nil {
		t.Fatalf("push refused: %+v", perr)
	}
	if head := gitOut(t, work, "rev-parse", "HEAD"); remoteRef(origin, "refs/heads/main") != head || res.Context.Blocked != "" {
		t.Fatalf("origin main = %q, want %s; after = %+v", remoteRef(origin, "refs/heads/main"), head, res.Context)
	}
}

// TestHandoffPushDivergedIsRefused: someone else pushed first. Never force: git's rejection
// comes back as it reported it and the remote keeps their commit.
func TestHandoffPushDivergedIsRefused(t *testing.T) {
	work, origin := pushFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	if out, err := exec.Command("git", "clone", "-q", origin, other).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	commitFile(t, other, "theirs", "theirs")
	gitRun(t, other, "push", "-q", "origin", "main")
	theirs := gitOut(t, other, "rev-parse", "HEAD")

	commitFile(t, work, "mine", "mine")
	_, perr := pushForHandoff(context.Background(), work)
	if perr == nil || perr.code != "push_rejected" || !strings.Contains(perr.message, "rejected") {
		t.Fatalf("diverged push = %+v, want push_rejected carrying git's message", perr)
	}
	if got := remoteRef(origin, "refs/heads/main"); got != theirs {
		t.Fatalf("origin main = %s, want their commit %s kept", got, theirs)
	}
}

func TestHandoffPushWithoutOriginIsRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	gitInit(t, dir)
	c := buildHandoffContext(dir)
	if c.Blocked != handoffBlockNoUpstream || c.PushRemote != "" {
		t.Fatalf("blocked=%q pushRemote=%q, want no_upstream and nothing to push to", c.Blocked, c.PushRemote)
	}
	if _, perr := pushForHandoff(context.Background(), dir); perr == nil || perr.code != "no_push_remote" {
		t.Fatalf("push without origin = %+v, want no_push_remote", perr)
	}
}

func TestHandoffPushDetachedIsNotOffered(t *testing.T) {
	work, origin := pushFixture(t)
	commitFile(t, work, "a", "one")
	gitRun(t, work, "checkout", "-q", "--detach")
	c := buildHandoffContext(work)
	if c.Blocked != handoffBlockDetached || c.PushRemote != "" || c.PushCommits != 0 {
		t.Fatalf("detached: %+v, want blocked detached_head with no push offered", c)
	}
	before := remoteRef(origin, "refs/heads/main")
	if _, perr := pushForHandoff(context.Background(), work); perr == nil || perr.code != handoffBlockDetached {
		t.Fatalf("detached push = %+v, want detached_head", perr)
	}
	if got := remoteRef(origin, "refs/heads/main"); got != before {
		t.Fatalf("origin main moved to %s", got)
	}
}

// TestHandoffPushRunsPrePushHook: the repository's own hooks run (a repo may guard its
// protected branches or scan for secrets there). A refusal reaches the user verbatim, minus
// any credential in a URL it printed.
func TestHandoffPushRunsPrePushHook(t *testing.T) {
	work, origin := pushFixture(t)
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\necho 'pre-push: blocked by policy for https://bot:s3cr3tvalue@example.com/x.git' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "config", "core.hooksPath", hooks)
	commitFile(t, work, "a", "one")
	before := remoteRef(origin, "refs/heads/main")

	_, perr := pushForHandoff(context.Background(), work)
	if perr == nil || perr.code != "push_rejected" || !strings.Contains(perr.message, "pre-push: blocked by policy") {
		t.Fatalf("hooked push = %+v, want push_rejected with the hook's output", perr)
	}
	if strings.Contains(perr.message, "s3cr3tvalue") || !strings.Contains(perr.message, "https://example.com/x.git") {
		t.Fatalf("hook output not scrubbed of the URL credential: %q", perr.message)
	}
	if got := remoteRef(origin, "refs/heads/main"); got != before {
		t.Fatalf("origin main moved to %s past a refusing hook", got)
	}
}

// A gate that is already open pushes nothing.
func TestHandoffPushNothingToDo(t *testing.T) {
	work, _ := pushFixture(t)
	res, perr := pushForHandoff(context.Background(), work)
	if perr != nil || res.Pushed || res.Context.Blocked != "" {
		t.Fatalf("clean pushed branch: res=%+v err=%+v, want pushed=false and no error", res, perr)
	}
}

func TestHandleSessionHandoffPush(t *testing.T) {
	work, origin := pushFixture(t)
	gitRun(t, work, "checkout", "-q", "-b", "temp/http")
	commitFile(t, work, "a", "one")
	session.WriteMeta(session.Meta{Name: "push1", Dir: work, Kind: session.KindClaude})
	session.WriteMeta(session.Meta{Name: "push2", Kind: session.KindClaude})
	call := func(name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/sessions/"+name+"/handoff-push", nil)
		r.SetPathValue("name", name)
		w := httptest.NewRecorder()
		HandleSessionHandoffPush(w, r)
		return w
	}
	if got := call("push2"); got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "no_working_copy") {
		t.Fatalf("no working copy: %d %s", got.Code, got.Body.String())
	}
	if got := call("nope"); got.Code != http.StatusNotFound {
		t.Fatalf("unknown session: %d %s", got.Code, got.Body.String())
	}
	got := call("push1")
	var res struct {
		Pushed  bool           `json:"pushed"`
		Context map[string]any `json:"context"`
	}
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &res) != nil || !res.Pushed {
		t.Fatalf("push: %d %s", got.Code, got.Body.String())
	}
	if res.Context["headSha"] != remoteRef(origin, "refs/heads/temp/http") || res.Context["blocked"] != nil {
		t.Fatalf("context after push = %v", res.Context)
	}
}

func TestScrubPushOutput(t *testing.T) {
	in := "To https://x-access-token:ghp_abcdefghijklmnopqrstuvwxyz0123@github.com/o/r.git\n ! [rejected] main -> main (fetch first)"
	got := scrubPushOutput(in)
	if strings.Contains(got, "ghp_") || strings.Contains(got, "x-access-token") {
		t.Fatalf("credential survived: %q", got)
	}
	if !strings.Contains(got, "https://github.com/o/r.git") || !strings.Contains(got, "[rejected] main -> main (fetch first)") {
		t.Fatalf("git's message was mangled: %q", got)
	}
}
