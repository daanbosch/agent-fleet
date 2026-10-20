// warmnat.bpf.c — per-clone identity NAT for warm-launched Firecracker clones.
//
// A warm clone restores a guest whose network identity (its own IP + MAC and
// its gateway's IP + MAC) is frozen inside the memory image. The kernel can
// only route one instance of that identity, which is why fastvm previously
// limited each warm template to a single live clone. This program sits on each
// clone's tap device and translates at L2/L3 so the host sees a unique,
// per-VMID identity while the guest keeps its frozen one:
//
//   TCX egress (host -> guest):  dst IP clone IP -> frozen guest IP
//   TCX ingress (guest -> host): src IP frozen guest IP -> clone IP, plus an
//     ARP responder answering the guest's "who-has <frozen gateway>" probes
//     with the frozen gateway MAC — no netns, no guest-side state.
//
// Every clone attaches its own program instance: fastvm-api rewrites the
// constants below (spec.Variables, cilium/ebpf) before loading, so each tap's
// programs carry their clone's parameters in .rodata.
//
// Build: clang -target bpf -O2 -g -c warmnat.bpf.c -o warmnat.o

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/in.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/types.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#ifndef TC_ACT_OK
#define TC_ACT_OK   0
#define TC_ACT_SHOT 2
#endif

#define ARPOP_REQUEST 1
#define ARPOP_REPLY   2
struct arphdr {
    __be16 ar_hrd;
    __be16 ar_pro;
    __u8   ar_hln;
    __u8   ar_pln;
    __be16 ar_op;
} __attribute__((packed));

// Rewritten per clone by fastvm-api (cilium spec.Variables) at load time.
// Initializers exist only so clang keeps them in .rodata; fastvm always sets
// real values before the program reaches the kernel.
volatile const __u32 CFG_GUEST_IP;  // frozen guest address in the memory image
volatile const __u32 CFG_GW_IP;      // frozen gateway address (source tap IP)
volatile const __u32 CFG_CLONE_IP;  // host-visible per-VMID clone address
volatile const __u8  CFG_GW_MAC[6]; // frozen gateway MAC

// L3 isolation lock (P1 parity with the sandbox netns policy): every guest
// may speak only with its own source address and may only reach its own
// subnet, the DNS resolver and the public internet. The host's management
// and OOB networks, other tenants' 192.168 blocks, the per-clone NAT range
// and metadata addresses never pass. Set CFG_ENFORCE=0 to disable.
volatile const __u32 CFG_OWN_NET;    // guest subnet network address (packed)
volatile const __u32 CFG_NETMASK;    // guest subnet mask (packed), /24 default
volatile const __u32 CFG_DNS_IP;     // permitted resolver address (packed)
volatile const __u32 CFG_HOSTGW_IP;  // host-side peer (the tap's own address)
// Host-infrastructure blocks inside 10/8 that guests must never reach
// (fastvm host fast-net, mgmt vlan). Corporate 10.x outside these blocks
// is allowed: tenants on a shared lab node legitimately need query-prod
// and friends. Unused slots use mask 0 and never match.
volatile const __u32 CFG_HOSTBLOCK_NET_0;
volatile const __u32 CFG_HOSTBLOCK_NET_1;
volatile const __u32 CFG_HOSTBLOCK_NET_2;
volatile const __u32 CFG_HOSTBLOCK_NET_3;
volatile const __u32 CFG_HOSTBLOCK_MASK_0;
volatile const __u32 CFG_HOSTBLOCK_MASK_1;
volatile const __u32 CFG_HOSTBLOCK_MASK_2;
volatile const __u32 CFG_HOSTBLOCK_MASK_3;
volatile const __u32 CFG_ENFORCE;    // 1 = enforce the L3 lock
volatile const __u32 CFG_VM_MESH;     // 1 = allow vm-to-vm on this node
                                     // (warm clone pool 172.29/16, cold pool
                                     // 192.168/16) so tenants can cluster

#define EPH_PORT_LO 32768
#define EPH_PORT_HI 60999

#define IP6_NH_OFF (ETH_HDR_LEN + 6) // next-header byte of the IPv6 header

