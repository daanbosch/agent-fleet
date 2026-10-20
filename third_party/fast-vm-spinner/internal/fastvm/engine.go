package fastvm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// validVMName matches alphanumeric, dashes, and underscores (max 63 chars).
var validVMName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

// Engine is the main orchestrator for VM lifecycle operations.
// It replaces the bash CLI — all operations run via exec.Command directly.
type Engine struct {
	state    *StateManager
	stateDir string
	sshHost  string // public hostname/IP for SSH proxy commands
}

type proxmoxDestroyJob struct {
	vmid    string
	vmidInt int
	vm      VMState
}

// CreateOpts configures VM creation parameters.
type CreateOpts struct {
	Template      int    // Proxmox template VMID (default: 9012)
	Memory        int    // Memory in MB (default: 4096)
	Cores         int    // CPU cores (default: 2)
	Node          string // Proxmox node name
	SSHKey        string // Path to SSH public key on the Proxmox host
	SSHKeyContent string // Actual SSH public key content (takes precedence over SSHKey path)
	Name          string // Custom VM name (optional; if count>1, appends -1, -2, etc.)
	TTLMinutes    int    // Optional auto-expiry: VM destroyed after this many minutes
}

// ExecResult is the outcome of a guest-side command run.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// ExposedPortInfo describes a host port forwarding into a guest TCP port.
type ExposedPortInfo struct {
	VMID      string `json:"vmid"`
	GuestPort int    `json:"guest_port"`
	HostPort  int    `json:"host_port"`
}

// notSupportedBackend is the shared answer for capabilities the Proxmox
// backend does not implement (exec/expose/pause live in the Firecracker engine).
func notSupportedBackend(op string) error {
	return fmt.Errorf("%s not supported on the proxmox backend", op)
}

// VMInfo is the detailed information about a created/listed VM.
type VMInfo struct {
	VMID         string `json:"vmid"`
	Name         string `json:"name"`
	User         string `json:"user"`
	Template     int    `json:"template"`
	TemplateName string `json:"template_name,omitempty"`
	Node         string `json:"node"`
	IP           string `json:"ip,omitempty"`
	Created      string `json:"created"`
	SSHPort      int    `json:"ssh_port"`              // external SSH port (= VMID)
	SSHCommand   string `json:"ssh_command,omitempty"` // e.g. "ssh -p 60000 ubuntu@<host>"
	Backend      string `json:"backend,omitempty"`
	SSHUser      string `json:"ssh_user,omitempty"`
	// SSHKeyPending reports that a launch-time authorized key (-k) is being
	// appended asynchronously: allow up to ~1 s before first SSHing with it.
	SSHKeyPending bool `json:"ssh_key_pending,omitempty"`
	// TTL expiry (RFC3339) if set at create time.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Paused reports a VMM-paused guest.
	Paused bool `json:"paused,omitempty"`
	// ExposedPorts maps guest port -> host port.
	ExposedPorts map[int]int `json:"exposed_ports,omitempty"`
}

// Defaults for VM creation.
const (
	fallbackTemplate = 9012
	defaultMemory    = 4096
	defaultCores     = 2
	defaultNode      = "atl1-pc1-025"
	defaultSSHKey    = "/root/.ssh/id_rsa.pub"
	cloudInitUser    = "ubuntu"

	proxmoxMaxNICQueues = 64
)

func defaultTemplateID() int {
	return intEnv("FASTVM_DEFAULT_TEMPLATE", fallbackTemplate)
}

func intEnv(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("invalid integer environment variable; using fallback", "key", key, "value", v, "fallback", fallback)
		return fallback
	}
	return n
}

func isBaseTemplate(template, configuredDefault int) bool {
	return template == configuredDefault || template == fallbackTemplate
}

func proxmoxNICQueues(cores int) int {
	if cores <= 0 {
		return defaultCores
	}
	if cores > proxmoxMaxNICQueues {
		return proxmoxMaxNICQueues
	}
	return cores
}

