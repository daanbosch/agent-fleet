package fastvm

import (
	crand "crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const firecrackerBackendName = "firecracker"
const firecrackerMaxVCPUs = 32

// FirecrackerConfig configures the server-side Firecracker backend.
type FirecrackerConfig struct {
	StateDir        string
	SSHHost         string
	InstallDir      string
	Bin             string
	DefaultTemplate string
	DefaultMemory   int
	DefaultCores    int
	Node            string
	SSHUser         string
}

// FirecrackerEngine exposes Firecracker microVMs through the same API contract
// used by the Proxmox-backed Engine.
type FirecrackerEngine struct {
	state           *StateManager
	sshHost         string
	installDir      string
	bin             string
	defaultTemplate string
	defaultMemory   int
	defaultCores    int
	node            string
	sshUser         string
}

// NewFirecrackerEngine creates a Firecracker-backed fastvm API engine.
func NewFirecrackerEngine(cfg FirecrackerConfig) *FirecrackerEngine {
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/fastvm"
	}
	if cfg.SSHHost == "" {
		h, err := os.Hostname()
		if err != nil {
			h = "localhost"
		}
		cfg.SSHHost = h
	}
	if cfg.InstallDir == "" {
		cfg.InstallDir = "/opt/fastvm-microvm"
	}
	if cfg.Bin == "" {
		cfg.Bin = filepath.Join(cfg.InstallDir, "bin", "firecrackerctl")
	}
	if cfg.DefaultTemplate == "" {
		cfg.DefaultTemplate = "9013"
	}
	if cfg.DefaultMemory == 0 {
		cfg.DefaultMemory = defaultMemory
	}
	if cfg.DefaultCores == 0 {
		cfg.DefaultCores = defaultCores
	}
	if cfg.Node == "" {
		cfg.Node = "atl1-pc1-027"
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = "root"
	}

	e := &FirecrackerEngine{
		state:           NewStateManager(cfg.StateDir),
		sshHost:         cfg.SSHHost,
		installDir:      cfg.InstallDir,
		bin:             cfg.Bin,
		defaultTemplate: cfg.DefaultTemplate,
		defaultMemory:   cfg.DefaultMemory,
		defaultCores:    cfg.DefaultCores,
		node:            cfg.Node,
		sshUser:         cfg.SSHUser,
	}

	portAllocator.LoadExisting()
	vms, _ := e.state.ReadVMs()
	activeVMIDs := make(map[string]bool, len(vms))
	for vmid := range vms {
		activeVMIDs[vmid] = true
	}
	cleanupOrphanProxies(activeVMIDs)
	e.restoreRuntime(vms)

	return e
}

// SSHHost returns the configured SSH host for proxy commands.
func (e *FirecrackerEngine) SSHHost() string {
	return e.sshHost
}