// All address math below runs on the native-packed wire bytes (little-endian
// load of the network-order octets), the same representation the rewrites
// use: octet 0 in bits 0-7, octet 1 in bits 8-15, ...

#define IP_OFF      sizeof(struct ethhdr)
#define ETH_HDR_LEN sizeof(struct ethhdr)

// Host -> guest: the host addressed the packet to the clone's unique IP; the
// guest only answers its frozen IP, so rewrite the destination. The ethernet
// destination was set by the host's static neighbor entry (clone IP ->
// frozen guest MAC) on this tap.
SEC("tcx/egress")
int tc_to_guest(struct __sk_buff *skb)
{
    void *data = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;
    struct ethhdr *eth = data;
    if (data + ETH_HDR_LEN + sizeof(struct iphdr) > data_end)
        return TC_ACT_OK;

    if (eth->h_proto != bpf_htons(ETH_P_IP))
        return TC_ACT_OK; // ARP and friends pass untouched

    struct iphdr *ip = (void *)(eth + 1);
    __be32 old_ip = ip->daddr;
    if (old_ip != CFG_CLONE_IP || CFG_GUEST_IP == CFG_CLONE_IP) {
        // Not addressing the clone identity, or identity NAT is a true no-op
        // (cold VMs): touch nothing. The csum/store path below is only ever
        // exercised when a real rewrite is needed.
        return TC_ACT_OK;
    }

    __be32 new_ip = CFG_GUEST_IP;
    __u8 proto = ip->protocol;
    __u32 l4_base = IP_OFF + ((ip->ihl ? ip->ihl : 5) << 2);

    // Incremental checksum maintenance: fold the exact byte-level delta
    // (network order in, one's complement out) into each checksum, then
    // rewrite the address. No packet pointers survive the first store.
    __u32 delta = bpf_csum_diff(&old_ip, sizeof(old_ip), &new_ip, sizeof(new_ip), 0);
    bpf_l3_csum_replace(skb, IP_OFF + offsetof(struct iphdr, check), 0, delta, 0);
    bpf_skb_store_bytes(skb, IP_OFF + offsetof(struct iphdr, daddr),
                        &new_ip, sizeof(new_ip), 0);

    // TCP/UDP checksums embed the pseudo-header, which covers the IPs;
    // BPF_F_PSEUDO_HDR applies the same delta to them.
    if (proto == IPPROTO_TCP || proto == IPPROTO_UDP) {
        __u32 l4_off = l4_base +
            (proto == IPPROTO_TCP ? offsetof(struct tcphdr, check)
                                  : offsetof(struct udphdr, check));
        bpf_l4_csum_replace(skb, l4_off, 0, delta,
                            BPF_F_PSEUDO_HDR | BPF_F_MARK_MANGLED_0);
    }
    // ICMPv4's checksum does not cover the pseudo-header: nothing to do.

    return TC_ACT_OK;
}