// NewEngine creates a new Engine with the given state directory.
// sshHost is the public hostname/IP that users will SSH to (for proxy commands).
// If empty, it defaults to the machine's hostname.
func NewEngine(stateDir string, sshHost string) *Engine {
	if sshHost == "" {
		h, err := os.Hostname()
		if err != nil {
			h = "localhost"
		}
		sshHost = h
	}

	e := &Engine{
		state:    NewStateManager(stateDir),
		stateDir: stateDir,
		sshHost:  sshHost,
	}

	// Load existing port mappings from disk into in-memory allocator
	portAllocator.LoadExisting()

	// Clean up orphan proxy processes from previous runs
	vms, _ := e.state.ReadVMs()
	activeVMIDs := make(map[string]bool, len(vms))
	for vmid := range vms {
		activeVMIDs[vmid] = true
	}
	cleanupOrphanProxies(activeVMIDs)
	e.restoreRuntime(vms)
	go e.reconcileCPUPlacement("api-start")

	return e
}

func (e *Engine) restoreRuntime(vms map[string]VMState) {
	restoredUsers := make(map[string]bool)

	for _, vm := range vms {
		if restoredUsers[vm.User] {
			continue
		}
		restoredUsers[vm.User] = true

		userInfo, ok := e.state.GetUser(vm.User)
		if !ok {
			slog.Warn("cannot restore networking; user state missing", "user", vm.User)
			continue
		}
		if err := e.setupNetworking(vm.User, userInfo.Block); err != nil {
			slog.Warn("failed to restore user networking", "user", vm.User, "err", err)
		}
	}

	for vmidStr, vm := range vms {
		vmid, err := strconv.Atoi(vmidStr)
		if err != nil {
			slog.Warn("cannot restore SSH proxy; invalid VMID", "vmid", vmidStr)
			continue
		}

		userInfo, ok := e.state.GetUser(vm.User)
		if !ok {
			slog.Warn("cannot restore SSH proxy; user state missing", "vmid", vmidStr, "user", vm.User)
			continue
		}
		vmIP := calcVMIPFromName(vmidStr, vm.Name, vm.User, userInfo.Block)
		if sshProxyRunning(vmid) {
			slog.Info("SSH proxy already running", "vmid", vmidStr, "ip", vmIP, "port", ProxyPort(vmid))
			continue
		}
		if err := startSSHProxy(vmid, vmIP); err != nil {
			slog.Warn("failed to restore SSH proxy", "vmid", vmidStr, "ip", vmIP, "err", err)
		}
	}
}

// SSHHost returns the configured SSH host for proxy commands.
func (e *Engine) SSHHost() string {
	return e.sshHost
}

// State returns the underlying StateManager for direct reads.
func (e *Engine) State() *StateManager {
	return e.state
}

