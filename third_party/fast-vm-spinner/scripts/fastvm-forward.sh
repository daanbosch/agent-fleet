#!/usr/bin/env bash
# fastvm-forward — forward local ports to a fastvm VM
#
# Usage:
#   fastvm-forward <vmid> <local_port>:<remote_port> [<local_port>:<remote_port> ...]
#   fastvm-forward 60000 8080:80 3000:3000 8443:443
#
# This opens SSH tunnels through the socat proxy so you can access
# VM services on localhost. Ctrl+C to stop all tunnels.
#
# Requires: FASTVM_API env var (or defaults to http://localhost:8090)

set -euo pipefail

FASTVM_API="${FASTVM_API:-http://localhost:8090}"

usage() {
    echo "Usage: fastvm-forward <vmid> <local:remote> [<local:remote> ...]"
    echo ""
    echo "Examples:"
    echo "  fastvm-forward 60000 8080:80          # localhost:8080 → VM:80"
    echo "  fastvm-forward 60000 3000:3000 8443:443"
    echo "  fastvm-forward 60000 8080:8080 -k ~/.ssh/id_rsa  # custom key"
    echo ""
    echo "Options:"
    echo "  -k, --key PATH    SSH private key (default: auto-detect)"
    echo "  -u, --user USER   SSH user (default: ubuntu)"
    exit 1
}

[[ $# -lt 2 ]] && usage

VMID="$1"; shift
SSH_KEY=""
SSH_USER="ubuntu"
FORWARDS=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        -k|--key) SSH_KEY="$2"; shift 2 ;;
        -u|--user) SSH_USER="$2"; shift 2 ;;
        -h|--help) usage ;;
        *) FORWARDS+=("$1"); shift ;;
    esac
done

[[ ${#FORWARDS[@]} -eq 0 ]] && { echo "error: no port forwards specified"; usage; }

# Get VM info from API
VM_JSON=$(curl -sf "${FASTVM_API}/api/v1/vms/${VMID}" 2>/dev/null) || {
    echo "error: failed to get VM ${VMID} from ${FASTVM_API}"
    exit 1
}

SSH_PORT=$(echo "$VM_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['ssh_port'])" 2>/dev/null || echo "$VMID")
SSH_HOST=$(echo "$VM_JSON" | python3 -c "import sys,json; d=json.load(sys.stdin)['data']; cmd=d.get('ssh_command',''); print(cmd.split('@')[-1] if '@' in cmd else 'localhost')" 2>/dev/null || echo "localhost")

# Build SSH args
SSH_ARGS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
SSH_ARGS+=(-o ServerAliveInterval=30 -o ServerAliveCountMax=3)
SSH_ARGS+=(-N) # no command, just tunnels

if [[ -n "$SSH_KEY" ]]; then
    SSH_ARGS+=(-i "$SSH_KEY")
fi

SSH_ARGS+=(-p "$SSH_PORT")

# Add port forwards
for fw in "${FORWARDS[@]}"; do
    LOCAL="${fw%%:*}"
    REMOTE="${fw##*:}"
    SSH_ARGS+=(-L "${LOCAL}:localhost:${REMOTE}")
    echo "  localhost:${LOCAL} → VM ${VMID}:${REMOTE}"
done

echo ""
echo "Forwarding to ${SSH_USER}@${SSH_HOST}:${SSH_PORT} (VMID ${VMID})"
echo "Press Ctrl+C to stop"
echo ""

exec ssh "${SSH_ARGS[@]}" "${SSH_USER}@${SSH_HOST}"
