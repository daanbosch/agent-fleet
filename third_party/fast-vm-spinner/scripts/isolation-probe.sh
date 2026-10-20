#!/bin/bash
# isolation-probe.sh — P1 isolation acceptance for fastvm (the L3 lock).
#
# Run INSIDE a guest VM or warm clone, e.g.:
#   ssh -p 60000 root@<api-host> 'bash -s' < scripts/isolation-probe.sh
#
# Adapts the sandbox stack's v0 acceptance table (section 5 of their
# sandbox-v0.md) to fastvm's environment. The per-tap eBPF L3 lock must make
# every DROP row time out and every ALLOW row answer (open or instantly
# refused).
#
# Override targets with env:
#   API_HOST   host:port of the fastvm API on the fast/OOB net (10/8)
#   MGMT_IP    a host-management address to verify unreachable
#   DNS_IP     permitted resolver (default 10.255.0.100)
#   OTHER_USER_NET  another tenant's 192.168 block probe (host part .2)
#   NAT_PEER   another clone's NAT address (172.29.x.x)

PASS=0; FAIL=0

probe() {
    # probe <expect: drop|allow> <name> <ip> <port>
    local expect="$1" name="$2" ip="$3" port="${4:-22}"
    local out rc
    out=$(timeout -k 1 3 bash -c "echo > /dev/tcp/$ip/$port" 2>&1); rc=$?
    local verdict
    if [ "$rc" -eq 0 ]; then verdict=allow
    elif [ "$rc" -eq 124 ] || [ "$rc" -eq 143 ]; then verdict=drop   # timed out
    else verdict=allow   # refused/other fast error => packet traversed
    fi
    if [ "$verdict" = "$expect" ]; then
        printf '  PASS  %-38s %s\n' "$name" "$expect (rc=$rc)"
        PASS=$((PASS+1))
    else
        printf '  FAIL  %-38s expected %s, got %s (rc=%s %s)\n' "$name" "$expect" "$verdict" "$rc" "$out"
        FAIL=$((FAIL+1))
    fi
}

DNS_IP="${DNS_IP:-10.255.0.100}"
API_HOST_TARGET="${API_HOST:-10.242.128.198:8090}"
API_IP="${API_HOST_TARGET%%:*}"; API_PORT="${API_HOST_TARGET##*:}"

echo "fastvm isolation probe (expects L3 lock enforced)"

# --- must be REACHABLE (allow) ---
probe allow "DNS resolver $DNS_IP:53"     "$DNS_IP" 53
probe allow "public internet 1.1.1.1:443"  "1.1.1.1" 443
# corporate 10.x services must be reachable (query-prod: LiteLLM et al.)
probe allow "corporate LiteLLM :4000"     litellm-aiswe.query.prod.telnyx.io 4000

# --- must be UNREACHABLE (drop) ---
# The tap gateway IS the host: all well-known host services are off limits
# from a guest; only dports in the ephemeral reply window (32768-60999) are
# open for host-established flows — which is why SSH into the VM works while
# this probe's port-22 dial to the same address must drop.
probe drop "host sshd on the tap gw :22"   192.168.137.1 22
probe drop "host API on the tap gw :8090" 192.168.137.1 8090
probe drop "fastvm API host (host fast net)" "$API_IP" "$API_PORT"
probe drop "metadata 169.254.169.254"      169.254.169.254 80
[ -n "${MGMT_IP:-}" ] && probe drop "host management $MGMT_IP" "$MGMT_IP" 22
[ -n "${OTHER_USER_NET:-}" ] && probe drop "other tenant subnet $OTHER_USER_NET" "$OTHER_USER_NET" 22
[ -n "${NAT_PEER:-}" ] && probe drop "another clone's NAT IP $NAT_PEER" "$NAT_PEER" 22

# DNS resolution must work through the permitted resolver
if command -v getent >/dev/null 2>&1; then
    if getent hosts api.telnyx.com >/dev/null 2>&1; then
        echo "  PASS  DNS resolution api.telnyx.com"; PASS=$((PASS+1))
    else
        echo "  FAIL  DNS resolution api.telnyx.com (resolver $DNS_IP)"; FAIL=$((FAIL+1))
    fi
fi

echo
echo "probe summary: $PASS pass, $FAIL fail"
[ "$FAIL" -eq 0 ]