// CreateVMs allocates a network block, sets up networking, clones VMs,
// configures them with cloud-init, and starts them in parallel.
func (e *Engine) CreateVMs(user string, count int, opts CreateOpts) ([]VMInfo, error) {
	defaultTemplate := defaultTemplateID()

	// Apply defaults
	if opts.Template == 0 {
		opts.Template = defaultTemplate
	}
	if opts.Memory == 0 {
		opts.Memory = defaultMemory
	}
	if opts.Cores == 0 {
		opts.Cores = defaultCores
	}
	if opts.Node == "" {
		opts.Node = defaultNode
	}
	// Handle SSH key: content takes precedence over path
	if opts.SSHKeyContent != "" {
		// Write the key content to a temp file on this host
		tmpKey := filepath.Join(e.stateDir, fmt.Sprintf("ssh-key-%s.pub", user))
		if err := os.WriteFile(tmpKey, []byte(opts.SSHKeyContent+"\n"), 0644); err != nil {
			return nil, fmt.Errorf("write SSH key: %w", err)
		}
		opts.SSHKey = tmpKey
	}
	if opts.SSHKey == "" {
		opts.SSHKey = defaultSSHKey
	}

	if count < 1 || count > 50 {
		return nil, fmt.Errorf("count must be 1-50, got %d", count)
	}

	// Validate custom name if provided
	if opts.Name != "" {
		if !validVMName.MatchString(opts.Name) {
			return nil, fmt.Errorf("invalid VM name %q: must be alphanumeric with dashes/underscores, 1-63 chars, starting with alphanumeric", opts.Name)
		}
	}

	log := slog.With("user", user, "count", count, "template", opts.Template)
	log.Info("creating VMs")
	securityTemplate := proxmoxTemplateHasSecurityBaseline(e.state, opts.Template)

	// Allocate/get network block for user
	block, err := e.state.AllocateBlock(user)
	if err != nil {
		return nil, fmt.Errorf("allocate block: %w", err)
	}

	bridge := bridgeName(user)
	gwIP := blockGateway(block)

	log.Info("network block assigned",
		"block", block,
		"bridge", bridge,
		"gateway", gwIP,
		"cidr", blockCIDR(block),
	)

	// Set up networking
	if err := e.setupNetworking(user, block); err != nil {
		return nil, fmt.Errorf("setup networking: %w", err)
	}

	// Clone and configure VMs
	type vmEntry struct {
		vmid    int
		name    string
		ip      string
		sshPort int
	}

	var entries []vmEntry

	// Phase 1: Allocate VMIDs and register in state (must be sequential — state ops)
	type vmPlan struct {
		vmid   int
		name   string
		suffix int
		ip     string
	}
	var plans []vmPlan

	for i := 1; i <= count; i++ {
		vmid, err := e.state.NextVMID()
		if err != nil {
			return nil, fmt.Errorf("allocate VMID: %w", err)
		}

		var name string
		var suffix int
		if opts.Name != "" {
			// Custom name: use as-is for count==1, append suffix for count>1
			if count == 1 {
				name = opts.Name
			} else {
				name = fmt.Sprintf("%s-%d", opts.Name, i)
			}
			suffix = i
			// Check for name collision
			if e.state.NameExists(name) {
				return nil, fmt.Errorf("VM name %q already exists", name)
			}
		} else {
			// Auto-generate name
			suffix = i
			name = fmt.Sprintf("fastvm-%s-%d", user, suffix)
			for e.state.NameExists(name) {
				suffix++
				name = fmt.Sprintf("fastvm-%s-%d", user, suffix)
			}
		}

		vmIP := blockVMIP(block, suffix)

		// Register in state immediately so NextVMID/NameExists work for next iteration
		vmState := VMState{
			User:             user,
			Name:             name,
			Template:         opts.Template,
			Node:             opts.Node,
			Memory:           opts.Memory,
			Cores:            opts.Cores,
			SecurityTemplate: securityTemplate,
			Created:          time.Now().UTC().Format(time.RFC3339),
		}
		if err := e.state.RegisterVM(strconv.Itoa(vmid), vmState); err != nil {
			return nil, fmt.Errorf("register VM %d: %w", vmid, err)
		}

		plans = append(plans, vmPlan{vmid: vmid, name: name, suffix: suffix, ip: vmIP})
	}

	// Phase 2: Clone + configure + start ALL in parallel
	log.Info("cloning, configuring, and starting VMs in parallel", "count", len(plans))
	placements, err := e.cpuPlacementsForCurrentState()
	if err != nil {
		log.Warn("failed to calculate CPU placements", "err", err)
	}
	g := new(errgroup.Group)
	g.SetLimit(10) // max 10 concurrent VM operations

	var mu sync.Mutex

	for _, plan := range plans {
		p := plan
		g.Go(func() error {
			log.Info("creating VM", "vmid", p.vmid, "name", p.name, "ip", p.ip)

			// Clone (linked clone)
			if err := qmClone(opts.Template, p.vmid, p.name); err != nil {
				mu.Lock()
				// Unregister on failure
				e.state.UnregisterVM(strconv.Itoa(p.vmid))
				mu.Unlock()
				return fmt.Errorf("clone %d: %w", p.vmid, err)
			}

			// Configure — cpu must be set separately because qm set
			// with many flags can silently drop --cpu in some Proxmox versions
			isCustomTemplate := !isBaseTemplate(opts.Template, defaultTemplate)

			qmOpts := map[string]string{
				"cores":      strconv.Itoa(opts.Cores),
				"memory":     strconv.Itoa(opts.Memory),
				"ipconfig0":  fmt.Sprintf("ip=%s/%d,gw=%s", p.ip, blockMask, gwIP),
				"nameserver": dnsServer,
				"net0":       fmt.Sprintf("virtio,bridge=%s,queues=%d", bridge, proxmoxNICQueues(opts.Cores)),
				"onboot":     "0",
				"agent":      "enabled=1",
			}
			if placement, ok := placements[p.vmid]; ok && placement.Affinity() != "" {
				qmOpts["affinity"] = placement.Affinity()
			}

			// For the default base template, apply full cloud-init config.
			// For custom templates (user snapshots), strip inherited
			// cloud-init user config so installed packages, venvs,
			// etc. are preserved on the cloned VM.
			if !isCustomTemplate {
				qmOpts["ciuser"] = cloudInitUser
				qmOpts["sshkeys"] = opts.SSHKey
			} else {
				log.Info("custom template: using cicustom to preserve snapshot state",
					"template", opts.Template, "vmid", p.vmid)
				// Delete inherited ciuser/sshkeys/cicustom from the cloned VM
				// config. Without this, linked clones inherit ciuser
				// from the parent template, Proxmox regenerates the
				// config drive ISO with "user: <ciuser>", and
				// cloud-init re-provisions the user on first boot —
				// wiping the home directory and installed packages.
				if err := qmDelete(p.vmid, "ciuser", "sshkeys", "cicustom"); err != nil {
					log.Warn("failed to delete inherited cloud-init config", "vmid", p.vmid, "err", err)
				}

				// Use cicustom to inject SSH keys without triggering user
				// re-provisioning. This writes a cloud-config snippet with
				// "users: []" so cloud-init skips cc_users_groups entirely.
				if opts.SSHKeyContent != "" || opts.SSHKey != defaultSSHKey {
					// Read the SSH public key content
					sshPubKey := opts.SSHKeyContent
					if sshPubKey == "" {
						keyBytes, err := os.ReadFile(opts.SSHKey)
						if err != nil {
							return fmt.Errorf("read SSH key %s: %w", opts.SSHKey, err)
						}
						sshPubKey = string(keyBytes)
					}

					snippetRef, err := writeCloudInitSnippet(p.vmid, sshPubKey)
					if err != nil {
						log.Warn("failed to write cloud-init snippet, falling back to sshkeys",
							"vmid", p.vmid, "err", err)
						qmOpts["sshkeys"] = opts.SSHKey
					} else {
						qmOpts["cicustom"] = fmt.Sprintf("user=%s", snippetRef)
					}
				}
			}

			if err := qmSet(p.vmid, qmOpts); err != nil {
				return fmt.Errorf("configure %d: %w", p.vmid, err)
			}
			// Set CPU type separately to ensure it's applied
			if err := qmSet(p.vmid, map[string]string{"cpu": "host"}); err != nil {
				log.Warn("failed to set cpu=host", "vmid", p.vmid, "err", err)
			}

			// Tune disk: enable discard (TRIM) on the cloned scsi0 disk.
			// iothread is intentionally omitted — it causes Ceph msgr-worker
			// segfaults on this RBD storage backend.
			if diskVal, err := qmGetConfigValue(p.vmid, "scsi0"); err == nil {
				tuned := strings.ReplaceAll(diskVal, ",discard=on", "")
				tuned += ",discard=on"
				if err := qmSet(p.vmid, map[string]string{"scsi0": tuned}); err != nil {
					log.Warn("failed to set disk discard flag", "vmid", p.vmid, "err", err)
				}
			} else {
				log.Warn("failed to read scsi0 config for disk tuning", "vmid", p.vmid, "err", err)
			}

			// Skip cloud-init update for custom templates to preserve user-space state
			if !isCustomTemplate {
				if err := qmCloudInitUpdate(p.vmid); err != nil {
					log.Warn("cloud-init update failed", "vmid", p.vmid, "err", err)
				}
			}

			// Start
			if err := qmStart(p.vmid); err != nil {
				return fmt.Errorf("start %d: %w", p.vmid, err)
			}
			if placement, ok := placements[p.vmid]; ok {
				if err := pinVMCPUs(p.vmid, placement); err != nil {
					log.Warn("failed to pin VM CPU threads", "vmid", p.vmid, "placement", cpuPlacementSummary(placement), "err", err)
				}
			}

			// Start SSH proxy (port VMID → vmIP:22)
			if err := startSSHProxy(p.vmid, p.ip); err != nil {
				log.Warn("SSH proxy failed (non-fatal)", "vmid", p.vmid, "err", err)
			}

			mu.Lock()
			actualPort := ProxyPort(p.vmid)
			entries = append(entries, vmEntry{vmid: p.vmid, name: p.name, ip: p.ip, sshPort: actualPort})
			mu.Unlock()

			log.Info("VM ready", "vmid", p.vmid, "ip", p.ip)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		log.Error("some VMs failed to create", "err", err)
		// If ALL VMs failed, return the error
		if len(entries) == 0 {
			return nil, fmt.Errorf("all VMs failed to create: %w", err)
		}
		// Otherwise continue with whatever succeeded
	}
	e.reconcileCPUPlacement("create")

	// Remove the old sequential start block — starts are now part of Phase 2
	// Sort entries by VMID for consistent output
	sort.Slice(entries, func(i, j int) bool { return entries[i].vmid < entries[j].vmid })

	// Build result
	result := make([]VMInfo, 0, len(entries))
	for _, entry := range entries {
		sshPort := entry.sshPort
		if sshPort == 0 {
			sshPort = entry.vmid
		}
		result = append(result, VMInfo{
			VMID:       strconv.Itoa(entry.vmid),
			Name:       entry.name,
			User:       user,
			Template:   opts.Template,
			Node:       opts.Node,
			IP:         entry.ip,
			Created:    time.Now().UTC().Format(time.RFC3339),
			SSHPort:    sshPort,
			SSHCommand: fmt.Sprintf("ssh -p %d %s@%s", sshPort, cloudInitUser, e.sshHost),
		})
	}

	log.Info("VMs created successfully", "count", len(result))
	return result, nil
}

// DestroyVM queues destruction of a single VM and immediately removes it from
// FastVM state. Proxmox disk purge can take tens of seconds, so the blocking
// qm work continues in the background to avoid client-side HTTP timeouts.
func (e *Engine) DestroyVM(vmid string) error {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}

	log := slog.With("vmid", vmid, "user", vm.User, "name", vm.Name)
	log.Info("destroying VM")

	vmidInt, err := strconv.Atoi(vmid)
	if err != nil {
		return fmt.Errorf("invalid VMID %s: %w", vmid, err)
	}

	// Stop SSH proxy first
	if err := stopSSHProxy(vmidInt); err != nil {
		log.Warn("stop SSH proxy failed", "err", err)
	}

	e.queueProxmoxDestroy([]proxmoxDestroyJob{{
		vmid:    vmid,
		vmidInt: vmidInt,
		vm:      *vm,
	}}, "destroy")

	if err := e.state.UnregisterVM(vmid); err != nil {
		log.Warn("unregister failed", "err", err)
	}

	log.Info("VM destroy queued")

	e.cleanupUserNetworkingIfEmpty(vm.User)

	return nil
}

func (e *Engine) queueProxmoxDestroy(jobs []proxmoxDestroyJob, reason string) {
	if len(jobs) == 0 {
		return
	}

	queued := append([]proxmoxDestroyJob(nil), jobs...)
	go func() {
		slog.Info("background Proxmox destroy queue started", "reason", reason, "count", len(queued))
		for _, job := range queued {
			log := slog.With("vmid", job.vmid, "user", job.vm.User, "name", job.vm.Name, "reason", reason)
			log.Info("background Proxmox destroy started")

			if err := qmStop(job.vmidInt); err != nil {
				log.Warn("background Proxmox stop failed; continuing destroy", "err", err)
			}
			if err := qmDestroy(job.vmidInt); err != nil {
				log.Error("background Proxmox destroy failed", "err", err)
				continue
			}

			removeCloudInitSnippet(job.vmidInt)
			log.Info("background Proxmox destroy completed")
		}
		e.reconcileCPUPlacement(reason)
		slog.Info("background Proxmox destroy queue completed", "reason", reason, "count", len(queued))
	}()
}

// DestroyUserVMs stops and destroys all VMs for a user.
func (e *Engine) DestroyUserVMs(user string) error {
	vmids := e.state.UserVMIDs(user)
	if len(vmids) == 0 {
		return fmt.Errorf("no VMs found for user '%s'", user)
	}

	log := slog.With("user", user, "count", len(vmids))
	log.Info("destroying all user VMs")

	var jobs []proxmoxDestroyJob
	for _, vmid := range vmids {
		vm, ok := e.state.GetVM(vmid)
		if !ok {
			continue
		}

		vmidInt, err := strconv.Atoi(vmid)
		if err != nil {
			log.Warn("invalid VMID in state during destroy-user", "vmid", vmid)
			continue
		}

		stopSSHProxy(vmidInt)
		jobs = append(jobs, proxmoxDestroyJob{vmid: vmid, vmidInt: vmidInt, vm: *vm})

		if err := e.state.UnregisterVM(vmid); err != nil {
			log.Warn("unregister failed", "vmid", vmid, "err", err)
		}
	}

	if len(jobs) > 0 {
		e.queueProxmoxDestroy(jobs, "destroy-user")
	}

	e.cleanupUserNetworkingIfEmpty(user)

	log.Info("all VM destroys queued", "queued", len(jobs))
	return nil
}

// Converge keeps a single VM and destroys all others for a user.
func (e *Engine) Converge(user, keepVMID string) error {
	// Verify the keep VM exists and belongs to user
	keepVM, ok := e.state.GetVM(keepVMID)
	if !ok {
		return fmt.Errorf("VM %s not found", keepVMID)
	}
	if keepVM.User != user {
		return fmt.Errorf("VM %s belongs to '%s', not '%s'", keepVMID, keepVM.User, user)
	}

	vmids := e.state.UserVMIDs(user)
	var destroyIDs []string
	for _, id := range vmids {
		if id != keepVMID {
			destroyIDs = append(destroyIDs, id)
		}
	}

	if len(destroyIDs) == 0 {
		slog.Info("nothing to converge — only one VM", "vmid", keepVMID)
		return nil
	}

	log := slog.With("user", user, "keep", keepVMID, "destroy_count", len(destroyIDs))
	log.Info("converging")

	var jobs []proxmoxDestroyJob
	for _, vmid := range destroyIDs {
		vm, ok := e.state.GetVM(vmid)
		if !ok {
			continue
		}

		vmidInt, err := strconv.Atoi(vmid)
		if err != nil {
			log.Warn("invalid VMID in state during converge", "vmid", vmid)
			continue
		}

		stopSSHProxy(vmidInt)
		jobs = append(jobs, proxmoxDestroyJob{vmid: vmid, vmidInt: vmidInt, vm: *vm})

		if err := e.state.UnregisterVM(vmid); err != nil {
			log.Warn("unregister failed", "vmid", vmid, "err", err)
		}
	}

	if len(jobs) > 0 {
		e.queueProxmoxDestroy(jobs, "converge")
	}

	log.Info("converge destroy queued", "queued", len(jobs))
	return nil
}

// ListVMs returns all VMs, optionally filtered by user, with calculated IPs.
func (e *Engine) ListVMs(user string) ([]VMInfo, error) {
	vms, err := e.state.ReadVMs()
	if err != nil {
		return nil, err
	}

	users, _ := e.state.ReadUsers()

	var result []VMInfo
	for vmid, vm := range vms {
		if user != "" && vm.User != user {
			continue
		}

		vmidInt, _ := strconv.Atoi(vmid)
		sshPort := ProxyPort(vmidInt)
		info := VMInfo{
			VMID:     vmid,
			Name:     vm.Name,
			User:     vm.User,
			Template: vm.Template,
			Node:     vm.Node,
			Created:  vm.Created,
			SSHPort:  sshPort,
		}

		// Calculate deterministic IP
		if userInfo, ok := users[vm.User]; ok {
			info.IP = calcVMIPFromName(vmid, vm.Name, vm.User, userInfo.Block)
		}

		if sshPort > 0 {
			info.SSHCommand = fmt.Sprintf("ssh -p %d %s@%s", sshPort, cloudInitUser, e.sshHost)
		}

		result = append(result, info)
	}

	return result, nil
}

// GetVM returns detailed info for a single VM.
func (e *Engine) GetVM(vmid string) (*VMInfo, error) {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return nil, fmt.Errorf("VM %s not found", vmid)
	}

	vmidInt, _ := strconv.Atoi(vmid)
	sshPort := ProxyPort(vmidInt)
	info := &VMInfo{
		VMID:     vmid,
		Name:     vm.Name,
		User:     vm.User,
		Template: vm.Template,
		Node:     vm.Node,
		Created:  vm.Created,
		SSHPort:  sshPort,
	}

	if userInfo, ok := e.state.GetUser(vm.User); ok {
		info.IP = calcVMIPFromName(vmid, vm.Name, vm.User, userInfo.Block)
	}

	if sshPort > 0 {
		info.SSHCommand = fmt.Sprintf("ssh -p %d %s@%s", sshPort, cloudInitUser, e.sshHost)
	}

	return info, nil
}

