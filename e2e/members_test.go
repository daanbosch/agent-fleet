//go:build e2e

// Multi-member L2 harness: one CP with AUTH=proxy, a tenant of its own, one client per
// identity and one workspace container per member, all through the public API.
//
// AUTH=proxy takes the caller's identity from X-Forwarded-Email, so a test acts as any
// member by setting that header; SUPER_ADMIN_EMAILS makes the harness's admin identity a
// super_admin, which creates the tenant and invites the members through the admin API.
// AF_PROVISION=invite keeps an unknown address out instead of dropping it into the
// default tenant. Every request a member sends carries X-AF-Tenant.
//
// Adding a multi-member scenario:
//
//	f := startMemberFleet(t)              // CP + tenant; the image check runs here
//	a, b := f.addMember("alice"), f.addMember("bob")
//	a.startWorkspace()                    // each member's container is its own
//	got := a.getJSON("/api/...")          // or a.postJSON / a.do for any status
//	a.shell(session, "git ...")           // run a command in a shell session, read its output
//
// A git remote both workspaces can reach comes from newGitRemote(t, dockerBridgeGateway(t)).
// Teardown (CP, every member's container and network, the data dir) is registered by
// startMemberFleet and addMember; a scenario only has to stop what it wants stopped as part
// of what it tests. Names are unique per test because the tenant slug is random and every
// container and network is scoped by it (af-ws-<slug>-<user key>).
package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// emailDomain is reserved (RFC 2606), so no address the harness mints can belong to
// anybody.
const emailDomain = "e2e.test"

// memberFleet is one CP running AUTH=proxy with a tenant created for this test.
type memberFleet struct {
	t       *testing.T
	base    string
	dataDir string
	slug    string
	admin   *member
	members []*member
}

// member is one identity's client: every request carries its email header and the
// fleet's tenant.
type member struct {
	f     *memberFleet
	t     *testing.T
	name  string
	email string
	key   string // the user key the CP derived from the email (also the home dir name)
	cmds  int    // shell() call counter, for unique output file names
}

// startMemberFleet starts the CP with AUTH=proxy and creates this test's tenant. Docker and
// the workspace image are required (a skip without them, a failure under E2E_REQUIRE=1).
func startMemberFleet(t *testing.T) *memberFleet {
	t.Helper()
	image := envOr("WS_IMAGE", "agent-fleet/workspace:dev")
	requireDockerAndImage(t, image)
	return bootMemberFleet(t, image)
}

// bootMemberFleet is startMemberFleet without the docker prerequisite: the CP and the
// tenant/membership setup run without docker, which is what lets the setup be tested on a
// host that has none (TestMemberFleetSetup). Starting a workspace still needs docker.
func bootMemberFleet(t *testing.T, image string) *memberFleet {
	t.Helper()
	f := &memberFleet{t: t, slug: "e2e" + randHex(t, 4)}
	f.dataDir = newDataDir(t, image)
	f.admin = &member{f: f, t: t, name: "admin", email: "admin-" + f.slug + "@" + emailDomain}
	// Each member's Agent port is WS_AGENT_PORT plus the order its workspace was created
	// in, so the whole range has to be free.
	f.base = launchCP(t, image, f.dataDir, f.workspaceNames,
		"AUTH=proxy",
		"AF_PROVISION=invite",
		"SUPER_ADMIN_EMAILS="+f.admin.email,
		fmt.Sprintf("WS_AGENT_PORT=%d", freePortRange(t, 4)),
	)
	created := f.admin.postJSON("/api/admin/tenants", map[string]any{"slug": f.slug, "name": "e2e " + f.slug}, 200)
	if created["slug"] != f.slug {
		t.Fatalf("tenant create returned slug %v, want %q", created["slug"], f.slug)
	}
	return f
}

// workspaceNames lists the af-ws-/af-net- suffixes of every member, for teardown.
func (f *memberFleet) workspaceNames() []string {
	out := make([]string, 0, len(f.members))
	for _, m := range f.members {
		out = append(out, f.slug+"-"+m.key)
	}
	return out
}

// addMember invites name@e2e.test into the fleet's tenant as a plain member and prepares
// the directories its workspace will mount. The workspace itself is not started.
func (f *memberFleet) addMember(name string) *member {
	f.t.Helper()
	m := &member{f: f, t: f.t, name: name, email: name + "@" + emailDomain}
	got := f.admin.postJSON("/api/admin/memberships", map[string]any{
		"email": m.email, "tenant_slug": f.slug, "role": "member",
	}, 200)
	m.key, _ = got["user_key"].(string)
	if m.key == "" {
		f.t.Fatalf("add membership %s: no user_key in %v", m.email, got)
	}
	// manager.workspaceNames: a non-default tenant's workspace lives at <WS_DATA>/<slug>/<key>.
	prepareWorkspaceDirs(f.t, filepath.Join(f.dataDir, f.slug, m.key))
	f.members = append(f.members, m)
	return m
}

