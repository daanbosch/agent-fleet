package fastvm

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const proxyPIDDir = "/var/lib/fastvm/proxy"

// portAllocator tracks which ports are claimed by fastvm proxies in-memory.
// Prevents double-allocation during concurrent VM creation, and catches
// external port conflicts (KVM/Ceph) via net.Listen probe.
var portAllocator = &portAlloc{
	byPort: make(map[int]int), // port → vmid
	byVMID: make(map[int]int), // vmid → port
}

type portAlloc struct {
	mu     sync.Mutex
	byPort map[int]int // port → vmid
	byVMID map[int]int // vmid → port
}

// Reserve claims a port for a VMID. Returns the actual port to use.
// Tries the VMID as port first, then VMID+1000..VMID+1099.
// Checks both in-memory reservations AND OS-level availability (net.Listen).
func (pa *portAlloc) Reserve(vmid int) (int, error) {
	return pa.ReservePreferred(vmid, 0)
}

// ReservePreferred claims a port for a VMID, trying preferred first when set.
func (pa *portAlloc) ReservePreferred(vmid int, preferred int) (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// If this VMID already has a port, release it first
	if oldPort, ok := pa.byVMID[vmid]; ok {
		delete(pa.byPort, oldPort)
		delete(pa.byVMID, vmid)
	}

	// Build candidate list: preferred first, then VMID, then VMID+1000..+1099
	candidates := make([]int, 0, 101)
	if preferred > 0 && preferred <= 65535 {
		candidates = append(candidates, preferred)
	}
	candidates = append(candidates, vmid)
	for offset := 1000; offset < 1100; offset++ {
		c := vmid + offset
		if c <= 65535 {
			candidates = append(candidates, c)
		}
	}

	seen := make(map[int]bool, len(candidates))
	for _, port := range candidates {
		if port <= 0 || port > 65535 {
			continue
		}
		if seen[port] {
			continue
		}
		seen[port] = true
		// Skip if another fastvm proxy already claimed this port
		if _, taken := pa.byPort[port]; taken {
			continue
		}
		// Probe OS-level availability (catches KVM/Ceph ephemeral ports)
		if !isPortAvailable(port) {
			continue
		}
		// Claim it
		pa.byPort[port] = vmid
		pa.byVMID[vmid] = port
		return port, nil
	}

	return 0, fmt.Errorf("no available port for VMID %d (tried %d candidates)", vmid, len(candidates))
}

// Release frees a port reservation for a VMID.
func (pa *portAlloc) Release(vmid int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	if port, ok := pa.byVMID[vmid]; ok {
		delete(pa.byPort, port)
		delete(pa.byVMID, vmid)
	}
}

// LoadExisting populates the in-memory map from on-disk .port files.
// Call on startup after reading active VMIDs.
func (pa *portAlloc) LoadExisting() {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	entries, err := os.ReadDir(proxyPIDDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".port") {
			continue
		}
		vmidStr := strings.TrimSuffix(name, ".port")
		vmid, err := strconv.Atoi(vmidStr)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(proxyPIDDir, name))
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			continue
		}
		pa.byPort[port] = vmid
		pa.byVMID[vmid] = port
		slog.Debug("loaded port mapping", "vmid", vmid, "port", port)
	}

	slog.Info("loaded existing port mappings", "count", len(pa.byVMID))
}

// proxyPIDFile returns the path for the socat PID file for a given VMID.
func proxyPIDFile(vmid int) string {
	return filepath.Join(proxyPIDDir, fmt.Sprintf("%d.pid", vmid))
}

// portMapFile returns the path that stores the actual port used for a VMID's SSH proxy.
func portMapFile(vmid int) string {
	return filepath.Join(proxyPIDDir, fmt.Sprintf("%d.port", vmid))
}

// ProxyPort returns the actual SSH proxy port for a VMID.
// Checks in-memory map first (fast), falls back to disk, then VMID.
func ProxyPort(vmid int) int {
	portAllocator.mu.Lock()
	if port, ok := portAllocator.byVMID[vmid]; ok {
		portAllocator.mu.Unlock()
		return port
	}
	portAllocator.mu.Unlock()

	// Fallback: check disk
	data, err := os.ReadFile(portMapFile(vmid))
	if err != nil {
		return vmid
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return vmid
	}
	return port
}

