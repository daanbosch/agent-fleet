package main

import (
	"github.com/k-k1/agent-fleet/workspace/agent/internal/gitx"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestListReposRealCopies times GET /repos over a real ~/repos tree. It is opt-in
// (AF_BENCH_REPOS_ROOT=<dir holding the working copies>) because it needs a populated
// workspace; the numbers in the PR that introduced the cache came from it. The copies are
// reached through a symlink under a scratch HOME and are only read.
func TestListReposRealCopies(t *testing.T) {
	root := os.Getenv("AF_BENCH_REPOS_ROOT")
	if root == "" {
		t.Skip("AF_BENCH_REPOS_ROOT not set")
	}
	home := t.TempDir()
	if err := os.Symlink(root, filepath.Join(home, "repos")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	call := func() (time.Duration, int) {
		start := time.Now()
		rec := httptest.NewRecorder()
		gitx.HandleListRepos(rec, httptest.NewRequest("GET", "/repos", nil))
		if dump := os.Getenv("AF_BENCH_DUMP"); dump != "" {
			_ = os.WriteFile(dump, rec.Body.Bytes(), 0o600)
		}
		return time.Since(start), rec.Body.Len()
	}
	for _, label := range []string{"cold", "warm-1", "warm-2", "warm-3", "after-11s"} {
		if label == "after-11s" {
			time.Sleep(11 * time.Second) // past the status max age: git status runs again, nothing else
		}
		d, n := call()
		t.Logf("%-7s %8.0f ms  %d bytes", label, float64(d.Microseconds())/1000, n)
	}
}
