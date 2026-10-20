package fastvm

import (
	"crypto/md5"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	netBase   = "192.168"
	blockSize = 64
	blockMask = 26
	dnsServer = "10.255.0.100"
	natIface  = "vlan300"
)

// bridgeName returns the Linux bridge name for a user (max 15 chars).
func bridgeName(user string) string {
	h := md5.Sum([]byte(user))
	return fmt.Sprintf("fvm-%x", h[:3])
}

// blockNetwork returns the base IP for a block: 192.168.<block/4>.<(block%4)*64>
func blockNetwork(block int) string {
	octet3 := block / 4
	octet4Base := (block % 4) * blockSize
	return fmt.Sprintf("%s.%d.%d", netBase, octet3, octet4Base)
}

// blockGateway returns the gateway IP (first usable address in the /26).
func blockGateway(block int) string {
	net := blockNetwork(block)
	parts := strings.Split(net, ".")
	base, _ := strconv.Atoi(parts[3])
	return fmt.Sprintf("%s.%s.%s.%d", parts[0], parts[1], parts[2], base+1)
}

// blockVMIP returns the VM IP at a given 1-based offset within a block.
func blockVMIP(block, offset int) string {
	net := blockNetwork(block)
	parts := strings.Split(net, ".")
	base, _ := strconv.Atoi(parts[3])
	return fmt.Sprintf("%s.%s.%s.%d", parts[0], parts[1], parts[2], base+1+offset)
}

// blockCIDR returns the CIDR notation for a block's network.
func blockCIDR(block int) string {
	return fmt.Sprintf("%s/%d", blockNetwork(block), blockMask)
}

// setupNetworking creates the bridge, assigns the gateway IP,
// sets up iptables NAT/forwarding rules, and starts dnsmasq for DNS relay.
func (e *Engine) setupNetworking(user string, block int) error {
	bridge := bridgeName(user)
	gwIP := blockGateway(block)
	cidr := blockCIDR(block)

	log := slog.With("user", user, "bridge", bridge, "block", block)

	// Create bridge if it doesn't exist
	if _, err := runCmd("ip", "link", "show", bridge); err != nil {
		log.Info("creating bridge")
		if _, err := runCmd("ip", "link", "add", "name", bridge, "type", "bridge"); err != nil {
			return fmt.Errorf("create bridge %s: %w", bridge, err)
		}
	}
	if _, err := runCmd("ip", "link", "set", bridge, "up"); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", bridge, err)
	}

	// Assign gateway IP if not already set
	addrOut, _ := runCmd("ip", "addr", "show", bridge)
	gwCIDR := fmt.Sprintf("%s/%d", gwIP, blockMask)
	if !strings.Contains(addrOut, gwCIDR) {
		log.Info("assigning gateway IP", "ip", gwCIDR)
		// Ignore error if already exists
		runCmd("ip", "addr", "add", gwCIDR, "dev", bridge)
	}

	// Enable IP forwarding
	data, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if strings.TrimSpace(string(data)) != "1" {
		log.Info("enabling IP forwarding")
		runCmd("sysctl", "-w", "net.ipv4.ip_forward=1")
	}

	// Disable conntrack checksum verification so NIC hardware offload
	// is not defeated for NATed packets. Without this, every forwarded
	// packet has its checksum re-verified in software (~50% throughput loss).
	data, _ = os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_checksum")
	if strings.TrimSpace(string(data)) != "0" {
		log.Info("disabling conntrack checksum for NAT performance")
		runCmd("sysctl", "-w", "net.netfilter.nf_conntrack_checksum=0")
	}

	// iptables MASQUERADE
	if _, err := runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-o", natIface, "-j", "MASQUERADE"); err != nil {
		log.Info("adding NAT masquerade rule", "cidr", cidr)
		if _, err := runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-o", natIface, "-j", "MASQUERADE"); err != nil {
			return fmt.Errorf("add masquerade rule: %w", err)
		}
	}

	// FORWARD rules: bridge → NAT iface
	if _, err := runCmd("iptables", "-C", "FORWARD", "-i", bridge, "-o", natIface, "-j", "ACCEPT"); err != nil {
		if _, err := runCmd("iptables", "-A", "FORWARD", "-i", bridge, "-o", natIface, "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("add forward rule (out): %w", err)
		}
	}

	// FORWARD rules: NAT iface → bridge (RELATED,ESTABLISHED)
	if _, err := runCmd("iptables", "-C", "FORWARD", "-i", natIface, "-o", bridge, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		if _, err := runCmd("iptables", "-A", "FORWARD", "-i", natIface, "-o", bridge, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("add forward rule (in): %w", err)
		}
	}

	// dnsmasq for DNS relay (no DHCP — VMs use static IPs via cloud-init)
	pidDir := filepath.Join(e.stateDir, "dnsmasq")
	os.MkdirAll(pidDir, 0755)
	pidFile := filepath.Join(pidDir, fmt.Sprintf("dnsmasq-%s.pid", user))
	leaseFile := filepath.Join(pidDir, fmt.Sprintf("leases-%s", user))
	logFile := fmt.Sprintf("/var/log/fastvm-dnsmasq-%s.log", user)

	// Check if already running
	if pidData, err := os.ReadFile(pidFile); err == nil {
		pid := strings.TrimSpace(string(pidData))
		if _, err := os.Stat(fmt.Sprintf("/proc/%s", pid)); err == nil {
			log.Info("dnsmasq already running", "pid", pid)
			return nil
		}
	}

	// Touch lease file
	os.WriteFile(leaseFile, nil, 0644)

	log.Info("starting dnsmasq DNS relay")
	_, err := runCmd("dnsmasq",
		"--interface="+bridge,
		"--bind-interfaces",
		"--no-dhcp-interface="+bridge,
		"--no-resolv",
		"--server="+dnsServer,
		"--pid-file="+pidFile,
		"--log-facility="+logFile,
		"--except-interface=lo",
		"--no-hosts",
		"--dhcp-leasefile="+leaseFile,
	)
	if err != nil {
		return fmt.Errorf("start dnsmasq: %w", err)
	}

	return nil
}

// teardownNetworking stops dnsmasq, removes iptables rules, and deletes the bridge.
func (e *Engine) teardownNetworking(user string, block int) error {
	bridge := bridgeName(user)
	cidr := blockCIDR(block)

	log := slog.With("user", user, "bridge", bridge)

	// Stop dnsmasq
	pidFile := filepath.Join(e.stateDir, "dnsmasq", fmt.Sprintf("dnsmasq-%s.pid", user))
	if pidData, err := os.ReadFile(pidFile); err == nil {
		pid := strings.TrimSpace(string(pidData))
		if pidInt, err := strconv.Atoi(pid); err == nil {
			log.Info("stopping dnsmasq", "pid", pidInt)
			runCmd("kill", pid)
		}
		os.Remove(pidFile)
	}

	// Remove iptables rules (ignore errors if already gone)
	runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", cidr, "-o", natIface, "-j", "MASQUERADE")
	runCmd("iptables", "-D", "FORWARD", "-i", bridge, "-o", natIface, "-j", "ACCEPT")
	runCmd("iptables", "-D", "FORWARD", "-i", natIface, "-o", bridge, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")

	// Remove bridge
	if _, err := runCmd("ip", "link", "show", bridge); err == nil {
		log.Info("removing bridge")
		runCmd("ip", "link", "set", bridge, "down")
		runCmd("ip", "link", "delete", bridge, "type", "bridge")
	}

	return nil
}