// GetVMByName finds a VM by its name and returns detailed info.
// PauseVM is a Firecracker-backend capability.
func (e *Engine) PauseVM(vmid string) error { return notSupportedBackend("pause") }

// ResumeVM is a Firecracker-backend capability.
func (e *Engine) ResumeVM(vmid string) error { return notSupportedBackend("resume") }

// ExecVM is a Firecracker-backend capability.
func (e *Engine) ExecVM(vmid, command string, timeoutSec int) (*ExecResult, error) {
	return nil, notSupportedBackend("exec")
}

// ExposePort is a Firecracker-backend capability.
func (e *Engine) ExposePort(vmid string, guestPort int) (*ExposedPortInfo, error) {
	return nil, notSupportedBackend("expose")
}

// UnexposePort is a Firecracker-backend capability.
func (e *Engine) UnexposePort(vmid string, hostPort int) error {
	return notSupportedBackend("unexpose")
}

func (e *Engine) GetVMByName(name string) (*VMInfo, error) {
	vmid, vm, ok := e.state.FindVMByName(name)
	if !ok {
		return nil, fmt.Errorf("VM with name %q not found", name)
	}

	vmidInt, _ := strconv.Atoi(vmid)
	sshPort := ProxyPort(vmidInt)
	info := &VMInfo{
		VMID:     vmid,
		Name:     vm.Name,
		User:     vm.User,
		Template: vm.Template,
		Node:     vm.Node,
		Created:  vm.Created,
		SSHPort:  sshPort,
	}

	if userInfo, ok := e.state.GetUser(vm.User); ok {
		info.IP = calcVMIPFromName(vmid, vm.Name, vm.User, userInfo.Block)
	}

	if sshPort > 0 {
		info.SSHCommand = fmt.Sprintf("ssh -p %d %s@%s", sshPort, cloudInitUser, e.sshHost)
	}

	return info, nil
}