// CreateVMs creates Firecracker instances while preserving the fastvm API shape.
func (e *FirecrackerEngine) CreateVMs(user string, count int, opts CreateOpts) ([]VMInfo, error) {
	if count < 1 || count > 50 {
		return nil, fmt.Errorf("count must be 1-50, got %d", count)
	}
	if opts.Memory == 0 {
		opts.Memory = e.defaultMemory
	}
	if opts.Cores == 0 {
		opts.Cores = e.defaultCores
	}
	if opts.Cores > firecrackerMaxVCPUs {
		return nil, newInvalidRequestError("Firecracker supports at most %d vCPUs per microVM; requested %d. Use the Proxmox backend for a single larger VM or split the workload across multiple Firecracker VMs", firecrackerMaxVCPUs, opts.Cores)
	}
	if opts.Node == "" {
		opts.Node = e.node
	}
	if opts.Name != "" && !validVMName.MatchString(opts.Name) {
		return nil, fmt.Errorf("invalid VM name %q: must be alphanumeric with dashes/underscores, 1-63 chars, starting with alphanumeric", opts.Name)
	}
	if _, err := os.Stat(e.bin); err != nil {
		return nil, fmt.Errorf("firecrackerctl unavailable at %s: %w", e.bin, err)
	}

	templateName := e.templateName(opts.Template)
	templateID := e.templateID(opts.Template, templateName)
	securityTemplate := firecrackerTemplateOrRegistryHasSecurityBaseline(e.state, e.installDir, templateName)
	sshPubKey, err := firecrackerSSHKeyContent(opts)
	if err != nil {
		return nil, err
	}

	if _, err := e.state.AllocateBlock(user); err != nil {
		return nil, fmt.Errorf("allocate user state: %w", err)
	}

	log := slog.With("backend", firecrackerBackendName, "user", user, "count", count, "template", templateName)
	log.Info("creating Firecracker VMs")

	type vmPlan struct {
		vmid   int
		id     string
		name   string
		suffix int
	}
	var plans []vmPlan

	reservedIDs := e.reservedFirecrackerIDs()
	for i := 1; i <= count; i++ {
		vmid, err := e.state.ClaimVMID(reservedIDs)
		if err == nil {
			defer e.state.ReleaseVMID(vmid)
		}
		if err != nil {
			return nil, fmt.Errorf("allocate VMID: %w", err)
		}
		id := strconv.Itoa(vmid)

		var name string
		suffix := i
		if opts.Name != "" {
			if count == 1 {
				name = opts.Name
			} else {
				name = fmt.Sprintf("%s-%d", opts.Name, i)
			}
			if e.state.NameExists(name) {
				return nil, fmt.Errorf("VM name %q already exists", name)
			}
		} else {
			name = fmt.Sprintf("fastvm-%s-%d", user, suffix)
			for e.state.NameExists(name) {
				suffix++
				name = fmt.Sprintf("fastvm-%s-%d", user, suffix)
			}
		}

		vmState := VMState{
			User:             user,
			Name:             name,
			Template:         templateID,
			TemplateName:     templateName,
			Node:             opts.Node,
			Memory:           opts.Memory,
			Cores:            opts.Cores,
			Backend:          firecrackerBackendName,
			SSHUser:          e.sshUser,
			SecurityTemplate: securityTemplate,
			Created:          time.Now().UTC().Format(time.RFC3339),
		}
		if err := e.state.RegisterVM(id, vmState); err != nil {
			return nil, fmt.Errorf("register VM %s: %w", id, err)
		}
		reservedIDs[vmid] = true
		plans = append(plans, vmPlan{vmid: vmid, id: id, name: name, suffix: suffix})
	}

	type vmEntry struct {
		vmid    int
		id      string
		name    string
		ip      string
		sshPort int
		created string
	}
	var entries []vmEntry
	var mu sync.Mutex

	g := new(errgroup.Group)
	g.SetLimit(10)
	for _, plan := range plans {
		p := plan
		g.Go(func() error {
			log.Info("creating Firecracker VM", "vmid", p.id, "name", p.name)
			prof := newProf("cold_create", p.id)
			prof.name = p.name
			prof.addDetail("cli", "firecrackerctl clone")
			out, err := e.firecrackerctl("clone", templateName, p.id,
				"--vcpus", strconv.Itoa(opts.Cores),
				"--mem-mib", strconv.Itoa(opts.Memory),
				"--install-dir", e.installDir,
			)
			prof.stage("ctl_clone_total")
			if err != nil {
				e.state.UnregisterVM(p.id)
				prof.finish("ctl_failed")
				return fmt.Errorf("clone %s: %w", p.id, err)
			}

			values := parseFirecrackerKeyValues(out)
			// The ctl emits its own internal stage timings; fold them in as
			// authoritative splits of the ctl_clone_total segment.
			for _, kv := range []struct {
				key, stage string
			}{
				{"TIME_PLACEMENT_MS", "ctl_placement"},
				{"TIME_ROOTFS_CLONE_MS", "ctl_rootfs_clone"},
				{"TIME_NETWORK_MS", "ctl_network"},
				{"TIME_FIRECRACKER_START_MS", "ctl_fc_start"},
				{"TIME_CONFIGURE_VM_MS", "ctl_configure_vm"},
				{"TIME_STATE_MS", "ctl_state"},
				{"TIME_SSH_READY_MS", "ctl_ssh_ready"},
				{"TIME_TOTAL_MS", "ctl_reported_total"},
			} {
				if ms, ok := msFromString(values[kv.key]); ok {
					prof.addStage(kv.stage, ms)
				}
			}
			prof.addDetail("rootfs_clone_method", values["ROOTFS_CLONE_METHOD"])
			guestIP := values["GUEST_IP"]
			if guestIP == "" {
				e.cleanupFailedCreate(p.id, p.vmid)
				return fmt.Errorf("clone %s did not return GUEST_IP", p.id)
			}

			if sshPubKey != "" {
				if err := e.installGuestSSHKey(guestIP, sshPubKey); err != nil {
					e.cleanupFailedCreate(p.id, p.vmid)
					prof.finish("ssh_key_install_failed")
					return err
				}
				prof.stage("ssh_key_install")
			}

			if err := startSSHProxy(p.vmid, guestIP); err != nil {
				log.Warn("SSH proxy failed (non-fatal)", "vmid", p.id, "ip", guestIP, "err", err)
			}
			prof.stage("ssh_proxy_spawn")

			// P1 isolation parity: attach the per-tap L3 lock (source-address
			// lock + destination policy) to this VM's tap. Identity NAT
			// parameters — no rewriting, enforcement only. Non-fatal: the
			// lock is defense in depth, the VM stays usable without it.
			if tap := values["TAP"]; tap != "" {
				if env, err := readInstanceEnv(filepath.Join(e.installDir, "instances", p.id)); err == nil && env["TAP_IP"] != "" {
					if err := e.attachL3Lock(tap, guestIP, strings.TrimSuffix(env["TAP_IP"], "/24")); err != nil {
						log.Warn("L3 lock attach failed (non-fatal)", "vmid", p.id, "tap", tap, "err", err)
					} else {
						prof.stage("l3_lock_attach")
					}
				}
			}

			created := time.Now().UTC().Format(time.RFC3339)
			vmState := VMState{
				User:             user,
				Name:             p.name,
				Template:         templateID,
				TemplateName:     templateName,
				Node:             opts.Node,
				Memory:           opts.Memory,
				Cores:            opts.Cores,
				IP:               guestIP,
				Backend:          firecrackerBackendName,
				SSHUser:          e.sshUser,
				SecurityTemplate: securityTemplate,
				Created:          created,
			}
			if opts.TTLMinutes > 0 {
				vmState.ExpiresAt = time.Now().UTC().
					Add(time.Duration(opts.TTLMinutes) * time.Minute).
					Format(time.RFC3339)
			}
			if err := e.state.RegisterVM(p.id, vmState); err != nil {
				e.cleanupFailedCreate(p.id, p.vmid)
				prof.finish("register_failed")
				return fmt.Errorf("update VM %s state: %w", p.id, err)
			}
			prof.stage("register")

			mu.Lock()
			entries = append(entries, vmEntry{
				vmid:    p.vmid,
				id:      p.id,
				name:    p.name,
				ip:      guestIP,
				sshPort: ProxyPort(p.vmid),
				created: created,
			})
			mu.Unlock()

			prof.finish("done")
			log.Info("Firecracker VM ready", firecrackerReadyLogAttrs(p.id, p.name, guestIP, values)...)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		if len(entries) == 0 {
			return nil, fmt.Errorf("all Firecracker VMs failed to create: %w", err)
		}
		log.Warn("some Firecracker VMs failed to create", "err", err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].vmid < entries[j].vmid })
	result := make([]VMInfo, 0, len(entries))
	for _, entry := range entries {
		result = append(result, e.vmInfoFromParts(entry.id, entry.name, user, templateID, templateName, opts.Node, entry.ip, entry.created, entry.sshPort))
	}

	log.Info("Firecracker VMs created successfully", "count", len(result))
	return result, nil
}

// DestroyVM stops and deletes a Firecracker instance.
func (e *FirecrackerEngine) DestroyVM(vmid string) error {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	vmidInt, err := strconv.Atoi(vmid)
	if err != nil {
		return fmt.Errorf("invalid VMID %s: %w", vmid, err)
	}

	log := slog.With("backend", firecrackerBackendName, "vmid", vmid, "user", vm.User, "name", vm.Name)
	log.Info("destroying Firecracker VM")

	// Warm-restored instances use the Go warm lifecycle, not firecrackerctl.
	if vm.WarmTemplate != "" {
		return e.warmDestroy(vmid, *vm)
	}

	if err := stopSSHProxy(vmidInt); err != nil {
		log.Warn("stop SSH proxy failed", "err", err)
	}
	// Drop the per-tap L3 lock pins before the tap dies with fc-stop, and
	// stop any exposed-port socat forwards.
	e.reapExposedPorts(vmid, *vm)
	if env, err := readInstanceEnv(filepath.Join(e.installDir, "instances", vmid)); err == nil && env["TAP_DEV"] != "" {
		e.warmDetachBPF(env["TAP_DEV"])
	}
	if _, err := e.firecrackerctl("stop", "--id", vmid, "--install-dir", e.installDir, "--delete-instance"); err != nil {
		return fmt.Errorf("stop Firecracker VM %s: %w", vmid, err)
	}
	if err := e.state.UnregisterVM(vmid); err != nil {
		log.Warn("unregister failed", "err", err)
	}
	e.cleanupUserIfEmpty(vm.User)
	log.Info("Firecracker VM destroyed")
	return nil
}

// DestroyUserVMs stops and deletes all Firecracker instances owned by a user.
func (e *FirecrackerEngine) DestroyUserVMs(user string) error {
	vmids := e.state.UserVMIDs(user)
	if len(vmids) == 0 {
		return fmt.Errorf("no VMs found for user '%s'", user)
	}

	var errs []string
	for _, vmid := range vmids {
		if err := e.DestroyVM(vmid); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", vmid, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("some VMs failed to destroy: %v", errs)
	}
	return nil
}

// Converge keeps one Firecracker instance and destroys the rest for a user.
func (e *FirecrackerEngine) Converge(user, keepVMID string) error {
	keepVM, ok := e.state.GetVM(keepVMID)
	if !ok {
		return fmt.Errorf("VM %s not found", keepVMID)
	}
	if keepVM.User != user {
		return fmt.Errorf("VM %s belongs to '%s', not '%s'", keepVMID, keepVM.User, user)
	}

	var errs []string
	for _, vmid := range e.state.UserVMIDs(user) {
		if vmid == keepVMID {
			continue
		}
		if err := e.DestroyVM(vmid); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", vmid, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("some VMs failed to destroy: %v", errs)
	}
	return nil
}

// SnapshotVM converts a Firecracker instance rootfs into a reusable template.
func (e *FirecrackerEngine) SnapshotVM(vmid string) error {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	vmidInt, err := strconv.Atoi(vmid)
	if err != nil {
		return fmt.Errorf("invalid VMID %s: %w", vmid, err)
	}

	log := slog.With("backend", firecrackerBackendName, "vmid", vmid, "user", vm.User, "name", vm.Name)
	log.Info("snapshotting Firecracker VM")
	if err := requireFirecrackerSnapshotSecurityBaseline(e.state, e.installDir, vmid, vm); err != nil {
		return err
	}
	sourceTemplate := firecrackerTemplateNameFromVM(vm)

	if _, err := e.firecrackerctl("snapshot", "--id", vmid, "--template", vmid, "--install-dir", e.installDir, "--delete-instance"); err != nil {
		return fmt.Errorf("snapshot Firecracker VM %s: %w", vmid, err)
	}
	metadataErr := inheritFirecrackerSecurityMetadata(e.installDir, sourceTemplate, vmid, vmid)
	registerErr := e.state.RegisterTemplate(vmid, TemplateState{
		User:               vm.User,
		SourceVMID:         vmid,
		SourceTemplate:     vm.Template,
		SourceTemplateName: sourceTemplate,
		Backend:            firecrackerBackendName,
		SecurityTemplate:   true,
		Created:            time.Now().UTC().Format(time.RFC3339),
	})
	if err := stopSSHProxy(vmidInt); err != nil {
		log.Warn("stop SSH proxy failed", "err", err)
	}
	if err := e.state.UnregisterVM(vmid); err != nil {
		log.Warn("unregister failed", "err", err)
	}
	e.cleanupUserIfEmpty(vm.User)
	if metadataErr != nil {
		return fmt.Errorf("write Firecracker template security metadata for %s: %w", vmid, metadataErr)
	}
	if registerErr != nil {
		return fmt.Errorf("register template %s: %w", vmid, registerErr)
	}
	log.Info("Firecracker VM converted to template", "template", vmid)
	return nil
}

// ListVMs returns all Firecracker-backed VMs in fastvm API shape.
func (e *FirecrackerEngine) ListVMs(user string) ([]VMInfo, error) {
	vms, err := e.state.ReadVMs()
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(vms))
	for vmid, vm := range vms {
		if user != "" && vm.User != user {
			continue
		}
		ids = append(ids, vmid)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, leftErr := strconv.Atoi(ids[i])
		right, rightErr := strconv.Atoi(ids[j])
		if leftErr == nil && rightErr == nil {
			return left < right
		}
		return ids[i] < ids[j]
	})

	result := make([]VMInfo, 0, len(ids))
	for _, vmid := range ids {
		result = append(result, e.vmInfo(vmid, vms[vmid]))
	}
	return result, nil
}

