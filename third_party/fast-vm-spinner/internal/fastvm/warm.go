package fastvm

// Warm snapshots: pause + dump a running Firecracker microVM (memory + merged
// disk) so clones can be restored in milliseconds with no guest boot at all.
//
// A warm template lives in <installDir>/templates-warm/<name>/ holding:
//   vmstate.snap  Firecracker microVM state (binary; embeds drive path + tap name)
//   mem.img       full guest RAM dump
//   rootfs.ext4   merged disk image (the VM's base after flush)
//   meta.json     recorded network identity needed to wire a clone
//
// Launch clones the three files into a fresh instance dir (XFS reflink),
// rewrites the embedded drive path and tap name in the vmstate (equal-length
// binary patch — all VMIDs are five digits so paths never change size), recreates
// the recorded tap, spawns a pristine Firecracker, PUTs /snapshot/load and
// resumes. The guest clock, sshd listeners and processes continue where the
// pause left off.
//
// Concurrency: all clones of a warm template share the guest's frozen network
// identity (IP + MAC live in the memory image; per-VRF routing is out of
// scope), so at most ONE clone may run per warm template at a time. Trying to
// launch a second clone while one is live fails fast.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	warmTemplateRoot = "templates-warm"
	warmVMStateFile  = "vmstate.snap"
	warmMemFile      = "mem.img"
	warmRootfsFile   = "rootfs.ext4"
	warmKernelFile   = "kernel"
	warmMetaFile     = "meta.json"
)

// WarmTemplate is the durable record of a paused-and-dumped microVM.
type WarmTemplate struct {
	Name         string   `json:"name"`
	SourceVMID   string   `json:"source_vmid"`
	SourceTap    string   `json:"source_tap"`               // tap name recorded in vmstate (rewritten per clone)
	TapIP        string   `json:"tap_ip"`                   // host-side gateway IP for the tap
	GuestIP      string   `json:"guest_ip"`                 // guest address baked into the memory image
	GuestMAC     string   `json:"guest_mac"`                // NIC MAC baked into the memory image
	SourceTapMac string   `json:"source_tap_mac,omitempty"` // MAC of the source tap; clones set it on their taps so the guest's cached gateway keeps matching
	Owner        string   `json:"owner,omitempty"`          // user who froze the VM
	Public       *bool    `json:"public,omitempty"`         // nil = legacy template (permissive); new snapshots default true
	Readers      []string `json:"readers,omitempty"`        // explicit per-user grants when not public
	Kernel       string   `json:"kernel"`                   // kernel image path recorded in vmstate (shared, not patched)
	VCPUs        int      `json:"vcpus"`
	MemMiB       int      `json:"mem_mib"`
	RootfsSource string   `json:"rootfs_source"` // template the source VM was created from
	Created      string   `json:"created"`
}

// --- warm template registry ---

func (e *FirecrackerEngine) warmRoot() string {
	return filepath.Join(e.installDir, warmTemplateRoot)
}

func (e *FirecrackerEngine) warmDir(name string) string {
	return filepath.Join(e.warmRoot(), name)
}

func (e *FirecrackerEngine) GetWarmTemplate(name string) (*WarmTemplate, error) {
	if !warmNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid warm template name %q: must be alphanumeric with dashes, 1-63 chars", name)
	}
	raw, err := os.ReadFile(filepath.Join(e.warmDir(name), warmMetaFile))
	if err != nil {
		return nil, fmt.Errorf("warm template %q not found: %w", name, err)
	}
	var t WarmTemplate
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("warm template %q meta corrupt: %w", name, err)
	}
	if t.Name == "" {
		t.Name = name
	}
	return &t, nil
}

func (e *FirecrackerEngine) ListWarmTemplates() ([]WarmTemplate, error) {
	entries, err := os.ReadDir(e.warmRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []WarmTemplate
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		t, err := e.GetWarmTemplate(ent.Name())
		if err != nil {
			continue
		}
		out = append(out, *t)
	}
	return out, nil
}

func (e *FirecrackerEngine) saveWarmTemplate(t *WarmTemplate) error {
	if err := os.MkdirAll(e.warmDir(t.Name), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(e.warmDir(t.Name), warmMetaFile), raw)
}

// --- mini HTTP client over the Firecracker unix API socket ---

type fcClient struct {
	socket string
	http   http.Client
}

func newFCClient(socket string) *fcClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 30 * time.Second}
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &fcClient{socket: socket, http: http.Client{Transport: transport}}
}