// ResolveVMID takes a name or numeric VMID and returns the VMID string.
// It tries VMID lookup first (if numeric), then falls back to name lookup.
func (e *Engine) ResolveVMID(nameOrID string) (string, error) {
	// Try as VMID first (numeric)
	if _, err := strconv.Atoi(nameOrID); err == nil {
		if _, ok := e.state.GetVM(nameOrID); ok {
			return nameOrID, nil
		}
		// Numeric but not found — still try name lookup as fallback
	}

	// Try name lookup
	vmid, _, ok := e.state.FindVMByName(nameOrID)
	if ok {
		return vmid, nil
	}

	return "", fmt.Errorf("VM %q not found (tried VMID and name lookup)", nameOrID)
}

// Status returns an overview of all fastvm resources.
func (e *Engine) Status() (*StatusInfo, error) {
	status, err := e.state.ReadStatus()
	if err != nil {
		return nil, err
	}
	status.Backend = "proxmox"
	return status, nil
}

// SnapshotVM stops a running VM, converts it to a Proxmox template, and
// unregisters it from active state. The resulting template VMID can be passed
// to CreateVMs via -t to clone from a user-customized image.
func (e *Engine) SnapshotVM(vmid string) error {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}

	vmidInt, err := strconv.Atoi(vmid)
	if err != nil {
		return fmt.Errorf("invalid VMID %s: %w", vmid, err)
	}

	log := slog.With("vmid", vmid, "user", vm.User, "name", vm.Name)
	log.Info("snapshotting VM to template")
	if err := requireProxmoxSnapshotSecurityBaseline(e.state, vmid, vm); err != nil {
		return err
	}

	// Graceful shutdown: gives the guest OS time to flush dirty pages to disk.
	// Without this, qm stop (hard power-off) can lose recently-written data,
	// resulting in zero-byte files and corrupt pip/venv installs in clones.
	// --forceStop=1 ensures we don't hang if ACPI shutdown is unresponsive.
	if err := qmShutdown(vmidInt, 30); err != nil {
		log.Warn("graceful shutdown failed, falling back to hard stop", "err", err)
		if err := qmStop(vmidInt); err != nil {
			log.Warn("hard stop also failed (may already be stopped)", "err", err)
		}
	}

	// Stop SSH proxy
	if err := stopSSHProxy(vmidInt); err != nil {
		log.Warn("stop SSH proxy failed", "err", err)
	}

	// Convert to Proxmox template
	if err := qmTemplate(vmidInt); err != nil {
		return fmt.Errorf("convert VM %s to template: %w", vmid, err)
	}

	if err := e.state.RegisterTemplate(vmid, TemplateState{
		User:               vm.User,
		SourceVMID:         vmid,
		SourceTemplate:     vm.Template,
		SourceTemplateName: vm.TemplateName,
		Backend:            "proxmox",
		SecurityTemplate:   true,
		Created:            time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("register template %s: %w", vmid, err)
	}

	// Unregister from active VM state (it's a template now, not a running VM)
	if err := e.state.UnregisterVM(vmid); err != nil {
		log.Warn("unregister failed", "err", err)
	}

	log.Info("VM converted to template", "template_vmid", vmid)

	// Clean up networking if no VMs left for this user
	e.cleanupUserNetworkingIfEmpty(vm.User)

	return nil
}

