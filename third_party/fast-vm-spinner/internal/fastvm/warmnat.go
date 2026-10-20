package fastvm

// Per-clone identity NAT for warm-restored Firecracker clones: eBPF backend.
//
// A warm template freezes the guest's IP, MAC, gateway IP and gateway MAC into
// the memory image, so the kernel can route only one clone at a time. The
// identity NAT gives every clone a unique, per-VMID host-visible address while
// the guest keeps believing it owns its frozen one:
//
//   - the clone's tap carries an address from FASTVM_WARM_NET (default
//     172.29.0.0/16, disjoint from every routed 192.168/16 firecrackerctl
//     subnet), one /30 per VMID;
//   - the tap's MAC is set to the frozen gateway MAC, so the guest's cached
//     neighbor entry stays valid;
//   - a static neighbor entry pins clone-IP -> frozen guest MAC so the host
//     never ARPs;
//   - a tc/eBPF pair (warmnat.bpf.c) on the tap rewrites clone IP <->
//     frozen IP in both directions and answers the guest's ARP probes for
//     its frozen gateway with the frozen gateway MAC.
//
// The compiled object is embedded and materialized at
// <install-dir>/warmnat.o; tc pins each attachment's maps under
// <install-dir>/bpf/<tap>/, which fastvm fills with the clone's parameters.

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"strconv"
	"syscall"
	"time"
)

//go:embed bpf/warmnat.o
var warmnatELF []byte

const (
	warmNetEnv     = "FASTVM_WARM_NET"
	defaultWarmNet = "172.29.0.0/16"
)

// ippacketReg converts a dotted IPv4 address to the CPU register value the
// BPF program sees when loading the four packet bytes natively: on the
// little-endian targets this runs on, that is the wire-order bytes packed
// little-endian, matching bpf_skb_load_bytes-then-compare and
// bpf_skb_store_bytes alike.
func ippacketReg(ip string) (uint32, error) {
	if _, err := ipU32(ip); err != nil {
		return 0, err
	}
	b := netip.MustParseAddr(ip).As4()
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24, nil
}

// warmIdentityNATEnabled reports whether warm launches use the eBPF identity
// NAT (default) or the legacy frozen-identity tap.
func warmIdentityNATEnabled() bool {
	return strings.TrimSpace(os.Getenv("FASTVM_WARM_IDENTITY_NAT")) != "0"
}

// warmNet returns the /16 base used for clone addressing.
func warmNet() netip.Prefix {
	raw := strings.TrimSpace(os.Getenv(warmNetEnv))
	if raw == "" {
		raw = defaultWarmNet
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || prefix.Addr().Is4() == false || prefix.Bits() > 22 {
		return netip.MustParsePrefix(defaultWarmNet)
	}
	return prefix
}

// warmCloneNet derives the per-clone /30 from the VMID inside warmNet().
// Mapping: VMID -> (i = vmid % 63, j = vmid / 63 % 253). The pair is unique
// for every possible fastvm VMID (period 63*253 = 15939 > the 10000-wide
// VMID band), clones never share subnets, and the range never intersects
// the 192.168/16 world cold VMs live in.
func warmCloneNet(vmid int) (gwCIDR string, cloneIP string) {
	base := warmNet()
	shifted := uint32(base.Addr().As4()[0])<<24 | uint32(base.Addr().As4()[1])<<16 | uint32(base.Addr().As4()[2])<<8 | uint32(base.Addr().As4()[3])
	if base.Bits() != 16 {
		// only the /16 layout is derived here; anything else degrades to
		// the documented default.
		shifted = uint32(172)<<24 | uint32(29)<<16
	}
	i := vmid % 63
	j := (vmid / 63) % 253
	host := shifted | uint32(j+1)<<8 | uint32(i*4+1)
	gw := host
	clone := host + 1
	return u32ToIP(gw) + "/30", u32ToIP(clone)
}

func u32ToIP(v uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func ipU32(s string) (uint32, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !addr.Is4() {
		return 0, fmt.Errorf("invalid IPv4 %q", s)
	}
	a := addr.As4()
	return uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]), nil
}

