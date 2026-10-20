package fastvm

// ops.go — Firecracker-backend guest operations for sandbox parity:
// pause/resume (VMM-level, memory resident), exec over the engine's SSH
// channel, and TCP port exposure (socat forward host -> guest).

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	exposePortMin = 61000
	exposePortMax = 64999
	exposePIDDir  = "/var/lib/fastvm/proxy"
)

// fcSocketPath returns the Firecracker API socket path of an instance.
func (e *FirecrackerEngine) fcSocketPath(vmid string, warm bool) string {
	return filepath.Join(e.installDir, "instances", vmid, "firecracker.socket")
}

// PauseVM freezes the guest at the VMM level: vCPUs stop, memory stays
// resident and untouched. Parity with the sandbox stack's pause lifecycle
// (their CLH pause/resume).
func (e *FirecrackerEngine) PauseVM(vmid string) error {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	if vm.Paused {
		return fmt.Errorf("VM %s is already paused", vmid)
	}
	client := newFCClient(e.fcSocketPath(vmid, vm.WarmTemplate != ""))
	if _, _, err := client.do("PATCH", "/vm", map[string]string{"state": "Paused"}); err != nil {
		return fmt.Errorf("pause VM %s: %w", vmid, err)
	}
	if err := e.state.SetVMPaused(vmid, true); err != nil {
		slog.Warn("failed to record paused state", "vmid", vmid, "err", err)
	}
	slog.Info("VM paused", "backend", firecrackerBackendName, "vmid", vmid, "warm", vm.WarmTemplate != "")
	return nil
}

// ResumeVM resumes a paused guest.
func (e *FirecrackerEngine) ResumeVM(vmid string) error {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	if !vm.Paused {
		return fmt.Errorf("VM %s is not paused", vmid)
	}
	client := newFCClient(e.fcSocketPath(vmid, vm.WarmTemplate != ""))
	if _, _, err := client.do("PATCH", "/vm", map[string]string{"state": "Resumed"}); err != nil {
		return fmt.Errorf("resume VM %s: %w", vmid, err)
	}
	if err := e.state.SetVMPaused(vmid, false); err != nil {
		slog.Warn("failed to record resumed state", "vmid", vmid, "err", err)
	}
	slog.Info("VM resumed", "backend", firecrackerBackendName, "vmid", vmid)
	return nil
}

// ExecVM runs a non-interactive command inside the guest over the engine's
// SSH channel — the fastvm equivalent of the sandbox platform's vsock exec.
// The command runs as the engine SSH user (root in default images).
func (e *FirecrackerEngine) ExecVM(vmid, command string, timeoutSec int) (*ExecResult, error) {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return nil, fmt.Errorf("VM %s not found", vmid)
	}
	if vm.Paused {
		return nil, fmt.Errorf("VM %s is paused; resume it first", vmid)
	}
	if command == "" {
		return nil, fmt.Errorf("empty command")
	}
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	timeout := time.Duration(timeoutSec) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	privateKey := filepath.Join(e.installDir, "images", "id_rsa")
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		"-i", privateKey,
		fmt.Sprintf("%s@%s", e.sshUser, vm.IP),
		// run via sh so pipes/redirection inside the guest behave
		"sh -c "+shQuote(command),
	)
	// Same restored-guest first-connect caveat as the hygiene kick: retry
	// once after a short pause; to the caller it just looks like a long exec.
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "Connection timed out") {
		time.Sleep(2 * time.Second)
		cmd = exec.CommandContext(ctx, "ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			"-o", "BatchMode=yes",
			"-i", privateKey,
			fmt.Sprintf("%s@%s", e.sshUser, vm.IP),
			"sh -c "+shQuote(command),
		)
		out, err = cmd.CombinedOutput()
	}
	res := &ExecResult{Output: string(out), ExitCode: 0}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
			res.Output += "\n" + err.Error()
		}
	}
	return res, nil
}

