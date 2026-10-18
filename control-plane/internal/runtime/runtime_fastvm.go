// runtime_fastvm.go — fastvm (Firecracker warm-clone microVM) Runtime adapter.
//
// Workspace = one fastvm warm clone on a fastvm node (default template
// "warm-af-ws-2": an Ubuntu guest with git/tmux and workspace-agent baked,
// user dev uid 1000). VMs launch in ~0.5s from memory-preserving snapshots.
//
// Mapping onto the Runtime port:
//   Start     warm-launch (or resume) + env file + agent via systemd-run + expose 7700
//   Stop      VM pause (guest CPUs stop; disk+RAM stay resident = reversible persistence)
//   State     /healthz probe: running | starting | stopped(paused) | none(no VM)
//   Endpoint  http://<node>:<exposed hostport>   (socat forwards on the fastvm node)
//   Token     ws.AgentToken (delivered into the guest via a 0600 env file)
//   Name      ws.ContainerName (also the fastvm VM name)
//
// Persistence: unlike docker (container + home bind mount), a fastvm VM's
// whole disk and RAM live in the VM, so Stop=Pause keeps everything and
// Start resumes in ~0.3s with the agent still running. Destroy deletes the
// VM irreversibly, like every other adapter. Home ports (WipeHome etc.) are
// not implemented in v1: home lives inside the VM, and wiping it means the
// same work the docker adapter does against its bind mount.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/team-telnyx/fast-vm-spinner/sdk/fastvm"
)

const (
	fastvmAgentGuestPort = 7700 // workspace-agent's in-guest HTTP port
	fastvmAgentUnit      = "afws-agent"
	fastvmStateFile      = "fastvm.json"
)

// fastvmFactory builds one fastvmRuntime per workspace. All state lives on
// the fastvm node (VM) plus one sidecar JSON under the workspace data dir so
// a CP restart re-adopts {vmid, exposed host port} without re-launching.
type fastvmFactory struct {
	apiURL        string // fastvm REST API, e.g. http://10.242.128.198:8090
	template      string // warm template to launch clones from
	user          string // fastvm user (network block owner)
	nodeHost      string // host the CP reaches socat-exposed ports on
	extraEnv      []string
	rootDataDir   func(Workspace) string
	healthTimeout time.Duration
}

func newFastVMFactory(mcfg Config) (*fastvmFactory, error) {
	apiURL := strings.TrimSpace(os.Getenv("AF_FASTVM_API"))
	if apiURL == "" {
		return nil, fmt.Errorf("AF_FASTVM_API is required for AF_RUNTIME=fastvm (e.g. http://10.242.128.198:8090)")
	}
	tpl := os.Getenv("AF_FASTVM_TEMPLATE")
	if tpl == "" {
		tpl = "warm-af-ws-2"
	}
	user := os.Getenv("AF_FASTVM_USER")
	if user == "" {
		user = "dav"
	}
	nodeHost := os.Getenv("AF_FASTVM_HOST")
	if nodeHost == "" {
		u, err := url.Parse(apiURL)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("AF_FASTVM_API %q: cannot derive host; set AF_FASTVM_HOST", apiURL)
		}
		nodeHost = u.Hostname()
	}
	return &fastvmFactory{
		apiURL:        apiURL,
		template:      tpl,
		user:          user,
		nodeHost:      nodeHost,
		extraEnv:      mcfg.ExtraEnv,
		rootDataDir:   mcfg.RootDataDir,
		healthTimeout: 4 * time.Second,
	}, nil
}

type fastvmRuntime struct {
	ws   Workspace
	keys SecretKeys
	f    *fastvmFactory
	env  []string // per-workspace extra env (KEY=VAL)
}

func (f *fastvmFactory) New(ws Workspace, keys SecretKeys, extraEnv []string) Runtime {
	return &fastvmRuntime{ws: ws, keys: keys, f: f, env: extraEnv}
}

var _ RuntimeFactory = (*fastvmFactory)(nil)

// sidecarPath is the per-workspace file recording {vmid, hostport}.
func (r *fastvmRuntime) sidecarPath() string {
	root := r.ws.DataDir
	if r.f.rootDataDir != nil {
		root = r.f.rootDataDir(r.ws)
	}
	return filepath.Join(root, fastvmStateFile)
}

type fastvmSidecar struct {
	VMID     string `json:"vmid"`
	HostPort int    `json:"host_port"`
}

func (r *fastvmRuntime) loadSidecar() (fastvmSidecar, error) {
	var sc fastvmSidecar
	b, err := os.ReadFile(r.sidecarPath())
	if err != nil {
		return sc, err
	}
	err = json.Unmarshal(b, &sc)
	return sc, err
}

