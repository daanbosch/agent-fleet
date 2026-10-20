#!/usr/bin/env bash
# ==============================================================================
# FastVM End-to-End Test with Timing
# ==============================================================================
# Tests the full lifecycle: create → verify → SSH → converge → destroy
# Reports timing for every operation.
#
# Usage: FASTVM_API=http://atl1-pc1-025:8090 ./test/e2e.sh
#
# Requires: fastvm CLI, SSH access via port-forwarding (port = VMID)
# ==============================================================================
set -euo pipefail

# --- Config ---
API="${FASTVM_API:?Set FASTVM_API (e.g. http://atl1-pc1-025:8090)}"
USER="e2e-test-$$"
SSH_KEY="/home/terraform/.ssh/id_ed25519.pub"
# SSH host for port-forwarded connections (defaults from API URL hostname)
SSH_HOST="${FASTVM_SSH_HOST:-$(echo "$API" | sed 's|https\?://||;s|:.*||')}"
SSH_IDENTITY="${FASTVM_SSH_KEY:-$HOME/.ssh/terraform_rsa}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; DIM='\033[2m'; BOLD='\033[1m'; RESET='\033[0m'

# --- Helpers ---
ms() {
    if command -v gdate >/dev/null 2>&1; then
        gdate +%s%3N
    elif date +%s%3N 2>/dev/null | grep -qv N; then
        date +%s%3N
    else
        python3 -c 'import time; print(int(time.time()*1000))'
    fi
}
elapsed() { echo $(( $(ms) - $1 )); }

PASS=0; FAIL=0; TOTAL_START=$(ms)

ok()   { PASS=$((PASS+1)); printf "  ${GREEN}✔${RESET} %s ${DIM}(%sms)${RESET}\n" "$1" "$2"; }
fail() { FAIL=$((FAIL+1)); printf "  ${RED}✖${RESET} %s ${DIM}(%sms)${RESET}\n" "$1" "$2"; }
section() { printf "\n${BOLD}${CYAN}━━━ %s ━━━${RESET}\n" "$1"; }

ssh_vm() {
    local vmid="$1"; shift
    local cmd="$*"
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes \
        -o ConnectTimeout=8 -o LogLevel=ERROR -i "$SSH_IDENTITY" \
        -p "$vmid" "ubuntu@${SSH_HOST}" "$cmd" 2>/dev/null
}

cleanup() {
    printf "\n${DIM}Cleaning up user '${USER}'...${RESET}\n"
    fastvm destroy -u "$USER" 2>/dev/null || true
}
trap cleanup EXIT

# ==============================================================================
printf "${BOLD}FastVM End-to-End Test${RESET}\n"
printf "API: ${API}\nUser: ${USER}\n"