// GetVM returns detailed info for a single Firecracker instance.
func (e *FirecrackerEngine) GetVM(vmid string) (*VMInfo, error) {
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return nil, fmt.Errorf("VM %s not found", vmid)
	}
	info := e.vmInfo(vmid, *vm)
	return &info, nil
}

// GetVMByName finds a Firecracker instance by name.
func (e *FirecrackerEngine) GetVMByName(name string) (*VMInfo, error) {
	vmid, vm, ok := e.state.FindVMByName(name)
	if !ok {
		return nil, fmt.Errorf("VM with name %q not found", name)
	}
	info := e.vmInfo(vmid, *vm)
	return &info, nil
}

// ResolveVMID takes a name or numeric VMID and returns the VMID string.
func (e *FirecrackerEngine) ResolveVMID(nameOrID string) (string, error) {
	if _, err := strconv.Atoi(nameOrID); err == nil {
		if _, ok := e.state.GetVM(nameOrID); ok {
			return nameOrID, nil
		}
	}
	vmid, _, ok := e.state.FindVMByName(nameOrID)
	if ok {
		return vmid, nil
	}
	return "", fmt.Errorf("VM %q not found (tried VMID and name lookup)", nameOrID)
}

// Status returns an overview of Firecracker resources.
func (e *FirecrackerEngine) Status() (*StatusInfo, error) {
	status, err := e.state.ReadStatus()
	if err != nil {
		return nil, err
	}
	status.Backend = firecrackerBackendName
	return status, nil
}

