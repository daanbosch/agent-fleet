package gitx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// twoRemoteClone builds a clone whose second remote ("aaa", sorting before origin)
// also has main and topic/x, with no local main — the shape behind #2268. aaa/main
// is made older than origin/main so the picker showing the wrong one is visible.
func twoRemoteClone(t *testing.T) (dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	seed := filepath.Join(home, "seed")
	gitInit(t, seed) // main + feature
	gitAt(t, seed, "branch", "topic/x")
	for _, r := range []string{"origin", "aaa"} {
		gitAt(t, home, "clone", "--bare", seed, r+".git")
	}
	// origin/main gets a newer commit than aaa/main.
	gitAt(t, seed, "commit", "--allow-empty", "-m", "newer on origin")
	gitAt(t, seed, "push", filepath.Join(home, "origin.git"), "main")

	dir = filepath.Join(home, "repos", "app")
	gitAt(t, home, "clone", filepath.Join(home, "origin.git"), dir)
	gitAt(t, dir, "remote", "add", "aaa", filepath.Join(home, "aaa.git"))
	gitAt(t, dir, "fetch", "aaa")
	gitAt(t, dir, "remote", "set-head", "origin", "main")
	gitAt(t, dir, "checkout", "-b", "work")
	gitAt(t, dir, "branch", "-D", "main")
	return dir
}

func TestBranchInfosTwoRemotes(t *testing.T) {
	dir := twoRemoteClone(t)
	byName := map[string]BranchInfo{}
	for _, b := range gitBranchInfos(dir, "work") {
		byName[b.Name] = b
	}
	if _, ok := byName["origin"]; ok || byName["HEAD"].Name != "" {
		t.Errorf("refs/remotes/origin/HEAD leaked into the list as origin or HEAD")
	}
	for _, n := range []string{"main", "topic/x"} {
		b, ok := byName[n]
		if !ok || !b.Remote || b.RemoteName != "origin" {
			t.Errorf("%s = %+v, want remote-only entry from origin", n, b)
		}
	}
	if b := byName["main"]; b.Subject != "newer on origin" {
		t.Errorf("main shows subject %q, want origin's tip", b.Subject)
	}
}

func TestCheckoutTwoRemotes(t *testing.T) {
	for _, c := range []struct{ name, branch, remote, wantRemote string }{
		{"bare name (old client)", "main", "", "origin"},
		{"explicit origin", "main", "origin", "origin"},
		{"explicit other remote", "main", "aaa", "aaa"},
		{"slash in branch", "topic/x", "origin", "origin"},
		{"unknown remote ignored", "main", "nope", "origin"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := twoRemoteClone(t)
			old := deps.LiveSessionsInDir
			deps.LiveSessionsInDir = func(string) []string { return nil }
			defer func() { deps.LiveSessionsInDir = old }()

			body, _ := json.Marshal(map[string]any{"branch": c.branch, "remote": c.remote})
			req := httptest.NewRequest("POST", "/repos/app/checkout", bytes.NewReader(body))
			req.SetPathValue("name", "app")
			rec := httptest.NewRecorder()
			HandleRepoCheckout(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("checkout = %d: %s", rec.Code, rec.Body)
			}
			if got := GitCurrentBranch(dir); got != c.branch {
				t.Fatalf("branch = %q, want %q", got, c.branch)
			}
			up := strings.TrimSpace(gitAt(t, dir, "rev-parse", "--abbrev-ref", c.branch+"@{upstream}"))
			if want := c.wantRemote + "/" + c.branch; up != want {
				t.Errorf("upstream = %q, want %q", up, want)
			}
		})
	}
}