// cleanupUserNetworkingIfEmpty tears down networking if the user has no more VMs.
func (e *Engine) cleanupUserNetworkingIfEmpty(user string) {
	if e.state.UserHasVMs(user) {
		return
	}

	userInfo, ok := e.state.GetUser(user)
	if !ok {
		return
	}

	slog.Info("no VMs remaining, tearing down networking", "user", user)
	e.teardownNetworking(user, userInfo.Block)
	e.state.RemoveUser(user)
}

// calcVMIPFromName derives the deterministic IP for a VM based on the user's block
// and the VM's suffix number extracted from its name.
func calcVMIPFromName(vmid, name, user string, block int) string {
	suffix := 1
	prefix := fmt.Sprintf("fastvm-%s-", user)
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		rest := name[len(prefix):]
		if n, err := strconv.Atoi(rest); err == nil {
			suffix = n
		}
	}
	return blockVMIP(block, suffix)
}

// WarmSnapshot is the firecracker-only warm (memory-preserving) template flow.
func (e *Engine) WarmSnapshot(vmid, name string) (*WarmTemplate, error) {
	return nil, fmt.Errorf("warm snapshots are not supported on the proxmox backend")
}

// WarmLaunch restores a warm-template clone (firecracker backend only).
func (e *Engine) WarmLaunch(user, template, name, sshPubKey string) (VMInfo, error) {
	return VMInfo{}, fmt.Errorf("warm snapshots are not supported on the proxmox backend")
}

// ListWarmTemplates returns nothing on non-firecracker backends.
func (e *Engine) ListWarmTemplates() ([]WarmTemplate, error) {
	return nil, fmt.Errorf("warm snapshots are not supported on the proxmox backend")
}

// ListWarmTemplatesForUser returns nothing on non-firecracker backends.
func (e *Engine) ListWarmTemplatesForUser(string) ([]WarmTemplate, error) {
	return nil, fmt.Errorf("warm snapshots are not supported on the proxmox backend")
}

// UpdateWarmTemplateACL is not supported on non-firecracker backends.
func (e *Engine) UpdateWarmTemplateACL(_, _ string, _ bool, _ []string) (*WarmTemplate, error) {
	return nil, fmt.Errorf("warm snapshots are not supported on the proxmox backend")
}