func (e *FirecrackerEngine) restoreRuntime(vms map[string]VMState) {
	for vmid, vm := range vms {
		if vm.IP == "" {
			continue
		}
		vmidInt, err := strconv.Atoi(vmid)
		if err != nil {
			slog.Warn("cannot restore Firecracker SSH proxy; invalid VMID", "vmid", vmid)
			continue
		}
		if sshProxyRunning(vmidInt) {
			continue
		}
		if err := startSSHProxy(vmidInt, vm.IP); err != nil {
			slog.Warn("failed to restore Firecracker SSH proxy", "vmid", vmid, "ip", vm.IP, "err", err)
		}
	}

	// P1 isolation parity: re-attach the per-tap L3 lock for every running VM
	// whose tap still exists after a daemon restart (cold VMs identity-locked;
	// warm clones re-attach via their template metadata so the NAT rules come
	// back too — same warmAttachBPF parameters as the original launch).
	for vmid := range vms {
		instanceDir := filepath.Join(e.installDir, "instances", vmid)
		env, err := readInstanceEnv(instanceDir)
		if err != nil || env["TAP_DEV"] == "" || env["GUEST_IP"] == "" || env["TAP_IP"] == "" {
			continue
		}
		if _, err := net.InterfaceByName(env["TAP_DEV"]); err != nil {
			continue
		}
		if env["WARM_TPL"] != "" {
			if meta, err := e.GetWarmTemplate(env["WARM_TPL"]); err == nil {
				frozenGWIP := strings.TrimSuffix(meta.TapIP, "/30")
				gwMAC := meta.SourceTapMac
				if gwMAC == "" {
					if iface, lerr := net.InterfaceByName(env["TAP_DEV"]); lerr == nil && len(iface.HardwareAddr) == 6 {
						gwMAC = iface.HardwareAddr.String()
					} else {
						gwMAC = meta.GuestMAC // last resort; ARP responder only
					}
				}
				hostGW := strings.TrimSuffix(env["TAP_IP"], "/30")
				if err := e.warmAttachBPF(env["TAP_DEV"], meta.GuestIP, frozenGWIP, env["GUEST_IP"], gwMAC, hostGW); err == nil {
					slog.Info("re-attached warm NAT + L3 lock post-restart", "vmid", vmid, "tap", env["TAP_DEV"])
				}
			}
			continue
		}
		if err := e.attachL3Lock(env["TAP_DEV"], env["GUEST_IP"], strings.TrimSuffix(env["TAP_IP"], "/24")); err == nil {
			slog.Info("re-attached L3 lock post-restart", "vmid", vmid, "tap", env["TAP_DEV"])
		}
	}
}