func (r *fastvmRuntime) saveSidecar(sc fastvmSidecar) error {
	if err := os.MkdirAll(filepath.Dir(r.sidecarPath()), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(sc)
	return os.WriteFile(r.sidecarPath(), b, 0o644)
}

// client returns a fastvm SDK client for the node.
func (r *fastvmRuntime) client() *fastvm.Client {
	return fastvm.New(r.f.apiURL, r.f.user)
}

// Start brings the workspace up: launch-or-resume the VM, deliver env, boot
// the agent if needed, and expose its guest port. It returns once the launch
// is COMMITTED (agent health stays a State()/ensureWorkspaceReady concern —
// see the port contract), never blocking on a slow node.
func (r *fastvmRuntime) Start(ctx context.Context) error {
	c := r.client()
	name := r.ws.ContainerName
	vm, err := c.Get(name)
	switch {
	case err == nil && vm.Paused:
		if err := c.Resume(vm.Name); err != nil {
			return fmt.Errorf("fastvm resume %s: %w", name, err)
		}
	case err == nil:
		// running clone: reuse
	default:
		// no VM yet: warm-launch one from the template
		launched, lerr := c.WarmLaunch(fastvm.LaunchOptions{Template: r.f.template, Name: name})
		if lerr != nil {
			return fmt.Errorf("fastvm warm-launch %s: %w", name, lerr)
		}
		vm = &launched
		if err := r.saveSidecar(fastvmSidecar{VMID: launched.VMID}); err != nil {
			return fmt.Errorf("fastvm sidecar write: %w", err)
		}
	}

	// Deliver the environment and (re)start the agent inside the guest.
	if err := r.bootAgent(c, vm.Name); err != nil {
		return err
	}

	// Expose the agent's guest port on the fastvm node (idempotent: adopt an
	// existing socat mapping from the sidecar or the VM record).
	sc, serr := r.loadSidecar()
	needExpose := serr != nil || sc.HostPort == 0
	if !needExpose && sc.VMID != "" && sc.VMID != vm.VMID {
		needExpose = true // re-adopting a different VMID: old forward is dead
	}
	if needExpose {
		ep, exerr := c.ExposePort(vm.Name, fastvmAgentGuestPort)
		if exerr != nil {
			return fmt.Errorf("fastvm expose %d on %s: %w", fastvmAgentGuestPort, name, exerr)
		}
		if err := r.saveSidecar(fastvmSidecar{VMID: vm.VMID, HostPort: ep.HostPort}); err != nil {
			return fmt.Errorf("fastvm sidecar write: %w", err)
		}
	}

	// Courtesy grace only — a live /healthz just makes the common start feel
	// instant; boot phases and slow node disks must not turn into errors.
	_ = r.agentHealth()
	return nil
}

// bootAgent writes the 0600 env file inside the guest and starts
// workspace-agent under systemd as user dev (uid 1000), once.
func (r *fastvmRuntime) bootAgent(c *fastvm.Client, vmName string) error {
	env := r.agentEnv()
	var sb strings.Builder
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(shellSingleQuote(v))
		sb.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(r.sidecarPath()), 0o755); err != nil {
		return err
	}
	// Exec via the engine SSH channel (shell on the guest as the exec user).
	// Exec via the engine SSH channel (shell on the guest as the exec user).
	// The env file is written directly; a tiny launcher script sources it and
	// execs the agent (systemd-run --env-file is not portable across guest
	// systemd versions, but a plain launcher under a user unit is).
	execCmd := "install -d -m 0755 -o 1000 -g 1000 /var/lib/af /home/dev && " +
		"printf '%s' " + shellSingleQuote(sb.String()) + " > /var/lib/af/af.env && " +
		"chmod 600 /var/lib/af/af.env && chown 1000:1000 /var/lib/af/af.env && " +
		"printf '%s\n' '#!/bin/bash' 'set -a' 'source /var/lib/af/af.env' 'exec /usr/local/bin/workspace-agent' " +
		"> /usr/local/bin/af-agent-launch.sh && chmod 755 /usr/local/bin/af-agent-launch.sh && " +
		"if ! systemctl is-active --quiet " + fastvmAgentUnit + "; then " +
		"systemd-run --unit=" + fastvmAgentUnit + " --uid=1000 --gid=1000 " +
		"--working-directory=/home/dev /usr/local/bin/af-agent-launch.sh; fi && echo BOOT-OK"
	res, err := c.Exec(vmName, execCmd, 60)
	if err != nil {
		return fmt.Errorf("fastvm agent boot exec: %w", err)
	}
	if strings.TrimSpace(res.Output) == "" || res.ExitCode != 0 {
		return fmt.Errorf("fastvm agent boot failed rc=%d: %s", res.ExitCode, strings.TrimSpace(res.Output))
	}
	return nil
}

