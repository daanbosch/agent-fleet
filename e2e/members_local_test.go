//go:build e2e

// The parts of the multi-member harness that need no docker: the git remote and the
// AUTH=proxy tenant/membership setup. They run wherever the e2e module runs, so a broken
// harness shows up here before the docker scenario spends a workspace start on it.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitRemote clones, pushes and fetches through the harness's git remote on loopback.
func TestGitRemote(t *testing.T) {
	r := newGitRemote(t, "127.0.0.1")
	url := r.newRepo(t, "fixture")

	one := filepath.Join(t.TempDir(), "one")
	runGit(t, "", "clone", "-q", url, one)
	if b, err := os.ReadFile(filepath.Join(one, "README.md")); err != nil || len(b) == 0 {
		t.Fatalf("clone has no README.md: %v", err)
	}
	runGit(t, one, "switch", "-q", "-c", "topic")
	if err := os.WriteFile(filepath.Join(one, "topic.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, one, "add", "topic.txt")
	runGit(t, one, "commit", "-q", "-m", "topic")
	runGit(t, one, "push", "-q", "-u", "origin", "HEAD")
	head := runGit(t, one, "rev-parse", "HEAD")

	two := filepath.Join(t.TempDir(), "two")
	runGit(t, "", "clone", "-q", url, two)
	if got := runGit(t, two, "rev-parse", "origin/topic"); got != head {
		t.Fatalf("second clone sees origin/topic at %s, want %s", got, head)
	}
}

// TestMemberFleetSetup boots the CP with AUTH=proxy (no workspace) and checks that the
// invited members resolve into the test's tenant by header alone, and that an address
// nobody invited is refused rather than provisioned.
func TestMemberFleetSetup(t *testing.T) {
	f := bootMemberFleet(t, envOr("WS_IMAGE", "agent-fleet/workspace:dev"))
	a, b := f.addMember("alice"), f.addMember("bob")
	if a.key == b.key {
		t.Fatalf("two members share the user key %q", a.key)
	}
	for _, m := range []*member{a, b} {
		code, body := m.do("GET", "/api/tenants", nil)
		if code != 200 {
			t.Fatalf("%s: GET /api/tenants: %d %s", m.name, code, truncate(body))
		}
		if !strings.Contains(string(body), `"`+f.slug+`"`) {
			t.Fatalf("%s: tenant %s not among %s", m.name, f.slug, truncate(body))
		}
		who := m.getJSON("/api/whoami")
		if who["resolved_user"] != m.key {
			t.Fatalf("%s: whoami resolved_user = %v, want %q", m.name, who["resolved_user"], m.key)
		}
	}
	stranger := &member{f: f, t: t, name: "stranger", email: "stranger-" + f.slug + "@" + emailDomain}
	got := stranger.call("GET", "/api/session-handoff-offers/received", nil, 403)
	if code, _ := apiErr(got); code != "not_provisioned" {
		t.Fatalf("uninvited address: error %q, want not_provisioned", code)
	}
}