// shQuote single-quotes a string for sh -c consumption.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExposePort maps a guest TCP service to a free host port via a detached
// socat forward, and records the mapping in VMState so destroy reaps it and
// restarts can reconstruct. Parity with the sandbox platform's "expose".
func (e *FirecrackerEngine) ExposePort(vmid string, guestPort int) (*ExposedPortInfo, error) {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return nil, fmt.Errorf("VM %s not found", vmid)
	}
	if guestPort <= 0 || guestPort > 65535 {
		return nil, fmt.Errorf("invalid guest port %d", guestPort)
	}
	if _, err := exec.LookPath("socat"); err != nil {
		return nil, fmt.Errorf("socat not installed on backend host")
	}
	// idempotent re-expose of the same guest port
	if vm.ExposedPorts != nil {
		if hp, exists := vm.ExposedPorts[guestPort]; exists {
			return &ExposedPortInfo{VMID: vmid, GuestPort: guestPort, HostPort: hp}, nil
		}
	}
	hostPort := e.pickHostPort(vmid, *vm, guestPort)
	if hostPort == 0 {
		return nil, fmt.Errorf("no free host port in %d-%d", exposePortMin, exposePortMax)
	}
	if err := os.MkdirAll(exposePIDDir, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command("socat",
		fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr,bind=0.0.0.0", hostPort),
		fmt.Sprintf("TCP:%s:%d", vm.IP, guestPort),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start socat for VM %s guest port %d: %w", vmid, guestPort, err)
	}
	// prove it bound before answering
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 50*time.Millisecond); derr == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pidFile := filepath.Join(exposePIDDir, fmt.Sprintf("x%s-%d.pid", vmid, guestPort))
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	cmd.Process.Release()

	if err := e.state.SetVMExposedPort(vmid, guestPort, hostPort); err != nil {
		return nil, err
	}
	slog.Info("exposed guest port", "backend", firecrackerBackendName,
		"vmid", vmid, "guest_port", guestPort, "host_port", hostPort)
	return &ExposedPortInfo{VMID: vmid, GuestPort: guestPort, HostPort: hostPort}, nil
}

// pickHostPort finds a free host port deterministically: the VMID-derived
// preferred slot first, then a linear scan. Deterministic preference makes
// the mapping reconstructable across restarts.
func (e *FirecrackerEngine) pickHostPort(vmid string, vm VMState, guestPort int) int {
	taken := make(map[int]bool)
	for _, hp := range vm.ExposedPorts {
		taken[hp] = true
	}
	preferred := 0
	vmidInt, _ := strconv.Atoi(vmid)
	if vmidInt >= 60000 {
		preferred = exposePortMin + (vmidInt-60000)*50 + (guestPort % 50)
		if preferred >= exposePortMin && preferred < exposePortMax && !taken[preferred] && isPortAvailable(preferred) {
			return preferred
		}
	}
	for p := exposePortMin; p <= exposePortMax; p++ {
		if !taken[p] && isPortAvailable(p) {
			return p
		}
	}
	return 0
}

// UnexposePort kills the socat forward listening on hostPort and removes
// the state mapping.
func (e *FirecrackerEngine) UnexposePort(vmid string, hostPort int) error {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	var guestPort int
	found := false
	for gp, hp := range vm.ExposedPorts {
		if hp == hostPort {
			guestPort, found = gp, true
			break
		}
	}
	if !found {
		return fmt.Errorf("VM %s has no exposure on host port %d", vmid, hostPort)
	}
	pidFile := filepath.Join(exposePIDDir, fmt.Sprintf("x%s-%d.pid", vmid, guestPort))
	if raw, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			stopProcess(pid)
		}
	}
	_ = os.Remove(pidFile)
	if err := e.state.SetVMExposedPort(vmid, guestPort, 0); err != nil {
		return err
	}
	slog.Info("unexposed guest port", "backend", firecrackerBackendName,
		"vmid", vmid, "host_port", hostPort)
	return nil
}

// reapExposedPorts stops every socat exposure of a VM (destroy path).
func (e *FirecrackerEngine) reapExposedPorts(vmid string, vm VMState) {
	if len(vm.ExposedPorts) == 0 {
		return
	}
	guestPorts := make([]int, 0, len(vm.ExposedPorts))
	for gp := range vm.ExposedPorts {
		guestPorts = append(guestPorts, gp)
	}
	sort.Ints(guestPorts)
	for _, gp := range guestPorts {
		pidFile := filepath.Join(exposePIDDir, fmt.Sprintf("x%s-%d.pid", vmid, gp))
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				stopProcess(pid)
			}
		}
		_ = os.Remove(pidFile)
	}
	_ = e.state.ClearVMExposedPorts(vmid)
}

// stopProcess sends SIGTERM and waits briefly for exit.
func stopProcess(pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			return // exited
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Kill()
}