// Guest -> host: rewrite the frozen guest source IP to the clone's unique IP,
// and answer the guest's ARP probes for its frozen gateway with the frozen
// gateway MAC (rewriting the request in place into an is-at reply and
// hairpinning it back down the tap).
SEC("tcx/ingress")
int tc_from_guest(struct __sk_buff *skb)
{
    void *data = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;
    struct ethhdr *eth = data;
    if (data + ETH_HDR_LEN > data_end)
        return TC_ACT_OK;

    if (eth->h_proto == bpf_htons(ETH_P_ARP)) {
        struct arphdr *arp = (void *)(eth + 1);
        if ((void *)arp + sizeof(struct arphdr) + 20 > data_end)
            return TC_ACT_OK; // too short for full ARP frame

        __u32 off = ETH_HDR_LEN; // bpf_*_bytes after this: no raw ptrs used
        __u32 payload_off = off + sizeof(struct arphdr);

        __u16 op = 0;
        bpf_skb_load_bytes(skb, off + offsetof(struct arphdr, ar_op),
                           &op, sizeof(op));
        if (op != bpf_htons(ARPOP_REQUEST))
            return TC_ACT_OK;

        // TPA at payload+16, SPA at payload+6. TPA is already network order;
        // configure bytes via memcpy so endianness of the const matches.
        __u32 tpa = 0, spa = 0;
        bpf_skb_load_bytes(skb, payload_off + 16, &tpa, 4);
        bpf_skb_load_bytes(skb, payload_off + 6, &spa, 4);
        __u32 cfg_gw = CFG_GW_IP;

        // Only answer lookups of the frozen gateway. Probes for the guest's
        // own address (duplicate detection) and gratuitous announcements have
        // TPA != gw IP and pass exactly as they came in. tpa and CFG_GW_IP are
        // both native loads of the same 4 wire-order bytes, so they compare.
        if (tpa != cfg_gw) {
            return TC_ACT_OK;
        }

        __u8 old_sha[6];
        bpf_skb_load_bytes(skb, payload_off, old_sha, 6);

        __u16 rop = bpf_htons(ARPOP_REPLY);
        bpf_skb_store_bytes(skb, off + offsetof(struct arphdr, ar_op),
                            &rop, sizeof(rop), 0);
        bpf_skb_store_bytes(skb, payload_off, CFG_GW_MAC, 6, 0);
        bpf_skb_store_bytes(skb, payload_off + 6, &cfg_gw, 4, 0);
        bpf_skb_store_bytes(skb, payload_off + 10, old_sha, 6, 0);
        bpf_skb_store_bytes(skb, payload_off + 16, &spa, 4, 0);
        bpf_skb_store_bytes(skb, offsetof(struct ethhdr, h_dest),
                            old_sha, 6, 0);
        bpf_skb_store_bytes(skb, offsetof(struct ethhdr, h_source),
                            CFG_GW_MAC, 6, 0);

        // Hairpin: out the tap's egress toward the guest. tc_to_guest
        // ignores ARP, so there is no rewrite loop.
        return bpf_redirect(skb->ifindex, 0);
    }

    if (CFG_ENFORCE && eth->h_proto == bpf_htons(ETH_P_IPV6)) {
        // Parity guarantee: "enable IPv6 in the guest, any v6 destination
        // -> no route". Dropping egress v6 entirely is the honest version.
        return TC_ACT_SHOT;
    }

    if (eth->h_proto != bpf_htons(ETH_P_IP))
        return TC_ACT_OK;

    if (data + ETH_HDR_LEN + sizeof(struct iphdr) > data_end)
        return TC_ACT_OK;

    struct iphdr *ip = (void *)(eth + 1);
    __be32 old_ip = ip->saddr;
    if (old_ip != CFG_GUEST_IP) {
        // Source-address lock: a guest may only ever speak as itself.
        // Anything else is a spoofing attempt (or misconfiguration).
        if (CFG_ENFORCE)
            return TC_ACT_SHOT;
        return TC_ACT_OK;
    }

    if (CFG_ENFORCE) {
        // Destination policy on the native-packed daddr (see the constant
        // block for the representation). Order: own subnet first, then the
        // resolver allow, then the drops.
        __u32 dst = ip->daddr;

        // Host services are unreachable from a guest, with one stateless
        // carve-out: host-ORIGINATED connections carry ephemeral source
        // ports (32768-60999), so guest replies into that window are
        // legitimate established traffic. Everything else aimed at the
        // host-side peer address — sshd on :22, the API on :8090, anything
        // well-known — is dropped. This is the BPF equivalent of the netns
        // "accept only DNS and ct-established" input rule.
        if (dst == CFG_HOSTGW_IP) {
            __u8 proto = ip->protocol;
            if (proto == IPPROTO_TCP || proto == IPPROTO_UDP) {
                __u32 l4_base = ETH_HDR_LEN + ((ip->ihl ? ip->ihl : 5) << 2);
                if ((void *)(data + l4_base + 4) > data_end)
                    return TC_ACT_SHOT; // truncated L4 to the host: drop
                __u16 dport_raw = 0;
                bpf_skb_load_bytes(skb, l4_base + 2, &dport_raw, 2);
                // load_bytes copies wire bytes as-is: on little-endian the
                // u16 therefore equals bswap16(port). Undo it once, compare
                // in plain host order — a bpf_htons comparison double-swaps
                // (the trap that made this window reject everything).
                __u16 dport = __builtin_bswap16(dport_raw);
                if (dport >= EPH_PORT_LO && dport <= EPH_PORT_HI)
                    goto ingress_ok; // reply slot for established host flows
            }
            return TC_ACT_SHOT;
        }

        if ((dst & CFG_NETMASK) == CFG_OWN_NET)
            goto ingress_ok;    // same-user VMs inside the tap subnet
        if (dst == CFG_DNS_IP)
            goto ingress_ok;
        if (CFG_VM_MESH) {
            // Packed representation keeps the first octet in the LOW byte
            // (every other drop above extracts it with `dst & 0xFF`).
            __u32 o1 = dst & 0xFFU, o2 = (dst >> 8) & 0xFFU;
            if ((o1 == 0xACU && o2 == 0x1DU) ||   // 172.29/16: warm clone pool
                (o1 == 0xC0U && o2 == 0xA8U))     // 192.168/16: cold VM pool
                goto ingress_ok;
        }
        if ((dst & 0xFFU) == 0x0AU) {         // 10.0.0.0/8: allow corporate
// bpf loop bounds must be provably constant
#pragma unroll
            for (int i = 0; i < 4; i++) {
                __u32 hb_mask = 0, hb_net = 0;
                if (i == 0) { hb_mask = CFG_HOSTBLOCK_MASK_0; hb_net = CFG_HOSTBLOCK_NET_0; }
                if (i == 1) { hb_mask = CFG_HOSTBLOCK_MASK_1; hb_net = CFG_HOSTBLOCK_NET_1; }
                if (i == 2) { hb_mask = CFG_HOSTBLOCK_MASK_2; hb_net = CFG_HOSTBLOCK_NET_2; }
                if (i == 3) { hb_mask = CFG_HOSTBLOCK_MASK_3; hb_net = CFG_HOSTBLOCK_NET_3; }
                if (hb_mask != 0 && (dst & hb_mask) == hb_net)
                    return TC_ACT_SHOT; // host fast-net / mgmt: never reachable
            }
            goto ingress_ok; // corporate 10.x service (query-prod etc.)
        }
        if ((dst & 0xFFU) == 0xACU && ((dst >> 8) & 0xF0U) == 0x10U) // 172.16/12
            return TC_ACT_SHOT;
        if ((dst & 0xFFU) == 0xC0U && ((dst >> 8) & 0xFFU) == 0xA8U) // 192.168/16
            return TC_ACT_SHOT; // own /24 already passed above
        if ((dst & 0xFFU) == 0xA9U && ((dst >> 8) & 0xFFU) == 0xFEU) // 169.254/16
            return TC_ACT_SHOT;
        if ((dst & 0xFFU) == 0x7FU)          // 127.0.0.0/8
            return TC_ACT_SHOT;
        if ((dst & 0xF0U) == 0xE0U)          // multicast 224.0.0.0/4
            return TC_ACT_SHOT;
        // everything else is public internet via the masquerade
    }

ingress_ok:;

    // Identity NAT is a true no-op (cold VMs: frozen == clone). The csum
    // machinery below exists for warm clones only; running it with a zero
    // delta was observed to corrupt forwarded (vm-to-vm) packets silently.
    if (CFG_GUEST_IP == CFG_CLONE_IP)
        return TC_ACT_OK;

    __be32 new_ip = CFG_CLONE_IP;
    __u8 proto = ip->protocol;
    __u32 l4_base = IP_OFF + ((ip->ihl ? ip->ihl : 5) << 2);

    __u32 delta = bpf_csum_diff(&old_ip, sizeof(old_ip), &new_ip, sizeof(new_ip), 0);
    bpf_l3_csum_replace(skb, IP_OFF + offsetof(struct iphdr, check), 0, delta, 0);
    bpf_skb_store_bytes(skb, IP_OFF + offsetof(struct iphdr, saddr),
                        &new_ip, sizeof(new_ip), 0);

    if (proto == IPPROTO_TCP || proto == IPPROTO_UDP) {
        __u32 l4_off = l4_base +
            (proto == IPPROTO_TCP ? offsetof(struct tcphdr, check)
                                  : offsetof(struct udphdr, check));
        bpf_l4_csum_replace(skb, l4_off, 0, delta,
                            BPF_F_PSEUDO_HDR | BPF_F_MARK_MANGLED_0);
    }

    return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