func macBytes(s string) ([6]byte, error) {
	var mac [6]byte
	var b [6]int
	if n, _ := fmt.Sscanf(strings.TrimSpace(s), "%x:%x:%x:%x:%x:%x", &b[0], &b[1], &b[2], &b[3], &b[4], &b[5]); n != 6 {
		return mac, fmt.Errorf("invalid MAC %q", s)
	}
	for i, v := range b {
		mac[i] = byte(v)
	}
	return mac, nil
}

var (
	warmNatMu   sync.Mutex
	warmNatOnce sync.Once
)

// warmTapIdentity prepares the tap for a NAT-mode clone: the tap carries the
// frozen gateway MAC (so the guest's cached neighbor entry keeps matching)
// and a static neighbor entry pins the clone IP to the frozen guest MAC (so
// the host never needs ARP on the tap).
func warmTapIdentity(tap, sourceTapMAC, cloneIP, guestMAC string) error {
	// IP forwarding for the clone range (idempotent, best effort).
	if raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(raw)) != "1" {
		_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	}
	// One `ip -batch -` for the MAC set and the static neighbor entry —
	// exec setup alone is several ms per invocation.
	batch := ""
	if sourceTapMAC != "" {
		batch += "link set dev " + tap + " address " + sourceTapMAC + "\n"
	}
	batch += "neigh replace " + cloneIP + " lladdr " + guestMAC + " dev " + tap + "\n"
	cmd := exec.Command("ip", "-batch", "-")
	cmd.Stdin = strings.NewReader(batch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tap identity batch %s: %w: %s", tap, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureWarmEgress installs the once-per-host NAT plumbing for the warm clone
// range: forwarding and masquerade. Idempotent; safe to call every launch.
func ensureWarmEgress() error {
	net := warmNet().String()
	// [probe argv, add argv] pairs: probes are allowed to fail with the rule
	// missing; the add variant runs only for missing rules.
	pairs := [][2][]string{
		{{"-C", "FORWARD", "-s", net, "-j", "ACCEPT"}, {"-A", "FORWARD", "-s", net, "-j", "ACCEPT"}},
		{{"-C", "FORWARD", "-d", net, "-j", "ACCEPT"}, {"-A", "FORWARD", "-d", net, "-j", "ACCEPT"}},
		{{"-t", "nat", "-C", "POSTROUTING", "-s", net, "-j", "MASQUERADE"}, {"-t", "nat", "-A", "POSTROUTING", "-s", net, "-j", "MASQUERADE"}},
	}
	for _, pair := range pairs {
		if err := exec.Command("iptables", pair[0]...).Run(); err == nil {
			continue // rule present
		}
		if err := exec.Command("iptables", pair[1]...).Run(); err != nil {
			return fmt.Errorf("iptables %v: %w", pair[1], err)
		}
	}
	return nil
}

// warmAttachBPF loads per-clone instances of the embedded warmnat object with
// this clone's parameters rewritten into the program constants, attaches them
// to the tap (TCX egress + ingress) and pins the links so the NAT survives
// fastvm-api restarts.
func (e *FirecrackerEngine) warmAttachBPF(tap, frozenGuestIP, frozenGWIP, cloneIP, gwMAC, hostGWIP string) error {
	warmNatMu.Lock()
	defer warmNatMu.Unlock()

	// Re-parsing the ELF (with its BTF) is a visible slice of a ~135 ms
	// launch; parse once per process and deep-copy per launch so the
	// per-clone constant rewrites never leak between clones.
	spec, err := warmSpecCached()
	if err != nil {
		return fmt.Errorf("parse warmnat: %w", err)
	}
	setIP := func(name, ip string) error {
		v, err := ippacketReg(ip)
		if err != nil {
			return err
		}
		return spec.Variables[name].Set(v)
	}
	for name, ip := range map[string]string{
		"CFG_GUEST_IP": frozenGuestIP,
		"CFG_GW_IP":    frozenGWIP,
		"CFG_CLONE_IP": cloneIP,
	} {
		if err := setIP(name, ip); err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
	}
	mac, err := macBytes(gwMAC)
	if err != nil {
		return err
	}
	spec.Variables["CFG_GW_MAC"].Set(mac)

	// L3 isolation lock parameters. All values use the same native-packed
	// wire-byte representation as the rewrites (see ippacketReg). Duplicate
	// constants per launch are fine: Variables are set per cloned spec.
	if v, err := ippacketReg(hostGWIP); err == nil {
		// 0 = "no distinct host peer" (cold VMs: the tap gw IS the host peer
		// and CFG_GW_IP equals CFG_HOSTGW_IP, so the same value is fine).
		spec.Variables["CFG_HOSTGW_IP"].Set(v)
	}
	_ = frozenGWIP    // used above in the map
	mesh := uint32(1) // vm-to-vm allowed by default: tenants build clusters
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FASTVM_L3_VM_MESH"))) {
	case "0", "false", "off", "no":
		mesh = uint32(0)
	}
	_ = spec.Variables["CFG_VM_MESH"].Set(mesh)
	if enforce := l3LockDefault(); enforce {
		spec.Variables["CFG_ENFORCE"].Set(uint32(1))
	} else {
		spec.Variables["CFG_ENFORCE"].Set(uint32(0))
	}
	spec.Variables["CFG_NETMASK"].Set(uint32(0x00FFFFFF)) // /24 (packed form)
	if v, err := ippacketReg(frozenGuestIP); err == nil {
		spec.Variables["CFG_OWN_NET"].Set(v & uint32(0x00FFFFFF))
	}
	if v, err := ippacketReg(l3LockDNS()); err == nil {
		spec.Variables["CFG_DNS_IP"].Set(v)
	}
	// Host-infrastructure blocks inside corporate 10/8 (fastvm host fast-net,
	// host mgmt vlan). Corporate 10.x outside these blocks is allowed so
	// tenants can reach internal services (query-prod: LiteLLM, loki, ...).
	nets, masks := l3HostBlocks()
	for i := 0; i < 4; i++ {
		_ = spec.Variables[fmt.Sprintf("CFG_HOSTBLOCK_NET_%d", i)].Set(nets[i])
		_ = spec.Variables[fmt.Sprintf("CFG_HOSTBLOCK_MASK_%d", i)].Set(masks[i])
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("load warmnat: %w", err)
	}
	defer coll.Close()

	iface, err := net.InterfaceByName(tap)
	if err != nil {
		return fmt.Errorf("lookup tap %s: %w", tap, err)
	}

	pinDir := filepath.Join("/sys/fs/bpf/fastvm-warm", tap)
	// Retries after a failed attempt leave pins behind; a fresh attach starts
	// from a clean dir (removing the last reference detaches dead links).
	_ = os.RemoveAll(pinDir)
	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		return err
	}

	attach := func(progName, pinName string, attachType ebpf.AttachType) error {
		prog, ok := coll.Programs[progName]
		if !ok {
			return fmt.Errorf("program %s missing from warmnat", progName)
		}
		l, err := link.AttachTCX(link.TCXOptions{
			Program:   prog,
			Interface: iface.Index,
			Attach:    attachType,
		})
		if err != nil {
			return fmt.Errorf("attach %s to %s: %w", progName, tap, err)
		}
		defer l.Close()
		return l.Pin(filepath.Join(pinDir, pinName))
	}
	if err := attach("tc_to_guest", "to_guest", ebpf.AttachTCXEgress); err != nil {
		return err
	}
	if err := attach("tc_from_guest", "from_guest", ebpf.AttachTCXIngress); err != nil {
		return err
	}
	return nil
}