// agentEnv assembles the workspace-agent environment: image-style base env +
// shared template env + per-workspace extraEnv + secret keys + agent token.
// Args of the form KEY=VAL only; same discipline as native's rootfsEnv.
func (r *fastvmRuntime) agentEnv() []string {
	base := []string{
		"HOME=/home/dev",
		"USER=dev",
		"LOGNAME=dev",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/home/dev/.local/bin",
		"LANG=C.UTF-8",
		"TERM=xterm-256color",
		"CLAUDE_CONFIG_DIR=/var/lib/af/claude",
		"AGENT_ADDR=0.0.0.0:" + strconv.Itoa(fastvmAgentGuestPort),
		"AGENT_SHELL=/bin/bash",
	}
	env := append([]string{}, base...)
	env = append(env, r.f.extraEnv...)
	env = append(env, r.env...)
	for _, kv := range r.keys.envPairs() {
		env = append(env, kv[0]+"="+kv[1])
	}
	if r.ws.AgentToken != "" {
		env = append(env, "AGENT_TOKEN="+r.ws.AgentToken)
	}
	// De-dup, last wins (mirrors overlayWorkspaceEnv semantics).
	out := make([]string, 0, len(env))
	seen := map[string]int{}
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if j, ok := seen[k]; ok {
			out[j] = kv
			continue
		}
		seen[k] = len(out)
		out = append(out, kv)
	}
	return out
}

// Stop pauses the VM: guest CPUs stop, disk and RAM stay resident — the
// reversible persistence story for this adapter.
func (r *fastvmRuntime) Stop(ctx context.Context) error {
	c := r.client()
	vm, err := c.Get(r.ws.ContainerName)
	if err != nil {
		// nothing to stop is success for Stop (docker removes remnant too)
		return nil
	}
	if vm.Paused {
		return nil
	}
	return c.Pause(vm.Name)
}

// State reports the workspace's live state.
func (r *fastvmRuntime) State(ctx context.Context) string {
	c := r.client()
	vm, err := c.Get(r.ws.ContainerName)
	if err != nil {
		return "none"
	}
	if vm.Paused {
		return "stopped"
	}
	if r.agentUp() {
		return "running"
	}
	return "starting"
}

// agentUp is a single quick /healthz check.
func (r *fastvmRuntime) agentUp() bool {
	return len(r.agentHealth()) > 0
}

// agentHealth fetches /healthz through the exposed host port; returns the body.
func (r *fastvmRuntime) agentHealth() string {
	sc, err := r.loadSidecar()
	if err != nil || sc.HostPort == 0 {
		return ""
	}
	client := &http.Client{Timeout: r.f.healthTimeout}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/healthz", r.f.nodeHost, sc.HostPort))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b := make([]byte, 512)
	n, _ := resp.Body.Read(b)
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return string(b[:n])
}

// Endpoint is the CP→Agent HTTP base URL (fastvm node, socat-exposed port).
func (r *fastvmRuntime) Endpoint() string {
	sc, err := r.loadSidecar()
	port := sc.HostPort
	if err != nil || port == 0 {
		port = 0 // zero before first Start; callers gate on State
	}
	return fmt.Sprintf("http://%s:%d", r.f.nodeHost, port)
}

// Token is the Bearer secret for CP→Agent calls.
func (r *fastvmRuntime) Token() string { return r.ws.AgentToken }

// Name is the workspace's live-resource name (the fastvm VM name).
func (r *fastvmRuntime) Name() string { return r.ws.ContainerName }

// Destroy tears the workspace down irreversibly: VM first, then the sidecar.
func (r *fastvmRuntime) Destroy(ctx context.Context) ([]string, error) {
	c := r.client()
	vm, err := c.Get(r.ws.ContainerName)
	if err == nil {
		if derr := c.Destroy(vm.Name); derr != nil {
			return []string{"fastvm VM " + vm.Name + " may still exist: " + derr.Error()}, derr
		}
	}
	if err := os.Remove(r.sidecarPath()); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return nil, nil
}

var _ runtimeDestroyer = (*fastvmRuntime)(nil)

// shellSingleQuote quotes s for the guest's shell.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