func (e *FirecrackerEngine) cleanupFailedCreate(id string, vmid int) {
	stopSSHProxy(vmid)
	if _, err := e.firecrackerctl("stop", "--id", id, "--install-dir", e.installDir, "--delete-instance"); err != nil {
		slog.Warn("failed to clean up failed Firecracker create", "vmid", id, "err", err)
	}
	e.state.UnregisterVM(id)
}

func (e *FirecrackerEngine) cleanupUserIfEmpty(user string) {
	if e.state.UserHasVMs(user) {
		return
	}
	e.state.RemoveUser(user)
}

func (e *FirecrackerEngine) firecrackerctl(args ...string) (string, error) {
	return runCmd(e.bin, args...)
}

func (e *FirecrackerEngine) templateName(template int) string {
	if template > 0 {
		return strconv.Itoa(template)
	}
	return e.defaultTemplate
}

func (e *FirecrackerEngine) templateID(requested int, templateName string) int {
	if requested > 0 {
		return requested
	}
	if id, err := strconv.Atoi(templateName); err == nil {
		return id
	}
	return 0
}

func (e *FirecrackerEngine) vmInfo(vmid string, vm VMState) VMInfo {
	templateName := vm.TemplateName
	if templateName == "" && vm.Template > 0 {
		templateName = strconv.Itoa(vm.Template)
	}
	node := vm.Node
	if node == "" {
		node = e.node
	}
	sshUser := vm.SSHUser
	if sshUser == "" {
		sshUser = e.sshUser
	}

	sshPort := 0
	if vmidInt, err := strconv.Atoi(vmid); err == nil {
		sshPort = ProxyPort(vmidInt)
	}

	info := VMInfo{
		VMID:         vmid,
		Name:         vm.Name,
		User:         vm.User,
		Template:     vm.Template,
		TemplateName: templateName,
		Node:         node,
		IP:           vm.IP,
		Created:      vm.Created,
		SSHPort:      sshPort,
		Backend:      firecrackerBackendName,
		SSHUser:      sshUser,
		ExpiresAt:    vm.ExpiresAt,
		Paused:       vm.Paused,
		ExposedPorts: vm.ExposedPorts,
	}
	if sshPort > 0 {
		info.SSHCommand = fmt.Sprintf("ssh -p %d %s@%s", sshPort, sshUser, e.sshHost)
	}
	return info
}