# --- Health Check ---
section "Health Check"
T=$(ms)
OUTPUT=$(fastvm health 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "ok"; then
    ok "API health" "$D"
else
    fail "API health" "$D"
    echo "    $OUTPUT"
    exit 1
fi

# --- Create 1 VM ---
section "Create 1 VM"
T=$(ms)
OUTPUT=$(fastvm create -u "$USER" -c 1 --ssh-key "$SSH_KEY" 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "created"; then
    ok "Create 1 VM" "$D"
else
    fail "Create 1 VM" "$D"
    echo "    $OUTPUT"
fi
CREATE_1_MS=$D

# --- List VMs ---
section "List VMs"
T=$(ms)
OUTPUT=$(fastvm list -u "$USER" 2>&1)
D=$(elapsed $T)
VM_COUNT=$(echo "$OUTPUT" | grep -c "fastvm-" || true)
if [[ $VM_COUNT -eq 1 ]]; then
    ok "List shows 1 VM" "$D"
else
    fail "List shows $VM_COUNT VMs (expected 1)" "$D"
fi
LIST_MS=$D

# Extract VMID from list output (used as SSH port)
VM1_ID=$(echo "$OUTPUT" | grep "fastvm-" | awk '{print $1}')
VM1_IP=$(echo "$OUTPUT" | grep "fastvm-" | awk '{print $4}')

# --- Get VM ---
section "Get VM ($VM1_ID)"
T=$(ms)
OUTPUT=$(fastvm get "$VM1_ID" 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "$VM1_IP"; then
    ok "Get VM details" "$D"
else
    fail "Get VM details" "$D"
fi
GET_MS=$D

# --- SSH Readiness ---
section "SSH Readiness (port $VM1_ID → $VM1_IP)"
T=$(ms)
SSH_OK=0
for i in $(seq 1 60); do
    if ssh_vm "$VM1_ID" "echo ok" 2>/dev/null | grep -q "ok"; then
        SSH_OK=1
        break
    fi
    sleep 2
done
D=$(elapsed $T)
if [[ $SSH_OK -eq 1 ]]; then
    ok "SSH ready (attempt $i)" "$D"
else
    fail "SSH not ready after 120s" "$D"
fi
SSH_READY_MS=$D

# --- VM Verification ---
section "VM Verification"

# Internet
T=$(ms)
INET=$(ssh_vm "$VM1_ID" "ping -c1 -W3 8.8.8.8 >/dev/null 2>&1 && echo ok || echo fail")
D=$(elapsed $T)
if [[ "$INET" == "ok" ]]; then ok "Internet access" "$D"; else fail "Internet access" "$D"; fi

# DNS
T=$(ms)
DNS=$(ssh_vm "$VM1_ID" "getent hosts argo-cd.query.prod.telnyx.io >/dev/null 2>&1 && echo ok || echo fail")
D=$(elapsed $T)
if [[ "$DNS" == "ok" ]]; then ok "Internal DNS" "$D"; else fail "Internal DNS" "$D"; fi

# Tools
T=$(ms)
TOOLS=$(ssh_vm "$VM1_ID" "command -v go >/dev/null || test -x /usr/local/go/bin/go && echo go; command -v python3 >/dev/null && echo python3; command -v git >/dev/null && echo git" 2>/dev/null || true)
D=$(elapsed $T)
if echo "$TOOLS" | grep -q "go"; then ok "Go installed" "$D"; else fail "Go not found" "$D"; fi
if echo "$TOOLS" | grep -q "python3"; then ok "Python3 installed" "$D"; else fail "Python3 not found" "$D"; fi
if echo "$TOOLS" | grep -q "git"; then ok "Git installed" "$D"; else fail "Git not found" "$D"; fi

# Destroy 1 VM
section "Destroy 1 VM"
T=$(ms)
OUTPUT=$(fastvm destroy --vm "$VM1_ID" 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "destroyed\|Destroyed"; then
    ok "Destroy single VM" "$D"
else
    fail "Destroy single VM" "$D"
    echo "    $OUTPUT"
fi
DESTROY_1_MS=$D

# --- Create 3 VMs ---
section "Create 3 VMs"
T=$(ms)
OUTPUT=$(fastvm create -u "$USER" -c 3 --ssh-key "$SSH_KEY" --cores 4 -m 8192 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "created"; then
    ok "Create 3 VMs" "$D"
else
    fail "Create 3 VMs" "$D"
    echo "    $OUTPUT"
fi
CREATE_3_MS=$D

# Verify we have 3
T=$(ms)
OUTPUT=$(fastvm list -u "$USER" 2>&1)
D=$(elapsed $T)
VM_COUNT=$(echo "$OUTPUT" | grep -c "fastvm-" || true)
if [[ $VM_COUNT -eq 3 ]]; then
    ok "3 VMs listed" "$D"
else
    fail "$VM_COUNT VMs listed (expected 3)" "$D"
    echo "$OUTPUT"
fi

# Get all VMIDs (used as SSH ports)
VM_IDS=( $(echo "$OUTPUT" | grep "fastvm-" | awk '{print $1}') )
VM_IPS=( $(echo "$OUTPUT" | grep "fastvm-" | awk '{print $4}') )

# SSH into all 3 via port forwarding
section "SSH to all 3 VMs (port-forwarded)"
for idx in 0 1 2; do
    id="${VM_IDS[$idx]:-}"
    ip="${VM_IPS[$idx]:-}"
    [[ -z "$id" ]] && { fail "No VMID for VM $idx" "0"; continue; }
    T=$(ms)
    SSH_OK=0
    for i in $(seq 1 60); do
        if ssh_vm "$id" "echo ok" 2>/dev/null | grep -q "ok"; then
            SSH_OK=1
            break
        fi
        sleep 2
    done
    D=$(elapsed $T)
    if [[ $SSH_OK -eq 1 ]]; then
        ok "SSH to $id via port $id ($ip)" "$D"
    else
        fail "SSH to $id via port $id ($ip)" "$D"
    fi
done

# --- Converge ---
section "Converge (keep ${VM_IDS[0]})"
KEEP="${VM_IDS[0]}"
T=$(ms)
OUTPUT=$(fastvm converge -u "$USER" --keep "$KEEP" 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "Converged\|converged"; then
    ok "Converge (3→1)" "$D"
else
    fail "Converge" "$D"
    echo "    $OUTPUT"
fi
CONVERGE_MS=$D

# Verify only 1 remains
T=$(ms)
OUTPUT=$(fastvm list -u "$USER" 2>&1)
D=$(elapsed $T)
VM_COUNT=$(echo "$OUTPUT" | grep -c "fastvm-" || true)
if [[ $VM_COUNT -eq 1 ]]; then
    ok "1 VM remaining after converge" "$D"
else
    fail "$VM_COUNT VMs remaining (expected 1)" "$D"
fi

# --- Create 5 VMs ---
section "Create 5 VMs"
# First destroy remaining
fastvm destroy -u "$USER" 2>/dev/null || true
T=$(ms)
OUTPUT=$(fastvm create -u "$USER" -c 5 --ssh-key "$SSH_KEY" 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "created"; then
    ok "Create 5 VMs" "$D"
else
    fail "Create 5 VMs" "$D"
fi
CREATE_5_MS=$D

# --- Final Destroy All ---
section "Destroy All VMs"
T=$(ms)
OUTPUT=$(fastvm destroy -u "$USER" 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "Destroyed\|destroyed"; then
    ok "Destroy all for user" "$D"
else
    fail "Destroy all" "$D"
fi
DESTROY_ALL_MS=$D

# --- Status ---
section "Status"
T=$(ms)
OUTPUT=$(fastvm status 2>&1)
D=$(elapsed $T)
if echo "$OUTPUT" | grep -q "Total VMs"; then
    ok "Status" "$D"
else
    fail "Status" "$D"
fi
STATUS_MS=$D

# ==============================================================================
TOTAL_MS=$(( $(ms) - TOTAL_START ))

printf "\n${BOLD}${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
printf "${BOLD}  Results: ${GREEN}${PASS} passed${RESET}, "
[[ $FAIL -gt 0 ]] && printf "${RED}${FAIL} failed${RESET}\n" || printf "${GREEN}0 failed${RESET}\n"
printf "${BOLD}${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n\n"

printf "${BOLD}  Timing Summary${RESET}\n"
printf "  %-30s %8s\n" "Operation" "Time"
printf "  %-30s %8s\n" "──────────────────────────────" "────────"
printf "  %-30s %7sms\n" "Create 1 VM" "$CREATE_1_MS"
printf "  %-30s %7sms\n" "Create 3 VMs" "$CREATE_3_MS"
printf "  %-30s %7sms\n" "Create 5 VMs" "$CREATE_5_MS"
printf "  %-30s %7sms\n" "SSH ready (1st VM)" "$SSH_READY_MS"
printf "  %-30s %7sms\n" "List VMs" "$LIST_MS"
printf "  %-30s %7sms\n" "Get VM" "$GET_MS"
printf "  %-30s %7sms\n" "Converge (3→1)" "$CONVERGE_MS"
printf "  %-30s %7sms\n" "Destroy 1 VM" "$DESTROY_1_MS"
printf "  %-30s %7sms\n" "Destroy all" "$DESTROY_ALL_MS"
printf "  %-30s %7sms\n" "Status" "$STATUS_MS"
printf "  %-30s %8s\n" "──────────────────────────────" "────────"
printf "  %-30s %7sms\n" "TOTAL E2E" "$TOTAL_MS"
printf "\n"