// isPortAvailable checks whether a TCP port can be bound on 0.0.0.0.
func isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// startSSHProxy starts a socat listener that forwards a port → vmIP:22.
// Uses the in-memory port allocator to find a free port (prefers VMID,
// falls back to VMID+1000 range). Checks both internal reservations and
// OS-level availability to avoid conflicts with KVM/Ceph ephemeral ports.
func startSSHProxy(vmid int, vmIP string) error {
	// Check socat is available
	if _, err := exec.LookPath("socat"); err != nil {
		slog.Warn("socat not installed, skipping SSH proxy", "vmid", vmid)
		return nil
	}

	// Ensure PID directory exists
	if err := os.MkdirAll(proxyPIDDir, 0755); err != nil {
		return fmt.Errorf("create proxy PID dir: %w", err)
	}

	// Kill any existing proxy on this port (orphan cleanup)
	preferredPort := ProxyPort(vmid)
	stopSSHProxy(vmid)

	// Reserve a port via the allocator (in-memory + OS probe)
	port, err := portAllocator.ReservePreferred(vmid, preferredPort)
	if err != nil {
		return fmt.Errorf("reserve port for VMID %d: %w", vmid, err)
	}

	log := slog.With("vmid", vmid, "vmIP", vmIP, "port", port)
	if port != vmid {
		log.Warn("preferred port occupied, using alternative", "preferred", vmid, "actual", port)
	}
	log.Info("starting SSH proxy")

	// Start socat as a background process
	cmd := exec.Command("socat",
		fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr,bind=0.0.0.0", port),
		fmt.Sprintf("TCP:%s:22", vmIP),
	)

	// Detach from parent process group so it survives API restarts
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if err := cmd.Start(); err != nil {
		portAllocator.Release(vmid)
		return fmt.Errorf("start socat for VMID %d on port %d: %w", vmid, port, err)
	}

	// Wait for socat to actually bind (it forks before listening); a fixed
	// sleep here was once worth 10-25% of a warm launch.
	probeDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(probeDeadline) {
		conn, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if derr == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Write PID file
	pid := cmd.Process.Pid
	if err := os.WriteFile(proxyPIDFile(vmid), []byte(strconv.Itoa(pid)), 0644); err != nil {
		cmd.Process.Kill()
		portAllocator.Release(vmid)
		return fmt.Errorf("write PID file for VMID %d: %w", vmid, err)
	}

	// Write port mapping to disk (survives API restarts)
	if err := os.WriteFile(portMapFile(vmid), []byte(strconv.Itoa(port)), 0644); err != nil {
		slog.Warn("failed to write port map file", "vmid", vmid, "err", err)
	}

	// Release the process so it's not waited on by us
	cmd.Process.Release()

	log.Info("SSH proxy started", "pid", pid, "port", port)
	return nil
}

func sshProxyRunning(vmid int) bool {
	pidData, err := os.ReadFile(proxyPIDFile(vmid))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}

	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(string(cmdline), "socat")
}

// stopSSHProxy kills the socat process for a given VMID and removes the PID file.
func stopSSHProxy(vmid int) error {
	pidPath := proxyPIDFile(vmid)

	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No proxy running
		}
		return fmt.Errorf("read proxy PID file for VMID %d: %w", vmid, err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		os.Remove(pidPath)
		return fmt.Errorf("invalid PID in %s: %w", pidPath, err)
	}

	log := slog.With("vmid", vmid, "pid", pid)

	// Check if process is still alive
	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(pidPath)
		return nil
	}

	// Signal 0 to check if process exists
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process already dead
		log.Debug("proxy process already dead")
		os.Remove(pidPath)
		return nil
	}

	// Kill the process
	if err := proc.Kill(); err != nil {
		log.Warn("failed to kill proxy process", "err", err)
	} else {
		log.Info("SSH proxy stopped")
	}

	os.Remove(pidPath)
	os.Remove(portMapFile(vmid))
	portAllocator.Release(vmid)
	return nil
}

// cleanupOrphanProxies kills any socat proxy processes from previous runs
// that no longer correspond to active VMs. Call on startup.
func cleanupOrphanProxies(activeVMIDs map[string]bool) {
	entries, err := os.ReadDir(proxyPIDDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		slog.Warn("failed to read proxy PID dir", "err", err)
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".pid") {
			continue
		}

		vmidStr := strings.TrimSuffix(name, ".pid")
		if activeVMIDs[vmidStr] {
			continue // VM is still active, leave proxy running
		}

		vmid, err := strconv.Atoi(vmidStr)
		if err != nil {
			slog.Warn("invalid proxy PID filename", "name", name)
			os.Remove(filepath.Join(proxyPIDDir, name))
			continue
		}

		slog.Info("cleaning up orphan SSH proxy", "vmid", vmid)
		stopSSHProxy(vmid)
	}
}
