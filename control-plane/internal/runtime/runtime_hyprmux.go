package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// hyprmux-host runs the read-only hyprmux shim (workspace/hyprmux-shim) as a host process
// in place of a workspace Agent. The shim serves the host's own session registry, so like
// native it is single-user only: it requires AUTH=dev. It takes its settings from its
// environment only (never argv), and no workspace env or secret keys reach it.

const (
	hyprmuxReadyWait = 5 * time.Second
	hyprmuxStopGrace = 5 * time.Second
	hyprmuxPidFile   = "shim.pid"
	hyprmuxLogFile   = "shim.log"
)

type hyprmuxFactory struct {
	shimBin     string
	registry    string
	logsDir     string
	rootDataDir func(Workspace) string
}

func newHyprmuxFactory(mcfg Config) (RuntimeFactory, error) {
	if mcfg.AuthMode != "dev" {
		return nil, fmt.Errorf("AF_RUNTIME=hyprmux-host is single-user only (it serves the host's own session registry); it requires AUTH=dev, got AUTH=%s", mcfg.AuthMode)
	}
	shim, err := hyprmuxAbsEnv("AF_HYPRMUX_SHIM")
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(shim); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("AF_HYPRMUX_SHIM: %q is not an executable file (build it with scripts/build-hyprmux-shim.sh)", shim)
	}
	registry, err := hyprmuxAbsEnv("AF_HYPRMUX_REGISTRY")
	if err != nil {
		return nil, err
	}
	logs, err := hyprmuxAbsEnv("AF_HYPRMUX_LOGS")
	if err != nil {
		return nil, err
	}
	return &hyprmuxFactory{
		shimBin:     shim,
		registry:    registry,
		logsDir:     logs,
		rootDataDir: mcfg.rootedDataDir,
	}, nil
}

func hyprmuxAbsEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("AF_RUNTIME=hyprmux-host: %s is required", key)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("%s: resolve %q: %w", key, v, err)
	}
	return abs, nil
}

func (f *hyprmuxFactory) New(ws Workspace, _ SecretKeys, _ []string) Runtime {
	dataDir := f.rootDataDir(ws)
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	return &hyprmuxRuntime{
		shimBin:  f.shimBin,
		registry: f.registry,
		logsDir:  f.logsDir,
		name:     ws.ContainerName,
		dataDir:  dataDir,
		port:     ws.AgentPort,
		token:    ws.AgentToken,
	}
}

type hyprmuxRuntime struct {
	shimBin  string
	registry string
	logsDir  string
	name     string
	dataDir  string
	port     string
	token    string
}

// The shim binds loopback only (HYPRMUX_SHIM_ADDR rejects anything else), so the endpoint
// is the loopback the CP shares with it.
func (h *hyprmuxRuntime) Endpoint() string { return "http://127.0.0.1:" + h.port }
func (h *hyprmuxRuntime) Token() string    { return h.token }
func (h *hyprmuxRuntime) Name() string     { return h.name }

func (h *hyprmuxRuntime) pidFile() string { return filepath.Join(h.dataDir, hyprmuxPidFile) }
func (h *hyprmuxRuntime) logFile() string { return filepath.Join(h.dataDir, hyprmuxLogFile) }

// State is "starting" for a live shim that has not answered /healthz yet. The shim has no
// boot phase, so that window is only the bind; there is no time-boxed marker for it.
func (h *hyprmuxRuntime) State(ctx context.Context) string {
	pid := readPidFile(h.pidFile())
	if pid <= 0 {
		return "none"
	}
	if !pidAlive(pid, h.shimBin) {
		return "stopped"
	}
	if agentHealthy(ctx, h.Endpoint()) {
		return "running"
	}
	return "starting"
}

// Start returns an error when the shim does not answer within hyprmuxReadyWait. Unlike a
// container agent it has no boot to wait out, so a silent failure here is a misconfiguration
// (a port in use, an unreadable registry) and must surface at once.
func (h *hyprmuxRuntime) Start(ctx context.Context) error {
	switch h.State(ctx) {
	case "running", "starting":
		return nil
	}
	_ = os.Remove(h.pidFile())
	if err := os.MkdirAll(h.dataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir data dir: %w", err)
	}
	logf, err := os.OpenFile(h.logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open shim log: %w", err)
	}
	defer logf.Close()
	cmd := exec.Command(h.shimBin)
	cmd.Env = []string{
		"HYPRMUX_SHIM_ADDR=127.0.0.1:" + h.port,
		"HYPRMUX_SHIM_TOKEN=" + h.token,
		"HYPRMUX_REGISTRY=" + h.registry,
		"HYPRMUX_AGENT_LOGS=" + h.logsDir,
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start hyprmux shim: %w", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	if err := os.WriteFile(h.pidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return fmt.Errorf("write pidfile: %w", err)
	}
	if err := WaitAgentHealthy(ctx, h.Endpoint(), hyprmuxReadyWait); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		removePidFileIfPID(h.pidFile(), pid)
		return fmt.Errorf("hyprmux shim not answering on %s (see %s): %w", h.Endpoint(), h.logFile(), err)
	}
	return nil
}

// Stop sends SIGTERM (the shim shuts down its listener on it) and escalates to SIGKILL
// after hyprmuxStopGrace.
func (h *hyprmuxRuntime) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pid := readPidFile(h.pidFile())
	if pid > 0 && pidAlive(pid, h.shimBin) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		deadline := time.Now().Add(hyprmuxStopGrace)
		for pidAlive(pid, h.shimBin) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if pidAlive(pid, h.shimBin) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			for pidAlive(pid, h.shimBin) {
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	removePidFileIfPID(h.pidFile(), pid)
	return nil
}

// Destroy stops the shim and removes its pid file and log. The registry and the agent logs
// are the host's, not the workspace's, and are never touched.
func (h *hyprmuxRuntime) Destroy(ctx context.Context) ([]string, error) {
	if err := h.Stop(ctx); err != nil {
		return nil, err
	}
	for _, p := range []string{h.pidFile(), h.logFile()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return nil, nil
}