// --- a member's requests ---------------------------------------------------

// do sends one request as this member and returns the status and the whole body. It never
// fails the test, so it is the one to use inside waitFor conditions.
func (m *member) do(method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, []byte(err.Error())
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, m.f.base+path, rd)
	if err != nil {
		return 0, []byte(err.Error())
	}
	req.Header.Set("X-Forwarded-Email", m.email)
	if m.f.slug != "" && m != m.f.admin {
		req.Header.Set("X-AF-Tenant", m.f.slug)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// call sends a request, fails unless the status is want, and decodes a JSON object body.
func (m *member) call(method, path string, body any, want int) map[string]any {
	m.t.Helper()
	code, b := m.do(method, path, body)
	if code != want {
		m.t.Fatalf("%s: %s %s: %d (want %d) %s", m.name, method, path, code, want, truncate(b))
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			m.t.Fatalf("%s: %s %s: bad JSON: %v: %s", m.name, method, path, err, truncate(b))
		}
	}
	return out
}

func (m *member) getJSON(path string) map[string]any {
	m.t.Helper()
	return m.call(http.MethodGet, path, nil, http.StatusOK)
}

func (m *member) postJSON(path string, body any, want int) map[string]any {
	m.t.Helper()
	return m.call(http.MethodPost, path, body, want)
}

// apiErr returns the error code of an {"error":{"code":…}} body, or "".
func apiErr(v map[string]any) (code, reason string) {
	e, _ := v["error"].(map[string]any)
	code, _ = e["code"].(string)
	reason, _ = e["reason"].(string)
	return code, reason
}

// --- a member's workspace --------------------------------------------------

// workspaceState is GET /api/workspace's state ("running", "stopped", …).
func (m *member) workspaceState() string {
	m.t.Helper()
	return fmt.Sprint(m.getJSON("/api/workspace")["state"])
}

// startWorkspace starts the member's workspace and waits until it is running. Start
// answers 500 whenever the Agent-healthz wait is missed once, but it is idempotent, so it
// is retried until it takes.
func (m *member) startWorkspace() {
	m.t.Helper()
	waitFor(m.t, 150*time.Second, m.name+" workspace/start accepted", func() (bool, string) {
		code, b := m.do(http.MethodPost, "/api/workspace/start", nil)
		return code == 200, fmt.Sprintf("%d %s", code, truncate(b))
	})
	waitFor(m.t, 60*time.Second, m.name+" workspace running", func() (bool, string) {
		code, b := m.do(http.MethodGet, "/api/workspace", nil)
		var ws map[string]any
		_ = json.Unmarshal(b, &ws)
		return code == 200 && ws["state"] == "running", fmt.Sprintf("%d %v", code, ws["state"])
	})
}

// stopWorkspace stops the member's workspace and checks it reads as stopped.
func (m *member) stopWorkspace() {
	m.t.Helper()
	got := m.postJSON("/api/workspace/stop", nil, 200)
	if got["state"] != "stopped" {
		m.t.Fatalf("%s: workspace/stop state = %v, want stopped", m.name, got["state"])
	}
}

// requireStopped fails unless the workspace is still not running — used after reads that
// must not wake it.
func (m *member) requireStopped(why string) {
	m.t.Helper()
	if s := m.workspaceState(); s == "running" {
		m.t.Fatalf("%s: workspace is running %s; it must have stayed stopped", m.name, why)
	}
}

// createSession creates a session (POST /api/sessions) and returns its name.
func (m *member) createSession(body map[string]any) string {
	m.t.Helper()
	got := m.postJSON("/api/sessions", body, 201)
	name, _ := got["name"].(string)
	if name == "" {
		m.t.Fatalf("%s: session create returned no name: %v", m.name, got)
	}
	m.t.Logf("%s: session %s created (kind=%v dir=%v)", m.name, name, got["kind"], got["dir"])
	return name
}

// shell runs cmd in a shell session (in the session's own cwd) and returns its combined
// output, failing on a non-zero exit. The output goes to a file in home and is read back
// through the fs API, the same route TestFleet uses; the rename makes the file appear only
// once the command has finished.
func (m *member) shell(session, cmd string) string {
	m.t.Helper()
	m.cmds++
	out := fmt.Sprintf("e2e-%s-cmd-%d.out", m.name, m.cmds)
	line := fmt.Sprintf(`{ %s ; } > ~/%s.tmp 2>&1; echo "e2e-exit=$?" >> ~/%s.tmp; mv ~/%s.tmp ~/%s`,
		cmd, out, out, out, out)
	waitFor(m.t, 20*time.Second, m.name+" session input accepted", func() (bool, string) {
		code, b := m.do(http.MethodPost, "/api/sessions/"+session+"/input", map[string]any{"prompt": line})
		return code == 200, fmt.Sprintf("%d %s", code, truncate(b))
	})
	var content string
	waitFor(m.t, 90*time.Second, m.name+" $ "+cmd, func() (bool, string) {
		code, b := m.do(http.MethodGet, "/api/fs/file?path="+url.QueryEscape(out), nil)
		if code != 200 {
			return false, fmt.Sprintf("%d %s", code, truncate(b))
		}
		var f struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(b, &f)
		content = f.Content
		return strings.Contains(content, "e2e-exit="), string(truncate(b))
	})
	body, status, _ := strings.Cut(content, "e2e-exit=")
	if strings.TrimSpace(status) != "0" {
		m.t.Fatalf("%s: `%s` exited %s:\n%s", m.name, cmd, strings.TrimSpace(status), body)
	}
	return strings.TrimSpace(body)
}

