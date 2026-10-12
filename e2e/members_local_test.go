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

// TestCPEnvironIsolated checks the test CP cannot inherit a deployment from the shell: a
// database, a runtime or a custodian setting exported there is dropped, docker access is
// kept. bootMemberFleet then runs with those set: a CP that inherited the Postgres URL
// would fail to boot, and one that inherited AF_DB would create the file outside dataDir.
func TestCPEnvironIsolated(t *testing.T) {
	got := cpEnviron([]string{
		"PATH=/bin", "HOME=/h", "DOCKER_HOST=unix:///d.sock", "LC_ALL=C",
		"AF_DB=/x.db", "AF_DATABASE_URL=postgres://u@db.invalid/x", "AF_DB_HOST=db.invalid",
		"AF_RUNTIME=ecs", "AUTH=oauth", "AF_KEY_CUSTODIAN=kms", "AF_CP_INTERNAL_LISTEN=:1", "WS_ENV=A=B",
	})
	want := []string{"PATH=/bin", "HOME=/h", "DOCKER_HOST=unix:///d.sock", "LC_ALL=C"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("cpEnviron kept %q, want %q", got, want)
	}

	outside := filepath.Join(t.TempDir(), "outside.db")
	t.Setenv("AF_DB", outside)
	t.Setenv("AF_DATABASE_URL", "postgres://e2e@db.invalid:1/none?sslmode=disable")
	t.Setenv("AF_DB_HOST", "db.invalid")
	t.Setenv("AF_RUNTIME", "ecs")
	f := bootMemberFleet(t, envOr("WS_IMAGE", "agent-fleet/workspace:dev"))
	f.addMember("alice")
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("the CP wrote the database named by the shell's AF_DB (%s): %v", outside, err)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "e2e-cp.db")); err != nil {
		t.Fatalf("the CP's database is not inside its data dir: %v", err)
	}
}

// TestCheckStopped is requireStopped's negative control: only the states without a
// running container pass.
func TestCheckStopped(t *testing.T) {
	for _, s := range []string{"none", "stopped"} {
		if err := checkStopped(s); err != nil {
			t.Fatalf("%s rejected: %v", s, err)
		}
	}
	for _, s := range []string{"starting", "running", "", "<nil>", "unknown"} {
		if checkStopped(s) == nil {
			t.Fatalf("state %q accepted as stopped", s)
		}
	}
}