func (c *fcClient) do(method, path string, body any) (int, []byte, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, "http://fc"+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, buf.Bytes(), fmt.Errorf("firecracker API %s %s -> %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	return resp.StatusCode, buf.Bytes(), nil
}

// --- instance helpers ---

type instanceEnv map[string]string

func readInstanceEnv(instanceDir string) (instanceEnv, error) {
	raw, err := os.ReadFile(filepath.Join(instanceDir, "state.env"))
	if err != nil {
		return nil, fmt.Errorf("read state.env: %w", err)
	}
	env := instanceEnv{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return env, nil
}

// writeWarmStateEnv serializes the per-instance state file so hand-spawned and
// warm-restored instances remain indistinguishable from firecrackerctl ones.
func writeWarmStateEnv(instanceDir string, vmid int, tap, tapIP, guestIP, guestMAC, rootfs string, vcpus, memMiB int, warmTpl string, pid int, kernel string) error {
	env := []string{
		"ID=" + strconv.Itoa(vmid),
		"PID=" + strconv.Itoa(pid),
		"TAP_DEV=" + tap,
		"TAP_IP=" + tapIP,
		"GUEST_IP=" + guestIP,
		"GUEST_MAC=" + guestMAC,
		"API_SOCKET=" + filepath.Join(instanceDir, "firecracker.socket"),
		"ROOTFS=" + rootfs,
		"TEMPLATE=warm:" + warmTpl,
		"VCPUS=" + strconv.Itoa(vcpus),
		"MEM_MIB=" + strconv.Itoa(memMiB),
		"FC_SLICE=",
		"HOST_IFACE=",
		"KERNEL=" + kernel,
	}
	return atomicWrite(filepath.Join(instanceDir, "state.env"), []byte(strings.Join(env, "\n")+"\n"))
}

// --- vmstate binary patching ---

// patchVMState rewrites an embedded absolute path in-place and refreshes the
// trailing CRC64. Firecracker serializes the microVM state with length-prefixed
// strings, so replacements must be byte-identical in length; all instances live
// under .../instances/<5-digit VMID>/ so paths never grow or shrink. The last
// 8 bytes of the file hold crc64(0, everything-before) little-endian, chosen so
// that crc64(0, whole file) == 0 (vmm/src/snapshot/mod.rs).
func patchVMState(state []byte, old, new string) ([]byte, error) {
	if len(state) < 8 {
		return nil, fmt.Errorf("vmstate too short for CRC trailer (%d bytes)", len(state))
	}
	if len(old) != len(new) {
		return nil, fmt.Errorf("vmstate path patch requires equal lengths (%q %d bytes vs new %d bytes)", old, len(old), len(new))
	}
	if !bytes.Contains(state[:len(state)-8], []byte(old)) {
		return nil, fmt.Errorf("vmstate does not contain expected path %q", old)
	}
	patched := bytes.ReplaceAll(state[:len(state)-8], []byte(old), []byte(new))
	crc := crc64xz(0, patched)
	return append(patched, le64(crc)...), nil
}

// le64 encodes v little-endian.
func le64(v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return b[:]
}

// validateVMStateCRC returns nil when the file carries a self-consistent CRC.
func validateVMStateCRC(state []byte) error {
	if len(state) < 8 {
		return fmt.Errorf("vmstate too short for CRC trailer (%d bytes)", len(state))
	}
	if crc64xz(0, state) != 0 {
		return fmt.Errorf("vmstate CRC64 check failed")
	}
	return nil
}

// crc64xz is the rolling CRC-64/Jones (the firecracker `crc64` crate, the same
// CRC used by Redis): crc64(crc, buf) with no pre/post inversion, so trailing
// bytes carrying the checksum of the prefix yield a whole-file CRC of zero
// (`crc64(0, data + le64(crc64(0, data))) == 0`). The table lives in
// crc64_table.go, transplanted verbatim from the crate's crc_table.rs.“')
func crc64xz(crc uint64, data []byte) uint64 {
	for _, b := range data {
		crc = crc64Table[byte(crc^uint64(b))] ^ (crc >> 8)
	}
	return crc
}

// --- public engine operations ---

// WarmSnapshot converts a running VM into a warm template the clone can be
// restored from almost instantly. The source VM is consumed (stopped and
// unregistered) exactly like the cold snapshot flow.
func (e *FirecrackerEngine) WarmSnapshot(vmid, name string) (*WarmTemplate, error) {
	if name == "" {
		return nil, fmt.Errorf("warm template name is required")
	}
	vm, ok := e.state.GetVM(vmid)
	if !ok {
		return nil, fmt.Errorf("VM %s not found", vmid)
	}
	instanceDir := filepath.Join(e.installDir, "instances", vmid)
	env, err := readInstanceEnv(instanceDir)
	if err != nil {
		return nil, err
	}
	srcTap := env["TAP_DEV"]
	tapIP := env["TAP_IP"]
	guestIP := env["GUEST_IP"]
	guestMAC := env["GUEST_MAC"]
	rootfs := env["ROOTFS"]
	kernel := env["KERNEL"]
	if srcTap == "" || tapIP == "" || guestIP == "" || rootfs == "" {
		return nil, fmt.Errorf("instance %s state.env is missing network/identity fields", vmid)
	}

	sourceTapMac, _ := cloneTapMAC(srcTap)

	prof := newProf("warm_snapshot", vmid)
	prof.name = name
	var snapMeta *WarmTemplate // set once the template is fully built
	defer func() {
		if snapMeta != nil {
			prof.finish("consume_cleanup")
		}
	}()

	log := slog.With("backend", firecrackerBackendName, "vmid", vmid, "warm_template", name)
	log.Info("creating warm snapshot")

	// Fresh metadata dir; snapshot files are written directly into it.
	dir := e.warmDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	socket := filepath.Join(instanceDir, "firecracker.socket")
	client := newFCClient(socket)

	prof.stage("preflight")
	if _, _, err := client.do(http.MethodPatch, "/vm", map[string]string{"state": "Paused"}); err != nil {
		return nil, fmt.Errorf("pause VM %s: %w", vmid, err)
	}
	prof.stage("fc_pause")
	// clock_type Realtime records the guest clock state so restores can use
	// clock_realtime (native kvmclock advancement — the guest agent hygiene
	// the sandbox stack implements, without the agent). Older VMMs reject the
	// field; degrade gracefully so snapshots still work everywhere.
	snapBody := map[string]interface{}{
		"snapshot_type": "Full",
		"snapshot_path": filepath.Join(dir, warmVMStateFile),
		"mem_file_path": filepath.Join(dir, warmMemFile),
	}
	// Firecracker fsyncs the snapshot files by default, which caps the dump
	// at the disk's synced-write rate (570 MB/s on DO block storage vs
	// ~1 GB/s buffered). Skip it: the artifacts flush through the page cache
	// in the background, and a warm template that dies on a host reboot
	// loses nothing a re-snapshot cannot rebuild. FASTVM_WARM_SYNC=1
	// restores the synced behaviour for operators who want that durability
	// immediately. Older VMMs reject the field (and clock_type) — degrade
	// field-by-field on "unknown field" so snapshotting works everywhere.
	if !warmSyncSnapshot() {
		snapBody["sync_snapshot_files"] = false
	}
	snapBody["clock_type"] = "Realtime"
	dropped := ""
	for attempt := 0; ; attempt++ {
		_, _, err := client.do(http.MethodPut, "/snapshot/create", snapBody)
		if err == nil {
			break
		}
		droppedAny := false
		for _, field := range []string{"clock_type", "sync_snapshot_files"} {
			if _, present := snapBody[field]; present && strings.Contains(err.Error(), "`"+field+"`") {
				delete(snapBody, field)
				dropped += "," + field
				droppedAny = true
			}
		}
		if !droppedAny || attempt >= 3 {
			// Real failure: do not leave the source VM paused.
			if _, _, rerr := client.do(http.MethodPatch, "/vm", map[string]string{"state": "Resumed"}); rerr != nil {
				log.Warn("warm snapshot failed AND resume of source VM failed", "resume_err", rerr)
			} else {
				log.Warn("warm snapshot failed; source VM resumed", "paused_ms", prof.stages[len(prof.stages)-1].MS)
			}
			return nil, fmt.Errorf("create full snapshot of %s: %w", vmid, err)
		}
	}
	if dropped != "" {
		prof.addDetail("vmm_degraded", strings.TrimPrefix(dropped, ","))
		log.Info("snapshot create degraded for this VMM", "dropped", strings.TrimPrefix(dropped, ","))
	}
	prof.stage("fc_snapshot_write")
	// With sync_snapshot_files off, the artifacts stay in the page cache and
	// the kernel's writeback flushes them at its own pace. Deliberately no
	// explicit fsync here: forcing one showed up as a 5-7 s stall on the
	// FIRST launch after a fresh snapshot (the reflink ioctl waits for the
	// in-flight writeback of the 4 GiB dump). Host operators should size
	// vm.dirty_bytes so one snapshot fits comfortably (we ship a sysctl
	// drop-in with the install: vm.dirty_bytes >= 2x template size).
	if memMiB, ok := atoiEnv(env["MEM_MIB"]); ok && memMiB > 0 {
		dumpMB := memMiB // MiB ~= MB for a coarse throughput glance
		secs := prof.stages[len(prof.stages)-1].MS / 1000.0
		if secs > 0 && dumpMB > 0 {
			prof.addDetail("dump_mbps", fmt.Sprintf("%.0f", float64(dumpMB)/secs))
			prof.addDetail("dump_gib", fmt.Sprintf("%.1f", float64(memMiB)/1024.0))
		}
	}

	// The full snapshot flushes the diff into the instance rootfs; move the
	// merged base into the template dir before anyone can delete it.
	dst := e.warmDir(name)
	if _, err := os.Stat(rootfs); err != nil {
		return nil, fmt.Errorf("instance rootfs %s vanished after snapshot: %w", rootfs, err)
	}
	if err := os.Rename(rootfs, filepath.Join(dst, warmRootfsFile)); err != nil {
		return nil, fmt.Errorf("move merged rootfs to warm template: %w", err)
	}
	prof.stage("rootfs_move")
	if err := os.WriteFile(filepath.Join(dst, warmKernelFile), []byte(kernel+"\n"), 0o644); err != nil {
		return nil, err
	}

	// Permissive defaults: a new template is public until its owner restricts
	// it via warm-acl. (Legacy templates without an owner stay permissive too.)
	snapPublic := true
	snapMeta = &WarmTemplate{
		Name: name, SourceTapMac: sourceTapMac,
		Owner:        vm.User,
		Public:       &snapPublic,
		SourceVMID:   vmid,
		SourceTap:    srcTap,
		TapIP:        tapIP,
		GuestIP:      guestIP,
		GuestMAC:     guestMAC,
		Kernel:       kernel,
		VCPUs:        vm.Cores,
		MemMiB:       vm.Memory,
		RootfsSource: strconv.Itoa(vm.Template),
		Created:      time.Now().UTC().Format(time.RFC3339),
	}
	if snapMeta.VCPUs == 0 {
		if v, err := strconv.Atoi(env["VCPUS"]); err == nil {
			snapMeta.VCPUs = v
		}
	}
	if snapMeta.MemMiB == 0 {
		if v, err := strconv.Atoi(env["MEM_MIB"]); err == nil {
			snapMeta.MemMiB = v
		}
	}
	if err := e.saveWarmTemplate(snapMeta); err != nil {
		return nil, err
	}
	prof.stage("meta_save")
	prof.vmid = vmid

	// Consume the source VM with the warm instance cleanup (its rootfs was
	// already moved away; firecrackerctl stop --delete-instance would work but
	// this path is guaranteed for our own instance layout).
	vmidInt, _ := strconv.Atoi(vmid)
	if err := stopSSHProxy(vmidInt); err != nil {
		log.Warn("stop SSH proxy failed", "err", err)
	}
	if err := e.warmCleanupInstance(vmid); err != nil {
		log.Warn("warm snapshot source cleanup incomplete", "err", err)
	}
	if err := e.state.UnregisterVM(vmid); err != nil {
		log.Warn("unregister source VM failed", "err", err)
	}
	e.cleanupUserIfEmpty(vm.User)
	log.Info("warm template created", "dir", dir)
	return snapMeta, nil
}

// warmTemplateReadable reports whether user may launch from meta. Permissive
// defaults everywhere: legacy templates (no owner) are readable by all; new
// templates are public until their owner restricts them.
// warmWriteMeta persists template meta as a single atomic file, keeping the
// in-place strategy (the registry is file-driven, no rewrites of vmstate).
func (e *FirecrackerEngine) warmWriteMeta(name string, meta *WarmTemplate) error {
	if !warmNameRe.MatchString(name) {
		return fmt.Errorf("invalid warm template name %q", name)
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	dst := filepath.Join(e.warmDir(name), warmMetaFile)
	return os.WriteFile(dst, raw, 0o644)
}

func warmTemplateReadable(meta *WarmTemplate, user string) bool {
	if meta == nil {
		return false
	}
	if meta.Owner == "" || meta.Owner == user {
		return true
	}
	if meta.Public != nil {
		if *meta.Public {
			return true
		}
	}
	for _, r := range meta.Readers {
		if r == user {
			return true
		}
	}
	return false
}

// WarmLaunch restores a clone of a warm template. With the identity NAT
// (default) multiple clones of one template run concurrently, each with a
// unique host-visible IP; legacy behaviour (one clone at a time, frozen
// identity) is available with FASTVM_WARM_IDENTITY_NAT=0.
func (e *FirecrackerEngine) WarmLaunch(user, templateName, name, sshPubKey string) (VMInfo, error) {
	meta, err := e.GetWarmTemplate(templateName)
	if err != nil {
		return VMInfo{}, err
	}
	if !warmTemplateReadable(meta, user) {
		return VMInfo{}, fmt.Errorf("warm template %q is restricted: owner %q did not grant access to %q (public=false, readers: %v)", templateName, meta.Owner, user, meta.Readers)
	}
	if !warmValidUsername(user) {
		return VMInfo{}, fmt.Errorf("invalid user %q", user)
	}

	if !warmIdentityNATEnabled() {
		if vmid, vm, busy := e.warmCloneInUse(templateName); busy {
			return VMInfo{}, fmt.Errorf("warm template %q already has a running clone (VM %s, created %s); destroy it first", templateName, vmid, vm.Created)
		}
	}

	// Claim, don't just scan: N concurrent launches must not race onto the
	// same VMID slot between allocation and RegisterVM (each builds its own
	// tap and instance dir keyed by that VMID).
	vmid, err := e.state.ClaimVMID(e.reservedFirecrackerIDs())
	if err != nil {
		return VMInfo{}, err
	}
	defer e.state.ReleaseVMID(vmid)
	if name == "" {
		name = fmt.Sprintf("fastvm-%s-warm-%d", user, vmid)
	} else if !validVMName.MatchString(name) {
		return VMInfo{}, fmt.Errorf("invalid VM name %q: must be alphanumeric with dashes/underscores, 1-63 chars, starting with alphanumeric", name)
	}
	if e.state.NameExists(name) {
		return VMInfo{}, fmt.Errorf("VM name %q already exists", name)
	}

	log := slog.With("backend", firecrackerBackendName, "vmid", vmid, "user", user, "warm_template", templateName)
	started := time.Now()
	prof := newProf("warm_launch", strconv.Itoa(vmid))
	prof.name = name
	prof.addDetail("template", templateName)
	log.Info("restoring warm clone")

	srcDir := e.warmDir(templateName)
	instanceDir := filepath.Join(e.installDir, "instances", strconv.Itoa(vmid))
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		return VMInfo{}, err
	}

	// Reflink only vmstate + rootfs into the instance dir. The 4 GiB memory
	// image is NOT copied: Firecracker maps it private (mem_backend File),
	// so guest RAM writes stay in the VMM's anonymous memory and every clone
	// demand-pages the shared template file from the host page cache — the
	// same sharing that makes fork density cheap on the sandbox stack.
	// A per-launch reflink of the mem file would additionally block on the
	// first launch behind XFS flushing the fresh dump (FICLONE waits for the
	// source's dirty writeback), a multi-second stall.
	for _, f := range []string{warmVMStateFile, warmRootfsFile} {
		if err := reflink(filepath.Join(srcDir, f), filepath.Join(instanceDir, f)); err != nil {
			return VMInfo{}, fmt.Errorf("clone snapshot file %s: %w", f, err)
		}
	}
	prof.stage("preflight_reflink")

	// Patch the embedded drive path and tap name. The vmstate records the
	// SOURCE instance's absolute rootfs path; rewrite it to this clone's
	// (equal length, guaranteed: both are 5-digit instance dirs).
	stateRaw, err := os.ReadFile(filepath.Join(instanceDir, warmVMStateFile))
	if err != nil {
		return VMInfo{}, err
	}
	srcRootfsPath := strings.TrimSuffix(meta.SourceVMID, "") // informational only
	_ = srcRootfsPath
	recordedRootfs := recordedInstancePath(e.installDir, meta.SourceVMID, warmRootfsFile)
	cloneRootfs := recordedInstancePath(e.installDir, strconv.Itoa(vmid), warmRootfsFile)
	stateRaw, err = patchVMState(stateRaw, recordedRootfs, cloneRootfs)
	if err != nil {
		return VMInfo{}, err
	}
	// The tap no longer needs patching: v1.15 restore accepts network_overrides.
	tap, err := cloneTapName(vmid, len(meta.SourceTap))
	if err != nil {
		return VMInfo{}, err
	}
	if err := atomicWrite(filepath.Join(instanceDir, warmVMStateFile), stateRaw); err != nil {
		return VMInfo{}, err
	}
	prof.stage("vmstate_patch")

	// Host networking for the clone.
	nat := warmIdentityNATEnabled()
	var tapCIDR string
	var hostGuestIP string // the host-visible address proxies and SSH dial
	if nat {
		// Identity NAT: unique per-VMID address the host routes to this tap,
		// eBPF pair rewrites it to the frozen identity in both directions.
		var cloneIP string
		tapCIDR, cloneIP = warmCloneNet(vmid)
		hostGuestIP = cloneIP
	} else {
		// Legacy frozen-identity mode: the tap carries the template's own /30.
		// state.env records the bare gateway address, so append the prefix when
		// it is missing.
		tapCIDR = meta.TapIP
		if !strings.Contains(tapCIDR, "/") {
			tapCIDR += "/30"
		}
		hostGuestIP = meta.GuestIP
	}
	if err := createTap(tap, tapCIDR); err != nil {
		return VMInfo{}, fmt.Errorf("create tap: %w", err)
	}
	prof.stage("tap_create")
	prof.addDetail("nat", map[bool]string{true: "1", false: "0"}[warmIdentityNATEnabled()])
	if nat {
		if err := ensureWarmEgress(); err != nil {
			log.Warn("warm egress rules failed (internet may not work)", "err", err)
		}
		effectiveGWMac := meta.SourceTapMac
		if effectiveGWMac == "" {
			// Templates snapshotted before source_tap_mac was recorded answer
			// with the fresh tap's own MAC instead — the guest's neighbor cache
			// updates on the reply, which is equally valid for L2 adjacency.
			if m, err := cloneTapMAC(tap); err == nil {
				effectiveGWMac = m
			}
		}
		if err := warmTapIdentity(tap, meta.SourceTapMac, hostGuestIP, meta.GuestMAC); err != nil {
			return VMInfo{}, fmt.Errorf("tap identity: %w", err)
		}
		prof.stage("tap_identity_neigh")
		frozenGWIP := strings.TrimSuffix(meta.TapIP, "/30")
		hostGWIP := strings.TrimSuffix(tapCIDR, "/30")
		if err := e.warmAttachBPF(tap, meta.GuestIP, frozenGWIP, hostGuestIP, effectiveGWMac, hostGWIP); err != nil {
			return VMInfo{}, fmt.Errorf("attach warm identity NAT: %w", err)
		}
		prof.stage("bpf_attach")
		// Update the restored guest's frozen gateway-neighbor entry right
		// away: without this the guest's first host-directed frames carry the
		// template's stale tap MAC and are silently discarded (25-40s of
		// first-connect "deafness" before NUD forces a fresh ARP).
		go warmAnnounceGateway(tap, meta.GuestIP, frozenGWIP, effectiveGWMac, meta.GuestMAC)
	}

	// Spawn a pristine Firecracker and restore.
	fcBin := strings.TrimSuffix(e.bin, "ctl")
	if _, err := os.Stat(fcBin); err != nil {
		return VMInfo{}, fmt.Errorf("firecracker binary %s unavailable: %w", fcBin, err)
	}
	socket := filepath.Join(instanceDir, "firecracker.socket")
	var pid int
	if err := spawnPristineFC(fcBin, socket, instanceDir, &pid); err != nil {
		return VMInfo{}, err
	}
	prof.stage("fc_spawn")

	// The socket needs a moment to accept; bounded retry.
	client := newFCClient(socket)
	if err := validateVMStateCRC(stateRaw); err != nil {
		return VMInfo{}, fmt.Errorf("patched vmstate failed self-check: %w", err)
	}
	// clock_realtime advances the kvmclock by the wall time elapsed since the
	// snapshot (the sandbox stack re-seeds clocks via a guest agent; a warm
	// clone gets FC's native equivalent). Templates snapshotted before the
	// clock_type create field lack the clock state: Firecracker exits on the
	// rejected load, so detect it once, respawn a pristine VMM and restore
	// without clock advancement for that template.
	useClock := true
	loadBody := func(withClock bool) map[string]interface{} {
		body := map[string]interface{}{
			"snapshot_path": filepath.Join(instanceDir, warmVMStateFile),
			"mem_backend": map[string]interface{}{
				"backend_type": "File",
				// Shared, read-only-for-the-guest template file (mapped
				// private by Firecracker).
				"backend_path": filepath.Join(srcDir, warmMemFile),
			},
			"resume_vm": true,
			"network_overrides": []map[string]string{
				{"iface_id": "net1", "host_dev_name": tap},
			},
		}
		if withClock {
			body["clock_realtime"] = true
		}
		return body
	}
	// clock_realtime lets the restored kvmclock advance by the wall time
	// elapsed since the snapshot — the sandbox stack pushes clock updates via a
	// guest agent; a warm clone gets Firecracker's native support for free.
	// Templates snapshotted before the clock_type create field carry no clock
	// state: Firecracker exits on the rejected load, so respawn a pristine VMM
	// once and finish without clock advancement. The loop likewise tolerates
	// the fresh VMM's socket not accepting yet.
	deadline := time.Now().Add(15 * time.Second)
	var loadErr error
	for {
		_, _, loadErr = client.do(http.MethodPut, "/snapshot/load", loadBody(useClock))
		if loadErr == nil {
			break
		}
		if useClock && strings.Contains(loadErr.Error(), "clock_realtime") {
			// The rejected request killed the VMM; start a clean one and go
			// clock-less for the rest of this launch. The guest of such a
			// template reads the freeze time until we re-snapshot it.
			useClock = false
			_ = syscall.Kill(pid, syscall.SIGKILL)
			if err := spawnPristineFC(fcBin, socket, instanceDir, &pid); err != nil {
				return VMInfo{}, err
			}
			client = newFCClient(socket)
			continue
		}
		if time.Now().After(deadline) {
			return VMInfo{}, fmt.Errorf("warm restore of %s: %w", templateName, loadErr)
		}
		// A freshly spawned VMM binds its API socket within tens of ms; dial
		// fast at first (5ms steps) and fall back to 100ms cadence later so a
		// genuinely wedged VMM still hits the deadline cheaply.
		if time.Since(prof.started) < 400*time.Millisecond {
			time.Sleep(5 * time.Millisecond)
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	prof.stage("fc_load_restore")
	if err := startSSHProxy(vmid, hostGuestIP); err != nil {
		log.Warn("SSH proxy failed (non-fatal)", "err", err)
	}
	prof.stage("ssh_proxy_spawn")

	created := time.Now().UTC().Format(time.RFC3339)
	if err := writeWarmStateEnv(instanceDir, vmid, tap, tapCIDR, hostGuestIP, meta.GuestMAC, cloneRootfs, meta.VCPUs, meta.MemMiB, templateName, pid, meta.Kernel); err != nil {
		return VMInfo{}, err
	}
	prof.stage("state_env_write")

	vmState := VMState{
		User:             user,
		Name:             name,
		Template:         0,
		TemplateName:     "warm:" + templateName,
		Node:             e.node,
		Memory:           meta.MemMiB,
		Cores:            meta.VCPUs,
		IP:               hostGuestIP,
		Backend:          firecrackerBackendName,
		SSHUser:          e.sshUser,
		SecurityTemplate: false,
		WarmTemplate:     templateName,
		Created:          created,
	}
	if err := e.state.RegisterVM(strconv.Itoa(vmid), vmState); err != nil {
		return VMInfo{}, fmt.Errorf("update VM %d state: %w", vmid, err)
	}
	prof.stage("register")

	// Per-launch authorized key (the sandbox stack pushes a fresh key via its
	// vsock guest agent; a warm clone never boots, so plain SSH into the live
	// guest with the template-baked key instead). The SSH handshake costs
	// several hundred ms — more than the whole restore — so append
	// asynchronously and flag it in the response; the first connection with
	// the new key may race the install for up to ~1 s.
	keyAsync := sshPubKey != ""
	go func() {
		if err := e.warmHygieneKick(hostGuestIP, sshPubKey); err != nil {
			slog.Warn("hygiene: post-restore kick failed", "vmid", vmid, "err", err)
		}
	}()
	prof.stage("ssh_key_kickoff")

	prof.finish("done")
	log.Info("Firecracker warm clone ready",
		"ip", hostGuestIP, "tap", tap,
		"restore_ms", time.Since(started).Milliseconds())

	info := e.vmInfoFromParts(strconv.Itoa(vmid), name, user, 0, "warm:"+templateName, e.node, hostGuestIP, created, ProxyPort(vmid))
	info.SSHKeyPending = keyAsync
	return info, nil
}

// warmCloneInUse reports whether the template currently has a live clone.
func (e *FirecrackerEngine) warmCloneInUse(templateName string) (string, VMState, bool) {
	vms, err := e.state.ReadVMs()
	if err != nil {
		return "", VMState{}, false
	}
	for vmid, vm := range vms {
		if vm.WarmTemplate == templateName {
			return vmid, vm, true
		}
	}
	return "", VMState{}, false
}

// warmDestroy tears down a warm-restored instance without firecrackerctl.
func (e *FirecrackerEngine) warmDestroy(vmid string, vm VMState) error {
	log := slog.With("backend", firecrackerBackendName, "vmid", vmid, "warm_template", vm.WarmTemplate)
	log.Info("destroying warm Firecracker VM")
	e.reapExposedPorts(vmid, vm)
	if err := e.warmCleanupInstance(vmid); err != nil {
		return err
	}
	if err := e.state.UnregisterVM(vmid); err != nil {
		log.Warn("unregister failed", "err", err)
	}
	e.cleanupUserIfEmpty(vm.User)
	log.Info("Firecracker VM destroyed")
	return nil
}

func (e *FirecrackerEngine) warmCleanupInstance(vmid string) error {
	instanceDir := filepath.Join(e.installDir, "instances", vmid)
	vmidInt, _ := strconv.Atoi(vmid)
	_ = stopSSHProxy(vmidInt)

	// Consume the transient systemd unit when the instance was cold-created
	// (firecrackerctl spawns the VMM via systemd-run --unit). Killing the pid
	// alone leaves the unit loaded in systemd and blocks cold creates from
	// ever reusing this VMID ("Unit fastvm-microvm-N.service was already
	// loaded or has a fragment file").
	unit := fmt.Sprintf("fastvm-microvm-%s.service", vmid)
	_ = exec.Command("systemctl", "stop", unit).Run()
	_ = exec.Command("systemctl", "reset-failed", unit).Run()

	// Stop the VMM.
	if raw, err := os.ReadFile(filepath.Join(instanceDir, "firecracker.pid")); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if env, err := readInstanceEnv(instanceDir); err == nil && env["TAP_DEV"] != "" {
		teardownTap(env["TAP_DEV"])
		e.warmDetachBPF(env["TAP_DEV"])
	}
	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("remove instance dir %s: %w", instanceDir, err)
	}
	return nil
}

// --- low-level helpers ---

var (
	warmNameRe = validVMName // reuse the VM name rule for template names
	warmMu     sync.Mutex
)

func recordedInstancePath(installDir, vmid, file string) string {
	return filepath.Join(installDir, "instances", vmid, file)
}

// cloneTapName builds a deterministic per-clone tap name with EXACTLY the
// byte length of the tap name recorded in the vmstate (the blob is patched
// in place, so no length changes are permitted).
func cloneTapName(vmid, recordedLen int) (string, error) {
	base := "fc" + strconv.Itoa(vmid)
	if len(base) >= recordedLen {
		return "", fmt.Errorf("cannot fit VMID %d into a %d-char tap name", vmid, recordedLen)
	}
	for len(base) < recordedLen {
		base += "0"
	}
	return base, nil
}

func reflink(src, dst string) error {
	// Never hard-link: the disk image is opened read-write by the clone, so
	// copies must be reflinks or full copies, never shared inodes.
	out, err := exec.Command("cp", "--reflink=always", "--", src, dst).CombinedOutput()
	if err != nil {
		// Filesystems without reflink still work with a full copy.
		if cperr := exec.Command("cp", "--", src, dst).Run(); cperr != nil {
			return fmt.Errorf("reflink %s -> %s: %v (%s)", src, dst, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// spawnPristineFC starts a fresh Firecracker VMM on its API socket and
// records the pid in the instance dir (used at launch and respawn).
func spawnPristineFC(fcBin, socket, instanceDir string, pid *int) error {
	os.Remove(socket)
	cmd := exec.Command(fcBin, "--api-sock", socket, "--enable-pci")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, _ := os.OpenFile(filepath.Join(instanceDir, "firecracker.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if stdout != nil {
		cmd.Stdout = stdout
		cmd.Stderr = stdout
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn firecracker: %w", err)
	}
	*pid = cmd.Process.Pid
	_ = cmd.Process.Release()
	if err := atomicWrite(filepath.Join(instanceDir, "firecracker.pid"), []byte(strconv.Itoa(*pid))); err != nil {
		return err
	}
	return nil
}

func createTap(name, ipCIDR string) error {
	warmMu.Lock()
	defer warmMu.Unlock()
	if _, err := exec.Command("ip", "link", "show", name).CombinedOutput(); err == nil {
		return fmt.Errorf("tap %s already exists", name)
	}
	// One `ip -batch -` instead of three execs: process setup alone is
	// several ms per exec, a visible slice of the restore path otherwise.
	batch := "tuntap add dev " + name + " mode tap\n" +
		"addr add " + ipCIDR + " dev " + name + "\n" +
		"link set dev " + name + " up\n"
	cmd := exec.Command("ip", "-batch", "-")
	cmd.Stdin = strings.NewReader(batch)
	if out, err := cmd.CombinedOutput(); err != nil {
		teardownTap(name)
		return fmt.Errorf("tap batch create %s: %v (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func teardownTap(name string) {
	_ = exec.Command("ip", "link", "del", "dev", name).Run()
}

// warmValidUsername mirrors the API-side username rule without importing api.
func warmValidUsername(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// atoiEnv is a tolerant env parser for profiling details.
// warmSyncSnapshot reports whether operators asked for fsync'd snapshot
// files (durability immediately on snapshot completion).
func warmSyncSnapshot() bool {
	return strings.TrimSpace(os.Getenv("FASTVM_WARM_SYNC")) == "1"
}

func atoiEnv(raw string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return v, true
}

// ListWarmTemplatesForUser lists templates readable by user (filtered); an
// empty user lists everything (operator view).
func (e *FirecrackerEngine) ListWarmTemplatesForUser(user string) ([]WarmTemplate, error) {
	all, err := e.ListWarmTemplates()
	if err != nil {
		return nil, err
	}
	if user == "" {
		return all, nil
	}
	out := []WarmTemplate{}
	for _, t := range all {
		if warmTemplateReadable(&t, user) || t.Owner == user {
			out = append(out, t)
		}
	}
	return out, nil
}

// UpdateWarmTemplateACL restricts or opens a warm template. Permissive rules:
// an unowned (legacy) template can be adopted by the first user who sets an
// ACL; an owned template can only be changed by its owner.
func (e *FirecrackerEngine) UpdateWarmTemplateACL(user, name string, public bool, readers []string) (*WarmTemplate, error) {
	meta, err := e.GetWarmTemplate(name)
	if err != nil {
		return nil, err
	}
	if meta.Owner != "" && meta.Owner != user {
		return nil, fmt.Errorf("warm template %q belongs to %q", name, meta.Owner)
	}
	meta.Owner = user
	meta.Public = &public
	meta.Readers = readers
	if err := e.warmWriteMeta(name, meta); err != nil {
		return nil, err
	}
	return meta, nil
}