// notifications returns the member's notification items (GET /api/notifications).
func (m *member) notifications() []map[string]any {
	m.t.Helper()
	items, _ := m.getJSON("/api/notifications")["items"].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if n, ok := it.(map[string]any); ok {
			out = append(out, n)
		}
	}
	return out
}

// --- a git remote both workspaces can reach ---------------------------------

// gitRemote serves bare repositories over git's smart HTTP protocol (`git http-backend`
// as CGI) from the test process, without credentials, so every member's workspace can
// clone, fetch and push the same repository.
type gitRemote struct {
	root string // GIT_PROJECT_ROOT: one bare repository per <name>.git
	base string // http://host:port
}

// newGitRemote serves an empty remote on host (an address of this machine the workspace
// containers can reach — dockerBridgeGateway — or 127.0.0.1 for host-only use). It needs
// git on the host; the server stops with the test.
func newGitRemote(t *testing.T, host string) *gitRemote {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		prereqMissing(t, "git not on PATH")
	}
	r := &gitRemote{root: t.TempDir()}
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("git remote: listen on %s: %v", host, err)
	}
	r.base = "http://" + l.Addr().String()
	srv := &http.Server{Handler: gitHTTPBackend(gitBin, r.root), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return r
}

// gitHTTPBackend is `git http-backend` behind net/http/cgi, the same way the CP serves its
// internal git provider. The host's git config is kept out, so a fixture behaves the same
// on a laptop and on a runner.
func gitHTTPBackend(gitBin, root string) http.Handler {
	return &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
		},
		InheritEnv: []string{"PATH"},
	}
}

// URL is the clone URL of the repository name.
func (r *gitRemote) URL(name string) string { return r.base + "/" + name + ".git" }

// newRepo creates the bare repository name with one commit on main and returns its URL.
// receivepack is switched on because http-backend refuses an unauthenticated push
// otherwise.
func (r *gitRemote) newRepo(t *testing.T, name string) string {
	t.Helper()
	bare := filepath.Join(r.root, name+".git")
	work := filepath.Join(t.TempDir(), name)
	runGit(t, "", "init", "-q", "--bare", "-b", "main", bare)
	runGit(t, bare, "config", "http.receivepack", "true")
	runGit(t, "", "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("e2e remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-q", "-m", "initial")
	runGit(t, work, "push", "-q", bare, "main")
	return r.URL(name)
}

// runGit runs git with a fixed identity and no user or system config, so the host's own
// settings (signing, hooks, templates) cannot leak into the fixture.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=e2e", "-c", "user.email=e2e@" + emailDomain}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// dockerBridgeGateway is the host's address on docker's default bridge. Every member's
// container sits on its own network (af-net-…), but traffic to an address the host owns is
// delivered locally rather than forwarded between bridges, so all of them reach a server
// bound here, and nothing outside this machine does.
func dockerBridgeGateway(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "network", "inspect", "bridge",
		"-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}").Output()
	if err != nil {
		t.Fatalf("docker network inspect bridge: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
			return f
		}
	}
	t.Fatalf("docker bridge has no IPv4 gateway: %q", out)
	return ""
}

// --- helpers ---------------------------------------------------------------

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// freePortRange returns the first of n consecutive ports that are free right now.
func freePortRange(t *testing.T, n int) int {
	t.Helper()
	for try := 0; try < 50; try++ {
		base := freePort(t)
		ok := true
		for p := base + 1; p < base+n && ok; p++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				ok = false
				break
			}
			l.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatalf("no %d consecutive free ports found", n)
	return 0
}

// decode sends a request as m, fails unless the status is want, and unmarshals the body
// into out.
func decode(t *testing.T, m *member, method, path string, body any, want int, out any) {
	t.Helper()
	code, b := m.do(method, path, body)
	if code != want {
		t.Fatalf("%s: %s %s: %d (want %d) %s", m.name, method, path, code, want, truncate(b))
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %s %s: bad JSON: %v: %s", m.name, method, path, err, truncate(b))
	}
}
