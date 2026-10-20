package fastvm

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runCmd executes a command and returns combined output.
func runCmd(name string, args ...string) (string, error) {
	slog.Debug("exec", "cmd", name, "args", args)
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, output)
	}
	return output, nil
}

// qmClone creates a linked clone from a template.
func qmClone(template, vmid int, name string) error {
	_, err := runCmd("qm", "clone",
		strconv.Itoa(template),
		strconv.Itoa(vmid),
		"--name", name,
		"--full", "0",
	)
	return err
}

// qmSet configures a VM with arbitrary key=value options.
func qmSet(vmid int, opts map[string]string) error {
	args := []string{"set", strconv.Itoa(vmid)}
	for k, v := range opts {
		args = append(args, "--"+k, v)
	}
	_, err := runCmd("qm", args...)
	return err
}

// qmGetConfigValue reads a single configuration key from a VM.
// Parses "qm config" output which uses "key: value" format, one per line.
func qmGetConfigValue(vmid int, key string) (string, error) {
	output, err := runCmd("qm", "config", strconv.Itoa(vmid))
	if err != nil {
		return "", err
	}
	prefix := key + ": "
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return line[len(prefix):], nil
		}
	}
	return "", fmt.Errorf("key %q not found in VM %d config", key, vmid)
}

func qmGetConfigInt(vmid int, key string) (int, error) {
	value, err := qmGetConfigValue(vmid, key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s for VM %d: %w", key, vmid, err)
	}
	return n, nil
}

// qmDelete removes configuration keys from a VM.
// Equivalent to: qm set <vmid> --delete key1,key2,...
func qmDelete(vmid int, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := runCmd("qm", "set", strconv.Itoa(vmid), "--delete", strings.Join(keys, ","))
	return err
}

// qmCloudInitUpdate regenerates the cloud-init drive for a VM.
func qmCloudInitUpdate(vmid int) error {
	_, err := runCmd("qm", "cloudinit", "update", strconv.Itoa(vmid))
	return err
}

// qmStart starts a VM.
func qmStart(vmid int) error {
	_, err := runCmd("qm", "start", strconv.Itoa(vmid))
	return err
}

func qmStatusPID(vmid int) (int, error) {
	output, err := runCmd("qm", "status", strconv.Itoa(vmid), "--verbose")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pid:") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid:")))
		if err != nil {
			return 0, fmt.Errorf("parse qm status pid for VM %d: %w", vmid, err)
		}
		return pid, nil
	}
	return 0, fmt.Errorf("qm status for VM %d did not include a pid", vmid)
}

const qemuSliceCPUSetPath = "/sys/fs/cgroup/qemu.slice/cpuset.cpus.effective"

type vcpuThread struct {
	index int
	tid   int
}

func parseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty CPU list")
	}

	var cpus []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if strings.Contains(part, "-") {
			bounds := strings.Split(part, "-")
			if len(bounds) != 2 {
				return nil, fmt.Errorf("invalid CPU range %q", part)
			}
			start, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid CPU range start %q: %w", part, err)
			}
			end, err := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid CPU range end %q: %w", part, err)
			}
			if end < start {
				return nil, fmt.Errorf("invalid descending CPU range %q", part)
			}
			for cpu := start; cpu <= end; cpu++ {
				cpus = append(cpus, cpu)
			}
			continue
		}

		cpu, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CPU %q: %w", part, err)
		}
		cpus = append(cpus, cpu)
	}
	if len(cpus) == 0 {
		return nil, fmt.Errorf("empty CPU list")
	}
	return cpus, nil
}

func formatCPUList(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}

	parts := make([]string, 0, len(cpus))
	start := cpus[0]
	prev := cpus[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}

	for _, cpu := range cpus[1:] {
		if cpu == prev+1 {
			prev = cpu
			continue
		}
		flush()
		start = cpu
		prev = cpu
	}
	flush()

	return strings.Join(parts, ",")
}

func readQemuSliceCPUs() ([]int, error) {
	data, err := os.ReadFile(qemuSliceCPUSetPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", qemuSliceCPUSetPath, err)
	}
	cpus, err := parseCPUList(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", qemuSliceCPUSetPath, err)
	}
	return cpus, nil
}

func listVCPUThreads(pid int) ([]vcpuThread, error) {
	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", taskDir, err)
	}

	var threads []vcpuThread
	for _, entry := range entries {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		commPath := filepath.Join(taskDir, entry.Name(), "comm")
		commBytes, err := os.ReadFile(commPath)
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(commBytes))
		if !strings.HasPrefix(comm, "CPU ") || !strings.HasSuffix(comm, "/KVM") {
			continue
		}

		indexText := strings.TrimSuffix(strings.TrimPrefix(comm, "CPU "), "/KVM")
		index, err := strconv.Atoi(indexText)
		if err != nil {
			continue
		}
		threads = append(threads, vcpuThread{index: index, tid: tid})
	}

	sort.Slice(threads, func(i, j int) bool {
		return threads[i].index < threads[j].index
	})
	return threads, nil
}