func (e *FirecrackerEngine) vmInfoFromParts(vmid, name, user string, template int, templateName, node, ip, created string, sshPort int) VMInfo {
	info := VMInfo{
		VMID:         vmid,
		Name:         name,
		User:         user,
		Template:     template,
		TemplateName: templateName,
		Node:         node,
		IP:           ip,
		Created:      created,
		SSHPort:      sshPort,
		Backend:      firecrackerBackendName,
		SSHUser:      e.sshUser,
	}
	if sshPort > 0 {
		info.SSHCommand = fmt.Sprintf("ssh -p %d %s@%s", sshPort, e.sshUser, e.sshHost)
	}
	return info
}

func (e *FirecrackerEngine) installGuestSSHKey(guestIP, publicKey string) error {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return nil
	}

	privateKey := filepath.Join(e.installDir, "images", "id_rsa")
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-i", privateKey,
		fmt.Sprintf("%s@%s", e.sshUser, guestIP),
		"umask 077; mkdir -p ~/.ssh; touch ~/.ssh/authorized_keys; cat >> ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys",
	)
	cmd.Stdin = strings.NewReader(publicKey + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install SSH key in Firecracker guest %s: %w: %s", guestIP, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// warmHygieneKick is the post-restore guest-side hygiene for warm clones,
// executed over a single SSH session off the launch critical path: append
// the launch-time authorized key (vsock-agent equivalent) if given, and set
// the guest wall clock to the host's now. FC 1.17 snapshots carry no KVM
// clock payload (that version has no clock_type create field), so without
// the kick a restored guest keeps the freeze time forever.
func (e *FirecrackerEngine) warmHygieneKick(guestIP, publicKey string) error {
	publicKey = strings.TrimSpace(publicKey)
	privateKey := filepath.Join(e.installDir, "images", "id_rsa")
	cmdParts := func(key bool) *exec.Cmd {
		script := ""
		if key {
			script = "umask 077; mkdir -p ~/.ssh; touch ~/.ssh/authorized_keys; cat >> ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys; "
		}
		// clock REALTIME correction: template vmstates on FC 1.17 have no
		// KVM clock payload, so the guest CLOCK_REALTIME is stuck at the
		// freeze time. date -s sets it forward to the host's now.
		script += fmt.Sprintf("date -u -s @%d >/dev/null", time.Now().Unix())
		// Entropy decorrelation: every clone of a template resumes with the
		// identical CRNG state, so random output correlates across clones
		// until entropy diverges. Push 64 fresh host bytes into the guest
		// pool (mixed without credit -- the fork-safety the guest-agent
		// channel gives the sandbox stack for free).
		seed := make([]byte, 64)
		if _, err := crand.Read(seed); err == nil {
			script += fmt.Sprintf("; printf %%s %s | base64 -d > /dev/urandom",
				base64.StdEncoding.EncodeToString(seed))
		}
		return exec.Command("ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			"-i", privateKey,
			fmt.Sprintf("%s@%s", e.sshUser, guestIP),
			script,
		)
	}
	// The first host-originated TCP connection into a freshly restored guest
	// is not always accepted immediately (first-connect SYN-retransmission
	// dance measured up to ~30 s, then instantly μs clean; see
	// Findings/warm-snapshots-vs-sandbox-platform.md). Every subsequent
	// attempt is instant, so the kick retries on a fixed budget.
	cmd := cmdParts(publicKey != "")
	if publicKey != "" {
		cmd.Stdin = strings.NewReader(publicKey + "\n")
	}
	var out []byte
	var err error
	deadline := time.Now().Add(90 * time.Second)
	for attempt := 1; ; attempt++ {
		out, err = cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("warm hygiene kick for guest %s after %d attempts: %w: %s",
				guestIP, attempt, err, strings.TrimSpace(string(out)))
		}
		time.Sleep(3 * time.Second)
		cmd = cmdParts(publicKey != "")
		if publicKey != "" {
			cmd.Stdin = strings.NewReader(publicKey + "\n")
		}
	}
}