// warmDetachBPF unpins (and thereby detaches) the identity NAT links of a
// clone tap; the programs go away with them when the last reference drops.
func (e *FirecrackerEngine) warmDetachBPF(tap string) {
	_ = os.RemoveAll(filepath.Join("/sys/fs/bpf/fastvm-warm", tap))
}

// runCmdLink runs an already-built command and returns trimmed output.
func runCmdLink(cmd *exec.Cmd) (string, error) {
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// cloneTapMAC returns the current MAC of a host interface.
func cloneTapMAC(tap string) (string, error) {
	out, err := runCmdLink(exec.Command("ip", "-br", "link", "show", "dev", tap))
	if err != nil {
		return "", err
	}
	// "tap  UP  ...  aa:bb:cc:dd:ee:ff"
	fields := strings.Fields(out)
	for _, f := range fields {
		if strings.Count(f, ":") == 5 {
			return f, nil
		}
	}
	return "", fmt.Errorf("no MAC found for %s", tap)
}

// loadWarmSpec parses the embedded warmnat ELF (exported for tests).
func loadWarmSpec() (*ebpf.CollectionSpec, error) {
	return ebpf.LoadCollectionSpecFromReader(bytes.NewReader(warmnatELF))
}

// l3LockDefault reports whether the per-tap L3 isolation lock is active
// (default on; FASTVM_L3_LOCK=0 disables it host-wide).
// l3HostBlocks returns the fixed 4-slot host-block table (native-packed) for
// the corporate-allow policy. FASTVM_L3_HOST_CIDRS overrides; the default
// blocks the fastvm host fast-net and the host mgmt vlan only.
func l3HostBlocks() ([4]uint32, [4]uint32) {
	var nets, masks [4]uint32
	list := strings.TrimSpace(os.Getenv("FASTVM_L3_HOST_CIDRS"))
	if list == "" {
		list = "10.242.0.0/16,10.226.12.0/24" // default: host planes only
	}
	if strings.EqualFold(list, "none") || strings.EqualFold(list, "off") ||
		strings.EqualFold(list, "0") {
		// explicitly disabled: corporate 10/8 fully reachable from guests
		return [4]uint32{}, [4]uint32{}
	}
	for i, part := range strings.Split(list, ",") {
		if i >= 4 || part == "" {
			break
		}
		part = strings.TrimSpace(part)
		base, bits := part, ""
		if idx := strings.IndexByte(part, '/'); idx >= 0 {
			base, bits = part[:idx], part[idx+1:]
		}
		v, err := ippacketReg(base)
		if err != nil {
			continue
		}
		n := uint32(32)
		if bits != "" {
			pb, err := strconv.Atoi(bits)
			if err != nil || pb < 0 || pb > 32 {
				continue
			}
			n = uint32(pb)
		}
		mask := ^uint32(0) << (32 - n)
		if n == 0 {
			mask = 0
		}
		nets[i] = v & mask // normalize the network base with its mask
		masks[i] = mask
	}
	return nets, masks
}

func l3LockDefault() bool {
	return strings.TrimSpace(os.Getenv("FASTVM_L3_LOCK")) != "0"
}

// l3LockDNS returns the resolver address guests may reach inside otherwise
// blocked RFC1918 space (default 10.255.0.100; FASTVM_L3_DNS overrides).
func l3LockDNS() string {
	raw := strings.TrimSpace(os.Getenv("FASTVM_L3_DNS"))
	if raw == "" {
		return "10.255.0.100"
	}
	return raw
}

// attachL3Lock attaches the warmnat programs to a COLD VM's tap with identity
// parameters (no NAT rewrite: frozen == clone == the VM's own address), which
// turns the same code path into the per-tap source-address lock plus the
// destination policy for freshly booted VMs. The gateway MAC is the tap's own.
func (e *FirecrackerEngine) attachL3Lock(tap, guestIP, gwIP string) error {
	if !l3LockDefault() {
		return nil
	}
	iface, err := net.InterfaceByName(tap)
	if err != nil {
		return fmt.Errorf("lookup tap %s: %w", tap, err)
	}
	hw := iface.HardwareAddr.String()
	// normalize "aa:bb:cc:dd:ee:ff" (HandleLen 6 over the 20-byte slab):
	// HardwareAddr.String() already renders DstPrefixLen correctly for len-6.
	if len(hw) < 17 {
		return fmt.Errorf("tap %s has no usable MAC", tap)
	}
	return e.warmAttachBPF(tap, guestIP, gwIP, guestIP, hw, gwIP)
}

// warmSpecCached parses the embedded ELF once per process and hands out
// deep copies (sub-millisecond) for each per-clone rewrite.
func warmSpecCached() (*ebpf.CollectionSpec, error) {
	warmSpecOnce.Do(func() {
		warmSpecBase, warmSpecErr = ebpf.LoadCollectionSpecFromReader(bytes.NewReader(warmnatELF))
	})
	if warmSpecErr != nil {
		return nil, warmSpecErr
	}
	return warmSpecBase.Copy(), nil
}

var (
	warmSpecOnce sync.Once
	warmSpecBase *ebpf.CollectionSpec
	warmSpecErr  error
)

// warmAnnounceGateway pushes gratuitous ARP replies into a freshly restored
// clone so its frozen neighbor entry for the gateway immediately points at
// the new tap's MAC. Without this, the guest keeps addressing its frames to
// the source tap's (stale, foreign) MAC, the host kernel receives and drops them
// silently, and first TCP connections only work after the guest's NUD
// re-validation forces a fresh ARP (measured 25-40s, "SSH deaf window").
func warmAnnounceGateway(tap, guestFrozenIP, gwFrozenIP, tapMac, guestMAC string) {
	sha, err := macBytes(tapMac)
	if err != nil {
		return
	}
	tha, err := macBytes(guestMAC)
	if err != nil || len(tha) != 6 {
		return
	}
	gw, err := ip4Bytes(gwFrozenIP)
	if err != nil {
		return
	}
	guest, err := ip4Bytes(guestFrozenIP)
	if err != nil {
		return
	}
	ifi, err := net.InterfaceByName(tap)
	if err != nil {
		return
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htonsRaw(0x0806)))
	if err != nil {
		return
	}
	defer syscall.Close(fd)

	buildFrame := func(broadcast bool) []byte {
		var dst [6]byte
		if broadcast {
			dst = [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		} else {
			dst = tha
		}
		frame := make([]byte, 14+28)
		copy(frame[0:6], dst[:])
		copy(frame[6:12], sha[:])
		frame[12], frame[13] = 0x08, 0x06
		frame[14], frame[15] = 0x00, 0x01 // htype Ethernet
		frame[16], frame[17] = 0x08, 0x00 // ptype IPv4
		frame[18] = 6
		frame[19] = 4
		frame[20], frame[21] = 0x00, 0x02 // reply
		copy(frame[22:28], sha[:])
		copy(frame[28:32], gw[:])
		copy(frame[32:38], tha[:])
		copy(frame[38:42], guest[:])
		return frame
	}
	unicast, broadcast := buildFrame(false), buildFrame(true)
	sa := &syscall.SockaddrLinklayer{Protocol: htonsRaw(0x0806), Ifindex: ifi.Index}
	for i := 0; i < 8; i++ {
		if i > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		_ = syscall.Sendto(fd, unicast, 0, sa)
		_ = syscall.Sendto(fd, broadcast, 0, sa)
	}
}

func ip4Bytes(ip string) ([4]byte, error) {
	v := net.ParseIP(ip)
	if v == nil {
		return [4]byte{}, fmt.Errorf("bad ip %q", ip)
	}
	b := v.To4()
	if b == nil {
		return [4]byte{}, fmt.Errorf("not ipv4 %q", ip)
	}
	var out [4]byte
	copy(out[:], b)
	return out, nil
}

func htonsRaw(v uint16) uint16 { return v<<8 | v>>8 }