func pinVMCPUs(vmid int, placement cpuPlacement) error {
	if len(placement.HostCPUs) == 0 {
		return nil
	}

	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		if err := pinVMCPUsOnce(vmid, placement); err != nil {
			lastErr = err
			if attempt < 19 {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		return nil
	}
	return lastErr
}

func pinVMCPUsOnce(vmid int, placement cpuPlacement) error {
	pid, err := qmStatusPID(vmid)
	if err != nil {
		return err
	}
	threads, err := listVCPUThreads(pid)
	if err != nil {
		return err
	}
	if len(threads) == 0 {
		return fmt.Errorf("no KVM vCPU threads found for VM %d pid %d", vmid, pid)
	}

	if len(placement.HostCPUs) == 0 {
		return fmt.Errorf("no host CPUs selected for VM %d", vmid)
	}

	sharedMask := placement.Affinity()
	for i, thread := range threads {
		target := sharedMask
		if placement.Mode == cpuPlacementDedicated {
			target = strconv.Itoa(placement.HostCPUs[i%len(placement.HostCPUs)])
		}
		if _, err := runCmd("taskset", "-pc", target, strconv.Itoa(thread.tid)); err != nil {
			return fmt.Errorf("pin VM %d vCPU %d tid %d to host CPU %s: %w", vmid, thread.index, thread.tid, target, err)
		}
	}

	slog.Info("pinned VM vCPU threads", "vmid", vmid, "pid", pid, "mode", placement.Mode, "vcpus", len(threads), "host_cpus", placement.Affinity())
	return nil
}

// qmShutdown sends an ACPI shutdown to a VM and waits up to timeoutSec
// for it to power off. This gives the guest OS time to flush dirty pages
// and cleanly unmount filesystems — critical for snapshot integrity.
func qmShutdown(vmid, timeoutSec int) error {
	_, err := runCmd("qm", "shutdown", strconv.Itoa(vmid),
		"--timeout", strconv.Itoa(timeoutSec),
		"--forceStop", "1", // hard-stop after timeout as fallback
	)
	return err
}

// qmStop stops a VM immediately (hard power-off), using --skiplock to force.
// WARNING: does not flush guest filesystem — data written since last sync
// may be lost. Prefer qmShutdown for any flow that needs data integrity
// (e.g. snapshot/template conversion).
func qmStop(vmid int) error {
	_, err := runCmd("qm", "stop", strconv.Itoa(vmid), "--skiplock")
	return err
}

// qmDestroy destroys a VM, purging disks and ignoring locks.
func qmDestroy(vmid int) error {
	_, err := runCmd("qm", "destroy", strconv.Itoa(vmid), "--purge", "--skiplock")
	return err
}

// qmTemplate converts a VM into a Proxmox template (immutable, cloneable).
func qmTemplate(vmid int) error {
	_, err := runCmd("qm", "template", strconv.Itoa(vmid))
	return err
}

// snippetsDir is the Proxmox snippets directory for cloud-init custom configs.
const snippetsDir = "/var/lib/vz/snippets"

// writeCloudInitSnippet writes a cloud-init user-data YAML snippet that
// injects SSH keys without triggering user re-provisioning. Returns the
// Proxmox storage reference (e.g. "local:snippets/fastvm-60042.yaml").
//
// When cloud-init sees a new instance-id (which happens on every linked clone),
// it re-runs all once-per-instance modules including cc_users_groups. This
// recreates the user's home directory from /etc/skel, wiping any installed
// packages, venvs, or deployed files. Setting "users: []" tells cloud-init
// to skip user provisioning entirely while still allowing ssh_authorized_keys
// to be applied to the existing default user.
func writeCloudInitSnippet(vmid int, sshPubKey string) (string, error) {
	if err := os.MkdirAll(snippetsDir, 0755); err != nil {
		return "", fmt.Errorf("create snippets dir: %w", err)
	}

	filename := fmt.Sprintf("fastvm-%d.yaml", vmid)
	path := filepath.Join(snippetsDir, filename)

	// Minimal cloud-config:
	// - The single user entry with no_create_home prevents cc_users_groups
	//   from recreating the home directory while still providing a target
	//   user for ssh_authorized_keys injection.
	//   NOTE: "users: []" does NOT work — it skips cc_users_groups entirely,
	//   which means ssh_authorized_keys has no target user and keys are
	//   never injected. We must define the user explicitly.
	// - manage_etc_hosts: false prevents overwriting /etc/hosts
	content := fmt.Sprintf(`#cloud-config
# Auto-generated by fast-vm-spinner for VMID %d
# Preserves home directory on custom template clones
users:
  - name: ubuntu
    no_create_home: true
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - %s
manage_etc_hosts: false
`, vmid, strings.TrimSpace(sshPubKey))

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("write snippet %s: %w", path, err)
	}

	slog.Info("wrote cloud-init snippet", "vmid", vmid, "path", path)
	return fmt.Sprintf("local:snippets/%s", filename), nil
}

// removeCloudInitSnippet removes the cloud-init snippet for a VM.
func removeCloudInitSnippet(vmid int) {
	filename := fmt.Sprintf("fastvm-%d.yaml", vmid)
	path := filepath.Join(snippetsDir, filename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove cloud-init snippet", "vmid", vmid, "err", err)
	}
}

// listProxmoxVMIDs returns all VMIDs that exist on Proxmox (VMs + templates),
// filtered to the fastvm range (60000-60999). Used by NextVMID to avoid
// allocating VMIDs that exist as templates or externally-created VMs.
func listProxmoxVMIDs() []int {
	output, err := runCmd("qm", "list")
	if err != nil {
		slog.Warn("failed to list Proxmox VMs for VMID check", "err", err)
		return nil
	}

	var ids []int
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "VMID") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		if id >= vmidMin && id <= vmidMax {
			ids = append(ids, id)
		}
	}
	return ids
}