func (e *FirecrackerEngine) reservedFirecrackerIDs() map[int]bool {
	ids := make(map[int]bool)
	for _, dir := range []string{
		filepath.Join(e.installDir, "images"),
		filepath.Join(e.installDir, "instances"),
		filepath.Join(e.installDir, "templates"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && filepath.Ext(name) == ".ext4" {
				name = strings.TrimSuffix(name, ".ext4")
			} else if !entry.IsDir() {
				continue
			}
			id, err := strconv.Atoi(name)
			if err != nil {
				continue
			}
			if id >= vmidMin && id <= vmidMax {
				ids[id] = true
			}
		}
	}
	return ids
}

func parseFirecrackerKeyValues(output string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = strings.TrimSpace(value)
	}
	return values
}

func firecrackerReadyLogAttrs(vmid, name, guestIP string, values map[string]string) []any {
	attrs := []any{"vmid", vmid, "name", name, "ip", guestIP}
	for _, item := range []struct {
		key  string
		attr string
	}{
		{"ROOTFS_CLONE_METHOD", "rootfs_clone_method"},
		{"ROOTFS_SOURCE_BYTES", "rootfs_source_bytes"},
		{"ROOTFS_DEST_BYTES", "rootfs_dest_bytes"},
		{"STORAGE_FSTYPE", "storage_fstype"},
		{"TIME_PLACEMENT_MS", "time_placement_ms"},
		{"TIME_ROOTFS_CLONE_MS", "time_rootfs_clone_ms"},
		{"TIME_NETWORK_MS", "time_network_ms"},
		{"TIME_FIRECRACKER_START_MS", "time_firecracker_start_ms"},
		{"TIME_CONFIGURE_VM_MS", "time_configure_vm_ms"},
		{"TIME_STATE_MS", "time_state_ms"},
		{"TIME_SSH_READY_MS", "time_ssh_ready_ms"},
		{"TIME_TOTAL_MS", "time_total_ms"},
	} {
		if value := values[item.key]; value != "" {
			attrs = append(attrs, item.attr, value)
		}
	}
	return attrs
}

func firecrackerSSHKeyContent(opts CreateOpts) (string, error) {
	if strings.TrimSpace(opts.SSHKeyContent) != "" {
		return strings.TrimSpace(opts.SSHKeyContent), nil
	}
	if strings.TrimSpace(opts.SSHKey) == "" {
		return "", nil
	}
	data, err := os.ReadFile(opts.SSHKey)
	if err != nil {
		return "", fmt.Errorf("read SSH key %s: %w", opts.SSHKey, err)
	}
	return strings.TrimSpace(string(data)), nil
}
